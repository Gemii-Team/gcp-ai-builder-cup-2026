package identity

import (
	"cmp"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"golang.org/x/oauth2"

	identityv1 "kiwi/gen/kiwi/identity/v1"
	"kiwi/internal/identity/internal/store"
	"kiwi/internal/identity/internal/vault"
	"kiwi/internal/platform/authz"
	"kiwi/internal/platform/cache"
	"kiwi/internal/platform/config"
	"kiwi/internal/platform/db"
	"kiwi/internal/platform/errs"
	"kiwi/internal/platform/google"
	"kiwi/internal/platform/oidc"
)

// meetScope is requested on its own, when a user first starts a meeting.
const meetScope = "https://www.googleapis.com/auth/meetings.space.created"

// loginScopes are all that sign-in asks for; everything else is incremental.
var loginScopes = []string{"openid", "email", "profile"}

const (
	callbackPath = "/api/auth/callback"
	consentPath  = "/api/auth/meet-consent"

	// providerTimeout bounds each request to the OAuth provider (code exchange,
	// token refresh).
	providerTimeout = 15 * time.Second
	// maxReturnTo bounds the path a sign-in returns to.
	maxReturnTo = 2048
)

// Deps are what New builds the identity domain from.
type Deps struct {
	// Config supplies PublicURL, the OAuth client, the OIDC issuer and the token
	// encryption key, and the presentational WorkspaceName and WorkspaceDomain.
	Config config.Config
	DB     *db.DB
	Cache  *cache.Cache
	// Logger receives rejected sign-ins and failures; nil selects slog.Default().
	Logger *slog.Logger
}

// Module is the identity domain: the Service the other domains use, plus the HTTP
// surface of sign-in (AuthHandler). Handler serves the same Service over Connect.
type Module struct {
	cfg      config.Config
	db       *db.DB
	cache    *cache.Cache
	log      *slog.Logger
	verifier *oidc.Verifier
	vault    *vault.Vault
	// oauth is shared by every request and never copied: oauth2 caches the
	// auth style inside the Config on first use.
	oauth  *oauth2.Config
	client *http.Client
}

var _ Service = (*Module)(nil)

// New discovers the OIDC provider and builds the module. It needs the provider to
// answer, so a process that cannot reach its issuer fails at startup, naming it.
func New(ctx context.Context, d Deps) (*Module, error) {
	cfg := d.Config
	pub, err := url.Parse(cfg.PublicURL)
	if err != nil || (pub.Scheme != "http" && pub.Scheme != "https") || pub.Host == "" {
		return nil, errors.New("identity: KIWI_PUBLIC_URL must be an absolute http(s) URL")
	}
	if d.DB == nil || d.Cache == nil {
		return nil, errors.New("identity: Deps needs DB and Cache")
	}
	// The key is configured as base64 and config decodes it; vault.New takes the
	// configured form so a key is validated in one place.
	v, err := vault.New(base64.StdEncoding.EncodeToString(cfg.TokenEncKey))
	if err != nil {
		return nil, fmt.Errorf("identity: KIWI_TOKEN_ENC_KEY: %w", err)
	}
	ver, err := oidc.NewVerifier(ctx, cfg.OIDCIssuer, cfg.OIDCInternalURL, cfg.OAuthClientID)
	if err != nil {
		return nil, fmt.Errorf("identity: OIDC provider %s: %w", cfg.OIDCIssuer, err)
	}
	endpoint := ver.Endpoint()
	endpoint.AuthStyle = oauth2.AuthStyleInParams // as Google's own endpoint

	return &Module{
		cfg:      cfg,
		db:       d.DB,
		cache:    d.Cache,
		log:      cmp.Or(d.Logger, slog.Default()),
		verifier: ver,
		vault:    v,
		oauth: &oauth2.Config{
			ClientID:     cfg.OAuthClientID,
			ClientSecret: cfg.OAuthClientSecret,
			Endpoint:     endpoint,
			RedirectURL:  pub.Scheme + "://" + pub.Host + callbackPath,
			Scopes:       loginScopes,
		},
		client: &http.Client{Timeout: providerTimeout},
	}, nil
}

// withClient makes the oauth2 package use the module's timeout-bound client.
func (m *Module) withClient(ctx context.Context) context.Context {
	return context.WithValue(ctx, oauth2.HTTPClient, m.client)
}

// LoginURL implements Service. The HTTP route also binds the flow to the browser
// that started it, which only it can do; see handleLogin.
func (m *Module) LoginURL(ctx context.Context, returnTo string) (string, error) {
	authURL, _, err := m.begin(ctx, returnTo, loginScopes)
	return authURL, err
}

