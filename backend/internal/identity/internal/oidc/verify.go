// Package oidc verifies the ID tokens the sign-in callback receives. The Workspace
// boundary rests on it: signature, audience, issuer, expiry, a verified email and
// the hd (hosted domain) claim are all read from the signed token, never from a URL
// parameter.
package oidc

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	gooidc "github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

// discoveryTimeout bounds every request to the provider: discovery and key fetches.
const discoveryTimeout = 15 * time.Second

// Claims are the verified claims of an ID token.
type Claims struct {
	Sub           string
	Email         string
	Name          string
	Picture       string
	HD            string
	EmailVerified bool
	// Nonce is the nonce claim of the token, empty when it has none.
	Nonce string
}

// ErrNoHostedDomain is returned, together with the otherwise verified claims,
// for a token without an hd claim: an account that does not belong to a Workspace
// domain, such as a personal Gmail address.
var ErrNoHostedDomain = errors.New("oidc: the ID token has no hd claim")

// Verifier checks ID tokens of one issuer for one client.
type Verifier struct {
	provider *gooidc.Provider
	verifier *gooidc.IDTokenVerifier
}

// NewVerifier discovers the provider of issuer and returns a Verifier for tokens
// issued to clientID. When internalURL is set, discovery happens there instead (and
// so do the key fetches its document points to) while iss is still checked against
// issuer: the address the backend reaches the provider at is not always the one
// its tokens name, as with a mock reached inside a cluster. go-oidc's
// InsecureIssuerURLContext does this: it skips the comparison of the discovery
// document's issuer and makes the verifier require iss == issuer as given here.
func NewVerifier(ctx context.Context, issuer, internalURL, clientID string) (*Verifier, error) {
	if clientID == "" {
		return nil, errors.New("oidc: the OAuth client id is empty")
	}
	discoverAt := issuer
	if internalURL != "" {
		ctx = gooidc.InsecureIssuerURLContext(ctx, issuer)
		discoverAt = internalURL
	}
	ctx = gooidc.ClientContext(ctx, &http.Client{Timeout: discoveryTimeout})
	p, err := gooidc.NewProvider(ctx, discoverAt)
	if err != nil {
		return nil, fmt.Errorf("oidc: discover provider: %w", err)
	}
	// Google signs ID tokens with RS256. The algorithm is pinned here rather than
	// taken from the discovery document, so neither "none", nor an HMAC keyed with
	// the public key, nor whatever else a document lists can ever verify a token.
	cfg := &gooidc.Config{ClientID: clientID, SupportedSigningAlgs: []string{gooidc.RS256}}
	return &Verifier{provider: p, verifier: p.Verifier(cfg)}, nil
}

// Endpoint returns the provider's OAuth 2.0 authorization and token endpoints as
// its discovery document advertises them.
func (v *Verifier) Endpoint() oauth2.Endpoint { return v.provider.Endpoint() }

// Verify checks raw's signature, audience, issuer and expiry, that the email is
// verified and that the token names a hosted domain, and returns its claims.
//
// A token that passes every check except for the missing hd claim returns its
// verified claims together with ErrNoHostedDomain, so the caller can tell the user
// which account was turned away. Every other failure returns zero Claims.
func (v *Verifier) Verify(ctx context.Context, raw string) (Claims, error) {
	tok, err := v.verifier.Verify(ctx, raw)
	if err != nil {
		return Claims{}, fmt.Errorf("oidc: verify ID token: %w", err)
	}
	var body struct {
		Email         string `json:"email"`
		EmailVerified bool   `json:"email_verified"`
		Name          string `json:"name"`
		Picture       string `json:"picture"`
		HD            string `json:"hd"`
	}
	if err := tok.Claims(&body); err != nil {
		return Claims{}, fmt.Errorf("oidc: read ID token claims: %w", err)
	}
	switch {
	case tok.Subject == "" || body.Email == "":
		return Claims{}, errors.New("oidc: the ID token carries no subject or email")
	case !body.EmailVerified:
		return Claims{}, errors.New("oidc: the email of the ID token is not verified")
	}

	c := Claims{
		Sub:           tok.Subject,
		Email:         body.Email,
		Name:          body.Name,
		Picture:       body.Picture,
		HD:            body.HD,
		EmailVerified: true,
		Nonce:         tok.Nonce,
	}
	if c.HD == "" {
		return c, ErrNoHostedDomain
	}
	return c, nil
}
