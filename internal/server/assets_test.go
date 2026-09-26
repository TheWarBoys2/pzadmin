package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

// After an upgrade an open tab must not keep running the old app.js: the
// page asks for a URL that names this build, and anything else revalidates.
func TestAssetsAreVersionedByBuild(t *testing.T) {
	assets := fstest.MapFS{
		"index.html": &fstest.MapFile{Data: []byte(`<link rel="stylesheet" href="/app.css"><script src="/app.js"></script>`)},
		"app.js":     &fstest.MapFile{Data: []byte("// v1")},
		"app.css":    &fstest.MapFile{Data: []byte("body{}")},
	}
	app, err := New(Options{DataDir: t.TempDir(), Assets: assets, SetupCode: testSetupCode})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(app.Close)
	h := app.Handler()
	get := func(path string, header ...string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		for i := 0; i+1 < len(header); i += 2 {
			req.Header.Set(header[i], header[i+1])
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	page := get("/").Body.String()
	want := "/app.js?v=" + app.assetBuild
	if !strings.Contains(page, want) || !strings.Contains(page, "/app.css?v="+app.assetBuild) {
		t.Fatalf("index.html should point at this build's assets, got %s", page)
	}
	if cc := get(want).Header().Get("Cache-Control"); !strings.Contains(cc, "immutable") {
		t.Fatalf("a versioned asset can be cached for good, got %q", cc)
	}
	if cc := get("/app.js").Header().Get("Cache-Control"); cc != "no-cache" {
		t.Fatalf("an unversioned asset must revalidate, got %q", cc)
	}
	if code := get("/app.js", "If-None-Match", `"`+app.assetBuild+`"`).Code; code != http.StatusNotModified {
		t.Fatalf("an unchanged asset should be a 304, got %d", code)
	}

	other := fstest.MapFS{"app.js": &fstest.MapFile{Data: []byte("// v2")}, "app.css": assets["app.css"]}
	if assetHash(other) == app.assetBuild {
		t.Fatal("a different app.js must give a different build")
	}
}
