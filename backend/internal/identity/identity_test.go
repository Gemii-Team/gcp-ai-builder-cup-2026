package identity

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/redis/go-redis/v9"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	identityv1 "kiwi/gen/kiwi/identity/v1"
	"kiwi/gen/kiwi/identity/v1/identityv1connect"
	"kiwi/internal/identity/internal/vault"
	"kiwi/internal/platform/authz"
	"kiwi/internal/platform/cache"
	"kiwi/internal/platform/config"
	"kiwi/internal/platform/db"
	"kiwi/internal/platform/errs"
	"kiwi/internal/platform/google"
	"kiwi/internal/testkit/dbtest"
	"kiwi/internal/testkit/fakeoidc"
)

const (
	publicURL   = "http://localhost:8080"
	callbackURL = publicURL + "/api/auth/callback"

	// Real Google subjects are long numeric strings; the placeholder a seed
	// leaves behind is "seed:" + email.
	aliceSub = "110169484474386276334"
	bobSub   = "104851734927460312845"
)

var baseScopes = []string{
	"https://www.googleapis.com/auth/userinfo.email",
	"https://www.googleapis.com/auth/userinfo.profile",
	"openid",
}

// One plain Valkey serves the package. Tests flush it on entry and never run in
// parallel, so a test can count the keys its own requests left behind.
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

	ctr, err := testcontainers.Run(ctx, "valkey/valkey:8",
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

type settings struct {
	idp []fakeoidc.Option
	cfg func(*config.Config)
}

type setting func(*settings)

func withIDP(o ...fakeoidc.Option) setting { return func(s *settings) { s.idp = o } }

func withConfig(fn func(*config.Config)) setting { return func(s *settings) { s.cfg = fn } }

// realSubs gives Alice and Bob the long numeric subjects Google issues.
func realSubs() fakeoidc.Option {
	return fakeoidc.WithUsers(
		fakeoidc.User{Email: fakeoidc.AliceEmail, Name: fakeoidc.AliceName, Sub: aliceSub},
		fakeoidc.User{Email: fakeoidc.BobEmail, Name: fakeoidc.BobName, Sub: bobSub},
	)
}

type fixture struct {
	t       *testing.T
	env     *dbtest.Env
	db      *db.DB
	cache   *cache.Cache
	rdb     *redis.Client
	idp     *fakeoidc.Server
	mod     *Module
	cfg     config.Config
	handler http.Handler
	tokKey  string
}

// newFixture starts Postgres, Valkey and the fake provider and builds the
// identity module over them. Alice and Bob carry real-looking subjects unless
// the test says otherwise.
func newFixture(t *testing.T, opts ...setting) *fixture {
	t.Helper()
	s := settings{idp: []fakeoidc.Option{realSubs()}}
	for _, o := range opts {
		o(&s)
	}

	valkeyOnce.Do(func() { valkeyURL, valkeyErr = bootValkey() })
	if valkeyErr != nil {
		t.Fatalf("valkey: %v", valkeyErr)
	}
	ro, err := redis.ParseURL(valkeyURL)
	if err != nil {
		t.Fatal(err)
	}
	rdb := redis.NewClient(ro)
	t.Cleanup(func() { _ = rdb.Close() })
	if err := rdb.FlushAll(t.Context()).Err(); err != nil {
		t.Fatalf("flush valkey: %v", err)
	}

	env := dbtest.Start(t)
	idp := fakeoidc.New(t, s.idp...)
	tokKey := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x42}, 32))
	rawKey, _ := base64.StdEncoding.DecodeString(tokKey)
	cfg := config.Config{
		Env:               "local",
		PublicURL:         publicURL,
		DatabaseURL:       env.AppURL,
		DatabaseSystemURL: env.SystemURL,
		ValkeyURL:         valkeyURL,
		OIDCIssuer:        idp.Issuer(),
		OAuthClientID:     idp.ClientID(),
		OAuthClientSecret: idp.ClientSecret(),
		TokenEncKey:       rawKey,
		WorkspaceName:     "Acme Corp",
		WorkspaceDomain:   fakeoidc.DefaultDomain,
	}
	if s.cfg != nil {
		s.cfg(&cfg)
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

	mod, err := New(t.Context(), Deps{Config: cfg, DB: d, Cache: c, Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return &fixture{t: t, env: env, db: d, cache: c, rdb: rdb, idp: idp, mod: mod, cfg: cfg, handler: mod.AuthHandler(), tokKey: tokKey}
}

// asOwner runs fn as kiwi_owner in a transaction bound to ws, which is how the
// tests seed and inspect tenant rows without touching kiwi_system.
func (f *fixture) asOwner(ws uuid.UUID, fn func(ctx context.Context, tx pgx.Tx)) {
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
	fn(ctx, tx)
	if err := tx.Commit(ctx); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) seedWorkspace(domain string) uuid.UUID {
	f.t.Helper()
	ws := uuid.New()
	f.asOwner(ws, func(ctx context.Context, tx pgx.Tx) {
		if _, err := tx.Exec(ctx, `INSERT INTO identity.workspaces (id, name, google_domain) VALUES ($1, $2, $2)`, ws, domain); err != nil {
			f.t.Fatalf("seed workspace: %v", err)
		}
	})
	return ws
}

func (f *fixture) seedUser(ws uuid.UUID, sub, email, name string) uuid.UUID {
	f.t.Helper()
	id := uuid.New()
	f.asOwner(ws, func(ctx context.Context, tx pgx.Tx) {
		if _, err := tx.Exec(ctx, `INSERT INTO identity.users (id, workspace_id, google_sub, email, name) VALUES ($1, $2, $3, $4, $5)`, id, ws, sub, email, name); err != nil {
			f.t.Fatalf("seed user: %v", err)
		}
	})
	return id
}

type userRow struct {
	ID          uuid.UUID
	WorkspaceID uuid.UUID
	Sub         string
	Email       string
	Name        string
}

// users lists every user of every workspace (kiwi_system bypasses RLS and may
// read identity.users), ordered by email.
func (f *fixture) users() []userRow {
	f.t.Helper()
	ctx := f.t.Context()
	conn, err := pgx.Connect(ctx, f.env.SystemURL)
	if err != nil {
		f.t.Fatal(err)
	}
	defer func() { _ = conn.Close(ctx) }()
	rows, err := conn.Query(ctx, `SELECT id, workspace_id, google_sub, email, name FROM identity.users ORDER BY email`)
	if err != nil {
		f.t.Fatal(err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (userRow, error) {
		var u userRow
		return u, r.Scan(&u.ID, &u.WorkspaceID, &u.Sub, &u.Email, &u.Name)
	})
	if err != nil {
		f.t.Fatal(err)
	}
	return out
}

func (f *fixture) workspaceCount() int {
	f.t.Helper()
	ctx := f.t.Context()
	conn, err := pgx.Connect(ctx, f.env.SystemURL)
	if err != nil {
		f.t.Fatal(err)
	}
	defer func() { _ = conn.Close(ctx) }()
	var n int
	if err := conn.QueryRow(ctx, `SELECT count(*) FROM identity.workspaces`).Scan(&n); err != nil {
		f.t.Fatal(err)
	}
	return n
}

func (f *fixture) userByEmail(email string) userRow {
	f.t.Helper()
	for _, u := range f.users() {
		if u.Email == email {
			return u
		}
	}
	f.t.Fatalf("no user %s", email)
	return userRow{}
}

// oauthRow returns the stored token row of a user.
func (f *fixture) oauthRow(u userRow) (enc []byte, scopes []string, found bool) {
	f.t.Helper()
	f.asOwner(u.WorkspaceID, func(ctx context.Context, tx pgx.Tx) {
		err := tx.QueryRow(ctx, `SELECT refresh_token_enc, scopes FROM identity.oauth_tokens WHERE user_id = $1`, u.ID).Scan(&enc, &scopes)
		switch {
		case err == nil:
			found = true
		case errors.Is(err, pgx.ErrNoRows):
		default:
			f.t.Fatal(err)
		}
	})
	return enc, scopes, found
}

func (f *fixture) serve(method, target string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	f.t.Helper()
	req := httptest.NewRequest(method, target, nil)
	for _, c := range cookies {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	f.handler.ServeHTTP(rec, req)
	return rec
}

var noRedirect = &http.Client{
	Timeout:       10 * time.Second,
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

// login starts a sign-in and returns the provider URL and the cookie that binds
// the flow to this browser.
func (f *fixture) login(returnTo string) (authURL *url.URL, bind *http.Cookie) {
	f.t.Helper()
	target := "/api/auth/login"
	if returnTo != "" {
		target += "?return_to=" + url.QueryEscape(returnTo)
	}
	rec := f.serve(http.MethodGet, target)
	return f.providerRedirect(rec), cookieNamed(rec, stateCookie)
}

func (f *fixture) providerRedirect(rec *httptest.ResponseRecorder) *url.URL {
	f.t.Helper()
	if rec.Code != http.StatusFound {
		f.t.Fatalf("status = %d (%s), want a redirect to the provider", rec.Code, strings.TrimSpace(rec.Body.String()))
	}
	u, err := url.Parse(rec.Header().Get("Location"))
	if err != nil {
		f.t.Fatal(err)
	}
	return u
}

// approve plays the browser at the provider: it opens authURL as the account
// email and returns the callback URL the provider redirects to.
func (f *fixture) approve(authURL *url.URL, email string) *url.URL {
	f.t.Helper()
	q := authURL.Query()
	q.Set("login_hint", email)
	u := *authURL
	u.RawQuery = q.Encode()
	resp, err := noRedirect.Get(u.String())
	if err != nil {
		f.t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusFound {
		f.t.Fatalf("provider answered %d, want a redirect", resp.StatusCode)
	}
	cb, err := url.Parse(resp.Header.Get("Location"))
	if err != nil {
		f.t.Fatal(err)
	}
	return cb
}

func (f *fixture) callback(cb *url.URL, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	f.t.Helper()
	return f.serve(http.MethodGet, cb.RequestURI(), cookies...)
}

// signIn runs a whole sign-in as email and returns the callback's response.
func (f *fixture) signIn(email string) *httptest.ResponseRecorder {
	f.t.Helper()
	authURL, bind := f.login("/")
	return f.callback(f.approve(authURL, email), bind)
}

// mustSignIn is signIn that fails the test unless a session was issued.
func (f *fixture) mustSignIn(email string) *http.Cookie {
	f.t.Helper()
	rec := f.signIn(email)
	sid := cookieNamed(rec, SessionCookie)
	if sid == nil {
		f.t.Fatalf("sign-in as %s issued no session: %d %s", email, rec.Code, rec.Header().Get("Location"))
	}
	return sid
}

// consent runs the incremental Meet consent for the signed-in sid.
func (f *fixture) consent(sid *http.Cookie, email, returnTo string) *httptest.ResponseRecorder {
	f.t.Helper()
	rec := f.serve(http.MethodGet, "/api/auth/meet-consent?return_to="+url.QueryEscape(returnTo), sid)
	authURL := f.providerRedirect(rec)
	return f.callback(f.approve(authURL, email), cookieNamed(rec, stateCookie))
}

func cookieNamed(rec *httptest.ResponseRecorder, name string) *http.Cookie {
	for _, c := range rec.Result().Cookies() {
		if c.Name == name {
			return c
		}
	}
	return nil
}

func (f *fixture) sessionKeys() int64 {
	f.t.Helper()
	n, err := f.rdb.DBSize(f.t.Context()).Result()
	if err != nil {
		f.t.Fatal(err)
	}
	return n
}

func (f *fixture) vault() *vault.Vault {
	f.t.Helper()
	v, err := vault.New(f.tokKey)
	if err != nil {
		f.t.Fatal(err)
	}
	return v
}

func TestLogin_RedirectsToProviderWithPKCEAndBindsState(t *testing.T) {
	f := newFixture(t)

	rec := f.serve(http.MethodGet, "/api/auth/login?return_to=%2Fchannels%2Fgeneral")
	u := f.providerRedirect(rec)
	q := u.Query()

	if got, want := u.Scheme+"://"+u.Host+u.Path, f.idp.URL+"/authorize"; got != want {
		t.Fatalf("redirect target = %s, want %s", got, want)
	}
	want := map[string]string{
		"client_id":             f.idp.ClientID(),
		"redirect_uri":          callbackURL,
		"response_type":         "code",
		"scope":                 "openid email profile",
		"code_challenge_method": "S256",
		"access_type":           "offline",
	}
	for k, v := range want {
		if q.Get(k) != v {
			t.Errorf("%s = %q, want %q", k, q.Get(k), v)
		}
	}
	for _, k := range []string{"include_granted_scopes", "prompt", "hd", "login_hint"} {
		if q.Has(k) {
			t.Errorf("sign-in sends %s=%q; it must not", k, q.Get(k))
		}
	}

	state := q.Get("state")
	if len(state) < 22 { // 128 bits in base64
		t.Fatalf("state %q has fewer than 128 bits", state)
	}

	// The browser is bound to the state by a cookie that only it holds.
	bind := cookieNamed(rec, stateCookie)
	if bind == nil || bind.Value != state {
		t.Fatalf("binding cookie = %+v, want %s=<state>", bind, stateCookie)
	}
	if bind.Path != "/" || bind.Domain != "" || !bind.Secure || !bind.HttpOnly || bind.SameSite != http.SameSiteLaxMode || bind.MaxAge != 600 {
		t.Errorf("binding cookie attributes = %+v", bind)
	}

	// The Valkey record holds the PKCE verifier the challenge was made from.
	rec2, err := f.cache.TakeOAuthState(t.Context(), state)
	if err != nil {
		t.Fatalf("TakeOAuthState: %v", err)
	}
	sum := sha256.Sum256([]byte(rec2.Verifier))
	if got := base64.RawURLEncoding.EncodeToString(sum[:]); got != q.Get("code_challenge") {
		t.Errorf("code_challenge = %s, want S256 of the stored verifier (%s)", q.Get("code_challenge"), got)
	}
	if rec2.ReturnTo != "/channels/general" {
		t.Errorf("stored return_to = %q", rec2.ReturnTo)
	}
}

func TestLogin_ReturnToMustBeALocalPath(t *testing.T) {
	f := newFixture(t)
	for in, want := range map[string]string{
		"/board?x=1#y":                  "/board?x=1#y",
		"/":                             "/",
		"":                              "/",
		"https://evil.example/x":        "/",
		"//evil.example/x":              "/",
		`/\evil.example`:                "/",
		"javascript:alert(1)":           "/",
		"channels":                      "/",
		"/ok\r\nSet-Cookie: x=y":        "/",
		"/" + strings.Repeat("a", 3000): "/",
	} {
		rec := f.serve(http.MethodGet, "/api/auth/login?return_to="+url.QueryEscape(in))
		state := f.providerRedirect(rec).Query().Get("state")
		got, err := f.cache.TakeOAuthState(t.Context(), state)
		if err != nil {
			t.Fatal(err)
		}
		if got.ReturnTo != want {
			t.Errorf("return_to %q stored as %q, want %q", in, got.ReturnTo, want)
		}
	}
}

func TestCallback_HappyPath_CreatesWorkspaceUserSession(t *testing.T) {
	// The workspace is resolved, not created: the sign-in callback only ever
	// reads identity.workspaces (EnsureSeedWorkspace creates it).
	f := newFixture(t)
	ws := f.seedWorkspace(fakeoidc.DefaultDomain)

	authURL, bind := f.login("/channels/general")
	rec := f.callback(f.approve(authURL, fakeoidc.AliceEmail), bind)

	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/channels/general" {
		t.Fatalf("callback = %d -> %q, want a redirect to return_to", rec.Code, rec.Header().Get("Location"))
	}

	sid := cookieNamed(rec, SessionCookie)
	if sid == nil {
		t.Fatal("no session cookie")
	}
	var raw string
	for _, h := range rec.Result().Header.Values("Set-Cookie") {
		if strings.HasPrefix(h, SessionCookie+"=") {
			raw = h
		}
	}
	// Attributes, exactly: Secure even over http://localhost, no Domain.
	if want := SessionCookie + "=" + sid.Value + "; Path=/; Max-Age=2592000; HttpOnly; Secure; SameSite=Lax"; raw != want {
		t.Errorf("Set-Cookie\n got %s\nwant %s", raw, want)
	}
	if sid.Domain != "" {
		t.Errorf("session cookie has Domain=%q", sid.Domain)
	}

	// The binding cookie is spent.
	if c := cookieNamed(rec, stateCookie); c == nil || c.MaxAge >= 0 {
		t.Errorf("binding cookie after sign-in = %+v, want it deleted", c)
	}

	users := f.users()
	if len(users) != 1 {
		t.Fatalf("users = %+v, want one", users)
	}
	u := users[0]
	if u.WorkspaceID != ws || u.Sub != aliceSub || u.Email != fakeoidc.AliceEmail || u.Name != fakeoidc.AliceName {
		t.Errorf("user = %+v, want alice in workspace %s with sub %s", u, ws, aliceSub)
	}
	if n := f.workspaceCount(); n != 1 {
		t.Errorf("workspaces = %d, want the one that was seeded", n)
	}

	sess, err := f.mod.Session(t.Context(), sid.Value)
	if err != nil {
		t.Fatalf("Session: %v", err)
	}
	if sess.UserID != u.ID || sess.WorkspaceID != ws || sess.ID != sid.Value {
		t.Errorf("session = %+v, want user %s in %s", sess, u.ID, ws)
	}
}

func TestSessionID_HasAtLeast128BitsOfEntropy(t *testing.T) {
	f := newFixture(t)
	f.seedWorkspace(fakeoidc.DefaultDomain)

	seen := map[string]bool{}
	for range 3 {
		sid := f.mustSignIn(fakeoidc.AliceEmail)
		b, err := base64.RawURLEncoding.DecodeString(sid.Value)
		if err != nil {
			t.Fatalf("session id %q is not base64url: %v", sid.Value, err)
		}
		if len(b) < 16 {
			t.Errorf("session id carries %d bytes, want at least 16", len(b))
		}
		if seen[sid.Value] {
			t.Errorf("session id %q was issued twice", sid.Value)
		}
		seen[sid.Value] = true
	}
}

func TestCallback_RejectsStateMismatch(t *testing.T) {
	f := newFixture(t)
	f.seedWorkspace(fakeoidc.DefaultDomain)
	const mismatch = "/signin?error=state_mismatch"

	expectRejected := func(t *testing.T, rec *httptest.ResponseRecorder) {
		t.Helper()
		if got := rec.Header().Get("Location"); rec.Code != http.StatusFound || got != mismatch {
			t.Fatalf("callback = %d -> %q, want %s", rec.Code, got, mismatch)
		}
		if cookieNamed(rec, SessionCookie) != nil {
			t.Fatal("a session was issued")
		}
		if n := len(f.users()); n != 0 {
			t.Fatalf("%d users were created", n)
		}
	}
	forged := func(state string) *http.Cookie { return &http.Cookie{Name: stateCookie, Value: state} }

	t.Run("a state that was never issued", func(t *testing.T) {
		cb, _ := url.Parse(callbackURL + "?code=anything&state=forged")
		expectRejected(t, f.callback(cb, forged("forged")))
	})
	t.Run("no state", func(t *testing.T) {
		cb, _ := url.Parse(callbackURL + "?code=anything")
		expectRejected(t, f.callback(cb))
	})
	t.Run("a leaked state url completed by another browser", func(t *testing.T) {
		authURL, bind := f.login("/")
		cb := f.approve(authURL, fakeoidc.AliceEmail)

		expectRejected(t, f.callback(cb))                                      // no cookie
		expectRejected(t, f.callback(cb, forged("some-other-browsers-state"))) // someone else's flow

		// The failed attempts did not burn the state: the browser that started
		// the flow can still finish it.
		rec := f.callback(cb, bind)
		if cookieNamed(rec, SessionCookie) == nil || rec.Header().Get("Location") != "/" {
			t.Fatalf("the legitimate browser could not finish: %d -> %q", rec.Code, rec.Header().Get("Location"))
		}
	})
}

func TestCallback_StateIsSingleUse(t *testing.T) {
	f := newFixture(t)
	f.seedWorkspace(fakeoidc.DefaultDomain)

	authURL, bind := f.login("/")
	cb := f.approve(authURL, fakeoidc.AliceEmail)
	if rec := f.callback(cb, bind); cookieNamed(rec, SessionCookie) == nil {
		t.Fatal("the first callback failed")
	}
	rec := f.callback(cb, bind)
	if got := rec.Header().Get("Location"); got != "/signin?error=state_mismatch" || cookieNamed(rec, SessionCookie) != nil {
		t.Fatalf("replayed callback = %q (session %v), want state_mismatch and no session", got, cookieNamed(rec, SessionCookie))
	}
}

func TestCallback_RejectsTokenWithoutHD_NoRowsNoSession(t *testing.T) {
	f := newFixture(t)
	f.seedWorkspace(fakeoidc.DefaultDomain)

	authURL, bind := f.login("/")
	cb := f.approve(authURL, fakeoidc.CarolEmail)
	// Google appends hd to the callback URL. It is a request parameter anyone
	// can write, so the token's own claim is the only thing that counts.
	q := cb.Query()
	q.Set("hd", fakeoidc.DefaultDomain)
	cb.RawQuery = q.Encode()

	rec := f.callback(cb, bind)

	if rec.Code != http.StatusFound || !strings.HasPrefix(rec.Header().Get("Location"), "/signin?error=domain_not_allowed") {
		t.Fatalf("callback = %d -> %q, want domain_not_allowed", rec.Code, rec.Header().Get("Location"))
	}
	if cookieNamed(rec, SessionCookie) != nil {
		t.Fatal("a session cookie was set")
	}
	if users := f.users(); len(users) != 0 {
		t.Fatalf("rows were written: %+v", users)
	}
	if n := f.workspaceCount(); n != 1 {
		t.Fatalf("workspaces = %d, want only the seeded one", n)
	}
	if n := f.sessionKeys(); n != 0 {
		t.Fatalf("Valkey holds %d keys after a rejected sign-in, want none (state spent, no session)", n)
	}
}

func TestCallback_UnknownHostedDomain_RejectedNoRowsNoSession(t *testing.T) {
	f := newFixture(t)
	f.seedWorkspace("other.test") // alice's hd, acme.test, matches no workspace

	rec := f.signIn(fakeoidc.AliceEmail)

	want := "/signin?error=domain_not_allowed&email=" + url.QueryEscape(fakeoidc.AliceEmail)
	if got := rec.Header().Get("Location"); got != want {
		t.Fatalf("Location = %q, want %q", got, want)
	}
	if cookieNamed(rec, SessionCookie) != nil {
		t.Fatal("a session cookie was set")
	}
	if users := f.users(); len(users) != 0 {
		t.Fatalf("rows were written: %+v", users)
	}
	if n := f.workspaceCount(); n != 1 {
		t.Fatalf("workspaces = %d, want only the seeded one: the callback must not create tenants", n)
	}
	if n := f.sessionKeys(); n != 0 {
		t.Fatalf("Valkey holds %d keys, want none", n)
	}
}

func TestCallback_NoHD_RedirectsWithErrorAndTheVerifiedEmail(t *testing.T) {
	f := newFixture(t)
	f.seedWorkspace(fakeoidc.DefaultDomain)

	authURL, bind := f.login("/")
	cb := f.approve(authURL, fakeoidc.CarolEmail)
	q := cb.Query()
	q.Set("email", "forged@evil.test") // an email in the URL is never read
	cb.RawQuery = q.Encode()

	rec := f.callback(cb, bind)

	want := "/signin?error=domain_not_allowed&email=carol%40personal.test"
	if got := rec.Header().Get("Location"); got != want {
		t.Fatalf("Location = %q, want %q", got, want)
	}
}

func TestCallback_OtherErrors_CarryNoEmail(t *testing.T) {
	f := newFixture(t)
	f.seedWorkspace(fakeoidc.DefaultDomain)

	tests := []struct {
		name string
		// run returns the callback's response.
		run  func() *httptest.ResponseRecorder
		want string
	}{
		{"state mismatch", func() *httptest.ResponseRecorder {
			cb, _ := url.Parse(callbackURL + "?code=x&state=forged&email=alice%40acme.test")
			return f.callback(cb, &http.Cookie{Name: stateCookie, Value: "forged"})
		}, "/signin?error=state_mismatch"},
		{"the user refuses at the provider", func() *httptest.ResponseRecorder {
			authURL, bind := f.login("/")
			return f.callback(f.approve(authURL, "nobody@acme.test"), bind) // the provider answers error=access_denied
		}, "/signin?error=access_denied"},
		{"another provider error", func() *httptest.ResponseRecorder {
			authURL, bind := f.login("/")
			cb := &url.URL{Path: "/api/auth/callback", RawQuery: url.Values{"error": {"server_error"}, "state": {authURL.Query().Get("state")}}.Encode()}
			return f.callback(cb, bind)
		}, "/signin?error=exchange_failed"},
		{"the code is refused", func() *httptest.ResponseRecorder {
			authURL, bind := f.login("/")
			cb := &url.URL{Path: "/api/auth/callback", RawQuery: url.Values{"code": {"not-a-code"}, "state": {authURL.Query().Get("state")}}.Encode()}
			return f.callback(cb, bind)
		}, "/signin?error=exchange_failed"},
		{"no code", func() *httptest.ResponseRecorder {
			authURL, bind := f.login("/")
			cb := &url.URL{Path: "/api/auth/callback", RawQuery: url.Values{"state": {authURL.Query().Get("state")}}.Encode()}
			return f.callback(cb, bind)
		}, "/signin?error=exchange_failed"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := tc.run()
			got := rec.Header().Get("Location")
			if got != tc.want {
				t.Fatalf("Location = %q, want %q", got, tc.want)
			}
			if strings.Contains(got, "email") {
				t.Fatalf("Location %q carries an email", got)
			}
			if cookieNamed(rec, SessionCookie) != nil {
				t.Fatal("a session cookie was set")
			}
		})
	}
	if users := f.users(); len(users) != 0 {
		t.Fatalf("rows were written: %+v", users)
	}
}

func TestCallback_SecondLoginReusesUserAndWorkspace(t *testing.T) {
	f := newFixture(t)
	ws := f.seedWorkspace(fakeoidc.DefaultDomain)

	first := f.mustSignIn(fakeoidc.AliceEmail)
	alice := f.userByEmail(fakeoidc.AliceEmail)
	rt := f.idp.RefreshToken(fakeoidc.AliceEmail)

	second := f.mustSignIn(fakeoidc.AliceEmail)

	if first.Value == second.Value {
		t.Error("both sign-ins share a session id")
	}
	users := f.users()
	if len(users) != 1 || users[0].ID != alice.ID || users[0].WorkspaceID != ws {
		t.Fatalf("users after the second sign-in = %+v, want alice only, unchanged", users)
	}
	if n := f.workspaceCount(); n != 1 {
		t.Fatalf("workspaces = %d", n)
	}
	// Like Google, the provider issues a refresh token once; the second sign-in
	// brings none and must leave the stored one alone.
	enc2, _, found := f.oauthRow(alice)
	if !found {
		t.Fatal("the stored token vanished")
	}
	if got, err := f.vault().Decrypt(enc2); err != nil || string(got) != rt {
		t.Fatalf("stored refresh token = %q, %v; want the first one, %q", got, err, rt)
	}

	// The earlier session is not invalidated by signing in again elsewhere.
	if _, err := f.mod.Session(t.Context(), first.Value); err != nil {
		t.Errorf("first session: %v", err)
	}
}

func TestCallback_ConcurrentFirstLogins_CreateOneUser(t *testing.T) {
	f := newFixture(t)
	f.seedWorkspace(fakeoidc.DefaultDomain)

	const n = 4
	results := make(chan *httptest.ResponseRecorder, n)
	var wg sync.WaitGroup
	for range n {
		wg.Go(func() { results <- f.signIn(fakeoidc.AliceEmail) })
	}
	wg.Wait()
	close(results)

	for rec := range results {
		if cookieNamed(rec, SessionCookie) == nil {
			t.Errorf("a concurrent sign-in failed: %q", rec.Header().Get("Location"))
		}
	}
	if users := f.users(); len(users) != 1 {
		t.Fatalf("users = %+v, want exactly one", users)
	}
	enc, _, found := f.oauthRow(f.userByEmail(fakeoidc.AliceEmail))
	if rt := f.idp.RefreshToken(fakeoidc.AliceEmail); !found {
		t.Fatal("no refresh token was stored")
	} else if got, err := f.vault().Decrypt(enc); err != nil || string(got) != rt {
		t.Fatalf("stored refresh token = %q, %v; want %q", got, err, rt)
	}
}

func TestCallback_AdoptsSeedPlaceholderUserByEmail(t *testing.T) {
	f := newFixture(t)
	ws := f.seedWorkspace(fakeoidc.DefaultDomain)
	placeholder := f.seedUser(ws, "seed:"+fakeoidc.AliceEmail, fakeoidc.AliceEmail, "alice")

	sid := f.mustSignIn(fakeoidc.AliceEmail)

	users := f.users()
	if len(users) != 1 {
		t.Fatalf("users = %+v, want the placeholder adopted, not duplicated", users)
	}
	u := users[0]
	if u.ID != placeholder || u.Sub != aliceSub || u.Name != fakeoidc.AliceName || u.WorkspaceID != ws {
		t.Errorf("user = %+v, want id %s with the real sub %s and Google's name", u, placeholder, aliceSub)
	}
	if sess, err := f.mod.Session(t.Context(), sid.Value); err != nil || sess.UserID != placeholder {
		t.Errorf("session = %+v, %v; want it bound to the adopted user %s", sess, err, placeholder)
	}

	// Seeding again after the adoption must not bring the placeholder back.
	_, ids, err := EnsureSeedWorkspace(t.Context(), f.db, fakeoidc.DefaultDomain, []string{fakeoidc.AliceEmail})
	if err != nil {
		t.Fatalf("EnsureSeedWorkspace: %v", err)
	}
	if len(ids) != 1 || ids[0] != placeholder || len(f.users()) != 1 {
		t.Errorf("seed after adoption: ids = %v, users = %+v", ids, f.users())
	}
}

func TestCallback_UserOfAnotherWorkspaceIsRejected(t *testing.T) {
	f := newFixture(t)
	acme := f.seedWorkspace(fakeoidc.DefaultDomain)
	other := f.seedWorkspace("other.test")
	// Alice's real sub already belongs to a user of another workspace.
	f.seedUser(other, aliceSub, "alice@other.test", "Alice elsewhere")

	rec := f.signIn(fakeoidc.AliceEmail)

	if got := rec.Header().Get("Location"); !strings.HasPrefix(got, "/signin?error=domain_not_allowed") || cookieNamed(rec, SessionCookie) != nil {
		t.Fatalf("callback = %q, want domain_not_allowed and no session", got)
	}
	if got := len(f.users()); got != 1 {
		t.Fatalf("users = %d, want only the pre-existing one (acme %s got none)", got, acme)
	}
}

func TestEnsureSeedWorkspace_IdempotentAndCallbackAdoptsPlaceholders(t *testing.T) {
	// The mock provider mints sub = "seed:" + email, so a seeded user is found by
	// sub; the real-sub adoption path is TestCallback_AdoptsSeedPlaceholderUserByEmail.
	f := newFixture(t, withIDP())
	emails := []string{fakeoidc.AliceEmail, fakeoidc.BobEmail}

	ws1, ids1, err := EnsureSeedWorkspace(t.Context(), f.db, fakeoidc.DefaultDomain, emails)
	if err != nil {
		t.Fatalf("first run: %v", err)
	}
	ws2, ids2, err := EnsureSeedWorkspace(t.Context(), f.db, fakeoidc.DefaultDomain, emails)
	if err != nil {
		t.Fatalf("second run: %v", err)
	}

	if ws1 == uuid.Nil || ws1 != ws2 || !slices.Equal(ids1, ids2) || len(ids1) != 2 || ids1[0] == ids1[1] {
		t.Fatalf("runs differ: %s %v / %s %v", ws1, ids1, ws2, ids2)
	}
	if n := f.workspaceCount(); n != 1 {
		t.Fatalf("workspaces = %d, want 1", n)
	}
	users := f.users()
	if len(users) != 2 {
		t.Fatalf("users = %+v, want one per email", users)
	}
	for i, u := range users { // ordered by email: alice, bob
		if u.WorkspaceID != ws1 || u.ID != ids1[i] || u.Sub != "seed:"+u.Email || u.Name == "" {
			t.Errorf("placeholder %d = %+v", i, u)
		}
	}

	f.asOwner(ws1, func(ctx context.Context, tx pgx.Tx) {
		var name, domain string
		if err := tx.QueryRow(ctx, `SELECT name, google_domain FROM identity.workspaces WHERE id = $1`, ws1).Scan(&name, &domain); err != nil {
			t.Fatal(err)
		}
		if name != fakeoidc.DefaultDomain || domain != fakeoidc.DefaultDomain {
			t.Errorf("workspace name %q, domain %q: both must be the domain", name, domain)
		}
	})

	// A callback for a seeded email keeps the user id.
	sid := f.mustSignIn(fakeoidc.AliceEmail)
	after := f.users()
	if len(after) != 2 || after[0].ID != ids1[0] {
		t.Fatalf("users after the sign-in = %+v, want the same two ids", after)
	}
	if after[0].Name != fakeoidc.AliceName {
		t.Errorf("alice's name = %q, want Google's %q", after[0].Name, fakeoidc.AliceName)
	}
	if sess, err := f.mod.Session(t.Context(), sid.Value); err != nil || sess.UserID != ids1[0] || sess.WorkspaceID != ws1 {
		t.Errorf("session = %+v, %v", sess, err)
	}
}

func TestEnsureSeedWorkspace_RejectsEmptyDomainAndBadEmails(t *testing.T) {
	f := newFixture(t)
	for name, tc := range map[string]struct {
		domain string
		emails []string
	}{
		"empty domain":   {"", []string{"a@x.test"}},
		"blank domain":   {"  ", []string{"a@x.test"}},
		"empty email":    {"x.test", []string{""}},
		"not an address": {"x.test", []string{"alice"}},
	} {
		if _, _, err := EnsureSeedWorkspace(t.Context(), f.db, tc.domain, tc.emails); !errors.Is(err, errs.ErrInvalid) {
			t.Errorf("%s: err = %v, want ErrInvalid", name, err)
		}
	}
	if n := f.workspaceCount(); n != 0 {
		t.Errorf("a rejected call created %d workspaces", n)
	}
}

func TestRefreshToken_StoredEncrypted(t *testing.T) {
	f := newFixture(t)
	f.seedWorkspace(fakeoidc.DefaultDomain)
	f.mustSignIn(fakeoidc.AliceEmail)
	rt := f.idp.RefreshToken(fakeoidc.AliceEmail)
	if rt == "" {
		t.Fatal("the provider issued no refresh token")
	}

	enc, scopes, found := f.oauthRow(f.userByEmail(fakeoidc.AliceEmail))
	if !found {
		t.Fatal("no oauth_tokens row")
	}
	if bytes.Contains(enc, []byte(rt)) || string(enc) == rt {
		t.Fatal("the column holds the refresh token in clear")
	}
	got, err := f.vault().Decrypt(enc)
	if err != nil || string(got) != rt {
		t.Fatalf("decrypted = %q, %v; want %q", got, err, rt)
	}
	if !slices.Equal(scopes, baseScopes) {
		t.Errorf("scopes = %v, want %v", scopes, baseScopes)
	}
}

func TestMeetConsent_MergesGrantedScope(t *testing.T) {
	f := newFixture(t)
	f.seedWorkspace(fakeoidc.DefaultDomain)
	sid := f.mustSignIn(fakeoidc.AliceEmail)
	alice := f.userByEmail(fakeoidc.AliceEmail)

	rec := f.serve(http.MethodGet, "/api/auth/meet-consent?return_to=%2Fchannels%2Fq4", sid)
	u := f.providerRedirect(rec)
	q := u.Query()
	// The Meet scope is the new one; the sign-in scopes ride along so the ID token
	// still carries the identity of whoever consents.
	if want := "openid email profile " + meetScope; q.Get("scope") != want {
		t.Errorf("scope = %q, want %q", q.Get("scope"), want)
	}
	for k, v := range map[string]string{
		"include_granted_scopes": "true",
		"access_type":            "offline",
		"login_hint":             fakeoidc.AliceEmail,
		"code_challenge_method":  "S256",
		"redirect_uri":           callbackURL,
	} {
		if q.Get(k) != v {
			t.Errorf("%s = %q, want %q", k, q.Get(k), v)
		}
	}
	if q.Get("code_challenge") == "" || q.Get("state") == "" {
		t.Error("the consent flow carries no PKCE challenge or state")
	}

	done := f.callback(f.approve(u, fakeoidc.AliceEmail), cookieNamed(rec, stateCookie))
	if got := done.Header().Get("Location"); got != "/channels/q4" {
		t.Fatalf("consent callback -> %q, want the return_to", got)
	}

	_, scopes, found := f.oauthRow(alice)
	want := append([]string{meetScope}, baseScopes...) // sorted: https://www.googleapis.com/auth/meetings... < ...userinfo... < openid
	if !found || !slices.Equal(scopes, want) {
		t.Fatalf("scopes = %v, want %v merged with the sign-in scopes", scopes, want)
	}
	if n := len(f.users()); n != 1 {
		t.Errorf("users = %d: consent must not create users", n)
	}
	// Consenting again changes nothing: no duplicates.
	f.consent(f.mustSignIn(fakeoidc.AliceEmail), fakeoidc.AliceEmail, "/")
	if _, again, _ := f.oauthRow(alice); !slices.Equal(again, want) {
		t.Errorf("scopes after a second consent = %v, want %v", again, want)
	}
}

func TestMeetConsent_RequiresASession(t *testing.T) {
	f := newFixture(t)
	f.seedWorkspace(fakeoidc.DefaultDomain)

	for name, cookies := range map[string][]*http.Cookie{
		"no cookie":      nil,
		"unknown cookie": {{Name: SessionCookie, Value: "not-a-session"}},
	} {
		rec := f.serve(http.MethodGet, "/api/auth/meet-consent?return_to=%2F", cookies...)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s: status = %d, want 401", name, rec.Code)
		}
	}
	if n := f.sessionKeys(); n != 0 {
		t.Errorf("Valkey holds %d keys; an unauthenticated consent request must start no flow", n)
	}
}

func TestUserTokenSource_ConsentRequiredWithoutScope(t *testing.T) {
	f := newFixture(t)
	ws := f.seedWorkspace(fakeoidc.DefaultDomain)
	f.mustSignIn(fakeoidc.AliceEmail)
	alice := f.userByEmail(fakeoidc.AliceEmail)

	var cr *google.ConsentRequiredError
	if _, err := f.mod.UserTokenSource(t.Context(), alice.ID, meetScope); !errors.As(err, &cr) {
		t.Fatalf("err = %v, want *google.ConsentRequiredError", err)
	} else if cr.URL != "/api/auth/meet-consent" {
		t.Errorf("consent URL = %q", cr.URL)
	}

	// A seeded user who never signed in has no grant at all.
	ghost := f.seedUser(ws, "seed:ghost@acme.test", "ghost@acme.test", "ghost")
	if _, err := f.mod.UserTokenSource(t.Context(), ghost, meetScope); !errors.As(err, &cr) {
		t.Errorf("never signed in: err = %v, want *google.ConsentRequiredError", err)
	}

	if _, err := f.mod.UserTokenSource(t.Context(), uuid.New(), meetScope); !errors.Is(err, errs.ErrNotFound) {
		t.Errorf("unknown user: err = %v, want ErrNotFound", err)
	}
}

func TestUserTokenSource_RefreshesWithTheStoredGrant(t *testing.T) {
	f := newFixture(t)
	f.seedWorkspace(fakeoidc.DefaultDomain)
	sid := f.mustSignIn(fakeoidc.AliceEmail)
	f.consent(sid, fakeoidc.AliceEmail, "/")
	alice := f.userByEmail(fakeoidc.AliceEmail)

	src, err := f.mod.UserTokenSource(t.Context(), alice.ID, meetScope)
	if err != nil {
		t.Fatalf("UserTokenSource: %v", err)
	}
	tok, err := src.Token()
	if err != nil {
		t.Fatalf("Token: %v", err)
	}
	if !strings.HasPrefix(tok.AccessToken, "fakeoidc-access-") || !tok.Valid() {
		t.Errorf("token = %+v, want an access token the provider issued from the refresh token", tok)
	}
}

func TestUserTokenSource_RevokedGrantIsConsentRequired(t *testing.T) {
	f := newFixture(t)
	f.seedWorkspace(fakeoidc.DefaultDomain)
	f.consent(f.mustSignIn(fakeoidc.AliceEmail), fakeoidc.AliceEmail, "/")
	alice := f.userByEmail(fakeoidc.AliceEmail)

	src, err := f.mod.UserTokenSource(t.Context(), alice.ID, meetScope)
	if err != nil {
		t.Fatal(err)
	}
	f.idp.Revoke(fakeoidc.AliceEmail) // "Remove access" in the user's Google account

	var cr *google.ConsentRequiredError
	if _, err := src.Token(); !errors.As(err, &cr) {
		t.Fatalf("Token after revocation: err = %v, want *google.ConsentRequiredError", err)
	}
}

func TestCallback_NewRefreshTokenReplacesTheRevokedOne(t *testing.T) {
	f := newFixture(t)
	f.seedWorkspace(fakeoidc.DefaultDomain)
	f.consent(f.mustSignIn(fakeoidc.AliceEmail), fakeoidc.AliceEmail, "/")
	alice := f.userByEmail(fakeoidc.AliceEmail)
	old := f.idp.RefreshToken(fakeoidc.AliceEmail)

	f.idp.Revoke(fakeoidc.AliceEmail)
	f.mustSignIn(fakeoidc.AliceEmail) // Google issues a new refresh token for the new grant

	fresh := f.idp.RefreshToken(fakeoidc.AliceEmail)
	if fresh == "" || fresh == old {
		t.Fatalf("provider refresh token %q after re-consent, old %q", fresh, old)
	}
	enc, scopes, _ := f.oauthRow(alice)
	if got, err := f.vault().Decrypt(enc); err != nil || string(got) != fresh {
		t.Fatalf("stored token = %q, %v; want the new one", got, err)
	}
	// The new grant covers sign-in only: the Meet scope of the old grant is gone.
	if !slices.Equal(scopes, baseScopes) {
		t.Errorf("scopes = %v, want %v", scopes, baseScopes)
	}
	var cr *google.ConsentRequiredError
	if _, err := f.mod.UserTokenSource(t.Context(), alice.ID, meetScope); !errors.As(err, &cr) {
		t.Errorf("err = %v, want consent to be required again", err)
	}
}

func TestLogout_DeletesSession(t *testing.T) {
	f := newFixture(t)
	f.seedWorkspace(fakeoidc.DefaultDomain)
	sid := f.mustSignIn(fakeoidc.AliceEmail)
	other := f.mustSignIn(fakeoidc.AliceEmail) // a second browser

	rec := f.serve(http.MethodPost, "/api/auth/logout", sid)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rec.Code)
	}
	if _, err := f.mod.Session(t.Context(), sid.Value); !errors.Is(err, errs.ErrNotFound) {
		t.Errorf("Session after logout: err = %v, want ErrNotFound", err)
	}
	if _, err := f.mod.Session(t.Context(), other.Value); err != nil {
		t.Errorf("logging out one browser ended another's session: %v", err)
	}
	c := cookieNamed(rec, SessionCookie)
	if c == nil || c.Value != "" || c.MaxAge >= 0 || !c.Secure || !c.HttpOnly || c.Path != "/" {
		t.Errorf("cookie after logout = %+v, want it deleted with the same attributes", c)
	}

	// Logging out twice, or without a cookie, is harmless.
	if rec := f.serve(http.MethodPost, "/api/auth/logout", sid); rec.Code != http.StatusNoContent {
		t.Errorf("second logout status = %d", rec.Code)
	}
	if rec := f.serve(http.MethodPost, "/api/auth/logout"); rec.Code != http.StatusNoContent {
		t.Errorf("logout without a cookie status = %d", rec.Code)
	}
	if rec := f.serve(http.MethodGet, "/api/auth/logout", sid); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET logout status = %d, want 405", rec.Code)
	}
}

