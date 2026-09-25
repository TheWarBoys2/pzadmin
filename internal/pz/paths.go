// Package pz understands the on-disk shape of a Project Zomboid dedicated
// server: where its config, saves, logs and mods live, how to read and write
// its INI files, how to follow its logs, and how to archive it.
//
// Every path that comes from a request is resolved through SafeJoin, which
// refuses anything that escapes the configured root. Symlinks are resolved
// before the check so a link inside the tree cannot be used to read outside it.
package pz

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// ErrOutsideRoot is returned when a path escapes the configured PZ root.
var ErrOutsideRoot = errors.New("path is outside the Project Zomboid root")

// SafeJoin resolves rel against root and guarantees the result stays inside it.
//
// It is deliberately strict: the root must exist, the result is cleaned, and if
// the target exists its symlinks are resolved before the containment check.
func SafeJoin(root, rel string) (string, error) {
	if root == "" {
		return "", errors.New("no Project Zomboid root is configured")
	}
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(rootAbs); err == nil {
		rootAbs = resolved
	}

	rel = filepath.FromSlash(strings.TrimSpace(rel))
	// An absolute input is only acceptable if it already sits under the root.
	var candidate string
	if filepath.IsAbs(rel) {
		candidate = filepath.Clean(rel)
	} else {
		candidate = filepath.Clean(filepath.Join(rootAbs, rel))
	}

	// Resolve symlinks on the deepest existing ancestor so that a link placed
	// inside the tree cannot be followed out of it.
	probe := candidate
	for {
		if resolved, err := filepath.EvalSymlinks(probe); err == nil {
			suffix := strings.TrimPrefix(candidate, probe)
			candidate = filepath.Clean(resolved + suffix)
			break
		}
		parent := filepath.Dir(probe)
		if parent == probe {
			break
		}
		probe = parent
	}

	if candidate != rootAbs && !strings.HasPrefix(candidate, rootAbs+string(os.PathSeparator)) {
		return "", fmt.Errorf("%w: %s", ErrOutsideRoot, rel)
	}
	return candidate, nil
}

// Layout describes where the interesting directories of one server live.
type Layout struct {
	// Base is the folder the operator selected.
	Base string `json:"base"`
	// ConfigDir contains the *.ini and *_SandboxVars.lua files.
	ConfigDir string `json:"configDir,omitempty"`
	// SavesDir contains the world folders.
	SavesDir string `json:"savesDir,omitempty"`
	// LogsDir contains the dated log files.
	LogsDir string `json:"logsDir,omitempty"`
	// ModDirs are every directory that may contain installed mods.
	ModDirs []string `json:"modDirs,omitempty"`
	// BackupDir is Project Zomboid's own backup folder, if present.
	BackupDir string `json:"backupDir,omitempty"`
	// GameDir is this server's own Project Zomboid installation: the directory
	// containing media/scripts. Servers are commonly installed one per folder
	// so that each can run its own build and mod set, so this is detected per
	// server rather than configured globally.
	GameDir string `json:"gameDir,omitempty"`
}

// Valid reports whether enough of the layout was found to be useful.
func (l Layout) Valid() bool { return l.ConfigDir != "" }

// candidateRoots returns the directories that may hold a Zomboid data tree.
// Docker images differ: some put everything at the root of the mount, others
// nest it under projectzomboid/, config/ or data/.
func candidateRoots(base string) []string {
	base = filepath.Clean(base)
	seen := map[string]bool{}
	var out []string
	add := func(p string) {
		if seen[p] {
			return
		}
		if st, err := os.Stat(p); err == nil && st.IsDir() {
			seen[p] = true
			out = append(out, p)
		}
	}
	add(base)
	first := []string{"projectzomboid", "Zomboid", "config", "data", "server"}
	for _, a := range first {
		pa := filepath.Join(base, a)
		add(pa)
		for _, b := range []string{"config", "data", "Zomboid", "projectzomboid"} {
			add(filepath.Join(pa, b))
		}
	}
	return out
}

