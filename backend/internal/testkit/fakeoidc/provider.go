// Package fakeoidc is an OpenID Connect provider shaped like Google's, for tests
// and for the smoke stack's mock sign-in (backend/tools/mockoidc serves a
// Provider as a standalone binary).
//
// It speaks the authorization-code flow with PKCE S256 only: discovery, JWKS,
// /authorize (which approves the account named by login_hint, or lists the
// accounts to pick from when there is none) and /token (code and refresh-token
// grants). Like Google it adds hd to the redirect and the hd claim to the ID
// token for Workspace accounts and for them only, issues a refresh token to a
// client that asks for offline access once per grant, and reports granted
// scopes under their full URLs.
package fakeoidc

import (
	"cmp"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"math/big"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"
)

// Scopes Google reports under their URLs although a client asks for them as
// "email" and "profile".
const (
	scopeEmail   = "https://www.googleapis.com/auth/userinfo.email"
	scopeProfile = "https://www.googleapis.com/auth/userinfo.profile"
)

const (
	codeTTL        = 5 * time.Minute
	accessTokenTTL = time.Hour
)

// User is an account that can sign in. Sub is the ID token's subject; it
// defaults to "seed:" + Email, which is what a seeded placeholder user carries
// in identity.users, so a mock sign-in lands on the seeded user.
type User struct {
	Email string
	Name  string
	Sub   string
}

func (u User) sub() string { return cmp.Or(u.Sub, "seed:"+u.Email) }

// Config configures a Provider.
type Config struct {
	// Issuer is the issuer URL exactly as tokens carry it and as the browser
	// reaches the provider. The provider serves its endpoints under the path of
	// this URL (for "http://localhost:8080/mockoidc": /mockoidc/authorize, ...),
	// so the same provider answers on any host that routes that path to it.
	Issuer       string
	ClientID     string
	ClientSecret string
	// HostedDomain is the hd claim of Users.
	HostedDomain string
	// Users are Workspace accounts: their ID tokens carry hd.
	Users []User
	// Outsider, when set, is an account without hd, like a personal Gmail one.
	Outsider *User
	// RedirectURIs are the redirect_uri values /authorize accepts, compared
	// exactly. A request for any other is answered 400 and never redirected.
	RedirectURIs []string
	// AdvertisedAlgs is what the discovery document lists as the signing
	// algorithms of ID tokens (default: RS256 only, like Google).
	AdvertisedAlgs []string
}

// Claims are the ID-token claims K.I.W.I. reads. An empty HD leaves the hd
// claim out; an empty Picture leaves out picture.
type Claims struct {
	Sub           string
	Email         string
	Name          string
	Picture       string
	HD            string
	EmailVerified bool
}

// MintOptions bend a minted token away from a valid one; the zero value is valid.
type MintOptions struct {
	// Audience replaces the client id as aud.
	Audience string
	// Issuer replaces the provider's issuer as iss.
	Issuer string
	// ExpiresIn is the token's lifetime (default one hour); a negative value
	// mints an already expired token.
	ExpiresIn time.Duration
	// Nonce is echoed as the nonce claim.
	Nonce string
	// WrongKey signs with a key the provider's JWKS does not publish.
	WrongKey bool
	// PSS signs with RSASSA-PSS (alg PS256) instead of RS256, with the published key.
	PSS bool
}

// Provider is the OpenID Connect provider; it is an http.Handler and safe for
// concurrent use.
type Provider struct {
	cfg    Config
	base   string // path the endpoints live under, without trailing slash
	public string // scheme://host + base: what the browser reaches
	mux    *http.ServeMux
	signer *signingKey
	other  *signingKey

	mu      sync.Mutex
	codes   map[string]authCode
	refresh map[string]string   // refresh token -> email
	latest  map[string]string   // email -> the refresh token last issued
	granted map[string][]string // email -> scopes granted to the client
}

type authCode struct {
	user           User
	hd             string
	scopes         []string
	challenge      string
	redirectURI    string
	nonce          string
	offline        bool
	includeGranted bool
	forceConsent   bool
	expires        time.Time
}

type signingKey struct {
	kid string
	key *rsa.PrivateKey
}

