package api

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/wrapperspb"

	chatv1 "kiwi/gen/kiwi/chat/v1"
	"kiwi/gen/kiwi/chat/v1/chatv1connect"
	identityv1 "kiwi/gen/kiwi/identity/v1"
	"kiwi/gen/kiwi/identity/v1/identityv1connect"
	"kiwi/internal/chat"
	"kiwi/internal/identity"
	"kiwi/internal/platform/authz"
	"kiwi/internal/platform/blob"
	"kiwi/internal/platform/cache"
	"kiwi/internal/platform/config"
	"kiwi/internal/platform/db"
	"kiwi/internal/platform/errs"
	"kiwi/internal/platform/google"
	"kiwi/internal/platform/httpx"
	"kiwi/internal/testkit/containers"
	"kiwi/internal/testkit/dbtest"
	"kiwi/internal/testkit/fakeoidc"
	"kiwi/internal/testkit/fakeoidc/fakeoidctest"
)

// One plain Valkey serves the package. Every test signs in users it creates itself,
// so the session and ticket keys of one test never meet another's.
var (
	valkeyOnce sync.Once
	valkeyURL  string
	valkeyErr  error
	valkeyCtr  testcontainers.Container
)

func TestMain(m *testing.M) {
	code := m.Run()
	if valkeyCtr != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		_ = valkeyCtr.Terminate(ctx)
		cancel()
	}
	os.Exit(code)
}

func bootValkey() (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	ctr, err := containers.Start(ctx, "valkey/valkey:8",
		testcontainers.WithLogger(log.New(io.Discard, "", 0)),
		testcontainers.WithExposedPorts("6379/tcp"),
		testcontainers.WithCmd("valkey-server", "--save", "", "--appendonly", "no"),
		testcontainers.WithWaitStrategy(wait.ForLog("Ready to accept connections")),
	)
	if err != nil {
		return "", fmt.Errorf("start valkey (is the Docker daemon reachable?): %w", err)
	}
	valkeyCtr = ctr
	host, err := ctr.Host(ctx)
	if err != nil {
		return "", err
	}
	port, err := ctr.MappedPort(ctx, "6379/tcp")
	if err != nil {
		return "", err
	}
	return "redis://" + host + ":" + port.Port(), nil
}

// ---- helpers ----------------------------------------------------------------

// syncBuffer is what the shell's logger writes to, so a test can read what an
// operator would see.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// headerTransport adds fixed headers to every request.
type headerTransport struct {
	base   http.RoundTripper
	header http.Header
}

func (t headerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	for k, v := range t.header {
		r.Header[k] = v
	}
	return t.base.RoundTrip(r)
}

// wireError is the body of a Connect error response.
type wireError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// probeGet and probePost call a local test listener with the test's context.
func probeGet(t *testing.T, target string) (*http.Response, error) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, target, nil)
	if err != nil {
		t.Fatal(err)
	}
	return http.DefaultClient.Do(req)
}

func probePost(t *testing.T, target, body string) (*http.Response, error) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, target, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	return http.DefaultClient.Do(req)
}

func connectError(t *testing.T, err error) *connect.Error {
	t.Helper()
	var ce *connect.Error
	if !errors.As(err, &ce) {
		t.Fatalf("err = %v (%T), want a *connect.Error", err, err)
	}
	return ce
}

// wantCode fails the test unless err is a Connect error with code want.
func wantCode(t *testing.T, err error, want connect.Code) {
	t.Helper()
	if err == nil {
		t.Fatalf("err = nil, want code %s", want)
	}
	if ce := connectError(t, err); ce.Code() != want {
		t.Fatalf("code = %s (%v), want %s", ce.Code(), ce.Message(), want)
	}
}

// errorWith is wantCode that returns the error for further checks.
func errorWith(t *testing.T, err error, want connect.Code) *connect.Error {
	t.Helper()
	wantCode(t, err, want)
	return connectError(t, err)
}

// ---- fixture ----------------------------------------------------------------

// probePath is the route of a test-only Connect service whose one method does
// whatever the test registered under the name it is called with.
const probePath = "/kiwi.test.v1.ProbeService/"

type probeCase func(ctx context.Context) (string, error)

func probeHandler(log *slog.Logger, cases map[string]probeCase) Handler {
	h := connect.NewUnaryHandler(probePath+"Probe",
		func(ctx context.Context, req *connect.Request[wrapperspb.StringValue]) (*connect.Response[wrapperspb.StringValue], error) {
			fn, ok := cases[req.Msg.GetValue()]
			if !ok {
				return nil, connect.NewError(connect.CodeUnimplemented, fmt.Errorf("no probe case %q", req.Msg.GetValue()))
			}
			out, err := fn(ctx)
			if err != nil {
				return nil, err
			}
			return connect.NewResponse(wrapperspb.String(out)), nil
		},
		HandlerOptions(log)...)
	return Handler{Path: probePath, H: h}
}

// fixture is the shell over a real Postgres, a real Valkey, the real identity and
// chat services and a real local blob store, behind a real HTTP server whose
// address is PublicURL.
type fixture struct {
	t         *testing.T
	env       *dbtest.Env
	cache     *cache.Cache
	store     blob.Store
	logs      *syncBuffer
	handler   http.Handler
	srv       *httptest.Server
	publicURL string

	ws         uuid.UUID
	alice, bob uuid.UUID
}

