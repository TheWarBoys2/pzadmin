package pz

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// Root returns the folder archives are kept in.
func (b *Backupper) Root() string { return b.root }

// MoveFrom moves archives left in an older backups folder into this one, so
// pointing PZADMIN_BACKUP_DIR somewhere new does not strand the backups made
// before. An archive whose name is already taken here is left where it is.
// It returns how many archives moved and how many were left behind.
func (b *Backupper) MoveFrom(old string) (moved, left int, err error) {
	if old == "" || filepath.Clean(old) == filepath.Clean(b.root) {
		return 0, 0, nil
	}
	servers, err := os.ReadDir(old)
	if errors.Is(err, os.ErrNotExist) {
		return 0, 0, nil
	}
	if err != nil {
		return 0, 0, err
	}
	var errs []error
	for _, s := range servers {
		if !s.IsDir() {
			continue
		}
		from := filepath.Join(old, s.Name())
		to := filepath.Join(b.root, s.Name())
		files, err := os.ReadDir(from)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		for _, f := range files {
			name := f.Name()
			if f.IsDir() || !strings.HasSuffix(name, ".tar.gz") {
				continue
			}
			if _, err := os.Stat(filepath.Join(to, name)); err == nil {
				left++
				continue
			}
			if err := os.MkdirAll(to, 0o750); err != nil {
				return moved, left, err
			}
			if err := moveFile(filepath.Join(from, name), filepath.Join(to, name)); err != nil {
				errs = append(errs, err)
				left++
				continue
			}
			moved++
			if _, err := os.Stat(filepath.Join(from, name+".note")); err == nil {
				_ = moveFile(filepath.Join(from, name+".note"), filepath.Join(to, name+".note"))
			}
		}
		// Only succeeds once the folder is empty, which is what we want.
		_ = os.Remove(from)
	}
	_ = os.Remove(old)
	return moved, left, errors.Join(errs...)
}

// moveFile renames src to dst, copying across filesystems when a rename is
// not possible, as it is not between a Docker volume and a bind mount. The
// copy is written under a temporary name and renamed into place, so a crash
// part-way leaves the original untouched and no half-written archive.
func moveFile(src, dst string) error {
	if err := os.Rename(src, dst); err == nil {
		return nil
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp := dst + ".partial"
	out, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o640)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(tmp)
		return fmt.Errorf("copying %s: %w", filepath.Base(src), err)
	}
	if err := out.Sync(); err != nil {
		out.Close()
		os.Remove(tmp)
		return err
	}
	if err := out.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if info, err := in.Stat(); err == nil {
		_ = os.Chtimes(tmp, info.ModTime(), info.ModTime())
	}
	if err := os.Rename(tmp, dst); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Remove(src)
}
