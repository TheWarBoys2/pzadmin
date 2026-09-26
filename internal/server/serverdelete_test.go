package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/TheWarBoys2/pzadmin/internal/config"
)

// containerFake is just enough of Arcane's container API for a delete.
type containerFake struct {
	mu      sync.Mutex
	running bool
	exists  bool
	deleted []string
	query   string
}

func (f *containerFake) server(t *testing.T) string {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		switch {
		case strings.HasSuffix(r.URL.Path, "/containers") && r.Method == http.MethodGet:
			data := []any{}
			if f.exists {
				data = append(data, map[string]any{"id": "c1", "names": []string{"/pz-doomed"}, "state": "exited"})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "data": data})
		case strings.HasSuffix(r.URL.Path, "/containers/c1") && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{
				"name": "pz-doomed", "state": map[string]any{"running": f.running, "status": "exited"}}})
		case strings.HasSuffix(r.URL.Path, "/containers/c1") && r.Method == http.MethodDelete:
			f.deleted = append(f.deleted, "c1")
			f.query = r.URL.RawQuery
			f.exists = false
			_ = json.NewEncoder(w).Encode(map[string]any{"success": true})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func deleteSetup(t *testing.T, f *containerFake) (*App, *client, string, string) {
	t.Helper()
	url := f.server(t)
	app, handler, stacksRoot, dataRoot := discoveryApp(t, func(o *Options) {
		o.ArcaneURL, o.ArcaneEnvID, o.ArcaneAPIKey = url, "0", "k"
	})
	addStack(t, stacksRoot, dataRoot, "doomed", "unless-stopped")
	addStack(t, stacksRoot, dataRoot, "keeper", "unless-stopped")
	app.rescan()
	c := &client{t: t, handler: handler}
	c.setup("rick", "a-long-enough-password")
	return app, c, stacksRoot, dataRoot
}

// stopMonitors ends every server monitor and waits for them to finish, so a
// test can set a server's status without a probe overwriting it.
func stopMonitors(t *testing.T, app *App) {
	t.Helper()
	app.mu.Lock()
	for id, cancel := range app.monitors {
		cancel()
		delete(app.monitors, id)
	}
	app.mu.Unlock()
	if !waitTimeout(&app.wg, 10*time.Second) {
		t.Fatal("the monitors did not stop")
	}
}

func TestDeleteMovesTheStackAndKeepsTheWorldByDefault(t *testing.T) {
	f := &containerFake{exists: true}
	app, c, stacksRoot, dataRoot := deleteSetup(t, f)

	rec := c.do(http.MethodPost, "/api/server/destroy", map[string]any{"id": "doomed", "confirm": "doomed"})
	if rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	if len(f.deleted) != 1 || f.query != "force=false&volumes=false" {
		t.Fatalf("container delete: %v %q", f.deleted, f.query)
	}
	if _, err := os.Stat(filepath.Join(stacksRoot, "doomed")); !os.IsNotExist(err) {
		t.Fatal("the stack folder should have been moved")
	}
	trashed, _ := filepath.Glob(filepath.Join(stacksRoot, trashDir, "doomed-*", "docker-compose.yml.deleted"))
	if len(trashed) != 1 {
		t.Fatal("the stack should be in the trash with its compose file renamed")
	}
	if _, err := os.Stat(filepath.Join(dataRoot, "doomed", "projectzomboid", "data")); err != nil {
		t.Fatal("the world must be kept unless deleting it was asked for")
	}
	for _, s := range app.cfg.Get().Servers {
		if s.ID == "doomed" {
			t.Fatal("the server should be gone from the config")
		}
	}
	// A rescan must not find it again, and the other server is untouched.
	app.rescan()
	servers := app.cfg.Get().Servers
	if len(servers) != 1 || servers[0].ID != "keeper" || servers[0].Missing {
		t.Fatalf("after rescan: %+v", servers)
	}
}

