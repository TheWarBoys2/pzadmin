package server

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

// bearer sends a request authenticated only by an API key: no cookies, no
// CSRF header, and a cross-site fetch marker to prove neither check applies.
func bearer(t *testing.T, handler http.Handler, key, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

func createKey(t *testing.T, c *client, req map[string]any) (string, string) {
	t.Helper()
	rec := c.do(http.MethodPost, "/api/keys/create", req)
	if rec.Code != http.StatusOK {
		t.Fatalf("create key: %d %s", rec.Code, rec.Body.String())
	}
	out := decode(t, rec)
	key, _ := out["key"].(string)
	meta, _ := out["apiKey"].(map[string]any)
	id, _ := meta["id"].(string)
	if !strings.HasPrefix(key, apiKeyPrefix+id+"_") || id == "" {
		t.Fatalf("unexpected create response: %s", rec.Body.String())
	}
	return key, id
}

// apiTestApp is a signed-in app with two servers whose RCON ports refuse
// connections, so actions fail fast but are still audited.
func apiTestApp(t *testing.T) (*App, http.Handler, *client, string, string) {
	t.Helper()
	app, handler, _ := newTestApp(t)
	c := &client{t: t, handler: handler}
	c.setup("rick", "a-long-enough-password")
	seedServer(t, app, `{"name":"Riverside","enabled":false,"host":"127.0.0.1","rconPort":1,"rconPassword":"x","public":{"description":"Vanilla","address":"pz.example.com"}}`)
	seedServer(t, app, `{"name":"Muldraugh","enabled":false,"host":"127.0.0.1","rconPort":1,"rconPassword":"x","gamePort":16261}`)
	servers := app.cfg.Get().Servers
	return app, handler, c, servers[0].ID, servers[1].ID
}

func TestAPIKeyStoredHashedAndPrivate(t *testing.T) {
	_, _, c, _, _ := apiTestApp(t)
	key, _ := createKey(t, c, map[string]any{"name": "discord-bot"})

	rec := c.do(http.MethodGet, "/api/keys", nil)
	if strings.Contains(rec.Body.String(), key) || strings.Contains(rec.Body.String(), hashToken(key)) {
		t.Fatal("the key list must not contain the key or its hash")
	}
}

func TestAPIKeyFileHasNoPlaintext(t *testing.T) {
	app, handler, _ := newTestApp(t)
	c := &client{t: t, handler: handler}
	c.setup("rick", "a-long-enough-password")
	key, _ := createKey(t, c, map[string]any{"name": "grafana"})

	b, err := os.ReadFile(app.keys.path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), key) {
		t.Fatal("apikeys.json holds the key itself")
	}
	if !strings.Contains(string(b), hashToken(key)) {
		t.Fatal("apikeys.json does not hold the key's hash")
	}
	if fi, _ := os.Stat(app.keys.path); fi.Mode().Perm() != 0o600 {
		t.Fatalf("apikeys.json mode = %v, want 0600", fi.Mode().Perm())
	}

	// A fresh store reads the key back, so a restart does not break scripts.
	again := newAPIKeyStore(app.keys.path)
	if _, found := again.lookup(key, "1.2.3.4"); !found {
		t.Fatal("the key did not survive a reload")
	}
}

func TestAPIv1AuthIsKeyOnly(t *testing.T) {
	_, handler, c, _, _ := apiTestApp(t)
	key, _ := createKey(t, c, map[string]any{"name": "bot", "scopes": []string{"control"}})

	if rec := bearer(t, handler, key, http.MethodGet, "/api/v1/servers", ""); rec.Code != http.StatusOK {
		t.Fatalf("a key should list servers, got %d %s", rec.Code, rec.Body.String())
	}
	if rec := bearer(t, handler, "", http.MethodGet, "/api/v1/servers", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("no key should be 401, got %d", rec.Code)
	}
	if rec := bearer(t, handler, "pzk_nope_nope", http.MethodGet, "/api/v1/servers", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("a bad key should be 401, got %d", rec.Code)
	}
	// A browser session is not enough for /api/v1.
	if rec := c.do(http.MethodGet, "/api/v1/servers", nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("a session cookie must not open /api/v1, got %d", rec.Code)
	}
	// And a key is not enough for the dashboard's own routes.
	for _, tc := range []struct{ method, path, body string }{
		{http.MethodGet, "/api/state", ""},
		{http.MethodGet, "/api/keys", ""},
		{http.MethodPost, "/api/keys/create", `{"name":"escalate"}`},
		{http.MethodPost, "/api/password", `{"current":"x","new":"y"}`},
	} {
		if rec := bearer(t, handler, key, tc.method, tc.path, tc.body); rec.Code != http.StatusUnauthorized {
			t.Fatalf("a key must not reach %s, got %d", tc.path, rec.Code)
		}
	}
}

