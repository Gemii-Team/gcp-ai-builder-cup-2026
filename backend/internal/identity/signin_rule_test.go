package identity

// The sign-in rule for a token without hd (resolveInvitedUser) and the rejections of
// the rule for a token with one (resolveUser), with the helpers they share: an account
// without hd is let in only when a seed or an invitation made exactly one user row for
// its address, and every other outcome writes nothing.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"kiwi/internal/testkit/fakeoidc"
	"kiwi/internal/testkit/fakeoidc/fakeoidctest"
)

// Google appends hd to the callback URL, and so can anybody. See also
// TestNoHD_ForgedHDParameterIsNeverRead_NotInvited: the token is what counts.
func TestCallback_NoHD_UninvitedAccountIsNotInvitedWhateverTheURLSays(t *testing.T) {
	f := newFixture(t)
	f.seedWorkspace(fakeoidc.DefaultDomain)
	before := f.snapshot()

	authURL, bind := f.login("/")
	cb := f.approve(authURL, fakeoidc.CarolEmail)
	q := cb.Query()
	q.Set("hd", fakeoidc.DefaultDomain)
	q.Set("email", fakeoidc.AliceEmail)
	cb.RawQuery = q.Encode()

	f.expectRejected(f.callback(cb, bind), notInvited(fakeoidc.CarolEmail), before)
}

func TestCallback_NoHD_RedirectsWithErrorAndTheVerifiedEmail(t *testing.T) {
	f := newFixture(t)
	f.seedWorkspace(fakeoidc.DefaultDomain)

	authURL, bind := f.login("/")
	cb := f.approve(authURL, fakeoidc.CarolEmail)
	q := cb.Query()
	q.Set("email", "forged@evil.test") // an email in the URL is never read
	cb.RawQuery = q.Encode()

	rec := f.callback(cb, bind)

	want := notInvited(fakeoidc.CarolEmail)
	if got := rec.Header().Get("Location"); got != want {
		t.Fatalf("Location = %q, want %q", got, want)
	}
}

// ---- invite-only sign-in: accounts without hd -------------------------------

// Subjects of the accounts below, long numeric strings like Google's.
const (
	carolSub = "117203948471662035918"
	otherSub = "100000000000000000009"
)

// withUsers serves exactly these accounts, with the default hosted domain.
func withUsers(users ...fakeoidc.User) setting { return withIDP(fakeoidctest.WithUsers(users...)) }

// personal is an account without hd, like a Gmail one.
func personal(email string) fakeoidc.User {
	sum := sha256.Sum256([]byte(email))
	return fakeoidc.User{Email: email, Name: localPart(strings.ToLower(email)), Sub: fmt.Sprintf("%x", sum[:10]), NoHD: true}
}

// requireSessionCookie asserts that rec carries the session cookie with exactly the
// attributes of the sign-in contract (Secure even over http://localhost, HttpOnly,
// SameSite=Lax, Path=/, no Domain) and returns it.
func requireSessionCookie(t *testing.T, rec *httptest.ResponseRecorder) *http.Cookie {
	t.Helper()
	sid := cookieNamed(rec, SessionCookie)
	if sid == nil {
		t.Fatal("no session cookie")
	}
	var raw string
	for _, h := range rec.Result().Header.Values("Set-Cookie") {
		if strings.HasPrefix(h, SessionCookie+"=") {
			raw = h
		}
	}
	if want := SessionCookie + "=" + sid.Value + "; Path=/; Max-Age=2592000; HttpOnly; Secure; SameSite=Lax"; raw != want {
		t.Errorf("Set-Cookie\n got %s\nwant %s", raw, want)
	}
	if sid.Domain != "" {
		t.Errorf("session cookie has Domain=%q", sid.Domain)
	}
	return sid
}

// snapshot is everything a sign-in could write: the workspaces, the stored Google
// grants, every user row as it stands and the keys Valkey holds.
type snapshot struct {
	workspaces, tokens int
	keys               int64
	users              []userRow
}

func (s snapshot) equal(o snapshot) bool {
	return s.workspaces == o.workspaces && s.tokens == o.tokens && s.keys == o.keys && slices.Equal(s.users, o.users)
}

