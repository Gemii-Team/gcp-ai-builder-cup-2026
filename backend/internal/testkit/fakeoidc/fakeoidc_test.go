package fakeoidc

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

const redirectURI = "http://localhost:8080/api/auth/callback"

var noRedirect = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

func challenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// authorize asks the provider at base to approve email and returns the redirect.
func authorize(t *testing.T, base string, extra url.Values) *url.URL {
	t.Helper()
	q := url.Values{
		"client_id":             {DefaultClientID},
		"redirect_uri":          {redirectURI},
		"response_type":         {"code"},
		"scope":                 {"openid email profile"},
		"state":                 {"st"},
		"code_challenge":        {challenge("verifier")},
		"code_challenge_method": {"S256"},
		"login_hint":            {AliceEmail},
	}
	for k, v := range extra {
		q[k] = v
	}
	resp, err := noRedirect.Get(base + "/authorize?" + q.Encode())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("authorize answered %d, want a redirect", resp.StatusCode)
	}
	loc, err := url.Parse(resp.Header.Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	return loc
}

func exchange(t *testing.T, base string, form url.Values) (int, map[string]any) {
	t.Helper()
	form.Set("client_id", DefaultClientID)
	form.Set("client_secret", DefaultClientSecret)
	resp, err := http.PostForm(base+"/token", form)
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

// claimsOf decodes the payload of an ID token without checking it.
func claimsOf(t *testing.T, jwt string) map[string]any {
	t.Helper()
	parts := strings.Split(jwt, ".")
	if len(parts) != 3 {
		t.Fatalf("not a JWT: %q", jwt)
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatal(err)
	}
	var c map[string]any
	if err := json.Unmarshal(raw, &c); err != nil {
		t.Fatal(err)
	}
	return c
}

func TestProvider_ServesUnderTheIssuerPath(t *testing.T) {
	// The mock stack mounts the provider at /mockoidc behind an ingress on
	// localhost:8080 while the backend discovers it at the pod's own address.
	p, err := NewProvider(Config{
		Issuer:       "http://localhost:8080/mockoidc",
		ClientID:     DefaultClientID,
		ClientSecret: DefaultClientSecret,
		HostedDomain: "demo.example",
		Users:        []User{{Email: "alice@demo.example", Name: "Alice"}},
		RedirectURIs: []string{"http://localhost:8080/api/auth/callback"},
	})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(p)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/.well-known/openid-configuration")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("discovery at the root: status %d, want 404", resp.StatusCode)
	}

	resp, err = http.Get(srv.URL + "/mockoidc/.well-known/openid-configuration")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var doc map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		t.Fatal(err)
	}
	for k, want := range map[string]string{
		"issuer":                 "http://localhost:8080/mockoidc",
		"authorization_endpoint": "http://localhost:8080/mockoidc/authorize", // the browser's address
		"token_endpoint":         srv.URL + "/mockoidc/token",                // the address discovery went through
		"jwks_uri":               srv.URL + "/mockoidc/jwks",
	} {
		if doc[k] != want {
			t.Errorf("%s = %v, want %s", k, doc[k], want)
		}
	}

	keys, err := http.Get(srv.URL + "/mockoidc/jwks")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = keys.Body.Close() }()
	if keys.StatusCode != http.StatusOK {
		t.Errorf("jwks: status %d", keys.StatusCode)
	}
}

func TestProvider_AddsHostedDomainForWorkspaceAccountsOnly(t *testing.T) {
	f := New(t)
	for _, tc := range []struct {
		email  string
		hd     string
		hasHD  bool
		wantSu string
	}{
		{AliceEmail, DefaultDomain, true, "seed:" + AliceEmail},
		{CarolEmail, "", false, "seed:" + CarolEmail},
	} {
		cb := authorize(t, f.URL, url.Values{"login_hint": {tc.email}})
		if got := cb.Query().Get("hd"); got != tc.hd {
			t.Errorf("%s: hd parameter = %q, want %q", tc.email, got, tc.hd)
		}
		if cb.Query().Get("state") != "st" || cb.Query().Get("code") == "" {
			t.Errorf("%s: redirect %s carries no state or code", tc.email, cb)
		}
		status, tok := exchange(t, f.URL, url.Values{
			"grant_type": {"authorization_code"}, "code": {cb.Query().Get("code")},
			"redirect_uri": {redirectURI}, "code_verifier": {"verifier"},
		})
		if status != http.StatusOK {
			t.Fatalf("%s: token endpoint answered %d: %v", tc.email, status, tok)
		}
		claims := claimsOf(t, tok["id_token"].(string))
		if _, has := claims["hd"]; has != tc.hasHD {
			t.Errorf("%s: hd claim present = %v, want %v", tc.email, has, tc.hasHD)
		}
		if claims["sub"] != tc.wantSu || claims["email"] != tc.email || claims["email_verified"] != true || claims["aud"] != DefaultClientID || claims["iss"] != f.Issuer() {
			t.Errorf("%s: claims = %v", tc.email, claims)
		}
	}
}

