package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/coder/websocket"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/redis/go-redis/v9"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	agentsv1 "kiwi/gen/kiwi/agents/v1"
	"kiwi/gen/kiwi/agents/v1/agentsv1connect"
	chatv1 "kiwi/gen/kiwi/chat/v1"
	"kiwi/gen/kiwi/chat/v1/chatv1connect"
	eventsv1 "kiwi/gen/kiwi/events/v1"
	filesv1 "kiwi/gen/kiwi/files/v1"
	"kiwi/gen/kiwi/files/v1/filesv1connect"
	identityv1 "kiwi/gen/kiwi/identity/v1"
	"kiwi/gen/kiwi/identity/v1/identityv1connect"
	meetingsv1 "kiwi/gen/kiwi/meetings/v1"
	"kiwi/gen/kiwi/meetings/v1/meetingsv1connect"
	realtimev1 "kiwi/gen/kiwi/realtime/v1"
	tasksv1 "kiwi/gen/kiwi/tasks/v1"
	"kiwi/gen/kiwi/tasks/v1/tasksv1connect"
	"kiwi/internal/api"
	"kiwi/internal/identity"
	"kiwi/internal/platform/authz"
	"kiwi/internal/platform/bus"
	"kiwi/internal/platform/cache"
	"kiwi/internal/platform/config"
	"kiwi/internal/platform/db"
	"kiwi/internal/platform/events"
	"kiwi/internal/platform/google"
	"kiwi/internal/platform/llm"
	"kiwi/internal/platform/outbox"
	"kiwi/internal/testkit/containers"
	"kiwi/internal/testkit/dbtest"
	"kiwi/internal/testkit/fakegoogle"
	"kiwi/internal/testkit/fakellm"
	"kiwi/internal/testkit/fakeoidc"
	"kiwi/internal/testkit/fakeoidc/fakeoidctest"
)

// One plain Valkey serves the package. Every test signs in users and makes channels of
// its own, so the keys of one test never meet another's.
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

func sharedValkey(t *testing.T) string {
	t.Helper()
	valkeyOnce.Do(func() { valkeyURL, valkeyErr = bootValkey() })
	if valkeyErr != nil {
		t.Fatalf("valkey: %v", valkeyErr)
	}
	return valkeyURL
}

// ---- helpers ----------------------------------------------------------------

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// redisLogger sends go-redis's own log lines (it writes them with the standard logger,
// not slog) to slog, where captureLogs collects them: a test that takes Valkey away
// provokes "failed to dial" lines that belong in the buffer, not in the test output.
type redisLogger struct{}

func (redisLogger) Printf(ctx context.Context, format string, v ...any) {
	slog.WarnContext(ctx, "redis: "+fmt.Sprintf(format, v...))
}

// captureLogs sends slog output to a buffer for the rest of the test, so what the app
// logs while a dependency is down is read by the test, not printed. The buffer is shown
// when the test fails.
func captureLogs(t *testing.T) func() string {
	t.Helper()
	prev := slog.Default()
	buf := &syncBuffer{}
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	redis.SetLogger(redisLogger{})
	t.Cleanup(func() {
		slog.SetDefault(prev)
		redis.SetLogger(stderrRedisLogger{log.New(os.Stderr, "redis: ", log.LstdFlags|log.Lshortfile)})
		if t.Failed() {
			t.Logf("logs of the app:\n%s", buf.String())
		}
	})
	return buf.String
}

// stderrRedisLogger is go-redis's own default, put back when a test is done.
type stderrRedisLogger struct{ l *log.Logger }

func (s stderrRedisLogger) Printf(_ context.Context, format string, v ...any) {
	_ = s.l.Output(2, fmt.Sprintf(format, v...))
}

// eventually polls cond until it holds; it fails the test after 15 s. It waits for
// server state, it never asserts on elapsed time.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// proxy forwards TCP to target and can be cut and restored, which is how a test takes
// Postgres or Valkey away from the app and brings it back on the same address.
type proxy struct {
	target string
	addr   string

	mu     sync.Mutex
	ln     net.Listener
	conns  map[net.Conn]struct{}
	closed bool
}

func newProxy(t *testing.T, target string) *proxy {
	t.Helper()
	p := &proxy{target: target, conns: map[net.Conn]struct{}{}}
	if err := p.listen("127.0.0.1:0"); err != nil {
		t.Fatalf("proxy listen: %v", err)
	}
	t.Cleanup(p.shutdown)
	return p
}

func (p *proxy) listen(addr string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	p.mu.Lock()
	p.ln, p.addr = ln, ln.Addr().String()
	p.mu.Unlock()
	go p.accept(ln)
	return nil
}

func (p *proxy) accept(ln net.Listener) {
	for {
		in, err := ln.Accept()
		if err != nil {
			return
		}
		out, err := net.Dial("tcp", p.target)
		if err != nil {
			_ = in.Close()
			continue
		}
		p.mu.Lock()
		if p.closed {
			p.mu.Unlock()
			_ = in.Close()
			_ = out.Close()
			return
		}
		p.conns[in], p.conns[out] = struct{}{}, struct{}{}
		p.mu.Unlock()
		go func() { _, _ = io.Copy(out, in); _ = out.Close(); _ = in.Close() }()
		go func() { _, _ = io.Copy(in, out); _ = in.Close(); _ = out.Close() }()
	}
}

// cut refuses new connections and drops the open ones: the server is gone.
func (p *proxy) cut() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.ln != nil {
		_ = p.ln.Close()
		p.ln = nil
	}
	for c := range p.conns {
		_ = c.Close()
	}
	p.conns = map[net.Conn]struct{}{}
}

// restore listens on the address it had.
func (p *proxy) restore(t *testing.T) {
	t.Helper()
	p.mu.Lock()
	addr := p.addr
	p.mu.Unlock()
	if err := p.listen(addr); err != nil {
		t.Fatalf("proxy restore: %v", err)
	}
}

func (p *proxy) shutdown() {
	p.mu.Lock()
	p.closed = true
	p.mu.Unlock()
	p.cut()
}

// spyBus is the in-process bus with a record of what the app does to it, in order, and
// the means to replace the handler of a consumer with a test's.
type spyBus struct {
	*bus.Memory

	mu     sync.Mutex
	events []string
	stubs  map[string]bus.Handler
}

func newSpyBus() *spyBus {
	return &spyBus{Memory: bus.NewMemory(bus.MemoryOptions{Backoff: []time.Duration{0}}), stubs: map[string]bus.Handler{}}
}

// stub replaces the handler of consumer; call it before the app starts.
func (s *spyBus) stub(consumer string, h bus.Handler) { s.stubs[consumer] = h }

func (s *spyBus) record(what string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, what)
}

func (s *spyBus) log() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.events)
}

func (s *spyBus) Register(consumer string, topics []string, h bus.Handler) {
	s.record("register:" + consumer)
	if stub, ok := s.stubs[consumer]; ok {
		h = stub
	}
	s.Memory.Register(consumer, topics, func(ctx context.Context, m bus.Message) error {
		s.record("deliver:" + consumer)
		return h(ctx, m)
	})
}

func (s *spyBus) Publish(ctx context.Context, m bus.Message) error {
	s.record("publish:" + m.Topic)
	return s.Memory.Publish(ctx, m)
}

