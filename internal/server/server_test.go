package server

import (
	"encoding/json"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/TheWarBoys2/pzadmin/internal/config"
)

func newTestApp(t *testing.T) (*App, http.Handler, string) {
	t.Helper()
	dir := t.TempDir()
	assets := fstest.MapFS{
		"index.html": &fstest.MapFile{Data: []byte("<html>app</html>")},
		"app.js":     &fstest.MapFile{Data: []byte("// app")},
	}
	var sub fs.FS = assets
	app, err := New(Options{DataDir: dir, Assets: sub})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(app.Close)
	return app, app.Handler(), dir
}

type client struct {
	t       *testing.T
	handler http.Handler
	cookies []*http.Cookie
	csrf    string
}

func (c *client) do(method, path string, body any) *httptest.ResponseRecorder {
	c.t.Helper()
	var reader *strings.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		reader = strings.NewReader(string(b))
	} else {
		reader = strings.NewReader("")
	}
	req := httptest.NewRequest(method, path, reader)
	for _, ck := range c.cookies {
		req.AddCookie(ck)
	}
	if method == http.MethodPost && c.csrf != "" {
		req.Header.Set("X-CSRF-Token", c.csrf)
	}
	rec := httptest.NewRecorder()
	c.handler.ServeHTTP(rec, req)
	if ck := rec.Result().Cookies(); len(ck) > 0 {
		c.cookies = append(c.cookies, ck...)
		for _, k := range ck {
			if k.Name == csrfCookie && k.Value != "" {
				c.csrf = k.Value
			}
		}
	}
	return rec
}

func (c *client) setup(username, password string) {
	c.t.Helper()
	rec := c.do(http.MethodPost, "/api/setup", map[string]string{
		"username": username, "password": password, "timezone": "UTC",
	})
	if rec.Code != http.StatusOK {
		c.t.Fatalf("setup failed: %d %s", rec.Code, rec.Body.String())
	}
}

func decode(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("bad JSON (%d): %s", rec.Code, rec.Body.String())
	}
	return out
}

func TestUnauthenticatedRequestsAreRefused(t *testing.T) {
	_, handler, _ := newTestApp(t)
	for _, path := range []string{"/api/state", "/api/events", "/api/players", "/api/browse", "/api/sessions"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s returned %d, expected 401", path, rec.Code)
		}
	}
}

func TestSetupThenLoginFlow(t *testing.T) {
	_, handler, _ := newTestApp(t)
	c := &client{t: t, handler: handler}

	// Weak passwords must be refused outright.
	rec := c.do(http.MethodPost, "/api/setup", map[string]string{"username": "rick", "password": "short"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("a five character password should be refused, got %d", rec.Code)
	}

	c.setup("rick", "a-long-enough-password")
	rec = c.do(http.MethodGet, "/api/state", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("setup should sign the operator in, got %d", rec.Code)
	}

	// Setup cannot be run twice, which would otherwise let anyone reset the
	// account by reaching the endpoint before the real owner.
	rec = c.do(http.MethodPost, "/api/setup", map[string]string{"username": "mallory", "password": "another-long-password"})
	if rec.Code != http.StatusConflict {
		t.Fatalf("second setup should be refused, got %d", rec.Code)
	}
}

func TestCSRFTokenIsRequiredForMutations(t *testing.T) {
	_, handler, _ := newTestApp(t)
	c := &client{t: t, handler: handler}
	c.setup("rick", "a-long-enough-password")

	// The browser will send cookies on a cross-site POST; it cannot set the
	// header. Dropping the header must fail the request.
	saved := c.csrf
	c.csrf = ""
	rec := c.do(http.MethodPost, "/api/settings", map[string]any{"timezone": "UTC"})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("a POST without the CSRF header must be refused, got %d", rec.Code)
	}

	c.csrf = "wrong-value"
	rec = c.do(http.MethodPost, "/api/settings", map[string]any{"timezone": "UTC"})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("a POST with the wrong CSRF token must be refused, got %d", rec.Code)
	}

	c.csrf = saved
	rec = c.do(http.MethodPost, "/api/settings", map[string]any{"timezone": "UTC"})
	if rec.Code != http.StatusOK {
		t.Fatalf("a correct CSRF token should be accepted, got %d %s", rec.Code, rec.Body.String())
	}
}

