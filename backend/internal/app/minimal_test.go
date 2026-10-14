package app

import (
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	chatv1 "kiwi/gen/kiwi/chat/v1"
	identityv1 "kiwi/gen/kiwi/identity/v1"
	"kiwi/gen/kiwi/identity/v1/identityv1connect"
	"kiwi/internal/identity"
	"kiwi/internal/platform/config"
	"kiwi/internal/testkit/fakeoidc"
)

// The minimal profile is one process that serves the web app, the API and the WebSocket
// on one port, keeps its tickets and fan-out in memory and its sessions in Postgres, and
// never dials Valkey. These tests run the real composition root that way.

// webDir is a built web app of the shape Vite makes, with a marker in each file.
func webDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range map[string]string{
		"index.html":                "<!doctype html><title>kiwi shell</title>",
		"sw.js":                     "// service worker",
		"manifest.webmanifest":      `{"name":"kiwi"}`,
		"assets/index-abc123.js":    "// bundle",
		"assets/index-abc123.css":   "body{}",
		"icons/icon-192.png":        "\x89PNG",
		"api/looks-like-a-route.js": "// a file under a reserved prefix",
	} {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func fetch(t *testing.T, r *rig, method, path string) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, r.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := (&http.Client{Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}).Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

func TestApp_Minimal_ServesTheWebAppNextToTheAPIOnOnePort(t *testing.T) {
	r := newRig(t, withMemoryCache(), withWebDir(webDir(t)))
	r.start()

	for _, tc := range []struct {
		path        string
		want        int
		contains    string // of the body
		notContains string
		cache       string // Cache-Control, when the case says
	}{
		{"/", 200, "kiwi shell", "", "no-cache"},
		{"/signin", 200, "kiwi shell", "", "no-cache"},
		{"/c/00000000-0000-0000-0000-000000000000", 200, "kiwi shell", "", "no-cache"},
		{"/board", 200, "kiwi shell", "", "no-cache"},
		{"/sw.js", 200, "service worker", "", "no-cache"},
		{"/manifest.webmanifest", 200, `"name"`, "", "no-cache"},
		{"/assets/index-abc123.js", 200, "// bundle", "", "public, max-age=31536000, immutable"},
		{"/assets/missing.js", 404, "", "kiwi shell", ""},
		// Not the app's routes: never the shell.
		{"/nowhere", 404, "", "kiwi shell", ""},
		{"/index-abc123.js", 404, "", "kiwi shell", ""},
		// The other roles' paths are theirs, whatever the web directory holds.
		{"/api/auth/config", 200, "", "kiwi shell", ""},
		{"/api/no-such-route", 404, "", "kiwi shell", ""},
		{"/api/looks-like-a-route.js", 404, "", "a file under a reserved prefix", ""},
		{"/ws", 401, "", "kiwi shell", ""}, // the gateway, which wants a ticket
		{"/mockoidc/authorize", 404, "", "kiwi shell", ""},
		{"/healthz", 200, "ok", "kiwi shell", ""},
		{"/readyz", 200, "ready", "kiwi shell", ""},
	} {
		resp, body := fetch(t, r, http.MethodGet, tc.path)
		if resp.StatusCode != tc.want {
			t.Errorf("GET %s = %d, want %d", tc.path, resp.StatusCode, tc.want)
		}
		if tc.contains != "" && !strings.Contains(body, tc.contains) {
			t.Errorf("GET %s body = %q, want it to contain %q", tc.path, body, tc.contains)
		}
		if tc.notContains != "" && strings.Contains(body, tc.notContains) {
			t.Errorf("GET %s body = %q: it must not contain %q", tc.path, body, tc.notContains)
		}
		if tc.cache != "" && resp.Header.Get("Cache-Control") != tc.cache {
			t.Errorf("GET %s Cache-Control = %q, want %q", tc.path, resp.Header.Get("Cache-Control"), tc.cache)
		}
	}

	// The browser security headers are on the app's own responses.
	resp, _ := fetch(t, r, http.MethodGet, "/")
	for name, want := range map[string]string{"X-Content-Type-Options": "nosniff", "X-Frame-Options": "DENY", "Referrer-Policy": "same-origin"} {
		if got := resp.Header.Get(name); got != want {
			t.Errorf("GET / %s = %q, want %q", name, got, want)
		}
	}
}

// Without KIWI_WEB_DIR kiwi serves no static files: today's behaviour is unchanged.
func TestApp_WithoutAWebDir_ServesNoStaticFiles(t *testing.T) {
	r := newRig(t, withMemoryCache())
	r.start()
	for _, path := range []string{"/", "/signin", "/assets/index-abc123.js"} {
		if resp, _ := fetch(t, r, http.MethodGet, path); resp.StatusCode != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404 with no web directory", path, resp.StatusCode)
		}
	}
}

