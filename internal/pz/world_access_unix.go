//go:build unix

package pz

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
)

// checkWorldAccess walks a world before anything is changed and reports the
// first thing PZAdmin's user could not handle: a folder it cannot delete
// from, or, when a backup is wanted, a file it cannot read.
//
// The game server usually runs in its own container. When that container's
// user is not PZAdmin's (PUID/PGID in its .env differ, or it ran as root),
// the world belongs to someone PZAdmin is not, and deleting it fails part way
// with a bare "permission denied". Checking first means nothing is touched
// and the error says exactly what to change.
func checkWorldAccess(savesDir string, dirs []string, needRead bool) error {
	// Deleting an entry needs write and search on the folder holding it.
	if err := accessible(savesDir, 0x2|0x1); err != nil {
		return err
	}
	for _, root := range dirs {
		err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				if errors.Is(err, fs.ErrNotExist) {
					return nil
				}
				return accessError(p, err)
			}
			switch {
			case d.IsDir():
				mode := uint32(0x2 | 0x1)
				if needRead {
					mode |= 0x4
				}
				return accessible(p, mode)
			case needRead && d.Type().IsRegular():
				return accessible(p, 0x4)
			}
			return nil
		})
		if err != nil {
			return err
		}
	}
	return nil
}

func accessible(p string, mode uint32) error {
	if err := syscall.Access(p, mode); err != nil {
		return accessError(p, err)
	}
	return nil
}

// accessError names the path, who owns it and who PZAdmin is, and gives
// the command that fixes it.
func accessError(p string, cause error) error {
	if !errors.Is(cause, fs.ErrPermission) {
		return fmt.Errorf("cannot check %s: %w", p, cause)
	}
	uid, gid := os.Getuid(), os.Getgid()
	owner := ""
	if st, err := os.Lstat(p); err == nil {
		if sys, ok := st.Sys().(*syscall.Stat_t); ok {
			owner = fmt.Sprintf(", which belongs to user %d:%d", sys.Uid, sys.Gid)
		}
	}
	return &AccessError{Path: p, msg: fmt.Sprintf(
		"PZAdmin (user %d:%d) is not allowed to change %s%s",
		uid, gid, p, owner)}
}