func (f *fixture) snapshot() snapshot {
	f.t.Helper()
	ctx := f.t.Context()
	conn, err := pgx.Connect(ctx, f.env.SystemURL)
	if err != nil {
		f.t.Fatal(err)
	}
	defer func() { _ = conn.Close(ctx) }()
	rows, err := conn.Query(ctx, `SELECT id FROM identity.workspaces`)
	if err != nil {
		f.t.Fatal(err)
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	if err != nil {
		f.t.Fatal(err)
	}
	s := snapshot{workspaces: len(ids), users: f.users(), keys: f.sessionKeys()}
	for _, ws := range ids {
		f.asOwner(ws, func(ctx context.Context, tx pgx.Tx) {
			var n int
			if err := tx.QueryRow(ctx, `SELECT count(*) FROM identity.oauth_tokens`).Scan(&n); err != nil {
				f.t.Fatal(err)
			}
			s.tokens += n
		})
	}
	return s
}

// expectRejected asserts that rec sent the browser to want, set no session cookie and
// left every row and every Valkey key exactly as before was taken.
func (f *fixture) expectRejected(rec *httptest.ResponseRecorder, want string, before snapshot) {
	f.t.Helper()
	if got := rec.Header().Get("Location"); rec.Code != http.StatusFound || got != want {
		f.t.Fatalf("callback = %d -> %q, want %q", rec.Code, got, want)
	}
	if c := cookieNamed(rec, SessionCookie); c != nil {
		f.t.Fatalf("a session cookie was set: %+v", c)
	}
	if after := f.snapshot(); !after.equal(before) {
		f.t.Fatalf("a rejected sign-in changed the database or Valkey:\n before %+v\n  after %+v", before, after)
	}
}

func notInvited(email string) string {
	return "/signin?error=not_invited&email=" + url.QueryEscape(email)
}

// seedNoDomain seeds the domain-less workspace with a placeholder for each address.
func (f *fixture) seedNoDomain(emails ...string) (uuid.UUID, []uuid.UUID) {
	f.t.Helper()
	ws, ids, err := EnsureSeedWorkspace(f.t.Context(), f.db, "", emails)
	if err != nil {
		f.t.Fatalf("EnsureSeedWorkspace without a domain: %v", err)
	}
	return ws, ids
}

// seedElsewhere makes a second workspace with a user for email bound to the Google
// subject sub, straight through kiwi_system: the seed refuses to put one address in
// two workspaces, and a placeholder cannot be there twice (google_sub is unique).
func (f *fixture) seedElsewhere(domain, email, sub string) {
	f.t.Helper()
	ctx := f.t.Context()
	conn, err := pgx.Connect(ctx, f.env.SystemURL)
	if err != nil {
		f.t.Fatal(err)
	}
	defer func() { _ = conn.Close(ctx) }()
	ws := uuid.New()
	if _, err := conn.Exec(ctx, `INSERT INTO identity.workspaces (id, name, google_domain) VALUES ($1, $2, $2)`, ws, domain); err != nil {
		f.t.Fatalf("second workspace: %v", err)
	}
	if _, err := conn.Exec(ctx, `INSERT INTO identity.users (id, workspace_id, google_sub, email, name) VALUES ($1, $2, $3, $4, 'elsewhere')`, uuid.New(), ws, sub, email); err != nil {
		f.t.Fatalf("second placeholder: %v", err)
	}
}

// setSub binds the user to another Google account's subject.
func (f *fixture) setSub(u userRow, sub string) {
	f.t.Helper()
	f.asOwner(u.WorkspaceID, func(ctx context.Context, tx pgx.Tx) {
		if _, err := tx.Exec(ctx, `UPDATE identity.users SET google_sub = $1 WHERE id = $2`, sub, u.ID); err != nil {
			f.t.Fatalf("rebind user: %v", err)
		}
	})
}

// rowLockWaiters counts the database sessions waiting for a row another transaction holds.
func (f *fixture) rowLockWaiters() int {
	f.t.Helper()
	ctx := f.t.Context()
	conn, err := pgx.Connect(ctx, f.env.SystemURL)
	if err != nil {
		f.t.Fatal(err)
	}
	defer func() { _ = conn.Close(ctx) }()
	var n int
	if err := conn.QueryRow(ctx, `SELECT count(*) FROM pg_locks WHERE NOT granted AND locktype IN ('tuple', 'transactionid')`).Scan(&n); err != nil {
		f.t.Fatal(err)
	}
	return n
}

func TestNoHD_UnverifiedEmail_NotInvited(t *testing.T) {
	f := newFixture(t, withUsers(fakeoidc.User{Email: fakeoidc.CarolEmail, Name: fakeoidc.CarolName, Sub: carolSub, NoHD: true, Unverified: true}))
	f.seedNoDomain(fakeoidc.CarolEmail)
	before := f.snapshot()

	rec := f.signIn(fakeoidc.CarolEmail)

	f.expectRejected(rec, notInvited(fakeoidc.CarolEmail), before)
	if u := f.userByEmail(fakeoidc.CarolEmail); u.Sub != "seed:"+fakeoidc.CarolEmail {
		t.Errorf("the placeholder = %+v, want it still unbound", u)
	}
}

func TestNoHD_UninvitedAddress_NotInvited_CreatesNothing(t *testing.T) {
	const invited = "carol.smith@gmail.test"
	accounts := []fakeoidc.User{
		personal("carolsmith@gmail.test"),      // Gmail ignores the dot, K.I.W.I. does not
		personal("carol.smith+tag@gmail.test"), // nor the +tag
		personal("stranger@gmail.test"),
	}
	f := newFixture(t, withUsers(accounts...))
	f.seedNoDomain(invited) // the invitee never signs in here
	f.seedWorkspace(fakeoidc.DefaultDomain)
	for _, a := range accounts {
		before := f.snapshot()
		f.expectRejected(f.signIn(a.Email), notInvited(a.Email), before)
	}

	// With no workspace at all, a miss does not make one.
	g := newFixture(t, withUsers(accounts...))
	before := g.snapshot()
	g.expectRejected(g.signIn("stranger@gmail.test"), notInvited("stranger@gmail.test"), before)
	if n := g.workspaceCount(); n != 0 {
		t.Errorf("workspaces = %d, want none: sign-in never creates a workspace", n)
	}
}

func TestNoHD_InvitedAddress_SignsInAndAdoptsPlaceholder(t *testing.T) {
	f := newFixture(t)
	ws, ids := f.seedNoDomain(fakeoidc.CarolEmail)
	before := f.snapshot()

	authURL, bind := f.login("/channels/general")
	rec := f.callback(f.approve(authURL, fakeoidc.CarolEmail), bind)

	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/channels/general" {
		t.Fatalf("callback = %d -> %q, want a redirect to return_to", rec.Code, rec.Header().Get("Location"))
	}
	sid := requireSessionCookie(t, rec)
	if c := cookieNamed(rec, stateCookie); c == nil || c.MaxAge >= 0 {
		t.Errorf("binding cookie after sign-in = %+v, want it deleted", c)
	}

	users := f.users()
	if len(users) != 1 {
		t.Fatalf("users = %+v, want the placeholder adopted, not duplicated", users)
	}
	u := users[0]
	if u.ID != ids[0] || u.WorkspaceID != ws || u.Sub != carolSub || u.Email != fakeoidc.CarolEmail || u.Name != fakeoidc.CarolName {
		t.Errorf("user = %+v, want %s in %s with the token's sub %s and Google's name", u, ids[0], ws, carolSub)
	}
	if after := f.snapshot(); after.workspaces != before.workspaces || len(after.users) != len(before.users) || after.tokens != before.tokens+1 {
		t.Errorf("rows before %+v, after %+v: want no new workspace or user and one stored grant", before, after)
	}
	rt := f.idp.RefreshToken(fakeoidc.CarolEmail)
	enc, _, found := f.oauthRow(u)
	if !found || rt == "" || bytes.Contains(enc, []byte(rt)) {
		t.Fatalf("stored grant: found %v, refresh token %q: want it stored and not in clear", found, rt)
	}
	if got, err := f.vault().Decrypt(enc); err != nil || string(got) != rt {
		t.Errorf("decrypted = %q, %v; want %q", got, err, rt)
	}
	if sess, err := f.mod.Session(t.Context(), sid.Value); err != nil || sess.UserID != ids[0] || sess.WorkspaceID != ws {
		t.Errorf("session = %+v, %v; want it bound to the adopted user %s", sess, err, ids[0])
	}

	// The account is known by its sub now, and comes back by it.
	f.mustSignIn(fakeoidc.CarolEmail)
	if again := f.users(); len(again) != 1 || again[0] != u {
		t.Fatalf("users after the second sign-in = %+v, want %+v unchanged", again, u)
	}

	// The address belongs to one Google account: a token of another one for it is turned away.
	f.setSub(u, otherSub)
	before = f.snapshot()
	f.expectRejected(f.signIn(fakeoidc.CarolEmail), notInvited(fakeoidc.CarolEmail), before)
	if got := f.userByEmail(fakeoidc.CarolEmail).Sub; got != otherSub {
		t.Errorf("the row is bound to %q after the rejection, want %q", got, otherSub)
	}
}

