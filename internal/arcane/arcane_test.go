package arcane

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// fake mimics the parts of Arcane 2.13.1 PZAdmin uses, with the response
// shapes from its OpenAPI spec.
type fake struct {
	mu         sync.Mutex
	key        string
	env        string
	containers []containerSummary
	denied     map[string]bool // permission -> 403
	calls      []string
	logBody    []byte
	listCalls  int
	projects   []Project
	deployed   []string
	deployBody string
}

func (f *fake) handler(t *testing.T) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.calls = append(f.calls, r.Method+" "+r.URL.Path)

		problem := func(code int, detail string) {
			w.Header().Set("Content-Type", "application/problem+json")
			w.WriteHeader(code)
			_ = json.NewEncoder(w).Encode(map[string]any{"title": http.StatusText(code), "status": code, "detail": detail})
		}
		if r.Header.Get("X-API-Key") != f.key {
			problem(401, "Unauthorized: invalid API key")
			return
		}
		prefix := "/api/environments/" + f.env
		if !strings.HasPrefix(r.URL.Path, prefix+"/") {
			problem(404, "environment not found")
			return
		}
		rest := strings.TrimPrefix(r.URL.Path, prefix)
		if f.denied[PermissionFor(r.URL.Path)] {
			problem(403, "Forbidden")
			return
		}

		switch {
		case rest == "/projects" && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "data": f.projects,
				"pagination": map[string]any{"totalItems": len(f.projects)}})
		case strings.HasPrefix(rest, "/projects/") && strings.HasSuffix(rest, "/up"):
			b, _ := io.ReadAll(r.Body)
			f.deployBody = string(b)
			f.deployed = append(f.deployed, strings.Split(rest, "/")[2])
			_, _ = w.Write([]byte("Container pz-new Creating\nContainer pz-new Started\n"))
		case rest == "/containers" && r.Method == http.MethodGet:
			f.listCalls++
			if r.URL.Query().Get("includeHidden") != "true" {
				t.Errorf("list must include hidden containers")
			}
			search := r.URL.Query().Get("search")
			var data []containerSummary
			for _, c := range f.containers {
				if search == "" || strings.Contains(c.name(), search) {
					data = append(data, c)
				}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"success": true, "data": data,
				"pagination": map[string]any{"totalItems": len(data), "totalPages": 1},
			})
		case strings.HasSuffix(rest, "/start") || strings.HasSuffix(rest, "/stop"):
			id := strings.Split(rest, "/")[2]
			for i, c := range f.containers {
				if c.ID == id {
					if strings.HasSuffix(rest, "/start") {
						f.containers[i].State = "running"
					} else {
						f.containers[i].State = "exited"
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "data": map[string]any{"message": "ok"}})
					return
				}
			}
			problem(404, "No such container: "+id)
		case strings.HasSuffix(rest, "/logs/download"):
			_, _ = w.Write(f.logBody)
		case strings.HasPrefix(rest, "/containers/"):
			id := strings.TrimPrefix(rest, "/containers/")
			for _, c := range f.containers {
				if c.ID == id {
					_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "data": map[string]any{
						"id": c.ID, "name": c.Names[0], "image": c.Image,
						"mounts": []map[string]any{{"type": "bind", "source": "/srv/zomboid/x/data", "destination": "/project-zomboid"}},
						"state": map[string]any{
							"status": c.State, "running": c.State == "running",
							"startedAt": "2026-09-24T10:00:00.5Z", "finishedAt": "0001-01-01T00:00:00Z",
							"exitCode": 0, "health": map[string]any{"status": "healthy"},
						},
					}})
					return
				}
			}
			problem(404, "No such container: "+id)
		default:
			problem(404, "no route")
		}
	})
}

func newFake(t *testing.T) (*fake, *Client, *httptest.Server) {
	f := &fake{
		key: "secret", env: "0", denied: map[string]bool{},
		containers: []containerSummary{
			{ID: "aaa111", Names: []string{"/pz-coalfield"}, Image: "pz:1", State: "running"},
			// A name that contains another, to prove matching is exact.
			{ID: "bbb222", Names: []string{"/pz-coalfield-old"}, Image: "pz:1", State: "exited"},
			{ID: "ccc333", Names: []string{"/nextcloud"}, Image: "nc", State: "running"},
		},
	}
	srv := httptest.NewServer(f.handler(t))
	t.Cleanup(srv.Close)
	c := New(Options{
		URL: srv.URL, EnvironmentID: "0", APIKey: "secret",
		Allow: func(n string) bool { return strings.HasPrefix(n, "pz-") },
	})
	return f, c, srv
}

