package server

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/TheWarBoys2/pzadmin/internal/config"
)

// discoveryApp builds an app over a stacks root and data root laid out like
// The standard layout: one stack folder per server, bulk data under the data root.
func discoveryApp(t *testing.T, tweak ...func(*Options)) (*App, http.Handler, string, string) {
	t.Helper()
	dir := t.TempDir()
	stacksRoot := filepath.Join(dir, "home/rick/docker/pzserver")
	dataRoot := filepath.Join(dir, "srv/zomboid")
	for _, d := range []string{stacksRoot, dataRoot} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	opts := Options{
		DataDir:    filepath.Join(dir, "data"),
		Assets:     fstest.MapFS{"index.html": &fstest.MapFile{Data: []byte("x")}},
		DataRoot:   dataRoot,
		StacksRoot: stacksRoot,
	}
	for _, f := range tweak {
		f(&opts)
	}
	app, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(app.Close)
	return app, app.Handler(), stacksRoot, dataRoot
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func addStack(t *testing.T, stacksRoot, dataRoot, name, restart string) string {
	t.Helper()
	dir := filepath.Join(stacksRoot, name)
	base := filepath.Join(dataRoot, name, "projectzomboid")
	writeFile(t, filepath.Join(base, "data", "start-server.sh"), "")
	writeFile(t, filepath.Join(base, "config", "Logs", "log.txt"), "")
	writeFile(t, filepath.Join(dir, ".env"), "SERVER_NAME="+name+"\nRCON_PORT=27015\nRCON_PASSWORD=pw\n")
	writeFile(t, filepath.Join(dir, "Server", name+".ini"), "RCONPort=27015\nRCONPassword=pw\nPVP=true\nMods=A\n")
	writeFile(t, filepath.Join(dir, "Server", name+"_SandboxVars.lua"), "SandboxVars = {\n    Zombies = 3,\n}\n")
	restartLine := ""
	if restart != "" {
		restartLine = "    restart: " + restart + "\n"
	}
	writeFile(t, filepath.Join(dir, "docker-compose.yml"), "services:\n  pz:\n"+
		"    image: indifferentbroccoli/projectzomboid-server-docker:1.1.9\n"+
		"    stop_grace_period: 120s\n"+
		"    container_name: pz-"+name+"\n"+restartLine+
		"    env_file:\n      - .env\n"+
		"    ports:\n      - \"27101:27015/tcp\"\n      - \"16261:16261/udp\"\n"+
		"    volumes:\n"+
		"      - "+filepath.Join(base, "data")+":/project-zomboid\n"+
		"      - "+filepath.Join(base, "config")+":/project-zomboid-config\n"+
		"      - "+filepath.Join(dir, "Server")+":/project-zomboid-config/Server\n")
	return dir
}

func TestRescanCreatesServersFromStackFolders(t *testing.T) {
	app, _, stacksRoot, dataRoot := discoveryApp(t)
	addStack(t, stacksRoot, dataRoot, "coalfield", "unless-stopped")
	app.rescan()

	servers := app.cfg.Get().Servers
	if len(servers) != 1 {
		t.Fatalf("servers: %+v", servers)
	}
	s := servers[0]
	if s.ID != "coalfield" || s.DockerContainer != "pz-coalfield" || s.RCONPort != 27101 ||
		s.RCONPassword != "pw" || s.Host != "host.docker.internal" || s.RestartPolicy != "unless-stopped" {
		t.Fatalf("discovered fields: %+v", s)
	}
	if s.PZPath != filepath.Join(dataRoot, "coalfield", "projectzomboid") {
		t.Fatalf("pzPath: %s", s.PZPath)
	}
	layout := detectLayout(s)
	if layout.ConfigDir != filepath.Join(stacksRoot, "coalfield", "Server") {
		t.Fatalf("config files must come from the stack's Server/: %s", layout.ConfigDir)
	}
	if layout.LogsDir == "" {
		t.Fatal("logs should be found under the config folder")
	}
	if !app.cfg.ContainerAllowed("pz-coalfield") {
		t.Fatal("a discovered container should be controllable")
	}

	// Operator settings survive the next scan; discovered ones are refreshed.
	if _, err := app.cfg.Update(func(c *config.Config) error {
		c.Servers[0].Name = "Coalfield Western"
		c.Servers[0].RCONPort = 1
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	app.rescan()
	s = app.cfg.Get().Servers[0]
	if s.Name != "Coalfield Western" || s.RCONPort != 27101 {
		t.Fatalf("rescan: %+v", s)
	}
}

func TestRemovedStackIsMarkedMissingAndLocked(t *testing.T) {
	app, handler, stacksRoot, dataRoot := discoveryApp(t)
	dir := addStack(t, stacksRoot, dataRoot, "knight", "unless-stopped")
	app.rescan()
	c := &client{t: t, handler: handler}
	c.setup("rick", "a-long-enough-password")

	rec := c.do(http.MethodPost, "/api/server/delete", map[string]any{"id": "knight"})
	if rec.Code != http.StatusConflict {
		t.Fatalf("deleting a server whose folder exists should be refused: %d %s", rec.Code, rec.Body.String())
	}

	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	app.rescan()
	s := app.cfg.Get().Servers[0]
	if !s.Missing {
		t.Fatal("a vanished stack should be marked missing")
	}
	if app.cfg.ContainerAllowed("pz-knight") {
		t.Fatal("a missing server's container must not be controllable")
	}
	rec = c.do(http.MethodPost, "/api/server/delete", map[string]any{"id": "knight"})
	if rec.Code != http.StatusOK {
		t.Fatalf("a missing server should be removable: %s", rec.Body.String())
	}

	// Putting the folder back brings it back.
	addStack(t, stacksRoot, dataRoot, "knight", "unless-stopped")
	app.rescan()
	if got := app.cfg.Get().Servers; len(got) != 1 || got[0].Missing {
		t.Fatalf("restored stack: %+v", got)
	}
}

// Starting a container whose bind source has gone boots a blank world. The
// check runs on the files as they are now, before Arcane is asked anything.
func TestStartRefusesAnEmptyBindSource(t *testing.T) {
	app, handler, stacksRoot, dataRoot := discoveryApp(t)
	addStack(t, stacksRoot, dataRoot, "muldraugh", "unless-stopped")
	app.rescan()
	c := &client{t: t, handler: handler}
	c.setup("rick", "a-long-enough-password")

	data := filepath.Join(dataRoot, "muldraugh", "projectzomboid", "data")
	if err := os.RemoveAll(data); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(data, 0o755); err != nil {
		t.Fatal(err)
	}
	rec := c.do(http.MethodPost, "/api/lifecycle", map[string]any{"serverId": "muldraugh", "action": "start"})
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "empty") {
		t.Fatalf("got %d %s", rec.Code, rec.Body.String())
	}
}

func TestRCONRestartRefusedWithoutARevivingPolicy(t *testing.T) {
	app, _, _, _ := discoveryApp(t)
	for _, policy := range []string{"", "no", "on-failure"} {
		srv := config.Server{ID: "x", Name: "X", Host: "127.0.0.1", RCONPort: 1, RestartPolicy: policy}
		err := app.restartServer(context.Background(), srv, "test", "test", "")
		if err == nil || !strings.Contains(err.Error(), "unless-stopped") {
			t.Fatalf("policy %q: %v", policy, err)
		}
		if app.statusOf("x").Restarting {
			t.Fatalf("policy %q: a refused restart must not leave the server marked restarting", policy)
		}
	}
	for _, policy := range []string{"unless-stopped", "always"} {
		if !restartPolicyRevives(policy) {
			t.Fatalf("%s should revive", policy)
		}
	}
}

func TestArcaneStateExplainsMissingConfiguration(t *testing.T) {
	app, _, _, _ := discoveryApp(t)
	st := app.arcaneState()
	if st["configured"] != false || !strings.Contains(st["message"].(string), "PZADMIN_ARCANE_URL") {
		t.Fatalf("%+v", st)
	}
}

// An imported file carries settings onto servers that exist here. It must
// never be able to add a server, because a server's container name is what
// puts a container on the allowlist.
func TestImportCannotAddServersOrContainers(t *testing.T) {
	app, handler, stacksRoot, dataRoot := discoveryApp(t)
	addStack(t, stacksRoot, dataRoot, "knight", "unless-stopped")
	app.rescan()
	c := &client{t: t, handler: handler}
	c.setup("rick", "a-long-enough-password")

	rec := c.do(http.MethodPost, "/api/import", map[string]any{
		"servers": []map[string]any{
			{"id": "evil", "name": "Evil", "dockerContainer": "portainer", "host": "10.0.0.1", "stack": "nope"},
			{"id": "old-knight-id", "name": "The Knight", "stack": "knight", "dockerContainer": "portainer",
				"enabled": true, "recovery": map[string]any{"failuresBeforeRestart": 9, "cooldownMinutes": 5}},
		},
		"schedules": []map[string]any{
			{"name": "Nightly", "serverId": "old-knight-id", "kind": "save", "cron": "0 4 * * *", "enabled": true},
			{"name": "Orphan", "serverId": "evil", "kind": "save", "cron": "0 4 * * *", "enabled": true},
		},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	cfg := app.cfg.Get()
	if len(cfg.Servers) != 1 {
		t.Fatalf("an import added a server: %+v", cfg.Servers)
	}
	s := cfg.Servers[0]
	if s.Name != "The Knight" || s.Recovery.FailuresBeforeRestart != 9 || s.DockerContainer != "pz-knight" {
		t.Fatalf("settings should apply, the container must not: %+v", s)
	}
	if app.cfg.ContainerAllowed("portainer") {
		t.Fatal("an imported file put a container on the allowlist")
	}
	if len(cfg.Schedules) != 1 || cfg.Schedules[0].ServerID != "knight" {
		t.Fatalf("schedules should be remapped, orphans dropped: %+v", cfg.Schedules)
	}
}

func TestFoldersOutsideTheManagedRootsAreNeverStored(t *testing.T) {
	app, _, stacksRoot, dataRoot := discoveryApp(t)
	dir := addStack(t, stacksRoot, dataRoot, "sneaky", "unless-stopped")
	b, _ := os.ReadFile(filepath.Join(dir, "docker-compose.yml"))
	writeFile(t, filepath.Join(dir, "docker-compose.yml"),
		strings.Replace(string(b), filepath.Join(dir, "Server")+":", "/etc:", 1))
	app.rescan()
	s, found := app.cfg.Server("sneaky")
	if !found {
		t.Fatal("the server should still be listed, with its problem")
	}
	if s.ServerDir == "/etc" || strings.HasPrefix(detectLayout(s).ConfigDir, "/etc") {
		t.Fatalf("a folder outside the managed roots was stored: %+v", s)
	}
}
