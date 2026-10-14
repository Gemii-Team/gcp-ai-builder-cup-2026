package identity

import (
	"cmp"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/mail"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"kiwi/internal/identity/internal/store"
	"kiwi/internal/platform/cache"
	"kiwi/internal/platform/db"
	"kiwi/internal/platform/errs"
	"kiwi/internal/platform/oidc"
)

// This file is the identity domain's only db.System caller (see the allowlist in
// platform/db). kiwi_system bypasses row-level security but holds just SELECT and
// INSERT on identity.workspaces, SELECT, INSERT and UPDATE on identity.users and
// SELECT, INSERT, UPDATE and DELETE on identity.sessions, so nothing here can read or
// change another table. Everything that can run inside a workspace does, as kiwi_app.
//
// The session store (see Sessions below) is here and not in a file of its own because
// of that rule: a session is found by its id alone, before any workspace is known, so
// it can only be read as kiwi_system, and keeping every System caller of the domain in
// one file keeps the allowlist at one line for it.

// The codes of the sign-in page's error parameter.
const (
	CodeStateMismatch    = "state_mismatch"
	CodeDomainNotAllowed = "domain_not_allowed"
	CodeNotInvited       = "not_invited"
	CodeAccessDenied     = "access_denied"
	CodeExchangeFailed   = "exchange_failed"
)

// Why a sign-in was turned away. It goes to the log, never to the browser.
const (
	reasonUnverified        = "unverified"
	reasonUninvited         = "uninvited"
	reasonAmbiguous         = "ambiguous"
	reasonBoundToOtherSub   = "bound_to_other_subject"
	reasonSubBoundElsewhere = "sub_bound_elsewhere"
	reasonUnknownDomain     = "unknown_domain"
	reasonOtherWorkspace    = "sub_in_other_workspace"
)

// uniqueViolation is the SQLSTATE of a unique constraint violation.
const uniqueViolation = "23505"

// googleSubConstraint is the unique constraint of identity.users.google_sub
// (0002_identity.sql names it by Postgres's rule for `google_sub text NOT NULL UNIQUE`).
const googleSubConstraint = "users_google_sub_key"

// seedSubPrefix starts the google_sub of a placeholder user: "seed:" + email.
const seedSubPrefix = "seed:"

// CallbackError is a sign-in the callback turned away. Code is one of the Code*
// constants. Email is set only for CodeDomainNotAllowed and CodeNotInvited: the
// lowercased address of the signature-verified token, so the sign-in page can name the
// account that was refused. Reason says why, for the log.
type CallbackError struct {
	Code   string
	Email  string
	Reason string
	Err    error
}

// Error implements error. It leaves the email out so it stays out of logs.
func (e *CallbackError) Error() string {
	msg := "identity: sign-in rejected: " + e.Code
	if e.Reason != "" {
		msg += " (" + e.Reason + ")"
	}
	if e.Err != nil {
		msg += ": " + e.Err.Error()
	}
	return msg
}

// Unwrap returns the cause.
func (e *CallbackError) Unwrap() error { return e.Err }

func rejected(code, reason string) *CallbackError { return &CallbackError{Code: code, Reason: reason} }

// resolveUser is the sign-in rule for a verified token with an hd claim. The
// workspace is the one whose google_domain is the token's hd, compared without regard
// to case (never created here, and the hd parameter of the callback URL is never read:
// a domain without a workspace is CodeDomainNotAllowed); the user is the one with the
// token's sub, else the placeholder a seed left under the same email, else a new row.
// It also refreshes the user's email, name and avatar from the token. A token with hd
// is judged by its hd alone: it never falls through to resolveInvitedUser.
func resolveUser(ctx context.Context, d *db.DB, c oidc.Claims) (store.IdentityUser, error) {
	email := normalizeEmail(c.Email)
	name := cmp.Or(c.Name, localPart(email))

	var user store.IdentityUser
	err := d.System(ctx, func(ctx context.Context, tx pgx.Tx) error {
		q := store.New(tx)
		if err := q.LockSignIn(ctx, c.Sub); err != nil {
			return fmt.Errorf("lock sign-in: %w", err)
		}
		ws, err := q.FindWorkspaceByDomain(ctx, c.HD)
		if errors.Is(err, pgx.ErrNoRows) {
			return rejected(CodeDomainNotAllowed, reasonUnknownDomain)
		}
		if err != nil {
			return fmt.Errorf("find workspace: %w", err)
		}

		existing, err := q.GetUserBySub(ctx, c.Sub)
		switch {
		case err == nil:
			if existing.WorkspaceID != ws.ID {
				return rejected(CodeDomainNotAllowed, reasonOtherWorkspace)
			}
			user, err = q.UpdateUserProfile(ctx, store.UpdateUserProfileParams{ID: existing.ID, Email: email, Name: name, AvatarUrl: c.Picture})
			if err != nil {
				return fmt.Errorf("update user: %w", err)
			}
			return nil
		case !errors.Is(err, pgx.ErrNoRows):
			return fmt.Errorf("find user: %w", err)
		}

		user, err = q.AdoptSeedUser(ctx, store.AdoptSeedUserParams{
			GoogleSub: c.Sub, Name: name, AvatarUrl: c.Picture, WorkspaceID: ws.ID, Email: email,
		})
		if err == nil {
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("adopt seeded user: %w", err)
		}

		id, err := uuid.NewV7()
		if err != nil {
			return fmt.Errorf("user id: %w", err)
		}
		user, err = q.InsertUser(ctx, store.InsertUserParams{
			ID: id, WorkspaceID: ws.ID, GoogleSub: c.Sub, Email: email, Name: name, AvatarUrl: c.Picture,
		})
		if err != nil {
			return fmt.Errorf("create user: %w", err)
		}
		return nil
	})
	if err != nil {
		return store.IdentityUser{}, err
	}
	return user, nil
}