// keys are generated once per process: a 2048-bit RSA key takes long under the
// race detector and every test would pay for it.
var keys = sync.OnceValue(func() [2]*signingKey {
	gen := func(kid string) *signingKey {
		k, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			panic(fmt.Sprintf("fakeoidc: generate signing key: %v", err))
		}
		return &signingKey{kid: kid, key: k}
	}
	return [2]*signingKey{gen("fakeoidc-1"), gen("fakeoidc-unpublished")}
})

// NewProvider returns a Provider for cfg.
func NewProvider(cfg Config) (*Provider, error) {
	u, err := url.Parse(cfg.Issuer)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("fakeoidc: Issuer must be an absolute http(s) URL without query or fragment")
	}
	if cfg.ClientID == "" || cfg.ClientSecret == "" {
		return nil, errors.New("fakeoidc: ClientID and ClientSecret are required")
	}
	if len(cfg.Users) > 0 && cfg.HostedDomain == "" {
		return nil, errors.New("fakeoidc: HostedDomain is required when Users are set")
	}
	base := strings.TrimSuffix(u.Path, "/")
	if strings.ContainsAny(base, "{} ") {
		return nil, errors.New("fakeoidc: Issuer path must not contain braces or spaces")
	}
	if len(cfg.RedirectURIs) == 0 {
		return nil, errors.New("fakeoidc: RedirectURIs is required: /authorize redirects only to registered addresses")
	}
	for _, raw := range cfg.RedirectURIs {
		if r, err := url.Parse(raw); err != nil || (r.Scheme != "http" && r.Scheme != "https") || r.Host == "" || r.Fragment != "" {
			return nil, errors.New("fakeoidc: every RedirectURIs entry must be an absolute http(s) URL without fragment")
		}
	}
	for _, usr := range cfg.Users {
		if usr.Email == "" {
			return nil, errors.New("fakeoidc: a user has no Email")
		}
	}

	k := keys()
	p := &Provider{
		cfg:     cfg,
		base:    base,
		public:  u.Scheme + "://" + u.Host + base,
		mux:     http.NewServeMux(),
		signer:  k[0],
		other:   k[1],
		codes:   map[string]authCode{},
		refresh: map[string]string{},
		latest:  map[string]string{},
		granted: map[string][]string{},
	}
	p.mux.HandleFunc("GET "+base+"/.well-known/openid-configuration", p.discovery)
	p.mux.HandleFunc("GET "+base+"/jwks", p.jwks)
	p.mux.HandleFunc("GET "+base+"/authorize", p.authorize)
	p.mux.HandleFunc("POST "+base+"/token", p.token)
	return p, nil
}

// ServeHTTP implements http.Handler.
func (p *Provider) ServeHTTP(w http.ResponseWriter, r *http.Request) { p.mux.ServeHTTP(w, r) }

// Issuer returns the issuer URL tokens carry.
func (p *Provider) Issuer() string { return p.cfg.Issuer }

// ClientID returns the only client id the provider accepts.
func (p *Provider) ClientID() string { return p.cfg.ClientID }

// ClientSecret returns the client secret the token endpoint requires.
func (p *Provider) ClientSecret() string { return p.cfg.ClientSecret }

// RefreshToken returns the refresh token last issued to email, or "".
func (p *Provider) RefreshToken(email string) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.latest[email]
}

// Revoke withdraws everything email granted: its refresh tokens stop working,
// its scopes are forgotten, and the next offline sign-in is issued a new
// refresh token, as after "Remove access" in a Google account.
func (p *Provider) Revoke(email string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for rt, e := range p.refresh {
		if e == email {
			delete(p.refresh, rt)
		}
	}
	delete(p.latest, email)
	delete(p.granted, email)
}

// Mint signs an ID token for c.
func (p *Provider) Mint(c Claims, o MintOptions) string {
	now := time.Now()
	claims := map[string]any{
		"iss":            cmp.Or(o.Issuer, p.cfg.Issuer),
		"aud":            cmp.Or(o.Audience, p.cfg.ClientID),
		"sub":            c.Sub,
		"iat":            now.Unix(),
		"exp":            now.Add(cmp.Or(o.ExpiresIn, time.Hour)).Unix(),
		"email":          c.Email,
		"email_verified": c.EmailVerified,
		"name":           c.Name,
	}
	if c.Picture != "" {
		claims["picture"] = c.Picture
	}
	if c.HD != "" {
		claims["hd"] = c.HD
	}
	if o.Nonce != "" {
		claims["nonce"] = o.Nonce
	}
	k := p.signer
	if o.WrongKey {
		k = p.other
	}
	return sign(k, claims, o.PSS)
}

