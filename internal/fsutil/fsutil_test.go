package fsutil

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestWriteFileReplacesAndLeavesNoTempFiles(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sub", "state.json")
	for _, body := range []string{"one", "two"} {
		if err := WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if b, _ := os.ReadFile(path); string(b) != "two" {
		t.Fatalf("got %q", b)
	}
	if st, _ := os.Stat(path); st.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", st.Mode().Perm())
	}
	entries, _ := os.ReadDir(filepath.Dir(path))
	if len(entries) != 1 {
		t.Fatalf("temporary files were left behind: %v", entries)
	}
}

func TestConcurrentWritersNeverProduceAMixedFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	a, b := strings.Repeat("a", 1<<16), strings.Repeat("b", 1<<16)
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(2)
		go func() { defer wg.Done(); _ = WriteFile(path, []byte(a), 0o600) }()
		go func() { defer wg.Done(); _ = WriteFile(path, []byte(b), 0o600) }()
	}
	wg.Wait()
	got, _ := os.ReadFile(path)
	if string(got) != a && string(got) != b {
		t.Fatal("two writers interleaved into one file")
	}
}

func TestReadJSONMovesACorruptFileAside(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "apikeys.json")
	if err := os.WriteFile(path, []byte("[{broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	var v []any
	found, err := ReadJSON(path, &v)
	var corrupt *CorruptError
	if found || !errors.As(err, &corrupt) {
		t.Fatalf("expected a CorruptError, got found=%v err=%v", found, err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("the corrupt file should have been moved")
	}
	if b, err := os.ReadFile(corrupt.Moved); err != nil || string(b) != "[{broken" {
		t.Fatalf("the corrupt file should be kept as evidence: %v", err)
	}
	if !strings.Contains(err.Error(), "moved to apikeys.json.corrupt-") {
		t.Fatalf("message should say where it went: %v", err)
	}
}

func TestReadJSONMissingFileIsNotAnError(t *testing.T) {
	var v map[string]any
	found, err := ReadJSON(filepath.Join(t.TempDir(), "nope.json"), &v)
	if found || err != nil {
		t.Fatalf("got found=%v err=%v", found, err)
	}
}
