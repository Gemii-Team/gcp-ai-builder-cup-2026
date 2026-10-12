// Package fakeoidctest starts a fakeoidc.Provider for a test. It is a separate
// package so that fakeoidc, which the standalone mock sign-in server
// (backend/tools/mockoidc) links, does not pull in "testing" or httptest.
package fakeoidctest

import (
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"kiwi/internal/testkit/fakeoidc"
)

// Server is a Provider listening on a loopback port.
type Server struct {
	*fakeoidc.Provider
	// URL is where the server listens, including the path of the issuer. It is
	// the issuer itself unless WithIssuer says the browser reaches the provider
	// somewhere else.
	URL string
}

// Option changes the Config New starts from.
type Option func(*fakeoidc.Config)

// WithIssuer sets the issuer the provider claims, for tests of a client that
// discovers through an internal URL (Server.URL) while the issuer is another one.
func WithIssuer(issuer string) Option { return func(c *fakeoidc.Config) { c.Issuer = issuer } }

// WithAdvertisedAlgs makes the discovery document list algs as the signing
// algorithms of ID tokens, for tests of a verifier that must not trust that list.
func WithAdvertisedAlgs(algs ...string) Option {
	return func(c *fakeoidc.Config) { c.AdvertisedAlgs = algs }
}

// WithHostedDomain replaces the hd claim of the Workspace users.
func WithHostedDomain(hd string) Option { return func(c *fakeoidc.Config) { c.HostedDomain = hd } }

// WithUsers replaces the Workspace users.
func WithUsers(users ...fakeoidc.User) Option { return func(c *fakeoidc.Config) { c.Users = users } }

// WithRedirectURIs replaces the redirect_uri allow-list, which starts as
// fakeoidc.DefaultRedirectURI.
func WithRedirectURIs(uris ...string) Option {
	return func(c *fakeoidc.Config) { c.RedirectURIs = uris }
}

// New starts a Provider with fakeoidc.DefaultClientID and
// fakeoidc.DefaultClientSecret, hosted domain fakeoidc.DefaultDomain, users Alice
// and Bob (their tokens carry hd), Carol (hers does not: she is a personal
// account) and fakeoidc.DefaultRedirectURI as the only registered redirect_uri,
// and stops it when the test ends.
func New(t testing.TB, opts ...Option) *Server {
	t.Helper()

	// The listener exists before the server starts, so the default issuer can be
	// its address.
	srv := httptest.NewUnstartedServer(nil)
	t.Cleanup(srv.Close)
	root := "http://" + srv.Listener.Addr().String()

	cfg := fakeoidc.Config{
		Issuer:       root,
		ClientID:     fakeoidc.DefaultClientID,
		ClientSecret: fakeoidc.DefaultClientSecret,
		HostedDomain: fakeoidc.DefaultDomain,
		Users: []fakeoidc.User{
			{Email: fakeoidc.AliceEmail, Name: fakeoidc.AliceName},
			{Email: fakeoidc.BobEmail, Name: fakeoidc.BobName},
			{Email: fakeoidc.CarolEmail, Name: fakeoidc.CarolName, NoHD: true},
		},
		RedirectURIs: []string{fakeoidc.DefaultRedirectURI},
	}
	for _, opt := range opts {
		opt(&cfg)
	}
	p, err := fakeoidc.NewProvider(cfg)
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