func newFixture(t *testing.T, probe map[string]probeCase) *fixture {
	t.Helper()
	valkeyOnce.Do(func() { valkeyURL, valkeyErr = bootValkey() })
	if valkeyErr != nil {
		t.Fatalf("valkey: %v", valkeyErr)
	}
	env := dbtest.Start(t)

	// The listener exists before the server starts, so PublicURL can be its address.
	srv := httptest.NewUnstartedServer(nil)
	t.Cleanup(srv.Close)
	publicURL := "http://" + srv.Listener.Addr().String()

	idp := fakeoidctest.New(t, fakeoidctest.WithRedirectURIs(publicURL+"/api/auth/callback"))
	cfg := config.Config{
		Env:               "local",
		PublicURL:         publicURL,
		DatabaseURL:       env.AppURL,
		DatabaseSystemURL: env.SystemURL,
		ValkeyURL:         valkeyURL,
		OIDCIssuer:        idp.Issuer(),
		OAuthClientID:     idp.ClientID(),
		OAuthClientSecret: idp.ClientSecret(),
		TokenEncKey:       bytes.Repeat([]byte{0x42}, 32),
		WorkspaceName:     "Acme Corp",
		WorkspaceDomain:   fakeoidc.DefaultDomain,
		BlobURL:           (&url.URL{Scheme: "file", Path: t.TempDir()}).String(),
		BlobSigningKey:    bytes.Repeat([]byte{0x24}, 32),
	}

	d, err := db.Open(t.Context(), cfg)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(d.Close)
	c, err := cache.Open(t.Context(), valkeyURL, cache.Options{})
	if err != nil {
		t.Fatalf("cache.Open: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })

	logs := &syncBuffer{}
	logger := slog.New(slog.NewTextHandler(logs, nil))

	ident, err := identity.New(t.Context(), identity.Deps{Config: cfg, DB: d, Cache: c, Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatalf("identity.New: %v", err)
	}
	chatSvc := chat.New(chat.Deps{DB: d, Authz: authz.New(chat.NewMembership(d, c)), Cache: c})
	store, err := blob.Open(t.Context(), cfg)
	if err != nil {
		t.Fatalf("blob.Open: %v", err)
	}
	blobHandler, ok := store.(http.Handler)
	if !ok {
		t.Fatalf("blob.Open returned %T, which does not serve signed URLs", store)
	}
	if closer, ok := store.(io.Closer); ok {
		t.Cleanup(func() { _ = closer.Close() })
	}

	opts := HandlerOptions(logger)
	chatPath, chatH := chat.Handler(chatSvc, ActorFrom, opts...)
	idPath, idH := identity.Handler(ident, ActorFrom, opts...)
	handlers := []Handler{{Path: chatPath, H: chatH}, {Path: idPath, H: idH}}
	if probe != nil {
		handlers = append(handlers, probeHandler(logger, probe))
	}
	h := New(Deps{
		PublicURL: publicURL,
		Sessions:  c,
		Auth:      ident.AuthHandler(),
		Blob:      blobHandler,
		Handlers:  handlers,
		Logger:    logger,
	})
	srv.Config.Handler = h
	srv.Start()

	f := &fixture{t: t, env: env, cache: c, store: store, logs: logs, handler: h, srv: srv, publicURL: publicURL}
	f.ws = uuid.New()
	f.exec(f.ws, `INSERT INTO identity.workspaces (id, name, google_domain) VALUES ($1, $2, $2)`, f.ws, "acme.example")
	f.alice = f.seedUser("Alice")
	f.bob = f.seedUser("Bob")
	return f
}

// exec runs sql as kiwi_owner in a transaction bound to ws.
func (f *fixture) exec(ws uuid.UUID, sql string, args ...any) {
	f.t.Helper()
	ctx := f.t.Context()
	conn, err := pgx.Connect(ctx, f.env.OwnerURL)
	if err != nil {
		f.t.Fatalf("connect as owner: %v", err)
	}
	defer func() { _ = conn.Close(ctx) }()
	tx, err := conn.Begin(ctx)
	if err != nil {
		f.t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SELECT set_config('kiwi.workspace_id', $1, true)`, ws.String()); err != nil {
		f.t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, sql, args...); err != nil {
		f.t.Fatalf("%s: %v", sql, err)
	}
	if err := tx.Commit(ctx); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) seedUser(name string) uuid.UUID {
	f.t.Helper()
	id := uuid.New()
	f.exec(f.ws, `INSERT INTO identity.users (id, workspace_id, google_sub, email, name) VALUES ($1, $2, $3, $4, $5)`,
		id, f.ws, "sub-"+id.String(), strings.ToLower(name)+"@example.test", name)
	return id
}

// signIn stores a session for user and returns its id, what the cookie carries.
func (f *fixture) signIn(user uuid.UUID) string {
	f.t.Helper()
	sid := "sid-" + uuid.NewString()
	if err := f.cache.PutSession(f.t.Context(), sid, cache.SessionRecord{UserID: user, WorkspaceID: f.ws}); err != nil {
		f.t.Fatalf("PutSession: %v", err)
	}
	return sid
}

// browser is an HTTP client that behaves like the app's page: it sends the session
// cookie (none when sid is empty) and the page's Origin on every request.
func (f *fixture) browser(sid string) *http.Client {
	h := http.Header{"Origin": {f.publicURL}}
	if sid != "" {
		h.Set("Cookie", identity.SessionCookie+"="+sid)
	}
	return &http.Client{Transport: headerTransport{base: f.srv.Client().Transport, header: h}}
}

func (f *fixture) chatClient(sid string) chatv1connect.ChatServiceClient {
	return chatv1connect.NewChatServiceClient(f.browser(sid), f.srv.URL+"/api")
}

func (f *fixture) identityClient(sid string) identityv1connect.IdentityServiceClient {
	return identityv1connect.NewIdentityServiceClient(f.browser(sid), f.srv.URL+"/api")
}

func (f *fixture) probeClient(sid string) *connect.Client[wrapperspb.StringValue, wrapperspb.StringValue] {
	return connect.NewClient[wrapperspb.StringValue, wrapperspb.StringValue](
		f.browser(sid), f.srv.URL+"/api"+probePath+"Probe", connect.WithProtoJSON())
}

func probe(c *connect.Client[wrapperspb.StringValue, wrapperspb.StringValue], name string) (string, error) {
	res, err := c.CallUnary(context.Background(), connect.NewRequest(wrapperspb.String(name)))
	if err != nil {
		return "", err
	}
	return res.Msg.GetValue(), nil
}

// reply is a raw HTTP answer.
type reply struct {
	status int
	header http.Header
	body   string
}

// do sends a request the way a script would: only the headers given, no cookie, no
// Origin, no protocol version unless they are in hdr.
func (f *fixture) do(method, path string, hdr map[string]string, body string) reply {
	f.t.Helper()
	req, err := http.NewRequestWithContext(f.t.Context(), method, f.srv.URL+path, strings.NewReader(body))
	if err != nil {
		f.t.Fatal(err)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	res, err := f.srv.Client().Do(req)
	if err != nil {
		f.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer func() { _ = res.Body.Close() }()
	raw, err := io.ReadAll(res.Body)
	if err != nil {
		f.t.Fatal(err)
	}
	return reply{status: res.StatusCode, header: res.Header, body: string(raw)}
}

// rpc is a POST of a JSON body to a Connect procedure with everything a page sends,
// then hdr applied over it; an empty value removes the header.
func (f *fixture) rpc(sid, procedure, body string, hdr map[string]string) reply {
	f.t.Helper()
	h := map[string]string{
		"Content-Type":             "application/json",
		"Connect-Protocol-Version": "1",
		"Origin":                   f.publicURL,
		"Cookie":                   identity.SessionCookie + "=" + sid,
	}
	for k, v := range hdr {
		if v == "" {
			delete(h, k)
		} else {
			h[k] = v
		}
	}
	return f.do(http.MethodPost, "/api"+procedure, h, body)
}

func (f *fixture) createChannel(sid, name string, kind chatv1.ChannelKind) string {
	f.t.Helper()
	res, err := f.chatClient(sid).CreateChannel(f.t.Context(), connect.NewRequest(&chatv1.CreateChannelRequest{Name: name, Kind: kind}))
	if err != nil {
		f.t.Fatalf("CreateChannel: %v", err)
	}
	return res.Msg.GetChannel().GetId()
}

func decodeError(t *testing.T, r reply) wireError {
	t.Helper()
	var we wireError
	if err := json.Unmarshal([]byte(r.body), &we); err != nil {
		t.Fatalf("body %q is not a Connect error: %v", r.body, err)
	}
	return we
}

// ---- the eight named tests --------------------------------------------------

func TestErrorInterceptor_MapsDomainErrorsAndHidesInternals(t *testing.T) {
	const secret = `pq: password authentication failed for user "kiwi_app" at 10.1.2.3:5432`
	consent := &google.ConsentRequiredError{URL: "/api/auth/meet-consent"}
	missing := fmt.Errorf("chat: channel %s: %w", uuid.NewString(), errs.ErrNotFound)
	// authz wraps the sentinel and the cause of a failed membership lookup side by
	// side, so the full text of a mapped error can carry the database's.
	lookupFailed := fmt.Errorf("authz: user message.post: membership lookup: %w: %w", errs.ErrForbidden, errors.New(secret))

	cases := []struct {
		name string
		err  error
		code connect.Code
		// message is all the client may read: the text of the sentinel, never the
		// context a domain wrapped it in.
		message string
	}{
		{name: "not found", err: errs.ErrNotFound, code: connect.CodeNotFound, message: "not found"},
		{name: "wrapped not found", err: missing, code: connect.CodeNotFound, message: "not found"},
		{name: "forbidden", err: fmt.Errorf("tasks: not yours: %w", errs.ErrForbidden), code: connect.CodePermissionDenied, message: "forbidden"},
		{name: "forbidden beside an infrastructure error", err: lookupFailed, code: connect.CodePermissionDenied, message: "forbidden"},
		{name: "conflict", err: fmt.Errorf("tasks: already confirmed: %w", errs.ErrConflict), code: connect.CodeAborted, message: "conflict"},
		{name: "invalid", err: fmt.Errorf("chat: body must be 1-8000 characters: %w", errs.ErrInvalid), code: connect.CodeInvalidArgument, message: "invalid"},
		{name: "consent required", err: consent, code: connect.CodeFailedPrecondition, message: "consent required"},
		{name: "wrapped consent required", err: fmt.Errorf("meetings: start: %w", consent), code: connect.CodeFailedPrecondition, message: "consent required"},
		{name: "internal", err: errors.New(secret), code: connect.CodeInternal, message: "internal error"},
		{name: "wrapped internal", err: fmt.Errorf("chat: lock channel: %w", errors.New(secret)), code: connect.CodeInternal, message: "internal error"},
		// Text alone is not a sentinel: only errors.Is decides.
		{name: "lookalike text", err: errors.New("not found: " + secret), code: connect.CodeInternal, message: "internal error"},
	}
	probes := map[string]probeCase{}
	for _, tc := range cases {
		probes[tc.name] = func(context.Context) (string, error) { return "", tc.err }
	}
	f := newFixture(t, probes)
	c := f.probeClient(f.signIn(f.alice))

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := probe(c, tc.name)
			ce := errorWith(t, err, tc.code)
			if ce.Message() != tc.message {
				t.Errorf("message = %q, want %q", ce.Message(), tc.message)
			}
			if strings.Contains(ce.Message(), "10.1.2.3") || strings.Contains(ce.Message(), "kiwi_app") {
				t.Errorf("message %q leaks the internal error", ce.Message())
			}
		})
	}

	t.Run("consent detail carries the URL", func(t *testing.T) {
		_, err := probe(c, "consent required")
		ce := errorWith(t, err, connect.CodeFailedPrecondition)
		if len(ce.Details()) != 1 {
			t.Fatalf("details = %d, want 1", len(ce.Details()))
		}
		v, err := ce.Details()[0].Value()
		if err != nil {
			t.Fatalf("detail: %v", err)
		}
		info, ok := v.(*errdetails.ErrorInfo)
		if !ok {
			t.Fatalf("detail is %T, want *errdetails.ErrorInfo", v)
		}
		if info.GetReason() != ConsentRequiredReason || info.GetDomain() != ErrorDomain || info.GetMetadata()[ConsentURLKey] != consent.URL {
			t.Errorf("detail = %v, want reason %s, domain %s and %s=%s", info, ConsentRequiredReason, ErrorDomain, ConsentURLKey, consent.URL)
		}
	})

	t.Run("nothing internal reaches the wire in either encoding", func(t *testing.T) {
		sid := f.signIn(f.alice)
		binary, err := proto.Marshal(wrapperspb.String("internal"))
		if err != nil {
			t.Fatal(err)
		}
		for contentType, body := range map[string]string{"application/json": `"internal"`, "application/proto": string(binary)} {
			r := f.rpc(sid, probePath+"Probe", body, map[string]string{"Content-Type": contentType})
			if r.status != http.StatusInternalServerError {
				t.Errorf("%s: status = %d, want 500 (body %q)", contentType, r.status, r.body)
			}
			if strings.Contains(r.body, "10.1.2.3") || strings.Contains(r.body, "kiwi_app") || strings.Contains(r.body, "password") {
				t.Errorf("%s: body %q leaks the internal error", contentType, r.body)
			}
			if we := decodeError(t, r); we.Code != "internal" || we.Message != "internal error" {
				t.Errorf("%s: body = %+v, want code internal and message %q", contentType, we, "internal error")
			}
		}
	})

	t.Run("the operator still sees the cause", func(t *testing.T) {
		if logs := f.logs.String(); !strings.Contains(logs, "password authentication failed") || !strings.Contains(logs, probePath+"Probe") {
			t.Errorf("the log does not name the failed procedure and its cause:\n%s", logs)
		}
	})
}

func TestAuthInterceptor_RejectsMissingOrExpiredSession(t *testing.T) {
	var ran atomic.Int32
	f := newFixture(t, map[string]probeCase{
		"run": func(ctx context.Context) (string, error) {
			ran.Add(1)
			a := ActorFrom(ctx)
			return fmt.Sprintf("%s|%s|%s", a.Kind, a.UserID, a.WorkspaceID), nil
		},
	})

	t.Run("a live session reaches the handler as a user actor of its workspace", func(t *testing.T) {
		got, err := probe(f.probeClient(f.signIn(f.alice)), "run")
		if err != nil {
			t.Fatalf("probe: %v", err)
		}
		if want := fmt.Sprintf("%s|%s|%s", authz.ActorUser, f.alice, f.ws); got != want {
			t.Errorf("actor = %q, want %q", got, want)
		}
		if _, err := f.chatClient(f.signIn(f.alice)).ListChannels(t.Context(), connect.NewRequest(&chatv1.ListChannelsRequest{})); err != nil {
			t.Errorf("ListChannels with a live session: %v", err)
		}
	})

	rejected := func(t *testing.T, sid string) {
		t.Helper()
		before := ran.Load()
		_, err := probe(f.probeClient(sid), "run")
		wantCode(t, err, connect.CodeUnauthenticated)
		_, err = f.chatClient(sid).ListChannels(t.Context(), connect.NewRequest(&chatv1.ListChannelsRequest{}))
		wantCode(t, err, connect.CodeUnauthenticated)
		_, err = f.identityClient(sid).IssueRealtimeTicket(t.Context(), connect.NewRequest(&identityv1.IssueRealtimeTicketRequest{}))
		wantCode(t, err, connect.CodeUnauthenticated)
		if ran.Load() != before {
			t.Error("the handler ran for a request without a live session")
		}
	}

	t.Run("no cookie", func(t *testing.T) { rejected(t, "") })
	t.Run("a session id nobody issued", func(t *testing.T) { rejected(t, "sid-"+uuid.NewString()) })

	t.Run("an empty cookie value", func(t *testing.T) {
		r := f.rpc("", probePath+"Probe", `"run"`, map[string]string{"Cookie": identity.SessionCookie + "="})
		if r.status != http.StatusUnauthorized {
			t.Errorf("status = %d, want 401 (body %q)", r.status, r.body)
		}
	})

	t.Run("an expired session", func(t *testing.T) {
		// Created almost a whole absolute lifetime ago, it has half a second left
		// and reading it cannot extend that: polling ends in the rejection.
		sid := "sid-" + uuid.NewString()
		created := time.Now().Add(-(cache.DefaultSessionAbsolute - 500*time.Millisecond))
		if err := f.cache.PutSession(t.Context(), sid, cache.SessionRecord{UserID: f.alice, WorkspaceID: f.ws, CreatedAt: created}); err != nil {
			t.Fatalf("PutSession: %v", err)
		}
		c := f.probeClient(sid)
		deadline := time.Now().Add(10 * time.Second)
		for {
			_, err := probe(c, "run")
			if err != nil {
				wantCode(t, err, connect.CodeUnauthenticated)
				return
			}
			if time.Now().After(deadline) {
				t.Fatal("the session never expired")
			}
			time.Sleep(20 * time.Millisecond)
		}
	})

	t.Run("a session ended by sign-out", func(t *testing.T) {
		sid := f.signIn(f.alice)
		if _, err := probe(f.probeClient(sid), "run"); err != nil {
			t.Fatalf("probe before sign-out: %v", err)
		}
		r := f.do(http.MethodPost, "/api/auth/logout", map[string]string{
			"Origin": f.publicURL,
			"Cookie": identity.SessionCookie + "=" + sid,
		}, "")
		if r.status != http.StatusNoContent {
			t.Fatalf("logout status = %d, want 204 (body %q)", r.status, r.body)
		}
		rejected(t, sid)
	})

	t.Run("the rejection is a Connect error answer", func(t *testing.T) {
		r := f.rpc("", probePath+"Probe", `"run"`, map[string]string{"Cookie": ""})
		if r.status != http.StatusUnauthorized {
			t.Errorf("status = %d, want 401", r.status)
		}
		if ct := r.header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("Content-Type = %q, want application/json", ct)
		}
		if we := decodeError(t, r); we.Code != "unauthenticated" || we.Message == "" {
			t.Errorf("body = %+v, want code unauthenticated and a message", we)
		}
	})
}

func TestConnectProtocolHeaderRequired(t *testing.T) {
	f := newFixture(t, nil)
	sid := f.signIn(f.alice)

	for _, procedure := range []string{
		"/kiwi.identity.v1.IdentityService/IssueRealtimeTicket",
		"/kiwi.chat.v1.ChatService/ListChannels",
	} {
		t.Run(procedure, func(t *testing.T) {
			// A browser can send this cross-site without a preflight: a POST with a JSON
			// body, the cookie and, on a hostile page, its own Origin. Only the header
			// is missing here; the session and the Origin are right.
			r := f.rpc(sid, procedure, "{}", map[string]string{"Connect-Protocol-Version": ""})
			if r.status != http.StatusBadRequest {
				t.Errorf("without the header: status = %d, want 400 (body %q)", r.status, r.body)
			}
			if we := decodeError(t, r); we.Code != "invalid_argument" {
				t.Errorf("without the header: code = %q, want invalid_argument", we.Code)
			}
			if r := f.rpc(sid, procedure, "{}", map[string]string{"Connect-Protocol-Version": "2"}); r.status != http.StatusBadRequest {
				t.Errorf("with a wrong version: status = %d, want 400", r.status)
			}
			if r := f.rpc(sid, procedure, "{}", nil); r.status != http.StatusOK {
				t.Errorf("with the header: status = %d, want 200 (body %q)", r.status, r.body)
			}
		})
	}
}

func TestCrossOriginPOSTRejected(t *testing.T) {
	f := newFixture(t, nil)
	sid := f.signIn(f.alice)
	channel := f.createChannel(sid, "general", chatv1.ChannelKind_CHANNEL_KIND_PUBLIC)

	send := func(msgID string, hdr map[string]string) reply {
		body := fmt.Sprintf(`{"channelId":%q,"clientMsgId":%q,"body":"hello"}`, channel, msgID)
		return f.rpc(sid, "/kiwi.chat.v1.ChatService/SendMessage", body, hdr)
	}
	stored := func() int {
		res, err := f.chatClient(sid).ListMessages(t.Context(), connect.NewRequest(&chatv1.ListMessagesRequest{ChannelId: channel}))
		if err != nil {
			t.Fatalf("ListMessages: %v", err)
		}
		return len(res.Msg.GetMessages())
	}

	for name, origin := range map[string]string{
		"another site":         "https://evil.example",
		"the same host, http":  strings.Replace(f.publicURL, "http://", "https://", 1),
		"a sandboxed page":     "null",
		"no Origin at all":     "",
		"the right host, path": f.publicURL + "/x",
	} {
		t.Run(name, func(t *testing.T) {
			r := send("m-"+name, map[string]string{"Origin": origin})
			if r.status != http.StatusForbidden {
				t.Errorf("status = %d, want 403 (body %q)", r.status, r.body)
			}
			if got := stored(); got != 0 {
				t.Errorf("%d messages stored by a rejected request", got)
			}
		})
	}

	t.Run("the page's own origin", func(t *testing.T) {
		if r := send("m-ok", nil); r.status != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body %q)", r.status, r.body)
		}
		if got := stored(); got != 1 {
			t.Errorf("stored = %d, want 1", got)
		}
	})
}

func TestSendMessage_RoundTripOverConnect_ReturnsAckWithSeq(t *testing.T) {
	f := newFixture(t, nil)
	aliceSID, bobSID := f.signIn(f.alice), f.signIn(f.bob)
	alice, bob := f.chatClient(aliceSID), f.chatClient(bobSID)
	channel := f.createChannel(aliceSID, "private-room", chatv1.ChannelKind_CHANNEL_KIND_PRIVATE)
	bobID := f.bob.String()

	send := func(c chatv1connect.ChatServiceClient, msgID, body string) (*chatv1.Message, error) {
		res, err := c.SendMessage(t.Context(), connect.NewRequest(&chatv1.SendMessageRequest{
			ChannelId: channel, ClientMsgId: msgID, Body: body,
			// What a user actor sends here is ignored: nobody posts as someone else.
			AuthorId: &bobID,
		}))
		if err != nil {
			return nil, err
		}
		return res.Msg.GetMessage(), nil
	}

	first, err := send(alice, "c1", "hello")
	if err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	if first.GetSeq() != 1 || first.GetBody() != "hello" || first.GetClientMsgId() != "c1" {
		t.Errorf("first ack = %v, want seq 1", first)
	}
	if first.GetAuthorId() != f.alice.String() {
		t.Errorf("author = %s, want the session's user %s", first.GetAuthorId(), f.alice)
	}
	second, err := send(alice, "c2", "again")
	if err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	if second.GetSeq() != 2 {
		t.Errorf("second seq = %d, want 2", second.GetSeq())
	}
	// A phone that sends twice lands once: the retry gets the same ack back.
	retry, err := send(alice, "c1", "hello")
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if retry.GetId() != first.GetId() || retry.GetSeq() != 1 {
		t.Errorf("retry = %v, want the first message again", retry)
	}

	list, err := alice.ListMessages(t.Context(), connect.NewRequest(&chatv1.ListMessagesRequest{ChannelId: channel}))
	if err != nil || len(list.Msg.GetMessages()) != 2 {
		t.Fatalf("ListMessages = %v, %v; want the two messages", list, err)
	}

	// The real service's refusal of a non-member arrives mapped from its sentinel,
	// as PermissionDenied with the sentinel's text and none of authz's wording.
	_, err = send(bob, "b1", "let me in")
	ce := errorWith(t, err, connect.CodePermissionDenied)
	if ce.Message() != "forbidden" {
		t.Errorf("non-member send: message = %q, want %q", ce.Message(), "forbidden")
	}
}

func TestIssueRealtimeTicket_SingleUseAndBoundToCaller(t *testing.T) {
	f := newFixture(t, nil)
	ctx := t.Context()
	issue := func(user uuid.UUID) string {
		res, err := f.identityClient(f.signIn(user)).IssueRealtimeTicket(ctx, connect.NewRequest(&identityv1.IssueRealtimeTicketRequest{}))
		if err != nil {
			t.Fatalf("IssueRealtimeTicket: %v", err)
		}
		if res.Msg.GetTicket() == "" {
			t.Fatal("empty ticket")
		}
		return res.Msg.GetTicket()
	}

	aliceTicket, bobTicket := issue(f.alice), issue(f.bob)
	if aliceTicket == bobTicket {
		t.Fatal("two callers got the same ticket")
	}

	rec, err := f.cache.RedeemTicket(ctx, aliceTicket)
	if err != nil || rec.UserID != f.alice || rec.WorkspaceID != f.ws {
		t.Errorf("Alice's ticket = %+v, %v; want her user and workspace", rec, err)
	}
	if _, err := f.cache.RedeemTicket(ctx, aliceTicket); !errors.Is(err, errs.ErrNotFound) {
		t.Errorf("second redeem err = %v, want ErrNotFound (single use)", err)
	}
	rec, err = f.cache.RedeemTicket(ctx, bobTicket)
	if err != nil || rec.UserID != f.bob || rec.WorkspaceID != f.ws {
		t.Errorf("Bob's ticket = %+v, %v; want his user and workspace", rec, err)
	}

	// Without a session no ticket is issued to anyone.
	_, err = f.identityClient("").IssueRealtimeTicket(ctx, connect.NewRequest(&identityv1.IssueRealtimeTicketRequest{}))
	wantCode(t, err, connect.CodeUnauthenticated)
}

func TestHealthzAndReadyzOutsideAuth(t *testing.T) {
	f := newFixture(t, map[string]probeCase{"ok": func(context.Context) (string, error) { return "ok", nil }})

	// The process's listener, composed the way the app does it: probes answered by
	// httpx, everything else, /api/ with its full path, by the shell.
	mux := http.NewServeMux()
	mux.Handle("/api/", f.handler)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
	srv := httpx.New(addr, mux)
	srv.DrainTimeout = time.Millisecond
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- srv.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("Run: %v", err)
		}
	})

	get := func(path string) (int, string) {
		t.Helper()
		res, err := probeGet(t, "http://"+addr+path)
		if err != nil {
			return 0, err.Error()
		}
		defer func() { _ = res.Body.Close() }()
		b, _ := io.ReadAll(res.Body)
		return res.StatusCode, string(b)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		if status, _ := get("/healthz"); status == http.StatusOK {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the listener never came up")
		}
		time.Sleep(10 * time.Millisecond)
	}

	// No cookie, no Origin, no header on any of these.
	if status, body := get("/healthz"); status != http.StatusOK {
		t.Errorf("/healthz = %d %q, want 200", status, body)
	}
	if status, _ := get("/readyz"); status != http.StatusServiceUnavailable {
		t.Errorf("/readyz before ready = %d, want 503", status)
	}
	srv.SetReady(true)
	if status, body := get("/readyz"); status != http.StatusOK {
		t.Errorf("/readyz = %d %q, want 200", status, body)
	}

	// The same listener still demands a session of an RPC.
	res, err := probePost(t, "http://"+addr+"/api"+probePath+"Probe", `"ok"`)
	if err != nil {
		t.Fatal(err)
	}
	_ = res.Body.Close()
	if res.StatusCode == http.StatusOK {
		t.Errorf("an RPC without a session answered %d on the process listener", res.StatusCode)
	}
}

func TestBlobRoute_IsOutsideSessionAuth_AndTokenChecked(t *testing.T) {
	f := newFixture(t, nil)
	ctx := t.Context()
	const key, contentType = "files/report.pdf", "application/pdf"
	payload := []byte("%PDF-1.4 a report")

	putURL, headers, err := f.store.SignedPut(ctx, key, contentType, 1<<10, time.Minute)
	if err != nil {
		t.Fatalf("SignedPut: %v", err)
	}
	if !strings.HasPrefix(putURL, f.publicURL+"/api/blob/") {
		t.Fatalf("signed URL %q is not under PublicURL %s", putURL, f.publicURL)
	}
	// What the browser sends: the signed URL's headers and its page's Origin, and no
	// cookie, because the token is the only credential of this route.
	page := map[string]string{"Origin": f.publicURL}
	for k, v := range headers {
		page[k] = v
	}
	path := strings.TrimPrefix(putURL, f.publicURL)

	if r := f.do(http.MethodPut, path, page, string(payload)); r.status != http.StatusOK {
		t.Fatalf("signed PUT without a cookie: status = %d (body %q), want 200", r.status, r.body)
	}
	if attrs, err := f.store.Head(ctx, key); err != nil || attrs.Size != int64(len(payload)) {
		t.Fatalf("Head = %+v, %v; want the uploaded object", attrs, err)
	}

	getURL, err := f.store.SignedGet(ctx, key, time.Minute)
	if err != nil {
		t.Fatalf("SignedGet: %v", err)
	}
	if r := f.do(http.MethodGet, strings.TrimPrefix(getURL, f.publicURL), nil, ""); r.status != http.StatusOK || r.body != string(payload) {
		t.Errorf("signed GET without a cookie: status = %d, body %q; want 200 and the object", r.status, r.body)
	}

	t.Run("no token is refused by the token check", func(t *testing.T) {
		for _, p := range []string{"/api/blob/", "/api/blob/not-a-token", "/api/blob/" + strings.Repeat("A", 40) + "." + strings.Repeat("B", 43)} {
			r := f.do(http.MethodPut, p, page, string(payload))
			if r.status != http.StatusForbidden {
				t.Errorf("PUT %s: status = %d, want 403", p, r.status)
			}
			// The store answers with the status text only; the origin check's own
			// answer would mean this 403 proves nothing about the token.
			if strings.TrimSpace(r.body) != "Forbidden" {
				t.Errorf("PUT %s: body %q, want the store's refusal", p, r.body)
			}
		}
	})

	t.Run("a tampered token is refused", func(t *testing.T) {
		bad := []byte(path)
		bad[len(bad)-20] ^= 1 // one flipped bit inside the signature; the last character's low bits are padding the decoder ignores
		if r := f.do(http.MethodPut, string(bad), page, string(payload)); r.status != http.StatusForbidden {
			t.Errorf("status = %d, want 403", r.status)
		}
	})

	t.Run("the origin check still guards the route", func(t *testing.T) {
		putURL, headers, err := f.store.SignedPut(ctx, "files/other.pdf", contentType, 1<<10, time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		foreign := map[string]string{"Origin": "https://evil.example"}
		for k, v := range headers {
			foreign[k] = v
		}
		r := f.do(http.MethodPut, strings.TrimPrefix(putURL, f.publicURL), foreign, string(payload))
		if r.status != http.StatusForbidden || !strings.Contains(r.body, "cross-origin") {
			t.Errorf("cross-origin signed PUT: status = %d, body %q; want the origin check's 403", r.status, r.body)
		}
		if _, err := f.store.Head(ctx, "files/other.pdf"); err == nil {
			t.Error("a cross-origin PUT stored an object")
		}
	})

	t.Run("the RPC routes still need a session", func(t *testing.T) {
		_, err := f.chatClient("").ListChannels(ctx, connect.NewRequest(&chatv1.ListChannelsRequest{}))
		wantCode(t, err, connect.CodeUnauthenticated)
	})
}

// ---- the rest of the shell --------------------------------------------------

func TestRecover_PanicAnswersInternalAndTheServerSurvives(t *testing.T) {
	f := newFixture(t, map[string]probeCase{
		"panic": func(context.Context) (string, error) {
			panic("boom: " + base64.StdEncoding.EncodeToString([]byte("secret")))
		},
		"ok": func(context.Context) (string, error) { return "ok", nil },
	})
	sid := f.signIn(f.alice)

	r := f.rpc(sid, probePath+"Probe", `"panic"`, nil)
	if r.status != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500 (body %q)", r.status, r.body)
	}
	if we := decodeError(t, r); we.Code != "internal" || we.Message != "internal error" {
		t.Errorf("body = %+v, want code internal and message %q", we, "internal error")
	}
	if strings.Contains(r.body, "boom") || strings.Contains(r.body, "secret") {
		t.Errorf("body %q leaks the panic value", r.body)
	}
	if logs := f.logs.String(); !strings.Contains(logs, "boom") {
		t.Errorf("the panic was not logged:\n%s", logs)
	}
	if got, err := probe(f.probeClient(sid), "ok"); err != nil || got != "ok" {
		t.Errorf("the next request = %q, %v; want the server to carry on", got, err)
	}
}

// stubStack is the shell over fakes: it needs no database, and its Auth handler
// is not identity's, so only the shell's own guards can stop a request.
type stubStack struct {
	srv      *httptest.Server
	authHits atomic.Int32
	blobHits atomic.Int32
}

const stubPublicURL = "https://kiwi.example"

type sessionFunc func(ctx context.Context, id string) (cache.SessionRecord, error)

func (f sessionFunc) GetSession(ctx context.Context, id string) (cache.SessionRecord, error) {
	return f(ctx, id)
}

func newStubStack(t *testing.T, sessions SessionStore, logs io.Writer) *stubStack {
	t.Helper()
	s := &stubStack{}
	auth := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		s.authHits.Add(1)
		w.WriteHeader(http.StatusNoContent)
	})
	blobStub := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		s.blobHits.Add(1)
		w.WriteHeader(http.StatusOK)
	})
	logger := slog.New(slog.NewTextHandler(logs, nil))
	h := New(Deps{
		PublicURL: stubPublicURL,
		Sessions:  sessions,
		Auth:      auth,
		Blob:      blobStub,
		Handlers: []Handler{probeHandler(logger, map[string]probeCase{
			"ok": func(context.Context) (string, error) { return "ok", nil },
		})},
		Logger: logger,
	})
	s.srv = httptest.NewServer(h)
	t.Cleanup(s.srv.Close)
	return s
}

func (s *stubStack) do(t *testing.T, method, path string, hdr map[string]string) int {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, s.srv.URL+path, strings.NewReader(`"ok"`))
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	res, err := s.srv.Client().Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	_ = res.Body.Close()
	return res.StatusCode
}

func TestAuthRoutes_OutsideSessionAuth_LogoutInsideOriginCheck(t *testing.T) {
	var lookups atomic.Int32
	s := newStubStack(t, sessionFunc(func(context.Context, string) (cache.SessionRecord, error) {
		lookups.Add(1)
		return cache.SessionRecord{}, errs.ErrNotFound
	}), io.Discard)

	// The sign-in routes answer someone who has no session yet.
	if status := s.do(t, http.MethodGet, "/api/auth/login", nil); status != http.StatusNoContent {
		t.Errorf("GET /api/auth/login = %d, want the auth handler's answer", status)
	}
	if lookups.Load() != 0 {
		t.Errorf("session lookups = %d on an /api/auth/ route, want 0", lookups.Load())
	}

	// Sign-out is a POST: the shell's origin check stands in front of it whatever
	// the auth handler does itself (this one does nothing).
	hits := s.authHits.Load()
	for name, hdr := range map[string]map[string]string{
		"another site": {"Origin": "https://evil.example"},
		"no Origin":    nil,
	} {
		if status := s.do(t, http.MethodPost, "/api/auth/logout", hdr); status != http.StatusForbidden {
			t.Errorf("POST /api/auth/logout from %s = %d, want 403", name, status)
		}
	}
	if s.authHits.Load() != hits {
		t.Error("a cross-origin sign-out reached the auth handler")
	}
	if status := s.do(t, http.MethodPost, "/api/auth/logout", map[string]string{"Origin": stubPublicURL}); status != http.StatusNoContent {
		t.Errorf("POST /api/auth/logout from the page = %d, want 204", status)
	}

	// A path no domain registered is a 404, not a request for a session.
	if status := s.do(t, http.MethodGet, "/api/kiwi.nobody.v1.Nothing/Nothing", nil); status != http.StatusNotFound {
		t.Errorf("an unregistered route = %d, want 404", status)
	}
	if status := s.do(t, http.MethodGet, "/elsewhere", nil); status != http.StatusNotFound {
		t.Errorf("a path outside /api/ = %d, want 404", status)
	}
}

func TestAuthInterceptor_SessionStoreFailureIsInternalNotUnauthenticated(t *testing.T) {
	const cause = "dial tcp 10.0.0.5:6379: connect: connection refused"
	logs := &syncBuffer{}
	s := newStubStack(t, sessionFunc(func(context.Context, string) (cache.SessionRecord, error) {
		return cache.SessionRecord{}, fmt.Errorf("cache: get session: %w", errors.New(cause))
	}), logs)

	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, s.srv.URL+"/api"+probePath+"Probe", strings.NewReader(`"ok"`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Connect-Protocol-Version", "1")
	req.Header.Set("Origin", stubPublicURL)
	req.Header.Set("Cookie", identity.SessionCookie+"=whatever")
	res, err := s.srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	raw, _ := io.ReadAll(res.Body)

	// A broken Valkey is not a signed-out user: answering 401 would log everyone out
	// in the client for an outage.
	if res.StatusCode != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500 (body %q)", res.StatusCode, raw)
	}
	if we := decodeError(t, reply{body: string(raw)}); we.Code != "internal" || we.Message != "internal error" {
		t.Errorf("body = %+v, want code internal and message %q", we, "internal error")
	}
	if strings.Contains(string(raw), "10.0.0.5") {
		t.Errorf("body %q leaks the cause", raw)
	}
	if !strings.Contains(logs.String(), "connection refused") {
		t.Errorf("the cause was not logged:\n%s", logs.String())
	}
}

func TestNew_RefusesIncompleteDeps(t *testing.T) {
	ok := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})
	sessions := sessionFunc(func(context.Context, string) (cache.SessionRecord, error) { return cache.SessionRecord{}, nil })
	valid := func() Deps {
		return Deps{PublicURL: stubPublicURL, Sessions: sessions, Auth: ok, Blob: ok, Handlers: []Handler{{Path: "/kiwi.x.v1.X/", H: ok}}}
	}
	cases := map[string]func(*Deps){
		"no session store":          func(d *Deps) { d.Sessions = nil },
		"no auth handler":           func(d *Deps) { d.Auth = nil },
		"no blob handler":           func(d *Deps) { d.Blob = nil },
		"a relative public URL":     func(d *Deps) { d.PublicURL = "/api" },
		"a handler without a path":  func(d *Deps) { d.Handlers[0].Path = "" },
		"a path without its slash":  func(d *Deps) { d.Handlers[0].Path = "/kiwi.x.v1.X" },
		"a path without a start":    func(d *Deps) { d.Handlers[0].Path = "kiwi.x.v1.X/" },
		"a handler that is nil":     func(d *Deps) { d.Handlers[0].H = nil },
		"a path taken by sign-in":   func(d *Deps) { d.Handlers[0].Path = "/auth/" },
		"two handlers on one route": func(d *Deps) { d.Handlers = append(d.Handlers, d.Handlers[0]) },
	}
	if got := New(valid()); got == nil {
		t.Fatal("New of valid deps returned nil")
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			d := valid()
			mutate(&d)
			defer func() {
				if recover() == nil {
					t.Error("New did not panic")
				}
			}()
			New(d)
		})
	}
}
