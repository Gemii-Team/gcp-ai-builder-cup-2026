package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"kiwi/internal/platform/oidc"
	"kiwi/internal/testkit/fakeoidc/fakeoidctest"
)

const (
	// callback is the redirect_uri of a K.I.W.I. on http://localhost:8080.
	callback = "http://localhost:8080/api/auth/callback"
	// clientID is not "mock": the provider takes the id it is presented.
	clientID = "kiwi-local-client"
	verifier = "dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk"
	noHD     = "no hosted domain - signs in only if invited"
)

var noRedirect = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

// environ is a getenv over name/value pairs; everything else is unset.
func environ(kv ...string) func(string) string {
	m := map[string]string{}
	for i := 0; i+1 < len(kv); i += 2 {
		m[kv[i]] = kv[i+1]
	}
	return func(k string) string { return m[k] }
}

// startMock serves the provider the environment kv configures on a loopback
// port and returns it with a verifier built the way K.I.W.I. builds its own:
// discovery through the address the server really listens at (the pod's, in the
// cluster) while tokens name the public issuer.
func startMock(t *testing.T, issuer string, kv ...string) (*httptest.Server, *oidc.Verifier) {
	t.Helper()
	p, err := newProvider(environ(kv...))
	if err != nil {
		t.Fatalf("newProvider: %v", err)
	}
	srv := httptest.NewServer(p)
	t.Cleanup(srv.Close)
	v, err := oidc.NewVerifier(t.Context(), issuer, srv.URL+"/mockoidc", clientID)
	if err != nil {
		t.Fatalf("oidc.NewVerifier against the mock: %v", err)
	}
	return srv, v
}

const publicIssuer = "http://localhost:8080/mockoidc"

func challenge() string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// authorizeURL is the authorization request K.I.W.I. sends the browser to,
// addressed to srv (the browser reaches the mock through the Ingress; the test
// reaches it directly).
func authorizeURL(t *testing.T, v *oidc.Verifier, srv *httptest.Server, extra url.Values) *url.URL {
	t.Helper()
	auth, err := url.Parse(v.Endpoint().AuthURL)
	if err != nil {
		t.Fatal(err)
	}
	q := url.Values{
		"client_id": {clientID}, "redirect_uri": {callback}, "response_type": {"code"},
		"scope": {"openid email profile"}, "state": {"st"}, "nonce": {"n-123"},
		"code_challenge": {challenge()}, "code_challenge_method": {"S256"},
	}
	for k, v := range extra {
		q[k] = v
	}
	u, err := url.Parse(srv.URL + auth.Path)
	if err != nil {
		t.Fatal(err)
	}
	u.RawQuery = q.Encode()
	return u
}

