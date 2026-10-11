package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"runtime/debug"
	"slices"
	"sync/atomic"

	"connectrpc.com/connect"
	"google.golang.org/genproto/googleapis/rpc/errdetails"

	"kiwi/internal/identity"
	"kiwi/internal/platform/authz"
	"kiwi/internal/platform/errs"
	"kiwi/internal/platform/google"
)

// What a client reads off the error detail of a consent-required answer: an
// errdetails.ErrorInfo with this reason and domain and the consent URL under
// ConsentURLKey in its metadata.
const (
	ConsentRequiredReason = "CONSENT_REQUIRED"
	ErrorDomain           = "kiwi"
	ConsentURLKey         = "url"
)

// internalMessage is the only text a client ever reads of an unmapped error.
const internalMessage = "internal error"

type actorKey struct{}

// ActorFrom returns the actor the session check put in ctx: the signed-in user of
// the request's workspace. Domain handlers are given it as their actorFrom. Outside
// a request that passed the session check it returns the zero Actor, which no
// domain method accepts.
func ActorFrom(ctx context.Context) authz.Actor {
	a, _ := ctx.Value(actorKey{}).(authz.Actor)
	return a
}

// errorMapper is the one place where domain errors become Connect codes. New
// builds one and hands it to every Handler.Build as a handler option, because a
// built http.Handler cannot be given an interceptor afterwards; New also counts
// the handlers it was applied to, which is how it knows a Build used its options.
//
// It maps unary RPCs only. A streaming RPC passes through it unmapped, so a
// streaming service must map its own errors; the protos contain none.
type errorMapper struct {
	log *slog.Logger
	// wrapped counts the handlers that took the mapper in, one per RPC.
	wrapped atomic.Int64
}

var _ connect.Interceptor = (*errorMapper)(nil)

// WrapUnary implements connect.Interceptor. Connect calls it once per unary RPC when
// it builds the handler.
func (m *errorMapper) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	m.wrapped.Add(1)
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		res, err := next(ctx, req)
		if err != nil {
			return nil, m.mapError(ctx, req.Spec().Procedure, err)
		}
		return res, nil
	}
}

// WrapStreamingClient implements connect.Interceptor; the mapper is server-side.
func (m *errorMapper) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return next
}

// WrapStreamingHandler implements connect.Interceptor. It maps nothing (see
// errorMapper) but counts the handler, so a streaming-only service is not taken
// for one that ignored its options.
func (m *errorMapper) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	m.wrapped.Add(1)
	return next
}

// sentinelCode is a domain sentinel and the Connect code it becomes.
type sentinelCode struct {
	sentinel error
	code     connect.Code
}

var sentinelCodes = []sentinelCode{
	{errs.ErrNotFound, connect.CodeNotFound},
	{errs.ErrForbidden, connect.CodePermissionDenied},
	{errs.ErrConflict, connect.CodeAborted},
	{errs.ErrInvalid, connect.CodeInvalidArgument},
}

// mapError turns what a domain returned into what the client may see. A mapped
// error reaches the client as the text of its sentinel and nothing else: domains
// wrap a sentinel with context, and some chains carry an infrastructure error
// beside it (authz does for a failed membership lookup), so the full text could
// carry a database address or a query. The operator is told what the client is
// not: a refusal that carries such a cause at WARN (an outage must not hide behind
// a PermissionDenied), a plain refusal at DEBUG, anything unmapped at ERROR.
//
// A Connect error a handler built itself keeps its code and text, except Internal
// and Unknown: those are what an unmapped cause becomes, so building one from a
// cause must not publish the cause.
func (m *errorMapper) mapError(ctx context.Context, procedure string, err error) error {
	var coded *connect.Error
	if errors.As(err, &coded) {
		if c := coded.Code(); c != connect.CodeInternal && c != connect.CodeUnknown {
			return coded
		}
		return m.hide(ctx, procedure, err)
	}

	var consent *google.ConsentRequiredError
	if errors.As(err, &consent) {
		if ce := consentError(consent); ce != nil {
			return ce
		}
		return m.hide(ctx, procedure, err)
	}

	for _, s := range sentinelCodes {
		if !errors.Is(err, s.sentinel) {
			continue
		}
		if carriesForeignCause(err) {
			m.log.WarnContext(ctx, "api: rpc refused beside an infrastructure error", "procedure", procedure, "code", s.code.String(), "err", err)
		} else {
			m.log.DebugContext(ctx, "api: rpc refused", "procedure", procedure, "code", s.code.String(), "err", err)
		}
		return connect.NewError(s.code, s.sentinel)
	}
	return m.hide(ctx, procedure, err)
}

