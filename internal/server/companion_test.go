package server

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/TheWarBoys2/pzadmin/internal/config"
)

// companionSample is a snapshot written by the real mod script under
// mod/test/run.lua's stubbed game, so the two sides agree on the format.
func companionSample(t *testing.T, generatedAt time.Time) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "companion_snapshot.json"))
	if err != nil {
		t.Fatal(err)
	}
	return strings.Replace(string(b), `"generatedAtMs":1000000`,
		`"generatedAtMs":`+strconv.FormatInt(generatedAt.UnixMilli(), 10), 1)
}

func TestCompanionDir(t *testing.T) {
	if got := companionDir(config.Server{ConfigDir: "/srv/zomboid/a/projectzomboid/config"}); got != "/srv/zomboid/a/projectzomboid/config/Lua" {
		t.Fatalf("stack server: %q", got)
	}

	// A server added by folder: the Zomboid folder is the one holding Logs.
	root := t.TempDir()
	zomboid := filepath.Join(root, "Zomboid")
	mustMkdir(t, filepath.Join(zomboid, "Server"))
	mustMkdir(t, filepath.Join(zomboid, "Logs"))
	mustWrite(t, filepath.Join(zomboid, "Server", "s.ini"), "PublicName=x\n")
	if got := companionDir(config.Server{PZPath: root}); got != filepath.Join(zomboid, "Lua") {
		t.Fatalf("folder server: %q", got)
	}

	if got := companionDir(config.Server{PZPath: t.TempDir()}); got != "" {
		t.Fatalf("an empty folder has no Zomboid folder, got %q", got)
	}
}

func TestReadCompanionRejectsAHalfWrittenFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), companionSnapshotFile)
	whole := companionSample(t, time.Now())
	mustWrite(t, path, whole[:len(whole)/2])
	if _, err := readCompanion(path); err == nil {
		t.Fatal("half a snapshot must not parse")
	}
	// Valid JSON that lacks the closing newline is still mid-write.
	mustWrite(t, path, strings.TrimSuffix(whole, "\n"))
	if _, err := readCompanion(path); err == nil {
		t.Fatal("a snapshot without its closing newline must not be trusted")
	}
	mustWrite(t, path, whole)
	snap, err := readCompanion(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Players) != 2 || snap.Players[0].Username != `bob"the\builder` || *snap.Players[0].X != 10890 {
		t.Fatalf("players read wrongly: %+v", snap.Players)
	}
	if snap.World.Hour == nil || *snap.World.Hour != 14 || *snap.World.DayNumber != 23 || snap.World.Season != "Summer" {
		t.Fatalf("world read wrongly: %+v", snap.World)
	}
	// A player the mod could read little about keeps nil, not zero.
	if snap.Players[1].X != nil || snap.Players[1].Health != nil {
		t.Fatalf("missing fields should stay unset: %+v", snap.Players[1])
	}
}

func companionApp(t *testing.T) (*App, *client, string, string) {
	t.Helper()
	app, handler, dir := newTestApp(t)
	c := &client{t: t, handler: handler}
	c.setup("rick", "a-long-enough-password")
	configDir := filepath.Join(dir, "zomboid", "config")
	mustMkdir(t, configDir)
	seedServer(t, app, `{"name":"Riverside","enabled":false,"host":"127.0.0.1","rconPort":1,"rconPassword":"x","configDir":`+
		strconv.Quote(configDir)+`}`)
	return app, c, app.cfg.Get().Servers[0].ID, filepath.Join(configDir, "Lua")
}

