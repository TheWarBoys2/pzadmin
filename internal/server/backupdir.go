package server

import (
	"fmt"
	"log"
	"os"
	"path/filepath"

	"github.com/TheWarBoys2/pzadmin/internal/store"
)

// prepareBackupDir checks the backups folder can be written, and moves in any
// archives left in the old place inside the data folder.
//
// The folder is usually a bind mount. If it did not exist on the host, Docker
// created it owned by root, and PZAdmin, which does not run as root, cannot
// write to it. That would otherwise only show up at four in the morning when
// the first scheduled backup fails, so it is reported at start.
func (a *App) prepareBackupDir() {
	root := a.backup.Root()
	if err := writable(root); err != nil {
		msg := fmt.Sprintf("PZAdmin (user %d:%d) cannot write to the backups folder %s: %v. "+
			"On the host, give the folder you mounted there to that user, for example: "+
			"sudo chown %d:%d ./backups", os.Getuid(), os.Getgid(), root, err, os.Getuid(), os.Getgid())
		log.Print(msg)
		// backup.failed, so staff hear about it on Discord if they asked to.
		a.event(store.Event{Kind: "backup.failed", Severity: store.SevError, Source: "system",
			Message: "Backups cannot be saved", Detail: msg})
		return
	}

	old := filepath.Join(a.dataDir, "backups")
	if filepath.Clean(old) == filepath.Clean(root) {
		return
	}
	// Moving can take a while for big worlds, so it runs in the background.
	// Until it finishes, older archives are simply not listed yet.
	a.spawn(func() {
		moved, left, err := a.backup.MoveFrom(a.ctx, old)
		if moved == 0 && left == 0 && err == nil {
			return
		}
		detail := fmt.Sprintf("Moved %d backup(s) from %s to %s.", moved, old, root)
		sev := store.SevInfo
		if left > 0 {
			detail += fmt.Sprintf(" %d could not be moved and are still in %s.", left, old)
			sev = store.SevWarn
		}
		if err != nil {
			detail += " " + err.Error()
			sev = store.SevWarn
		}
		log.Print(detail)
		a.event(store.Event{Kind: "system.info", Severity: sev, Source: "system",
			Message: "Backups moved to the new backups folder", Detail: detail})
	})
}

// writable creates dir if needed and proves a file can be written in it.
func writable(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".writable-*")
	if err != nil {
		return err
	}
	name := f.Name()
	f.Close()
	return os.Remove(name)
}