func TestCrossSiteFetchIsRefused(t *testing.T) {
	_, handler, _ := newTestApp(t)
	c := &client{t: t, handler: handler}
	c.setup("rick", "a-long-enough-password")

	req := httptest.NewRequest(http.MethodPost, "/api/settings", strings.NewReader(`{"timezone":"UTC"}`))
	for _, ck := range c.cookies {
		req.AddCookie(ck)
	}
	req.Header.Set("X-CSRF-Token", c.csrf)
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("a cross-site request must be refused even with a valid token, got %d", rec.Code)
	}
}

func TestLoginRateLimiting(t *testing.T) {
	_, handler, _ := newTestApp(t)
	c := &client{t: t, handler: handler}
	c.setup("rick", "a-long-enough-password")

	limited := false
	for i := 0; i < 12; i++ {
		rec := c.do(http.MethodPost, "/api/login", map[string]string{"username": "rick", "password": "wrong"})
		if rec.Code == http.StatusTooManyRequests {
			limited = true
			break
		}
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("unexpected status %d", rec.Code)
		}
	}
	if !limited {
		t.Fatal("repeated failed logins must eventually be rate limited")
	}
}

// The old build shipped RCON passwords to the browser on every poll and in the
// export endpoint. Nothing that leaves the process may contain one.
func TestSecretsNeverReachTheClient(t *testing.T) {
	app, handler, _ := newTestApp(t)
	c := &client{t: t, handler: handler}
	c.setup("rick", "a-long-enough-password")

	const secret = "sup3r-secret-rcon-value"
	seedServer(t, app, `{"id":"k","name":"Riverside","host":"127.0.0.1","rconPort":27015,
	  "rconPassword":"`+secret+`","enabled":false}`)
	rec := c.do(http.MethodPost, "/api/server/save", map[string]any{
		"id": "k", "name": "Riverside", "enabled": false,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("save failed: %d %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), secret) {
		t.Fatal("the save response echoed the RCON password back")
	}

	_, err := app.cfg.Update(func(cfg *config.Config) error {
		cfg.Notify.WebhookURL = "https://discord.example/hook/abc"
		cfg.Metrics.Token = "metrics-token-value"
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	for _, path := range []string{"/api/state", "/api/export"} {
		rec := c.do(http.MethodGet, path, nil)
		body := rec.Body.String()
		for _, leak := range []string{secret, "discord.example", "metrics-token-value"} {
			if strings.Contains(body, leak) {
				t.Fatalf("%s leaked %q", path, leak)
			}
		}
	}

	// And it is genuinely still stored server-side.
	srv := app.cfg.Get().Servers[0]
	if srv.RCONPassword != secret {
		t.Fatal("the password should be persisted even though it is never sent out")
	}
}

// Everything discovery reads from the stack folder belongs to the files. An
// edit from the browser changes the operator's own settings and nothing else,
// whatever the payload carries.
func TestEditingAServerCannotOverrideDiscoveredFields(t *testing.T) {
	app, handler, _ := newTestApp(t)
	c := &client{t: t, handler: handler}
	c.setup("rick", "a-long-enough-password")

	seedServer(t, app, `{"id":"k","name":"Riverside","host":"host.docker.internal","rconPort":27101,
	  "rconPassword":"original","dockerContainer":"pz-riverside","pzPath":"/srv/zomboid/riverside/projectzomboid",
	  "stack":"riverside","enabled":true}`)

	rec := c.do(http.MethodPost, "/api/server/save", map[string]any{
		"id": "k", "name": "Renamed", "host": "evil.example", "rconPort": 1,
		"rconPassword": "", "dockerContainer": "portainer", "pzPath": "/etc", "enabled": false,
		"recovery": map[string]any{"enabled": true, "failuresBeforeRestart": 7, "cooldownMinutes": 5},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("edit failed: %s", rec.Body.String())
	}
	got := app.cfg.Get().Servers[0]
	if got.Name != "Renamed" || got.Enabled || got.Recovery.FailuresBeforeRestart != 7 {
		t.Fatalf("operator settings did not stick: %+v", got)
	}
	if got.Host != "host.docker.internal" || got.RCONPort != 27101 || got.RCONPassword != "original" ||
		got.DockerContainer != "pz-riverside" || got.PZPath != "/srv/zomboid/riverside/projectzomboid" {
		t.Fatalf("a discovered field was overwritten: %+v", got)
	}
}

func TestServersCannotBeAddedByHand(t *testing.T) {
	_, handler, _ := newTestApp(t)
	c := &client{t: t, handler: handler}
	c.setup("rick", "a-long-enough-password")
	rec := c.do(http.MethodPost, "/api/server/save", map[string]any{
		"name": "Manual", "host": "127.0.0.1", "rconPort": 27015, "rconPassword": "x",
	})
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "stack folders") {
		t.Fatalf("got %d %s", rec.Code, rec.Body.String())
	}
}

func TestServerPathIsConfinedToTheRoot(t *testing.T) {
	app, handler, dir := newTestApp(t)
	c := &client{t: t, handler: handler}
	c.setup("rick", "a-long-enough-password")

	root := filepath.Join(dir, "pzroot")
	if err := os.MkdirAll(filepath.Join(root, "srv"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := app.cfg.Update(func(cfg *config.Config) error { cfg.PZRoot = root; return nil }); err != nil {
		t.Fatal(err)
	}

	rec := c.do(http.MethodPost, "/api/server/save", map[string]any{
		"name": "Escape", "host": "127.0.0.1", "rconPort": 27015,
		"rconPassword": "x", "pzPath": "../../../etc", "enabled": false,
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("a path outside the root must be refused, got %d %s", rec.Code, rec.Body.String())
	}

	for _, path := range []string{"/api/browse?path=../..", "/api/browse?path=/etc"} {
		rec := c.do(http.MethodGet, path, nil)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s should be refused, got %d", path, rec.Code)
		}
	}
	if rec := c.do(http.MethodGet, "/api/browse?path=.", nil); rec.Code != http.StatusOK {
		t.Fatalf("browsing the root itself should work, got %d %s", rec.Code, rec.Body.String())
	}
}

func TestCommandBuilderRejectsBadArgumentsWithoutPanicking(t *testing.T) {
	// Every one of these panicked with an index-out-of-range in the previous
	// implementation, because it indexed args directly.
	cases := []struct {
		id   string
		args []string
	}{
		{"voiceban", []string{"Rick"}},
		// The toggles need a player and nothing else, so the missing-argument
		// case for them is an empty list.
		{"godmodeplayer", nil},
		{"invisibleplayer", nil},
		{"noclip", []string{}},
		{"createhorde", nil},
		{"createhorde2", []string{"1", "2", "3"}},
		{"teleportplayer", []string{"Rick"}},
		{"releasesafehouse", nil},
		{"banip", []string{"not-an-ip"}},
		{"additem", []string{"Rick"}},
		{"addxp", []string{"Rick", "Woodwork"}},
		{"setaccesslevel", []string{"Rick"}},
		{"changeoption", []string{"PVP"}},
	}
	for _, tc := range cases {
		cmd, found := LookupCommand(tc.id)
		if !found {
			t.Fatalf("command %q is missing from the catalogue", tc.id)
		}
		out, err := cmd.Build(tc.args)
		if err == nil {
			t.Fatalf("%s(%v) should have returned an error, produced %q", tc.id, tc.args, out)
		}
	}
}

func TestCommandBuilderQuotesAndValidates(t *testing.T) {
	cases := []struct {
		id   string
		args []string
		want string
	}{
		{"save", nil, "save"},
		{"servermsg", []string{`say "hi"`}, `servermsg "say \"hi\""`},
		{"kickuser", []string{"Rick", "afk"}, `kickuser "Rick" -r "afk"`},
		{"kickuser", []string{"Rick"}, `kickuser "Rick"`},
		{"godmodeplayer", []string{"Rick", "true"}, `godmodeplayer "Rick" -true`},
		{"additem", []string{"Rick", "Base.Axe", "3"}, `additem "Rick" "Base.Axe" 3`},
		{"additem", []string{"Rick", "Base.Axe"}, `additem "Rick" "Base.Axe"`},
		{"createhorde", []string{"20", "Rick"}, `createhorde 20 "Rick"`},
		{"startrain", nil, "startrain"},
		{"startrain", []string{"50"}, "startrain 50"},
	}
	for _, tc := range cases {
		cmd, _ := LookupCommand(tc.id)
		got, err := cmd.Build(tc.args)
		if err != nil {
			t.Fatalf("%s(%v): %v", tc.id, tc.args, err)
		}
		if got != tc.want {
			t.Fatalf("%s(%v) = %q, want %q", tc.id, tc.args, got, tc.want)
		}
	}

	// A player name containing a quote must not be able to inject a second
	// command argument.
	cmd, _ := LookupCommand("kickuser")
	got, _ := cmd.Build([]string{`Rick" -r "pwned`})
	if strings.Count(got, `-r`) != 1 {
		t.Fatalf("quote injection through a player name: %s", got)
	}
}

func TestNumericArgumentsAreValidated(t *testing.T) {
	cmd, _ := LookupCommand("addxp")
	if _, err := cmd.Build([]string{"Rick", "Woodwork", "lots"}); err == nil {
		t.Fatal("a non-numeric XP value should be refused")
	}
	cmd, _ = LookupCommand("godmodeplayer")
	if _, err := cmd.Build([]string{"Rick", "maybe"}); err == nil {
		t.Fatal("a non-boolean toggle should be refused")
	}
}

func TestValidateRawCommandBlocksMultiline(t *testing.T) {
	if _, err := ValidateRawCommand("players\nquit"); err == nil {
		t.Fatal("two commands in one line should be refused")
	}
	if _, err := ValidateRawCommand("   "); err == nil {
		t.Fatal("an empty command should be refused")
	}
	got, err := ValidateRawCommand("  players  ")
	if err != nil || got != "players" {
		t.Fatalf("got %q %v", got, err)
	}
}

func TestScheduleValidationRejectsBrokenTasks(t *testing.T) {
	app, handler, _ := newTestApp(t)
	c := &client{t: t, handler: handler}
	c.setup("rick", "a-long-enough-password")
	seedServer(t, app, `{"name":"Riverside","host":"127.0.0.1","rconPort":27015,"rconPassword":"x","enabled":false}`)
	id := app.cfg.Get().Servers[0].ID

	bad := []map[string]any{
		{"name": "", "serverId": id, "kind": "save", "cron": "0 4 * * *"},
		{"name": "Bad cron", "serverId": id, "kind": "save", "cron": "not a cron"},
		{"name": "Unknown server", "serverId": "nope", "kind": "save", "cron": "0 4 * * *"},
		{"name": "Unknown kind", "serverId": id, "kind": "explode", "cron": "0 4 * * *"},
		{"name": "Empty broadcast", "serverId": id, "kind": "broadcast", "cron": "0 4 * * *"},
	}
	for _, task := range bad {
		rec := c.do(http.MethodPost, "/api/schedules", map[string]any{"tasks": []any{task}})
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("task %v should be refused, got %d %s", task, rec.Code, rec.Body.String())
		}
	}

	rec := c.do(http.MethodPost, "/api/schedules", map[string]any{"tasks": []any{
		map[string]any{"name": "Nightly restart", "serverId": id, "kind": "restart", "cron": "0 4 * * *", "enabled": true},
	}})
	if rec.Code != http.StatusOK {
		t.Fatalf("a valid task should be accepted: %s", rec.Body.String())
	}

	// The state response should be able to say when it runs next.
	state := decode(t, c.do(http.MethodGet, "/api/state", nil))
	schedules := state["schedules"].([]any)
	entry := schedules[0].(map[string]any)
	if entry["nextRun"] == nil {
		t.Fatal("the next run time should be computed for the UI")
	}
	if entry["describe"] != "at 04:00" {
		t.Fatalf("cron should be described in plain words, got %v", entry["describe"])
	}
}

func TestDeletingAServerRemovesItsSchedules(t *testing.T) {
	app, handler, _ := newTestApp(t)
	c := &client{t: t, handler: handler}
	c.setup("rick", "a-long-enough-password")
	// Only a server whose stack folder has gone can be removed.
	seedServer(t, app, `{"name":"Riverside","host":"127.0.0.1","rconPort":27015,"rconPassword":"x","missing":true}`)
	id := app.cfg.Get().Servers[0].ID
	c.do(http.MethodPost, "/api/schedules", map[string]any{"tasks": []any{
		map[string]any{"name": "Nightly", "serverId": id, "kind": "save", "cron": "0 4 * * *", "enabled": true},
	}})

	rec := c.do(http.MethodPost, "/api/server/delete", map[string]any{"id": id, "forgetData": true})
	if rec.Code != http.StatusOK {
		t.Fatalf("delete failed: %s", rec.Body.String())
	}
	cfg := app.cfg.Get()
	if len(cfg.Servers) != 0 {
		t.Fatal("server was not removed")
	}
	if len(cfg.Schedules) != 0 {
		t.Fatal("a schedule pointing at a deleted server would never run and must be removed too")
	}
}

// The player registry used to be wiped whenever a server was deleted, because
// the whole config was replaced from the browser.
func TestDeletingOneServerKeepsOtherPlayerHistory(t *testing.T) {
	app, handler, _ := newTestApp(t)
	c := &client{t: t, handler: handler}
	c.setup("rick", "a-long-enough-password")

	for _, name := range []string{"One", "Two"} {
		seedServer(t, app, `{"name":"`+name+`","host":"127.0.0.1","rconPort":27015,"rconPassword":"x","missing":true}`)
	}
	cfg := app.cfg.Get()
	first, second := cfg.Servers[0].ID, cfg.Servers[1].ID
	app.store.SyncPlayers(first, []string{"Alice"})
	app.store.SyncPlayers(second, []string{"Bob"})

	c.do(http.MethodPost, "/api/server/delete", map[string]any{"id": first, "forgetData": true})

	if len(app.store.Players(second)) != 1 {
		t.Fatal("deleting one server must not touch another server's player history")
	}
	if len(app.store.Players(first)) != 0 {
		t.Fatal("the deleted server's history should be gone when requested")
	}
}

func TestMetricsRequireTokenWhenSet(t *testing.T) {
	app, handler, _ := newTestApp(t)
	c := &client{t: t, handler: handler}
	c.setup("rick", "a-long-enough-password")

	// Setup generates a token, so an anonymous scrape must be refused.
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("metrics without a token should be 401, got %d", rec.Code)
	}

	token := app.cfg.Get().Metrics.Token
	if token == "" {
		t.Fatal("setup should generate a metrics token")
	}
	req = httptest.NewRequest(http.MethodGet, "/metrics", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("a valid token should be accepted, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "pzadmin_server_up") {
		t.Fatalf("metrics output looks wrong: %s", rec.Body.String())
	}
}

func TestSecurityHeadersForbidInlineCode(t *testing.T) {
	_, handler, _ := newTestApp(t)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	csp := rec.Header().Get("Content-Security-Policy")
	if csp == "" {
		t.Fatal("no Content-Security-Policy was sent")
	}
	if strings.Contains(csp, "unsafe-inline") || strings.Contains(csp, "unsafe-eval") {
		t.Fatalf("the policy must not permit inline code: %s", csp)
	}
	for _, want := range []string{"script-src 'self'", "frame-ancestors 'none'", "default-src 'none'"} {
		if !strings.Contains(csp, want) {
			t.Fatalf("policy missing %q: %s", want, csp)
		}
	}
	if rec.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatal("nosniff header missing")
	}
}

func TestSessionCookieIsHttpOnlyAndStrict(t *testing.T) {
	_, handler, _ := newTestApp(t)
	c := &client{t: t, handler: handler}
	c.setup("rick", "a-long-enough-password")

	var session, csrf *http.Cookie
	for _, ck := range c.cookies {
		if ck.Name == sessionCookie {
			session = ck
		}
		if ck.Name == csrfCookie {
			csrf = ck
		}
	}
	if session == nil || csrf == nil {
		t.Fatal("both cookies should be set on sign-in")
	}
	if !session.HttpOnly {
		t.Fatal("the session cookie must be HttpOnly so script cannot read it")
	}
	if session.SameSite != http.SameSiteStrictMode {
		t.Fatal("the session cookie must be SameSite=Strict")
	}
	if csrf.HttpOnly {
		t.Fatal("the CSRF cookie must be readable by the page, that is how double submit works")
	}
}

func TestSessionsAreGarbageCollected(t *testing.T) {
	app, handler, _ := newTestApp(t)
	c := &client{t: t, handler: handler}
	c.setup("rick", "a-long-enough-password")

	app.sess.mu.Lock()
	for _, s := range app.sess.byID {
		s.Expires = time.Now().Add(-time.Hour)
	}
	app.sess.mu.Unlock()

	if removed := app.sess.gc(); removed != 1 {
		t.Fatalf("expected the expired session to be collected, removed %d", removed)
	}
	rec := c.do(http.MethodGet, "/api/state", nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("an expired session must not authenticate, got %d", rec.Code)
	}
	_ = handler
}

func TestChangingThePasswordSignsEveryoneOut(t *testing.T) {
	_, handler, _ := newTestApp(t)
	c := &client{t: t, handler: handler}
	c.setup("rick", "a-long-enough-password")

	rec := c.do(http.MethodPost, "/api/password", map[string]string{
		"current": "a-long-enough-password", "new": "a-different-long-password",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("password change failed: %s", rec.Body.String())
	}
	if rec := c.do(http.MethodGet, "/api/state", nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("the old session must stop working, got %d", rec.Code)
	}

	fresh := &client{t: t, handler: handler}
	rec = fresh.do(http.MethodPost, "/api/login", map[string]string{
		"username": "rick", "password": "a-different-long-password",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("the new password should work: %s", rec.Body.String())
	}
}

func TestWrongPasswordIsRejectedAfterChange(t *testing.T) {
	_, handler, _ := newTestApp(t)
	c := &client{t: t, handler: handler}
	c.setup("rick", "a-long-enough-password")
	rec := c.do(http.MethodPost, "/api/password", map[string]string{
		"current": "not-the-current-password", "new": "a-different-long-password",
	})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("changing the password without the current one must fail, got %d", rec.Code)
	}
}

func TestDockerOperationsRefuseUnmanagedContainers(t *testing.T) {
	app, _, _ := newTestApp(t)
	if app.cfg.ContainerAllowed("some-other-container") {
		t.Fatal("an unconfigured container must never be allowed")
	}
	_, err := app.cfg.Update(func(c *config.Config) error {
		c.Servers = []config.Server{{ID: "a", Name: "Riverside", DockerContainer: "pz-riverside"}}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !app.cfg.ContainerAllowed("pz-riverside") {
		t.Fatal("a configured container should be allowed")
	}
	if app.cfg.ContainerAllowed("portainer") {
		t.Fatal("still refusing unrelated containers")
	}
	// A server whose stack folder has gone must not be controllable: the
	// name may since belong to something else.
	if _, err := app.cfg.Update(func(c *config.Config) error {
		c.Servers[0].Missing = true
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if app.cfg.ContainerAllowed("pz-riverside") {
		t.Fatal("a missing server's container must not be allowed")
	}
}

func TestStaticAssetsAreServedWithoutAuth(t *testing.T) {
	_, handler, _ := newTestApp(t)
	req := httptest.NewRequest(http.MethodGet, "/app.js", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("the app bundle must load before sign-in, got %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/javascript") {
		t.Fatalf("unexpected content type %q", ct)
	}
}

func TestUnknownPathsFallBackToTheAppShell(t *testing.T) {
	_, handler, _ := newTestApp(t)
	req := httptest.NewRequest(http.MethodGet, "/servers/abc/overview", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "app") {
		t.Fatalf("deep links should serve the shell, got %d", rec.Code)
	}
}

func TestBootstrapDoesNotLeakWhetherSetupRan(t *testing.T) {
	_, handler, _ := newTestApp(t)
	req := httptest.NewRequest(http.MethodGet, "/api/bootstrap", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	body := decode(t, rec)
	if body["setupComplete"] != false {
		t.Fatal("a fresh install should report setup as incomplete")
	}
	if _, present := body["username"]; present {
		t.Fatal("bootstrap must not disclose the account name")
	}
}

// addxp takes the skill and amount as one unquoted "Perk=xp" argument. Sending
// them as three quoted arguments is accepted by the server and does nothing at
// all, which is exactly how this went unnoticed.
func TestAddXPUsesPerkEqualsAmount(t *testing.T) {
	cmd, found := LookupCommand("addxp")
	if !found {
		t.Fatal("addxp missing from the catalogue")
	}
	got, err := cmd.Build([]string{"Rick", "Woodwork", "5000"})
	if err != nil {
		t.Fatal(err)
	}
	want := `addxp "Rick" Woodwork=5000`
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}

	// A player name with a space still has to be quoted.
	got, _ = cmd.Build([]string{"Blue Fox", "Fitness", "200"})
	if got != `addxp "Blue Fox" Fitness=200` {
		t.Fatalf("got %q", got)
	}

	// Junk in the skill name would break the argument apart.
	for _, bad := range [][]string{
		{"Rick", "Wood work", "100"},
		{"Rick", `Wood"work`, "100"},
		{"Rick", "Woodwork=1", "100"},
		{"Rick", "Woodwork", "loads"},
		{"Rick", "", "100"},
	} {
		if out, err := cmd.Build(bad); err == nil {
			t.Fatalf("Build(%v) should be refused, produced %q", bad, out)
		}
	}
}

// additem and addvehicle argument orders differ; both are easy to get backwards.
func TestSpawnCommandArgumentOrder(t *testing.T) {
	item, _ := LookupCommand("additem")
	got, _ := item.Build([]string{"Rick", "Base.Axe", "5"})
	if got != `additem "Rick" "Base.Axe" 5` {
		t.Fatalf("additem takes the player first: %q", got)
	}

	vehicle, _ := LookupCommand("addvehicle")
	got, _ = vehicle.Build([]string{"Base.VanAmbulance", "Rick"})
	if got != `addvehicle "Base.VanAmbulance" "Rick"` {
		t.Fatalf("addvehicle takes the script first: %q", got)
	}
}

// godmodeplayer, invisibleplayer and noclip are toggles. Appending "-true"
// by default gave the server an argument it does not expect, and the command
// quietly did nothing.
func TestToggleCommandsDefaultToTheBareForm(t *testing.T) {
	for _, id := range []string{"godmodeplayer", "invisibleplayer", "noclip"} {
		cmd, found := LookupCommand(id)
		if !found {
			t.Fatalf("%s missing from the catalogue", id)
		}
		got, err := cmd.Build([]string{"Rick"})
		if err != nil {
			t.Fatalf("%s with just a player should be valid: %v", id, err)
		}
		if want := id + ` "Rick"`; got != want {
			t.Fatalf("got %q, want %q", got, want)
		}
		// An empty state argument is the same as omitting it.
		if got, _ := cmd.Build([]string{"Rick", ""}); got != id+` "Rick"` {
			t.Fatalf("a blank state should still toggle: %q", got)
		}
		// The explicit form remains available.
		if got, _ := cmd.Build([]string{"Rick", "false"}); got != id+` "Rick" -false` {
			t.Fatalf("explicit state wrong: %q", got)
		}
		if _, err := cmd.Build([]string{"Rick", "maybe"}); err == nil {
			t.Fatalf("%s should refuse a nonsense state", id)
		}
		if _, err := cmd.Build(nil); err == nil {
			t.Fatalf("%s should require a player", id)
		}
	}
}

// voiceban genuinely does take a -true/-false argument, so it must keep it.
func TestVoicebanKeepsItsExplicitFlag(t *testing.T) {
	cmd, _ := LookupCommand("voiceban")
	got, err := cmd.Build([]string{"Rick", "true"})
	if err != nil {
		t.Fatal(err)
	}
	if got != `voiceban "Rick" -true` {
		t.Fatalf("got %q", got)
	}
	if _, err := cmd.Build([]string{"Rick"}); err == nil {
		t.Fatal("voiceban needs an explicit state")
	}
}

// seedServer stands in for discovery in tests that need a server but are not
// about discovery: it stores a server entry directly, the way a scan would.
// pzPath may be relative to the configured root, as the old save endpoint
// accepted.
func seedServer(t *testing.T, app *App, payload string) *httptest.ResponseRecorder {
	t.Helper()
	var in config.Server
	if err := json.Unmarshal([]byte(payload), &in); err != nil {
		t.Fatalf("seed payload: %v", err)
	}
	if in.ID == "" {
		in.ID = config.RandomToken(8)
	}
	if in.PZPath != "" && !filepath.IsAbs(in.PZPath) {
		in.PZPath = filepath.Join(app.cfg.Get().PZRoot, in.PZPath)
	}
	if in.Recovery.FailuresBeforeRestart == 0 {
		in.Recovery = config.DefaultRecovery()
	}
	if in.Backup.Keep == 0 {
		in.Backup = config.DefaultBackup()
	}
	if _, err := app.cfg.Update(func(c *config.Config) error {
		c.Servers = append(c.Servers, in)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	rec.WriteHeader(http.StatusOK)
	return rec
}