func TestAPIv1ServerViewAndAllowlist(t *testing.T) {
	_, handler, c, riverside, muldraugh := apiTestApp(t)
	key, _ := createKey(t, c, map[string]any{"name": "one-server", "servers": []string{riverside}})

	out := decode(t, bearer(t, handler, key, http.MethodGet, "/api/v1/servers", ""))
	list, _ := out["servers"].([]any)
	if len(list) != 1 {
		t.Fatalf("a limited key should see one server, got %d", len(list))
	}
	s := list[0].(map[string]any)
	if s["id"] != riverside || s["description"] != "Vanilla" || s["address"] != "pz.example.com" {
		t.Fatalf("unexpected server view: %v", s)
	}
	for _, path := range []string{
		"/api/v1/servers/" + muldraugh,
		"/api/v1/servers/" + muldraugh + "/players",
		"/api/v1/events?server=" + muldraugh,
	} {
		if rec := bearer(t, handler, key, http.MethodGet, path, ""); rec.Code != http.StatusNotFound {
			t.Fatalf("%s: a server outside the key must look missing, got %d", path, rec.Code)
		}
	}

	all, _ := createKey(t, c, map[string]any{"name": "all-servers"})
	one := decode(t, bearer(t, handler, all, http.MethodGet, "/api/v1/servers/"+muldraugh, ""))
	if one["port"] != float64(16261) {
		t.Fatalf("port should fall back to the game port, got %v", one["port"])
	}
}

func TestAPIv1ScopesAndAudit(t *testing.T) {
	app, handler, c, riverside, _ := apiTestApp(t)
	reader, _ := createKey(t, c, map[string]any{"name": "status-page"})
	bot, _ := createKey(t, c, map[string]any{"name": "discord-bot", "scopes": []string{"control"}})

	path := "/api/v1/servers/" + riverside + "/actions"
	body := `{"action":"save"}`
	if rec := bearer(t, handler, reader, http.MethodPost, path, body); rec.Code != http.StatusForbidden {
		t.Fatalf("a read key must not run actions, got %d", rec.Code)
	}
	if rec := bearer(t, handler, bot, http.MethodPost, "/api/v1/servers/"+riverside+"/console", `{"command":"save"}`); rec.Code != http.StatusForbidden {
		t.Fatalf("console needs its own scope, got %d", rec.Code)
	}
	if rec := bearer(t, handler, bot, http.MethodPost, path, `{"serverId":"x","action":"save"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("serverId in the body should be refused, got %d", rec.Code)
	}
	if rec := bearer(t, handler, bot, http.MethodPost, path, `{"action":"save","bogus":1}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown fields should be refused, got %d", rec.Code)
	}

	// RCON is refused in tests, so the command fails, but it reaches the
	// same handler as the dashboard and is audited against the key.
	rec := bearer(t, handler, bot, http.MethodPost, path, body)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("expected the RCON failure to come back as 502, got %d %s", rec.Code, rec.Body.String())
	}
	found := false
	for _, e := range app.store.Events(riverside, "admin.action", 20) {
		if e.Source == "api" && e.Actor == "key:discord-bot" {
			found = true
		}
	}
	if !found {
		t.Fatal("the action was not audited with the key as actor and api as source")
	}

	// Sign-in records never reach a key.
	for _, e := range decode(t, bearer(t, handler, reader, http.MethodGet, "/api/v1/events?limit=500", ""))["events"].([]any) {
		if strings.HasPrefix(e.(map[string]any)["kind"].(string), "auth.") {
			t.Fatal("auth events must not be served to a key")
		}
	}
}

