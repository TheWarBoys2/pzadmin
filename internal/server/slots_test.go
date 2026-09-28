package server

import (
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/TheWarBoys2/pzadmin/internal/arcane"
	"github.com/TheWarBoys2/pzadmin/internal/compose"
	"github.com/TheWarBoys2/pzadmin/internal/config"
)

func TestPickSlotPrefersTheLastOne(t *testing.T) {
	slots := config.PortSlots{FirstPort: 16261, Count: 2}
	srv := config.Server{ID: "a", GamePort: 16263, UDPPort: 16264}
	if sl, ok := pickSlot(slots, srv, map[int]config.Server{}); !ok || sl.Number != 2 {
		t.Fatalf("got %+v %v, want slot 2", sl, ok)
	}
	busy := map[int]config.Server{2: {ID: "b", Name: "B"}}
	if sl, ok := pickSlot(slots, srv, busy); !ok || sl.Number != 1 || sl.GamePort != 16261 || sl.UDPPort != 16262 {
		t.Fatalf("got %+v %v, want slot 1", sl, ok)
	}
	busy[1] = config.Server{ID: "c", Name: "C"}
	if _, ok := pickSlot(slots, srv, busy); ok {
		t.Fatal("no slot is free")
	}
	msg := slotsBusyMessage(slots, busy, principal{Owner: true})
	if !strings.Contains(msg, "C on 16261") || !strings.Contains(msg, "B on 16263") {
		t.Fatal(msg)
	}
}

func TestStoppedServersHoldNoSlot(t *testing.T) {
	slots := config.PortSlots{FirstPort: 16261, Count: 2}
	s := config.Server{ID: "a", GamePort: 16261, UDPPort: 16262}
	for _, c := range []struct {
		st   Status
		want int
	}{
		{Status{ContainerState: "running"}, 1},
		{Status{Restarting: true}, 1},
		{Status{Deploying: true}, 1},
		{Status{ContainerState: "exited"}, 0},
		{Status{ContainerState: "running", Stopped: true}, 0},
		{Status{ContainerState: "not created"}, 0},
	} {
		if got := slotHeld(slots, s, c.st); got != c.want {
			t.Errorf("%+v: slot %d, want %d", c.st, got, c.want)
		}
	}
	if got := slotHeld(slots, config.Server{GamePort: 16271}, Status{ContainerState: "running"}); got != 0 {
		t.Errorf("a port outside the range holds no slot, got %d", got)
	}
}

// slotStack is addStack with both game ports mapped, as a real stack has.
func slotStack(t *testing.T, stacksRoot, dataRoot, name string, game int) {
	t.Helper()
	dir := addStack(t, stacksRoot, dataRoot, name, "unless-stopped")
	path := filepath.Join(dir, "docker-compose.yml")
	b, _ := os.ReadFile(path)
	g, u := strconv.Itoa(game), strconv.Itoa(game+1)
	text := strings.Replace(string(b), `"16261:16261/udp"`,
		`"`+g+`:`+g+`/udp"`+"\n      - \""+u+":"+u+"/udp\"   # second game port", 1)
	writeFile(t, path, text)
	env, _ := os.ReadFile(filepath.Join(dir, ".env"))
	writeFile(t, filepath.Join(dir, ".env"), string(env)+"DEFAULT_PORT="+g+"\nUDP_PORT="+u+"\n")
}

