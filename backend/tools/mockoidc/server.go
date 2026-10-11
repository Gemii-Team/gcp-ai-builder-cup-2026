package main

import (
	"cmp"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"kiwi/internal/testkit/fakeoidc"
)

// The defaults describe the mockauth stack: K.I.W.I. on http://localhost:8080
// and a workspace seeded for demo.example with `kiwi seed`.
const (
	listenAddr          = ":8090"
	defaultIssuer       = "http://localhost:8080/mockoidc"
	defaultHD           = "demo.example"
	defaultUsers        = "alice@demo.example,bob@demo.example"
	defaultOutsider     = "carol@elsewhere.example"
	defaultRedirectURIs = "http://localhost:8080/api/auth/callback"
)

// newProvider builds the mock sign-in provider the MOCKOIDC_* variables
// describe: a fakeoidc.Provider whose Workspace users carry hd and sub
// "seed:" + email (the placeholder a seed leaves, so a sign-in lands on the
// seeded user), plus an outsider without hd, and which takes whatever OAuth
// client it is presented. It redirects only to MOCKOIDC_REDIRECT_URIS.
//
// A mock that signs anybody in must never run beside real users: any KIWI_ENV
// other than unset or "local" is refused.
func newProvider(getenv func(string) string) (*fakeoidc.Provider, error) {
	if env := getenv("KIWI_ENV"); env != "" && env != "local" {
		return nil, fmt.Errorf("refusing to start with KIWI_ENV=%q: the mock sign-in provider signs anybody in and runs only locally (KIWI_ENV unset or \"local\")", env)
	}

	value := func(name, fallback string) string { return cmp.Or(strings.TrimSpace(getenv(name)), fallback) }

	issuer := value("MOCKOIDC_ISSUER", defaultIssuer)
	if err := checkHTTPURL("MOCKOIDC_ISSUER", issuer); err != nil {
		return nil, err
	}
	hd := value("MOCKOIDC_HD", defaultHD)
	if hd == "" || strings.ContainsAny(hd, " \t,@/:") {
		return nil, fmt.Errorf("MOCKOIDC_HD must be a domain such as %s, got %q", defaultHD, hd)
	}

	var users []fakeoidc.User
	seen := map[string]bool{}
	for _, email := range splitList(value("MOCKOIDC_USERS", defaultUsers)) {
		if err := checkEmail("MOCKOIDC_USERS", email); err != nil {
			return nil, err
		}
		if seen[strings.ToLower(email)] {
			return nil, fmt.Errorf("MOCKOIDC_USERS lists %s twice", email)
		}
		seen[strings.ToLower(email)] = true
		users = append(users, fakeoidc.User{Email: email, Name: displayName(email)})
	}

	outsider := value("MOCKOIDC_OUTSIDER", defaultOutsider)
	if err := checkEmail("MOCKOIDC_OUTSIDER", outsider); err != nil {
		return nil, err
	}
	if seen[strings.ToLower(outsider)] {
		return nil, fmt.Errorf("MOCKOIDC_OUTSIDER %s is also in MOCKOIDC_USERS: the outsider has no hosted domain", outsider)
	}

	redirects := splitList(value("MOCKOIDC_REDIRECT_URIS", defaultRedirectURIs))
	for _, uri := range redirects {
		if err := checkHTTPURL("MOCKOIDC_REDIRECT_URIS", uri); err != nil {
			return nil, err
		}
	}

	p, err := fakeoidc.NewProvider(fakeoidc.Config{
		Issuer:       issuer,
		AnyClient:    true,
		HostedDomain: hd,
		Users:        users,
		Outsider:     &fakeoidc.User{Email: outsider, Name: displayName(outsider)},
		RedirectURIs: redirects,
	})
	if err != nil {
		return nil, fmt.Errorf("configure the provider: %w", err)
	}
	return p, nil
}

// run serves the provider on listenAddr until ctx is done and returns the exit
// code: 0 after a clean shutdown, 1 for a configuration or serving failure.
func run(ctx context.Context, getenv func(string) string, stderr io.Writer) int {
	p, err := newProvider(getenv)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "mockoidc: %v\n", err)
		return 1
	}

	srv := &http.Server{Addr: listenAddr, Handler: p, ReadHeaderTimeout: 10 * time.Second}
	failed := make(chan error, 1)
	go func() { failed <- srv.ListenAndServe() }()
	slog.Info("serving", "addr", listenAddr, "issuer", p.Issuer())

	select {
	case err := <-failed:
		_, _ = fmt.Fprintf(stderr, "mockoidc: %v\n", err)
		return 1
	case <-ctx.Done():
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdown); err != nil {
		_, _ = fmt.Fprintf(stderr, "mockoidc: shutdown: %v\n", err)
		return 1
	}
	return 0
}

// splitList splits a comma-separated list and drops blank entries.
func splitList(s string) []string {
	var out []string
	for part := range strings.SplitSeq(s, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

func checkEmail(name, email string) error {
	local, host, ok := strings.Cut(email, "@")
	if !ok || local == "" || host == "" || strings.ContainsAny(email, " \t<>") || strings.Contains(host, "@") {
		return fmt.Errorf("%s: %q is not an email address", name, email)
	}
	return nil
}

func checkHTTPURL(name, raw string) error {
	if u, err := url.Parse(raw); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("%s: %q is not an absolute http(s) URL", name, raw)
	}
	return nil
}

// displayName is the local part of an address, capitalised: alice@x -> Alice.
func displayName(email string) string {
	local, _, _ := strings.Cut(email, "@")
	first, size := utf8.DecodeRuneInString(local)
	return string(unicode.ToUpper(first)) + local[size:]
}
