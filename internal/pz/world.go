package pz

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

// The world a dedicated server plays lives in Saves/Multiplayer/<server name>:
// the map chunks, player characters (players.db), vehicles and the world's
// mod data. Accounts, the whitelist and bans are kept in db/<server name>.db,
// outside Saves, so a reset leaves them alone.
const multiplayerDir = "Multiplayer"

// ErrNoWorld is returned when there is no world folder to reset.
var ErrNoWorld = errors.New("there is no world to reset: the Saves/Multiplayer folder is missing or empty")

// WorldInfo describes a server's Saves/Multiplayer folder.
type WorldInfo struct {
	// Dir is the Saves/Multiplayer folder, or empty if Saves was not found.
	Dir    string   `json:"dir,omitempty"`
	Exists bool     `json:"exists"`
	Worlds []string `json:"worlds"`
	Bytes  int64    `json:"bytes"`
	Files  int      `json:"files"`
	// Truncated is set when the folder was too big to count in full, so
	// Bytes and Files are a lower bound.
	Truncated bool `json:"truncated,omitempty"`
}

// World reports what a world reset would delete.
func World(l Layout) WorldInfo {
	info := WorldInfo{Worlds: []string{}}
	if l.SavesDir == "" {
		return info
	}
	info.Dir = filepath.Join(l.SavesDir, multiplayerDir)
	items, err := os.ReadDir(info.Dir)
	if err != nil {
		return info
	}
	info.Exists = true
	for _, it := range items {
		if it.IsDir() {
			info.Worlds = append(info.Worlds, it.Name())
		}
	}
	sort.Strings(info.Worlds)
	info.Bytes, info.Files, info.Truncated = DirSize(info.Dir, 200000)
	return info
}

// ResetWorld deletes a server's Saves/Multiplayer folder, so the server
// generates a new world the next time it starts. Nothing else is touched:
// the settings in Server/, the accounts in db/, mods and PZAdmin's own
// backups all stay.
//
// The caller must stop the server first and take any backup it wants
// beforehand. It shares the backup lock so a reset cannot run while a backup
// or restore of the same server is reading or writing the world.
func (b *Backupper) ResetWorld(serverID string, l Layout) (WorldInfo, error) {
	b.mu.Lock()
	if b.running[serverID] {
		b.mu.Unlock()
		return WorldInfo{}, ErrBackupRunning
	}
	b.running[serverID] = true
	b.mu.Unlock()
	defer func() {
		b.mu.Lock()
		delete(b.running, serverID)
		b.mu.Unlock()
	}()

	if l.SavesDir == "" {
		return WorldInfo{}, errors.New("PZAdmin could not find this server's Saves folder")
	}
	info := World(l)
	if !info.Exists {
		return info, ErrNoWorld
	}
	// A symlinked Multiplayer folder would otherwise have its link removed
	// and its real contents left behind, which is not a reset.
	if st, err := os.Lstat(info.Dir); err != nil {
		return info, err
	} else if st.Mode()&os.ModeSymlink != 0 {
		return info, fmt.Errorf("%s is a link to somewhere else; PZAdmin will not delete through it", info.Dir)
	}

	// Move the folder out of the way first, so the world is gone in one step
	// even if deleting a large tree fails part way.
	doomed := filepath.Join(l.SavesDir, multiplayerDir+".resetting")
	if err := os.RemoveAll(doomed); err != nil {
		return info, fmt.Errorf("could not clear %s left by an earlier reset: %w", doomed, err)
	}
	if err := os.Rename(info.Dir, doomed); err != nil {
		return info, fmt.Errorf("could not move the world aside: %w; nothing was deleted", err)
	}
	if err := os.RemoveAll(doomed); err != nil {
		return info, fmt.Errorf("the world was moved to %s but could not be fully deleted: %w; the server will still start a new world, delete that folder by hand", doomed, err)
	}
	return info, nil
}
