//go:build !unix

package pz

// checkWorldAccess has nothing to check where file ownership works
// differently; the reset reports whatever error it meets instead.
func checkWorldAccess(savesDir string, dirs []string, needRead bool) error { return nil }
