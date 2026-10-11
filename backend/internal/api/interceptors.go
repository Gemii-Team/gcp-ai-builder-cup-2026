package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"runtime/debug"

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

// HandlerOptions are the options every domain's Connect handler must be built
// with, so that the errors it returns leave as Connect codes and nothing internal
// reaches the client:
//
//	path, h := chat.Handler(svc, api.ActorFrom, api.HandlerOptions(log)...)
//
// A built handler cannot be given an interceptor afterwards, which is why the
// options are passed in rather than applied by New. log receives the errors that
// are hidden from the client; nil selects slog.Default().
func HandlerOptions(log *slog.Logger) []connect.HandlerOption {
	return []connect.HandlerOption{connect.WithInterceptors(errorInterceptor(log))}
}

// errorInterceptor is the one place where domain errors become Connect codes.
func errorInterceptor(log *slog.Logger) connect.UnaryInterceptorFunc {
	if log == nil {
		log = slog.Default()
	}
	return func(next connect.UnaryFunc) connect.UnaryFunc {
		return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
			res, err := next(ctx, req)
			if err != nil {
				return nil, mapError(ctx, log, req.Spec().Procedure, err)
			}
			return res, nil
		}
	}
}

// sentinelCodes is every domain sentinel and the Connect code it becomes.
var sentinelCodes = []struct {
	sentinel error
	code     connect.Code
}{
	{errs.ErrNotFound, connect.CodeNotFound},
	{errs.ErrForbidden, connect.CodePermissionDenied},
	{errs.ErrConflict, connect.CodeAborted},
	{errs.ErrInvalid, connect.CodeInvalidArgument},
}

// mapError turns what a domain returned into what the client may see. A mapped
// error reaches the client as the text of its sentinel and nothing else: domains
// wrap a sentinel with context, and some chains carry an infrastructure error
// beside it (authz does for a failed membership lookup), so the full text could
// carry a database address or a query. Whatever maps to no code is logged here and
// replaced by a fixed message.
func mapError(ctx context.Context, log *slog.Logger, procedure string, err error) error {
	var (
		coded   *connect.Error
		consent *google.ConsentRequiredError
	)
	if errors.As(err, &coded) {
		return err // the handler chose this code and text itself
	}
	if errors.As(err, &consent) {
		if ce := consentError(consent); ce != nil {
			return ce
		}
	}
	for _, m := range sentinelCodes {
		if errors.Is(err, m.sentinel) {
			log.DebugContext(ctx, "api: rpc refused", "procedure", procedure, "code", m.code.String(), "err", err)
			return connect.NewError(m.code, m.sentinel)
		}
	}
	log.ErrorContext(ctx, "api: rpc failed", "procedure", procedure, "err", err)
	return connect.NewError(connect.CodeInternal, errors.New(internalMessage))
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
