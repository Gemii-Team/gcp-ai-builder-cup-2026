package oidc

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	"kiwi/internal/testkit/fakeoidc"
	"kiwi/internal/testkit/fakeoidc/fakeoidctest"
)

// valid is what a Workspace user's token carries.
var valid = fakeoidc.Claims{
	Sub:           "110169484474386276334",
	Email:         "alice@acme.test",
	Name:          "Alice Adams",
	Picture:       "https://photos.example/alice.png",
	HD:            "acme.test",
	EmailVerified: true,
}

func verifier(t *testing.T, f *fakeoidctest.Server) *Verifier {
	t.Helper()
	v, err := NewVerifier(t.Context(), f.Issuer(), "", f.ClientID())
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	return v
}

// forgedClaims are the claims of a valid Workspace token, for tokens signed by
// someone other than the provider.
func forgedClaims(f *fakeoidctest.Server) map[string]any {
	return map[string]any{
		"iss": f.Issuer(), "aud": f.ClientID(), "sub": valid.Sub, "email": valid.Email,
		"email_verified": true, "name": valid.Name, "hd": valid.HD,
		"iat": time.Now().Unix(), "exp": time.Now().Add(time.Hour).Unix(),
	}
}

// forge assembles a compact JWT whose signature is whatever sign returns.
func forge(t *testing.T, header, claims map[string]any, sign func(signingInput string) []byte) string {
	t.Helper()
	enc := func(v any) string {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		return base64.RawURLEncoding.EncodeToString(b)
	}
	input := enc(header) + "." + enc(claims)
	return input + "." + base64.RawURLEncoding.EncodeToString(sign(input))
}

