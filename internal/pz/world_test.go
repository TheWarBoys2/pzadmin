package pz

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestResetWorldDeletesOnlyMultiplayer(t *testing.T) {
	saves := filepath.Join(t.TempDir(), "Saves")
	for _, p := range []string{"Multiplayer/servertest/map", "Sandbox/other"} {
		if err := os.MkdirAll(filepath.Join(saves, p), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	b := NewBackupper(t.TempDir())
	l := Layout{SavesDir: saves}
	info, err := b.ResetWorld("s", l)
	if err != nil {
		t.Fatal(err)
	}
	if len(info.Worlds) != 1 || info.Worlds[0] != "servertest" {
		t.Fatalf("reported worlds %v", info.Worlds)
	}
	if _, err := os.Stat(filepath.Join(saves, "Multiplayer")); !os.IsNotExist(err) {
		t.Fatal("Multiplayer should be gone")
	}
	if _, err := os.Stat(filepath.Join(saves, "Sandbox", "other")); err != nil {
		t.Fatal("other Saves folders must be kept")
	}
	if _, err := b.ResetWorld("s", l); !errors.Is(err, ErrNoWorld) {
		t.Fatalf("a second reset should find no world, got %v", err)
	}
}

func TestResetWorldRefusesALinkedFolder(t *testing.T) {
	base := t.TempDir()
	saves := filepath.Join(base, "Saves")
	elsewhere := filepath.Join(base, "elsewhere")
	if err := os.MkdirAll(filepath.Join(elsewhere, "servertest"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(saves, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(elsewhere, filepath.Join(saves, "Multiplayer")); err != nil {
		t.Skip("no symlinks here")
	}
	if _, err := NewBackupper(t.TempDir()).ResetWorld("s", Layout{SavesDir: saves}); err == nil {
		t.Fatal("a linked Multiplayer folder should be refused")
	}
	if _, err := os.Stat(filepath.Join(elsewhere, "servertest")); err != nil {
		t.Fatal("the link's target must be untouched")
	}
}

func TestResetWorldWaitsForABackup(t *testing.T) {
	b := NewBackupper(t.TempDir())
	b.running["s"] = true
	if _, err := b.ResetWorld("s", Layout{SavesDir: t.TempDir()}); !errors.Is(err, ErrBackupRunning) {
		t.Fatalf("a reset during a backup should be refused, got %v", err)
	}
}
