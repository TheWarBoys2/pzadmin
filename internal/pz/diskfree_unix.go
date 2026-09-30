//go:build unix

package pz

import "syscall"

// FreeBytes returns the space available to PZAdmin's user on the filesystem
// holding path, or -1 when it cannot be read.
func FreeBytes(path string) int64 {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return -1
	}
	return int64(st.Bavail) * int64(st.Bsize)
}
