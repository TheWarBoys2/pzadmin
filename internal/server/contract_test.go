package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/TheWarBoys2/pzadmin/internal/config"
)

// The API rejects unknown JSON fields, which catches typos but also means a
// mismatch between what web/app.js sends and what a handler expects turns into
// a confusing 400 at runtime. These tests send the exact payloads the browser
// builds, so any drift fails the build instead of the user's evening.
func TestBrowserPayloadsAreAccepted(t *testing.T) {
	app, handler, dir := newTestApp(t)
	c := &client{t: t, handler: handler}
	c.setup("rick", "a-long-enough-password")

	root := filepath.Join(dir, "pzroot", "riverside", "projectzomboid")
	mustMkdir(t, filepath.Join(root, "config", "Server"))
	mustMkdir(t, filepath.Join(root, "config", "Logs"))
	mustMkdir(t, filepath.Join(root, "data", "Saves", "Multiplayer", "kal"))
	mustWrite(t, filepath.Join(root, "config", "Server", "riv.ini"), "PublicName=Riverside\nMods=ModA\nMaxPlayers=16\n")
	mustWrite(t, filepath.Join(root, "data", "Saves", "Multiplayer", "kal", "map.bin"), "world")

	if _, err := app.cfg.Update(func(cfg *config.Config) error {
		cfg.PZRoot = filepath.Join(dir, "pzroot")
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	// This is the object web/app.js gather() produces, field for field.
	serverPayload := `{
      "id":"","name":"Riverside","enabled":false,"host":"127.0.0.1","rconPort":27015,
      "rconPassword":"secret","pzPath":"riverside/projectzomboid","gamePort":16261,
      "dockerContainer":"",
      "recovery":{"enabled":true,"failuresBeforeRestart":5,"cooldownMinutes":10,"maxAttempts":3},
      "mods":{"watchUpdates":true,"autoRestart":false,"restartDelayMinutes":10},
      "backup":{"enabled":true,"keep":10,"includeConfig":true},
      "notes":"","sortHint":0}`
	if rec := seedServer(t, app, serverPayload); rec.Code != http.StatusOK {
		t.Fatalf("server save payload rejected: %d %s", rec.Code, rec.Body.String())
	}
	id := app.cfg.Get().Servers[0].ID

	cases := []struct {
		path    string
		payload string
		want    int
	}{
		{"/api/settings", `{"pzRoot":"` + jsonPath(filepath.Join(dir, "pzroot")) + `","timezone":"Europe/London",
		  "interface":{"pollSeconds":10,"retainDays":30}}`, http.StatusOK},
		{"/api/settings", `{"notify":{"enabled":false,"webhookUrl":"","events":["server.down"],"minIntervalSeconds":300}}`, http.StatusOK},
		{"/api/settings", `{"metrics":{"enabled":true,"token":""}}`, http.StatusOK},
		{"/api/schedules", `{"tasks":[{"id":"","serverId":"` + id + `","name":"Nightly restart","kind":"restart",
		  "cron":"0 4 * * *","message":"","command":"","enabled":true,"warnMinutes":[15,5,1]}]}`, http.StatusOK},
		{"/api/backups/create", `{"serverId":"` + id + `","note":"before mod update"}`, http.StatusOK},
		{"/api/player/note", `{"serverId":"` + id + `","name":"Ghost","note":"x"}`, http.StatusNotFound},
		{"/api/player/forget", `{"serverId":"` + id + `"}`, http.StatusOK},
		{"/api/notify/test", `{"url":""}`, http.StatusBadRequest},
		{"/api/lifecycle", `{"serverId":"` + id + `","action":"cancel-pending"}`, http.StatusOK},
		{"/api/server/config/save", `{"serverId":"` + id + `","file":"riv.ini",
		  "content":"PublicName=Riverside\nMods=ModA\nMaxPlayers=24\n","reload":false}`, http.StatusOK},
		{"/api/password", `{"current":"wrong","new":"another-long-password"}`, http.StatusForbidden},
		{"/api/server/delete", `{"id":"nonexistent","forgetData":false}`, http.StatusNotFound},
		{"/api/console", `{"serverId":"` + id + `","command":""}`, http.StatusBadRequest},
		{"/api/action", `{"serverId":"` + id + `","action":"save","args":[]}`, http.StatusBadGateway},
	}
	for _, tc := range cases {
		rec := c.raw(tc.path, tc.payload)
		if rec.Code == http.StatusBadRequest && tc.want != http.StatusBadRequest {
			t.Fatalf("%s: payload was not understood (%s)", tc.path, strings.TrimSpace(rec.Body.String()))
		}
		if rec.Code != tc.want {
			t.Fatalf("%s: got %d want %d (%s)", tc.path, rec.Code, tc.want, strings.TrimSpace(rec.Body.String()))
		}
	}

	// Backup verify and delete need a real archive name.
	list := app.backup.List(id)
	if len(list) == 0 {
		t.Fatal("the backup created above should be listed")
	}
	name := list[0].Name
	for _, path := range []string{"/api/backups/verify", "/api/backups/delete"} {
		rec := c.raw(path, `{"serverId":"`+id+`","name":"`+name+`"}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", path, rec.Code, rec.Body.String())
		}
	}

	// The restore confirmation must match the server name exactly.
	rec := c.raw("/api/backups/restore", `{"serverId":"`+id+`","name":"`+name+`","confirm":"wrong"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("a mistyped confirmation must refuse the restore, got %d", rec.Code)
	}
}

func TestUnknownFieldsAreRejected(t *testing.T) {
	_, handler, _ := newTestApp(t)
	c := &client{t: t, handler: handler}
	c.setup("rick", "a-long-enough-password")
	rec := c.raw("/api/settings", `{"timezone":"UTC","totallyMadeUp":true}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("an unknown field should be refused, got %d", rec.Code)
	}
}

func (c *client) raw(path, payload string) *httptest.ResponseRecorder {
	c.t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(payload))
	for _, ck := range c.cookies {
		req.AddCookie(ck)
	}
	req.Header.Set("X-CSRF-Token", c.csrf)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	c.handler.ServeHTTP(rec, req)
	return rec
}

func jsonPath(p string) string {
	b, _ := json.Marshal(p)
	return strings.Trim(string(b), `"`)
}

func mustMkdir(t *testing.T, p string) {
	t.Helper()
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatal(err)
	}
}

func mustWrite(t *testing.T, p, content string) {
	t.Helper()
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