// Detect inspects base and works out where everything lives.
func Detect(base string) Layout {
	l := Layout{Base: filepath.Clean(base)}
	for _, root := range candidateRoots(base) {
		if l.ConfigDir == "" && hasINI(filepath.Join(root, "Server")) {
			l.ConfigDir = filepath.Join(root, "Server")
		}
		if l.SavesDir == "" && isDir(filepath.Join(root, "Saves")) {
			l.SavesDir = filepath.Join(root, "Saves")
		}
		if l.LogsDir == "" && isDir(filepath.Join(root, "Logs")) {
			l.LogsDir = filepath.Join(root, "Logs")
		}
		if l.BackupDir == "" && isDir(filepath.Join(root, "backups")) {
			l.BackupDir = filepath.Join(root, "backups")
		}
		for _, m := range []string{
			"mods",
			"Workshop",
			filepath.Join("steamapps", "workshop", "content", "108600"),
			filepath.Join("Steam", "steamapps", "workshop", "content", "108600"),
		} {
			p := filepath.Join(root, m)
			if isDir(p) && !contains(l.ModDirs, p) {
				l.ModDirs = append(l.ModDirs, p)
			}
		}
	}
	sort.Strings(l.ModDirs)
	if l.GameDir == "" {
		l.GameDir = DetectGameRoot(l.Base)
	}
	return l
}

// LooksLikeServer reports whether base contains a Project Zomboid server tree.
func LooksLikeServer(base string) bool { return Detect(base).Valid() }

// BrowseEntry is one directory shown by the folder picker.
type BrowseEntry struct {
	Name       string `json:"name"`
	Relative   string `json:"relative"`
	IsServer   bool   `json:"isServer"`
	ChildCount int    `json:"childCount"`
}

// Browse lists the subdirectories of rel within root, flagging the ones that
// look like Project Zomboid servers.
func Browse(root, rel string) (string, []BrowseEntry, bool, error) {
	abs, err := SafeJoin(root, rel)
	if err != nil {
		return "", nil, false, err
	}
	st, err := os.Stat(abs)
	if err != nil || !st.IsDir() {
		return "", nil, false, fmt.Errorf("no such directory: %s", rel)
	}
	items, err := os.ReadDir(abs)
	if err != nil {
		return "", nil, false, err
	}
	rootAbs, _ := SafeJoin(root, ".")
	out := make([]BrowseEntry, 0, len(items))
	for _, it := range items {
		if !it.IsDir() || strings.HasPrefix(it.Name(), ".") {
			continue
		}
		child := filepath.Join(abs, it.Name())
		r, err := filepath.Rel(rootAbs, child)
		if err != nil {
			continue
		}
		kids, _ := os.ReadDir(child)
		out = append(out, BrowseEntry{
			Name:       it.Name(),
			Relative:   filepath.ToSlash(r),
			IsServer:   LooksLikeServer(child),
			ChildCount: len(kids),
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].IsServer != out[j].IsServer {
			return out[i].IsServer
		}
		return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name)
	})
	return abs, out, LooksLikeServer(abs), nil
}

func isDir(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}

func hasINI(dir string) bool {
	items, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, it := range items {
		if !it.IsDir() && strings.HasSuffix(strings.ToLower(it.Name()), ".ini") {
			return true
		}
	}
	return false
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

// DirSize returns the total byte size of a directory tree, stopping early once
// the walk exceeds maxEntries so a huge Saves folder cannot stall a request.
func DirSize(dir string, maxEntries int) (int64, int, bool) {
	var total int64
	var count int
	truncated := false
	_ = filepath.WalkDir(dir, func(_ string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			return nil
		}
		count++
		if maxEntries > 0 && count > maxEntries {
			truncated = true
			return filepath.SkipAll
		}
		if info, err := d.Info(); err == nil {
			total += info.Size()
		}
		return nil
	})
	return total, count, truncated
}

func joinPath(parts ...string) string { return filepath.Join(parts...) }

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}
