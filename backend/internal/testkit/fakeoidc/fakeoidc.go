package fakeoidc

import (
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// The accounts New serves: two Workspace users and an outsider without hd.
const (
	DefaultClientID     = "kiwi-test-client"
	DefaultClientSecret = "kiwi-test-secret"
	DefaultDomain       = "acme.test"
	// DefaultRedirectURI is the callback of a local K.I.W.I. on KIWI_PUBLIC_URL
	// http://localhost:8080, the only redirect_uri New registers by default.
	DefaultRedirectURI = "http://localhost:8080/api/auth/callback"

	AliceEmail = "alice@" + DefaultDomain
	AliceName  = "Alice Adams"
	BobEmail   = "bob@" + DefaultDomain
	BobName    = "Bob Brown"
	CarolEmail = "carol@personal.test"
	CarolName  = "Carol Personal"
)

// Server is a Provider listening on a loopback port.
type Server struct {
	*Provider
	// URL is where the server listens, including the path of the issuer. It is
	// the issuer itself unless WithIssuer says the browser reaches the provider
	// somewhere else.
	URL string
}

// Option changes the Config New starts from.
type Option func(*Config)

// WithIssuer sets the issuer the provider claims, for tests of a client that
// discovers through an internal URL (Server.URL) while the issuer is another one.
func WithIssuer(issuer string) Option { return func(c *Config) { c.Issuer = issuer } }

// WithAdvertisedAlgs makes the discovery document list algs as the signing
// algorithms of ID tokens, for tests of a verifier that must not trust that list.
func WithAdvertisedAlgs(algs ...string) Option { return func(c *Config) { c.AdvertisedAlgs = algs } }

// WithUsers replaces the Workspace users.
func WithUsers(users ...User) Option { return func(c *Config) { c.Users = users } }

// WithRedirectURIs replaces the redirect_uri allow-list, which starts as
// DefaultRedirectURI.
func WithRedirectURIs(uris ...string) Option { return func(c *Config) { c.RedirectURIs = uris } }

// New starts a Provider with DefaultClientID and DefaultClientSecret, hosted
// domain DefaultDomain, users Alice and Bob, the outsider Carol and
// DefaultRedirectURI as the only registered redirect_uri, and stops it when the
// test ends.
func New(t testing.TB, opts ...Option) *Server {
	t.Helper()

	// The listener exists before the server starts, so the default issuer can be
	// its address.
	srv := httptest.NewUnstartedServer(nil)
	t.Cleanup(srv.Close)
	root := "http://" + srv.Listener.Addr().String()

	cfg := Config{
		Issuer:       root,
		ClientID:     DefaultClientID,
		ClientSecret: DefaultClientSecret,
		HostedDomain: DefaultDomain,
		Users:        []User{{Email: AliceEmail, Name: AliceName}, {Email: BobEmail, Name: BobName}},
		Outsider:     &User{Email: CarolEmail, Name: CarolName},
		RedirectURIs: []string{DefaultRedirectURI},
	}
	for _, opt := range opts {
		opt(&cfg)
	}
	p, err := NewProvider(cfg)
	if err != nil {
		t.Fatalf("fakeoidc: %v", err)
	}
	srv.Config.Handler = p
	srv.Start()

	// The provider serves the path of its issuer, whatever host the issuer names.
	u, err := url.Parse(cfg.Issuer)
	if err != nil {
		t.Fatalf("fakeoidc: %v", err)
	}
	return &Server{Provider: p, URL: root + strings.TrimSuffix(u.Path, "/")}
}
