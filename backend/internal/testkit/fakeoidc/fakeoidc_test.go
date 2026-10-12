package fakeoidc_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"

	"golang.org/x/net/html"

	"kiwi/internal/testkit/fakeoidc"
	"kiwi/internal/testkit/fakeoidc/fakeoidctest"
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
		"client_id":             {fakeoidc.DefaultClientID},
		"redirect_uri":          {redirectURI},
		"response_type":         {"code"},
		"scope":                 {"openid email profile"},
		"state":                 {"st"},
		"code_challenge":        {challenge("verifier")},
		"code_challenge_method": {"S256"},
		"login_hint":            {fakeoidc.AliceEmail},
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
	form.Set("client_id", fakeoidc.DefaultClientID)
	form.Set("client_secret", fakeoidc.DefaultClientSecret)
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
	p, err := fakeoidc.NewProvider(fakeoidc.Config{
		Issuer:       "http://localhost:8080/mockoidc",
		ClientID:     fakeoidc.DefaultClientID,
		ClientSecret: fakeoidc.DefaultClientSecret,
		HostedDomain: "demo.example",
		Users:        []fakeoidc.User{{Email: "alice@demo.example", Name: "Alice"}},
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
	f := fakeoidctest.New(t)
	for _, tc := range []struct {
		email  string
		hd     string
		hasHD  bool
		wantSu string
	}{
		{fakeoidc.AliceEmail, fakeoidc.DefaultDomain, true, "seed:" + fakeoidc.AliceEmail},
		{fakeoidc.CarolEmail, "", false, "seed:" + fakeoidc.CarolEmail},
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
		if claims["sub"] != tc.wantSu || claims["email"] != tc.email || claims["email_verified"] != true || claims["aud"] != fakeoidc.DefaultClientID || claims["iss"] != f.Issuer() {
			t.Errorf("%s: claims = %v", tc.email, claims)
		}
	}
}

// A NoHD user is an account the way a personal Gmail one is: its token and its
// redirect carry no hd although the provider has a hosted domain, and Unverified
// makes its token say email_verified=false.
func TestProvider_NoHDUsersCarryNoHostedDomain_UnverifiedOnesSayEmailUnverified(t *testing.T) {
	f := fakeoidctest.New(t, fakeoidctest.WithUsers(
		fakeoidc.User{Email: "alice@acme.test", Name: "Alice"},
		fakeoidc.User{Email: "bob@acme.test", Name: "Bob", NoHD: true},
		fakeoidc.User{Email: "quinn@gmail.test", Name: "Quinn", NoHD: true, Unverified: true},
	))
	for _, tc := range []struct {
		email    string
		hd       string
		verified bool
	}{
		{"alice@acme.test", "acme.test", true},
		{"bob@acme.test", "", true},
		{"quinn@gmail.test", "", false},
	} {
		cb := authorize(t, f.URL, url.Values{"login_hint": {tc.email}})
		if got := cb.Query().Get("hd"); got != tc.hd {
			t.Errorf("%s: hd parameter = %q, want %q", tc.email, got, tc.hd)
		}
		status, tok := exchange(t, f.URL, url.Values{
			"grant_type": {"authorization_code"}, "code": {cb.Query().Get("code")},
			"redirect_uri": {redirectURI}, "code_verifier": {"verifier"},
		})
		if status != http.StatusOK {
			t.Fatalf("%s: token endpoint answered %d: %v", tc.email, status, tok)
		}
		claims := claimsOf(t, tok["id_token"].(string))
		if hd, has := claims["hd"]; has != (tc.hd != "") || (has && hd != tc.hd) {
			t.Errorf("%s: hd claim = %v (present %v), want %q", tc.email, hd, has, tc.hd)
		}
		if claims["email_verified"] != tc.verified {
			t.Errorf("%s: email_verified = %v, want %v", tc.email, claims["email_verified"], tc.verified)
		}
	}
}

func TestProvider_PKCEIsMandatoryAndCodesAreSingleUse(t *testing.T) {
	f := fakeoidctest.New(t)

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
	resp, err := http.PostForm(f.URL+"/token", url.Values{"grant_type": {"authorization_code"}, "client_id": {fakeoidc.DefaultClientID}, "client_secret": {"wrong"}})
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("wrong client secret: status %d, want 401", resp.StatusCode)
	}
}