func TestStartTakesAFreeSlotAndRefusesWhenAllAreBusy(t *testing.T) {
	f := &deployFake{}
	url := f.server(t)
	app, handler, stacksRoot, dataRoot := discoveryApp(t, func(o *Options) {
		o.ArcaneURL, o.ArcaneEnvID, o.ArcaneAPIKey = url, "0", "k"
	})
	base := filepath.Base(stacksRoot)
	for i, name := range []string{"alpha", "bravo", "charlie"} {
		slotStack(t, stacksRoot, dataRoot, name, 16271+10*i)
		f.projects = append(f.projects, arcane.Project{ID: "p-" + name, Name: name, DirName: name, RelativePath: base + "/" + name})
	}
	app.rescan()
	c := &client{t: t, handler: handler}
	c.setup("rick", "a-long-enough-password")

	// Ticking the box before slots exist is refused.
	if rec := c.do(http.MethodPost, "/api/server/save", map[string]any{"id": "alpha", "useSlot": true}); rec.Code != http.StatusBadRequest {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	if rec := c.do(http.MethodPost, "/api/settings", map[string]any{"portSlots": map[string]any{"firstPort": 16261, "count": 99}}); rec.Code != http.StatusBadRequest {
		t.Fatalf("too many slots: %d", rec.Code)
	}
	if rec := c.do(http.MethodPost, "/api/settings", map[string]any{"portSlots": map[string]any{"firstPort": 16261, "count": 2}}); rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	for _, name := range []string{"alpha", "bravo", "charlie"} {
		rec := c.do(http.MethodPost, "/api/server/save", map[string]any{"id": name, "useSlot": true})
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", name, rec.Code, rec.Body.String())
		}
		b, _ := os.ReadFile(filepath.Join(stacksRoot, name, "docker-compose.yml"))
		if !strings.Contains(string(b), `"${DEFAULT_PORT}:${DEFAULT_PORT}/udp"`) ||
			!strings.Contains(string(b), `"${UDP_PORT}:${UDP_PORT}/udp"   # second game port`) {
			t.Fatalf("%s compose not switched to .env ports:\n%s", name, b)
		}
		matches, _ := filepath.Glob(filepath.Join(stacksRoot, name, "docker-compose.yml.before-slots-*"))
		if len(matches) != 1 {
			t.Fatalf("%s: the old compose file should be kept, found %v", name, matches)
		}
	}
	// Nothing moved yet: the ports are what they were.
	if s, _ := app.cfg.Server("bravo"); s.GamePort != 16281 || !s.UseSlot {
		t.Fatalf("bravo %+v", s)
	}

	start := func(name string) (int, string) {
		rec := c.do(http.MethodPost, "/api/lifecycle", map[string]any{"serverId": name, "action": "start"})
		deadline := time.Now().Add(5 * time.Second)
		for app.statusOf(name).Deploying && time.Now().Before(deadline) {
			time.Sleep(20 * time.Millisecond)
		}
		return rec.Code, rec.Body.String()
	}
	envPort := func(name string) string {
		env, err := compose.ReadEnvFile(filepath.Join(stacksRoot, name, ".env"))
		if err != nil {
			t.Fatal(err)
		}
		return env.Lookup("DEFAULT_PORT") + "/" + env.Lookup("UDP_PORT")
	}

	if code, body := start("alpha"); code != http.StatusOK || !strings.Contains(body, "slot 1") {
		t.Fatalf("alpha: %d %s", code, body)
	}
	if envPort("alpha") != "16261/16262" {
		t.Fatalf("alpha .env %s", envPort("alpha"))
	}
	if s, _ := app.cfg.Server("alpha"); s.GamePort != 16261 || joinPort(s) != 16261 {
		t.Fatalf("alpha after start %+v", s)
	}
	if code, body := start("bravo"); code != http.StatusOK || !strings.Contains(body, "slot 2") {
		t.Fatalf("bravo: %d %s", code, body)
	}
	if envPort("bravo") != "16263/16264" {
		t.Fatalf("bravo .env %s", envPort("bravo"))
	}
	code, body := start("charlie")
	if code != http.StatusConflict || !strings.Contains(body, "alpha on 16261") || !strings.Contains(body, "bravo on 16263") {
		t.Fatalf("charlie should be refused: %d %s", code, body)
	}
	if envPort("charlie") != "16291/16292" {
		t.Fatalf("a refused start must not touch .env, got %s", envPort("charlie"))
	}

	// alpha stops; charlie gets its slot.
	app.clearRestarting("alpha")
	app.updateStatus("alpha", func(st *Status) { st.Stopped = true; st.ContainerState = "exited" })
	if code, body := start("charlie"); code != http.StatusOK || !strings.Contains(body, "slot 1") {
		t.Fatalf("charlie: %d %s", code, body)
	}
	f.mu.Lock()
	deployed := strings.Join(f.deployed, ",")
	f.mu.Unlock()
	if deployed != "p-alpha,p-bravo,p-charlie" {
		t.Fatalf("deployed %s", deployed)
	}
	var backups []string
	_ = filepath.Walk(app.backup.Dir("alpha"), func(p string, info os.FileInfo, err error) error {
		if err == nil && strings.HasPrefix(filepath.Base(p), ".env-") {
			backups = append(backups, p)
		}
		return nil
	})
	if len(backups) == 0 {
		t.Fatal("the .env PZAdmin changed should have been kept")
	}
	for _, st := range app.allStatus() {
		want := map[string]int{"alpha": 0, "bravo": 2, "charlie": 1}[st.ServerID]
		if st.Slot != want {
			t.Errorf("%s shows slot %d, want %d", st.ServerID, st.Slot, want)
		}
	}
}

func TestSlotStartRefusesAComposeFileWithFixedPorts(t *testing.T) {
	f := &deployFake{}
	url := f.server(t)
	app, handler, stacksRoot, dataRoot := discoveryApp(t, func(o *Options) {
		o.ArcaneURL, o.ArcaneEnvID, o.ArcaneAPIKey = url, "0", "k"
	})
	slotStack(t, stacksRoot, dataRoot, "fixed", 16271)
	app.rescan()
	c := &client{t: t, handler: handler}
	c.setup("rick", "a-long-enough-password")
	c.do(http.MethodPost, "/api/settings", map[string]any{"portSlots": map[string]any{"firstPort": 16261, "count": 2}})
	c.do(http.MethodPost, "/api/server/save", map[string]any{"id": "fixed", "useSlot": true})
	// Someone puts the fixed ports back by hand afterwards.
	path := filepath.Join(stacksRoot, "fixed", "docker-compose.yml")
	b, _ := os.ReadFile(path)
	text := strings.NewReplacer(`"${DEFAULT_PORT}:${DEFAULT_PORT}/udp"`, `"16271:16271/udp"`,
		`"${UDP_PORT}:${UDP_PORT}/udp"`, `"16272:16272/udp"`).Replace(string(b))
	writeFile(t, path, text)

	rec := c.do(http.MethodPost, "/api/lifecycle", map[string]any{"serverId": "fixed", "action": "start"})
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "fixed game ports") {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	env, _ := compose.ReadEnvFile(filepath.Join(stacksRoot, "fixed", ".env"))
	if env.Lookup("DEFAULT_PORT") != "16271" {
		t.Fatalf(".env should be put back, has %s", env.Lookup("DEFAULT_PORT"))
	}
	if len(f.deployed) != 0 {
		t.Fatal("nothing should have been deployed")
	}
}
