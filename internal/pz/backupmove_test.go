package pz

import (
	"os"
	"path/filepath"
	"testing"
)

func TestBackupsMoveToANewFolder(t *testing.T) {
	old := filepath.Join(t.TempDir(), "backups")
	b := NewBackupper(filepath.Join(t.TempDir(), "new"))

	write := func(path, body string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(old, "srv1", "20260901-040000.tar.gz"), "one")
	write(filepath.Join(old, "srv1", "20260901-040000.tar.gz.note"), "before the wipe")
	write(filepath.Join(old, "srv2", "20260902-040000.tar.gz"), "two")
	// Already present in the new folder: must not be overwritten.
	write(filepath.Join(old, "srv2", "20260903-040000.tar.gz"), "old copy")
	write(filepath.Join(b.Root(), "srv2", "20260903-040000.tar.gz"), "new copy")

	moved, left, err := b.MoveFrom(old)
	if err != nil {
		t.Fatal(err)
	}
	if moved != 2 || left != 1 {
		t.Fatalf("moved %d, left %d; want 2 and 1", moved, left)
	}
	list := b.List("srv1")
	if len(list) != 1 || list[0].Note != "before the wipe" {
		t.Fatalf("srv1 archives after the move: %#v", list)
	}
	if got, _ := os.ReadFile(filepath.Join(b.Root(), "srv2", "20260903-040000.tar.gz")); string(got) != "new copy" {
		t.Fatal("an archive already in the new folder was overwritten")
	}
	if _, err := os.Stat(filepath.Join(old, "srv1")); !os.IsNotExist(err) {
		t.Fatal("the emptied folder should be removed")
	}

	// Running it again, or with nothing to move, does nothing.
	if moved, _, err := b.MoveFrom(old); moved != 0 || err != nil {
		t.Fatalf("second run moved %d: %v", moved, err)
	}
	if moved, _, err := b.MoveFrom(filepath.Join(t.TempDir(), "missing")); moved != 0 || err != nil {
		t.Fatalf("missing folder: %d %v", moved, err)
	}
}

func TestMoveFileCopiesWhenRenameFails(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "a.tar.gz")
	if err := os.WriteFile(src, []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A destination in a folder that does not exist makes rename fail; the
	// copy then fails too and the source must be left alone.
	if err := moveFile(src, filepath.Join(dir, "missing", "a.tar.gz")); err == nil {
		t.Fatal("expected an error")
	}
	if _, err := os.Stat(src); err != nil {
		t.Fatal("the source must survive a failed move")
	}
}
