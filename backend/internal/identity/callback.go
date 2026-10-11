package identity

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"kiwi/internal/identity/internal/store"
	"kiwi/internal/platform/db"
	"kiwi/internal/platform/errs"
	"kiwi/internal/platform/oidc"
)

// This file is the identity domain's only db.System caller (see the allowlist in
// platform/db). kiwi_system bypasses row-level security but holds just SELECT and
// INSERT on identity.workspaces and SELECT, INSERT and UPDATE on identity.users, so
// nothing here can read or change another table. Everything that can run inside a
// workspace does, as kiwi_app.

// The codes of the sign-in page's error parameter.
const (
	CodeStateMismatch    = "state_mismatch"
	CodeDomainNotAllowed = "domain_not_allowed"
	CodeAccessDenied     = "access_denied"
	CodeExchangeFailed   = "exchange_failed"
)

// CallbackError is a sign-in the callback turned away. Code is one of the Code*
// constants. Email is set only for CodeDomainNotAllowed, taken from the verified
// token, so the sign-in page can name the account that was refused.
type CallbackError struct {
	Code  string
	Email string
	Err   error
}

// Error implements error. It leaves the email out so it stays out of logs.
func (e *CallbackError) Error() string {
	if e.Err == nil {
		return "identity: sign-in rejected: " + e.Code
	}
	return "identity: sign-in rejected: " + e.Code + ": " + e.Err.Error()
}

// Unwrap returns the cause.
func (e *CallbackError) Unwrap() error { return e.Err }

// errDomainNotAllowed means the token's hosted domain has no workspace here, or
// its subject belongs to a user of another workspace.
var errDomainNotAllowed = errors.New("identity: the hosted domain has no workspace for this account")

// resolveUser finds the user a verified ID token stands for, creating it on the
// first sign-in. The workspace is the one whose google_domain is the token's hd
// (never created here: a domain without a workspace is errDomainNotAllowed); the
// user is the one with the token's sub, else the placeholder a seed left under the
// same email, else a new row. It also refreshes the user's email, name and avatar
// from the token.
func resolveUser(ctx context.Context, d *db.DB, c oidc.Claims) (store.IdentityUser, error) {
	email := normalizeEmail(c.Email)
	name := cmp.Or(c.Name, localPart(email))

	var user store.IdentityUser
	err := d.System(ctx, func(ctx context.Context, tx pgx.Tx) error {
		q := store.New(tx)
		if err := q.LockSignIn(ctx, c.Sub); err != nil {
			return fmt.Errorf("lock sign-in: %w", err)
		}
		ws, err := q.GetWorkspaceByDomain(ctx, normalizeDomain(c.HD))
		if errors.Is(err, pgx.ErrNoRows) {
			return errDomainNotAllowed
		}
		if err != nil {
			return fmt.Errorf("find workspace: %w", err)
		}

		existing, err := q.GetUserBySub(ctx, c.Sub)
		switch {
		case err == nil:
			if existing.WorkspaceID != ws.ID {
				return errDomainNotAllowed
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

// EnsureSeedWorkspace resolves the workspace of domain, creating it (named after
// the domain) if it does not exist, and makes sure each email has a user in it,
// creating a placeholder with google_sub = "seed:" + email and the local part of the
// address as its name. It returns the workspace id and the user ids in the order of
// emails. Running it again changes nothing: workspaces are unique by domain, users
// by (workspace, email), and a placeholder the sign-in has adopted keeps its id.
// The first real sign-in of a placeholder's email completes the user (resolveUser).
func EnsureSeedWorkspace(ctx context.Context, d *db.DB, domain string, emails []string) (workspaceID uuid.UUID, userIDs []uuid.UUID, err error) {
	domain = normalizeDomain(domain)
	if domain == "" {
		return uuid.Nil, nil, fmt.Errorf("identity: seed workspace: an empty domain: %w", errs.ErrInvalid)
	}
	norm := make([]string, len(emails))
	for i, e := range emails {
		norm[i] = normalizeEmail(e)
		if local, host, ok := strings.Cut(norm[i], "@"); !ok || local == "" || host == "" {
			return uuid.Nil, nil, fmt.Errorf("identity: seed workspace: %q is not an email address: %w", e, errs.ErrInvalid)
		}
	}

	ids := make([]uuid.UUID, len(norm))
	err = d.System(ctx, func(ctx context.Context, tx pgx.Tx) error {
		q := store.New(tx)
		wsID, err := uuid.NewV7()
		if err != nil {
			return fmt.Errorf("workspace id: %w", err)
		}
		// identity.workspaces.name is NOT NULL: the domain names the workspace.
		if err := q.InsertWorkspaceIfAbsent(ctx, store.InsertWorkspaceIfAbsentParams{ID: wsID, Name: domain, GoogleDomain: domain}); err != nil {
			return fmt.Errorf("create workspace: %w", err)
		}
		ws, err := q.GetWorkspaceByDomain(ctx, domain)
		if err != nil {
			return fmt.Errorf("find workspace: %w", err)
		}
		workspaceID = ws.ID

		for i, email := range norm {
			userID, err := uuid.NewV7()
			if err != nil {
				return fmt.Errorf("user id: %w", err)
			}
			if err := q.InsertSeedUserIfAbsent(ctx, store.InsertSeedUserIfAbsentParams{ID: userID, WorkspaceID: ws.ID, Email: email, Name: localPart(email)}); err != nil {
				return fmt.Errorf("create placeholder for %s: %w", email, err)
			}
			u, err := q.GetUserByEmail(ctx, store.GetUserByEmailParams{WorkspaceID: ws.ID, Email: email})
			if err != nil {
				return fmt.Errorf("find user %s: %w", email, err)
			}
			ids[i] = u.ID
		}
		return nil
	})
	if err != nil {
		return uuid.Nil, nil, fmt.Errorf("identity: seed workspace %s: %w", domain, err)
	}
	return workspaceID, ids, nil
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

func normalizeEmail(s string) string  { return strings.ToLower(strings.TrimSpace(s)) }
func normalizeDomain(s string) string { return strings.ToLower(strings.TrimSpace(s)) }

// localPart is the name a user has until Google supplies one.
func localPart(email string) string {
	local, _, _ := strings.Cut(email, "@")
	return local
}
