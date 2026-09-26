package server

import (
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"testing"
)

func TestBackupDownload(t *testing.T) {
	app, handler, _ := newTestApp(t)
	c := &client{t: t, handler: handler}
	c.setup("rick", "a-long-enough-password")
	seedServer(t, app, `{"id":"srv1","name":"Riverside","host":"127.0.0.1","rconPort":27015,"enabled":false}`)

	dir := app.backup.Dir("srv1")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	name := "srv1-20260926-040000.tar.gz"
	if err := os.WriteFile(filepath.Join(dir, name), []byte("archive bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Something outside the backups folder that a crafted name might reach.
	if err := os.WriteFile(filepath.Join(filepath.Dir(dir), "secret.tar.gz"), []byte("nope"), 0o600); err != nil {
		t.Fatal(err)
	}

	get := func(c *client, server, file string) *http.Response {
		q := url.Values{"serverId": {server}, "name": {file}}
		return c.do(http.MethodGet, "/api/backups/download?"+q.Encode(), nil).Result()
	}

	res := get(c, "srv1", name)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("download failed: %d", res.StatusCode)
	}
	if got := res.Header.Get("Content-Disposition"); got != `attachment; filename="`+name+`"` {
		t.Fatalf("Content-Disposition = %q", got)
	}
	if res.Header.Get("Content-Type") != "application/gzip" {
		t.Fatalf("Content-Type = %q", res.Header.Get("Content-Type"))
	}

	for _, tc := range []struct{ server, file string }{
		{"srv1", "../secret.tar.gz"},
		{"srv1", "srv1-20260926-040000.txt"},
		{"..", "secret.tar.gz"},
		{"srv1", "missing.tar.gz"},
	} {
		if res := get(c, tc.server, tc.file); res.StatusCode != http.StatusNotFound {
			t.Fatalf("%s/%s should be refused, got %d", tc.server, tc.file, res.StatusCode)
		}
	}

	// A signed-out browser gets nothing.
	if res := get(&client{t: t, handler: handler}, "srv1", name); res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("signed out: expected 401, got %d", res.StatusCode)
	}
}