func get(t *testing.T, u *url.URL) (*http.Response, []byte) {
	t.Helper()
	resp, err := noRedirect.Get(u.String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp, body
}

// codeFrom follows a redirect to the callback and returns its code.
func codeFrom(t *testing.T, resp *http.Response) string {
	t.Helper()
	loc, err := url.Parse(resp.Header.Get("Location"))
	if resp.StatusCode != http.StatusFound || err != nil || !strings.HasPrefix(loc.String(), callback+"?") {
		t.Fatalf("authorize answered %d -> %q, want a redirect to %s", resp.StatusCode, resp.Header.Get("Location"), callback)
	}
	if loc.Query().Get("state") != "st" || loc.Query().Get("code") == "" {
		t.Fatalf("callback %s carries no code or the wrong state", loc)
	}
	return loc.Query().Get("code")
}

// exchange redeems code at the token endpoint the discovery document names.
func exchange(t *testing.T, v *oidc.Verifier, code, codeVerifier string) (int, map[string]any) {
	t.Helper()
	resp, err := http.PostForm(v.Endpoint().TokenURL, url.Values{
		"grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {callback},
		"code_verifier": {codeVerifier}, "client_id": {clientID}, "client_secret": {"mock"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, body
}

// signIn plays the browser through the chooser (pick) or straight through
// login_hint (!pick) and returns the ID token.
func signIn(t *testing.T, srv *httptest.Server, v *oidc.Verifier, email string, pick bool) string {
	t.Helper()
	var code string
	if pick {
		page := authorizeURL(t, v, srv, nil)
		resp, body := get(t, page)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("the chooser answered %d", resp.StatusCode)
		}
		code = codeFrom(t, mustGet(t, fakeoidctest.PressAccount(t, page, body, email)))
	} else {
		resp, _ := get(t, authorizeURL(t, v, srv, url.Values{"login_hint": {email}}))
		code = codeFrom(t, resp)
	}
	status, tok := exchange(t, v, code, verifier)
	if status != http.StatusOK {
		t.Fatalf("token endpoint answered %d: %v", status, tok)
	}
	idToken, _ := tok["id_token"].(string)
	if idToken == "" {
		t.Fatalf("no id_token in %v", tok)
	}
	return idToken
}

func mustGet(t *testing.T, u *url.URL) *http.Response {
	t.Helper()
	resp, _ := get(t, u)
	return resp
}

func TestMockOIDC_FullCodeFlowWithPKCE_VerifiesWithKiwiVerifier(t *testing.T) {
	for _, tc := range []struct {
		name  string
		env   []string
		hd    string
		email string
		// display is the name claim: the local part, capitalised.
		display string
		other   string
		// otherHD is the hd of the other user's token: empty for a user marked :no-hd.
		otherHD string
	}{
		{
			name: "defaults", hd: "demo.example",
			email: "alice@demo.example", display: "Alice", other: "bob@demo.example", otherHD: "",
		},
		{
			name:  "MOCKOIDC_HD and MOCKOIDC_USERS",
			env:   []string{"MOCKOIDC_HD", "acme.example", "MOCKOIDC_USERS", "dora@acme.example, erin@acme.example"},
			hd:    "acme.example",
			email: "dora@acme.example", display: "Dora", other: "erin@acme.example", otherHD: "acme.example",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, v := startMock(t, publicIssuer, tc.env...)

			if v.Endpoint().AuthURL != publicIssuer+"/authorize" {
				t.Errorf("authorization endpoint = %s, want the browser's address %s/authorize", v.Endpoint().AuthURL, publicIssuer)
			}
			if v.Endpoint().TokenURL != srv.URL+"/mockoidc/token" {
				t.Errorf("token endpoint = %s, want the address discovery went through", v.Endpoint().TokenURL)
			}

			// Through the chooser, the way a developer signs in.
			c, err := v.Verify(t.Context(), signIn(t, srv, v, tc.email, true))
			if err != nil {
				t.Fatalf("the production verifier rejects the mock's token: %v", err)
			}
			if c.HD != tc.hd || c.Sub != "seed:"+tc.email || c.Email != tc.email || c.Name != tc.display || !c.EmailVerified || c.Nonce != "n-123" {
				t.Errorf("claims = %+v, want hd %s, sub seed:%s, name %s, the nonce echoed", c, tc.hd, tc.email, tc.display)
			}

			// With login_hint the code is issued without the page.
			c, err = v.Verify(t.Context(), signIn(t, srv, v, tc.other, false))
			if (tc.otherHD == "") != errors.Is(err, oidc.ErrNoHostedDomain) || (tc.otherHD != "" && err != nil) || c.HD != tc.otherHD || c.Sub != "seed:"+tc.other {
				t.Errorf("login_hint sign-in: claims %+v, err %v, want hd %q", c, err, tc.otherHD)
			}
		})
	}
}

func TestMockOIDC_ChooserListsEveryConfiguredUserAndTheOutsider(t *testing.T) {
	for _, tc := range []struct {
		name string
		env  []string
		want []fakeoidctest.Account
	}{
		{"defaults", nil, []fakeoidctest.Account{
			{Email: "alice@demo.example"}, {Email: "bob@demo.example", Note: noHD}, {Email: "carol@elsewhere.example", Note: noHD},
		}},
		{"configured", []string{"MOCKOIDC_USERS", "a@x.test,b@x.test:no-hd, c@x.test", "MOCKOIDC_OUTSIDER", "z@gmail.test"}, []fakeoidctest.Account{
			{Email: "a@x.test"}, {Email: "b@x.test", Note: noHD}, {Email: "c@x.test"}, {Email: "z@gmail.test", Note: noHD},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, v := startMock(t, publicIssuer, tc.env...)
			resp, body := get(t, authorizeURL(t, v, srv, nil))
			if resp.StatusCode != http.StatusOK || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/html") {
				t.Fatalf("chooser: %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
			}
			if got := fakeoidctest.ChooserAccounts(t, body); !slices.Equal(got, tc.want) {
				t.Errorf("accounts = %+v, want %+v", got, tc.want)
			}
			page := string(body)
			if !strings.Contains(page, `<html lang="en"`) || strings.Contains(page, "<script") || strings.Contains(page, "<link") {
				t.Errorf("the chooser must be plain HTML with lang=en, no script and no external asset:\n%s", page)
			}
		})
	}
}

// Alice's token carries hd; Bob (seeded, signing in through the invite rule) and
// Carol (not seeded) have none, and the production verifier says so while still
// returning who they are.
func TestMockOIDC_TokensOfUsersWithoutHD_CarryNoHDClaim(t *testing.T) {
	srv, v := startMock(t, publicIssuer)
	for _, tc := range []struct {
		email  string
		hd     string
		name   string
		hasHD  bool
		wantHD error
	}{
		{"alice@demo.example", "demo.example", "Alice", true, nil},
		{"bob@demo.example", "", "Bob", false, oidc.ErrNoHostedDomain},
		{"carol@elsewhere.example", "", "Carol", false, oidc.ErrNoHostedDomain},
	} {
		raw := signIn(t, srv, v, tc.email, true)

		c, err := v.Verify(t.Context(), raw)
		if !errors.Is(err, tc.wantHD) {
			t.Fatalf("%s: Verify = %v, want %v", tc.email, err, tc.wantHD)
		}
		if c.Email != tc.email || c.Sub != "seed:"+tc.email || c.Name != tc.name || c.HD != tc.hd || !c.EmailVerified {
			t.Errorf("%s: claims = %+v, want the verified identity with hd %q", tc.email, c, tc.hd)
		}
		payload, err := base64.RawURLEncoding.DecodeString(strings.Split(raw, ".")[1])
		if err != nil {
			t.Fatal(err)
		}
		var claims map[string]any
		if err := json.Unmarshal(payload, &claims); err != nil {
			t.Fatal(err)
		}
		if _, has := claims["hd"]; has != tc.hasHD {
			t.Errorf("%s: hd claim present = %v (%v), want %v", tc.email, has, claims["hd"], tc.hasHD)
		}
	}
}

func TestMockOIDC_PKCEMismatchRejected(t *testing.T) {
	srv, v := startMock(t, publicIssuer)

	code := codeFrom(t, mustGet(t, authorizeURL(t, v, srv, url.Values{"login_hint": {"alice@demo.example"}})))
	status, body := exchange(t, v, code, "not-the-verifier-of-this-challenge-0123456789abcdef")
	if status != http.StatusBadRequest || body["error"] != "invalid_grant" || body["id_token"] != nil {
		t.Errorf("wrong code_verifier: %d %v, want 400 invalid_grant and no token", status, body)
	}
	// The failed attempt spent the code.
	if status, _ := exchange(t, v, code, verifier); status != http.StatusBadRequest {
		t.Errorf("the code survived a failed exchange: status %d", status)
	}

	// A request without PKCE never gets a code.
	resp, _ := get(t, authorizeURL(t, v, srv, url.Values{"login_hint": {"alice@demo.example"}, "code_challenge": {""}}))
	if loc := resp.Header.Get("Location"); !strings.Contains(loc, "error=invalid_request") || strings.Contains(loc, "code=") {
		t.Errorf("an authorization request without a challenge: Location %q, want error=invalid_request and no code", loc)
	}
}

func TestMockOIDC_RedirectsOnlyToTheCallbackOfThePublicURL(t *testing.T) {
	for _, tc := range []struct {
		name    string
		env     []string
		allowed []string
	}{
		{"default", nil, []string{callback}},
		{"MOCKOIDC_REDIRECT_URIS", []string{"MOCKOIDC_REDIRECT_URIS", "http://localhost:5173/api/auth/callback, " + callback}, []string{"http://localhost:5173/api/auth/callback", callback}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, v := startMock(t, publicIssuer, tc.env...)
			for _, uri := range []string{"http://evil.example/cb", "http://localhost:8080/api/auth/callback/", "https://localhost:8080/api/auth/callback", ""} {
				for _, hint := range []string{"alice@demo.example", ""} {
					extra := url.Values{"redirect_uri": {uri}}
					if hint != "" {
						extra.Set("login_hint", hint)
					}
					resp, _ := get(t, authorizeURL(t, v, srv, extra))
					if resp.StatusCode != http.StatusBadRequest || resp.Header.Get("Location") != "" {
						t.Errorf("redirect_uri %q (login_hint %q): %d -> %q, want 400 and no redirect", uri, hint, resp.StatusCode, resp.Header.Get("Location"))
					}
				}
			}
			for _, uri := range tc.allowed {
				resp, _ := get(t, authorizeURL(t, v, srv, url.Values{"redirect_uri": {uri}, "login_hint": {"alice@demo.example"}}))
				if resp.StatusCode != http.StatusFound || !strings.HasPrefix(resp.Header.Get("Location"), uri+"?") {
					t.Errorf("registered %q: %d -> %q, want a redirect to it", uri, resp.StatusCode, resp.Header.Get("Location"))
				}
			}
		})
	}
}

func TestMockOIDC_RefusesToStartInProd(t *testing.T) {
	_, err := newProvider(environ("KIWI_ENV", "prod"))
	if err == nil || !strings.Contains(err.Error(), "KIWI_ENV") {
		t.Fatalf("newProvider with KIWI_ENV=prod = %v, want an error naming KIWI_ENV", err)
	}

	// Fail closed: only an unset KIWI_ENV or "local" starts the mock.
	for _, env := range []string{"Prod", "production", "staging", " prod "} {
		if _, err := newProvider(environ("KIWI_ENV", env)); err == nil || !strings.Contains(err.Error(), "KIWI_ENV") {
			t.Errorf("KIWI_ENV=%q: %v, want an error naming KIWI_ENV", env, err)
		}
	}
	for _, env := range []string{"", "local"} {
		if _, err := newProvider(environ("KIWI_ENV", env)); err != nil {
			t.Errorf("KIWI_ENV=%q: %v, want the mock to start", env, err)
		}
	}
}

func TestRun_ProdExitsWithCode1NamingKIWI_ENV(t *testing.T) {
	// The deadline only matters when the guard is broken: run would then serve
	// until it passes instead of returning 1 at once.
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	var stderr bytes.Buffer
	if code := run(ctx, environ("KIWI_ENV", "prod"), &stderr); code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "KIWI_ENV") {
		t.Errorf("stderr %q does not name KIWI_ENV", stderr.String())
	}
}

