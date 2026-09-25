package server

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// bearer sends a request authenticated only by an API key: no cookies, no
// CSRF header, and a cross-site fetch marker to prove neither check applies.
func bearer(t *testing.T, handler http.Handler, key, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

func createKey(t *testing.T, c *client, name, scope string) (string, string) {
	t.Helper()
	rec := c.do(http.MethodPost, "/api/keys/create", map[string]string{"name": name, "scope": scope})
	if rec.Code != http.StatusOK {
		t.Fatalf("create key: %d %s", rec.Code, rec.Body.String())
	}
	out := decode(t, rec)
	key, _ := out["key"].(string)
	meta, _ := out["apiKey"].(map[string]any)
	id, _ := meta["id"].(string)
	if !strings.HasPrefix(key, apiKeyPrefix) || id == "" {
		t.Fatalf("unexpected create response: %s", rec.Body.String())
	}
	return key, id
}

func TestAPIKeyReadOnlyScope(t *testing.T) {
	_, handler, _ := newTestApp(t)
	c := &client{t: t, handler: handler}
	c.setup("rick", "a-long-enough-password")
	key, _ := createKey(t, c, "grafana", "read")

	if rec := bearer(t, handler, key, http.MethodGet, "/api/state", ""); rec.Code != http.StatusOK {
		t.Fatalf("a read-only key should read state, got %d %s", rec.Code, rec.Body.String())
	}
	rec := bearer(t, handler, key, http.MethodPost, "/api/settings", `{"timezone":"UTC"}`)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("a read-only key must not POST, got %d", rec.Code)
	}
}

func TestAPIKeyFullScopeSkipsCSRFAndIsAudited(t *testing.T) {
	app, handler, _ := newTestApp(t)
	c := &client{t: t, handler: handler}
	c.setup("rick", "a-long-enough-password")
	key, _ := createKey(t, c, "discord-bot", "full")

	rec := bearer(t, handler, key, http.MethodPost, "/api/settings", `{"timezone":"UTC"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("a full key should POST without CSRF, got %d %s", rec.Code, rec.Body.String())
	}
	found := false
	for _, e := range app.store.Events("", "", 50) {
		if e.Message == "Settings updated" {
			if e.Actor != "discord-bot (API key)" {
				t.Fatalf("audit actor = %q, want the key name", e.Actor)
			}
			found = true
		}
	}
	if !found {
		t.Fatal("the settings change was not recorded in the audit log")
	}
}

func TestAPIKeyBadOrRevokedIsRefused(t *testing.T) {
	_, handler, _ := newTestApp(t)
	c := &client{t: t, handler: handler}
	c.setup("rick", "a-long-enough-password")
	key, id := createKey(t, c, "script", "full")

	for _, bad := range []string{"pzk_nope", "not-a-key", ""} {
		if rec := bearer(t, handler, bad, http.MethodGet, "/api/state", ""); rec.Code != http.StatusUnauthorized {
			t.Fatalf("key %q returned %d, expected 401", bad, rec.Code)
		}
	}

	// A bad Bearer header must not fall back to a valid session cookie.
	req := httptest.NewRequest(http.MethodGet, "/api/state", nil)
	for _, ck := range c.cookies {
		req.AddCookie(ck)
	}
	req.Header.Set("Authorization", "Bearer pzk_nope")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("a bad key with a good cookie returned %d, expected 401", rec.Code)
	}

	if rec := c.do(http.MethodPost, "/api/keys/revoke", map[string]string{"id": id}); rec.Code != http.StatusOK {
		t.Fatalf("revoke: %d %s", rec.Code, rec.Body.String())
	}
	if rec := bearer(t, handler, key, http.MethodGet, "/api/state", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("a revoked key returned %d, expected 401", rec.Code)
	}
	if rec := c.do(http.MethodPost, "/api/keys/revoke", map[string]string{"id": id}); rec.Code != http.StatusNotFound {
		t.Fatalf("revoking twice returned %d, expected 404", rec.Code)
	}
}

// A key, even a full one, cannot manage keys, sessions or the password, so a
// leaked key can be revoked from the browser and cannot entrench itself.
func TestAPIKeyCannotManageAccount(t *testing.T) {
	_, handler, _ := newTestApp(t)
	c := &client{t: t, handler: handler}
	c.setup("rick", "a-long-enough-password")
	key, id := createKey(t, c, "script", "full")

	cases := []struct{ method, path, body string }{
		{http.MethodGet, "/api/keys", ""},
		{http.MethodGet, "/api/sessions", ""},
		{http.MethodPost, "/api/keys/create", `{"name":"another","scope":"full"}`},
		{http.MethodPost, "/api/keys/revoke", `{"id":"` + id + `"}`},
		{http.MethodPost, "/api/sessions/revoke", `{}`},
		{http.MethodPost, "/api/password", `{"current":"x","new":"y"}`},
	}
	for _, tc := range cases {
		if rec := bearer(t, handler, key, tc.method, tc.path, tc.body); rec.Code != http.StatusForbidden {
			t.Fatalf("%s %s with a key returned %d, expected 403", tc.method, tc.path, rec.Code)
		}
	}
}

func TestAPIKeysPersistAsHashesOnly(t *testing.T) {
	app, handler, dir := newTestApp(t)
	c := &client{t: t, handler: handler}
	c.setup("rick", "a-long-enough-password")
	key, _ := createKey(t, c, "backup-cron", "read")

	b, err := os.ReadFile(filepath.Join(dir, "apikeys.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), key) || strings.Contains(string(b), strings.TrimPrefix(key, apiKeyPrefix)) {
		t.Fatal("apikeys.json must not contain the key itself")
	}
	if !strings.Contains(string(b), hashToken(key)) {
		t.Fatal("apikeys.json should hold the key's hash")
	}

	// The list shown in the browser carries neither the key nor its hash.
	rec := c.do(http.MethodGet, "/api/keys", nil)
	if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), hashToken(key)) || !strings.Contains(rec.Body.String(), "backup-cron") {
		t.Fatalf("unexpected key list: %d %s", rec.Code, rec.Body.String())
	}

	// Keys survive a restart.
	reloaded := newAPIKeyStore(filepath.Join(dir, "apikeys.json"))
	if _, ok := reloaded.lookup(key, "127.0.0.1"); !ok {
		t.Fatal("the key should still work after a reload")
	}
	_ = app
}

func TestAPIKeyCreateValidation(t *testing.T) {
	_, handler, _ := newTestApp(t)
	c := &client{t: t, handler: handler}
	c.setup("rick", "a-long-enough-password")
	createKey(t, c, "dup", "read")

	for _, body := range []map[string]string{
		{"name": "", "scope": "read"},
		{"name": "x", "scope": "admin"},
		{"name": "DUP", "scope": "full"},
		{"name": strings.Repeat("a", 41), "scope": "read"},
	} {
		if rec := c.do(http.MethodPost, "/api/keys/create", body); rec.Code != http.StatusBadRequest {
			t.Fatalf("%v returned %d, expected 400", body, rec.Code)
		}
	}
}