func TestNoHD_MixedCaseAddress_MatchesLowercasedRow(t *testing.T) {
	const (
		invited = "MIXED.case@GMAIL.com"
		refused = "Other.PERSON@Gmail.COM"
	)
	f := newFixture(t, withUsers(personal(invited), personal(refused)))
	_, ids, err := EnsureSeedWorkspace(t.Context(), f.db, "", []string{"Mixed.Case@Gmail.com"})
	if err != nil {
		t.Fatal(err)
	}
	if u := f.users(); len(u) != 1 || u[0].Email != "mixed.case@gmail.com" || u[0].Sub != "seed:mixed.case@gmail.com" {
		t.Fatalf("seeded = %+v, want the address lowercased", u)
	}

	sid := f.mustSignIn(invited)

	u := f.users()
	if len(u) != 1 || u[0].ID != ids[0] || u[0].Email != "mixed.case@gmail.com" || u[0].Sub == "seed:mixed.case@gmail.com" {
		t.Errorf("users = %+v, want the one row adopted and still lowercased", u)
	}
	if sess, err := f.mod.Session(t.Context(), sid.Value); err != nil || sess.UserID != ids[0] {
		t.Errorf("session = %+v, %v; want the seeded user", sess, err)
	}

	before := f.snapshot()
	f.expectRejected(f.signIn(refused), notInvited("other.person@gmail.com"), before)
}

