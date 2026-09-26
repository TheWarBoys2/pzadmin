// Package fsutil writes PZAdmin's state files safely.
//
// Every file under /data is replaced whole: written to a temporary file in
// the same folder, flushed to disk, then renamed over the old one. A crash or
// power cut at any point leaves either the old file or the new one, never a
// half-written one.
package fsutil

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// WriteFile atomically replaces path with data. The folder is created if it
// does not exist. Each call uses its own temporary file, so two writers never
// interleave; the last rename wins.
func WriteFile(path string, data []byte, perm fs.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	ok := false
	defer func() {
		if !ok {
			_ = os.Remove(tmp)
		}
	}()
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Chmod(perm); err != nil {
		f.Close()
		return err
	}
	// Sync before the rename, or a power cut can leave a renamed but empty file.
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	ok = true
	// Sync the folder so the rename itself survives a power cut. Not every
	// filesystem supports this, and the data is already safe, so a failure
	// here is not an error.
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		d.Close()
	}
	return nil
}

// WriteJSON marshals v and writes it with WriteFile.
func WriteJSON(path string, v any, perm fs.FileMode) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return WriteFile(path, b, perm)
}

// CorruptError says a state file could not be parsed and where it was moved.
type CorruptError struct {
	Path  string
	Moved string
	Err   error
}

func (e *CorruptError) Error() string {
	if e.Moved == "" {
		return fmt.Sprintf("%s could not be read (%v) and could not be moved aside", e.Path, e.Err)
	}
	return fmt.Sprintf("%s could not be read (%v). It was moved to %s and PZAdmin started without it",
		e.Path, e.Err, filepath.Base(e.Moved))
}

func (e *CorruptError) Unwrap() error { return e.Err }

// ReadJSON loads path into v. A missing file is not an error: it returns
// false and leaves v alone. A file that cannot be parsed is moved aside to
// <name>.corrupt-<time> so the next save cannot overwrite the evidence, and a
// *CorruptError says where it went.
func ReadJSON(path string, v any) (found bool, err error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if err := json.Unmarshal(b, v); err != nil {
		moved := fmt.Sprintf("%s.corrupt-%s", path, time.Now().UTC().Format("20060102-150405"))
		if rerr := os.Rename(path, moved); rerr != nil {
			moved = ""
		}
		return false, &CorruptError{Path: path, Moved: moved, Err: err}
	}
	return true, nil
}