func TestProvider_ChooserOffersAButtonPerAccountAndFlagsTheOnesWithoutHD(t *testing.T) {
	f := fakeoidctest.New(t)
	q := url.Values{
		"client_id": {fakeoidc.DefaultClientID}, "redirect_uri": {redirectURI}, "response_type": {"code"},
		"scope": {"openid"}, "code_challenge": {challenge("v")}, "code_challenge_method": {"S256"},
		"state": {"s"}, "nonce": {"n"},
	}
	pageURL, err := url.Parse(f.URL + "/authorize?" + q.Encode())
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.Get(pageURL.String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/html") {
		t.Fatalf("chooser: %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	if resp.Header.Get("Cache-Control") != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store: the page carries the flow's state", resp.Header.Get("Cache-Control"))
	}

	want := []fakeoidctest.Account{
		{Email: fakeoidc.AliceEmail},
		{Email: fakeoidc.BobEmail},
		{Email: fakeoidc.CarolEmail, Note: "no hosted domain - signs in only if invited"},
	}
	if got := fakeoidctest.ChooserAccounts(t, body); !slices.Equal(got, want) {
		t.Errorf("chooser accounts = %+v, want %+v", got, want)
	}

	// A page that needs no script and no network beyond this server.
	page := string(body)
	if !strings.Contains(page, `<html lang="en"`) {
		t.Error(`the chooser has no <html lang="en">`)
	}
	for _, banned := range []string{"<script", "<link", "<img", "<iframe", "src=", "@import", "url("} {
		if strings.Contains(page, banned) {
			t.Errorf("the chooser contains %q: it must need no script and no external asset", banned)
		}
	}

	// Pressing a button asks the same /authorize with login_hint added, every
	// other parameter of the flow intact, and is answered with the code.
	pressed := fakeoidctest.PressAccount(t, pageURL, body, fakeoidc.AliceEmail)
	if pressed.Path != pageURL.Path {
		t.Errorf("a button leads to %s, want the same authorize endpoint %s", pressed.Path, pageURL.Path)
	}
	wantQ := pageURL.Query()
	wantQ.Set("login_hint", fakeoidc.AliceEmail)
	if got := pressed.Query(); !maps.EqualFunc(got, wantQ, slices.Equal) {
		t.Errorf("a button asks %v, want %v", got, wantQ)
	}
	back, err := noRedirect.Get(pressed.String())
	if err != nil {
		t.Fatal(err)
	}
	_ = back.Body.Close()
	loc, _ := url.Parse(back.Header.Get("Location"))
	if back.StatusCode != http.StatusFound || loc == nil || loc.Query().Get("code") == "" || loc.Query().Get("state") != "s" {
		t.Errorf("pressing alice: %d -> %q, want a redirect carrying a code and the state", back.StatusCode, back.Header.Get("Location"))
	}
}