func TestCallback_HD_UnknownDomain_DomainNotAllowed_CreatesNothing(t *testing.T) {
	// Alice's token names hd=other.example, which has no workspace. The callback
	// request claims hd=acme.test, a workspace that exists, and Alice's address is
	// invited in the domain-less workspace: a token with hd is judged by its hd alone.
	f := newFixture(t, withIDP(realSubs(), fakeoidctest.WithHostedDomain("other.example")))
	f.seedWorkspace(fakeoidc.DefaultDomain)
	f.seedNoDomain(fakeoidc.AliceEmail)
	before := f.snapshot()

	authURL, bind := f.login("/")
	cb := f.approve(authURL, fakeoidc.AliceEmail)
	q := cb.Query()
	q.Set("hd", fakeoidc.DefaultDomain)
	cb.RawQuery = q.Encode()
	rec := f.callback(cb, bind)

	f.expectRejected(rec, "/signin?error=domain_not_allowed&email="+url.QueryEscape(fakeoidc.AliceEmail), before)
}

func TestNoHD_ForgedHDParameterIsNeverRead_NotInvited(t *testing.T) {
	f := newFixture(t)
	f.seedWorkspace(fakeoidc.DefaultDomain)
	before := f.snapshot()

	authURL, bind := f.login("/")
	cb := f.approve(authURL, fakeoidc.CarolEmail)
	// Google appends hd to the callback URL. It is a request parameter anyone can
	// write, so the token's own claim is the only thing that counts.
	q := cb.Query()
	q.Set("hd", fakeoidc.DefaultDomain)
	cb.RawQuery = q.Encode()

	f.expectRejected(f.callback(cb, bind), notInvited(fakeoidc.CarolEmail), before)
}