// forgeHS256 is the algorithm-confusion attack: a token whose HMAC key is the
// public modulus the provider publishes at its JWKS endpoint.
func forgeHS256(t *testing.T, f *fakeoidctest.Server) string {
	t.Helper()
	resp, err := http.Get(f.URL + "/jwks")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var set struct {
		Keys []struct {
			Kid string `json:"kid"`
			N   string `json:"n"`
		} `json:"keys"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&set); err != nil || len(set.Keys) == 0 {
		t.Fatalf("jwks: %v", err)
	}
	modulus, err := base64.RawURLEncoding.DecodeString(set.Keys[0].N)
	if err != nil {
		t.Fatal(err)
	}
	return forge(t, map[string]any{"alg": "HS256", "typ": "JWT", "kid": set.Keys[0].Kid}, forgedClaims(f), func(input string) []byte {
		mac := hmac.New(sha256.New, modulus)
		mac.Write([]byte(input))
		return mac.Sum(nil)
	})
}

func TestVerify_Valid(t *testing.T) {
	f := fakeoidctest.New(t)
	v := verifier(t, f)

	got, err := v.Verify(t.Context(), f.Mint(valid, fakeoidc.MintOptions{}))
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	want := Claims{
		Sub:           valid.Sub,
		Email:         valid.Email,
		Name:          valid.Name,
		Picture:       valid.Picture,
		HD:            valid.HD,
		EmailVerified: true,
	}
	if got != want {
		t.Fatalf("claims = %+v, want %+v", got, want)
	}
}

func TestVerify_RejectsWrongAudience(t *testing.T) {
	f := fakeoidctest.New(t)
	v := verifier(t, f)

	got, err := v.Verify(t.Context(), f.Mint(valid, fakeoidc.MintOptions{Audience: "some-other-client"}))
	if err == nil {
		t.Fatal("a token for another client was accepted")
	}
	if got != (Claims{}) {
		t.Fatalf("claims = %+v with an error, want zero", got)
	}
}

func TestVerify_RejectsWrongIssuer(t *testing.T) {
	f := fakeoidctest.New(t)
	v := verifier(t, f)

	if _, err := v.Verify(t.Context(), f.Mint(valid, fakeoidc.MintOptions{Issuer: "https://accounts.evil.example"})); err == nil {
		t.Fatal("a token from another issuer was accepted")
	}
}

func TestVerify_RejectsExpired(t *testing.T) {
	f := fakeoidctest.New(t)
	v := verifier(t, f)

	if _, err := v.Verify(t.Context(), f.Mint(valid, fakeoidc.MintOptions{ExpiresIn: -time.Minute})); err == nil {
		t.Fatal("an expired token was accepted")
	}
}

func TestVerify_RejectsBadSignature(t *testing.T) {
	f := fakeoidctest.New(t)
	v := verifier(t, f)

	if _, err := v.Verify(t.Context(), f.Mint(valid, fakeoidc.MintOptions{WrongKey: true})); err == nil {
		t.Fatal("a token signed with an unpublished key was accepted")
	}
}

func TestVerify_HDWithUnverifiedEmail_RejectedWithZeroClaims(t *testing.T) {
	f := fakeoidctest.New(t)
	v := verifier(t, f)

	c := valid
	c.EmailVerified = false
	got, err := v.Verify(t.Context(), f.Mint(c, fakeoidc.MintOptions{}))
	if err == nil {
		t.Fatal("a token with email_verified=false was accepted")
	}
	if errors.Is(err, ErrNoHostedDomain) || got != (Claims{}) {
		t.Fatalf("got %+v, %v; want zero claims and a plain error", got, err)
	}
}

func TestVerify_MissingHD_ReturnsErrNoHostedDomainWithVerifiedClaims(t *testing.T) {
	f := fakeoidctest.New(t)
	v := verifier(t, f)

	c := valid
	c.HD = ""
	got, err := v.Verify(t.Context(), f.Mint(c, fakeoidc.MintOptions{}))
	if !errors.Is(err, ErrNoHostedDomain) {
		t.Fatalf("err = %v, want ErrNoHostedDomain", err)
	}
	if got.Email != valid.Email || got.Sub != valid.Sub || got.HD != "" {
		t.Fatalf("claims = %+v, want the verified identity without hd", got)
	}
}

// An account without hd is judged by the invite rule, which needs to tell an
// unverified address from a verified one: the claims come back whichever it is.
func TestVerify_NoHD_UnverifiedEmail_ReturnsClaimsWithErrNoHostedDomain(t *testing.T) {
	f := fakeoidctest.New(t)
	v := verifier(t, f)

	c := valid
	c.HD, c.EmailVerified = "", false
	got, err := v.Verify(t.Context(), f.Mint(c, fakeoidc.MintOptions{}))
	if !errors.Is(err, ErrNoHostedDomain) {
		t.Fatalf("err = %v, want ErrNoHostedDomain", err)
	}
	want := Claims{Sub: valid.Sub, Email: valid.Email, Name: valid.Name, Picture: valid.Picture, EmailVerified: false}
	if got != want {
		t.Fatalf("claims = %+v, want %+v: the verified claims with EmailVerified false", got, want)
	}

	c.EmailVerified = true
	if got, err := v.Verify(t.Context(), f.Mint(c, fakeoidc.MintOptions{})); !errors.Is(err, ErrNoHostedDomain) || !got.EmailVerified {
		t.Fatalf("a verified address without hd: got %+v, %v; want EmailVerified true with ErrNoHostedDomain", got, err)
	}
}

func TestVerify_OtherFailuresReturnNoClaims(t *testing.T) {
	f := fakeoidctest.New(t)
	v := verifier(t, f)

	noSub := valid
	noSub.Sub = ""
	noEmail := valid
	noEmail.Email = ""

	tests := map[string]string{
		"wrong audience":                      f.Mint(valid, fakeoidc.MintOptions{Audience: "other"}),
		"wrong issuer":                        f.Mint(valid, fakeoidc.MintOptions{Issuer: "https://evil.example"}),
		"expired":                             f.Mint(valid, fakeoidc.MintOptions{ExpiresIn: -time.Hour}),
		"bad signature":                       f.Mint(valid, fakeoidc.MintOptions{WrongKey: true}),
		"bad signature and no hd":             f.Mint(fakeoidc.Claims{Sub: "1", Email: "x@y.test", EmailVerified: true}, fakeoidc.MintOptions{WrongKey: true}),
		"expired and no hd":                   f.Mint(fakeoidc.Claims{Sub: "1", Email: "x@y.test", EmailVerified: true}, fakeoidc.MintOptions{ExpiresIn: -time.Hour}),
		"no subject":                          f.Mint(noSub, fakeoidc.MintOptions{}),
		"no email":                            f.Mint(noEmail, fakeoidc.MintOptions{}),
		"not a token":                         "definitely-not-a-jwt",
		"empty":                               "",
		"header and payload but no signature": "e30.e30.",
		"alg none":                            forge(t, map[string]any{"alg": "none", "typ": "JWT"}, forgedClaims(f), func(string) []byte { return nil }),
		"HS256 keyed with the public modulus": forgeHS256(t, f),
	}
	for name, raw := range tests {
		t.Run(name, func(t *testing.T) {
			got, err := v.Verify(t.Context(), raw)
			if err == nil {
				t.Fatal("the token was accepted")
			}
			if errors.Is(err, ErrNoHostedDomain) {
				t.Fatalf("err = %v: only a token whose signature, audience, issuer, expiry and subject verified may be named by ErrNoHostedDomain", err)
			}
			if got != (Claims{}) {
				t.Fatalf("claims = %+v with an error, want zero", got)
			}
		})
	}
}

func TestVerify_InternalURLDiscoveryKeepsIssuerCheck(t *testing.T) {
	// The provider is reached at f.URL but claims another issuer, the way Google's
	// issuer is not the address of an in-cluster mock.
	const issuer = "https://accounts.example.test"
	f := fakeoidctest.New(t, fakeoidctest.WithIssuer(issuer))

	v, err := NewVerifier(t.Context(), issuer, f.URL, f.ClientID())
	if err != nil {
		t.Fatalf("NewVerifier through the internal URL: %v", err)
	}
	if _, err := v.Verify(t.Context(), f.Mint(valid, fakeoidc.MintOptions{})); err != nil {
		t.Fatalf("token from the configured issuer: %v", err)
	}
	// The internal URL is where keys come from, never what iss must equal.
	if _, err := v.Verify(t.Context(), f.Mint(valid, fakeoidc.MintOptions{Issuer: f.URL})); err == nil {
		t.Fatal("a token whose iss is the internal discovery URL was accepted")
	}
	if _, err := v.Verify(t.Context(), f.Mint(valid, fakeoidc.MintOptions{Issuer: "https://accounts.evil.example"})); err == nil {
		t.Fatal("a token from another issuer was accepted")
	}

	// Without the internal URL the same configuration cannot even discover:
	// the issuer is not an address anything answers at.
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	if _, err := NewVerifier(ctx, "http://127.0.0.1:1", "", f.ClientID()); err == nil {
		t.Fatal("NewVerifier succeeded against an address nothing listens on")
	}
}

func TestNewVerifier_RejectsIssuerMismatchWithoutInternalURL(t *testing.T) {
	f := fakeoidctest.New(t, fakeoidctest.WithIssuer("https://accounts.example.test"))

	// Discovering directly at f.URL, the document's issuer is not the one asked for.
	if _, err := NewVerifier(t.Context(), f.URL, "", f.ClientID()); err == nil {
		t.Fatal("discovery whose document names another issuer was accepted")
	}
}

func TestVerify_ExposesTheNonceClaim(t *testing.T) {
	f := fakeoidctest.New(t)
	v := verifier(t, f)

	got, err := v.Verify(t.Context(), f.Mint(valid, fakeoidc.MintOptions{Nonce: "n-0123456789"}))
	if err != nil || got.Nonce != "n-0123456789" {
		t.Fatalf("Verify = %+v, %v; want the nonce of the token", got, err)
	}
	got, err = v.Verify(t.Context(), f.Mint(valid, fakeoidc.MintOptions{}))
	if err != nil || got.Nonce != "" {
		t.Fatalf("a token without a nonce: %+v, %v; want an empty Nonce", got, err)
	}

	// The nonce stays visible next to ErrNoHostedDomain, so the caller checks it
	// before naming a rejected account.
	noHD := valid
	noHD.HD = ""
	got, err = v.Verify(t.Context(), f.Mint(noHD, fakeoidc.MintOptions{Nonce: "n-abc"}))
	if !errors.Is(err, ErrNoHostedDomain) || got.Nonce != "n-abc" {
		t.Fatalf("Verify = %+v, %v; want ErrNoHostedDomain with the nonce", got, err)
	}
}

func TestVerify_OnlyRS256IsAcceptedWhateverDiscoveryLists(t *testing.T) {
	// A discovery document that also lists PS256, and a token the provider's own
	// published key signed with PS256: valid in every other respect.
	f := fakeoidctest.New(t, fakeoidctest.WithAdvertisedAlgs("RS256", "PS256"))
	v := verifier(t, f)

	if _, err := v.Verify(t.Context(), f.Mint(valid, fakeoidc.MintOptions{})); err != nil {
		t.Fatalf("an RS256 token: %v", err)
	}
	got, err := v.Verify(t.Context(), f.Mint(valid, fakeoidc.MintOptions{PSS: true}))
	if err == nil {
		t.Fatal("a PS256 token was accepted: the algorithm must be pinned to RS256, not taken from the discovery document")
	}
	if got != (Claims{}) {
		t.Fatalf("claims = %+v with an error, want zero", got)
	}
}