// advertisedAlgs is the id_token_signing_alg_values_supported of the discovery
// document: RS256 only, like Google, unless Config says otherwise.
func (p *Provider) advertisedAlgs() []string {
	if len(p.cfg.AdvertisedAlgs) == 0 {
		return []string{"RS256"}
	}
	return p.cfg.AdvertisedAlgs
}

// sign signs claims as RS256, or as PS256 (RSASSA-PSS, same key) when pss is set.
func sign(k *signingKey, claims map[string]any, pss bool) string {
	enc := func(v any) string {
		b, err := json.Marshal(v)
		if err != nil {
			panic(fmt.Sprintf("fakeoidc: encode token: %v", err)) // only strings, numbers and bools go in
		}
		return base64.RawURLEncoding.EncodeToString(b)
	}
	alg := "RS256"
	if pss {
		alg = "PS256"
	}
	input := enc(map[string]string{"alg": alg, "typ": "JWT", "kid": k.kid}) + "." + enc(claims)
	sum := sha256.Sum256([]byte(input))
	var sig []byte
	var err error
	if pss {
		sig, err = rsa.SignPSS(rand.Reader, k.key, crypto.SHA256, sum[:], &rsa.PSSOptions{SaltLength: rsa.PSSSaltLengthEqualsHash})
	} else {
		sig, err = rsa.SignPKCS1v15(rand.Reader, k.key, crypto.SHA256, sum[:])
	}
	if err != nil {
		panic(fmt.Sprintf("fakeoidc: sign token: %v", err))
	}
	return input + "." + base64.RawURLEncoding.EncodeToString(sig)
}

// discovery advertises token_endpoint and jwks_uri on the host the request came
// through, so a client that discovers over an internal URL also exchanges codes
// and fetches keys there, while authorization_endpoint always uses the issuer's
// host, which is the one the browser reaches.
func (p *Provider) discovery(w http.ResponseWriter, r *http.Request) {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	via := scheme + "://" + r.Host + p.base
	writeJSON(w, http.StatusOK, map[string]any{
		"issuer":                                p.cfg.Issuer,
		"authorization_endpoint":                p.public + "/authorize",
		"token_endpoint":                        via + "/token",
		"jwks_uri":                              via + "/jwks",
		"response_types_supported":              []string{"code"},
		"subject_types_supported":               []string{"public"},
		"id_token_signing_alg_values_supported": p.advertisedAlgs(),
		"code_challenge_methods_supported":      []string{"S256"},
		"scopes_supported":                      []string{"openid", "email", "profile"},
		"claims_supported":                      []string{"iss", "aud", "sub", "iat", "exp", "email", "email_verified", "name", "picture", "hd"},
	})
}

func (p *Provider) jwks(w http.ResponseWriter, _ *http.Request) {
	pub := p.signer.key.PublicKey
	writeJSON(w, http.StatusOK, map[string]any{"keys": []map[string]string{{
		"kty": "RSA",
		"use": "sig",
		"alg": "RS256",
		"kid": p.signer.kid,
		"n":   base64.RawURLEncoding.EncodeToString(pub.N.Bytes()),
		"e":   base64.RawURLEncoding.EncodeToString(big.NewInt(int64(pub.E)).Bytes()),
	}}})
}

func (p *Provider) lookup(email string) (u User, hd string, ok bool) {
	for _, usr := range p.cfg.Users {
		if strings.EqualFold(usr.Email, email) {
			return usr, p.cfg.HostedDomain, true
		}
	}
	if o := p.cfg.Outsider; o != nil && strings.EqualFold(o.Email, email) {
		return *o, "", true
	}
	return User{}, "", false
}