func TestIssueRealtimeTicket_BoundToUser(t *testing.T) {
	f := newFixture(t)
	ws := f.seedWorkspace(fakeoidc.DefaultDomain)
	alice := f.seedUser(ws, aliceSub, fakeoidc.AliceEmail, fakeoidc.AliceName)
	bob := f.seedUser(ws, bobSub, fakeoidc.BobEmail, fakeoidc.BobName)

	ta, err := f.mod.IssueRealtimeTicket(t.Context(), authz.Actor{Kind: authz.ActorUser, UserID: alice, WorkspaceID: ws})
	if err != nil {
		t.Fatalf("IssueRealtimeTicket: %v", err)
	}
	tb, err := f.mod.IssueRealtimeTicket(t.Context(), authz.Actor{Kind: authz.ActorUser, UserID: bob, WorkspaceID: ws})
	if err != nil {
		t.Fatal(err)
	}
	if ta == tb {
		t.Fatal("two users got the same ticket")
	}

	got, err := f.cache.RedeemTicket(t.Context(), ta)
	if err != nil || got.UserID != alice || got.WorkspaceID != ws {
		t.Fatalf("ticket redeems to %+v, %v; want alice in %s", got, err, ws)
	}
	if _, err := f.cache.RedeemTicket(t.Context(), ta); !errors.Is(err, errs.ErrNotFound) {
		t.Errorf("second redeem: err = %v, want ErrNotFound (single use)", err)
	}

	// Only a signed-in person opens a WebSocket: an agent or a job never does.
	for _, kind := range []authz.ActorKind{authz.ActorAgent, authz.ActorSystem} {
		if _, err := f.mod.IssueRealtimeTicket(t.Context(), authz.Actor{Kind: kind, UserID: alice, OnBehalfOf: alice, WorkspaceID: ws}); !errors.Is(err, errs.ErrForbidden) {
			t.Errorf("%s actor: err = %v, want ErrForbidden", kind, err)
		}
	}
}

