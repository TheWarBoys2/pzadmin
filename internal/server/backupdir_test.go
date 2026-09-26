package server

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Pointing PZADMIN_BACKUP_DIR at a bind mount brings the older archives with
// it, so nobody's backups disappear from the list on upgrade.
func TestBackupDirAdoptsOldArchives(t *testing.T) {
	data := t.TempDir()
	old := filepath.Join(data, "backups", "srv1")
	if err := os.MkdirAll(old, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(old, "20260901-040000.tar.gz"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	mount := filepath.Join(t.TempDir(), "bind")
	app, err := New(Options{DataDir: data, BackupDir: mount, SetupCode: testSetupCode})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(app.Close)
	app.prepareBackupDir()

	deadline := time.Now().Add(5 * time.Second)
	for len(app.backup.List("srv1")) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the old archive was not moved into the new folder")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if _, err := os.Stat(filepath.Join(mount, "srv1", "20260901-040000.tar.gz")); err != nil {
		t.Fatal(err)
	}
}

// A folder PZAdmin cannot write to is reported at start, not at the first
// failed backup.
func TestUnwritableBackupDirIsReported(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root can write anywhere")
	}
	data := t.TempDir()
	locked := filepath.Join(t.TempDir(), "locked")
	if err := os.MkdirAll(locked, 0o500); err != nil {
		t.Fatal(err)
	}
	app, err := New(Options{DataDir: data, BackupDir: locked, SetupCode: testSetupCode})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(app.Close)
	app.prepareBackupDir()
	for _, e := range app.store.Events("", "backup.failed", 10) {
		if e.Message == "Backups cannot be saved" {
			return
		}
	}
	t.Fatal("expected a backup.failed event explaining the folder is not writable")
}