// authorize approves the account named by login_hint at once and sends the
// browser back with code and state. Without a login_hint it lists the accounts.
func (p *Provider) authorize(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	// Without a trusted client and redirect target there is nobody to tell by redirect.
	if q.Get("client_id") != p.cfg.ClientID {
		http.Error(w, "unknown client_id", http.StatusBadRequest)
		return
	}
	// The allow-list comes first and matches exactly: /authorize must never
	// redirect to an address nobody registered (it would be an open redirector).
	if !slices.Contains(p.cfg.RedirectURIs, q.Get("redirect_uri")) {
		http.Error(w, "redirect_uri is not registered for this client", http.StatusBadRequest)
		return
	}
	redirect, err := url.Parse(q.Get("redirect_uri"))
	if err != nil || !redirect.IsAbs() || redirect.Host == "" {
		http.Error(w, "redirect_uri must be an absolute URL", http.StatusBadRequest)
		return
	}
	back := func(params url.Values) {
		out := redirect.Query()
		for k, v := range params {
			out[k] = v
		}
		if s := q.Get("state"); s != "" {
			out.Set("state", s)
		}
		u := *redirect
		u.RawQuery = out.Encode()
		http.Redirect(w, r, u.String(), http.StatusFound)
	}
	fail := func(code string) { back(url.Values{"error": {code}}) }

	scopes := normalizeScopes(strings.Fields(q.Get("scope")))
	switch {
	case q.Get("response_type") != "code":
		fail("unsupported_response_type")
		return
	case q.Get("code_challenge") == "" || q.Get("code_challenge_method") != "S256":
		fail("invalid_request") // PKCE S256 is mandatory here
		return
	case !slices.Contains(scopes, "openid"):
		fail("invalid_scope")
		return
	}

	hint := q.Get("login_hint")
	if hint == "" {
		p.chooser(w, r)
		return
	}
	user, hd, ok := p.lookup(hint)
	if !ok {
		fail("access_denied")
		return
	}

	code := randomToken()
	p.mu.Lock()
	p.codes[code] = authCode{
		user:           user,
		hd:             hd,
		scopes:         scopes,
		challenge:      q.Get("code_challenge"),
		redirectURI:    q.Get("redirect_uri"),
		nonce:          q.Get("nonce"),
		offline:        q.Get("access_type") == "offline",
		includeGranted: q.Get("include_granted_scopes") == "true",
		forceConsent:   slices.Contains(strings.Fields(q.Get("prompt")), "consent"),
		expires:        time.Now().Add(codeTTL),
	}
	p.mu.Unlock()

	params := url.Values{"code": {code}, "scope": {strings.Join(scopes, " ")}, "authuser": {"0"}}
	if hd != "" {
		params.Set("hd", hd)
	}
	back(params)
}

