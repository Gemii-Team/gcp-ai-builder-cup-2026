package identity

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"time"

	"golang.org/x/oauth2"

	"kiwi/internal/platform/cache"
	"kiwi/internal/platform/errs"
	"kiwi/internal/platform/httpx"
)

const (
	// SessionCookie is the cookie of a signed-in browser: an opaque session id. The
	// __Host- prefix makes browsers insist on Secure, Path=/ and no Domain.
	SessionCookie = "__Host-kiwi_sid"
	// stateCookie binds a sign-in to the browser that started it. Without it, a
	// leaked callback URL could be completed by anyone who holds it.
	stateCookie = "__Host-kiwi_oauth"
	// stateCookieMaxAge is how long a person may take at the provider; the state
	// in Valkey lives as long.
	stateCookieMaxAge = 10 * time.Minute
)

// AuthHandler serves the /api/auth/ routes: login, callback, logout, config and
// meet-consent. Mount it at "/api/auth/" outside the session interceptor. The one
// POST, logout, carries its own origin check, so a cross-site sign-out is refused
// however the shell wraps this handler.
func (m *Module) AuthHandler() http.Handler {
	sameOrigin := httpx.RequireSameOrigin(m.cfg.PublicURL)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/auth/login", m.handleLogin)
	mux.HandleFunc("GET "+callbackPath, m.handleCallback)
	mux.Handle("POST /api/auth/logout", sameOrigin(http.HandlerFunc(m.handleLogout)))
	mux.HandleFunc("GET /api/auth/config", m.handleConfig)
	mux.HandleFunc("GET "+consentPath, m.handleMeetConsent)
	return mux
}

func (m *Module) handleLogin(w http.ResponseWriter, r *http.Request) {
	authURL, state, err := m.begin(r.Context(), r.URL.Query().Get("return_to"), loginScopes, false)
	if err != nil {
		m.log.ErrorContext(r.Context(), "sign-in could not start", "err", err)
		http.Error(w, "sign-in is unavailable", http.StatusInternalServerError)
		return
	}
	redirectToProvider(w, r, authURL, state)
}

// handleMeetConsent sends a signed-in user to Google for the Meet scope, keeping
// what they already granted. The sign-in scopes ride along (Google shows no new
// consent for them): the callback identifies the account that consented by its ID
// token, which carries email and hd only when those scopes are asked for. It always
// forces the consent screen (prompt=consent): Google issues a refresh token only
// when it shows that screen, and without one a grant our database lost while Google
// still remembers it could never be stored again.
func (m *Module) handleMeetConsent(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var email string
	sess, err := m.requestSession(r)
	if err == nil {
		email, err = m.userEmail(ctx, sess)
	}
	if errors.Is(err, errs.ErrNotFound) {
		http.Error(w, "sign in first", http.StatusUnauthorized)
		return
	}
	var authURL, state string
	if err == nil {
		authURL, state, err = m.begin(ctx, r.URL.Query().Get("return_to"), append(slices.Clone(loginScopes), meetConsentScopes...), true,
			oauth2.SetAuthURLParam("include_granted_scopes", "true"),
			oauth2.SetAuthURLParam("login_hint", email),
			oauth2.SetAuthURLParam("prompt", "consent"),
		)
	}
	if err != nil {
		m.log.ErrorContext(ctx, "Meet consent could not start", "err", err)
		http.Error(w, "consent is unavailable", http.StatusInternalServerError)
		return
	}
	redirectToProvider(w, r, authURL, state)
}

// requestSession returns the session the request's cookie names; errs.ErrNotFound
// when there is no cookie or the session is gone.
func (m *Module) requestSession(r *http.Request) (Session, error) {
	c, err := r.Cookie(SessionCookie)
	if err != nil {
		return Session{}, fmt.Errorf("identity: no session cookie: %w", errs.ErrNotFound)
	}
	return m.Session(r.Context(), c.Value)
}

func redirectToProvider(w http.ResponseWriter, r *http.Request, authURL, state string) {
	w.Header().Set("Cache-Control", "no-store")
	http.SetCookie(w, &http.Cookie{
		Name:     stateCookie,
		Value:    state,
		Path:     "/",
		MaxAge:   int(stateCookieMaxAge / time.Second),
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteLaxMode, // the provider's redirect back is a top-level navigation
	})
	http.Redirect(w, r, authURL, http.StatusFound)
}