func TestDeleteCanRemoveTheWorldToo(t *testing.T) {
	f := &containerFake{exists: true}
	_, c, _, dataRoot := deleteSetup(t, f)
	rec := c.do(http.MethodPost, "/api/server/destroy", map[string]any{"id": "doomed", "confirm": "doomed", "deleteData": true})
	if rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	if _, err := os.Stat(filepath.Join(dataRoot, "doomed")); !os.IsNotExist(err) {
		t.Fatal("the data folder should be gone")
	}
	if _, err := os.Stat(filepath.Join(dataRoot, "keeper", "projectzomboid", "data")); err != nil {
		t.Fatal("another server's data must be untouched")
	}
}

func TestDeleteRefusesARunningServer(t *testing.T) {
	f := &containerFake{exists: true, running: true}
	app, c, stacksRoot, _ := deleteSetup(t, f)
	rec := c.do(http.MethodPost, "/api/server/destroy", map[string]any{"id": "doomed", "confirm": "doomed"})
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "Stop it first") {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	// The monitor's first probe gets no RCON answer and would mark the server
	// offline, so stop it before pretending the game is answering.
	stopMonitors(t, app)
	app.updateStatus("doomed", func(st *Status) { st.Online = true })
	f.mu.Lock()
	f.running = false
	f.mu.Unlock()
	rec = c.do(http.MethodPost, "/api/server/destroy", map[string]any{"id": "doomed", "confirm": "doomed"})
	if rec.Code != http.StatusConflict {
		t.Fatalf("a server answering RCON must not be deleted: %d", rec.Code)
	}
	if len(f.deleted) != 0 {
		t.Fatal("nothing should have been deleted")
	}
	if _, err := os.Stat(filepath.Join(stacksRoot, "doomed")); err != nil {
		t.Fatal("the stack folder must be untouched")
	}
}

func TestDeleteNeedsTheNameTyped(t *testing.T) {
	f := &containerFake{exists: true}
	_, c, _, _ := deleteSetup(t, f)
	rec := c.do(http.MethodPost, "/api/server/destroy", map[string]any{"id": "doomed", "confirm": "keeper"})
	if rec.Code != http.StatusBadRequest || len(f.deleted) != 0 {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
}

func TestDeleteOfANeverCreatedContainerStillWorks(t *testing.T) {
	f := &containerFake{exists: false}
	_, c, _, _ := deleteSetup(t, f)
	rec := c.do(http.MethodPost, "/api/server/destroy", map[string]any{"id": "doomed", "confirm": "doomed"})
	if rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
}

func TestDataIsNeverDeletedWhenShared(t *testing.T) {
	cfg := config.Config{PZRoot: "/srv/zomboid", Servers: []config.Server{
		{ID: "a", Name: "a", PZPath: "/srv/zomboid/a/projectzomboid"},
		{ID: "b", Name: "b", PZPath: "/srv/zomboid/b/projectzomboid", DataDir: "/srv/zomboid/a/projectzomboid/data"},
	}}
	if _, err := dataToDelete(cfg.Servers[0], cfg); err == nil {
		t.Fatal("a folder another server mounts must not be deleted")
	}
	if _, err := dataToDelete(config.Server{Name: "c", PZPath: "/srv/zomboid"}, cfg); err == nil {
		t.Fatal("the data root itself must never be deleted")
	}
	if _, err := dataToDelete(config.Server{Name: "d", PZPath: "/home/rick"}, cfg); err == nil {
		t.Fatal("nothing outside the data root may be deleted")
	}
	if got, err := dataToDelete(cfg.Servers[1], cfg); err != nil || got != "/srv/zomboid/b/projectzomboid" {
		t.Fatalf("%q %v", got, err)
	}
}

func TestTrashFolderMustBeInsideTheStacksFolder(t *testing.T) {
	if _, err := stackToTrash(config.Server{Name: "x", StackDir: "/etc"}, "/home/rick/stacks", time.Now()); err == nil {
		t.Fatal("a stack outside the stacks folder must be refused")
	}
}
