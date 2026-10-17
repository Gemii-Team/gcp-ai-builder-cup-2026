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
	"kiwi/internal/testkit/containers"
	"kiwi/internal/testkit/dbtest"
	"kiwi/internal/testkit/fakeoidc"
	"kiwi/internal/testkit/fakeoidc/fakeoidctest"
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

type settings struct {
	idp    []fakeoidctest.Option
	cfg    func(*config.Config)
	memory bool // the minimal profile: the in-process cache and the sessions in Postgres
}

type setting func(*settings)

func withIDP(o ...fakeoidctest.Option) setting { return func(s *settings) { s.idp = o } }

func withConfig(fn func(*config.Config)) setting { return func(s *settings) { s.cfg = fn } }

// withMemoryProfile runs the module as the minimal profile does (KIWI_CACHE_PROVIDER=memory):
// login state and tickets in the process, the sessions in Postgres.
func withMemoryProfile() setting { return func(s *settings) { s.memory = true } }

// eachProfile runs fn once per cache profile, as subtests: Valkey holding everything, and
// the in-process cache with the sessions in Postgres. What the sign-in flow does must not
// depend on which one it runs on.
func eachProfile(t *testing.T, fn func(t *testing.T, opts ...setting)) {
	t.Helper()
	t.Run("valkey", func(t *testing.T) { fn(t) })
	t.Run("memory", func(t *testing.T) { fn(t, withMemoryProfile()) })
}

// realSubs gives Alice, Bob and Carol the long numeric subjects Google issues;
// Carol's token carries no hd.
func realSubs() fakeoidctest.Option {
	return fakeoidctest.WithUsers(
		fakeoidc.User{Email: fakeoidc.AliceEmail, Name: fakeoidc.AliceName, Sub: aliceSub},
		fakeoidc.User{Email: fakeoidc.BobEmail, Name: fakeoidc.BobName, Sub: bobSub},
		fakeoidc.User{Email: fakeoidc.CarolEmail, Name: fakeoidc.CarolName, Sub: carolSub, NoHD: true},
	)
}