func TestBaseURLGainsAPISuffixOnce(t *testing.T) {
	for in, want := range map[string]string{
		"http://h:3552":      "http://h:3552/api",
		"http://h:3552/":     "http://h:3552/api",
		"http://h:3552/api":  "http://h:3552/api",
		"http://h:3552/api/": "http://h:3552/api",
	} {
		if got := New(Options{URL: in}).base; got != want {
			t.Errorf("%q -> %q, want %q", in, got, want)
		}
	}
}

func TestUnconfiguredClientRefusesEverything(t *testing.T) {
	c := New(Options{URL: "http://x", APIKey: "k"}) // no environment
	if c.Configured() {
		t.Fatal("a client without an environment is not configured")
	}
	if _, err := c.Available(); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("Available: %v", err)
	}
	c.allow = func(string) bool { return true }
	if err := c.Start(context.Background(), "pz"); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("Start: %v", err)
	}
}

func TestStartStopResolveExactNameAndSendKey(t *testing.T) {
	f, c, _ := newFake(t)
	ctx := context.Background()
	if err := c.Stop(ctx, "pz-coalfield"); err != nil {
		t.Fatal(err)
	}
	if f.containers[0].State != "exited" || f.containers[1].State != "exited" {
		t.Fatalf("wrong container touched: %+v", f.containers)
	}
	if err := c.Start(ctx, "pz-coalfield"); err != nil {
		t.Fatal(err)
	}
	if f.containers[0].State != "running" {
		t.Fatal("start did not reach the container")
	}
	if f.containers[1].State != "exited" {
		t.Fatal("a container whose name merely contains the target was started")
	}
	want := "POST /api/environments/0/containers/aaa111/start"
	found := false
	for _, call := range f.calls {
		found = found || call == want
	}
	if !found {
		t.Fatalf("expected %q in %v", want, f.calls)
	}
	// The second action reused the cached ID.
	if f.listCalls != 1 {
		t.Fatalf("expected one lookup, got %d", f.listCalls)
	}
}

func TestAllowlistIsEnforcedBeforeAnyRequest(t *testing.T) {
	f, c, _ := newFake(t)
	if err := c.Stop(context.Background(), "nextcloud"); !errors.Is(err, ErrNotAllowed) {
		t.Fatalf("want ErrNotAllowed, got %v", err)
	}
	if len(f.calls) != 0 {
		t.Fatalf("a refused container still produced requests: %v", f.calls)
	}
}

func TestRecreatedContainerIsLookedUpAgain(t *testing.T) {
	f, c, _ := newFake(t)
	ctx := context.Background()
	if _, err := c.Inspect(ctx, "pz-coalfield"); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.containers[0].ID = "new999" // compose up --force-recreate
	f.mu.Unlock()
	d, err := c.Inspect(ctx, "pz-coalfield")
	if err != nil {
		t.Fatalf("stale ID was not refreshed: %v", err)
	}
	if len(d.Mounts) != 1 || d.Mounts[0].Source != "/srv/zomboid/x/data" || d.Mounts[0].Destination != "/project-zomboid" {
		t.Fatalf("mounts not parsed: %+v", d.Mounts)
	}
	if !d.Running || d.Health != "healthy" || d.StartedAt.IsZero() || !d.FinishedAt.IsZero() {
		t.Fatalf("details not parsed: %+v", d)
	}
}

func TestErrorsNameTheFix(t *testing.T) {
	f, c, _ := newFake(t)
	ctx := context.Background()

	f.denied["containers:stop"] = true
	err := c.Stop(ctx, "pz-coalfield")
	if !errors.Is(err, ErrForbidden) || !strings.Contains(err.Error(), "containers:stop") {
		t.Fatalf("403 should name the missing permission: %v", err)
	}

	c.key = "wrong"
	c.lastPing = c.lastPing.AddDate(-1, 0, 0)
	ok, err := c.Available()
	if ok || !errors.Is(err, ErrUnauthorized) || !strings.Contains(err.Error(), "PZADMIN_ARCANE_API_KEY") {
		t.Fatalf("401 should point at the key setting: %v", err)
	}

	c.key = "secret"
	if err := c.Start(ctx, "pz-missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown container: %v", err)
	}
}

