package fakeoidc

// The accounts fakeoidctest.New serves: two Workspace users and Carol, whose
// token has no hd.
const (
	DefaultClientID     = "kiwi-test-client"
	DefaultClientSecret = "kiwi-test-secret"
	DefaultDomain       = "acme.test"
	// DefaultRedirectURI is the callback of a local K.I.W.I. on KIWI_PUBLIC_URL
	// http://localhost:8080, the only redirect_uri fakeoidctest.New registers by default.
	DefaultRedirectURI = "http://localhost:8080/api/auth/callback"

	AliceEmail = "alice@" + DefaultDomain
	AliceName  = "Alice Adams"
	BobEmail   = "bob@" + DefaultDomain
	BobName    = "Bob Brown"
	CarolEmail = "carol@personal.test"
	CarolName  = "Carol Personal"
)
