//go:build !unix

package pz

// FreeBytes cannot be read here; -1 means unknown.
func FreeBytes(path string) int64 { return -1 }