func TestAPIv1RevokedAndExpiredKeys(t *testing.T) {
	app, handler, c, _, _ := apiTestApp(t)
	key, _ := createKey(t, c, map[string]any{"name": "old", "expiresDays": 30})

	app.keys.mu.Lock()
	for _, k := range app.keys.byHash {
		k.Expires = time.Now().Add(-time.Minute)
	}
	app.keys.mu.Unlock()
	if rec := bearer(t, handler, key, http.MethodGet, "/api/v1/key", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("an expired key should be 401, got %d", rec.Code)
	}

	key2, id2 := createKey(t, c, map[string]any{"name": "new"})
	if rec := bearer(t, handler, key2, http.MethodGet, "/api/v1/key", ""); rec.Code != http.StatusOK {
		t.Fatalf("whoami: %d", rec.Code)
	}
	if rec := c.do(http.MethodPost, "/api/keys/revoke", map[string]string{"id": id2}); rec.Code != http.StatusOK {
		t.Fatalf("revoke: %d", rec.Code)
	}
	if rec := bearer(t, handler, key2, http.MethodGet, "/api/v1/key", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("a revoked key should be 401, got %d", rec.Code)
	}

	// Changing the password leaves keys working: they belong to
	// integrations, not browsers.
	key3, _ := createKey(t, c, map[string]any{"name": "survivor"})
	if rec := c.do(http.MethodPost, "/api/password", map[string]string{"current": "a-long-enough-password", "new": "another-long-password"}); rec.Code != http.StatusOK {
		t.Fatalf("password change: %d %s", rec.Code, rec.Body.String())
	}
	if rec := bearer(t, handler, key3, http.MethodGet, "/api/v1/key", ""); rec.Code != http.StatusOK {
		t.Fatalf("a password change should not revoke keys, got %d", rec.Code)
	}
}

func TestAPIv1RateLimit(t *testing.T) {
	_, handler, c, _, _ := apiTestApp(t)
	key, _ := createKey(t, c, map[string]any{"name": "busy"})
	var last *httptest.ResponseRecorder
	for i := 0; i < int(rateRead.Burst)+1; i++ {
		last = bearer(t, handler, key, http.MethodGet, "/api/v1/servers", "")
	}
	if last.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429 after the burst, got %d", last.Code)
	}
	if last.Header().Get("Retry-After") == "" {
		t.Fatal("a 429 must say when to retry")
	}
}

func TestAPIv1UnknownPathIsJSON404(t *testing.T) {
	_, handler, c, riverside, _ := apiTestApp(t)
	key, _ := createKey(t, c, map[string]any{"name": "x"})
	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/api/v1/nope"},
		{http.MethodGet, "/api/v1/servers/" + riverside + "/actions"},
	} {
		rec := bearer(t, handler, key, tc.method, tc.path, "")
		if rec.Code != http.StatusNotFound || !strings.Contains(rec.Header().Get("Content-Type"), "json") {
			t.Fatalf("%s %s: got %d %s", tc.method, tc.path, rec.Code, rec.Header().Get("Content-Type"))
		}
	}
}

func TestAPIKeyCreateValidation(t *testing.T) {
	_, _, c, _, _ := apiTestApp(t)
	for _, req := range []map[string]any{
		{"name": ""},
		{"name": "x", "scopes": []string{"admin"}},
		{"name": "x", "servers": []string{"no-such-server"}},
		{"name": "x", "expiresDays": -1},
	} {
		if rec := c.do(http.MethodPost, "/api/keys/create", req); rec.Code != http.StatusBadRequest {
			t.Fatalf("%v should be refused, got %d", req, rec.Code)
		}
	}
	createKey(t, c, map[string]any{"name": "dup"})
	if rec := c.do(http.MethodPost, "/api/keys/create", map[string]any{"name": "DUP"}); rec.Code != http.StatusBadRequest {
		t.Fatalf("a duplicate name should be refused, got %d", rec.Code)
	}
}

func TestRateLimiterRefills(t *testing.T) {
	l := newRateLimiter()
	c := rateClass{Name: "t", PerMin: 60, Burst: 2}
	for i := 0; i < 2; i++ {
		if ok, _, _ := l.take("k", c); !ok {
			t.Fatalf("take %d should pass", i)
		}
	}
	ok, _, wait := l.take("k", c)
	if ok || wait <= 0 || wait > time.Second {
		t.Fatalf("third take: ok=%v wait=%v", ok, wait)
	}
	l.buckets["k"].last = time.Now().Add(-2 * time.Second)
	if ok, _, _ := l.take("k", c); !ok {
		t.Fatal("the bucket should refill")
	}
}

func TestAPIv1StreamEndsWhenKeyIsRevoked(t *testing.T) {
	app, handler, c, _, _ := apiTestApp(t)
	key, id := createKey(t, c, map[string]any{"name": "streamer"})

	srv := httptest.NewServer(handler)
	defer srv.Close()
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/api/v1/stream", nil)
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("stream: %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	buf := make([]byte, 4096)
	n, _ := resp.Body.Read(buf)
	if !strings.Contains(string(buf[:n]), "event: status") {
		t.Fatalf("the stream should open with status, got %q", buf[:n])
	}

	app.keys.revoke(id)
	done := make(chan struct{})
	go func() {
		for {
			if _, err := resp.Body.Read(buf); err != nil {
				close(done)
				return
			}
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the stream stayed open after its key was revoked")
	}
}
