package server

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/TheWarBoys2/pzadmin/internal/config"
)

func TestDuplicateCopiesTheWorldUnderTheNewName(t *testing.T) {
	app, handler, stacksRoot, dataRoot := discoveryApp(t, func(o *Options) { o.GameImage = testImage })
	dir := addStack(t, stacksRoot, dataRoot, "coalfield", "unless-stopped")
	writeFile(t, filepath.Join(dir, "Server", "coalfield.ini"), "RCONPort=27015\nRCONPassword=pw\nPVP=true\nResetID=123\nServerPlayerID=456\n")
	writeFile(t, filepath.Join(dir, ".env"), "SERVER_NAME=coalfield\nRCON_PORT=27015\nRCON_PASSWORD=pw\nADMIN_USERNAME=boss\nADMIN_PASSWORD=hunter22\nMEMORY_XMX_GB=6\n")
	srcConfig := filepath.Join(dataRoot, "coalfield", "projectzomboid", "config")
	writeFile(t, filepath.Join(srcConfig, "Saves", "Multiplayer", "coalfield", "map_1_1.bin"), "chunk")
	writeFile(t, filepath.Join(srcConfig, "Saves", "Multiplayer", "coalfield_player", "players.db"), "chars")
	writeFile(t, filepath.Join(srcConfig, "db", "coalfield.db"), "accounts")
	writeFile(t, filepath.Join(srcConfig, "backups", "startup", "big.zip"), "skip me")
	app.rescan()
	c := &client{t: t, handler: handler}
	c.setup("rick", "a-long-enough-password")

	rec := c.do(http.MethodGet, "/api/stack/duplicate/check?id=coalfield", nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"adminUsername":"boss"`) {
		t.Fatalf("check: %d %s", rec.Code, rec.Body.String())
	}

	// The wizard cannot make a duplicate: its IDs only fit its world.
	rec = c.do(http.MethodPost, "/api/stack/create", map[string]any{"name": "pz-sneaky", "start": "duplicate",
		"fromServerId": "coalfield", "adminPassword": "letmein"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("wizard duplicate: %d %s", rec.Code, rec.Body.String())
	}

	// Not while it runs. Monitoring is off, so the monitor's own view of
	// the server cannot overwrite the status set here.
	if _, err := app.cfg.Update(func(c *config.Config) error {
		for i := range c.Servers {
			c.Servers[i].Enabled = false
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	app.updateStatus("coalfield", func(st *Status) { st.Online = true })
	rec = c.do(http.MethodPost, "/api/stack/duplicate", map[string]any{"serverId": "coalfield", "name": "pz-coalfield2"})
	if rec.Code != http.StatusConflict {
		t.Fatalf("running source: %d %s", rec.Code, rec.Body.String())
	}
	app.updateStatus("coalfield", func(st *Status) { st.Online = false })

	rec = c.do(http.MethodPost, "/api/stack/duplicate", map[string]any{"serverId": "coalfield", "name": "pz-coalfield2"})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("duplicate: %d %s", rec.Code, rec.Body.String())
	}
	var started struct {
		Job string `json:"job"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &started)

	var status dupStatus
	deadline := time.Now().Add(10 * time.Second)
	for {
		rec = c.do(http.MethodGet, "/api/stack/duplicate/status?job="+started.Job, nil)
		_ = json.Unmarshal(rec.Body.Bytes(), &status)
		if status.State != "copying" || time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if status.State != "done" || status.ServerID != "pz-coalfield2" {
		t.Fatalf("status: %+v", status)
	}

	newConfig := filepath.Join(dataRoot, "coalfield2", "projectzomboid", "config")
	for path, want := range map[string]string{
		filepath.Join(newConfig, "Saves", "Multiplayer", "coalfield2", "map_1_1.bin"):       "chunk",
		filepath.Join(newConfig, "Saves", "Multiplayer", "coalfield2_player", "players.db"): "chars",
		filepath.Join(newConfig, "db", "coalfield2.db"):                                     "accounts",
	} {
		if b, err := os.ReadFile(path); err != nil || string(b) != want {
			t.Fatalf("%s: %q %v", path, b, err)
		}
	}
	if _, err := os.Stat(filepath.Join(newConfig, "backups")); err == nil {
		t.Fatal("the game's own backups should not be copied")
	}
	ini, _ := os.ReadFile(filepath.Join(stacksRoot, "pz-coalfield2", "Server", "coalfield2.ini"))
	if !strings.Contains(string(ini), "ResetID=123") || !strings.Contains(string(ini), "ServerPlayerID=456") ||
		strings.Contains(string(ini), "RCONPassword=pw\n") {
		t.Fatalf("ini: %s", ini)
	}
	env, _ := os.ReadFile(filepath.Join(stacksRoot, "pz-coalfield2", ".env"))
	if !strings.Contains(string(env), "ADMIN_USERNAME=boss") || !strings.Contains(string(env), "MEMORY_XMX_GB=6") {
		t.Fatalf("env: %s", env)
	}
	srv, found := app.cfg.Server("pz-coalfield2")
	if !found || srv.RCONPort == 27101 {
		t.Fatalf("new server: %+v", srv)
	}
	// The source is free again.
	if app.dups.copying("coalfield") || app.backup.Running("coalfield") {
		t.Fatal("the source is still held")
	}
}

func TestDuplicateRefusesAnUnusedFolderThatIsNotEmpty(t *testing.T) {
	app, handler, stacksRoot, dataRoot := discoveryApp(t, func(o *Options) { o.GameImage = testImage })
	dir := addStack(t, stacksRoot, dataRoot, "coalfield", "unless-stopped")
	writeFile(t, filepath.Join(dir, ".env"), "SERVER_NAME=coalfield\nRCON_PORT=27015\nRCON_PASSWORD=pw\nADMIN_PASSWORD=hunter22\n")
	writeFile(t, filepath.Join(dataRoot, "coalfield2", "projectzomboid", "config", "stray.txt"), "x")
	app.rescan()
	c := &client{t: t, handler: handler}
	c.setup("rick", "a-long-enough-password")
	rec := c.do(http.MethodPost, "/api/stack/duplicate", map[string]any{"serverId": "coalfield", "name": "pz-coalfield2"})
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "empty folders") {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
}