// The chooser echoes the authorization request into hidden inputs, and that
// request is whatever a stranger put in the URL: the page must carry it back
// byte for byte and never as markup.
func TestProvider_ChooserEscapesAHostileAuthorizationRequest(t *testing.T) {
	f := fakeoidctest.New(t)
	hostileState := `"><script>alert(1)</script>`
	q := url.Values{
		"client_id": {fakeoidc.DefaultClientID}, "redirect_uri": {redirectURI}, "response_type": {"code"},
		"scope": {"openid"}, "code_challenge": {challenge("v")}, "code_challenge_method": {"S256"},
		"state":                           {hostileState},
		"nonce":                           {"it's a & b < c é"},
		`x"><img src=y onerror=alert(1)>`: {`v"><script>alert(2)</script>`, `'><iframe src=z>`},
	}
	pageURL, err := url.Parse(f.URL + "/authorize?" + q.Encode())
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.Get(pageURL.String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("chooser: %d", resp.StatusCode)
	}

	// Raw: no markup of the attacker's survives as markup.
	page := string(body)
	for _, markup := range []string{"<script", "<img", "<iframe"} {
		if strings.Contains(page, markup) {
			t.Errorf("the page contains %q: a request parameter reached the page unescaped", markup)
		}
	}
	// Parsed: no such element and no event-handler or source attribute exists.
	doc, err := html.Parse(bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	var walk func(n *html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode {
			switch n.Data {
			case "script", "img", "iframe", "object", "embed", "link":
				t.Errorf("the page has a <%s> element", n.Data)
			}
			for _, a := range n.Attr {
				if strings.HasPrefix(a.Key, "on") || a.Key == "src" {
					t.Errorf("<%s> has the attribute %q: a request parameter became an attribute", n.Data, a.Key)
				}
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)

	// Every button sends the request back as it came, plus its own login_hint:
	// escaping neither corrupts a value nor drops a parameter.
	for _, email := range []string{fakeoidc.AliceEmail, fakeoidc.BobEmail, fakeoidc.CarolEmail} {
		pressed := fakeoidctest.PressAccount(t, pageURL, body, email)
		want := pageURL.Query()
		want.Set("login_hint", email)
		if got := pressed.Query(); !maps.EqualFunc(got, want, slices.Equal) {
			t.Errorf("the button of %s asks %q, want %q", email, got, want)
		}
	}

	// And the provider reads the hostile state back unchanged.
	pressed := fakeoidctest.PressAccount(t, pageURL, body, fakeoidc.AliceEmail)
	back, err := noRedirect.Get(pressed.String())
	if err != nil {
		t.Fatal(err)
	}
	_ = back.Body.Close()
	loc, _ := url.Parse(back.Header.Get("Location"))
	if back.StatusCode != http.StatusFound || loc == nil || loc.Query().Get("state") != hostileState {
		t.Errorf("pressing alice: %d -> %q, want a redirect that echoes the state unchanged", back.StatusCode, back.Header.Get("Location"))
	}
}

// anyClientServer is a provider that takes whatever client id it is presented,
// as the mock sign-in does, served under /mockoidc.
func anyClientServer(t *testing.T) *httptest.Server {
	t.Helper()
	p, err := fakeoidc.NewProvider(fakeoidc.Config{
		Issuer:       "http://localhost:8080/mockoidc",
		AnyClient:    true,
		HostedDomain: "demo.example",
		Users:        []fakeoidc.User{{Email: "alice@demo.example", Name: "Alice"}},
		RedirectURIs: []string{redirectURI},
	})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(p)
	t.Cleanup(srv.Close)
	return srv
}

// codeFor asks /authorize as clientID and returns the code of the redirect.
func codeFor(t *testing.T, srv *httptest.Server, clientID string) string {
	t.Helper()
	q := url.Values{
		"client_id": {clientID}, "redirect_uri": {redirectURI}, "response_type": {"code"},
		"scope": {"openid"}, "code_challenge": {challenge("verifier")}, "code_challenge_method": {"S256"},
		"login_hint": {"alice@demo.example"},
	}
	resp, err := noRedirect.Get(srv.URL + "/mockoidc/authorize?" + q.Encode())
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	loc, err := url.Parse(resp.Header.Get("Location"))
	if err != nil || resp.StatusCode != http.StatusFound || loc.Query().Get("code") == "" {
		t.Fatalf("authorize as %q: %d -> %q", clientID, resp.StatusCode, resp.Header.Get("Location"))
	}
	return loc.Query().Get("code")
}

func redeem(t *testing.T, srv *httptest.Server, clientID, secret, code string) (int, map[string]any) {
	t.Helper()
	resp, err := http.PostForm(srv.URL+"/mockoidc/token", url.Values{
		"grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {redirectURI},
		"code_verifier": {"verifier"}, "client_id": {clientID}, "client_secret": {secret},
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

func TestProvider_AnyClient_MintsThePresentedClientIDAsAudience(t *testing.T) {
	srv := anyClientServer(t)
	for _, clientID := range []string{"mock", "1234567890-abc.apps.googleusercontent.com"} {
		status, tok := redeem(t, srv, clientID, "whatever", codeFor(t, srv, clientID))
		if status != http.StatusOK {
			t.Fatalf("client %q: token endpoint answered %d: %v", clientID, status, tok)
		}
		if aud := claimsOf(t, tok["id_token"].(string))["aud"]; aud != clientID {
			t.Errorf("client %q: aud = %v, want the client id that was presented", clientID, aud)
		}
	}
}

func TestProvider_AnyClient_BindsACodeToTheClientItWasIssuedTo(t *testing.T) {
	srv := anyClientServer(t)
	status, body := redeem(t, srv, "client-b", "s", codeFor(t, srv, "client-a"))
	if status != http.StatusBadRequest || body["error"] != "invalid_grant" {
		t.Errorf("a code of client-a redeemed by client-b: %d %v, want invalid_grant", status, body)
	}
}

func TestProvider_AnyClient_StillNeedsAClientIDAndASecret(t *testing.T) {
	srv := anyClientServer(t)

	resp, err := noRedirect.Get(srv.URL + "/mockoidc/authorize?" + url.Values{
		"redirect_uri": {redirectURI}, "response_type": {"code"}, "scope": {"openid"},
		"code_challenge": {challenge("verifier")}, "code_challenge_method": {"S256"}, "login_hint": {"alice@demo.example"},
	}.Encode())
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest || resp.Header.Get("Location") != "" {
		t.Errorf("authorize without a client_id: %d -> %q, want 400 and no redirect", resp.StatusCode, resp.Header.Get("Location"))
	}

	for name, creds := range map[string][2]string{"no secret": {"mock", ""}, "no client id": {"", "s"}, "neither": {"", ""}} {
		if status, body := redeem(t, srv, creds[0], creds[1], codeFor(t, srv, "mock")); status != http.StatusUnauthorized || body["error"] != "invalid_client" {
			t.Errorf("%s: %d %v, want 401 invalid_client", name, status, body)
		}
	}
}

func TestProvider_RevokeEndsRefreshTokensAndForgetsTheGrant(t *testing.T) {
	f := fakeoidctest.New(t)
	cb := authorize(t, f.URL, url.Values{"access_type": {"offline"}})
	_, tok := exchange(t, f.URL, url.Values{
		"grant_type": {"authorization_code"}, "code": {cb.Query().Get("code")},
		"redirect_uri": {redirectURI}, "code_verifier": {"verifier"},
	})
	rt, _ := tok["refresh_token"].(string)
	if rt == "" || f.RefreshToken(fakeoidc.AliceEmail) != rt {
		t.Fatalf("no refresh token for an offline request: %v", tok)
	}
	if status, _ := exchange(t, f.URL, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {rt}}); status != http.StatusOK {
		t.Fatalf("refresh before revocation: status %d", status)
	}

	f.Revoke(fakeoidc.AliceEmail)
	if status, body := exchange(t, f.URL, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {rt}}); status != http.StatusBadRequest || body["error"] != "invalid_grant" {
		t.Errorf("refresh after revocation: %d %v", status, body)
	}
	if f.RefreshToken(fakeoidc.AliceEmail) != "" {
		t.Error("the revoked refresh token is still reported")
	}
}

func TestProvider_RedirectsOnlyToRegisteredURIs(t *testing.T) {
	f := fakeoidctest.New(t, fakeoidctest.WithRedirectURIs(redirectURI, "http://localhost:5173/api/auth/callback"))

	get := func(uri string, hint bool) (*http.Response, string) {
		q := url.Values{
			"client_id": {fakeoidc.DefaultClientID}, "redirect_uri": {uri}, "response_type": {"code"},
			"scope": {"openid"}, "code_challenge": {challenge("v")}, "code_challenge_method": {"S256"}, "state": {"s"},
		}
		if hint {
			q.Set("login_hint", fakeoidc.AliceEmail)
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
	ok := fakeoidc.Config{
		Issuer: "http://localhost:8080/mockoidc", ClientID: "c", ClientSecret: "s", HostedDomain: "x.test",
		Users: []fakeoidc.User{{Email: "a@x.test"}}, RedirectURIs: []string{"http://localhost:8080/api/auth/callback"},
	}
	for name, mod := range map[string]func(*fakeoidc.Config){
		"relative issuer":                  func(c *fakeoidc.Config) { c.Issuer = "/mockoidc" },
		"issuer with query":                func(c *fakeoidc.Config) { c.Issuer = "http://localhost:8080/?a=b" },
		"no client id":                     func(c *fakeoidc.Config) { c.ClientID = "" },
		"no client secret":                 func(c *fakeoidc.Config) { c.ClientSecret = "" },
		"users without a hd":               func(c *fakeoidc.Config) { c.HostedDomain = "" },
		"user without email":               func(c *fakeoidc.Config) { c.Users = []fakeoidc.User{{Name: "x"}} },
		"no redirect uris":                 func(c *fakeoidc.Config) { c.RedirectURIs = nil },
		"relative redirect":                func(c *fakeoidc.Config) { c.RedirectURIs = []string{"/api/auth/callback"} },
		"redirect with hash":               func(c *fakeoidc.Config) { c.RedirectURIs = []string{"http://localhost:8080/cb#x"} },
		"non-http redirect":                func(c *fakeoidc.Config) { c.RedirectURIs = []string{"javascript:alert(1)"} },
		"any client and a fixed client id": func(c *fakeoidc.Config) { c.AnyClient = true },
		"any client and a fixed secret":    func(c *fakeoidc.Config) { c.AnyClient, c.ClientID = true, "" },
	} {
		c := ok
		mod(&c)
		if _, err := fakeoidc.NewProvider(c); err == nil {
			t.Errorf("%s: NewProvider accepted the config", name)
		}
	}
	if _, err := fakeoidc.NewProvider(ok); err != nil {
		t.Errorf("the valid config was rejected: %v", err)
	}
	anyClient := ok
	anyClient.AnyClient, anyClient.ClientID, anyClient.ClientSecret = true, "", ""
	if _, err := fakeoidc.NewProvider(anyClient); err != nil {
		t.Errorf("a provider that takes any client was rejected: %v", err)
	}
}
