package server

import (
	"bytes"
	"image"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func pngBytes(t *testing.T, w, h int) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewGray(image.Rect(0, 0, w, h))); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// rawPost sends body as-is with the session and CSRF header, as the browser's
// file upload does.
func rawPost(c *client, path string, body []byte, contentType string) *httptest.ResponseRecorder {
	c.t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
	for _, ck := range c.cookies {
		req.AddCookie(ck)
	}
	req.Header.Set("X-CSRF-Token", c.csrf)
	req.Header.Set("Content-Type", contentType)
	rec := httptest.NewRecorder()
	c.handler.ServeHTTP(rec, req)
	return rec
}

func TestWorldMapUploadCalibrateAndRemove(t *testing.T) {
	app, handler, _ := newTestApp(t)
	c := &client{t: t, handler: handler}
	c.setup("rick", "a-long-enough-password")

	if out := decode(t, c.do(http.MethodGet, "/api/map", nil)); out["present"] != false {
		t.Fatalf("no map yet: %v", out)
	}
	if rec := rawPost(c, "/api/map/upload", []byte("<svg/>"), "image/svg+xml"); rec.Code != http.StatusBadRequest {
		t.Fatalf("an SVG must be refused: %d", rec.Code)
	}

	rec := rawPost(c, "/api/map/upload", pngBytes(t, 400, 300), "image/png")
	if rec.Code != http.StatusOK {
		t.Fatalf("upload: %d %s", rec.Code, rec.Body.String())
	}
	m, _ := decode(t, rec)["map"].(map[string]any)
	if m["width"] != float64(400) || m["height"] != float64(300) || m["type"] != "image/png" {
		t.Fatalf("map details: %v", m)
	}

	img := c.do(http.MethodGet, "/api/map/image", nil)
	if img.Code != http.StatusOK || img.Header().Get("Content-Type") != "image/png" {
		t.Fatalf("image: %d %s", img.Code, img.Header().Get("Content-Type"))
	}

	bad := []map[string]any{
		{"x": 1000, "y": 1000, "px": 10, "py": 10},
		{"x": 1050, "y": 5000, "px": 300, "py": 200},
	}
	if rec := c.do(http.MethodPost, "/api/map/calibrate", map[string]any{"points": bad}); rec.Code != http.StatusBadRequest ||
		!strings.Contains(rec.Body.String(), "100 tiles") {
		t.Fatalf("points too close across should be refused: %d %s", rec.Code, rec.Body.String())
	}
	outside := []map[string]any{
		{"x": 1000, "y": 1000, "px": 10, "py": 10},
		{"x": 9000, "y": 9000, "px": 900, "py": 200},
	}
	if rec := c.do(http.MethodPost, "/api/map/calibrate", map[string]any{"points": outside}); rec.Code != http.StatusBadRequest {
		t.Fatalf("a point off the image should be refused: %d", rec.Code)
	}
	good := []map[string]any{
		{"x": 1000, "y": 1000, "px": 10, "py": 10},
		{"x": 9000, "y": 9000, "px": 390, "py": 290},
	}
	if rec := c.do(http.MethodPost, "/api/map/calibrate", map[string]any{"points": good}); rec.Code != http.StatusOK {
		t.Fatalf("calibrate: %d %s", rec.Code, rec.Body.String())
	}
	if meta, _ := app.worldMapMeta(); len(meta.Points) != 2 {
		t.Fatalf("points not saved: %+v", meta)
	}

	// A new image clears the old points: they named the old image's pixels.
	if rec := rawPost(c, "/api/map/upload", pngBytes(t, 800, 600), "image/png"); rec.Code != http.StatusOK {
		t.Fatalf("second upload: %d", rec.Code)
	}
	if meta, _ := app.worldMapMeta(); len(meta.Points) != 0 || meta.Width != 800 {
		t.Fatalf("a new image should need lining up again: %+v", meta)
	}

	if rec := c.do(http.MethodPost, "/api/map/delete", map[string]any{}); rec.Code != http.StatusOK {
		t.Fatalf("delete: %d", rec.Code)
	}
	imgPath, metaPath := app.worldMapPaths()
	for _, p := range []string{imgPath, metaPath} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Fatalf("%s should be gone", p)
		}
	}
}

func TestWorldMapNeedsSignIn(t *testing.T) {
	_, handler, _ := newTestApp(t)
	c := &client{t: t, handler: handler}
	c.setup("rick", "a-long-enough-password")
	c.csrf = ""
	if rec := rawPost(c, "/api/map/upload", pngBytes(t, 10, 10), "image/png"); rec.Code != http.StatusForbidden {
		t.Fatalf("an upload without the CSRF header must be refused, got %d", rec.Code)
	}
	req := httptest.NewRequest(http.MethodGet, "/api/map/image", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("the image needs a session, got %d", rec.Code)
	}
}