func TestMe_ReturnsUserAndWorkspace(t *testing.T) {
	f := newFixture(t)
	ws := f.seedWorkspace(fakeoidc.DefaultDomain)
	alice := f.seedUser(ws, aliceSub, fakeoidc.AliceEmail, fakeoidc.AliceName)
	actor := authz.Actor{Kind: authz.ActorUser, UserID: alice, WorkspaceID: ws}

	u, w, err := f.mod.Me(t.Context(), actor)
	if err != nil {
		t.Fatalf("Me: %v", err)
	}
	if u.GetId() != alice.String() || u.GetEmail() != fakeoidc.AliceEmail || u.GetName() != fakeoidc.AliceName {
		t.Errorf("user = %v", u)
	}
	// KIWI_WORKSPACE_NAME is what the workspace is called to users; the column
	// holds the domain.
	if w.GetId() != ws.String() || w.GetGoogleDomain() != fakeoidc.DefaultDomain || w.GetName() != "Acme Corp" {
		t.Errorf("workspace = %v", w)
	}

	// Unset, the name falls back to the column.
	g := newFixture(t, withConfig(func(c *config.Config) { c.WorkspaceName = "" }))
	ws2 := g.seedWorkspace(fakeoidc.DefaultDomain)
	u2 := g.seedUser(ws2, aliceSub, fakeoidc.AliceEmail, fakeoidc.AliceName)
	_, w2, err := g.mod.Me(t.Context(), authz.Actor{Kind: authz.ActorUser, UserID: u2, WorkspaceID: ws2})
	if err != nil || w2.GetName() != fakeoidc.DefaultDomain {
		t.Errorf("workspace name without KIWI_WORKSPACE_NAME = %q, %v", w2.GetName(), err)
	}
}

