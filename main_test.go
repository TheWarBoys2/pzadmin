package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCheckRootLetsAnEmptyFolderStart(t *testing.T) {
	dir := t.TempDir()
	warning, err := checkRoot(dir)
	if err != nil {
		t.Fatalf("an empty folder is a new install and must not stop PZAdmin: %v", err)
	}
	if !strings.Contains(warning, "is empty") {
		t.Fatalf("an empty folder should still be mentioned in the log, got %q", warning)
	}

	if err := os.Mkdir(filepath.Join(dir, "pz-one"), 0o755); err != nil {
		t.Fatal(err)
	}
	if warning, err := checkRoot(dir); err != nil || warning != "" {
		t.Fatalf("a folder with a server should pass quietly, got %q, %v", warning, err)
	}
}

func TestCheckRootRefusesBadPaths(t *testing.T) {
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"", "relative/path", filepath.Join(t.TempDir(), "missing"), file} {
		if _, err := checkRoot(path); err == nil {
			t.Errorf("checkRoot(%q) should fail", path)
		}
	}
}