func (m *Module) handleCallback(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	q := r.URL.Query()
	state := q.Get("state")
	w.Header().Set("Cache-Control", "no-store")

	// Only the browser that started the flow holds the cookie. A mismatch leaves
	// the state in Valkey alone, so a stranger replaying a leaked URL cannot burn
	// the real person's sign-in.
	if !boundTo(r, state) {
		signinError(w, r, CodeStateMismatch, "")
		return
	}
	http.SetCookie(w, expired(stateCookie))

	if providerErr := q.Get("error"); providerErr != "" {
		// The person said no, or the provider failed; the state is spent either way.
		_, _ = m.cache.TakeOAuthState(ctx, state)
		code := CodeExchangeFailed
		if providerErr == "access_denied" {
			code = CodeAccessDenied
		}
		m.log.InfoContext(ctx, "sign-in refused by the provider", "provider_error", providerErr)
		signinError(w, r, code, "")
		return
	}

	sess, returnTo, err := m.HandleCallback(ctx, q.Get("code"), state)
	if err != nil {
		code, email := CodeExchangeFailed, ""
		var ce *CallbackError
		if errors.As(err, &ce) {
			code, email = ce.Code, ce.Email
			m.log.InfoContext(ctx, "sign-in rejected", "code", code, "reason", ce.Reason, "err", err)
		} else {
			m.log.ErrorContext(ctx, "sign-in failed", "err", err)
		}
		signinError(w, r, code, email)
		return
	}

	// A browser that signs in again gives up its previous session: the new id
	// replaces the cookie, so the old one would only linger in Valkey.
	if old, err := r.Cookie(SessionCookie); err == nil && old.Value != sess.ID {
		if err := m.cache.DeleteSession(ctx, old.Value); err != nil {
			m.log.WarnContext(ctx, "previous session was not removed", "err", err)
		}
	}
	http.SetCookie(w, &http.Cookie{
		Name:     SessionCookie,
		Value:    sess.ID,
		Path:     "/",
		MaxAge:   int(cache.DefaultSessionAbsolute / time.Second),
		HttpOnly: true,
		Secure:   true, // also over http://localhost: Chromium and Firefox accept it there, Safari does not
		SameSite: http.SameSiteLaxMode,
	})
	http.Redirect(w, r, returnTo, http.StatusFound)
}

// boundTo reports whether the request carries the binding cookie of state.
func boundTo(r *http.Request, state string) bool {
	c, err := r.Cookie(stateCookie)
	return err == nil && state != "" && subtle.ConstantTimeCompare([]byte(c.Value), []byte(state)) == 1
}

// signinError sends the browser to the sign-in page with the reason. The email, which
// only domain_not_allowed and not_invited carry and which comes from the verified token,
// lets the page name the account that was turned away.
func signinError(w http.ResponseWriter, r *http.Request, code, email string) {
	target := "/signin?error=" + url.QueryEscape(code)
	if email != "" {
		target += "&email=" + url.QueryEscape(email)
	}
	http.Redirect(w, r, target, http.StatusFound)
}

// expired is the Set-Cookie that deletes the cookie called name.
func expired(name string) *http.Cookie {
	return &http.Cookie{Name: name, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode}
}

func (m *Module) handleLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(SessionCookie); err == nil {
		if err := m.Logout(r.Context(), c.Value); err != nil {
			m.log.ErrorContext(r.Context(), "sign-out failed", "err", err)
			http.Error(w, "sign-out is unavailable", http.StatusInternalServerError)
			return
		}
	}
	http.SetCookie(w, expired(SessionCookie))
	w.WriteHeader(http.StatusNoContent)
}

// handleConfig answers the sign-in page before anyone is signed in. The names are
// presentational: authorisation is the verified hd claim and the Internal OAuth
// app, never these.
func (m *Module) handleConfig(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(struct {
		WorkspaceName string `json:"workspaceName"`
		Domain        string `json:"domain"`
	}{m.cfg.WorkspaceName, m.cfg.WorkspaceDomain})
}