func TestMockOIDC_RefusesBrokenConfigurationNamingTheVariable(t *testing.T) {
	for _, tc := range []struct {
		name string
		kv   []string
	}{
		{"MOCKOIDC_USERS", []string{"MOCKOIDC_USERS", "alice@demo.example,not-an-email"}},
		{"MOCKOIDC_USERS", []string{"MOCKOIDC_USERS", "alice@demo.example,bob@demo.example:nope"}},
		{"MOCKOIDC_USERS", []string{"MOCKOIDC_USERS", "alice@demo.example,ALICE@demo.example:no-hd"}},
		{"MOCKOIDC_OUTSIDER", []string{"MOCKOIDC_OUTSIDER", "not-an-email"}},
		{"MOCKOIDC_OUTSIDER", []string{"MOCKOIDC_OUTSIDER", "bob@demo.example"}},
		{"MOCKOIDC_ISSUER", []string{"MOCKOIDC_ISSUER", "localhost:8080/mockoidc"}},
		{"MOCKOIDC_REDIRECT_URIS", []string{"MOCKOIDC_REDIRECT_URIS", "/api/auth/callback"}},
		{"MOCKOIDC_HD", []string{"MOCKOIDC_HD", "demo.example, other.example"}},
	} {
		_, err := newProvider(environ(tc.kv...))
		if err == nil || !strings.Contains(err.Error(), tc.name) {
			t.Errorf("%s=%q: %v, want an error naming %s", tc.kv[0], tc.kv[1], err, tc.name)
		}
	}
}
