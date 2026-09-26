package pz

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// buildServer creates a realistic Indifferent-Broccoli-style layout.
func buildServer(t *testing.T) (root, base string) {
	t.Helper()
	root = t.TempDir()
	base = filepath.Join(root, "riverside2")
	pz := filepath.Join(base, "projectzomboid")
	writeFile(t, filepath.Join(pz, "config", "Server", "riverside.ini"),
		"# generated\nPublicName=Riverside\nMods=ModA;ModB\nWorkshopItems=1234;5678\nMaxPlayers=16\n")
	writeFile(t, filepath.Join(pz, "config", "Server", "riverside_SandboxVars.lua"),
		"SandboxVars = {\n  Zombies = 3,\n}\n")
	writeFile(t, filepath.Join(pz, "data", "Saves", "Multiplayer", "riverside", "map_t.bin"), "world")
	writeFile(t, filepath.Join(pz, "config", "Logs", "26-09-07_user.txt"), "")
	writeFile(t, filepath.Join(pz, "config", "mods", "ModA", "mod.info"), "name=Mod Alpha\nid=ModA\n")
	return root, base
}

func TestSafeJoinRefusesEscapes(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "inner"), 0o755); err != nil {
		t.Fatal(err)
	}
	escapes := []string{"..", "../", "../../etc", "inner/../..", "/etc/passwd", "inner/../../..", "./../"}
	for _, rel := range escapes {
		if _, err := SafeJoin(root, rel); !errors.Is(err, ErrOutsideRoot) {
			t.Fatalf("SafeJoin(%q) should be refused, got %v", rel, err)
		}
	}
	for _, rel := range []string{".", "inner", "./inner", "inner/deeper"} {
		if _, err := SafeJoin(root, rel); err != nil {
			t.Fatalf("SafeJoin(%q) should be allowed: %v", rel, err)
		}
	}
}

// A symlink placed inside the root must not become a way out of it.
func TestSafeJoinResolvesSymlinks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privileges on Windows")
	}
	root := t.TempDir()
	outside := t.TempDir()
	link := filepath.Join(root, "escape")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("cannot create symlink: %v", err)
	}
	if _, err := SafeJoin(root, "escape"); !errors.Is(err, ErrOutsideRoot) {
		t.Fatalf("a symlink out of the root must be refused, got %v", err)
	}
	if _, err := SafeJoin(root, "escape/secrets.txt"); !errors.Is(err, ErrOutsideRoot) {
		t.Fatal("a path through an escaping symlink must be refused")
	}
}

func TestSafeJoinAbsolutePathInsideRootIsAccepted(t *testing.T) {
	root := t.TempDir()
	inner := filepath.Join(root, "srv")
	if err := os.MkdirAll(inner, 0o755); err != nil {
		t.Fatal(err)
	}
	got, err := SafeJoin(root, inner)
	if err != nil {
		t.Fatalf("an absolute path already inside the root should be accepted: %v", err)
	}
	if filepath.Base(got) != "srv" {
		t.Fatalf("unexpected result %q", got)
	}
}

func TestDetectNestedLayout(t *testing.T) {
	_, base := buildServer(t)
	l := Detect(base)
	if !l.Valid() {
		t.Fatalf("layout not detected: %#v", l)
	}
	if filepath.Base(l.ConfigDir) != "Server" {
		t.Fatalf("config dir = %s", l.ConfigDir)
	}
	if l.SavesDir == "" || !strings.HasSuffix(l.SavesDir, "Saves") {
		t.Fatalf("saves dir = %s", l.SavesDir)
	}
	if l.LogsDir == "" {
		t.Fatal("logs dir not found")
	}
	if len(l.ModDirs) == 0 {
		t.Fatal("mod dirs not found")
	}
}

func TestBrowseFlagsServerFolders(t *testing.T) {
	root, _ := buildServer(t)
	_, entries, isServer, err := Browse(root, ".")
	if err != nil {
		t.Fatal(err)
	}
	if isServer {
		t.Fatal("the root itself is not a server")
	}
	if len(entries) != 1 || entries[0].Name != "riverside2" || !entries[0].IsServer {
		t.Fatalf("unexpected entries %#v", entries)
	}
	if _, _, _, err := Browse(root, "../.."); err == nil {
		t.Fatal("browse must refuse to leave the root")
	}
}