func TestMeAndListUsers_StayInTheActorsWorkspace(t *testing.T) {
	f := newFixture(t)
	acme := f.seedWorkspace(fakeoidc.DefaultDomain)
	rival := f.seedWorkspace("rival.test")
	f.seedUser(acme, bobSub, fakeoidc.BobEmail, fakeoidc.BobName)
	alice := f.seedUser(acme, aliceSub, fakeoidc.AliceEmail, fakeoidc.AliceName)
	mallory := f.seedUser(rival, "9999", "mallory@rival.test", "Mallory")

	users, err := f.mod.ListUsers(t.Context(), authz.Actor{Kind: authz.ActorUser, UserID: alice, WorkspaceID: acme})
	if err != nil {
		t.Fatalf("ListUsers: %v", err)
	}
	var got []string
	for _, u := range users {
		got = append(got, u.GetEmail())
	}
	if want := []string{fakeoidc.AliceEmail, fakeoidc.BobEmail}; !slices.Equal(got, want) {
		t.Fatalf("users = %v, want %v (own workspace only, by name)", got, want)
	}

	// An actor claiming a user of another workspace sees nothing of it.
	if _, _, err := f.mod.Me(t.Context(), authz.Actor{Kind: authz.ActorUser, UserID: mallory, WorkspaceID: acme}); !errors.Is(err, errs.ErrNotFound) {
		t.Errorf("Me across workspaces: err = %v, want ErrNotFound", err)
	}
}