func TestProvider_PKCEIsMandatoryAndCodesAreSingleUse(t *testing.T) {
	f := New(t)

	for name, drop := range map[string]url.Values{
		"no challenge":    {"code_challenge": {""}},
		"plain challenge": {"code_challenge_method": {"plain"}},
		"no openid scope": {"scope": {"email"}},
		"not a code flow": {"response_type": {"token"}},
	} {
		if got := authorize(t, f.URL, drop).Query().Get("error"); got == "" {
			t.Errorf("%s: the provider approved the request", name)
		}
	}

	cb := authorize(t, f.URL, nil)
	code := cb.Query().Get("code")
	base := url.Values{"grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {redirectURI}}

	wrong := url.Values{"code_verifier": {"another-verifier"}}
	for k, v := range base {
		wrong[k] = v
	}
	if status, body := exchange(t, f.URL, wrong); status != http.StatusBadRequest || body["error"] != "invalid_grant" {
		t.Fatalf("wrong verifier: %d %v, want invalid_grant", status, body)
	}
	// Even the failed attempt spent the code.
	right := url.Values{"code_verifier": {"verifier"}}
	for k, v := range base {
		right[k] = v
	}
	if status, _ := exchange(t, f.URL, right); status != http.StatusBadRequest {
		t.Fatalf("a code survived a failed exchange: status %d", status)
	}

	// A fresh code with a wrong redirect_uri or client secret is refused as well.
	cb = authorize(t, f.URL, nil)
	if status, body := exchange(t, f.URL, url.Values{
		"grant_type": {"authorization_code"}, "code": {cb.Query().Get("code")},
		"redirect_uri": {"http://evil.example/cb"}, "code_verifier": {"verifier"},
	}); status != http.StatusBadRequest || body["error"] != "invalid_grant" {
		t.Errorf("wrong redirect_uri: %d %v", status, body)
	}
	resp, err := http.PostForm(f.URL+"/token", url.Values{"grant_type": {"authorization_code"}, "client_id": {DefaultClientID}, "client_secret": {"wrong"}})
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("wrong client secret: status %d, want 401", resp.StatusCode)
	}
}

func TestProvider_ChooserListsEveryAccountWithoutALoginHint(t *testing.T) {
	f := New(t)
	q := url.Values{
		"client_id": {DefaultClientID}, "redirect_uri": {redirectURI}, "response_type": {"code"},
		"scope": {"openid"}, "code_challenge": {challenge("v")}, "code_challenge_method": {"S256"}, "state": {"s"},
	}
	resp, err := http.Get(f.URL + "/authorize?" + q.Encode())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	page := string(body)
	if resp.StatusCode != http.StatusOK || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/html") {
		t.Fatalf("chooser: %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	for _, email := range []string{AliceEmail, BobEmail, CarolEmail} {
		if !strings.Contains(page, "login_hint="+url.QueryEscape(email)) {
			t.Errorf("the chooser has no link for %s", email)
		}
	}
}

func TestProvider_RevokeEndsRefreshTokensAndForgetsTheGrant(t *testing.T) {
	f := New(t)
	cb := authorize(t, f.URL, url.Values{"access_type": {"offline"}})
	_, tok := exchange(t, f.URL, url.Values{
		"grant_type": {"authorization_code"}, "code": {cb.Query().Get("code")},
		"redirect_uri": {redirectURI}, "code_verifier": {"verifier"},
	})
	rt, _ := tok["refresh_token"].(string)
	if rt == "" || f.RefreshToken(AliceEmail) != rt {
		t.Fatalf("no refresh token for an offline request: %v", tok)
	}
	if status, _ := exchange(t, f.URL, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {rt}}); status != http.StatusOK {
		t.Fatalf("refresh before revocation: status %d", status)
	}

	f.Revoke(AliceEmail)
	if status, body := exchange(t, f.URL, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {rt}}); status != http.StatusBadRequest || body["error"] != "invalid_grant" {
		t.Errorf("refresh after revocation: %d %v", status, body)
	}
	if f.RefreshToken(AliceEmail) != "" {
		t.Error("the revoked refresh token is still reported")
	}
}