func TestINIPreservesCommentsAndOrder(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "server.ini")
	original := "# Riverside config\nPublicName=Riverside\n\n; a note\nMaxPlayers=16\nMods=ModA;ModB\n"
	writeFile(t, path, original)

	ini, err := LoadINI(path)
	if err != nil {
		t.Fatal(err)
	}
	if v, _ := ini.Get("maxplayers"); v != "16" {
		t.Fatalf("case-insensitive lookup failed, got %q", v)
	}
	if err := ini.Set("MaxPlayers", "32"); err != nil {
		t.Fatal(err)
	}
	if err := ini.Set("PauseEmpty", "true"); err != nil {
		t.Fatal(err)
	}
	if _, err := ini.Save(filepath.Join(dir, "backups")); err != nil {
		t.Fatal(err)
	}

	b, _ := os.ReadFile(path)
	out := string(b)
	for _, want := range []string{"# Riverside config", "; a note", "MaxPlayers=32", "PauseEmpty=true", "Mods=ModA;ModB"} {
		if !strings.Contains(out, want) {
			t.Fatalf("expected %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "MaxPlayers=16") {
		t.Fatal("the old value should have been replaced in place")
	}
	// The original must have been backed up.
	backups, _ := os.ReadDir(filepath.Join(dir, "backups"))
	if len(backups) != 1 {
		t.Fatalf("expected exactly one backup, got %d", len(backups))
	}
}

func TestINIRejectsNewlineInValue(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "s.ini")
	writeFile(t, path, "A=1\n")
	ini, _ := LoadINI(path)
	if err := ini.Set("A", "line1\nB=evil"); err == nil {
		t.Fatal("a newline in a value would corrupt every setting after it and must be refused")
	}
}

func TestConfigFileGuards(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "a.ini"), "A=1\n")
	for _, bad := range []string{"../escape.ini", "sub/a.ini", "", ".", "passwd", `..\a.ini`} {
		if _, err := ReadConfigFile(dir, bad); err == nil {
			t.Fatalf("ReadConfigFile(%q) should be refused", bad)
		}
	}
	if _, err := ReadConfigFile(dir, "a.ini"); err != nil {
		t.Fatalf("a plain filename should be readable: %v", err)
	}
}

func TestWriteConfigFileValidatesLua(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "x_SandboxVars.lua"), "SandboxVars = {\n Zombies = 3,\n}\n")
	backups := filepath.Join(dir, "bak")

	if _, err := WriteConfigFile(dir, "x_SandboxVars.lua", "SandboxVars = {\n Zombies = 3,\n", backups); err == nil {
		t.Fatal("unbalanced Lua must be refused before it stops the server booting")
	}
	// Braces inside strings and comments must not confuse the check.
	ok := "SandboxVars = {\n  Note = \"a } brace\", -- and } here\n  Zombies = 2,\n}\n"
	if _, err := WriteConfigFile(dir, "x_SandboxVars.lua", ok, backups); err != nil {
		t.Fatalf("valid Lua was rejected: %v", err)
	}
	got, _ := ReadConfigFile(dir, "x_SandboxVars.lua")
	if !strings.Contains(got, "Zombies = 2") {
		t.Fatal("write did not take effect")
	}
}

func TestModScanFindsMissingMods(t *testing.T) {
	_, base := buildServer(t)
	l := Detect(base)
	report := NewScanner(time.Minute).Scan(l)

	var modA *Mod
	for i := range report.Mods {
		if report.Mods[i].ID == "ModA" {
			modA = &report.Mods[i]
		}
	}
	if modA == nil || !modA.Enabled || !modA.Installed {
		t.Fatalf("ModA should be enabled and installed: %#v", report.Mods)
	}
	if modA.Name != "Mod Alpha" {
		t.Fatalf("mod.info name not read, got %q", modA.Name)
	}
	if len(report.Missing) != 1 || report.Missing[0] != "ModB" {
		t.Fatalf("ModB is enabled but not installed and should be flagged: %#v", report.Missing)
	}
	if len(report.WorkshopIDs) != 2 {
		t.Fatalf("workshop ids = %#v", report.WorkshopIDs)
	}
}