// A web directory that cannot be served stops the process at start, naming the variable.
func TestApp_AWebDirWithoutAnIndexStopsTheStart(t *testing.T) {
	_, err := New(t.Context(), config.Config{
		Roles: []config.Role{config.RoleRealtime}, DatabaseURL: "postgres://x:y@127.0.0.1:1/kiwi",
		CacheProvider: config.CacheMemory, WebDir: t.TempDir(),
	})
	if err == nil || !strings.Contains(err.Error(), "KIWI_WEB_DIR") {
		t.Fatalf("New with an empty web directory: err = %v, want it to name KIWI_WEB_DIR", err)
	}
}

// With the memory cache the signed-in user, the ticket and the fan-out all work end to
// end (see TestApp_EndToEnd_SendMessageReachesSubscribedMemberOverWebSocket), and a
// redeploy keeps the person signed in: the session is in Postgres.
func TestApp_Minimal_SessionSurvivesARedeploy(t *testing.T) {
	r := newRig(t, withMemoryCache())
	r.start()
	alice := r.signIn(fakeoidc.AliceEmail)
	if n := r.sessionRows(); n != 1 {
		t.Fatalf("%d session rows after one sign-in, want 1", n)
	}

	r.restart() // a new process: nothing in memory carries over

	me, err := alice.identity().GetMe(t.Context(), connect.NewRequest(&identityv1.GetMeRequest{}))
	if err != nil {
		t.Fatalf("GetMe with the cookie from before the redeploy: %v", err)
	}
	if me.Msg.GetUser().GetId() != alice.userID {
		t.Errorf("the session resolves to user %q after the redeploy, want %q", me.Msg.GetUser().GetId(), alice.userID)
	}
	// And the realtime ticket and WebSocket of the new process work for that session.
	if ws, resp, err := alice.dialSocket(""); err != nil {
		t.Errorf("the WebSocket after the redeploy: %v (%v)", err, resp)
	} else {
		_ = ws.c.CloseNow()
	}

	// Signing out ends it for good, redeploy or not.
	logout := func() int {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, r.URL+"/api/auth/logout", nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Origin", r.URL)
		req.AddCookie(&http.Cookie{Name: identity.SessionCookie, Value: alice.cookie})
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		return resp.StatusCode
	}
	if code := logout(); code != http.StatusNoContent {
		t.Fatalf("logout = %d, want 204", code)
	}
	_, err = alice.identity().GetMe(t.Context(), connect.NewRequest(&identityv1.GetMeRequest{}))
	if connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Errorf("GetMe after logout = %v, want unauthenticated", err)
	}
	if n := r.sessionRows(); n != 0 {
		t.Errorf("%d session rows after logout, want 0", n)
	}
}

// What a restart does lose is what lives in the process: a ticket that was not redeemed
// yet. The client asks for another.
func TestApp_Minimal_ATicketDoesNotSurviveARedeploy(t *testing.T) {
	r := newRig(t, withMemoryCache())
	r.start()
	alice := r.signIn(fakeoidc.AliceEmail)
	tk, err := alice.identity().IssueRealtimeTicket(t.Context(), connect.NewRequest(&identityv1.IssueRealtimeTicketRequest{}))
	if err != nil {
		t.Fatal(err)
	}

	r.restart()

	resp, err := http.Get(r.URL + "/ws?ticket=" + tk.Msg.GetTicket())
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("a ticket of the process before the redeploy = %d, want 401", resp.StatusCode)
	}
	if ws, _, err := alice.dialSocket(""); err != nil {
		t.Errorf("a fresh ticket after the redeploy: %v", err)
	} else {
		_ = ws.c.CloseNow()
	}
}

