package server

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func testKeyRequest() apiKeyRequest {
	return apiKeyRequest{Name: "bot", Scopes: []string{scopeRead}}
}

// The release review reproduced a revoked key coming back: a housekeeping
// flush snapshotted the keys before a revoke and wrote them after it.
func TestRevokedKeyStaysRevokedWhenAFlushRaces(t *testing.T) {
	for i := 0; i < 200; i++ {
		path := filepath.Join(t.TempDir(), "apikeys.json")
		s := newAPIKeyStore(path)
		_, k, err := s.create(testKeyRequest(), "rick", func(string) bool { return true })
		if err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		for j := 0; j < 3; j++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				s.mu.Lock()
				s.dirty = true
				s.mu.Unlock()
				s.flush()
			}()
		}
		if _, found, err := s.revoke(k.ID); !found || err != nil {
			t.Fatalf("revoke: found=%v err=%v", found, err)
		}
		wg.Wait()

		if reloaded := newAPIKeyStore(path); reloaded.active(k.ID) {
			t.Fatalf("run %d: the revoked key came back after a reload", i)
		}
	}
}

func TestCorruptKeyFileIsMovedAsideAndReported(t *testing.T) {
	path := filepath.Join(t.TempDir(), "apikeys.json")
	if err := os.WriteFile(path, []byte("[{broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := newAPIKeyStore(path)
	if s.loadErr == nil {
		t.Fatal("a corrupt key file should be reported")
	}
	matches, _ := filepath.Glob(path + ".corrupt-*")
	if len(matches) != 1 {
		t.Fatalf("the corrupt file should be kept aside, found %v", matches)
	}
	// The next save must not destroy the evidence.
	if _, _, err := s.create(testKeyRequest(), "rick", func(string) bool { return true }); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(matches[0]); string(b) != "[{broken" {
		t.Fatal("the moved-aside file was changed")
	}
}

func TestUnreadableDataFileShowsInActivity(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "sessions.json"), []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	app, err := New(Options{DataDir: dir, SetupCode: testSetupCode})
	if err != nil {
		t.Fatal(err)
	}
	app.Start()
	defer app.Close()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		for _, e := range app.store.Events("", "system.error", 50) {
			if e.Kind == "system.error" && strings.Contains(e.Detail, "sessions.json") {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("an unreadable sessions.json should be recorded as an event")
}