func TestConnect_IdentityServiceRoundTrip(t *testing.T) {
	f := newFixture(t)
	ws := f.seedWorkspace(fakeoidc.DefaultDomain)
	alice := f.seedUser(ws, aliceSub, fakeoidc.AliceEmail, fakeoidc.AliceName)
	f.seedUser(ws, bobSub, fakeoidc.BobEmail, fakeoidc.BobName)
	actor := authz.Actor{Kind: authz.ActorUser, UserID: alice, WorkspaceID: ws}

	path, h := Handler(f.mod, func(context.Context) authz.Actor { return actor }, connect.WithRequireConnectProtocolHeader())
	mux := http.NewServeMux()
	mux.Handle(path, h)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	client := identityv1connect.NewIdentityServiceClient(srv.Client(), srv.URL)

	me, err := client.GetMe(t.Context(), connect.NewRequest(&identityv1.GetMeRequest{}))
	if err != nil {
		t.Fatalf("GetMe: %v", err)
	}
	if me.Msg.GetUser().GetEmail() != fakeoidc.AliceEmail || me.Msg.GetWorkspace().GetGoogleDomain() != fakeoidc.DefaultDomain {
		t.Errorf("GetMe = %v", me.Msg)
	}
	list, err := client.ListUsers(t.Context(), connect.NewRequest(&identityv1.ListUsersRequest{}))
	if err != nil || len(list.Msg.GetUsers()) != 2 {
		t.Fatalf("ListUsers = %v, %v; want both users", list, err)
	}
	tk, err := client.IssueRealtimeTicket(t.Context(), connect.NewRequest(&identityv1.IssueRealtimeTicketRequest{}))
	if err != nil {
		t.Fatalf("IssueRealtimeTicket: %v", err)
	}
	if rec, err := f.cache.RedeemTicket(t.Context(), tk.Msg.GetTicket()); err != nil || rec.UserID != alice || rec.WorkspaceID != ws {
		t.Errorf("ticket = %+v, %v", rec, err)
	}
}

func TestAuthConfig_UnauthenticatedReturnsConfiguredNames(t *testing.T) {
	read := func(f *fixture) (string, map[string]string) {
		rec := f.serve(http.MethodGet, "/api/auth/config") // no cookie: the sign-in page asks before anyone is signed in
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d", rec.Code)
		}
		var body map[string]string
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("body %q: %v", rec.Body.String(), err)
		}
		return rec.Header().Get("Content-Type"), body
	}

	ct, body := read(newFixture(t))
	if !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type = %q", ct)
	}
	if want := map[string]string{"workspaceName": "Acme Corp", "domain": "acme.test"}; !equalMaps(body, want) {
		t.Errorf("config = %v, want %v", body, want)
	}

	// Unset, both are empty strings (the keys stay), and the page falls back to
	// its own heading.
	_, body = read(newFixture(t, withConfig(func(c *config.Config) { c.WorkspaceName, c.WorkspaceDomain = "", "" })))
	if want := map[string]string{"workspaceName": "", "domain": ""}; !equalMaps(body, want) {
		t.Errorf("unset config = %v, want %v", body, want)
	}
}

func equalMaps(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if w, ok := b[k]; !ok || w != v {
			return false
		}
	}
	return true
}