// An address in two workspaces is ambiguous. One row is an invitation, the other is
// already bound to the token's own subject, so taking whichever row comes first
// would let this account in.
func TestNoHD_AddressInTwoWorkspaces_NotInvited(t *testing.T) {
	f := newFixture(t)
	f.seedNoDomain(fakeoidc.CarolEmail)
	f.seedElsewhere("second.example", fakeoidc.CarolEmail, carolSub)
	before := f.snapshot()

	rec := f.signIn(fakeoidc.CarolEmail)

	// The invitation in the domain-less workspace was not adopted, the user already
	// bound to this very account in the second one was not signed in (either would
	// do for an implementation that took the first row), and the answer is the one an
	// uninvited address gets.
	f.expectRejected(rec, notInvited(fakeoidc.CarolEmail), before)
}

// The token's address is an unadopted placeholder, but its subject is already the
// user of another row (the same Google account seeded under an old and a new
// address, or one whose primary address changed). The adoption would bind the
// subject twice, which google_sub being unique refuses: that is the uninvited
// answer, not a failure, and nothing is written, not even as a new user.
func TestNoHD_SubBoundToAnotherRow_NotInvited(t *testing.T) {
	const oldAddress = "carol.old@gmail.test"
	f := newFixture(t)
	f.seedNoDomain(fakeoidc.CarolEmail)
	f.seedElsewhere("second.example", oldAddress, carolSub)
	before := f.snapshot()
	var logs bytes.Buffer
	f.mod.log = slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))

	rec := f.signIn(fakeoidc.CarolEmail)

	f.expectRejected(rec, notInvited(fakeoidc.CarolEmail), before)
	if got := f.userByEmail(fakeoidc.CarolEmail).Sub; got != "seed:"+fakeoidc.CarolEmail {
		t.Errorf("the placeholder is bound to %q, want it untouched", got)
	}
	if strings.Contains(logs.String(), `"level":"ERROR"`) || strings.Contains(logs.String(), "sign-in failed") {
		t.Errorf("the rejection was logged as a failure: %s", logs.String())
	}
}

// A token with hd whose email is not verified is turned away as a failed exchange,
// before any rule is applied: the verifier rejects it with zero claims, so there is
// no address to name and no row is read or written.
func TestCallback_HD_UnverifiedEmail_ExchangeFailed_CreatesNothing(t *testing.T) {
	f := newFixture(t, withUsers(fakeoidc.User{Email: fakeoidc.AliceEmail, Name: fakeoidc.AliceName, Sub: aliceSub, Unverified: true}))
	f.seedWorkspace(fakeoidc.DefaultDomain)
	before := f.snapshot()

	f.expectRejected(f.signIn(fakeoidc.AliceEmail), "/signin?error=exchange_failed", before)
}

