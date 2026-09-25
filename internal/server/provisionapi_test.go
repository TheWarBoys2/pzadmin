package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const testImage = "indifferentbroccoli/projectzomboid-server-docker@sha256:feedface"

func TestCreateWritesAStackThatIsDiscovered(t *testing.T) {
	app, handler, stacksRoot, dataRoot := discoveryApp(t, func(o *Options) { o.GameImage = testImage })
	addStack(t, stacksRoot, dataRoot, "pz-coalfield", "unless-stopped")
	app.rescan()
	c := &client{t: t, handler: handler}
	c.setup("rick", "a-long-enough-password")

	rec := c.do(http.MethodGet, "/api/stack/new", nil)
	if !strings.Contains(rec.Body.String(), `"serverId":"pz-coalfield"`) {
		t.Fatalf("sources: %s", rec.Body.String())
	}

	body := map[string]any{"name": "pz-western2", "start": "clone", "fromServerId": "pz-coalfield",
		"adminPassword": "letmein", "ini": map[string]string{"PVP": "false"}}
	rec = c.do(http.MethodPost, "/api/stack/plan", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("plan: %d %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "letmein") || !strings.Contains(rec.Body.String(), "RCONPassword=(generated on create)") {
		t.Fatalf("plan should mask secrets: %s", rec.Body.String())
	}
	if _, err := os.Stat(filepath.Join(stacksRoot, "pz-western2")); err == nil {
		t.Fatal("a plan must not write anything")
	}

	rec = c.do(http.MethodPost, "/api/stack/create", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	var out struct {
		ServerID string `json:"serverId"`
		Command  string `json:"command"`
		Plan     struct {
			RCONPort int `json:"rconPort"`
		} `json:"plan"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if out.ServerID != "pz-western2" || strings.Contains(rec.Body.String(), "letmein") ||
		out.Command != "cd "+filepath.Join(stacksRoot, "pz-western2")+" && docker compose up -d" {
		t.Fatalf("create response: %+v", out)
	}
	srv, found := app.cfg.Server("pz-western2")
	if !found || srv.Missing || srv.ServerName != "western2" || srv.RCONPort != out.Plan.RCONPort {
		t.Fatalf("server not discovered: %+v", srv)
	}
	if out.Plan.RCONPort == 27101 {
		t.Fatal("the new server was given a port already in use")
	}

	// Its config can be edited before it has ever booted.
	rec = c.do(http.MethodPost, "/api/server/config/apply", map[string]any{
		"serverId": "pz-western2", "file": "western2.ini", "changes": map[string]string{"PVP": "false"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("pre-boot edit: %d %s", rec.Code, rec.Body.String())
	}
}

func TestCreateRefusesWithoutAPinnedImage(t *testing.T) {
	app, handler, stacksRoot, dataRoot := discoveryApp(t)
	addStack(t, stacksRoot, dataRoot, "pz-coalfield", "unless-stopped")
	app.rescan()
	c := &client{t: t, handler: handler}
	c.setup("rick", "a-long-enough-password")
	rec := c.do(http.MethodPost, "/api/stack/create", map[string]any{
		"name": "pz-x", "start": "defaults", "adminPassword": "letmein",
	})
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "PZADMIN_GAME_IMAGE") {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
}

func TestINIKeysOwnedByEnvAreLocked(t *testing.T) {
	app, handler, stacksRoot, dataRoot := discoveryApp(t)
	addStack(t, stacksRoot, dataRoot, "pz-coalfield", "unless-stopped")
	app.rescan()
	c := &client{t: t, handler: handler}
	c.setup("rick", "a-long-enough-password")

	rec := c.do(http.MethodGet, "/api/server/config/fields?id=pz-coalfield&file=pz-coalfield.ini", nil)
	if !strings.Contains(rec.Body.String(), `"key":"RCONPort"`) || !strings.Contains(rec.Body.String(), "RCON_PORT in the stack") {
		t.Fatalf("RCONPort should be locked: %s", rec.Body.String())
	}

	rec = c.do(http.MethodPost, "/api/server/config/apply", map[string]any{
		"serverId": "pz-coalfield", "file": "pz-coalfield.ini", "changes": map[string]string{"RCONPort": "27999"},
	})
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "RCON_PORT") {
		t.Fatalf("form edit of a locked key: %d %s", rec.Code, rec.Body.String())
	}

	rec = c.do(http.MethodPost, "/api/server/config/save", map[string]any{
		"serverId": "pz-coalfield", "file": "pz-coalfield.ini",
		"content": "RCONPort=27015\nRCONPassword=changed\nPVP=true\nMods=A\n",
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("raw edit of a locked key: %d %s", rec.Code, rec.Body.String())
	}
	rec = c.do(http.MethodPost, "/api/server/config/save", map[string]any{
		"serverId": "pz-coalfield", "file": "pz-coalfield.ini",
		"content": "RCONPort=27015\nRCONPassword=pw\nPVP=false\nMods=A\n",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("raw edit of an unlocked key: %d %s", rec.Code, rec.Body.String())
	}
}

func TestEnvEditing(t *testing.T) {
	app, handler, stacksRoot, dataRoot := discoveryApp(t)
	addStack(t, stacksRoot, dataRoot, "pz-knight", "unless-stopped")
	app.rescan()
	c := &client{t: t, handler: handler}
	c.setup("rick", "a-long-enough-password")

	rec := c.do(http.MethodGet, "/api/stack/env?id=pz-knight", nil)
	if strings.Contains(rec.Body.String(), `"value":"pw"`) {
		t.Fatal("a secret reached the browser")
	}
	rec = c.do(http.MethodPost, "/api/stack/env/save", map[string]any{
		"serverId": "pz-knight", "changes": map[string]string{"MAX_PLAYERS": "20"},
	})
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "docker compose up -d") {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	b, _ := os.ReadFile(filepath.Join(stacksRoot, "pz-knight", ".env"))
	if !strings.Contains(string(b), "MAX_PLAYERS=20") {
		t.Fatal("not written")
	}
	backups, _ := filepath.Glob(filepath.Join(app.backup.Dir("pz-knight"), "env", ".env-*"))
	if len(backups) != 1 {
		t.Fatalf("expected one .env backup, got %v", backups)
	}
	rec = c.do(http.MethodPost, "/api/stack/env/save", map[string]any{
		"serverId": "pz-knight", "changes": map[string]string{"RCON_PORT": "1"},
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatal("ports must not be editable in .env alone")
	}
}

// fakeArcane serves a container list and one container's details.
func fakeArcane(t *testing.T, containers []map[string]any, details map[string]any) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/containers"):
			_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "data": containers})
		case strings.Contains(r.URL.Path, "/containers/"):
			_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "data": details})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func TestStartOfANeverCreatedContainerGivesTheCommand(t *testing.T) {
	url := fakeArcane(t, []map[string]any{}, nil)
	app, handler, stacksRoot, dataRoot := discoveryApp(t, func(o *Options) {
		o.ArcaneURL, o.ArcaneEnvID, o.ArcaneAPIKey = url, "0", "k"
	})
	addStack(t, stacksRoot, dataRoot, "pz-new", "unless-stopped")
	app.rescan()
	c := &client{t: t, handler: handler}
	c.setup("rick", "a-long-enough-password")
	rec := c.do(http.MethodPost, "/api/lifecycle", map[string]any{"serverId": "pz-new", "action": "start"})
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "docker compose up -d") {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
}

// A start reuses the container's own mounts, so those are what is checked,
// not the compose file.
func TestStartChecksTheContainersRealMounts(t *testing.T) {
	app, handler, stacksRoot, dataRoot := discoveryApp(t, func(o *Options) {
		empty := filepath.Join(o.DataRoot, "emptied")
		_ = os.MkdirAll(empty, 0o755)
		url := fakeArcane(t,
			[]map[string]any{{"id": "abc", "names": []string{"/pz-real"}, "state": "exited"}},
			map[string]any{"id": "abc", "name": "/pz-real", "state": map[string]any{"status": "exited"},
				"mounts": []map[string]any{{"type": "bind", "source": empty, "destination": "/project-zomboid"}}})
		o.ArcaneURL, o.ArcaneEnvID, o.ArcaneAPIKey = url, "0", "k"
	})
	addStack(t, stacksRoot, dataRoot, "real", "unless-stopped")
	app.rescan()
	c := &client{t: t, handler: handler}
	c.setup("rick", "a-long-enough-password")

	// The compose file is fine; the container's own data mount is empty.
	rec := c.do(http.MethodPost, "/api/lifecycle", map[string]any{"serverId": "real", "action": "start"})
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "is empty") {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
}

func TestWizardFieldsAndAFreshCreate(t *testing.T) {
	app, handler, _, _ := discoveryApp(t, func(o *Options) { o.GameImage = testImage })
	app.rescan()
	c := &client{t: t, handler: handler}
	c.setup("rick", "a-long-enough-password")

	rec := c.do(http.MethodGet, "/api/stack/wizard/fields?start=defaults", nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"key":"PublicName"`) ||
		!strings.Contains(rec.Body.String(), `"label":"Insane"`) {
		t.Fatalf("fields: %d %.300s", rec.Code, rec.Body.String())
	}

	// No existing server is needed to create one from the defaults.
	rec = c.do(http.MethodPost, "/api/stack/create", map[string]any{
		"name": "pz-first", "start": "defaults", "adminPassword": "letmein",
		"ini":     map[string]string{"PublicName": "First"},
		"sandbox": map[string]string{"Zombies": "5"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	if _, found := app.cfg.Server("pz-first"); !found {
		t.Fatal("not discovered")
	}
}