func TestScannerCachesUntilDiskChanges(t *testing.T) {
	_, base := buildServer(t)
	l := Detect(base)
	s := NewScanner(time.Minute)
	first := s.Scan(l)
	second := s.Scan(l)
	if !first.ScannedAt.Equal(second.ScannedAt) {
		t.Fatal("a repeated scan with no disk changes should be served from cache")
	}

	time.Sleep(10 * time.Millisecond)
	writeFile(t, filepath.Join(l.ConfigDir, "riverside.ini"),
		"PublicName=Riverside\nMods=ModA\nWorkshopItems=1234\n")
	third := s.Scan(l)
	if third.ScannedAt.Equal(first.ScannedAt) {
		t.Fatal("changing the config must invalidate the cache")
	}
	if len(third.Missing) != 0 {
		t.Fatalf("ModB is no longer enabled so nothing should be missing: %#v", third.Missing)
	}
}

func TestBackupCreateVerifyRestore(t *testing.T) {
	_, base := buildServer(t)
	l := Detect(base)
	b := NewBackupper(t.TempDir())

	res, err := b.Create("srv1", l, true, 3, "before mod update")
	if err != nil {
		t.Fatal(err)
	}
	if res.Archive.Files < 3 {
		t.Fatalf("expected saves and config in the archive, got %d files", res.Archive.Files)
	}
	if files, _, err := b.Verify("srv1", res.Archive.Name); err != nil || files < 3 {
		t.Fatalf("verify failed: files=%d err=%v", files, err)
	}
	if list := b.List("srv1"); len(list) != 1 || list[0].Note != "before mod update" {
		t.Fatalf("unexpected listing %#v", list)
	}

	// Destroy the world, then restore it.
	savePath := filepath.Join(l.SavesDir, "Multiplayer", "riverside", "map_t.bin")
	if err := os.RemoveAll(l.SavesDir); err != nil {
		t.Fatal(err)
	}
	restored, err := b.Restore("srv1", res.Archive.Name, l)
	if err != nil {
		t.Fatal(err)
	}
	if restored.Files == 0 {
		t.Fatal("restore wrote nothing")
	}
	got, err := os.ReadFile(savePath)
	if err != nil || string(got) != "world" {
		t.Fatalf("save file not restored: %v %q", err, got)
	}
}