// resolveInvitedUser is the sign-in rule for a verified token without an hd claim,
// such as a personal Google account. A person is let in only when a seed or an
// invitation already made exactly one user row for the address of the token:
//
//   - the email of the token must be verified (it is all the token says of who owns
//     the address, with no hosted domain behind it);
//   - no row is uninvited, two or more are ambiguous (the address is in two
//     workspaces): both are CodeNotInvited, indistinguishable to the browser;
//   - the one row is used only when its google_sub is the placeholder
//     "seed:" + email (it is adopted: the token's sub, name and avatar are set, in the
//     user's own workspace) or already the token's sub; a row bound to another sub
//     belongs to another Google account, and so does a placeholder whose adoption
//     would bind a sub that is already the user of another row (CodeNotInvited too).
//
// It never creates a workspace or a user, and the System query it runs is a read. When
// another sign-in adopts the placeholder between the read and the update, the update
// matches nothing and this one is turned away. The address is the lowercased one
// everywhere.
//
// What this rule trusts, on purpose:
//   - It applies to the rows of EVERY workspace, including one that has a
//     google_domain (the mock's bob, who has no hd although his workspace has a domain,
//     signs in through it); it is not restricted to the workspace without a domain.
//   - Until the first sign-in binds a placeholder, any Google account that proves that
//     exact address (a verified email, no hd) can claim it. The invitation is the
//     address, not a person.
//   - A placeholder never expires.
//   - The seed refuses an address that already has a row in another workspace, so the
//     seed never makes the ambiguous case. A sign-in with an hd does: it adds the address
//     to the workspace of its domain even when another workspace has invited it already.
//     From then on a sign-in without hd for that address is ambiguous and fails closed
//     (CodeNotInvited); it never picks one of the workspaces.
func resolveInvitedUser(ctx context.Context, d *db.DB, c oidc.Claims) (store.IdentityUser, error) {
	if !c.EmailVerified {
		return store.IdentityUser{}, rejected(CodeNotInvited, reasonUnverified)
	}
	email := normalizeEmail(c.Email)

	var rows []store.FindUsersByEmailRow
	err := d.System(ctx, func(ctx context.Context, tx pgx.Tx) (err error) {
		rows, err = store.New(tx).FindUsersByEmail(ctx, email)
		return err
	})
	if err != nil {
		return store.IdentityUser{}, fmt.Errorf("find users by email: %w", err)
	}
	switch {
	case len(rows) == 0:
		return store.IdentityUser{}, rejected(CodeNotInvited, reasonUninvited)
	case len(rows) > 1:
		return store.IdentityUser{}, rejected(CodeNotInvited, reasonAmbiguous)
	}
	row := rows[0]
	if row.GoogleSub != c.Sub && row.GoogleSub != seedSubPrefix+email {
		return store.IdentityUser{}, rejected(CodeNotInvited, reasonBoundToOtherSub)
	}

	name := cmp.Or(c.Name, localPart(email))
	var user store.IdentityUser
	err = d.InWorkspace(ctx, row.WorkspaceID, func(ctx context.Context, tx pgx.Tx) (err error) {
		q := store.New(tx)
		if row.GoogleSub == c.Sub {
			user, err = q.UpdateUserProfile(ctx, store.UpdateUserProfileParams{ID: row.ID, Email: email, Name: name, AvatarUrl: c.Picture})
			return err
		}
		user, err = q.AdoptSeedUser(ctx, store.AdoptSeedUserParams{
			GoogleSub: c.Sub, Name: name, AvatarUrl: c.Picture, WorkspaceID: row.WorkspaceID, Email: email,
		})
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		// The row changed after it was read: another account took the placeholder, or
		// the user is gone.
		return store.IdentityUser{}, rejected(CodeNotInvited, reasonBoundToOtherSub)
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == uniqueViolation && pgErr.ConstraintName == googleSubConstraint {
		// The token's subject is already the user of another row (the same Google
		// account seeded under two addresses, or one whose primary address changed):
		// binding it to this placeholder too is refused by the database. The
		// transaction rolled back, so nothing was written.
		return store.IdentityUser{}, rejected(CodeNotInvited, reasonSubBoundElsewhere)
	}
	if err != nil {
		return store.IdentityUser{}, fmt.Errorf("adopt user: %w", err)
	}
	return user, nil
}

// userWorkspace returns the workspace a user belongs to, for callers that have only
// the user id (UserTokenSource). It is errs.ErrNotFound for an unknown user.
func userWorkspace(ctx context.Context, d *db.DB, userID uuid.UUID) (uuid.UUID, error) {
	var ws uuid.UUID
	err := d.System(ctx, func(ctx context.Context, tx pgx.Tx) (err error) {
		ws, err = store.New(tx).GetUserWorkspace(ctx, userID)
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, fmt.Errorf("identity: user %s: %w", userID, errs.ErrNotFound)
	}
	if err != nil {
		return uuid.Nil, fmt.Errorf("identity: find the workspace of user %s: %w", userID, err)
	}
	return ws, nil
}

// noDomainWorkspaceName is what the workspace without a domain is called: there is no
// domain to name it after. KIWI_WORKSPACE_NAME, when set, is what users see instead.
const noDomainWorkspaceName = "Team"

// seedNamespace derives the id of the one workspace without a domain (UUIDv5): it has
// no domain to be found by, so a fixed id is what makes seeding it twice a no-op.
var (
	seedNamespace       = uuid.NewSHA1(uuid.NameSpaceDNS, []byte("identity.seed.kiwi.local"))
	noDomainWorkspaceID = uuid.NewSHA1(seedNamespace, []byte("workspace:no-domain"))
)

// EnsureSeedWorkspace resolves the workspace of domain, creating it (named after the
// domain) if it does not exist, and makes sure each email has a user in it, creating a
// placeholder with google_sub = "seed:" + email and the local part of the address as
// its name. It returns the workspace id and the user ids in the order of the distinct
// emails.
//
// An empty domain stands for the one workspace without a domain (google_domain NULL,
// named noDomainWorkspaceName, fixed id), for a team whose Google accounts carry no hd
// claim: its members sign in by invitation, which is this placeholder (see
// resolveInvitedUser). Another way to invite someone must write the same row, because
// that rule reads nothing else.
//
// Each email is trimmed, lowercased and must be a bare address (net/mail), else
// errs.ErrInvalid names it; duplicates collapse. An address that already has a user in
// another workspace is errs.ErrInvalid too: the seed never makes the address-in-two-
// workspaces case that sign-in turns away. Running it again changes nothing, and a
// placeholder the sign-in has adopted keeps its id. The first real sign-in of a
// placeholder's email completes the user.
func EnsureSeedWorkspace(ctx context.Context, d *db.DB, domain string, emails []string) (workspaceID uuid.UUID, userIDs []uuid.UUID, err error) {
	domain = normalizeDomain(domain)
	norm, err := seedAddresses(emails)
	if err != nil {
		return uuid.Nil, nil, err
	}
	label := cmp.Or(domain, "(no domain)")

	ids := make([]uuid.UUID, len(norm))
	err = d.System(ctx, func(ctx context.Context, tx pgx.Tx) error {
		q := store.New(tx)
		if domain == "" {
			err := q.InsertWorkspaceIfAbsent(ctx, store.InsertWorkspaceIfAbsentParams{ID: noDomainWorkspaceID, Name: noDomainWorkspaceName})
			if err != nil {
				return fmt.Errorf("create workspace: %w", err)
			}
			// The insert is untargeted and swallows any unique violation: read it back.
			ws, err := q.GetWorkspace(ctx, noDomainWorkspaceID)
			if err != nil {
				return fmt.Errorf("find workspace: %w", err)
			}
			workspaceID = ws.ID
		} else {
			wsID, err := uuid.NewV7()
			if err != nil {
				return fmt.Errorf("workspace id: %w", err)
			}
			// identity.workspaces.name is NOT NULL: the domain names the workspace.
			if err := q.InsertWorkspaceIfAbsent(ctx, store.InsertWorkspaceIfAbsentParams{ID: wsID, Name: domain, GoogleDomain: &domain}); err != nil {
				return fmt.Errorf("create workspace: %w", err)
			}
			ws, err := q.FindWorkspaceByDomain(ctx, domain)
			if err != nil {
				return fmt.Errorf("find workspace: %w", err)
			}
			workspaceID = ws.ID
		}

		for i, email := range norm {
			existing, err := q.FindUsersByEmail(ctx, email)
			if err != nil {
				return fmt.Errorf("find users of %s: %w", email, err)
			}
			for _, u := range existing {
				if u.WorkspaceID != workspaceID {
					return fmt.Errorf("%s already has a user in another workspace: %w", email, errs.ErrInvalid)
				}
			}
			if len(existing) > 0 {
				ids[i] = existing[0].ID
				continue
			}
			userID, err := uuid.NewV7()
			if err != nil {
				return fmt.Errorf("user id: %w", err)
			}
			if err := q.InsertSeedUserIfAbsent(ctx, store.InsertSeedUserIfAbsentParams{ID: userID, WorkspaceID: workspaceID, Email: email, Name: localPart(email)}); err != nil {
				return fmt.Errorf("create placeholder for %s: %w", email, err)
			}
			u, err := q.GetUserByEmail(ctx, store.GetUserByEmailParams{WorkspaceID: workspaceID, Email: email})
			if err != nil {
				return fmt.Errorf("find user %s: %w", email, err)
			}
			ids[i] = u.ID
		}
		return nil
	})
	if err != nil {
		return uuid.Nil, nil, fmt.Errorf("identity: seed workspace %s: %w", label, err)
	}
	return workspaceID, ids, nil
}

// seedAddresses trims and lowercases emails, requires each to be a bare address
// (neither a display name, nor angle brackets, a comment or a list) and drops
// repeats, keeping the order of first appearance.
func seedAddresses(emails []string) ([]string, error) {
	out := make([]string, 0, len(emails))
	for _, e := range emails {
		addr := normalizeEmail(e)
		parsed, err := mail.ParseAddress(addr)
		if err != nil || parsed.Name != "" || parsed.Address != addr {
			return nil, fmt.Errorf("identity: seed workspace: %q is not a bare email address: %w", e, errs.ErrInvalid)
		}
		if !slices.Contains(out, addr) {
			out = append(out, addr)
		}
	}
	return out, nil
}

// ListWorkspaceIDs returns the id of every workspace, in id order. It is for operator
// commands that must visit each tenant, such as `kiwi reindex`, which has no workspace
// to start from: identity owns identity.workspaces, so the enumeration lives here, and
// it reads nothing else. It is one SELECT on identity.workspaces, within what
// kiwi_system is granted; the caller then works inside each workspace as kiwi_app.
func ListWorkspaceIDs(ctx context.Context, d *db.DB) ([]uuid.UUID, error) {
	var ids []uuid.UUID
	err := d.System(ctx, func(ctx context.Context, tx pgx.Tx) (err error) {
		ids, err = store.New(tx).ListWorkspaceIDs(ctx)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("identity: list workspaces: %w", err)
	}
	return ids, nil
}

// NoDomainWorkspaceEmails returns the email of every user of the one workspace without
// a domain, by name and then email, and nothing when the workspace has no users or does
// not exist yet. It is for the seed, which must look at what an earlier seed made before
// it adds to it. The workspace has a fixed id, so this is a read inside it as kiwi_app:
// row-level security shows nothing of another workspace.
func NoDomainWorkspaceEmails(ctx context.Context, d *db.DB) ([]string, error) {
	var rows []store.IdentityUser
	err := d.InWorkspace(ctx, noDomainWorkspaceID, func(ctx context.Context, tx pgx.Tx) (err error) {
		rows, err = store.New(tx).ListUsers(ctx)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("identity: list the users of the workspace without a domain: %w", err)
	}
	emails := make([]string, len(rows))
	for i, u := range rows {
		emails[i] = u.Email
	}
	return emails, nil
}

func normalizeEmail(s string) string  { return strings.ToLower(strings.TrimSpace(s)) }
func normalizeDomain(s string) string { return strings.ToLower(strings.TrimSpace(s)) }

// localPart is the name a user has until Google supplies one.
func localPart(email string) string {
	local, _, _ := strings.Cut(email, "@")
	return local
}

// Sessions is the Postgres implementation of cache.Sessions, for a deployment whose
// cache is in the process and so cannot hold them (KIWI_CACHE_PROVIDER=memory): a
// redeploy must not sign everyone out. It keeps the behaviour of the Valkey one: a
// session lives for the sliding lifetime after its last read, never past the absolute
// lifetime after its creation, and a read slides it.
//
// The row is keyed by the SHA-256 of the session id, never the id itself: the id is a
// bearer credential, and a copy of the table or a read-only SQL injection must not be
// able to sign anyone in. Every statement runs as kiwi_system, which is granted
// SELECT, INSERT, UPDATE and DELETE on identity.sessions alone for this (migration
// 0012); kiwi_app has nothing on the table.
//
// Expired rows are deleted by the next PutSession, that is by the next sign-in, which
// is the only thing that adds rows; reads ignore them in the meantime. So the table
// never holds more than the live sessions plus those that expired since the last
// sign-in, and no background job exists to start, stop or monitor.
type Sessions struct {
	db  *db.DB
	opt cache.Options
	now func() time.Time // replaced by tests to move time
}

var _ cache.Sessions = (*Sessions)(nil)

// NewSessions returns the Postgres session store. Only the session fields of opt apply;
// a zero one takes its default.
func NewSessions(d *db.DB, opt cache.Options) *Sessions {
	return &Sessions{db: d, opt: opt.WithDefaults(), now: time.Now}
}

// sessionHash is the key of a session id: the SHA-256 of the id as it is in the cookie.
func sessionHash(id string) []byte {
	sum := sha256.Sum256([]byte(id))
	return sum[:]
}

func timestamptz(t time.Time) pgtype.Timestamptz { return pgtype.Timestamptz{Time: t, Valid: true} }

// PutSession implements cache.Sessions.
func (s *Sessions) PutSession(ctx context.Context, id string, rec cache.SessionRecord) error {
	if id == "" {
		return fmt.Errorf("identity: empty session id: %w", errs.ErrInvalid)
	}
	now := s.now()
	if rec.CreatedAt.IsZero() {
		rec.CreatedAt = now
	}
	rec.CreatedAt = rec.CreatedAt.UTC()
	ttl := min(s.opt.SessionSliding, s.opt.SessionAbsolute-now.Sub(rec.CreatedAt))
	if ttl < time.Millisecond {
		return fmt.Errorf("identity: session created at %s is past its absolute lifetime: %w", rec.CreatedAt.Format(time.RFC3339), errs.ErrInvalid)
	}
	err := s.db.System(ctx, func(ctx context.Context, tx pgx.Tx) error {
		q := store.New(tx)
		if err := q.DeleteExpiredSessions(ctx, timestamptz(now)); err != nil {
			return fmt.Errorf("delete expired sessions: %w", err)
		}
		return q.UpsertSession(ctx, store.UpsertSessionParams{
			IDHash: sessionHash(id), UserID: rec.UserID, WorkspaceID: rec.WorkspaceID,
			CreatedAt: timestamptz(rec.CreatedAt), ExpiresAt: timestamptz(now.Add(ttl)),
		})
	})
	if err != nil {
		return fmt.Errorf("identity: put session: %w", err)
	}
	return nil
}

// GetSession implements cache.Sessions: one UPDATE that matches a live session and slides it.
func (s *Sessions) GetSession(ctx context.Context, id string) (cache.SessionRecord, error) {
	var row store.TouchSessionRow
	err := s.db.System(ctx, func(ctx context.Context, tx pgx.Tx) (err error) {
		row, err = store.New(tx).TouchSession(ctx, store.TouchSessionParams{
			Now: timestamptz(s.now()), SlidingSecs: s.opt.SessionSliding.Seconds(),
			AbsoluteSecs: s.opt.SessionAbsolute.Seconds(), IDHash: sessionHash(id),
		})
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return cache.SessionRecord{}, fmt.Errorf("identity: session: %w", errs.ErrNotFound)
	}
	if err != nil {
		return cache.SessionRecord{}, fmt.Errorf("identity: get session: %w", err)
	}
	return cache.SessionRecord{UserID: row.UserID, WorkspaceID: row.WorkspaceID, CreatedAt: row.CreatedAt.Time}, nil
}

// DeleteSession implements cache.Sessions; deleting an absent session is not an error.
func (s *Sessions) DeleteSession(ctx context.Context, id string) error {
	err := s.db.System(ctx, func(ctx context.Context, tx pgx.Tx) error {
		return store.New(tx).DeleteSession(ctx, sessionHash(id))
	})
	if err != nil {
		return fmt.Errorf("identity: delete session: %w", err)
	}
	return nil
}