func TestProvider_RedirectsOnlyToRegisteredURIs(t *testing.T) {
	f := New(t, WithRedirectURIs(redirectURI, "http://localhost:5173/api/auth/callback"))

	get := func(uri string, hint bool) (*http.Response, string) {
		q := url.Values{
			"client_id": {DefaultClientID}, "redirect_uri": {uri}, "response_type": {"code"},
			"scope": {"openid"}, "code_challenge": {challenge("v")}, "code_challenge_method": {"S256"}, "state": {"s"},
		}
		if hint {
			q.Set("login_hint", AliceEmail)
		}
		resp, err := noRedirect.Get(f.URL + "/authorize?" + q.Encode())
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		body, _ := io.ReadAll(resp.Body)
		return resp, string(body)
	}

	for _, uri := range []string{
		"http://evil.example/cb",
		"https://localhost:8080/api/auth/callback",        // another scheme
		"http://localhost:8080/api/auth/callback/",        // a trailing slash
		"http://localhost:8080/api/auth/callback?next=/x", // a query nobody registered
		"http://localhost:8080/api/auth/callback#frag",    // a fragment
		"http://localhost:8080/api/auth/CALLBACK",         // another case
		"http://localhost:8080.evil.example/api/auth/callback",
		"",
	} {
		for _, hint := range []bool{true, false} {
			resp, body := get(uri, hint)
			if resp.StatusCode != http.StatusBadRequest || resp.Header.Get("Location") != "" {
				t.Errorf("redirect_uri %q (login_hint %v): status %d, Location %q; want 400 and no redirect", uri, hint, resp.StatusCode, resp.Header.Get("Location"))
			}
			if strings.Contains(body, "evil.example") {
				t.Errorf("redirect_uri %q is reflected in the error page", uri)
			}
		}
	}

	for _, uri := range []string{redirectURI, "http://localhost:5173/api/auth/callback"} {
		resp, _ := get(uri, true)
		if resp.StatusCode != http.StatusFound || !strings.HasPrefix(resp.Header.Get("Location"), uri+"?") {
			t.Errorf("registered %q: status %d, Location %q; want a redirect to it", uri, resp.StatusCode, resp.Header.Get("Location"))
		}
	}
}

func TestNewProvider_RejectsBrokenConfigs(t *testing.T) {
	ok := Config{
		Issuer: "http://localhost:8080/mockoidc", ClientID: "c", ClientSecret: "s", HostedDomain: "x.test",
		Users: []User{{Email: "a@x.test"}}, RedirectURIs: []string{"http://localhost:8080/api/auth/callback"},
	}
	for name, mod := range map[string]func(*Config){
		"relative issuer":    func(c *Config) { c.Issuer = "/mockoidc" },
		"issuer with query":  func(c *Config) { c.Issuer = "http://localhost:8080/?a=b" },
		"no client id":       func(c *Config) { c.ClientID = "" },
		"no client secret":   func(c *Config) { c.ClientSecret = "" },
		"users without a hd": func(c *Config) { c.HostedDomain = "" },
		"user without email": func(c *Config) { c.Users = []User{{Name: "x"}} },
		"no redirect uris":   func(c *Config) { c.RedirectURIs = nil },
		"relative redirect":  func(c *Config) { c.RedirectURIs = []string{"/api/auth/callback"} },
		"redirect with hash": func(c *Config) { c.RedirectURIs = []string{"http://localhost:8080/cb#x"} },
		"non-http redirect":  func(c *Config) { c.RedirectURIs = []string{"javascript:alert(1)"} },
	} {
		c := ok
		mod(&c)
		if _, err := NewProvider(c); err == nil {
			t.Errorf("%s: NewProvider accepted the config", name)
		}
	}
	if _, err := NewProvider(ok); err != nil {
		t.Errorf("the valid config was rejected: %v", err)
	}
}