// registered lists the consumers that have been registered, sorted.
func (s *spyBus) registered() []string {
	var names []string
	for _, e := range s.log() {
		if name, ok := strings.CutPrefix(e, "register:"); ok {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	return names
}

// setup is what a test may change before the app starts.
type setup struct {
	cfg    config.Config
	opts   []Option
	env    *dbtest.Env
	llm    *fakellm.Client
	google *fakegoogle.Client
}

func withOption(o Option) func(*setup) { return func(s *setup) { s.opts = append(s.opts, o) } }

func withRoles(roles ...config.Role) func(*setup) {
	return func(s *setup) { s.cfg.Roles = roles }
}

func withValkey(u string) func(*setup) { return func(s *setup) { s.cfg.ValkeyURL = u } }

// withMemoryCache runs the minimal profile's cache: in the process, with the sessions in
// Postgres. The Valkey address points at nothing, so a test that passes proves the app
// never dials it.
func withMemoryCache() func(*setup) {
	return func(s *setup) {
		s.cfg.CacheProvider = config.CacheMemory
		s.cfg.ValkeyURL = "redis://127.0.0.1:1"
	}
}

// withWebDir has the app serve the built web app found in dir (KIWI_WEB_DIR).
func withWebDir(dir string) func(*setup) { return func(s *setup) { s.cfg.WebDir = dir } }

// eachCache runs fn once per cache provider, as subtests: what the app does must not
// depend on whether the cache is Valkey or the process's own.
func eachCache(t *testing.T, fn func(t *testing.T, mods ...func(*setup))) {
	t.Helper()
	t.Run("valkey", func(t *testing.T) { fn(t) })
	t.Run("memory", func(t *testing.T) { fn(t, withMemoryCache()) })
}

// withDatabaseVia points both pools at addr, a proxy in front of the test's Postgres.
func withDatabaseVia(addr string) func(*setup) {
	return func(s *setup) {
		s.cfg.DatabaseURL = viaProxy(s.env.AppURL, addr)
		s.cfg.DatabaseSystemURL = viaProxy(s.env.SystemURL, addr)
	}
}

func viaProxy(raw, addr string) string {
	u, err := url.Parse(raw)
	if err != nil {
		panic(err)
	}
	u.Host = addr
	return u.String()
}

// rig is one running app: every role, on a real Postgres and Valkey, behind a loopback
// listener whose address is the public URL, with a fake model, a fake Google and a fake
// OpenID provider.
type rig struct {
	t      *testing.T
	setup  *setup
	ln     net.Listener // the listener of the running app
	app    *App
	env    *dbtest.Env
	db     *db.DB
	rdb    *redis.Client // observes Valkey directly
	llm    *fakellm.Client
	google *fakegoogle.Client
	URL    string
	logs   func() string
	ws     uuid.UUID // the workspace of the members

	cancel context.CancelFunc
	done   chan error
	once   sync.Once
	runErr error
}

// the workspace of the rig's domain: alice, bob and dan are its members, so the first
// sign-in of each adopts a placeholder user as the sign-in of a seeded demo does.
var members = []string{fakeoidc.AliceEmail, fakeoidc.BobEmail, "dan@" + fakeoidc.DefaultDomain}

// newRig builds the app of a test without starting it; Run is the test's to call.
func newRig(t *testing.T, mods ...func(*setup)) *rig {
	t.Helper()
	logs := captureLogs(t)
	valkey := sharedValkey(t)
	env := dbtest.Start(t)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	publicURL := "http://" + ln.Addr().String()
	idp := fakeoidctest.New(t,
		fakeoidctest.WithRedirectURIs(publicURL+"/api/auth/callback"),
		fakeoidctest.WithUsers(
			fakeoidc.User{Email: members[0], Name: "Alice"},
			fakeoidc.User{Email: members[1], Name: "Bob"},
			fakeoidc.User{Email: members[2], Name: "Dan"},
		))

	s := &setup{
		env:    env,
		llm:    fakellm.New(),
		google: fakegoogle.New(),
		cfg: config.Config{
			Env:               "local",
			Roles:             []config.Role{config.RoleAPI, config.RoleRealtime, config.RoleWorker, config.RoleAgents},
			PublicURL:         publicURL,
			DatabaseURL:       env.AppURL,
			DatabaseSystemURL: env.SystemURL,
			ValkeyURL:         valkey,
			BusProvider:       "memory",
			BlobURL:           (&url.URL{Scheme: "file", Path: t.TempDir()}).String(),
			BlobSigningKey:    bytes.Repeat([]byte{0x24}, 32),
			LLMProvider:       "openai",
			OIDCIssuer:        idp.Issuer(),
			OAuthClientID:     idp.ClientID(),
			OAuthClientSecret: idp.ClientSecret(),
			TokenEncKey:       bytes.Repeat([]byte{0x42}, 32),
			WorkspaceName:     "Acme Corp",
			WorkspaceDomain:   fakeoidc.DefaultDomain,
		},
	}
	for _, mod := range mods {
		mod(s)
	}
	opts := append([]Option{
		WithLLM(s.llm), WithGoogle(s.google), WithListener(ln),
		WithDrainTimeout(0), WithPollInterval(time.Hour),
	}, s.opts...)
	a, err := New(t.Context(), s.cfg, opts...)
	if err != nil {
		_ = ln.Close()
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(a.Close)

	d, err := db.Open(t.Context(), config.Config{DatabaseURL: env.AppURL, DatabaseSystemURL: env.SystemURL})
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(d.Close)
	ws, _, err := identity.EnsureSeedWorkspace(t.Context(), d, fakeoidc.DefaultDomain, members)
	if err != nil {
		t.Fatalf("seed the workspace: %v", err)
	}
	ro, err := redis.ParseURL(valkey)
	if err != nil {
		t.Fatal(err)
	}
	rdb := redis.NewClient(ro)
	t.Cleanup(func() { _ = rdb.Close() })

	return &rig{t: t, setup: s, ln: ln, app: a, env: env, db: d, rdb: rdb, llm: s.llm, google: s.google, URL: publicURL, logs: logs, ws: ws}
}

// restart stops the app and starts a new one on the same address, over the same
// database, as a redeploy does: nothing but Postgres (and Valkey, if it is the cache)
// carries over. It returns once /readyz says 200.
func (r *rig) restart() {
	r.t.Helper()
	if err := r.stop(); err != nil {
		r.t.Fatalf("stopping the app: %v", err)
	}
	r.app.Close()
	ln, err := net.Listen("tcp", strings.TrimPrefix(r.URL, "http://"))
	if err != nil {
		r.t.Fatalf("listen again on %s: %v", r.URL, err)
	}
	opts := append([]Option{
		WithLLM(r.setup.llm), WithGoogle(r.setup.google), WithListener(ln),
		WithDrainTimeout(0), WithPollInterval(time.Hour),
	}, r.setup.opts...)
	a, err := New(r.t.Context(), r.setup.cfg, opts...)
	if err != nil {
		_ = ln.Close()
		r.t.Fatalf("New after the restart: %v", err)
	}
	r.t.Cleanup(a.Close)
	r.ln, r.app, r.once, r.runErr = ln, a, sync.Once{}, nil
	r.start()
}

// start runs the app and returns once /readyz says 200.
func (r *rig) start() {
	r.t.Helper()
	r.run()
	eventually(r.t, "/readyz to answer 200", func() bool { return r.status("/readyz") == http.StatusOK })
}

// run starts Run without waiting for readiness.
func (r *rig) run() {
	r.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	r.cancel = cancel
	r.done = make(chan error, 1)
	go func() { r.done <- r.app.Run(ctx) }()
	r.t.Cleanup(func() {
		if err := r.stop(); err != nil {
			r.t.Errorf("Run returned %v after its context ended, want nil", err)
		}
	})
}

// stop cancels Run's context and waits for it to return; it is safe to call twice.
func (r *rig) stop() error {
	r.once.Do(func() {
		if r.cancel == nil {
			return
		}
		r.cancel()
		select {
		case r.runErr = <-r.done:
		case <-time.After(60 * time.Second):
			r.runErr = errors.New("Run did not return within 60 s of its context ending")
		}
	})
	return r.runErr
}

func (r *rig) status(path string) int {
	r.t.Helper()
	resp, err := (&http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}}).Get(r.URL + path)
	if err != nil {
		return 0
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode
}

// enqueue writes an outbox row as kiwi_owner, as a domain's transaction would.
func (r *rig) enqueue(ev outbox.Event) {
	r.t.Helper()
	ctx := r.t.Context()
	conn, err := pgx.Connect(ctx, r.env.OwnerURL)
	if err != nil {
		r.t.Fatalf("connect as owner: %v", err)
	}
	defer func() { _ = conn.Close(ctx) }()
	tx, err := conn.Begin(ctx)
	if err != nil {
		r.t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := outbox.Enqueue(ctx, tx, ev); err != nil {
		r.t.Fatalf("enqueue: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		r.t.Fatal(err)
	}
}

// ownerQuery runs a one-value query as kiwi_owner.
func (r *rig) ownerQuery(sql string, dest any, args ...any) {
	r.t.Helper()
	ctx := r.t.Context()
	conn, err := pgx.Connect(ctx, r.env.OwnerURL)
	if err != nil {
		r.t.Fatalf("connect as owner: %v", err)
	}
	defer func() { _ = conn.Close(ctx) }()
	if err := conn.QueryRow(ctx, sql, args...).Scan(dest); err != nil {
		r.t.Fatalf("%s: %v", sql, err)
	}
}

// workspaceID is the id of the workspace the members belong to.
func (r *rig) workspaceID() uuid.UUID { return r.ws }

// docAnalyzeRow is an outbox event for a consumer to receive: a doc.uploaded whose file
// does not exist, which a stubbed doc.analyze acknowledges.
func docUploaded(ws uuid.UUID) outbox.Event {
	payload, err := protojson.MarshalOptions{UseProtoNames: true}.Marshal(&eventsv1.DocUploaded{
		WorkspaceId: ws.String(), ChannelId: uuid.NewString(), FileId: uuid.NewString()})
	if err != nil {
		panic(err)
	}
	return outbox.Event{Topic: events.TopicDocUploaded, Key: uuid.NewString(), Payload: payload}
}

// ---- sign-in and clients ----------------------------------------------------

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

var noRedirect = &http.Client{
	Timeout:       10 * time.Second,
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

// session is a signed-in browser: its cookie and Origin go with every request.
type session struct {
	r      *rig
	email  string
	cookie string
	http   *http.Client
	userID string
}

// signIn runs the whole sign-in through the fake provider as email: the login route,
// the provider's authorization endpoint with the account chosen, and the callback.
func (r *rig) signIn(email string) *session {
	r.t.Helper()
	get := func(target string, cookies ...*http.Cookie) *http.Response {
		req, err := http.NewRequestWithContext(r.t.Context(), http.MethodGet, target, nil)
		if err != nil {
			r.t.Fatal(err)
		}
		for _, c := range cookies {
			req.AddCookie(c)
		}
		resp, err := noRedirect.Do(req)
		if err != nil {
			r.t.Fatalf("GET %s: %v", target, err)
		}
		_ = resp.Body.Close()
		return resp
	}
	cookieOf := func(resp *http.Response, name string) *http.Cookie {
		for _, c := range resp.Cookies() {
			if c.Name == name {
				return c
			}
		}
		return nil
	}

	login := get(r.URL + "/api/auth/login?return_to=%2F")
	if login.StatusCode != http.StatusFound {
		r.t.Fatalf("login = %d, want a redirect to the provider", login.StatusCode)
	}
	bind := cookieOf(login, "__Host-kiwi_oauth")
	if bind == nil {
		r.t.Fatal("login set no binding cookie")
	}
	authURL, err := url.Parse(login.Header.Get("Location"))
	if err != nil {
		r.t.Fatal(err)
	}
	q := authURL.Query()
	q.Set("login_hint", email)
	authURL.RawQuery = q.Encode()
	approve := get(authURL.String())
	if approve.StatusCode != http.StatusFound {
		r.t.Fatalf("the provider answered %d, want a redirect to the callback", approve.StatusCode)
	}
	callback := get(approve.Header.Get("Location"), bind)
	sid := cookieOf(callback, identity.SessionCookie)
	if sid == nil {
		r.t.Fatalf("sign-in as %s issued no session: %d %s", email, callback.StatusCode, callback.Header.Get("Location"))
	}

	s := &session{r: r, email: email, cookie: sid.Value}
	s.http = &http.Client{
		Timeout: 30 * time.Second,
		Transport: headerTransport{base: http.DefaultTransport, header: http.Header{
			"Origin": {r.URL},
			"Cookie": {identity.SessionCookie + "=" + sid.Value},
		}},
	}
	me, err := s.identity().GetMe(r.t.Context(), connect.NewRequest(&identityv1.GetMeRequest{}))
	if err != nil {
		r.t.Fatalf("GetMe as %s: %v", email, err)
	}
	s.userID = me.Msg.GetUser().GetId()
	return s
}

func (s *session) base() string { return s.r.URL + "/api" }

func (s *session) identity() identityv1connect.IdentityServiceClient {
	return identityv1connect.NewIdentityServiceClient(s.http, s.base())
}
func (s *session) chat() chatv1connect.ChatServiceClient {
	return chatv1connect.NewChatServiceClient(s.http, s.base())
}
func (s *session) files() filesv1connect.FileServiceClient {
	return filesv1connect.NewFileServiceClient(s.http, s.base())
}
func (s *session) meetings() meetingsv1connect.MeetingServiceClient {
	return meetingsv1connect.NewMeetingServiceClient(s.http, s.base())
}
func (s *session) tasks() tasksv1connect.TaskServiceClient {
	return tasksv1connect.NewTaskServiceClient(s.http, s.base())
}
func (s *session) agents() agentsv1connect.AgentServiceClient {
	return agentsv1connect.NewAgentServiceClient(s.http, s.base())
}

// newChannel makes a private channel as s and returns its id.
func (s *session) newChannel(name string) string {
	s.r.t.Helper()
	res, err := s.chat().CreateChannel(s.r.t.Context(), connect.NewRequest(&chatv1.CreateChannelRequest{
		Name: name, Kind: chatv1.ChannelKind_CHANNEL_KIND_PRIVATE}))
	if err != nil {
		s.r.t.Fatalf("CreateChannel: %v", err)
	}
	return res.Msg.GetChannel().GetId()
}

func (s *session) addMember(channel string, who *session) {
	s.r.t.Helper()
	if _, err := s.chat().AddMember(s.r.t.Context(), connect.NewRequest(&chatv1.AddMemberRequest{
		ChannelId: channel, UserId: who.userID})); err != nil {
		s.r.t.Fatalf("AddMember: %v", err)
	}
}

func (s *session) send(channel, body string, mentions ...*chatv1.Mention) *chatv1.Message {
	s.r.t.Helper()
	res, err := s.chat().SendMessage(s.r.t.Context(), connect.NewRequest(&chatv1.SendMessageRequest{
		ChannelId: channel, ClientMsgId: uuid.NewString(), Body: body, Mentions: mentions}))
	if err != nil {
		s.r.t.Fatalf("SendMessage: %v", err)
	}
	return res.Msg.GetMessage()
}

// socket is one WebSocket of a signed-in user.
type socket struct {
	t *testing.T
	c *websocket.Conn
}

// dialSocket redeems a fresh ticket from an Origin (the page's own when origin is "").
func (s *session) dialSocket(origin string) (*socket, *http.Response, error) {
	s.r.t.Helper()
	tk, err := s.identity().IssueRealtimeTicket(s.r.t.Context(), connect.NewRequest(&identityv1.IssueRealtimeTicketRequest{}))
	if err != nil {
		s.r.t.Fatalf("IssueRealtimeTicket: %v", err)
	}
	if origin == "" {
		origin = s.r.URL
	}
	ctx, cancel := context.WithTimeout(s.r.t.Context(), 10*time.Second)
	defer cancel()
	c, resp, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(s.r.URL, "http")+"/ws?ticket="+url.QueryEscape(tk.Msg.GetTicket()),
		&websocket.DialOptions{HTTPHeader: http.Header{"Origin": {origin}}})
	if err != nil {
		return nil, resp, err
	}
	s.r.t.Cleanup(func() { _ = c.CloseNow() })
	return &socket{t: s.r.t, c: c}, resp, nil
}

func (s *session) socket() *socket {
	s.r.t.Helper()
	ws, _, err := s.dialSocket("")
	if err != nil {
		s.r.t.Fatalf("dial the WebSocket: %v", err)
	}
	return ws
}

func (w *socket) subscribe(channels ...string) {
	w.t.Helper()
	b, err := proto.Marshal(&realtimev1.ClientFrame{Frame: &realtimev1.ClientFrame_Subscribe{
		Subscribe: &realtimev1.Subscribe{ChannelIds: channels}}})
	if err != nil {
		w.t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(w.t.Context(), 10*time.Second)
	defer cancel()
	if err := w.c.Write(ctx, websocket.MessageBinary, b); err != nil {
		w.t.Fatalf("write Subscribe: %v", err)
	}
}

// next reads the next server frame, giving up after d.
func (w *socket) next(d time.Duration) (*realtimev1.ServerEvent, error) {
	w.t.Helper()
	ctx, cancel := context.WithTimeout(w.t.Context(), d)
	defer cancel()
	typ, data, err := w.c.Read(ctx)
	if err != nil {
		return nil, err
	}
	if typ != websocket.MessageBinary {
		w.t.Fatalf("frame type = %v, want binary", typ)
	}
	var ev realtimev1.ServerEvent
	if err := proto.Unmarshal(data, &ev); err != nil {
		w.t.Fatalf("frame is not a ServerEvent: %v", err)
	}
	return &ev, nil
}

// followers is how many connections follow topic in the cache, which is how a test knows
// a Subscribe frame, which has no acknowledgement, has taken effect.
func (r *rig) followers(topic string) int64 {
	r.t.Helper()
	if mem, ok := r.app.cache.(*cache.Memory); ok {
		return int64(mem.Subscribers(topic))
	}
	m, err := r.rdb.PubSubShardNumSub(r.t.Context(), topic).Result()
	if err != nil {
		r.t.Fatalf("SHARDNUMSUB %s: %v", topic, err)
	}
	return m[topic]
}

// ---- the specified consumers -------------------------------------------------

var specifiedConsumers = []string{
	"agent.run", "doc.analyze", "embed.index", "meeting.ingest", "meeting.transcribe",
	"meeting.transcript_check", "task.draft",
}

func TestApp_RegistersExactlyTheSpecifiedConsumers(t *testing.T) {
	sb := newSpyBus()
	r := newRig(t, withOption(WithBus(sb)))
	r.start()

	if got := sb.registered(); !slices.Equal(got, specifiedConsumers) {
		t.Fatalf("registered consumers = %v\nwant              = %v", got, specifiedConsumers)
	}
}

func TestApp_RelayStartsOnlyAfterEveryJobIsRegistered(t *testing.T) {
	sb := newSpyBus()
	delivered := make(chan struct{})
	var once sync.Once
	sb.stub("doc.analyze", func(context.Context, bus.Message) error {
		once.Do(func() { close(delivered) })
		return nil
	})
	r := newRig(t, withOption(WithBus(sb)))
	// The row is in the outbox before the app runs, so the relay finds it at its first
	// pass: a relay that is already running while jobs are still being registered would
	// publish it into a topic nobody listens on and mark it done.
	r.enqueue(docUploaded(r.workspaceID()))
	r.start()

	select {
	case <-delivered:
	case <-time.After(15 * time.Second):
		t.Fatalf("the row that was waiting at startup never reached doc.analyze; bus log: %v", sb.log())
	}

	log := sb.log()
	firstPublish := slices.IndexFunc(log, func(e string) bool { return strings.HasPrefix(e, "publish:") })
	lastRegister := -1
	for i, e := range log {
		if strings.HasPrefix(e, "register:") {
			lastRegister = i
		}
	}
	if firstPublish < 0 || lastRegister < 0 || firstPublish < lastRegister {
		t.Fatalf("the relay published (first at %d) before the last job was registered (at %d): %v", firstPublish, lastRegister, log)
	}
	// The relay marks the row once Publish has returned, a moment after the handler did.
	eventually(t, "the delivered row to be marked published", func() bool {
		var published *time.Time
		r.ownerQuery(`SELECT published_at FROM platform.outbox WHERE topic = 'doc.uploaded'`, &published)
		return published != nil
	})
}

// gatedBus holds the registration of one consumer until release is closed, which keeps
// the app in the middle of its startup for as long as a test needs.
type gatedBus struct {
	*spyBus
	at               string
	reached, release chan struct{}
}

func (g *gatedBus) Register(consumer string, topics []string, h bus.Handler) {
	if consumer == g.at {
		close(g.reached)
		<-g.release
	}
	g.spyBus.Register(consumer, topics, h)
}

func TestApp_ReadyzWaitsForDependencies(t *testing.T) {
	t.Run("not before every job is registered", func(t *testing.T) {
		// agent.run is registered last, after the worker's jobs: the probe must say 503
		// while it is pending even though the server is up and the database answers.
		gate := &gatedBus{spyBus: newSpyBus(), at: "agent.run", reached: make(chan struct{}), release: make(chan struct{})}
		r := newRig(t, withOption(WithBus(gate)))
		r.run()
		release := sync.OnceFunc(func() { close(gate.release) })
		t.Cleanup(release) // a failed test must not leave startup blocked

		select {
		case <-gate.reached:
		case <-time.After(15 * time.Second):
			t.Fatal("startup never reached the registration of agent.run")
		}
		if got := r.status("/healthz"); got != http.StatusOK {
			t.Errorf("/healthz during startup = %d, want 200: the process is alive", got)
		}
		if got := r.status("/readyz"); got != http.StatusServiceUnavailable {
			t.Errorf("/readyz while a job is still being registered = %d, want 503", got)
		}
		release()
		eventually(t, "/readyz to turn 200 once everything is registered", func() bool { return r.status("/readyz") == http.StatusOK })
	})

	t.Run("valkey", func(t *testing.T) {
		u, err := url.Parse(sharedValkey(t))
		if err != nil {
			t.Fatal(err)
		}
		p := newProxy(t, u.Host)
		r := newRig(t, withValkey("redis://"+p.addr))
		r.start()

		p.cut()
		eventually(t, "/readyz to turn 503 with Valkey down", func() bool { return r.status("/readyz") == http.StatusServiceUnavailable })
		if got := r.status("/healthz"); got != http.StatusOK {
			t.Errorf("/healthz with Valkey down = %d, want 200: a dependency outage must not restart the pod", got)
		}
		p.restore(t)
		eventually(t, "/readyz to turn 200 once Valkey is back", func() bool { return r.status("/readyz") == http.StatusOK })
	})

	t.Run("postgres", func(t *testing.T) {
		var p *proxy
		r := newRig(t, func(s *setup) {
			u, err := url.Parse(s.env.AppURL)
			if err != nil {
				t.Fatal(err)
			}
			p = newProxy(t, u.Host)
			withDatabaseVia(p.addr)(s)
		})
		r.start()

		p.cut()
		eventually(t, "/readyz to turn 503 with Postgres down", func() bool { return r.status("/readyz") == http.StatusServiceUnavailable })
		if got := r.status("/healthz"); got != http.StatusOK {
			t.Errorf("/healthz with Postgres down = %d, want 200", got)
		}
		p.restore(t)
		eventually(t, "/readyz to turn 200 once Postgres is back", func() bool { return r.status("/readyz") == http.StatusOK })
	})
}

func TestApp_ServesAPIWSAndHealthOnOnePort(t *testing.T) {
	r := newRig(t)
	r.start()

	for _, tc := range []struct {
		path string
		want int
	}{
		{"/healthz", http.StatusOK},
		{"/readyz", http.StatusOK},
		{"/api/auth/config", http.StatusOK}, // the sign-in routes of the api role
		{"/ws", http.StatusUnauthorized},    // the gateway, which wants a ticket
		{"/api/no-such-route", http.StatusNotFound},
		{"/nowhere", http.StatusNotFound},
	} {
		if got := r.status(tc.path); got != tc.want {
			t.Errorf("GET %s = %d, want %d", tc.path, got, tc.want)
		}
	}

	// A Connect RPC of the api role, on the same port, wants a session.
	anon := identityv1connect.NewIdentityServiceClient(&http.Client{Transport: headerTransport{
		base: http.DefaultTransport, header: http.Header{"Origin": {r.URL}}}}, r.URL+"/api")
	_, err := anon.GetMe(t.Context(), connect.NewRequest(&identityv1.GetMeRequest{}))
	if connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Errorf("GetMe without a session = %v, want unauthenticated", err)
	}
	alice := r.signIn(fakeoidc.AliceEmail)
	if alice.userID == "" {
		t.Error("GetMe as a signed-in user returned no user")
	}
	if ws, resp, err := alice.dialSocket(""); err != nil {
		t.Errorf("a signed-in user could not open the WebSocket on the same port: %v (%v)", err, resp)
	} else {
		_ = ws.c.CloseNow()
	}
}

// ---- the router --------------------------------------------------------------

func chatCreated(t *testing.T, ws string, msg *chatv1.Message) outbox.Stored {
	t.Helper()
	payload, err := protojson.MarshalOptions{UseProtoNames: true}.Marshal(&eventsv1.ChatMessageCreated{
		WorkspaceId: ws, ChannelId: msg.GetChannelId(), Message: msg})
	if err != nil {
		t.Fatal(err)
	}
	return outbox.Stored{ID: 41, Event: outbox.Event{Topic: events.TopicChatMessageCreated, Key: msg.GetChannelId(), Payload: payload}}
}

func agentMention(name string) *chatv1.Mention {
	return &chatv1.Mention{Kind: chatv1.MentionKind_MENTION_KIND_AGENT, Id: authz.AgentID(name).String(), Label: name}
}

func userMessage(channel, author string, mentions ...*chatv1.Mention) *chatv1.Message {
	return &chatv1.Message{
		Id: uuid.NewString(), ChannelId: channel, Seq: 3, AuthorKind: chatv1.AuthorKind_AUTHOR_KIND_USER, AuthorId: author,
		Body: "hello", Mentions: mentions, ClientMsgId: uuid.NewString(),
	}
}

func TestRouter_AgentMentionPublishesOneAgentInvoke(t *testing.T) {
	ws, channel, author := uuid.NewString(), uuid.NewString(), uuid.NewString()
	// A duplicate mention of the same agent is one request, not two runs.
	msg := userMessage(channel, author, agentMention("assistant"), agentMention("assistant"))
	stored := chatCreated(t, ws, msg)

	extra, _, err := Router{}.Route(stored)
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	if len(extra) != 1 {
		t.Fatalf("Route added %d bus messages, want exactly one agent.invoke: %+v", len(extra), extra)
	}
	m := extra[0]
	if m.Topic != events.TopicAgentInvoke {
		t.Fatalf("topic = %q, want %q", m.Topic, events.TopicAgentInvoke)
	}
	var inv eventsv1.AgentInvoke
	if err := protojson.Unmarshal(m.Data, &inv); err != nil {
		t.Fatalf("agent.invoke payload: %v\n%s", err, m.Data)
	}
	want := &eventsv1.AgentInvoke{
		WorkspaceId: ws, RunId: runID(msg.GetId(), "assistant").String(), Agent: "assistant",
		ChannelId: channel, TriggerMessageId: msg.GetId(), OnBehalfOf: author,
	}
	if !proto.Equal(&inv, want) {
		t.Errorf("agent.invoke = %v\nwant           %v", &inv, want)
	}
	if m.Key != inv.GetRunId() {
		t.Errorf("Key = %q, want the run id %q", m.Key, inv.GetRunId())
	}

	// A redelivered row is routed again and must ask for the same run: the id of the run is
	// its identity, and the agents job reuses the row it finds under it.
	again, _, err := Router{}.Route(stored)
	if err != nil || len(again) != 1 || !bytes.Equal(again[0].Data, m.Data) || again[0].ID != m.ID {
		t.Errorf("routing the same row again gave %+v (err %v), want the same message", again, err)
	}
	if other := runID(uuid.NewString(), "assistant"); other.String() == inv.GetRunId() {
		t.Error("two messages share a run id")
	}
}

func TestRouter_NoAgentMention_NoAgentInvoke(t *testing.T) {
	ws, channel, author := uuid.NewString(), uuid.NewString(), uuid.NewString()
	extra, _, err := Router{}.Route(chatCreated(t, ws, userMessage(channel, author)))
	if err != nil || len(extra) != 0 {
		t.Fatalf("a message with no mention: extra = %+v, err = %v; want nothing", extra, err)
	}

	// Neither does an event of another topic, whatever it carries.
	payload, err := protojson.Marshal(&eventsv1.TaskChanged{WorkspaceId: ws, ChannelId: channel, Task: &tasksv1.Task{Id: uuid.NewString(), ChannelId: channel}})
	if err != nil {
		t.Fatal(err)
	}
	extra, _, err = Router{}.Route(outbox.Stored{ID: 7, Event: outbox.Event{Topic: events.TopicTaskChanged, Payload: payload}})
	if err != nil || len(extra) != 0 {
		t.Fatalf("task.changed: extra = %+v, err = %v; want nothing", extra, err)
	}
}

func TestRouter_UserMentionOnly_NoAgentInvoke(t *testing.T) {
	ws, channel, author, mentioned := uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()
	msg := userMessage(channel, author, &chatv1.Mention{Kind: chatv1.MentionKind_MENTION_KIND_USER, Id: mentioned, Label: "Bob"})
	extra, _, err := Router{}.Route(chatCreated(t, ws, msg))
	if err != nil || len(extra) != 0 {
		t.Fatalf("a mention of a person: extra = %+v, err = %v; want nothing", extra, err)
	}
}

func TestRouter_ChatMessageFansOutToChannelTopic(t *testing.T) {
	ws, channel, author := uuid.NewString(), uuid.NewString(), uuid.NewString()
	msg := userMessage(channel, author, agentMention("assistant"))
	_, fan, err := Router{}.Route(chatCreated(t, ws, msg))
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	if len(fan) != 1 || fan[0].Topic != "ch:"+channel {
		t.Fatalf("fan-out = %+v, want one frame on ch:%s", fan, channel)
	}
	var ev realtimev1.ServerEvent
	if err := proto.Unmarshal(fan[0].Frame, &ev); err != nil {
		t.Fatalf("the frame is not a ServerEvent: %v", err)
	}
	if !proto.Equal(ev.GetMessageCreated(), msg) {
		t.Errorf("frame carries %v, want the message %v", ev.GetMessageCreated(), msg)
	}
}

// An agent's own reply may quote @assistant; it must not start another run, or two
// agents would answer each other for ever.
func TestRouter_AgentAuthoredMessage_NoAgentInvoke(t *testing.T) {
	ws, channel := uuid.NewString(), uuid.NewString()
	msg := userMessage(channel, authz.AgentID("assistant").String(), agentMention("assistant"))
	msg.AuthorKind = chatv1.AuthorKind_AUTHOR_KIND_AGENT
	extra, fan, err := Router{}.Route(chatCreated(t, ws, msg))
	if err != nil || len(extra) != 0 {
		t.Fatalf("a message written by an agent: extra = %+v, err = %v; want no run", extra, err)
	}
	if len(fan) != 1 {
		t.Errorf("the frame of an agent's message is still fanned out: got %d", len(fan))
	}
}

func TestRouter_UnreadableChatEvent_IsLoggedAndDroppedNotRetried(t *testing.T) {
	logs := captureLogs(t)
	for _, tc := range []struct {
		name, payload string
		wantFrames    int
	}{
		{"not JSON", `{`, 0},
		{"no message", `{"workspace_id":"` + uuid.NewString() + `","channel_id":"` + uuid.NewString() + `"}`, 0},
		// The frame is still fanned out: only the run cannot be started.
		{"unreadable author", `{"workspace_id":"` + uuid.NewString() + `","channel_id":"` + uuid.NewString() +
			`","message":{"id":"` + uuid.NewString() + `","author_kind":"AUTHOR_KIND_USER","author_id":"not-a-uuid","mentions":[{"kind":"MENTION_KIND_AGENT","label":"assistant"}]}}`, 1},
	} {
		extra, fan, err := Router{}.Route(outbox.Stored{ID: 9, Event: outbox.Event{Topic: events.TopicChatMessageCreated, Payload: []byte(tc.payload)}})
		if err != nil || len(extra) != 0 || len(fan) != tc.wantFrames {
			t.Errorf("%s: extra = %+v, fan = %+v, err = %v; want no run, %d frames and no error", tc.name, extra, fan, err, tc.wantFrames)
		}
	}
	if !strings.Contains(logs(), "level=ERROR") {
		t.Errorf("dropped events were not logged at ERROR:\n%s", logs())
	}
}

// ---- the whole path ----------------------------------------------------------

// nextMessage reads frames until a message_created arrives, for at most d. The frames
// of other events (the member that was just added) may come first.
func (w *socket) nextMessage(d time.Duration) (*chatv1.Message, error) {
	w.t.Helper()
	deadline := time.Now().Add(d)
	for {
		ev, err := w.next(time.Until(deadline))
		if err != nil {
			return nil, err
		}
		if m := ev.GetMessageCreated(); m != nil {
			return m, nil
		}
	}
}

func TestApp_EndToEnd_SendMessageReachesSubscribedMemberOverWebSocket(t *testing.T) {
	eachCache(t, testEndToEndSendMessageReachesSubscribedMemberOverWebSocket)
}

func testEndToEndSendMessageReachesSubscribedMemberOverWebSocket(t *testing.T, mods ...func(*setup)) {
	r := newRig(t, mods...)
	r.start()

	// Signed in through the fake provider: the login route, its authorization endpoint
	// and the callback, as three browsers would.
	alice, bob, dan := r.signIn(fakeoidc.AliceEmail), r.signIn(fakeoidc.BobEmail), r.signIn(members[2])
	channel := alice.newChannel("planning")
	alice.addMember(channel, bob)

	danWS := dan.socket()
	danWS.subscribe(channel) // dan is in the workspace but not in the channel: refused
	bobWS := bob.socket()
	bobWS.subscribe(channel)
	eventually(t, "bob's subscription to reach the cache", func() bool { return r.followers("ch:"+channel) >= 1 })

	sent := alice.send(channel, "hello bob, the plan is ready")

	got, err := bobWS.nextMessage(2 * time.Second)
	if err != nil {
		t.Fatalf("bob received no frame within 2 s: %v", err)
	}
	if got.GetId() != sent.GetId() || got.GetBody() != "hello bob, the plan is ready" || got.GetSeq() != sent.GetSeq() || got.GetAuthorId() != alice.userID {
		t.Errorf("bob's frame = %v, want the message alice sent: %v", got, sent)
	}
	if m, err := danWS.nextMessage(300 * time.Millisecond); err == nil {
		t.Errorf("a non-member received %v", m)
	}
}

func TestApp_EveryMountedHandlerMapsASentinelToItsConnectCode(t *testing.T) {
	r := newRig(t)
	r.start()
	alice, dan := r.signIn(fakeoidc.AliceEmail), r.signIn(members[2])
	// The session outlives the person: its user is gone, which GetMe answers with
	// not found.
	r.ownerExec(`DELETE FROM identity.users WHERE id = $1`, dan.userID)

	ctx := t.Context()
	missing := uuid.NewString()
	byService := map[string]func() error{
		"/kiwi.chat.v1.ChatService/": func() error {
			_, err := alice.chat().JoinChannel(ctx, connect.NewRequest(&chatv1.JoinChannelRequest{ChannelId: missing}))
			return err
		},
		"/kiwi.files.v1.FileService/": func() error {
			_, err := alice.files().GetFile(ctx, connect.NewRequest(&filesv1.GetFileRequest{FileId: missing}))
			return err
		},
		"/kiwi.meetings.v1.MeetingService/": func() error {
			_, err := alice.meetings().GetMeeting(ctx, connect.NewRequest(&meetingsv1.GetMeetingRequest{MeetingId: missing}))
			return err
		},
		"/kiwi.tasks.v1.TaskService/": func() error {
			_, err := alice.tasks().DiscardDraft(ctx, connect.NewRequest(&tasksv1.DiscardDraftRequest{Id: missing}))
			return err
		},
		"/kiwi.agents.v1.AgentService/": func() error {
			_, err := alice.agents().GetRun(ctx, connect.NewRequest(&agentsv1.GetRunRequest{RunId: missing}))
			return err
		},
		"/kiwi.identity.v1.IdentityService/": func() error {
			_, err := dan.identity().GetMe(ctx, connect.NewRequest(&identityv1.GetMeRequest{}))
			return err
		},
	}

	// Every handler the app mounts is covered, so a seventh one without a case fails here.
	var mounted []string
	for _, h := range r.app.handlers {
		path, _ := h.Build()
		mounted = append(mounted, path)
	}
	var covered []string
	for path := range byService {
		covered = append(covered, path)
	}
	slices.Sort(mounted)
	slices.Sort(covered)
	if !slices.Equal(mounted, covered) {
		t.Fatalf("the app mounts %v but the test calls %v", mounted, covered)
	}

	for path, call := range byService {
		err := call()
		var ce *connect.Error
		if !errors.As(err, &ce) {
			t.Errorf("%s: err = %v, want a Connect error", path, err)
			continue
		}
		if ce.Code() != connect.CodeNotFound {
			t.Errorf("%s: code = %v (%s), want not_found: the error mapper is not on this handler", path, ce.Code(), ce.Message())
		}
		if ce.Message() != "not found" {
			t.Errorf("%s: message = %q, want the sentinel's text and nothing of the cause", path, ce.Message())
		}
	}
}

// ownerExec runs one statement as kiwi_owner in the workspace of the members.
func (r *rig) ownerExec(sql string, args ...any) {
	r.t.Helper()
	ctx := r.t.Context()
	conn, err := pgx.Connect(ctx, r.env.OwnerURL)
	if err != nil {
		r.t.Fatalf("connect as owner: %v", err)
	}
	defer func() { _ = conn.Close(ctx) }()
	tx, err := conn.Begin(ctx)
	if err != nil {
		r.t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SELECT set_config('kiwi.workspace_id', $1, true)`, r.workspaceID().String()); err != nil {
		r.t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, sql, args...); err != nil {
		r.t.Fatalf("%s: %v", sql, err)
	}
	if err := tx.Commit(ctx); err != nil {
		r.t.Fatal(err)
	}
}

func TestApp_BusDeadLettersAfterTheAttemptTheDomainsTreatAsFinal(t *testing.T) {
	logs := captureLogs(t)
	// docs and meetings settle a failure on their fifth delivery and agents on
	// Deps.FinalAttempt, which is this number: a bus that gave up earlier would leave a
	// file, a meeting or a run unsettled for good, and one that gave up later would
	// retry past the point where they have already settled.
	if maxAttempts != 5 {
		t.Fatalf("maxAttempts = %d: docs and meetings treat delivery 5 as the final one", maxAttempts)
	}

	b := newMemoryBus(0)
	var attempts []int
	b.Register("always.fails", []string{"x"}, func(_ context.Context, m bus.Message) error {
		attempts = append(attempts, m.Attempt)
		return errors.New("boom")
	})
	ws := uuid.NewString()
	if err := b.Publish(t.Context(), bus.Message{ID: "1", Topic: "x", Attrs: map[string]string{"workspace_id": ws}}); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if want := []int{1, 2, 3, 4, 5}; !slices.Equal(attempts, want) {
		t.Fatalf("deliveries = %v, want %v", attempts, want)
	}
	for _, want := range []string{"level=ERROR", "dead-lettered", "consumer=always.fails", "workspace_id=" + ws, "topic=x"} {
		if !strings.Contains(logs(), want) {
			t.Errorf("the dead letter is not logged with %q:\n%s", want, logs())
		}
	}
}

func TestOriginPatterns(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  config.Config
		want []string
	}{
		{"local: the public host and the Vite dev server", config.Config{Env: "local", PublicURL: "http://localhost:8080"}, []string{"localhost:8080", "localhost:5173"}},
		{"prod: the public host only", config.Config{Env: "prod", PublicURL: "https://kiwi.example"}, []string{"kiwi.example"}},
		{"a port is part of the host", config.Config{Env: "prod", PublicURL: "https://kiwi.example:8443/"}, []string{"kiwi.example:8443"}},
	} {
		if got := originPatterns(tc.cfg); !slices.Equal(got, tc.want) {
			t.Errorf("%s: originPatterns = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestApp_WebSocketAcceptsOnlyItsOwnOriginAndTheDevServer(t *testing.T) {
	r := newRig(t)
	r.start()
	alice := r.signIn(fakeoidc.AliceEmail)

	for _, origin := range []string{r.URL, "http://localhost:5173"} {
		ws, resp, err := alice.dialSocket(origin)
		if err != nil {
			t.Errorf("Origin %s refused: %v (%v)", origin, err, resp)
			continue
		}
		_ = ws.c.CloseNow()
	}
	// The gateway matches the host of the Origin, whatever its scheme (the library's
	// rule for patterns without "://"), so only another host or port is refused.
	for _, origin := range []string{"http://evil.example", "http://localhost:5174", "http://127.0.0.1:5173"} {
		ws, resp, err := alice.dialSocket(origin)
		if err == nil {
			_ = ws.c.CloseNow()
			t.Errorf("Origin %s was accepted", origin)
			continue
		}
		if resp == nil || resp.StatusCode != http.StatusForbidden {
			t.Errorf("Origin %s: %v (%v), want 403", origin, err, resp)
		}
	}
}

func TestApp_Shutdown_FailsReadinessThenRollsBackInFlightRelayRows(t *testing.T) {
	sb := newSpyBus()
	started := make(chan struct{})
	var startedOnce sync.Once
	var readyWentDown, abortedAfterReadyWentDown atomic.Bool
	aborted := make(chan struct{})
	sb.stub("doc.analyze", func(ctx context.Context, _ bus.Message) error {
		startedOnce.Do(func() { close(started) })
		<-ctx.Done()
		abortedAfterReadyWentDown.Store(readyWentDown.Load())
		close(aborted)
		return ctx.Err()
	})
	r := newRig(t, withOption(WithBus(sb)), withOption(WithDrainTimeout(time.Second)))
	r.start()
	r.enqueue(docUploaded(r.workspaceID()))
	select {
	case <-started:
	case <-time.After(15 * time.Second):
		t.Fatal("the job never started")
	}

	r.cancel() // SIGTERM
	eventually(t, "/readyz to turn 503 while the server drains", func() bool {
		if r.status("/readyz") == http.StatusServiceUnavailable {
			readyWentDown.Store(true)
			return true
		}
		return false
	})
	select {
	case <-aborted:
	case <-time.After(30 * time.Second):
		t.Fatal("the job in flight was never cancelled")
	}
	if err := r.stop(); err != nil {
		t.Fatalf("Run = %v, want nil after a clean shutdown", err)
	}

	if !abortedAfterReadyWentDown.Load() {
		t.Error("the context of the jobs was cancelled before readiness failed: the load balancer was not given the drain")
	}
	var published *time.Time
	r.ownerQuery(`SELECT published_at FROM platform.outbox WHERE topic = 'doc.uploaded'`, &published)
	if published != nil {
		t.Errorf("the row in flight was marked published at %v: a cancelled job must roll its claim back so the next process delivers it", published)
	}
}

func TestApp_AgentMention_ReachesTheAgentRunConsumerOnce(t *testing.T) {
	sb := newSpyBus()
	runs := make(chan bus.Message, 8)
	sb.stub("agent.run", func(_ context.Context, m bus.Message) error {
		runs <- m
		return nil
	})
	r := newRig(t, withOption(WithBus(sb)))
	r.start()
	alice := r.signIn(fakeoidc.AliceEmail)
	channel := alice.newChannel("ask")

	msg := alice.send(channel, "@assistant when do we ship?", &chatv1.Mention{Kind: chatv1.MentionKind_MENTION_KIND_AGENT, Label: "assistant"})

	var m bus.Message
	select {
	case m = <-runs:
	case <-time.After(15 * time.Second):
		t.Fatalf("agent.run received nothing; bus log: %v", sb.log())
	}
	var inv eventsv1.AgentInvoke
	if err := protojson.Unmarshal(m.Data, &inv); err != nil {
		t.Fatalf("agent.invoke payload: %v", err)
	}
	want := &eventsv1.AgentInvoke{
		WorkspaceId: r.workspaceID().String(), RunId: runID(msg.GetId(), "assistant").String(), Agent: "assistant",
		ChannelId: channel, TriggerMessageId: msg.GetId(), OnBehalfOf: alice.userID,
	}
	if !proto.Equal(&inv, want) {
		t.Errorf("agent.invoke = %v\nwant           %v", &inv, want)
	}
	if m.Attrs["workspace_id"] != r.workspaceID().String() || m.Attrs["topic"] != events.TopicAgentInvoke {
		t.Errorf("attrs = %v, want the relay's topic and workspace_id", m.Attrs)
	}

	// A message that mentions no agent starts none, and the first one started exactly
	// one: when embed.index has seen both messages, agent.run has seen one.
	alice.send(channel, "thanks")
	count := func(consumer string) int {
		n := 0
		for _, e := range sb.log() {
			if e == "deliver:"+consumer {
				n++
			}
		}
		return n
	}
	eventually(t, "both messages to reach embed.index", func() bool { return count("embed.index") >= 2 })
	if n := count("agent.run"); n != 1 {
		t.Errorf("agent.run was delivered %d times, want 1", n)
	}
}

// summaryJSON is what the model answers the summary request with.
const summaryJSON = `{"summary":"The team will ship the release on Friday.",` +
	`"decisions":[{"text":"Ship the release on Friday","entry_refs":[0]}],"open_questions":[],` +
	`"action_items":[{"title":"Send the release notes","owner_hint":"","due_hint":"none","entry_refs":[0]}]}`

func TestApp_RecordingFlowsFromUploadToASummarySlip(t *testing.T) {
	wav, err := os.ReadFile(filepath.Join("..", "platform", "llm", "openai", "testdata", "speech.wav"))
	if err != nil {
		t.Fatalf("read the recorded speech: %v", err)
	}
	r := newRig(t)
	r.llm.QueueTranscribe(fakellm.TranscribeReply{Entries: []llm.TranscriptEntry{{Text: "We will ship the release on Friday."}}})
	r.llm.QueueGenerate(fakellm.GenerateReply{Result: llm.Result{Text: summaryJSON}})
	r.start()
	alice := r.signIn(fakeoidc.AliceEmail)
	channel := alice.newChannel("standup")
	ctx := t.Context()

	up, err := alice.files().CreateUpload(ctx, connect.NewRequest(&filesv1.CreateUploadRequest{
		ChannelId: channel, Name: "standup.wav", Mime: "audio/wav", Size: int64(len(wav)),
		Purpose: filesv1.UploadPurpose_UPLOAD_PURPOSE_RECORDING}))
	if err != nil {
		t.Fatalf("CreateUpload: %v", err)
	}
	put, err := http.NewRequestWithContext(ctx, http.MethodPut, up.Msg.GetUploadUrl(), bytes.NewReader(wav))
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range up.Msg.GetHeaders() {
		put.Header.Set(k, v)
	}
	put.Header.Set("Origin", r.URL) // the signed URLs are behind the origin check too
	resp, err := http.DefaultClient.Do(put)
	if err != nil {
		t.Fatalf("PUT the recording: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		t.Fatalf("PUT the recording = %d", resp.StatusCode)
	}
	done, err := alice.files().CompleteUpload(ctx, connect.NewRequest(&filesv1.CompleteUploadRequest{FileId: up.Msg.GetFileId()}))
	if err != nil {
		t.Fatalf("CompleteUpload: %v (docs refused the recording)", err)
	}
	if got := done.Msg.GetFile().GetStatus(); got != filesv1.FileStatus_FILE_STATUS_UPLOADED {
		t.Fatalf("the accepted recording is %v, want uploaded", got)
	}

	// The object is under the key the meetings job reads it from: uploads/{workspace}/{file}.
	key := "uploads/" + r.workspaceID().String() + "/" + up.Msg.GetFileId()
	if attrs, err := r.app.blob.Head(ctx, key); err != nil || attrs.Size != int64(len(wav)) {
		t.Fatalf("Head(%s) = %+v, %v: docs and meetings disagree on the blob key", key, attrs, err)
	}

	created, err := alice.meetings().CreateFromRecording(ctx, connect.NewRequest(&meetingsv1.CreateFromRecordingRequest{
		ChannelId: channel, FileId: up.Msg.GetFileId(), Title: "Standup"}))
	if err != nil {
		t.Fatalf("CreateFromRecording: %v (the meetings re-check refused what docs accepted)", err)
	}
	id := created.Msg.GetMeeting().GetId()

	var meeting *meetingsv1.Meeting
	eventually(t, "the meeting to be summarized", func() bool {
		res, err := alice.meetings().GetMeeting(ctx, connect.NewRequest(&meetingsv1.GetMeetingRequest{MeetingId: id}))
		if err != nil {
			t.Fatalf("GetMeeting: %v", err)
		}
		meeting = res.Msg.GetMeeting()
		if meeting.GetStatus() == meetingsv1.MeetingStatus_MEETING_STATUS_FAILED {
			t.Fatalf("the meeting failed: %s", meeting.GetFailureReason())
		}
		return meeting.GetStatus() == meetingsv1.MeetingStatus_MEETING_STATUS_READY
	})
	if !strings.HasPrefix(meeting.GetRef(), "MTG-") || meeting.GetSummary() != "The team will ship the release on Friday." {
		t.Errorf("meeting = %v, want a REF and the model's summary", meeting)
	}

	calls := r.llm.TranscribeCalls()
	if len(calls) != 1 || !bytes.Equal(calls[0].Data, wav) {
		t.Errorf("transcription calls = %d, want one with the bytes docs accepted", len(calls))
	}
	msgs, err := alice.chat().ListMessages(ctx, connect.NewRequest(&chatv1.ListMessagesRequest{ChannelId: channel, Limit: 50}))
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	var slip *chatv1.Artifact
	for _, m := range msgs.Msg.GetMessages() {
		if a := m.GetArtifact(); a.GetKind() == chatv1.ArtifactKind_ARTIFACT_KIND_MEETING_SUMMARY {
			slip = a
		}
	}
	if slip == nil || slip.GetRef() != meeting.GetRef() {
		t.Errorf("the channel has no summary slip with %s: %v", meeting.GetRef(), msgs.Msg.GetMessages())
	}
}

func TestApp_PollsMeetOnItsInterval(t *testing.T) {
	r := newRig(t, withOption(WithPollInterval(50*time.Millisecond)))
	r.google.QueueCreateMeetSpace(fakegoogle.SpaceReply{Space: google.Space{
		Name: "spaces/abc123", MeetingURI: "https://meet.example/abc-defg-hij", Code: "abc-defg-hij"}})
	r.google.HandleListConferences(func(context.Context, uuid.UUID, string) ([]google.Conference, error) { return nil, nil })
	r.start()
	alice := r.signIn(fakeoidc.AliceEmail)
	channel := alice.newChannel("meet")
	if _, err := alice.meetings().StartMeeting(t.Context(), connect.NewRequest(&meetingsv1.StartMeetingRequest{ChannelId: channel})); err != nil {
		t.Fatalf("StartMeeting: %v", err)
	}

	eventually(t, "the poll to ask Meet about the space", func() bool {
		for _, c := range r.google.Calls() {
			if c.Method == "ListConferences" && c.Name == "spaces/abc123" {
				return true
			}
		}
		return false
	})
}

// A panic in one pass of the Meet poll must not take the process down: the meeting that
// caused it stays pollable, so a restart would poll it first and crash again. The pass is
// logged with its stack and the next one is made on time.
func TestApp_PollMeet_APanicIsLoggedAndThePollGoesOn(t *testing.T) {
	r := newRig(t, withOption(WithPollInterval(20*time.Millisecond)))
	r.google.QueueCreateMeetSpace(fakegoogle.SpaceReply{Space: google.Space{
		Name: "spaces/abc123", MeetingURI: "https://meet.example/abc-defg-hij", Code: "abc-defg-hij"}})
	var asked atomic.Int32
	r.google.HandleListConferences(func(context.Context, uuid.UUID, string) ([]google.Conference, error) {
		if asked.Add(1) == 1 {
			panic("poison meeting")
		}
		return nil, nil
	})
	r.start()
	alice := r.signIn(fakeoidc.AliceEmail)
	channel := alice.newChannel("meet")
	if _, err := alice.meetings().StartMeeting(t.Context(), connect.NewRequest(&meetingsv1.StartMeetingRequest{ChannelId: channel})); err != nil {
		t.Fatalf("StartMeeting: %v", err)
	}

	eventually(t, "the poll to ask Meet again after the pass that panicked", func() bool { return asked.Load() >= 3 })
	out := r.logs()
	for _, want := range []string{"poison meeting", "goroutine"} { // the panic value and its stack
		if !strings.Contains(out, want) {
			t.Errorf("the log does not carry %q:\n%s", want, out)
		}
	}
}

// The composed server answers a StartMeeting that Meet refuses with the typed answer
// of its cause, never as unknown or as the opaque internal error, with a text that
// carries nothing of Google's own message.
func TestApp_StartMeeting_GoogleFailureIsMappedPerCause(t *testing.T) {
	r := newRig(t)
	r.start()
	alice := r.signIn(fakeoidc.AliceEmail)
	channel := alice.newChannel("meet")
	raw := errors.New("google: spaces.create: googleapi: Error 403: not allowed for ana@example.org in projects/123456")

	for _, tc := range []struct {
		name    string
		err     error
		code    connect.Code
		message string
		reason  string
		cause   string
	}{
		{"access denied", errors.Join(raw, &google.MeetError{Reason: google.ReasonMeetAccess, Cause: google.CauseAccessDenied, Status: 401}),
			connect.CodeFailedPrecondition, "meet access refused", api.MeetAccessReason, google.CauseAccessDenied},
		{"api disabled", errors.Join(raw, &google.MeetError{Reason: google.ReasonMeetUnavailable, Cause: google.CauseAPIDisabled, Status: 403}),
			connect.CodeFailedPrecondition, "meet unavailable", api.MeetUnavailableReason, google.CauseAPIDisabled},
		{"rate limited", errors.Join(raw, &google.MeetError{Reason: google.ReasonMeetUnavailable, Cause: google.CauseRateLimited, Status: 429}),
			connect.CodeResourceExhausted, "meet unavailable", api.MeetUnavailableReason, google.CauseRateLimited},
		{"unavailable", errors.Join(raw, &google.MeetError{Reason: google.ReasonMeetUnavailable, Cause: google.CauseUnavailable, Status: 503}),
			connect.CodeUnavailable, "meet unavailable", api.MeetUnavailableReason, google.CauseUnavailable},
		{"refused", errors.Join(raw, &google.MeetError{Reason: google.ReasonMeetUnavailable, Cause: google.CauseRefused, Status: 400}),
			connect.CodeFailedPrecondition, "meet unavailable", api.MeetUnavailableReason, google.CauseRefused},
		{"consent required", &google.ConsentRequiredError{URL: "/api/auth/meet-consent"},
			connect.CodeFailedPrecondition, "consent required", api.ConsentRequiredReason, ""},
		{"untyped", raw, connect.CodeInternal, "internal error", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r.google.QueueCreateMeetSpace(fakegoogle.SpaceReply{Err: tc.err})

			_, err := alice.meetings().StartMeeting(t.Context(), connect.NewRequest(&meetingsv1.StartMeetingRequest{ChannelId: channel}))

			var ce *connect.Error
			if !errors.As(err, &ce) {
				t.Fatalf("err = %v, want a Connect error", err)
			}
			if ce.Code() != tc.code || ce.Message() != tc.message {
				t.Fatalf("answer = %s %q, want %s %q", ce.Code(), ce.Message(), tc.code, tc.message)
			}
			if strings.Contains(err.Error(), "ana@example.org") || strings.Contains(err.Error(), "projects/123456") {
				t.Errorf("the answer %q carries Google's message", err)
			}
			if tc.reason == "" {
				if len(ce.Details()) != 0 {
					t.Errorf("details = %v, want none", ce.Details())
				}
				return
			}
			if len(ce.Details()) != 1 {
				t.Fatalf("details = %d, want 1", len(ce.Details()))
			}
			v, err := ce.Details()[0].Value()
			if err != nil {
				t.Fatalf("detail: %v", err)
			}
			info, ok := v.(*errdetails.ErrorInfo)
			if !ok || info.GetReason() != tc.reason || info.GetMetadata()[api.MeetCauseKey] != tc.cause {
				t.Errorf("detail = %v, want reason %s and cause %q", v, tc.reason, tc.cause)
			}
		})
	}
}

// A space made without auto-transcription still starts the meeting, and the answer
// says the host has to turn the transcript on.
func TestApp_StartMeeting_FallbackSpaceSaysTranscriptionIsManual(t *testing.T) {
	r := newRig(t)
	r.start()
	alice := r.signIn(fakeoidc.AliceEmail)
	channel := alice.newChannel("meet")

	for _, automatic := range []bool{true, false} {
		r.google.QueueCreateMeetSpace(fakegoogle.SpaceReply{Space: google.Space{
			Name: fmt.Sprintf("spaces/auto-%t", automatic), MeetingURI: "https://meet.example/abc-defg-hij", TranscriptionAutomatic: automatic}})

		res, err := alice.meetings().StartMeeting(t.Context(), connect.NewRequest(&meetingsv1.StartMeetingRequest{ChannelId: channel}))
		if err != nil {
			t.Fatalf("StartMeeting (automatic=%t): %v", automatic, err)
		}
		if res.Msg.GetTranscriptionAutomatic() != automatic || res.Msg.GetMeeting().GetMeetUri() != "https://meet.example/abc-defg-hij" {
			t.Errorf("response = %v, want transcription_automatic=%t and the Meet link", res.Msg, automatic)
		}
	}
}

func TestNew_UnreachableOIDCIssuerNamesTheVariable(t *testing.T) {
	captureLogs(t)
	cfg := config.Config{
		Env:               "local",
		Roles:             []config.Role{config.RoleAPI, config.RoleRealtime, config.RoleWorker, config.RoleAgents},
		PublicURL:         "http://localhost:8080",
		DatabaseURL:       "postgres://kiwi_app:x@127.0.0.1:1/kiwi?sslmode=disable",
		DatabaseSystemURL: "postgres://kiwi_system:x@127.0.0.1:1/kiwi?sslmode=disable",
		ValkeyURL:         sharedValkey(t),
		BusProvider:       "memory",
		BlobURL:           (&url.URL{Scheme: "file", Path: t.TempDir()}).String(),
		BlobSigningKey:    bytes.Repeat([]byte{1}, 32),
		OIDCIssuer:        "http://127.0.0.1:1/issuer",
		OAuthClientID:     "id",
		OAuthClientSecret: "secret",
		TokenEncKey:       bytes.Repeat([]byte{2}, 32),
	}
	a, err := New(t.Context(), cfg, WithLLM(fakellm.New()), WithGoogle(fakegoogle.New()))
	if err == nil {
		a.Close()
		t.Fatal("New succeeded with an issuer nobody answers on")
	}
	if !strings.Contains(err.Error(), "KIWI_OIDC_ISSUER") {
		t.Errorf("error = %v, want it to name KIWI_OIDC_ISSUER", err)
	}
}

func TestApp_OnlyTheRolesItRunsAreMounted(t *testing.T) {
	t.Run("without realtime there is no /ws", func(t *testing.T) {
		r := newRig(t, withRoles(config.RoleAPI, config.RoleWorker, config.RoleAgents))
		r.start()
		if got := r.status("/ws"); got != http.StatusNotFound {
			t.Errorf("/ws = %d, want 404", got)
		}
		if got := r.status("/api/auth/config"); got != http.StatusOK {
			t.Errorf("/api/auth/config = %d, want 200", got)
		}
	})
	t.Run("realtime alone has no api and no jobs", func(t *testing.T) {
		sb := newSpyBus()
		r := newRig(t, withRoles(config.RoleRealtime), withOption(WithBus(sb)))
		r.start()
		if got := r.status("/ws"); got != http.StatusUnauthorized {
			t.Errorf("/ws = %d, want 401", got)
		}
		if got := r.status("/api/auth/config"); got != http.StatusNotFound {
			t.Errorf("/api/auth/config = %d, want 404", got)
		}
		if got := sb.registered(); len(got) != 0 {
			t.Errorf("a process without the worker and agents roles registered %v", got)
		}
	})
}