type fixture struct {
	t       *testing.T
	env     *dbtest.Env
	db      *db.DB
	cache   cache.Cache
	session cache.Sessions
	rdb     *redis.Client
	idp     *fakeoidctest.Server
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
	s := settings{idp: []fakeoidctest.Option{realSubs()}}
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
	// The provider redirects only to the callback of PublicURL, as Google does for
	// a registered client.
	idp := fakeoidctest.New(t, append([]fakeoidctest.Option{fakeoidctest.WithRedirectURIs(callbackURL)}, s.idp...)...)
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
	var c cache.Cache
	var sessions cache.Sessions
	if s.memory {
		c, sessions = cache.NewMemory(cache.Options{}), NewSessions(d, cache.Options{})
	} else {
		v, err := cache.OpenValkey(t.Context(), valkeyURL, cache.Options{})
		if err != nil {
			t.Fatalf("cache.Open: %v", err)
		}
		c, sessions = v, v
	}
	t.Cleanup(func() { _ = c.Close() })

	mod, err := New(t.Context(), Deps{Config: cfg, DB: d, Cache: c, Sessions: sessions, Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return &fixture{t: t, env: env, db: d, cache: c, session: sessions, rdb: rdb, idp: idp, mod: mod, cfg: cfg, handler: mod.AuthHandler(), tokKey: tokKey}
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

// post is serve for a POST the way this site's own page sends it: with its Origin.
func (f *fixture) post(target string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	f.t.Helper()
	req := httptest.NewRequest(http.MethodPost, target, nil)
	req.Header.Set("Origin", publicURL)
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
	return f.approveWith(authURL, email, nil)
}

// approveWith is approve after tamper has changed the authorization request's
// parameters, as a hostile page or a broken proxy could.
func (f *fixture) approveWith(authURL *url.URL, email string, tamper func(url.Values)) *url.URL {
	f.t.Helper()
	q := authURL.Query()
	q.Set("login_hint", email)
	if tamper != nil {
		tamper(q)
	}
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
	return f.consentWith(sid, email, returnTo, nil)
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
	// The nonce ties the ID token to this very request: 128 random bits in the
	// request, the same value in Valkey.
	if n := q.Get("nonce"); len(n) < 22 || n != rec2.Nonce {
		t.Errorf("nonce in the request = %q, stored = %q; want the same 128-bit value", n, rec2.Nonce)
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

func TestCallback_HD_MatchingSeededWorkspace_CreatesUserAndSession(t *testing.T) {
	// The workspace is resolved, not created: the sign-in callback only ever
	// reads identity.workspaces (EnsureSeedWorkspace creates it).
	f := newFixture(t)
	ws := f.seedWorkspace(fakeoidc.DefaultDomain)

	authURL, bind := f.login("/channels/general")
	rec := f.callback(f.approve(authURL, fakeoidc.AliceEmail), bind)

	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/channels/general" {
		t.Fatalf("callback = %d -> %q, want a redirect to return_to", rec.Code, rec.Header().Get("Location"))
	}

	sid := requireSessionCookie(t, rec)

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
	eachProfile(t, testSessionID_HasAtLeast128BitsOfEntropy)
}

func testSessionID_HasAtLeast128BitsOfEntropy(t *testing.T, opts ...setting) {
	f := newFixture(t, opts...)
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

func TestCallback_StateIsSingleUse(t *testing.T) { eachProfile(t, testCallback_StateIsSingleUse) }

func testCallback_StateIsSingleUse(t *testing.T, opts ...setting) {
	f := newFixture(t, opts...)
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

func TestCallback_HD_SecondLoginReusesUser(t *testing.T) {
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

// signInLockWaiters counts the database sessions waiting for an advisory lock.
func (f *fixture) signInLockWaiters() int {
	f.t.Helper()
	ctx := f.t.Context()
	conn, err := pgx.Connect(ctx, f.env.SystemURL)
	if err != nil {
		f.t.Fatal(err)
	}
	defer func() { _ = conn.Close(ctx) }()
	var n int
	if err := conn.QueryRow(ctx, `SELECT count(*) FROM pg_locks WHERE locktype = 'advisory' AND NOT granted`).Scan(&n); err != nil {
		f.t.Fatal(err)
	}
	return n
}

// Two browsers reach the callback for the same new person at once. The test holds
// that person's sign-in lock (the one resolveUser takes), watches both callbacks
// queue behind it in pg_locks, and only then lets go: nothing is timing-dependent
// on the passing path, and an implementation without the lock lets the callbacks
// through while the test still holds it.
func TestCallback_ConcurrentFirstLogins_CreateOneUser(t *testing.T) {
	f := newFixture(t)
	f.seedWorkspace(fakeoidc.DefaultDomain)

	const browsers = 2
	var (
		callbacks [browsers]*url.URL
		binds     [browsers]*http.Cookie
	)
	for i := range browsers {
		authURL, bind := f.login("/")
		callbacks[i], binds[i] = f.approve(authURL, fakeoidc.AliceEmail), bind
	}

	ctx := t.Context()
	conn, err := pgx.Connect(ctx, f.env.SystemURL)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close(ctx) }()
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1::text, 0))`, aliceSub); err != nil {
		t.Fatal(err)
	}

	results := make(chan *httptest.ResponseRecorder, browsers)
	for i := range browsers {
		go func() { results <- f.serve(http.MethodGet, callbacks[i].RequestURI(), binds[i]) }()
	}

	deadline := time.Now().Add(15 * time.Second)
	for f.signInLockWaiters() < browsers {
		if len(results) > 0 {
			t.Fatalf("a sign-in finished without waiting for the sign-in lock: %q", (<-results).Header().Get("Location"))
		}
		if time.Now().After(deadline) {
			t.Fatal("the sign-ins never queued behind the sign-in lock")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if users := f.users(); len(users) != 0 {
		t.Fatalf("users exist while every sign-in waits: %+v", users)
	}

	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	for range browsers {
		select {
		case rec := <-results:
			if cookieNamed(rec, SessionCookie) == nil {
				t.Errorf("a queued sign-in failed: %q", rec.Header().Get("Location"))
			}
		case <-time.After(15 * time.Second):
			t.Fatal("the sign-ins did not finish after the lock was released")
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

// oauth2.Config initialises a cache inside itself on the first token request, so
// the module's one shared Config must never be copied: authCodeURL builds its own
// for the authorization URL. This runs the first exchange of a fresh module next
// to many URL builds. Neither side does anything that synchronises with the other
// before touching the Config, so under go test -race a copy of the shared Config
// is reported whatever the interleaving.
func TestSharedOAuthConfig_IsNotCopiedWhileTheFirstExchangeInitialisesIt(t *testing.T) {
	f := newFixture(t)

	var wg sync.WaitGroup
	wg.Go(func() {
		for range 200 {
			f.mod.authCodeURL("state", "nonce", "verifier", loginScopes)
		}
	})
	// An unknown code: the provider refuses it, after the client has been set up.
	if _, err := f.mod.oauth.Exchange(f.mod.withClient(t.Context()), "no-such-code"); err == nil {
		t.Error("the provider accepted an unknown code")
	}
	wg.Wait()
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

// mockIDP is the provider the mock sign-in (backend/tools/mockoidc) runs: the
// seeded demo.example accounts, subject "seed:" + email. Alice's token is stamped
// with hd; Bob's has none, so he signs in by invitation.
func mockIDP(hd string) setting {
	return withIDP(
		fakeoidctest.WithHostedDomain(hd),
		fakeoidctest.WithUsers(
			fakeoidc.User{Email: "alice@demo.example", Name: "Alice"},
			fakeoidc.User{Email: "bob@demo.example", Name: "Bob", NoHD: true},
		),
	)
}

// The hd check still runs on the mock's tokens: the mock is a provider, not a
// way around the Workspace boundary.
func TestIdentityCallback_MockToken_HDOfAnotherWorkspaceRejected(t *testing.T) {
	const seededDomain = "demo.example"
	emails := []string{"alice@demo.example", "bob@demo.example"}

	t.Run("a hosted domain without a workspace is refused", func(t *testing.T) {
		f := newFixture(t, mockIDP("other.example"))
		ws, ids, err := EnsureSeedWorkspace(t.Context(), f.db, seededDomain, emails)
		if err != nil {
			t.Fatalf("EnsureSeedWorkspace: %v", err)
		}

		rec := f.signIn("alice@demo.example")

		if got := rec.Header().Get("Location"); !strings.HasPrefix(got, "/signin?error=domain_not_allowed") {
			t.Fatalf("callback = %q, want domain_not_allowed", got)
		}
		if cookieNamed(rec, SessionCookie) != nil {
			t.Error("a session cookie was set")
		}
		if n := f.sessionKeys(); n != 0 {
			t.Errorf("Valkey holds %d keys, want no session", n)
		}
		// Nothing was written: the seeded placeholders are exactly as the seed left them.
		users := f.users()
		if len(users) != 2 || f.workspaceCount() != 1 {
			t.Fatalf("users = %+v, workspaces = %d, want the two placeholders in the one seeded workspace", users, f.workspaceCount())
		}
		for i, u := range users {
			if u.ID != ids[i] || u.WorkspaceID != ws || u.Sub != "seed:"+emails[i] {
				t.Errorf("user %d = %+v, want the untouched placeholder %s", i, u, ids[i])
			}
		}
	})

	t.Run("the seeded hosted domain signs in as the seeded user", func(t *testing.T) {
		f := newFixture(t, mockIDP(seededDomain))
		ws, ids, err := EnsureSeedWorkspace(t.Context(), f.db, seededDomain, emails)
		if err != nil {
			t.Fatalf("EnsureSeedWorkspace: %v", err)
		}

		sid := f.mustSignIn("alice@demo.example")

		users := f.users()
		if len(users) != 2 {
			t.Fatalf("users = %+v, want the placeholder adopted, not duplicated", users)
		}
		alice := f.userByEmail("alice@demo.example")
		if alice.ID != ids[0] || alice.WorkspaceID != ws || alice.Sub != "seed:alice@demo.example" || alice.Name != "Alice" {
			t.Errorf("alice = %+v, want the seeded user %s with the mock's name", alice, ids[0])
		}
		if bob := f.userByEmail("bob@demo.example"); bob.ID != ids[1] || bob.Name != "bob" {
			t.Errorf("bob = %+v, want his placeholder %s untouched", bob, ids[1])
		}
		if sess, err := f.mod.Session(t.Context(), sid.Value); err != nil || sess.UserID != ids[0] || sess.WorkspaceID != ws {
			t.Errorf("session = %+v, %v; want it bound to the seeded user %s", sess, err, ids[0])
		}
	})
}

// Bob of the mock has no hd: he signs in as the seeded user through the invite
// rule, while Alice, of the same workspace, signs in by her hd.
func TestIdentityCallback_MockToken_BobWithoutHDSignsInByInvitation(t *testing.T) {
	f := newFixture(t, mockIDP("demo.example"))
	ws, ids, err := EnsureSeedWorkspace(t.Context(), f.db, "demo.example", []string{"alice@demo.example", "bob@demo.example"})
	if err != nil {
		t.Fatalf("EnsureSeedWorkspace: %v", err)
	}

	sid := f.mustSignIn("bob@demo.example")

	bob := f.userByEmail("bob@demo.example")
	if bob.ID != ids[1] || bob.WorkspaceID != ws || bob.Sub != "seed:bob@demo.example" || bob.Name != "Bob" {
		t.Errorf("bob = %+v, want the seeded user %s with the mock's name", bob, ids[1])
	}
	if sess, err := f.mod.Session(t.Context(), sid.Value); err != nil || sess.UserID != ids[1] || sess.WorkspaceID != ws {
		t.Errorf("session = %+v, %v; want it bound to the seeded user %s", sess, err, ids[1])
	}
	if cookieNamed(f.signIn("alice@demo.example"), SessionCookie) == nil {
		t.Error("alice, who carries hd, was refused")
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

// TestListWorkspaceIDs_ReturnsEveryWorkspaceThroughTheSystemPool: a command with no
// workspace to start from, `kiwi reindex`, enumerates the tenants through identity. A
// session bound to one workspace sees only that one (row-level security), so two ids
// coming back proves the enumeration really ran on the system pool.
func TestListWorkspaceIDs_ReturnsEveryWorkspaceThroughTheSystemPool(t *testing.T) {
	f := newFixture(t)

	if ids, err := ListWorkspaceIDs(t.Context(), f.db); err != nil || len(ids) != 0 {
		t.Fatalf("ListWorkspaceIDs on an empty database = %v, %v, want none", ids, err)
	}

	wsA, _, err := EnsureSeedWorkspace(t.Context(), f.db, "alpha.example", []string{"a@alpha.example"})
	if err != nil {
		t.Fatalf("seed alpha: %v", err)
	}
	wsB, _, err := EnsureSeedWorkspace(t.Context(), f.db, "beta.example", []string{"b@beta.example"})
	if err != nil {
		t.Fatalf("seed beta: %v", err)
	}

	got, err := ListWorkspaceIDs(t.Context(), f.db)
	if err != nil {
		t.Fatalf("ListWorkspaceIDs: %v", err)
	}
	want := []uuid.UUID{wsA, wsB}
	slices.SortFunc(want, func(a, b uuid.UUID) int { return strings.Compare(a.String(), b.String()) })
	if !slices.Equal(got, want) {
		t.Errorf("ListWorkspaceIDs = %v, want both workspaces %v in id order", got, want)
	}

	var seen int
	f.asOwner(wsA, func(ctx context.Context, tx pgx.Tx) {
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM identity.workspaces`).Scan(&seen); err != nil {
			t.Fatal(err)
		}
	})
	if seen != 1 {
		t.Errorf("a session bound to one workspace sees %d workspaces, want 1: the test would not tell the system pool from the application pool", seen)
	}
}

func TestListWorkspaceIDs_FailsWithoutASystemPool(t *testing.T) {
	f := newFixture(t)
	appOnly, err := db.Open(t.Context(), config.Config{DatabaseURL: f.env.AppURL})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(appOnly.Close)
	if _, err := ListWorkspaceIDs(t.Context(), appOnly); err == nil || !strings.Contains(err.Error(), "identity: list workspaces") {
		t.Errorf("ListWorkspaceIDs without a system pool = %v, want an error naming the step", err)
	}
}

func TestNoDomainWorkspaceEmails_ListsOnlyTheUsersOfTheWorkspaceWithoutADomain(t *testing.T) {
	f := newFixture(t)
	if got, err := NoDomainWorkspaceEmails(t.Context(), f.db); err != nil || len(got) != 0 {
		t.Fatalf("before the workspace exists: %v, %v; want nothing", got, err)
	}
	if _, _, err := EnsureSeedWorkspace(t.Context(), f.db, "acme.test", []string{"alice@acme.test"}); err != nil {
		t.Fatal(err)
	}
	if got, err := NoDomainWorkspaceEmails(t.Context(), f.db); err != nil || len(got) != 0 {
		t.Fatalf("with only a workspace of a domain: %v, %v; want nothing of it", got, err)
	}
	if _, _, err := EnsureSeedWorkspace(t.Context(), f.db, "", []string{"Zed@Gmail.example", "amy@gmail.example"}); err != nil {
		t.Fatal(err)
	}

	got, err := NoDomainWorkspaceEmails(t.Context(), f.db)

	if want := []string{"amy@gmail.example", "zed@gmail.example"}; err != nil || !slices.Equal(got, want) {
		t.Errorf("NoDomainWorkspaceEmails = %v, %v; want %v, lowercased, of that workspace only", got, err, want)
	}
}

func TestEnsureSeedWorkspace_NoDomain_LowercasesValidatesAndIsIdempotent(t *testing.T) {
	f := newFixture(t)
	emails := []string{" Mixed.Case@Gmail.com ", "second@gmail.com", "MIXED.CASE@gmail.com"} // the third repeats the first

	ws1, ids1, err := EnsureSeedWorkspace(t.Context(), f.db, "", emails)
	if err != nil {
		t.Fatalf("first run: %v", err)
	}
	if want := uuid.NewSHA1(seedNamespace, []byte("workspace:no-domain")); ws1 != want {
		t.Errorf("workspace id = %s, want the fixed id of the domain-less seed workspace %s", ws1, want)
	}
	if len(ids1) != 2 || ids1[0] == ids1[1] {
		t.Fatalf("user ids = %v, want one per distinct address", ids1)
	}
	users := f.users()
	if len(users) != 2 {
		t.Fatalf("users = %+v, want two", users)
	}
	for i, want := range []string{"mixed.case@gmail.com", "second@gmail.com"} {
		u := users[i]
		if u.Email != want || u.Sub != "seed:"+want || u.WorkspaceID != ws1 || u.Name != localPart(want) {
			t.Errorf("user %d = %+v, want the lowercased placeholder of %s in %s", i, u, want, ws1)
		}
	}
	if n := f.workspaceCount(); n != 1 {
		t.Fatalf("workspaces = %d, want the one domain-less seed workspace", n)
	}
	f.asOwner(ws1, func(ctx context.Context, tx pgx.Tx) {
		var name string
		var domain *string
		if err := tx.QueryRow(ctx, `SELECT name, google_domain FROM identity.workspaces WHERE id = $1`, ws1).Scan(&name, &domain); err != nil {
			t.Fatal(err)
		}
		if name != noDomainWorkspaceName || domain != nil {
			t.Errorf("workspace name %q, domain %v: want the seed's name and a NULL domain", name, domain)
		}
	})

	// A second run, also with a blank domain and one more address, finds the same
	// workspace and the same users and adds only the new one.
	ws2, ids2, err := EnsureSeedWorkspace(t.Context(), f.db, "  ", append(slices.Clone(emails), "third@gmail.com"))
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if ws2 != ws1 || !slices.Equal(ids2[:2], ids1) || len(ids2) != 3 {
		t.Errorf("second run = %s %v, want %s %v and one more user", ws2, ids2, ws1, ids1)
	}
	if n, m := f.workspaceCount(), len(f.users()); n != 1 || m != 3 {
		t.Errorf("workspaces = %d, users = %d, want 1 and 3", n, m)
	}
}

func TestEnsureSeedWorkspace_RejectsWhatIsNotABareAddress(t *testing.T) {
	f := newFixture(t)
	for _, domain := range []string{"x.test", ""} {
		for _, bad := range []string{
			"", "   ", "not an address", "alice", "Alice <alice@x.test>", "<alice@x.test>",
			"a@x.test, b@x.test", `"a b"@x.test`, "alice@x.test (Alice)",
		} {
			_, _, err := EnsureSeedWorkspace(t.Context(), f.db, domain, []string{"good@x.test", bad})
			if !errors.Is(err, errs.ErrInvalid) || !strings.Contains(err.Error(), fmt.Sprintf("%q", bad)) {
				t.Errorf("domain %q, address %q: err = %v, want ErrInvalid naming the entry", domain, bad, err)
			}
		}
	}
	if n, m := f.workspaceCount(), len(f.users()); n != 0 || m != 0 {
		t.Errorf("rejected calls left %d workspaces and %d users, want none", n, m)
	}
}

func TestEnsureSeedWorkspace_AddressInAnotherWorkspace_Invalid(t *testing.T) {
	f := newFixture(t)
	if _, _, err := EnsureSeedWorkspace(t.Context(), f.db, "acme.test", []string{"alice@acme.test"}); err != nil {
		t.Fatal(err)
	}
	f.seedUser(f.seedWorkspace("rival.test"), "9999", "Dora@Rival.Test", "Dora") // a row nobody lowercased
	f.seedNoDomain("carol@gmail.test")
	before := f.snapshot()

	for name, tc := range map[string]struct {
		domain, email string
	}{
		"a Workspace address in the domain-less workspace": {"", "Alice@Acme.test"},
		"a personal address in a domain workspace":         {"acme.test", "carol@gmail.test"},
		"a row stored with capitals":                       {"", "dora@rival.test"},
	} {
		_, _, err := EnsureSeedWorkspace(t.Context(), f.db, tc.domain, []string{tc.email})
		if !errors.Is(err, errs.ErrInvalid) || !strings.Contains(err.Error(), strings.ToLower(tc.email)) {
			t.Errorf("%s: err = %v, want ErrInvalid naming the address", name, err)
		}
	}
	if after := f.snapshot(); !after.equal(before) {
		t.Errorf("rejected seeds changed the database:\n before %+v\n  after %+v", before, after)
	}

	// The workspace an address is already in takes it again without complaint.
	if _, _, err := EnsureSeedWorkspace(t.Context(), f.db, "acme.test", []string{"alice@acme.test"}); err != nil {
		t.Errorf("seeding the same address into its own workspace: %v", err)
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
	// The Meet and Calendar scopes are the new ones; the sign-in scopes ride along so
	// the ID token still carries the identity of whoever consents.
	if want := "openid email profile " + meetScope + " " + calendarScope; q.Get("scope") != want {
		t.Errorf("scope = %q, want %q", q.Get("scope"), want)
	}
	for k, v := range map[string]string{
		"include_granted_scopes": "true",
		"access_type":            "offline",
		"login_hint":             fakeoidc.AliceEmail,
		"prompt":                 "consent", // the dedicated consent route always shows Google's screen: only then does Google issue a refresh token
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
	// Sorted: .../calendar.events < .../meetings.space.created < .../userinfo.email < .../userinfo.profile < openid.
	want := append([]string{calendarScope, meetScope}, baseScopes...)
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

// dropPrompt is what a Google that remembers the grant does with an authorization
// request that has no prompt: it skips the consent screen and so issues no refresh
// token. The fake provider does the same for a request without prompt=consent.
func dropPrompt(q url.Values) { q.Del("prompt") }

// forgetGrant deletes the stored Google grant of u: what a database reset, a
// restored backup or a rotated key leaves, while Google still remembers it.
func (f *fixture) forgetGrant(u userRow) {
	f.t.Helper()
	f.asOwner(u.WorkspaceID, func(ctx context.Context, tx pgx.Tx) {
		if _, err := tx.Exec(ctx, `DELETE FROM identity.oauth_tokens WHERE user_id = $1`, u.ID); err != nil {
			f.t.Fatalf("forget grant: %v", err)
		}
	})
}

// consentWith is consent after tamper has changed the authorization request.
func (f *fixture) consentWith(sid *http.Cookie, email, returnTo string, tamper func(url.Values)) *httptest.ResponseRecorder {
	f.t.Helper()
	rec := f.serve(http.MethodGet, "/api/auth/meet-consent?return_to="+url.QueryEscape(returnTo), sid)
	authURL := f.providerRedirect(rec)
	return f.callback(f.approveWith(authURL, email, tamper), cookieNamed(rec, stateCookie))
}

// captureLogs makes the module log into the returned buffer, one JSON record per line.
func (f *fixture) captureLogs() *bytes.Buffer {
	logs := new(bytes.Buffer)
	f.mod.log = slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	return logs
}

const missingGrantLog = "meet consent completed without a refresh token"

func TestLogin_DoesNotForcePromptOrIncrementalScopes(t *testing.T) {
	f := newFixture(t)
	authURL, _ := f.login("/")
	q := authURL.Query()
	if _, has := q["prompt"]; has {
		t.Errorf("the sign-in route sends prompt=%q: only the consent route forces Google's screen", q.Get("prompt"))
	}
	if q.Get("include_granted_scopes") != "" {
		t.Errorf("include_granted_scopes = %q on sign-in", q.Get("include_granted_scopes"))
	}
}

func TestBegin_StateRecordsWhetherItIsAConsentFlow(t *testing.T) {
	f := newFixture(t)
	f.seedWorkspace(fakeoidc.DefaultDomain)
	sid := f.mustSignIn(fakeoidc.AliceEmail)

	for name, tc := range map[string]struct {
		target  string
		cookies []*http.Cookie
		want    bool
	}{
		"login":   {"/api/auth/login", nil, false},
		"consent": {"/api/auth/meet-consent", []*http.Cookie{sid}, true},
	} {
		state := f.providerRedirect(f.serve(http.MethodGet, tc.target, tc.cookies...)).Query().Get("state")
		got, err := f.cache.TakeOAuthState(t.Context(), state)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got.Consent != tc.want {
			t.Errorf("%s: state.Consent = %v, want %v", name, got.Consent, tc.want)
		}
	}
}

// The user's second real run: Google remembered the Meet grant the database had
// lost, so the consent request (no prompt) was answered with a code and no refresh
// token, nothing was stored and the next Start meeting repeated the cycle. The
// consent route now forces the screen, which is what makes Google issue a token.
func TestMeetConsent_LostGrant_IsGivenAgainBecauseTheConsentScreenIsForced(t *testing.T) {
	f := newFixture(t)
	f.seedWorkspace(fakeoidc.DefaultDomain)
	sid := f.mustSignIn(fakeoidc.AliceEmail)
	alice := f.userByEmail(fakeoidc.AliceEmail)
	f.consent(sid, fakeoidc.AliceEmail, "/") // Google now remembers the Meet grant
	f.forgetGrant(alice)
	logs := f.captureLogs()

	done := f.consent(sid, fakeoidc.AliceEmail, "/channels/q4")

	if got := done.Header().Get("Location"); got != "/channels/q4" {
		t.Errorf("consent callback -> %q, want the return_to unchanged: the grant was stored", got)
	}
	enc, scopes, found := f.oauthRow(alice)
	if !found || !slices.Contains(scopes, meetScope) {
		t.Fatalf("grant stored = %v with scopes %v, want a row holding the Meet scope", found, scopes)
	}
	if got, err := f.vault().Decrypt(enc); err != nil || string(got) != f.idp.RefreshToken(fakeoidc.AliceEmail) {
		t.Errorf("stored refresh token = %q, %v; want the one Google just issued", got, err)
	}
	if _, err := f.mod.UserTokenSource(t.Context(), alice.ID, meetScope); err != nil {
		t.Errorf("UserTokenSource after the consent: %v", err)
	}
	if strings.Contains(logs.String(), missingGrantLog) {
		t.Errorf("a consent that stored the grant was logged as missing it: %s", logs.String())
	}
}

func TestMeetConsent_EndsWithoutAGrant_IsNotSilent(t *testing.T) {
	f := newFixture(t)
	f.seedWorkspace(fakeoidc.DefaultDomain)
	sid := f.mustSignIn(fakeoidc.AliceEmail)
	alice := f.userByEmail(fakeoidc.AliceEmail)
	f.forgetGrant(alice)
	logs := f.captureLogs()

	// A Google that does not show the consent screen answers with a code and no
	// refresh token: there is nothing to store and no row for the scopes to widen.
	done := f.consentWith(sid, fakeoidc.AliceEmail, "/channels/q4?x=1", dropPrompt)

	if got := done.Header().Get("Location"); got != "/channels/q4?x=1&meet_consent=missing" {
		t.Errorf("consent callback -> %q, want the return_to with its query and meet_consent=missing", got)
	}
	if cookieNamed(done, SessionCookie) == nil {
		t.Error("the user was not signed in: a consent without a grant is still a sign-in")
	}
	if _, _, found := f.oauthRow(alice); found {
		t.Error("a grant was stored although Google issued no refresh token")
	}
	var rec map[string]any
	for line := range strings.SplitSeq(strings.TrimSpace(logs.String()), "\n") {
		var r map[string]any
		if err := json.Unmarshal([]byte(line), &r); err == nil && r["msg"] == missingGrantLog {
			rec = r
		}
	}
	if rec == nil {
		t.Fatalf("no %q record in the log: %s", missingGrantLog, logs.String())
	}
	if rec["level"] != "WARN" || rec["user_id"] != alice.ID.String() || rec["workspace_id"] != alice.WorkspaceID.String() {
		t.Errorf("record = %v, want a WARN with the user and workspace ids", rec)
	}
}

// A refresh token Google did not send is not a missing grant when the one stored
// already holds the Meet scope: the consent only widened it.
func TestMeetConsent_NoRefreshTokenButGrantStored_IsNotFlagged(t *testing.T) {
	f := newFixture(t)
	f.seedWorkspace(fakeoidc.DefaultDomain)
	sid := f.mustSignIn(fakeoidc.AliceEmail)
	alice := f.userByEmail(fakeoidc.AliceEmail)
	logs := f.captureLogs()

	done := f.consentWith(sid, fakeoidc.AliceEmail, "/channels/q4", dropPrompt)

	if got := done.Header().Get("Location"); got != "/channels/q4" {
		t.Errorf("consent callback -> %q, want the return_to unchanged: the grant exists", got)
	}
	if _, scopes, found := f.oauthRow(alice); !found || !slices.Contains(scopes, meetScope) {
		t.Errorf("grant = %v with scopes %v, want the stored row widened with the Meet scope", found, scopes)
	}
	if strings.Contains(logs.String(), missingGrantLog) {
		t.Errorf("logged as missing a grant that exists: %s", logs.String())
	}
}

// Google's consent screen lets a person untick the Meet permission: the callback
// then carries a refresh token for the sign-in scopes only. The grant row exists,
// but it does not hold the Meet scope, so the person is told.
func TestMeetConsent_MeetScopeUnticked_IsFlagged(t *testing.T) {
	f := newFixture(t)
	f.seedWorkspace(fakeoidc.DefaultDomain)
	sid := f.mustSignIn(fakeoidc.AliceEmail)
	alice := f.userByEmail(fakeoidc.AliceEmail)

	done := f.consentWith(sid, fakeoidc.AliceEmail, "/channels/q4", func(q url.Values) { q.Set("scope", "openid email profile") })

	if got := done.Header().Get("Location"); got != "/channels/q4?meet_consent=missing" {
		t.Errorf("consent callback -> %q, want meet_consent=missing", got)
	}
	if _, scopes, _ := f.oauthRow(alice); slices.Contains(scopes, meetScope) {
		t.Errorf("scopes = %v, want no Meet scope", scopes)
	}
}

// withMeetConsentMissing puts exactly one meet_consent=missing in the query of a path the flow already validated, whatever
// shape that path has: its query and fragment are kept as they were, and no other parameter and no host is ever added.
func TestWithMeetConsentMissing_KeepsPathQueryAndFragmentAndAddsOneParameter(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"/", "/?meet_consent=missing"},
		{"/c/x", "/c/x?meet_consent=missing"},
		{"/c/x?a=b", "/c/x?a=b&meet_consent=missing"},
		{"/c/x#f", "/c/x?meet_consent=missing#f"},
		{"/c/x?a=b#f", "/c/x?a=b&meet_consent=missing#f"},
		{"/c/x#f?z", "/c/x?meet_consent=missing#f?z"}, // a ? after the # belongs to the fragment, not the query
		{"/c/x?", "/c/x?meet_consent=missing"},
		{"/c/x?a=b&", "/c/x?a=b&meet_consent=missing"},
		{"/c/x?a=b&&c=d", "/c/x?a=b&c=d&meet_consent=missing"},
		{"/c/x?a=%26b&c=d+e", "/c/x?a=%26b&c=d+e&meet_consent=missing"}, // what the query says is kept byte for byte
		// A return_to that already carries the parameter (the person came back flagged and the address was not cleaned)
		// still ends up with one, and it says missing.
		{"/c/x?meet_consent=missing", "/c/x?meet_consent=missing"},
		{"/c/x?meet_consent=other&a=b", "/c/x?a=b&meet_consent=missing"},
		{"/c/x?a=b&meet%5Fconsent=missing&meet_consent=again#f", "/c/x?a=b&meet_consent=missing#f"},
	} {
		got := withMeetConsentMissing(tc.in)
		if got != tc.want {
			t.Errorf("withMeetConsentMissing(%q) = %q, want %q", tc.in, got, tc.want)
		}
		path, query, _ := strings.Cut(strings.SplitN(got, "#", 2)[0], "?")
		values, err := url.ParseQuery(query)
		if err != nil || len(values["meet_consent"]) != 1 || values["meet_consent"][0] != "missing" {
			t.Errorf("withMeetConsentMissing(%q) = %q: query %v (%v), want exactly one meet_consent=missing", tc.in, got, values, err)
		}
		if u, err := url.Parse(got); err != nil || u.Scheme != "" || u.Host != "" || !strings.HasPrefix(path, "/") {
			t.Errorf("withMeetConsentMissing(%q) = %q is not a local path (%v)", tc.in, got, err)
		}
		if wantPath, _, _ := strings.Cut(strings.SplitN(tc.in, "#", 2)[0], "?"); path != wantPath {
			t.Errorf("withMeetConsentMissing(%q) = %q changed the path to %q", tc.in, got, path)
		}
		if _, frag, had := strings.Cut(tc.in, "#"); had && !strings.HasSuffix(got, "#"+frag) {
			t.Errorf("withMeetConsentMissing(%q) = %q lost the fragment %q", tc.in, got, frag)
		}
	}
}

// A state written by the binary that came before the consent flag has no consent key. It must decode as a plain sign-in
// and complete one: a sign-in begun just before a deploy is not broken by it.
func TestCallback_StateWithoutAConsentKey_CompletesAsAPlainSignIn(t *testing.T) {
	f := newFixture(t)
	f.seedWorkspace(fakeoidc.DefaultDomain)
	authURL, bind := f.login("/channels/q4?x=1")
	key := "oauth:" + authURL.Query().Get("state")
	raw, err := f.rdb.Get(t.Context(), key).Bytes()
	if err != nil {
		t.Fatal(err)
	}
	var stored map[string]any
	if err := json.Unmarshal(raw, &stored); err != nil {
		t.Fatal(err)
	}
	delete(stored, "consent")
	if len(stored) != 3 || stored["verifier"] == nil || stored["return_to"] != "/channels/q4?x=1" || stored["nonce"] == nil {
		t.Fatalf("state = %v, want the three keys the old binary wrote", stored)
	}
	old, err := json.Marshal(stored)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.rdb.Set(t.Context(), key, old, redis.KeepTTL).Err(); err != nil {
		t.Fatal(err)
	}
	logs := f.captureLogs()

	rec := f.callback(f.approve(authURL, fakeoidc.AliceEmail), bind)

	if got := rec.Header().Get("Location"); got != "/channels/q4?x=1" {
		t.Errorf("callback -> %q, want the return_to unchanged", got)
	}
	if cookieNamed(rec, SessionCookie) == nil {
		t.Error("the sign-in issued no session")
	}
	if strings.Contains(logs.String(), missingGrantLog) {
		t.Errorf("a sign-in was taken for a consent flow: %s", logs.String())
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

func TestUserTokenSource_InvalidGrant_ReturnsConsentRequired(t *testing.T) {
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

func TestLogout_DeletesSession(t *testing.T) { eachProfile(t, testLogout_DeletesSession) }

func testLogout_DeletesSession(t *testing.T, opts ...setting) {
	f := newFixture(t, opts...)
	f.seedWorkspace(fakeoidc.DefaultDomain)
	sid := f.mustSignIn(fakeoidc.AliceEmail)
	other := f.mustSignIn(fakeoidc.AliceEmail) // a second browser

	rec := f.post("/api/auth/logout", sid)

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
	if rec := f.post("/api/auth/logout", sid); rec.Code != http.StatusNoContent {
		t.Errorf("second logout status = %d", rec.Code)
	}
	if rec := f.post("/api/auth/logout"); rec.Code != http.StatusNoContent {
		t.Errorf("logout without a cookie status = %d", rec.Code)
	}
	if rec := f.serve(http.MethodGet, "/api/auth/logout", sid); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET logout status = %d, want 405", rec.Code)
	}
}

func TestLogout_RefusesCrossOriginRequests(t *testing.T) {
	f := newFixture(t)
	f.seedWorkspace(fakeoidc.DefaultDomain)
	sid := f.mustSignIn(fakeoidc.AliceEmail)

	post := func(origin string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/auth/logout", nil)
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		req.AddCookie(sid)
		rec := httptest.NewRecorder()
		f.handler.ServeHTTP(rec, req)
		return rec
	}

	for name, origin := range map[string]string{
		"another site":        "https://evil.example",
		"the opaque origin":   "null",
		"a sibling subdomain": "http://app.localhost:8080",
		"another port":        "http://localhost:9999",
		"no origin at all":    "",
	} {
		rec := post(origin)
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s: status = %d, want 403", name, rec.Code)
		}
		if cookieNamed(rec, SessionCookie) != nil {
			t.Errorf("%s: the refused request touched the session cookie", name)
		}
		if _, err := f.mod.Session(t.Context(), sid.Value); err != nil {
			t.Fatalf("%s: the refused request ended the session: %v", name, err)
		}
	}

	if rec := post(publicURL); rec.Code != http.StatusNoContent {
		t.Fatalf("same-origin logout: status = %d, want 204", rec.Code)
	}
	if _, err := f.mod.Session(t.Context(), sid.Value); !errors.Is(err, errs.ErrNotFound) {
		t.Errorf("Session after a same-origin logout: err = %v, want ErrNotFound", err)
	}
}

func TestIssueRealtimeTicket_BoundToUser(t *testing.T) {
	eachProfile(t, testIssueRealtimeTicket_BoundToUser)
}

func testIssueRealtimeTicket_BoundToUser(t *testing.T, opts ...setting) {
	f := newFixture(t, opts...)
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

func TestMe_WorkspaceWithoutDomain_HasNoDomainAndItsOwnName(t *testing.T) {
	me := func(f *fixture) *identityv1.Workspace {
		t.Helper()
		ws, ids := f.seedNoDomain(fakeoidc.CarolEmail)
		_, w, err := f.mod.Me(t.Context(), authz.Actor{Kind: authz.ActorUser, UserID: ids[0], WorkspaceID: ws})
		if err != nil {
			t.Fatalf("Me: %v", err)
		}
		if w.GetId() != ws.String() || w.GetGoogleDomain() != "" {
			t.Errorf("workspace = %v, want its id and no domain", w)
		}
		return w
	}

	// A deployment that names its workspace and has no domain: the name shows.
	f := newFixture(t, withConfig(func(c *config.Config) { c.WorkspaceName, c.WorkspaceDomain = "Acme Corp", "" }))
	if w := me(f); w.GetName() != "Acme Corp" {
		t.Errorf("name = %q, want the configured KIWI_WORKSPACE_NAME", w.GetName())
	}
	// Unnamed, the workspace is called what the seed called it.
	g := newFixture(t, withConfig(func(c *config.Config) { c.WorkspaceName, c.WorkspaceDomain = "", "" }))
	if w := me(g); w.GetName() != noDomainWorkspaceName {
		t.Errorf("name = %q, want %q", w.GetName(), noDomainWorkspaceName)
	}
	// A configured name belongs to the configured domain's workspace, not to this one.
	h := newFixture(t)
	if w := me(h); w.GetName() != noDomainWorkspaceName {
		t.Errorf("name = %q, want %q: KIWI_WORKSPACE_NAME names the workspace of KIWI_WORKSPACE_DOMAIN", w.GetName(), noDomainWorkspaceName)
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

	path, h := Handler(f.mod, func(context.Context) authz.Actor { return actor })
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

func TestCallback_RejectsAWrongOrMissingNonce(t *testing.T) {
	f := newFixture(t)
	f.seedWorkspace(fakeoidc.DefaultDomain)

	for _, tc := range []struct {
		name   string
		email  string
		tamper func(url.Values)
	}{
		{"the nonce of another request", fakeoidc.AliceEmail, func(q url.Values) { q.Set("nonce", "nonce-of-some-other-request") }},
		{"no nonce in the request", fakeoidc.AliceEmail, func(q url.Values) { q.Del("nonce") }},
		// An outsider's token is checked for its nonce before the account is named.
		{"an outsider with the wrong nonce", fakeoidc.CarolEmail, func(q url.Values) { q.Set("nonce", "another") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			authURL, bind := f.login("/")
			rec := f.callback(f.approveWith(authURL, tc.email, tc.tamper), bind)

			if got, want := rec.Header().Get("Location"), "/signin?error=exchange_failed"; got != want {
				t.Fatalf("Location = %q, want %q (and no email)", got, want)
			}
			if cookieNamed(rec, SessionCookie) != nil {
				t.Fatal("a session cookie was set")
			}
			if users := f.users(); len(users) != 0 {
				t.Fatalf("rows were written: %+v", users)
			}
			if n := f.sessionKeys(); n != 0 {
				t.Fatalf("Valkey holds %d keys, want none", n)
			}
		})
	}

	// The request's own nonce is accepted.
	if cookieNamed(f.signIn(fakeoidc.AliceEmail), SessionCookie) == nil {
		t.Fatal("a sign-in with the matching nonce was refused")
	}
}

func TestCallback_ReplacesThePreviousSessionOfTheBrowser(t *testing.T) {
	eachProfile(t, testCallback_ReplacesThePreviousSessionOfTheBrowser)
}

func testCallback_ReplacesThePreviousSessionOfTheBrowser(t *testing.T, opts ...setting) {
	f := newFixture(t, opts...)
	f.seedWorkspace(fakeoidc.DefaultDomain)
	first := f.mustSignIn(fakeoidc.AliceEmail)

	// The same browser signs in again with its old cookie still in hand.
	authURL, bind := f.login("/")
	rec := f.callback(f.approve(authURL, fakeoidc.AliceEmail), bind, first)
	second := cookieNamed(rec, SessionCookie)
	if second == nil || second.Value == first.Value {
		t.Fatalf("the second sign-in issued %+v, want a new session", second)
	}
	if _, err := f.mod.Session(t.Context(), first.Value); !errors.Is(err, errs.ErrNotFound) {
		t.Errorf("the previous session after signing in again: err = %v, want ErrNotFound", err)
	}
	if _, err := f.mod.Session(t.Context(), second.Value); err != nil {
		t.Errorf("the new session: %v", err)
	}

	// A cookie that names no session is harmless.
	authURL, bind = f.login("/")
	rec = f.callback(f.approve(authURL, fakeoidc.AliceEmail), bind, &http.Cookie{Name: SessionCookie, Value: "no-such-session"})
	if cookieNamed(rec, SessionCookie) == nil {
		t.Errorf("a stale cookie broke the sign-in: %q", rec.Header().Get("Location"))
	}
}

func TestConnect_RequiresTheProtocolHeader(t *testing.T) {
	f := newFixture(t)
	ws := f.seedWorkspace(fakeoidc.DefaultDomain)
	alice := f.seedUser(ws, aliceSub, fakeoidc.AliceEmail, fakeoidc.AliceName)
	actor := authz.Actor{Kind: authz.ActorUser, UserID: alice, WorkspaceID: ws}

	// No option is passed: the handler adds the requirement itself.
	path, h := Handler(f.mod, func(context.Context) authz.Actor { return actor })
	mux := http.NewServeMux()
	mux.Handle(path, h)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	// A browser can send this cross-site without a preflight: POST with a JSON body.
	call := func(version string) (int, string) {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, srv.URL+path+"IssueRealtimeTicket", strings.NewReader("{}"))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		if version != "" {
			req.Header.Set("Connect-Protocol-Version", version)
		}
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		body, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(body)
	}

	status, body := call("")
	if status != http.StatusBadRequest || !strings.Contains(body, "Connect-Protocol-Version") {
		t.Fatalf("POST without the header: %d %s; want 400 naming Connect-Protocol-Version", status, body)
	}
	if n := f.sessionKeys(); n != 0 {
		t.Fatalf("the refused call reached the service: Valkey holds %d keys", n)
	}

	status, body = call("1")
	if status != http.StatusOK || !strings.Contains(body, `"ticket"`) {
		t.Fatalf("POST with the header: %d %s; want 200 and a ticket", status, body)
	}
	if n := f.sessionKeys(); n != 1 {
		t.Fatalf("Valkey holds %d keys after the accepted call, want the one ticket", n)
	}
}
