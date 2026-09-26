package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCheckDataDirAcceptsItsOwnFiles(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := checkDataDir(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".writable")); !os.IsNotExist(err) {
		t.Fatal("the probe file should be removed")
	}
}

// After a change of user:, the folder can still be writable while the files
// the old user made are not. Startup must name the file, not fail later.
func TestCheckDataDirNamesAFileItCannotWrite(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can write any file, so this cannot be shown as root")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte("{}"), 0o400); err != nil {
		t.Fatal(err)
	}
	err := checkDataDir(dir)
	if err == nil || !strings.Contains(err.Error(), "config.json") {
		t.Fatalf("want an error naming config.json, got %v", err)
	}
}