func TestCompanionViewAndLiveFlag(t *testing.T) {
	_, c, id, lua := companionApp(t)

	// Before the mod has run: not found, no error.
	out := decode(t, c.do(http.MethodGet, "/api/server/companion?id="+id, nil))
	if out["found"] != false || out["error"] != nil || out["dir"] != lua {
		t.Fatalf("before the mod runs: %v", out)
	}

	// Live mode cannot start before the game has made Zomboid/Lua, and
	// PZAdmin must not make it: it would own it, not the game.
	rec := c.do(http.MethodPost, "/api/server/companion/live", map[string]any{"serverId": id, "on": true})
	if rec.Code != http.StatusConflict {
		t.Fatalf("live without the folder: %d %s", rec.Code, rec.Body.String())
	}
	if _, err := os.Stat(lua); !os.IsNotExist(err) {
		t.Fatal("PZAdmin created the game's Lua folder")
	}

	mustMkdir(t, lua)
	mustWrite(t, filepath.Join(lua, companionSnapshotFile), companionSample(t, time.Now().Add(-10*time.Second)))
	out = decode(t, c.do(http.MethodGet, "/api/server/companion?id="+id, nil))
	if out["found"] != true || out["fresh"] != true {
		t.Fatalf("with a recent snapshot: %v", out)
	}

	rec = c.do(http.MethodPost, "/api/server/companion/live", map[string]any{"serverId": id, "on": true})
	if rec.Code != http.StatusOK {
		t.Fatalf("live on: %d %s", rec.Code, rec.Body.String())
	}
	flag := filepath.Join(lua, companionLiveFile)
	st, err := os.Stat(flag)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o644 {
		t.Fatalf("the game must be able to read the flag, mode %v", st.Mode().Perm())
	}
	until := readLiveFlag(lua)
	if d := time.Until(until); d < 10*time.Second || d > companionLiveLease+time.Second {
		t.Fatalf("flag runs until %v, %v from now", until, d)
	}
	out = decode(t, c.do(http.MethodGet, "/api/server/companion?id="+id, nil))
	if out["liveUntil"] == nil {
		t.Fatalf("the view should report live mode: %v", out)
	}

	rec = c.do(http.MethodPost, "/api/server/companion/live", map[string]any{"serverId": id, "on": false})
	if rec.Code != http.StatusOK {
		t.Fatalf("live off: %d %s", rec.Code, rec.Body.String())
	}
	if _, err := os.Stat(flag); !os.IsNotExist(err) {
		t.Fatal("turning live mode off should remove the flag")
	}

	// An old snapshot is found but not fresh.
	mustWrite(t, filepath.Join(lua, companionSnapshotFile), companionSample(t, time.Now().Add(-time.Hour)))
	out = decode(t, c.do(http.MethodGet, "/api/server/companion?id="+id, nil))
	if out["found"] != true || out["fresh"] != false {
		t.Fatalf("with an hour-old snapshot: %v", out)
	}
}

func TestAPIServerCarriesWorldButNoPositions(t *testing.T) {
	_, c, id, lua := companionApp(t)
	key, _ := createKey(t, c, map[string]any{"name": "bot"})

	get := func() map[string]any {
		rec := bearer(t, c.handler, key, http.MethodGet, "/api/v1/servers/"+id, "")
		if rec.Code != http.StatusOK {
			t.Fatalf("GET server: %d %s", rec.Code, rec.Body.String())
		}
		return decode(t, rec)
	}
	if _, has := get()["world"]; has {
		t.Fatal("no snapshot, so no world")
	}

	mustMkdir(t, lua)
	mustWrite(t, filepath.Join(lua, companionSnapshotFile), companionSample(t, time.Now()))
	out := get()
	world, _ := out["world"].(map[string]any)
	if world == nil || world["hour"] != float64(14) || world["season"] != "Summer" {
		t.Fatalf("world missing or wrong: %v", out["world"])
	}
	body, _ := json.Marshal(out)
	for _, leak := range []string{"10890", "9412", "carpenter", `"x"`} {
		if strings.Contains(string(body), leak) {
			t.Fatalf("the API must not carry player positions or details, found %s in %s", leak, body)
		}
	}

	// Stale world data is left out rather than shown as current.
	mustWrite(t, filepath.Join(lua, companionSnapshotFile), companionSample(t, time.Now().Add(-time.Hour)))
	if _, has := get()["world"]; has {
		t.Fatal("an hour-old snapshot should not be reported")
	}
}