// begin starts an authorization-code flow with PKCE S256 for scopes: it stores the
// state, the verifier and a nonce in Valkey and returns the provider URL and the
// state.
func (m *Module) begin(ctx context.Context, returnTo string, scopes []string, extra ...oauth2.AuthCodeOption) (authURL, state string, err error) {
	state = rand.Text() // 128 random bits
	nonce := rand.Text()
	verifier := oauth2.GenerateVerifier()
	if err := m.cache.PutOAuthState(ctx, state, cache.OAuthState{Verifier: verifier, ReturnTo: safeReturnTo(returnTo), Nonce: nonce}); err != nil {
		return "", "", fmt.Errorf("identity: store login state: %w", err)
	}
	return m.authCodeURL(state, nonce, verifier, scopes, extra...), state, nil
}

// authCodeURL builds the provider's authorization URL. It makes its own
// oauth2.Config from the immutable fields: the shared one initialises a cache
// inside itself on the first token request, so it must never be copied.
func (m *Module) authCodeURL(state, nonce, verifier string, scopes []string, extra ...oauth2.AuthCodeOption) string {
	conf := oauth2.Config{ClientID: m.oauth.ClientID, Endpoint: m.oauth.Endpoint, RedirectURL: m.oauth.RedirectURL, Scopes: scopes}
	opts := append([]oauth2.AuthCodeOption{
		oauth2.S256ChallengeOption(verifier),
		oauth2.AccessTypeOffline,
		oauth2.SetAuthURLParam("nonce", nonce),
	}, extra...)
	return conf.AuthCodeURL(state, opts...)
}

// safeReturnTo keeps a sign-in from redirecting off the site: only a plain local
// path survives, anything else is "/".
func safeReturnTo(raw string) string {
	if raw == "" || len(raw) > maxReturnTo || raw[0] != '/' {
		return "/"
	}
	if len(raw) > 1 && (raw[1] == '/' || raw[1] == '\\') { // "//host" and "/\host" are off-site to a browser
		return "/"
	}
	for i := range len(raw) {
		if raw[i] < 0x20 || raw[i] == 0x7f {
			return "/"
		}
	}
	if u, err := url.Parse(raw); err != nil || u.Scheme != "" || u.Host != "" {
		return "/"
	}
	return raw
}

// HandleCallback implements Service. It completes a flow begun by LoginURL or the
// Meet consent: it spends the state, exchanges the code with the PKCE verifier,
// verifies the ID token, finds the user (by the hd claim and google_sub, or for a
// token without hd by the invitation of its address; see resolveUser and
// resolveInvitedUser), keeps the refresh token encrypted and opens a session.
// Rejections are *CallbackError and write nothing. Binding the state to the browser
// is the HTTP route's job (handleCallback), not this method's.
func (m *Module) HandleCallback(ctx context.Context, code, state string) (Session, string, error) {
	st, err := m.cache.TakeOAuthState(ctx, state)
	if errors.Is(err, errs.ErrNotFound) {
		return Session{}, "", &CallbackError{Code: CodeStateMismatch, Err: err}
	}
	if err != nil {
		return Session{}, "", fmt.Errorf("identity: take login state: %w", err)
	}
	if code == "" {
		return Session{}, "", &CallbackError{Code: CodeExchangeFailed, Err: errors.New("the callback carries no code")}
	}

	tok, err := m.oauth.Exchange(m.withClient(ctx), code, oauth2.VerifierOption(st.Verifier))
	if err != nil {
		return Session{}, "", &CallbackError{Code: CodeExchangeFailed, Err: fmt.Errorf("exchange code: %w", err)}
	}
	raw, _ := tok.Extra("id_token").(string)
	if raw == "" {
		return Session{}, "", &CallbackError{Code: CodeExchangeFailed, Err: errors.New("the token response carries no id_token")}
	}

	// The hd of the signed token decides; the hd parameter of the callback URL is
	// never read. A token that is not the answer to this very request (its nonce
	// is not the one sent) is turned away before anything about it is revealed.
	claims, err := m.verifier.Verify(m.withClient(ctx), raw)
	if err != nil && !errors.Is(err, oidc.ErrNoHostedDomain) {
		return Session{}, "", &CallbackError{Code: CodeExchangeFailed, Err: err}
	}
	if st.Nonce == "" || subtle.ConstantTimeCompare([]byte(claims.Nonce), []byte(st.Nonce)) != 1 {
		return Session{}, "", &CallbackError{Code: CodeExchangeFailed, Err: errors.New("the ID token does not carry the nonce of the request")}
	}

	// A token with hd belongs to a workspace by its domain; one without belongs to
	// whoever was invited by address. Never the second for a token with hd.
	resolve := resolveUser
	if errors.Is(err, oidc.ErrNoHostedDomain) {
		resolve = resolveInvitedUser
	}
	user, err := resolve(ctx, m.db, claims)
	var rejection *CallbackError
	if errors.As(err, &rejection) {
		rejection.Email = normalizeEmail(claims.Email)
		return Session{}, "", rejection
	}
	if err != nil {
		return Session{}, "", fmt.Errorf("identity: resolve user: %w", err)
	}
	if err := m.storeGrant(ctx, user, tok); err != nil {
		return Session{}, "", fmt.Errorf("identity: store Google grant: %w", err)
	}

	sid, err := newSessionID()
	if err != nil {
		return Session{}, "", err
	}
	if err := m.cache.PutSession(ctx, sid, cache.SessionRecord{UserID: user.ID, WorkspaceID: user.WorkspaceID}); err != nil {
		return Session{}, "", fmt.Errorf("identity: open session: %w", err)
	}
	return Session{ID: sid, UserID: user.ID, WorkspaceID: user.WorkspaceID}, st.ReturnTo, nil
}

