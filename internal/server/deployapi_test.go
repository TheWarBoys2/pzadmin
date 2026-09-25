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

	"github.com/TheWarBoys2/pzadmin/internal/arcane"
)

func TestProjectMatching(t *testing.T) {
	root := "/home/rick/docker/pzserver"
	for _, c := range []struct {
		p    arcane.Project
		want bool
	}{
		{arcane.Project{DirName: "pz-new", RelativePath: "pzserver/pz-new"}, true},
		{arcane.Project{DirName: "pz-new", RelativePath: "pz-new"}, true},
		{arcane.Project{DirName: "pz-new", RelativePath: "old/pzserver/pz-new"}, true},
		{arcane.Project{DirName: "pz-new", RelativePath: "elsewhere/pz-new"}, false},
		{arcane.Project{DirName: "pz-new2", RelativePath: "pzserver/pz-new2"}, false},
	} {
		if got := projectMatches(c.p, root, "pz-new"); got != c.want {
			t.Errorf("%+v: %v", c.p, got)
		}
	}
}

type deployFake struct {
	mu       sync.Mutex
	projects []arcane.Project
	deployed []string
}

func (f *deployFake) server(t *testing.T) string {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		switch {
		case strings.HasSuffix(r.URL.Path, "/projects"):
			_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "data": f.projects})
		case strings.HasSuffix(r.URL.Path, "/up"):
			parts := strings.Split(r.URL.Path, "/")
			f.deployed = append(f.deployed, parts[len(parts)-2])
			_, _ = w.Write([]byte("Container Started\n"))
		case strings.HasSuffix(r.URL.Path, "/containers"):
			_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "data": []any{}})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func TestDeployRunsComposeUpThroughArcane(t *testing.T) {
	f := &deployFake{}
	url := f.server(t)
	app, handler, stacksRoot, dataRoot := discoveryApp(t, func(o *Options) {
		o.ArcaneURL, o.ArcaneEnvID, o.ArcaneAPIKey = url, "0", "k"
	})
	addStack(t, stacksRoot, dataRoot, "newbie", "unless-stopped")
	app.rescan()
	f.projects = []arcane.Project{
		{ID: "p-other", Name: "vaultwarden", DirName: "vaultwarden", RelativePath: "vaultwarden"},
		{ID: "p-new", Name: "newbie", DirName: "newbie", RelativePath: filepath.Base(stacksRoot) + "/newbie"},
	}
	c := &client{t: t, handler: handler}
	c.setup("rick", "a-long-enough-password")

	if app.allowProject(f.projects[0]) {
		t.Fatal("a project that is not a server must not be deployable")
	}
	rec := c.do(http.MethodPost, "/api/stack/deploy", map[string]any{"serverId": "newbie"})
	if rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	deadline := time.Now().Add(5 * time.Second)
	for app.statusOf("newbie").Deploying && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.deployed) != 1 || f.deployed[0] != "p-new" {
		t.Fatalf("deployed %v", f.deployed)
	}
	if !app.statusOf("newbie").Restarting {
		t.Fatal("a deployed server should be in its start window, not an outage")
	}
}

func TestDeployRefusesAStackThatIsNotReady(t *testing.T) {
	f := &deployFake{}
	url := f.server(t)
	app, handler, stacksRoot, dataRoot := discoveryApp(t, func(o *Options) {
		o.ArcaneURL, o.ArcaneEnvID, o.ArcaneAPIKey = url, "0", "k"
	})
	dir := addStack(t, stacksRoot, dataRoot, "broken", "unless-stopped")
	app.rescan()
	// The data folder is emptied after discovery: Compose would mount an
	// empty folder and the game would install into a blank world.
	data := filepath.Join(dataRoot, "broken", "projectzomboid", "data")
	_ = os.RemoveAll(data)
	_ = os.MkdirAll(data, 0o755)
	_ = dir
	c := &client{t: t, handler: handler}
	c.setup("rick", "a-long-enough-password")
	rec := c.do(http.MethodPost, "/api/stack/deploy", map[string]any{"serverId": "broken"})
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "empty") {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	if len(f.deployed) != 0 {
		t.Fatal("nothing should have been deployed")
	}
}