var chooserPage = template.Must(template.New("chooser").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>Mock sign-in</title></head>
<body><main><h1>Mock sign-in</h1><p>Pick an account. This provider is a stand-in for Google.</p>
<ul>{{range .}}<li><a href="{{.Href}}">{{.Name}} &lt;{{.Email}}&gt;{{if not .Workspace}} (no hosted domain){{end}}</a></li>{{end}}</ul>
</main></body></html>`))

func (p *Provider) chooser(w http.ResponseWriter, r *http.Request) {
	type account struct {
		Name, Email string
		Workspace   bool
		Href        template.URL
	}
	var accounts []account
	add := func(u User, workspace bool) {
		q := r.URL.Query()
		q.Set("login_hint", u.Email)
		accounts = append(accounts, account{
			Name:      u.Name,
			Email:     u.Email,
			Workspace: workspace,
			Href:      template.URL(r.URL.Path + "?" + q.Encode()),
		})
	}
	for _, u := range p.cfg.Users {
		add(u, true)
	}
	if o := p.cfg.Outsider; o != nil {
		add(*o, false)
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if err := chooserPage.Execute(w, accounts); err != nil {
		http.Error(w, "render chooser", http.StatusInternalServerError)
	}
}

func (p *Provider) token(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		oauthError(w, http.StatusBadRequest, "invalid_request", "unreadable form")
		return
	}
	id, secret, basic := r.BasicAuth()
	if basic {
		// RFC 6749 §2.3.1: the basic credentials are form-urlencoded first.
		id, _ = url.QueryUnescape(id)
		secret, _ = url.QueryUnescape(secret)
	} else {
		id, secret = r.PostForm.Get("client_id"), r.PostForm.Get("client_secret")
	}
	if id != p.cfg.ClientID || subtle.ConstantTimeCompare([]byte(secret), []byte(p.cfg.ClientSecret)) != 1 {
		oauthError(w, http.StatusUnauthorized, "invalid_client", "client authentication failed")
		return
	}

	switch r.PostForm.Get("grant_type") {
	case "authorization_code":
		p.exchangeCode(w, r)
	case "refresh_token":
		p.refreshGrant(w, r)
	default:
		oauthError(w, http.StatusBadRequest, "unsupported_grant_type", "only authorization_code and refresh_token")
	}
}

func (p *Provider) exchangeCode(w http.ResponseWriter, r *http.Request) {
	form := r.PostForm
	code := form.Get("code")

	p.mu.Lock()
	ac, ok := p.codes[code]
	delete(p.codes, code) // single use, whatever follows
	p.mu.Unlock()

	verifier := form.Get("code_verifier")
	sum := sha256.Sum256([]byte(verifier))
	switch {
	case !ok || time.Now().After(ac.expires):
		oauthError(w, http.StatusBadRequest, "invalid_grant", "unknown or expired code")
		return
	case form.Get("redirect_uri") != ac.redirectURI:
		oauthError(w, http.StatusBadRequest, "invalid_grant", "redirect_uri does not match the authorization request")
		return
	case verifier == "" || subtle.ConstantTimeCompare([]byte(base64.RawURLEncoding.EncodeToString(sum[:])), []byte(ac.challenge)) != 1:
		oauthError(w, http.StatusBadRequest, "invalid_grant", "PKCE verification failed")
		return
	}

	email := ac.user.Email
	p.mu.Lock()
	grant := union(p.granted[email], ac.scopes)
	p.granted[email] = grant
	reported := ac.scopes
	if ac.includeGranted {
		reported = grant
	}
	var refreshToken string
	if ac.offline && (ac.forceConsent || p.latest[email] == "") {
		refreshToken = randomToken()
		p.refresh[refreshToken] = email
		p.latest[email] = refreshToken
	}
	p.mu.Unlock()

	resp := map[string]any{
		"access_token": "fakeoidc-access-" + randomToken(),
		"token_type":   "Bearer",
		"expires_in":   int(accessTokenTTL.Seconds()),
		"scope":        strings.Join(reported, " "),
		"id_token": p.Mint(Claims{
			Sub:           ac.user.sub(),
			Email:         ac.user.Email,
			Name:          ac.user.Name,
			HD:            ac.hd,
			EmailVerified: true,
		}, MintOptions{Nonce: ac.nonce}),
	}
	if refreshToken != "" {
		resp["refresh_token"] = refreshToken
	}
	writeJSON(w, http.StatusOK, resp)
}

func (p *Provider) refreshGrant(w http.ResponseWriter, r *http.Request) {
	p.mu.Lock()
	email, ok := p.refresh[r.PostForm.Get("refresh_token")]
	scopes := slices.Clone(p.granted[email])
	p.mu.Unlock()
	if !ok {
		oauthError(w, http.StatusBadRequest, "invalid_grant", "Token has been expired or revoked.")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"access_token": "fakeoidc-access-" + randomToken(),
		"token_type":   "Bearer",
		"expires_in":   int(accessTokenTTL.Seconds()),
		"scope":        strings.Join(scopes, " "),
	})
}

// normalizeScopes maps the short names a client sends to the URLs Google reports.
func normalizeScopes(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		switch s {
		case "email":
			s = scopeEmail
		case "profile":
			s = scopeProfile
		}
		if !slices.Contains(out, s) {
			out = append(out, s)
		}
	}
	return out
}

func union(a, b []string) []string {
	out := slices.Clone(a)
	for _, s := range b {
		if !slices.Contains(out, s) {
			out = append(out, s)
		}
	}
	return out
}

func randomToken() string { return rand.Text() }

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func oauthError(w http.ResponseWriter, status int, code, description string) {
	writeJSON(w, status, map[string]string{"error": code, "error_description": description})
}