// hide logs err, which the client must not read, and returns the fixed answer.
func (m *errorMapper) hide(ctx context.Context, procedure string, err error) error {
	m.log.ErrorContext(ctx, "api: rpc failed", "procedure", procedure, "err", err)
	return connect.NewError(connect.CodeInternal, errors.New(internalMessage))
}

// carriesForeignCause reports whether err's chain holds anything besides the
// domain sentinels: a leaf of the error tree that is none of them. A sentinel
// wrapped in context ("chat: join channel 7: not found") has none; authz's
// "membership lookup: forbidden: <database error>" has the database error.
func carriesForeignCause(err error) bool {
	switch u := err.(type) {
	case interface{ Unwrap() []error }:
		return slices.ContainsFunc(u.Unwrap(), carriesForeignCause)
	case interface{ Unwrap() error }:
		inner := u.Unwrap()
		return inner == nil || carriesForeignCause(inner)
	}
	return !slices.ContainsFunc(sentinelCodes, func(s sentinelCode) bool { return err == s.sentinel })
}

// consentError is FailedPrecondition with the consent URL in an ErrorInfo detail;
// the Error text of the cause stays out of the answer. It returns nil if the
// detail cannot be built, which the caller treats as an internal error.
func consentError(c *google.ConsentRequiredError) *connect.Error {
	detail, err := connect.NewErrorDetail(&errdetails.ErrorInfo{
		Reason:   ConsentRequiredReason,
		Domain:   ErrorDomain,
		Metadata: map[string]string{ConsentURLKey: c.URL},
	})
	if err != nil {
		return nil
	}
	ce := connect.NewError(connect.CodeFailedPrecondition, errors.New("consent required"))
	ce.AddDetail(detail)
	return ce
}

// requireSession answers 401 unless the request carries the cookie of a live
// session, and otherwise hands next a context that carries the user as ActorFrom's
// actor. It sits in front of Connect handlers, so it writes the Connect error
// answer itself.
func requireSession(sessions SessionStore, log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			c, err := r.Cookie(identity.SessionCookie)
			if err != nil || c.Value == "" {
				writeError(w, http.StatusUnauthorized, connect.CodeUnauthenticated, "sign in required")
				return
			}
			rec, err := sessions.GetSession(r.Context(), c.Value)
			switch {
			case errors.Is(err, errs.ErrNotFound):
				writeError(w, http.StatusUnauthorized, connect.CodeUnauthenticated, "sign in required")
				return
			case err != nil:
				// An unreachable session store is an outage, not a signed-out user:
				// 401 would make every client drop its sign-in for it.
				log.ErrorContext(r.Context(), "api: session lookup failed", "err", err)
				writeError(w, http.StatusInternalServerError, connect.CodeInternal, internalMessage)
				return
			}
			actor := authz.Actor{Kind: authz.ActorUser, UserID: rec.UserID, WorkspaceID: rec.WorkspaceID}
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), actorKey{}, actor)))
		})
	}
}

// recoverPanics answers a handler's panic with an internal error instead of the
// dropped connection net/http makes of it, and logs the panic with its stack.
func recoverPanics(log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				v := recover()
				if v == nil {
					return
				}
				if v == http.ErrAbortHandler {
					panic(v) // the signal net/http expects to see
				}
				log.ErrorContext(r.Context(), "api: handler panicked", "panic", v, "stack", string(debug.Stack()))
				writeError(w, http.StatusInternalServerError, connect.CodeInternal, internalMessage)
			}()
			next.ServeHTTP(w, r)
		})
	}
}

// writeError writes the answer of the Connect protocol to a failed unary call: the
// HTTP status of the code and a JSON body naming it.
func writeError(w http.ResponseWriter, status int, code connect.Code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}{code.String(), message})
}
