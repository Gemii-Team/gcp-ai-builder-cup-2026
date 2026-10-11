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

// WithUsers replaces the Workspace users.
func WithUsers(users ...User) Option { return func(c *Config) { c.Users = users } }

// New starts a Provider with DefaultClientID and DefaultClientSecret, hosted
// domain DefaultDomain, users Alice and Bob and the outsider Carol, and stops
// it when the test ends.
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