// A sign-in that read the placeholder and then lost it to another account is turned
// away; it never inserts a user of its own. The test holds the placeholder's row
// lock, lets the callback queue behind it at the adoption, binds the row to another
// subject and only then lets go.
func TestNoHD_LostAdoptionRace_RejectsAndNeverInserts(t *testing.T) {
	f := newFixture(t)
	ws, ids := f.seedNoDomain(fakeoidc.CarolEmail)
	authURL, bind := f.login("/")
	cb := f.approve(authURL, fakeoidc.CarolEmail)

	ctx := t.Context()
	conn, err := pgx.Connect(ctx, f.env.OwnerURL)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close(ctx) }()
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SELECT set_config('kiwi.workspace_id', $1, true)`, ws.String()); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `SELECT id FROM identity.users WHERE id = $1 FOR UPDATE`, ids[0]); err != nil {
		t.Fatal(err)
	}

	result := make(chan *httptest.ResponseRecorder, 1)
	go func() { result <- f.callback(cb, bind) }()

	deadline := time.Now().Add(15 * time.Second)
	for f.rowLockWaiters() < 1 {
		select {
		case rec := <-result:
			t.Fatalf("the sign-in finished without waiting for the placeholder: %q", rec.Header().Get("Location"))
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("the sign-in never reached the adoption of the placeholder")
		}
		time.Sleep(20 * time.Millisecond)
	}

	if _, err := tx.Exec(ctx, `UPDATE identity.users SET google_sub = $1 WHERE id = $2`, otherSub, ids[0]); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	select {
	case rec := <-result:
		if got := rec.Header().Get("Location"); rec.Code != http.StatusFound || got != notInvited(fakeoidc.CarolEmail) {
			t.Fatalf("callback = %d -> %q, want %q", rec.Code, got, notInvited(fakeoidc.CarolEmail))
		}
		if cookieNamed(rec, SessionCookie) != nil {
			t.Error("a session cookie was set")
		}
	case <-time.After(15 * time.Second):
		t.Fatal("the sign-in did not finish after the lock was released")
	}
	if users := f.users(); len(users) != 1 || users[0].ID != ids[0] || users[0].Sub != otherSub {
		t.Errorf("users = %+v, want the one row, bound to the account that won it", users)
	}
	if s := f.snapshot(); s.tokens != 0 || s.workspaces != 1 || s.keys != 0 {
		t.Errorf("after the lost race: %d grants, %d workspaces, %d Valkey keys; want none, 1 and none", s.tokens, s.workspaces, s.keys)
	}
}

func TestCallback_RejectionReasonIsLoggedAtInfoWithoutTheAddress(t *testing.T) {
	const (
		unverified = "unverified@gmail.test"
		stranger   = "stranger@gmail.test"
		twice      = "twice@gmail.test"
		taken      = "taken@gmail.test"
		bound      = "bound@gmail.test"
	)
	// Alice's token carries hd=acme.test, and no workspace has that domain.
	f := newFixture(t, withUsers(
		fakeoidc.User{Email: fakeoidc.AliceEmail, Name: fakeoidc.AliceName, Sub: aliceSub},
		fakeoidc.User{Email: unverified, Name: "u", Sub: "1001", NoHD: true, Unverified: true},
		personal(stranger), personal(twice), personal(taken), personal(bound),
	))
	f.seedNoDomain(unverified, twice, taken, bound)
	f.seedElsewhere("second.example", twice, personal(twice).Sub)
	f.setSub(f.userByEmail(taken), otherSub)
	// bound's own Google account is already the user of another address in a third workspace.
	f.seedElsewhere("third.example", "bound.old@gmail.test", personal(bound).Sub)

	var logs bytes.Buffer
	f.mod.log = slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelInfo}))
	for _, tc := range []struct{ email, code, reason string }{
		{unverified, CodeNotInvited, "unverified"},
		{stranger, CodeNotInvited, "uninvited"},
		{twice, CodeNotInvited, "ambiguous"},
		{taken, CodeNotInvited, "bound_to_other_subject"},
		{bound, CodeNotInvited, "sub_bound_elsewhere"},
		{fakeoidc.AliceEmail, CodeDomainNotAllowed, "unknown_domain"},
	} {
		logs.Reset()
		f.signIn(tc.email)

		var found bool
		dec := json.NewDecoder(bytes.NewReader(logs.Bytes()))
		for dec.More() {
			var rec map[string]any
			if err := dec.Decode(&rec); err != nil {
				t.Fatalf("%s: log line: %v", tc.email, err)
			}
			if rec["msg"] != "sign-in rejected" {
				continue
			}
			found = true
			if rec["level"] != "INFO" || rec["code"] != tc.code || rec["reason"] != tc.reason {
				t.Errorf("%s: log record %v, want INFO with code %s and reason %s", tc.email, rec, tc.code, tc.reason)
			}
		}
		if !found {
			t.Errorf("%s: no \"sign-in rejected\" record in %s", tc.email, logs.String())
		}
		if strings.Contains(strings.ToLower(logs.String()), strings.ToLower(tc.email)) {
			t.Errorf("%s: the log carries the address: %s", tc.email, logs.String())
		}
	}
}