// newSessionID returns 256 random bits as base64url: the session id is the only
// secret between a browser and its session.
func newSessionID() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("identity: session id: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// storeGrant keeps what Google granted. A refresh token in the response is a new
// grant and replaces the stored one with the scopes it covers; without one, the
// scopes just widen the grant already stored. The write runs as kiwi_app under the
// user's own workspace.
func (m *Module) storeGrant(ctx context.Context, u store.IdentityUser, tok *oauth2.Token) error {
	scopes := grantedScopes(tok)
	return m.db.InWorkspace(ctx, u.WorkspaceID, func(ctx context.Context, tx pgx.Tx) error {
		q := store.New(tx)
		if tok.RefreshToken == "" {
			return q.MergeOAuthScopes(ctx, store.MergeOAuthScopesParams{UserID: u.ID, Scopes: scopes})
		}
		enc, err := m.vault.Encrypt([]byte(tok.RefreshToken))
		if err != nil {
			return err
		}
		return q.ReplaceOAuthToken(ctx, store.ReplaceOAuthTokenParams{
			UserID:          u.ID,
			WorkspaceID:     u.WorkspaceID,
			RefreshTokenEnc: enc,
			Scopes:          scopes,
		})
	})
}

// grantedScopes reads the scope of a token response: sorted, without duplicates.
// A response without one granted what was asked at sign-in.
func grantedScopes(tok *oauth2.Token) []string {
	s, _ := tok.Extra("scope").(string)
	scopes := strings.Fields(s)
	if len(scopes) == 0 {
		scopes = slices.Clone(loginScopes)
	}
	slices.Sort(scopes)
	return slices.Compact(scopes)
}

// Session implements Service. Reading a session slides its lifetime.
func (m *Module) Session(ctx context.Context, sessionID string) (Session, error) {
	rec, err := m.cache.GetSession(ctx, sessionID)
	if err != nil {
		return Session{}, fmt.Errorf("identity: session: %w", err)
	}
	return Session{ID: sessionID, UserID: rec.UserID, WorkspaceID: rec.WorkspaceID}, nil
}

// Logout implements Service. Ending a session that is already gone is not an error.
func (m *Module) Logout(ctx context.Context, sessionID string) error {
	if err := m.cache.DeleteSession(ctx, sessionID); err != nil {
		return fmt.Errorf("identity: logout: %w", err)
	}
	return nil
}

// Me implements Service.
func (m *Module) Me(ctx context.Context, a authz.Actor) (*identityv1.User, *identityv1.Workspace, error) {
	var (
		u store.IdentityUser
		w store.IdentityWorkspace
	)
	err := m.db.InWorkspace(ctx, a.WorkspaceID, func(ctx context.Context, tx pgx.Tx) (err error) {
		q := store.New(tx)
		if u, err = q.GetUser(ctx, a.EffectiveUserID()); err != nil {
			return err
		}
		w, err = q.GetWorkspace(ctx, a.WorkspaceID)
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil, fmt.Errorf("identity: me: %w", errs.ErrNotFound)
	}
	if err != nil {
		return nil, nil, fmt.Errorf("identity: me: %w", err)
	}

	// KIWI_WORKSPACE_NAME is how the deployment calls its workspace; the column
	// holds the domain, or nothing for the workspace without one. The configured
	// name belongs to the workspace of KIWI_WORKSPACE_DOMAIN, or to the workspace of
	// a deployment that configures no domain.
	domain := ""
	if w.GoogleDomain != nil {
		domain = *w.GoogleDomain
	}
	name := w.Name
	if m.cfg.WorkspaceName != "" && (m.cfg.WorkspaceDomain == "" || strings.EqualFold(m.cfg.WorkspaceDomain, domain)) {
		name = m.cfg.WorkspaceName
	}
	return userProto(u), &identityv1.Workspace{Id: w.ID.String(), Name: name, GoogleDomain: domain}, nil
}

// ListUsers implements Service.
func (m *Module) ListUsers(ctx context.Context, a authz.Actor) ([]*identityv1.User, error) {
	var rows []store.IdentityUser
	err := m.db.InWorkspace(ctx, a.WorkspaceID, func(ctx context.Context, tx pgx.Tx) (err error) {
		rows, err = store.New(tx).ListUsers(ctx)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("identity: list users: %w", err)
	}
	out := make([]*identityv1.User, len(rows))
	for i, r := range rows {
		out[i] = userProto(r)
	}
	return out, nil
}

func userProto(u store.IdentityUser) *identityv1.User {
	return &identityv1.User{Id: u.ID.String(), Email: u.Email, Name: u.Name, AvatarUrl: u.AvatarUrl}
}

// IssueRealtimeTicket implements Service. Only a signed-in user opens a WebSocket;
// an agent or a job acting for one does not.
func (m *Module) IssueRealtimeTicket(ctx context.Context, a authz.Actor) (string, error) {
	if a.Kind != authz.ActorUser {
		return "", fmt.Errorf("identity: realtime ticket for a %s actor: %w", a.Kind, errs.ErrForbidden)
	}
	if a.UserID == uuid.Nil || a.WorkspaceID == uuid.Nil {
		return "", fmt.Errorf("identity: realtime ticket needs a user and a workspace: %w", errs.ErrInvalid)
	}
	ticket, err := m.cache.IssueTicket(ctx, cache.TicketRecord{UserID: a.UserID, WorkspaceID: a.WorkspaceID})
	if err != nil {
		return "", fmt.Errorf("identity: realtime ticket: %w", err)
	}
	return ticket, nil
}

// UserTokenSource implements Service. The token source refreshes with the user's
// stored refresh token, so ctx must outlive the calls made through it. It returns
// *google.ConsentRequiredError when the user never signed in, did not grant scope,
// or has revoked the grant (which only shows once the source asks Google).
func (m *Module) UserTokenSource(ctx context.Context, userID uuid.UUID, scope string) (oauth2.TokenSource, error) {
	ws, err := userWorkspace(ctx, m.db, userID)
	if err != nil {
		return nil, err
	}
	var row store.IdentityOauthToken
	err = m.db.InWorkspace(ctx, ws, func(ctx context.Context, tx pgx.Tx) (err error) {
		row, err = store.New(tx).GetOAuthToken(ctx, userID)
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, &google.ConsentRequiredError{URL: consentPath}
	}
	if err != nil {
		return nil, fmt.Errorf("identity: load Google grant: %w", err)
	}
	if !slices.Contains(row.Scopes, scope) {
		return nil, &google.ConsentRequiredError{URL: consentPath}
	}
	refresh, err := m.vault.Decrypt(row.RefreshTokenEnc)
	if err != nil {
		return nil, fmt.Errorf("identity: open Google grant: %w", err)
	}
	src := m.oauth.TokenSource(m.withClient(ctx), &oauth2.Token{RefreshToken: string(refresh)})
	return grantSource{src}, nil
}

// grantSource turns Google's answer to a revoked or expired refresh token into
// the consent the user has to give again.
type grantSource struct{ src oauth2.TokenSource }

func (g grantSource) Token() (*oauth2.Token, error) {
	tok, err := g.src.Token()
	var re *oauth2.RetrieveError
	if errors.As(err, &re) && re.ErrorCode == "invalid_grant" {
		return nil, &google.ConsentRequiredError{URL: consentPath}
	}
	return tok, err
}

// userEmail is the address of the session's user, which the Meet consent hands to
// Google as a login hint. errs.ErrNotFound when the user is gone.
func (m *Module) userEmail(ctx context.Context, s Session) (string, error) {
	var email string
	err := m.db.InWorkspace(ctx, s.WorkspaceID, func(ctx context.Context, tx pgx.Tx) error {
		u, err := store.New(tx).GetUser(ctx, s.UserID)
		email = u.Email
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return "", fmt.Errorf("identity: the user of the session is gone: %w", errs.ErrNotFound)
	}
	if err != nil {
		return "", fmt.Errorf("identity: load user: %w", err)
	}
	return email, nil
}