// Restoring must not leave behind files made after the backup: new map
// chunks mixed into an older world break it. They go aside instead.
func TestRestoreReplacesTheWorldAndKeepsTheOldOneAside(t *testing.T) {
	_, base := buildServer(t)
	l := Detect(base)
	b := NewBackupper(t.TempDir())
	res, err := b.Create("srv1", l, false, 3, "")
	if err != nil {
		t.Fatal(err)
	}
	world := filepath.Join(l.SavesDir, "Multiplayer", "riverside")
	newer := filepath.Join(world, "map_99_99.bin")
	if err := os.WriteFile(newer, []byte("explored later"), 0o644); err != nil {
		t.Fatal(err)
	}

	restored, err := b.Restore("srv1", res.Archive.Name, l)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(newer); !os.IsNotExist(err) {
		t.Fatal("a chunk made after the backup survived the restore")
	}
	if got, _ := os.ReadFile(filepath.Join(world, "map_t.bin")); string(got) != "world" {
		t.Fatalf("world not restored: %q", got)
	}
	aside := filepath.Join(restored.SetAside, "Multiplayer", "riverside", "map_99_99.bin")
	if got, _ := os.ReadFile(aside); string(got) != "explored later" {
		t.Fatalf("the world from before the restore should be kept aside at %s", aside)
	}

	// A broken archive leaves the world exactly as it was.
	name := "20260101-000000.tar.gz"
	full, _ := os.ReadFile(filepath.Join(b.Dir("srv1"), res.Archive.Name))
	if err := os.WriteFile(filepath.Join(b.Dir("srv1"), name), full[:len(full)/2], 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(newer, []byte("still here"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Restore("srv1", name, l); err == nil {
		t.Fatal("a truncated archive should fail")
	}
	if got, _ := os.ReadFile(newer); string(got) != "still here" {
		t.Fatal("a failed restore must put the world back as it was")
	}
}

func TestBackupRetentionPrunesOldest(t *testing.T) {
	_, base := buildServer(t)
	l := Detect(base)
	b := NewBackupper(t.TempDir())
	for i := 0; i < 4; i++ {
		if _, err := b.Create("srv1", l, false, 2, ""); err != nil {
			t.Fatal(err)
		}
		// Archive names carry a one-second resolution timestamp.
		time.Sleep(1100 * time.Millisecond)
	}
	if got := len(b.List("srv1")); got != 2 {
		t.Fatalf("retention of 2 should leave 2 archives, found %d", got)
	}
}

func TestBackupRejectsBadArchiveNames(t *testing.T) {
	b := NewBackupper(t.TempDir())
	for _, name := range []string{"../etc.tar.gz", "a/b.tar.gz", "notanarchive", ""} {
		if err := b.Delete("srv", name); err == nil {
			t.Fatalf("Delete(%q) should be refused", name)
		}
	}
}

func TestBackupRefusesConcurrentRuns(t *testing.T) {
	_, base := buildServer(t)
	l := Detect(base)
	b := NewBackupper(t.TempDir())
	b.mu.Lock()
	b.running["srv1"] = true
	b.mu.Unlock()
	if _, err := b.Create("srv1", l, false, 3, ""); !errors.Is(err, ErrBackupRunning) {
		t.Fatalf("expected ErrBackupRunning, got %v", err)
	}
}

func TestTailerSkipsHistoryThenReportsNewLines(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "26-09-07_user.txt")
	writeFile(t, logPath, "[07-09-26 10:00:00.000] user OldPlayer fully connected (76561198000000001).\n")

	tail := NewTailer(dir)
	if got := tail.Poll(0); len(got) != 0 {
		t.Fatalf("the first poll must not replay history, got %#v", got)
	}

	f, err := os.OpenFile(logPath, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString("[07-09-26 10:05:00.000] user Rick fully connected (76561198000000002).\n")
	f.WriteString("[07-09-26 10:09:00.000] user Rick disconnected (76561198000000002).\n")
	f.Close()

	got := tail.Poll(0)
	if len(got) != 2 {
		t.Fatalf("expected a join and a leave, got %#v", got)
	}
	if got[0].Kind != LogJoin || got[0].Player != "Rick" {
		t.Fatalf("bad join entry %#v", got[0])
	}
	if got[0].SteamID != "76561198000000002" {
		t.Fatalf("steam id not captured: %#v", got[0])
	}
	if got[1].Kind != LogLeave {
		t.Fatalf("bad leave entry %#v", got[1])
	}
	if len(tail.Poll(0)) != 0 {
		t.Fatal("a second poll with no new lines must return nothing")
	}
}

func TestTailerDetectsModUpdates(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "26-09-07_DebugLog-server.txt")
	writeFile(t, path, "boot\n")
	tail := NewTailer(dir)
	tail.Poll(0)

	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	f.WriteString("[07-09-26 11:00:00.000] CheckModsNeedUpdate: Mods updated, please restart\n")
	f.Close()

	got := tail.Poll(0)
	if len(got) != 1 || got[0].Kind != LogModUpdate {
		t.Fatalf("mod update notice not detected: %#v", got)
	}
}

func TestTailerHandlesTruncation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "26-09-07_user.txt")
	writeFile(t, path, strings.Repeat("noise\n", 50))
	tail := NewTailer(dir)
	tail.Poll(0)

	// Simulate the file being replaced by a smaller one.
	writeFile(t, path, "[07-09-26 12:00:00.000] user Zed fully connected (76561198000000003).\n")
	got := tail.Poll(0)
	if len(got) != 1 || got[0].Player != "Zed" {
		t.Fatalf("truncation should reset the offset, got %#v", got)
	}
}

func TestTailFileReturnsTrailingLines(t *testing.T) {
	dir := t.TempDir()
	var sb strings.Builder
	for i := 0; i < 500; i++ {
		sb.WriteString("line\n")
	}
	sb.WriteString("final\n")
	writeFile(t, filepath.Join(dir, "a_user.txt"), sb.String())

	out, err := TailFile(dir, "a_user.txt", 10)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 10 || lines[len(lines)-1] != "final" {
		t.Fatalf("unexpected tail: %d lines, last %q", len(lines), lines[len(lines)-1])
	}
	if _, err := TailFile(dir, "../escape.txt", 10); err == nil {
		t.Fatal("TailFile must refuse path separators")
	}
}

func TestBackupRemovesLeftoverPartialArchives(t *testing.T) {
	_, base := buildServer(t)
	l := Detect(base)
	b := NewBackupper(t.TempDir())
	if err := os.MkdirAll(b.Dir("srv1"), 0o755); err != nil {
		t.Fatal(err)
	}
	leftover := filepath.Join(b.Dir("srv1"), "20260101-000000.tar.gz.partial")
	if err := os.WriteFile(leftover, []byte("half an archive"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Create("srv1", l, false, 5, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(leftover); !os.IsNotExist(err) {
		t.Fatal("a .partial left by an interrupted backup should be removed")
	}
}