func TestUnreachableArcaneIsUnavailable(t *testing.T) {
	_, c, srv := newFake(t)
	srv.Close()
	if _, err := c.Available(); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("want ErrUnavailable, got %v", err)
	}
}

// A name only the host can resolve is the usual first-install mistake, and
// Go's "no such host" does not say what to do about it.
func TestUnresolvableArcaneSaysHowToFixIt(t *testing.T) {
	_, c, _ := newFake(t)
	c.base = "http://arcane.invalid:3552/api"
	_, err := c.Available()
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("want ErrUnavailable, got %v", err)
	}
	var dns *net.DNSError
	if !errors.As(err, &dns) {
		t.Skipf("this environment did not fail with a DNS error: %v", err)
	}
	if !strings.Contains(err.Error(), "host.docker.internal") {
		t.Fatalf("the error should say how to fix it: %v", err)
	}
}

func TestLogsTailAndDemultiplex(t *testing.T) {
	f, c, _ := newFake(t)
	var framed []byte
	for _, line := range []string{"one\n", "two\n", "three\n"} {
		framed = append(framed, 1, 0, 0, 0, 0, 0, 0, byte(len(line)))
		framed = append(framed, line...)
	}
	f.logBody = framed
	out, err := c.Logs(context.Background(), "pz-coalfield", 2)
	if err != nil {
		t.Fatal(err)
	}
	if out != "two\nthree" {
		t.Fatalf("got %q", out)
	}

	f.logBody = []byte("plain a\nplain b\n")
	if out, _ := c.Logs(context.Background(), "pz-coalfield", 10); out != "plain a\nplain b" {
		t.Fatalf("unframed log: %q", out)
	}
}

func TestListFlagsManagedContainers(t *testing.T) {
	_, c, _ := newFake(t)
	list, err := c.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	managed := 0
	for _, x := range list {
		if x.Managed {
			managed++
		}
	}
	if len(list) != 3 || managed != 2 {
		t.Fatalf("got %+v", list)
	}
}

func TestPermissionFor(t *testing.T) {
	for path, want := range map[string]string{
		"/environments/0/containers?limit=1":           "containers:list",
		"/environments/0/containers/abc":               "containers:read",
		"/environments/0/containers/abc/start":         "containers:start",
		"/environments/0/containers/abc/stop":          "containers:stop",
		"/environments/0/containers/abc/logs/download": "containers:logs",
	} {
		if got := PermissionFor(path); got != want {
			t.Errorf("%s: %s, want %s", path, got, want)
		}
	}
}

func TestDeployIsAllowlistedAndNeverTouchesVolumes(t *testing.T) {
	f, c, _ := newFake(t)
	f.projects = []Project{
		{ID: "p1", Name: "pz-new", DirName: "pz-new", RelativePath: "pzserver/pz-new"},
		{ID: "p2", Name: "vaultwarden", DirName: "vaultwarden", RelativePath: "vaultwarden"},
	}
	c.allowProject = func(p Project) bool { return strings.HasPrefix(p.RelativePath, "pzserver/") }
	ctx := context.Background()
	list, err := c.Projects(ctx)
	if err != nil || len(list) != 2 {
		t.Fatalf("%v %v", list, err)
	}
	if _, err := c.Deploy(ctx, list[1]); !errors.Is(err, ErrNotAllowed) {
		t.Fatalf("a project that is not a server must be refused: %v", err)
	}
	out, err := c.Deploy(ctx, list[0])
	if err != nil || !strings.Contains(out, "Started") {
		t.Fatalf("%q %v", out, err)
	}
	if len(f.deployed) != 1 || f.deployed[0] != "p1" {
		t.Fatalf("deployed %v", f.deployed)
	}
	if !strings.Contains(f.deployBody, `"recreateVolumes":false`) || !strings.Contains(f.deployBody, `"pullPolicy":"missing"`) {
		t.Fatalf("deploy options: %s", f.deployBody)
	}
	f.denied["projects:deploy"] = true
	if _, err := c.Deploy(ctx, list[0]); !errors.Is(err, ErrForbidden) || !strings.Contains(err.Error(), "projects:deploy") {
		t.Fatalf("403 should name projects:deploy: %v", err)
	}
}
