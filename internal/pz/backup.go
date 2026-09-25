package pz

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Archive describes one PZAdmin backup on disk.
type Archive struct {
	Name      string    `json:"name"`
	Path      string    `json:"path"`
	Size      int64     `json:"size"`
	CreatedAt time.Time `json:"createdAt"`
	Files     int       `json:"files"`
	Note      string    `json:"note,omitempty"`
}

// BackupResult summarises a completed run.
type BackupResult struct {
	Archive  Archive       `json:"archive"`
	Duration time.Duration `json:"duration"`
	Pruned   []string      `json:"pruned,omitempty"`
}

// Backupper creates and restores archives. One backup runs at a time per
// server: two concurrent tars of the same world would both be inconsistent and
// would fight for disk bandwidth.
type Backupper struct {
	root string // directory holding all archives

	mu      sync.Mutex
	running map[string]bool
}

// NewBackupper stores archives beneath root.
func NewBackupper(root string) *Backupper {
	return &Backupper{root: root, running: map[string]bool{}}
}

// ErrBackupRunning is returned when a backup is already in progress.
var ErrBackupRunning = errors.New("a backup is already running for this server")

// Dir returns the archive directory for a server.
func (b *Backupper) Dir(serverID string) string { return filepath.Join(b.root, safeID(serverID)) }

// Running reports whether a backup is in progress.
func (b *Backupper) Running(serverID string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.running[serverID]
}

// Create archives a server's Saves directory, and its config directory when
// includeConfig is set. Old archives beyond keep are removed afterwards.
func (b *Backupper) Create(serverID string, l Layout, includeConfig bool, keep int, note string) (BackupResult, error) {
	b.mu.Lock()
	if b.running[serverID] {
		b.mu.Unlock()
		return BackupResult{}, ErrBackupRunning
	}
	b.running[serverID] = true
	b.mu.Unlock()
	defer func() {
		b.mu.Lock()
		delete(b.running, serverID)
		b.mu.Unlock()
	}()

	if l.SavesDir == "" && l.ConfigDir == "" {
		return BackupResult{}, errors.New("nothing to back up: no Saves or config directory was detected")
	}
	dir := b.Dir(serverID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return BackupResult{}, err
	}

	start := time.Now()
	name := fmt.Sprintf("%s.tar.gz", start.UTC().Format("20060102-150405"))
	finalPath := filepath.Join(dir, name)
	tmpPath := finalPath + ".partial"

	files, err := writeArchive(tmpPath, l, includeConfig)
	if err != nil {
		_ = os.Remove(tmpPath)
		return BackupResult{}, err
	}
	if err := os.Rename(tmpPath, finalPath); err != nil {
		_ = os.Remove(tmpPath)
		return BackupResult{}, err
	}
	if note != "" {
		_ = os.WriteFile(finalPath+".note", []byte(note), 0o644)
	}

	st, err := os.Stat(finalPath)
	if err != nil {
		return BackupResult{}, err
	}
	res := BackupResult{
		Archive: Archive{
			Name: name, Path: finalPath, Size: st.Size(),
			CreatedAt: start, Files: files, Note: note,
		},
		Duration: time.Since(start),
	}
	res.Pruned = b.prune(serverID, keep)
	return res, nil
}

func writeArchive(path string, l Layout, includeConfig bool) (int, error) {
	out, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return 0, err
	}
	defer out.Close()

	gz, err := gzip.NewWriterLevel(out, gzip.BestSpeed)
	if err != nil {
		return 0, err
	}
	tw := tar.NewWriter(gz)

	count := 0
	add := func(srcRoot, prefix string) error {
		if srcRoot == "" {
			return nil
		}
		return filepath.Walk(srcRoot, func(p string, info os.FileInfo, err error) error {
			if err != nil {
				// A file vanishing mid-backup (Zomboid rotating a save chunk)
				// must not abort the whole archive.
				if errors.Is(err, os.ErrNotExist) {
					return nil
				}
				return err
			}
			rel, err := filepath.Rel(srcRoot, p)
			if err != nil {
				return err
			}
			if rel == "." {
				return nil
			}
			// Skip anything that is not a regular file or directory: sockets
			// and devices have no place in a world backup.
			if !info.Mode().IsRegular() && !info.IsDir() {
				return nil
			}
			hdr, err := tar.FileInfoHeader(info, "")
			if err != nil {
				return err
			}
			hdr.Name = filepath.ToSlash(filepath.Join(prefix, rel))
			if info.IsDir() {
				hdr.Name += "/"
			}
			if err := tw.WriteHeader(hdr); err != nil {
				return err
			}
			if info.IsDir() {
				return nil
			}
			f, err := os.Open(p)
			if err != nil {
				if errors.Is(err, os.ErrNotExist) {
					return nil
				}
				return err
			}
			defer f.Close()
			if _, err := io.Copy(tw, f); err != nil {
				return err
			}
			count++
			return nil
		})
	}

	if err := add(l.SavesDir, "Saves"); err != nil {
		tw.Close()
		gz.Close()
		return 0, fmt.Errorf("archive saves: %w", err)
	}
	if includeConfig {
		if err := add(l.ConfigDir, "Server"); err != nil {
			tw.Close()
			gz.Close()
			return 0, fmt.Errorf("archive config: %w", err)
		}
	}
	if err := tw.Close(); err != nil {
		gz.Close()
		return 0, err
	}
	if err := gz.Close(); err != nil {
		return 0, err
	}
	return count, out.Sync()
}

