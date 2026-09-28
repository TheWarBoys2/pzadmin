package server

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/TheWarBoys2/pzadmin/internal/config"
)

func TestWorldReset(t *testing.T) {
	app, handler, dir := newTestApp(t)
	c := &client{t: t, handler: handler}
	c.setup("rick", "a-long-enough-password")

	root := filepath.Join(dir, "pzroot", "riverside", "projectzomboid")
	ini := filepath.Join(root, "config", "Server", "riv.ini")
	world := filepath.Join(root, "config", "Saves", "Multiplayer")
	accounts := filepath.Join(root, "config", "db", "riv.db")
	mustMkdir(t, filepath.Join(world, "riv"))
	mustMkdir(t, filepath.Dir(accounts))
	mustMkdir(t, filepath.Dir(ini))
	mustWrite(t, ini, "PublicName=Riverside\nMods=ModA\n")
	mustWrite(t, filepath.Join(world, "riv", "map_1_1.bin"), "world")
	mustWrite(t, filepath.Join(world, "riv", "players.db"), "characters")
	mustWrite(t, accounts, "whitelist")
	if _, err := app.cfg.Update(func(cfg *config.Config) error {
		cfg.PZRoot = filepath.Join(dir, "pzroot")
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if rec := seedServer(t, app, `{"name":"Riverside","enabled":false,"host":"127.0.0.1","rconPort":27015,
		"rconPassword":"secret","pzPath":"riverside/projectzomboid","backup":{"enabled":true,"keep":10,"includeConfig":true}}`); rec.Code != http.StatusOK {
		t.Fatalf("seed: %d %s", rec.Code, rec.Body.String())
	}
	id := app.cfg.Get().Servers[0].ID

	info := decode(t, c.do(http.MethodGet, "/api/server/world?id="+id, nil))
	if info["exists"] != true || info["dir"] != world {
		t.Fatalf("world info should find %s: %v", world, info)
	}
	if w, _ := info["worlds"].([]any); len(w) != 1 || w[0] != "riv" {
		t.Fatalf("world info should list the riv world: %v", info["worlds"])
	}

	if rec := c.raw("/api/server/world/reset", `{"serverId":"`+id+`","confirm":"riverside"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("a mistyped confirmation must refuse the reset, got %d", rec.Code)
	}
	if _, err := os.Stat(world); err != nil {
		t.Fatal("a refused reset must leave the world alone")
	}

	app.updateStatus(id, func(st *Status) { st.Online = true })
	if rec := c.raw("/api/server/world/reset", `{"serverId":"`+id+`","confirm":"Riverside"}`); rec.Code != http.StatusConflict {
		t.Fatalf("a running server's world must not be reset, got %d", rec.Code)
	}
	app.updateStatus(id, func(st *Status) { st.Online = false })

	// The browser's payload, backup ticked.
	rec := c.raw("/api/server/world/reset", `{"serverId":"`+id+`","confirm":"Riverside","backup":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("reset: %d %s", rec.Code, rec.Body.String())
	}
	if _, err := os.Stat(world); !os.IsNotExist(err) {
		t.Fatalf("Saves/Multiplayer should be gone, stat says %v", err)
	}
	if _, err := os.Stat(world + ".resetting"); !os.IsNotExist(err) {
		t.Fatal("the moved-aside folder should be deleted too")
	}
	for _, keep := range []string{ini, accounts} {
		if _, err := os.Stat(keep); err != nil {
			t.Fatalf("%s should be kept: %v", keep, err)
		}
	}
	list := app.backup.List(id)
	if len(list) != 1 || list[0].Note != "Before world reset" {
		t.Fatalf("a backup should be taken first: %+v", list)
	}
	if files, _, err := app.backup.Verify(id, list[0].Name); err != nil || files < 2 {
		t.Fatalf("the backup should hold the old world: %d files, %v", files, err)
	}
	var logged bool
	for _, e := range app.store.Events(id, "world.reset", 100) {
		if e.Kind == "world.reset" && e.Actor == "rick" && strings.Contains(e.Detail, "riv") {
			logged = true
		}
	}
	if !logged {
		t.Fatal("the reset should be in the activity log with who did it")
	}

	// Nothing left to reset.
	if rec := c.raw("/api/server/world/reset", `{"serverId":"`+id+`","confirm":"Riverside","backup":false}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("resetting a missing world should say so, got %d", rec.Code)
	}

	// Skipping the backup takes none.
	mustMkdir(t, filepath.Join(world, "riv"))
	mustWrite(t, filepath.Join(world, "riv", "map_1_1.bin"), "new world")
	if rec := c.raw("/api/server/world/reset", `{"serverId":"`+id+`","confirm":"Riverside","backup":false}`); rec.Code != http.StatusOK {
		t.Fatalf("reset without backup: %d %s", rec.Code, rec.Body.String())
	}
	if n := len(app.backup.List(id)); n != 1 {
		t.Fatalf("an unticked backup box should take no backup, have %d", n)
	}
}

func TestWorldResetExplainsAWorldItCannotDelete(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root may delete anything")
	}
	app, handler, dir := newTestApp(t)
	c := &client{t: t, handler: handler}
	c.setup("rick", "a-long-enough-password")

	root := filepath.Join(dir, "pzroot", "riverside", "projectzomboid")
	saves := filepath.Join(root, "config", "Saves")
	locked := filepath.Join(saves, "Multiplayer", "riv")
	mustMkdir(t, locked)
	mustMkdir(t, filepath.Join(root, "config", "Server"))
	mustWrite(t, filepath.Join(root, "config", "Server", "riv.ini"), "PublicName=Riverside\n")
	mustWrite(t, filepath.Join(locked, "players.db"), "characters")
	if err := os.Chmod(locked, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })
	if _, err := app.cfg.Update(func(cfg *config.Config) error {
		cfg.PZRoot = filepath.Join(dir, "pzroot")
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if rec := seedServer(t, app, `{"name":"Riverside","host":"127.0.0.1","rconPort":27015,"rconPassword":"secret",
		"pzPath":"riverside/projectzomboid"}`); rec.Code != http.StatusOK {
		t.Fatalf("seed: %d %s", rec.Code, rec.Body.String())
	}
	id := app.cfg.Get().Servers[0].ID

	rec := c.raw("/api/server/world/reset", `{"serverId":"`+id+`","confirm":"Riverside","backup":true}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("want a refusal, got %d %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{locked, "Nothing was changed", "sudo chown -R", saves} {
		if !strings.Contains(body, want) {
			t.Fatalf("the refusal should mention %q: %s", want, body)
		}
	}
	if n := len(app.backup.List(id)); n != 0 {
		t.Fatalf("no backup should be taken for a reset that cannot happen, have %d", n)
	}
	if _, err := os.Stat(filepath.Join(locked, "players.db")); err != nil {
		t.Fatal("the world must be left alone")
	}
}