// Without a cache to go stale, a removal from a channel stops the frames at once.
func TestApp_Minimal_RemovedMemberStopsReceivingFrames(t *testing.T) {
	r := newRig(t, withMemoryCache())
	r.start()
	alice, bob := r.signIn(fakeoidc.AliceEmail), r.signIn(fakeoidc.BobEmail)
	channel := alice.newChannel("planning")
	alice.addMember(channel, bob)

	bobWS := bob.socket()
	bobWS.subscribe(channel)
	eventually(t, "bob's subscription to be followed", func() bool { return r.followers("ch:"+channel) >= 1 })
	alice.send(channel, "while bob is in")
	if _, err := bobWS.nextMessage(5 * time.Second); err != nil {
		t.Fatalf("bob, still a member, received nothing: %v", err)
	}

	if _, err := alice.chat().RemoveMember(t.Context(), connect.NewRequest(&chatv1.RemoveMemberRequest{ChannelId: channel, UserId: bob.userID})); err != nil {
		t.Fatalf("RemoveMember: %v", err)
	}
	// Bob is told and dropped from the channel in the gateway.
	var removed bool
	for deadline := time.Now().Add(5 * time.Second); !removed && time.Now().Before(deadline); {
		ev, err := bobWS.next(time.Until(deadline))
		if err != nil {
			t.Fatalf("waiting for the removal frame: %v", err)
		}
		removed = ev.GetMembershipChanged().GetChange() == chatv1.MembershipChange_MEMBERSHIP_CHANGE_REMOVED
	}
	if !removed {
		t.Fatal("bob never received the removal frame")
	}
	// He cannot get back in by subscribing again either: the membership is read from
	// Postgres, with no cache that still lists him. The one timed read is the last use of
	// the socket, since a read that times out closes it.
	bobWS.subscribe(channel)
	alice.send(channel, "after bob left")
	alice.send(channel, "after bob tried to come back")
	if m, err := bobWS.nextMessage(time.Second); err == nil {
		t.Errorf("bob received %v after he was removed", m)
	}
	// Nor can he read the channel.
	_, err := bob.chat().ListMessages(t.Context(), connect.NewRequest(&chatv1.ListMessagesRequest{ChannelId: channel}))
	if connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Errorf("ListMessages by the removed member = %v, want permission denied", err)
	}
}

// The unauthenticated surface of the minimal profile: no session, no data.
func TestApp_Minimal_APIStillWantsASession(t *testing.T) {
	r := newRig(t, withMemoryCache(), withWebDir(webDir(t)))
	r.start()
	anon := identityv1connect.NewIdentityServiceClient(&http.Client{Transport: headerTransport{
		base: http.DefaultTransport, header: http.Header{"Origin": {r.URL}}}}, r.URL+"/api")
	_, err := anon.GetMe(t.Context(), connect.NewRequest(&identityv1.GetMeRequest{}))
	if connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Errorf("GetMe without a session = %v, want unauthenticated", err)
	}
	// A made-up cookie is a session that does not exist.
	forged := identityv1connect.NewIdentityServiceClient(&http.Client{Transport: headerTransport{
		base: http.DefaultTransport, header: http.Header{"Origin": {r.URL}, "Cookie": {identity.SessionCookie + "=" + uuid.NewString()}}}}, r.URL+"/api")
	if _, err := forged.GetMe(t.Context(), connect.NewRequest(&identityv1.GetMeRequest{})); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Errorf("GetMe with a made-up cookie = %v, want unauthenticated", err)
	}
}

// sessionRows counts the sessions in Postgres, as the owner: the table is bound to a
// workspace like the others, so the count is of the rig's workspace.
func (r *rig) sessionRows() int {
	r.t.Helper()
	ctx := r.t.Context()
	conn, err := pgx.Connect(ctx, r.env.OwnerURL)
	if err != nil {
		r.t.Fatalf("connect as owner: %v", err)
	}
	defer func() { _ = conn.Close(ctx) }()
	tx, err := conn.Begin(ctx)
	if err != nil {
		r.t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SELECT set_config('kiwi.workspace_id', $1, true)`, r.ws.String()); err != nil {
		r.t.Fatal(err)
	}
	var n int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM identity.sessions`).Scan(&n); err != nil {
		r.t.Fatal(err)
	}
	return n
}