// List returns a server's archives, newest first.
func (b *Backupper) List(serverID string) []Archive {
	dir := b.Dir(serverID)
	items, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []Archive
	for _, it := range items {
		if it.IsDir() || !strings.HasSuffix(it.Name(), ".tar.gz") {
			continue
		}
		info, err := it.Info()
		if err != nil {
			continue
		}
		a := Archive{
			Name: it.Name(), Path: filepath.Join(dir, it.Name()),
			Size: info.Size(), CreatedAt: info.ModTime(),
		}
		if note, err := os.ReadFile(a.Path + ".note"); err == nil {
			a.Note = strings.TrimSpace(string(note))
		}
		if t, err := time.Parse("20060102-150405", strings.TrimSuffix(it.Name(), ".tar.gz")); err == nil {
			a.CreatedAt = t.UTC()
		}
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out
}

func (b *Backupper) prune(serverID string, keep int) []string {
	if keep <= 0 {
		return nil
	}
	all := b.List(serverID)
	if len(all) <= keep {
		return nil
	}
	var removed []string
	for _, a := range all[keep:] {
		if err := os.Remove(a.Path); err == nil {
			_ = os.Remove(a.Path + ".note")
			removed = append(removed, a.Name)
		}
	}
	return removed
}

// Delete removes one archive by name.
func (b *Backupper) Delete(serverID, name string) error {
	if name == "" || strings.ContainsAny(name, `/\`) || !strings.HasSuffix(name, ".tar.gz") {
		return fmt.Errorf("invalid archive name %q", name)
	}
	path := filepath.Join(b.Dir(serverID), name)
	if err := os.Remove(path); err != nil {
		return err
	}
	_ = os.Remove(path + ".note")
	return nil
}

// Open returns a reader for an archive so it can be downloaded off the host.
func (b *Backupper) Open(serverID, name string) (*os.File, os.FileInfo, error) {
	if name == "" || strings.ContainsAny(name, `/\`) || !strings.HasSuffix(name, ".tar.gz") {
		return nil, nil, fmt.Errorf("invalid archive name %q", name)
	}
	path := filepath.Join(b.Dir(serverID), name)
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, nil, err
	}
	return f, st, nil
}

// Verify walks an archive without extracting it, confirming it is readable and
// counting its contents. Worth doing before you rely on a backup.
func (b *Backupper) Verify(serverID, name string) (int, int64, error) {
	f, _, err := b.Open(serverID, name)
	if err != nil {
		return 0, 0, err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return 0, 0, fmt.Errorf("archive is not readable: %w", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	var files int
	var bytes int64
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return files, bytes, fmt.Errorf("archive is damaged after %d entries: %w", files, err)
		}
		if hdr.Typeflag == tar.TypeReg {
			files++
			n, err := io.Copy(io.Discard, tr)
			if err != nil {
				return files, bytes, fmt.Errorf("archive is damaged in %s: %w", hdr.Name, err)
			}
			bytes += n
		}
	}
	return files, bytes, nil
}

// Restore extracts an archive back over a server's directories.
//
// The caller is responsible for stopping the server first. Restoring into a
// running world produces corruption, so the HTTP layer refuses unless the
// server is offline.
func (b *Backupper) Restore(serverID, name string, l Layout) (int, error) {
	f, _, err := b.Open(serverID, name)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return 0, err
	}
	defer gz.Close()

	targets := map[string]string{}
	if l.SavesDir != "" {
		targets["Saves"] = l.SavesDir
	}
	if l.ConfigDir != "" {
		targets["Server"] = l.ConfigDir
	}
	if len(targets) == 0 {
		return 0, errors.New("no restore target: the server layout was not detected")
	}

	tr := tar.NewReader(gz)
	restored := 0
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return restored, err
		}
		clean := filepath.ToSlash(filepath.Clean(hdr.Name))
		if strings.HasPrefix(clean, "../") || strings.Contains(clean, "/../") {
			// Refuse path traversal inside the archive.
			continue
		}
		parts := strings.SplitN(clean, "/", 2)
		if len(parts) != 2 {
			continue
		}
		base, ok := targets[parts[0]]
		if !ok {
			continue
		}
		dest := filepath.Join(base, filepath.FromSlash(parts[1]))
		// Belt and braces: the resolved destination must stay under the target.
		if !strings.HasPrefix(dest, filepath.Clean(base)+string(os.PathSeparator)) {
			continue
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(dest, 0o755); err != nil {
				return restored, err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
				return restored, err
			}
			out, err := os.OpenFile(dest, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, os.FileMode(hdr.Mode)&0o777)
			if err != nil {
				return restored, err
			}
			if _, err := io.Copy(out, tr); err != nil {
				out.Close()
				return restored, err
			}
			out.Close()
			restored++
		}
	}
	return restored, nil
}

// TotalSize returns the disk used by a server's archives.
func (b *Backupper) TotalSize(serverID string) int64 {
	var total int64
	for _, a := range b.List(serverID) {
		total += a.Size
	}
	return total
}

func safeID(id string) string {
	var b strings.Builder
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	if b.Len() == 0 {
		return "unknown"
	}
	return b.String()
}
