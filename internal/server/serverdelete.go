package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/TheWarBoys2/pzadmin/internal/arcane"
	"github.com/TheWarBoys2/pzadmin/internal/compose"
	"github.com/TheWarBoys2/pzadmin/internal/config"
	"github.com/TheWarBoys2/pzadmin/internal/store"
)

// Deleting a server, as opposed to forgetting one whose folder has gone.
//
// The order is chosen so that a failure at any step leaves something whole:
//
//  1. The container must already be stopped. Delete never stops a server
//     itself, so the operator has seen it go down and the game has saved.
//  2. The container is removed through Arcane, without its volumes.
//  3. The stack folder is moved, not deleted, into <stacks>/.pzadmin-deleted.
//     Discovery skips dot folders, and the compose file inside is renamed so
//     Arcane, which scans nested folders, does not list it as a project
//     either. Putting it back is a move and a rename.
//  4. Only if asked, the world, game files and logs are deleted for good.
//     They are usually most of the disk a server uses, so leaving them is the
//     default and the dialog says so.

// trashDir is where deleted stack folders go, inside the stacks folder so the
// move is a rename on the same mount.
const trashDir = ".pzadmin-deleted"

type deleteRequest struct {
	ID string `json:"id"`
	// Confirm must repeat the server's name.
	Confirm string `json:"confirm"`
	// DeleteData also deletes the world, game files and logs, permanently.
	DeleteData bool `json:"deleteData"`
}

// dataToDelete returns the folder holding a server's data and config, after
// checking it is safe to delete: strictly inside the data root, and not used
// by any other server.
func dataToDelete(srv config.Server, cfg config.Config) (string, error) {
	base := strings.TrimSpace(srv.PZPath)
	if base == "" {
		return "", fmt.Errorf("PZAdmin does not know where %s keeps its data, so it cannot delete it", srv.Name)
	}
	base = filepath.Clean(base)
	root := filepath.Clean(cfg.PZRoot)
	if cfg.PZRoot == "" || base == root || !sameOrUnder(root, base) {
		return "", fmt.Errorf("%s's data folder %s is not inside the data folder %s, so PZAdmin will not delete it",
			srv.Name, base, root)
	}
	for _, other := range cfg.Servers {
		if other.ID == srv.ID {
			continue
		}
		for _, p := range []string{other.PZPath, other.DataDir, other.ConfigDir, other.ServerDir, other.StackDir} {
			if p == "" {
				continue
			}
			if sameOrUnder(base, p) || sameOrUnder(p, base) {
				return "", fmt.Errorf("%s shares %s with %s, so it cannot be deleted without breaking that server",
					srv.Name, base, other.Name)
			}
		}
	}
	return base, nil
}

// stackToTrash checks a stack folder is one of the stacks folder's own
// subfolders and returns where it will be moved to.
func stackToTrash(srv config.Server, stacksRoot string, now time.Time) (string, error) {
	dir := filepath.Clean(srv.StackDir)
	root := filepath.Clean(stacksRoot)
	if srv.StackDir == "" || stacksRoot == "" || filepath.Dir(dir) != root {
		return "", fmt.Errorf("%s's stack folder %s is not directly inside the stacks folder %s",
			srv.Name, srv.StackDir, stacksRoot)
	}
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		return "", fmt.Errorf("%s's stack folder %s cannot be read", srv.Name, dir)
	}
	return filepath.Join(root, trashDir, filepath.Base(dir)+"-"+now.Format("20060102-150405")), nil
}

// moveStackToTrash moves the folder and renames every compose file in it, so
// neither PZAdmin nor Arcane finds it again.
func moveStackToTrash(dir, dest string) error {
	if err := os.MkdirAll(filepath.Dir(dest), 0o700); err != nil {
		return err
	}
	if err := os.Rename(dir, dest); err != nil {
		return err
	}
	for i := 0; i < 8; i++ {
		file := compose.FindComposeFile(dest)
		if file == "" {
			return nil
		}
		if err := os.Rename(file, file+".deleted"); err != nil {
			return fmt.Errorf("moved to %s, but renaming %s failed: %w", dest, filepath.Base(file), err)
		}
	}
	return fmt.Errorf("moved to %s, but it still contains a compose file", dest)
}

func (a *App) handleServerDestroy(w http.ResponseWriter, r *http.Request) {
	var p deleteRequest
	if !decodeJSON(w, r, &p) {
		return
	}
	srv, found := a.cfg.Server(p.ID)
	if !found {
		httpError(w, http.StatusNotFound, "no such server")
		return
	}
	if srv.Missing {
		httpError(w, http.StatusConflict, srv.Name+"'s stack folder has already gone. Forget it instead.")
		return
	}
	if strings.TrimSpace(p.Confirm) != srv.Name {
		httpError(w, http.StatusBadRequest, "Type the server's name, "+srv.Name+", to confirm.")
		return
	}
	cfg := a.cfg.Get()

	// Every check comes before anything is changed.
	dest, err := stackToTrash(srv, cfg.StacksRoot, time.Now())
	if err != nil {
		httpError(w, http.StatusConflict, err.Error())
		return
	}
	dataDir := ""
	if p.DeleteData {
		if dataDir, err = dataToDelete(srv, cfg); err != nil {
			httpError(w, http.StatusConflict, err.Error())
			return
		}
	}
	st := a.statusOf(srv.ID)
	if st.Online || st.Restarting || st.Deploying {
		httpError(w, http.StatusConflict, srv.Name+" is running. Stop it first, so the world is saved and "+
			"nobody is playing when it goes.")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	if srv.DockerContainer != "" {
		details, err := a.arcane.Inspect(ctx, srv.DockerContainer)
		switch {
		case errors.Is(err, arcane.ErrNotFound):
			// Never created, or already removed: nothing to do.
		case err != nil:
			httpError(w, http.StatusBadGateway, "Cannot check the container before deleting it: "+err.Error())
			return
		case details.Running:
			httpError(w, http.StatusConflict, srv.Name+"'s container is still running. Stop it first.")
			return
		default:
			if err := a.arcane.Remove(ctx, srv.DockerContainer); err != nil {
				httpError(w, http.StatusBadGateway, "Arcane did not remove the container, so nothing was deleted: "+err.Error())
				return
			}
		}
	}

	if err := moveStackToTrash(srv.StackDir, dest); err != nil {
		httpError(w, http.StatusInternalServerError, "The container was removed, but the stack folder was not moved: "+
			err.Error()+". The server will be found again on the next scan; move the folder by hand.")
		return
	}

	if err := a.removeServer(srv.ID); err != nil {
		// The next scan marks it missing, and Forget finishes the job.
		httpError(w, http.StatusInternalServerError, "The stack folder was moved to "+dest+
			", but PZAdmin could not update its own settings: "+err.Error())
		return
	}
	a.setCapabilities(srv.ID, nil)

	detail := "Container removed. Stack folder moved to " + dest + "."
	var dataErr error
	if dataDir != "" {
		if dataErr = os.RemoveAll(dataDir); dataErr == nil {
			// The per-server folder above it is empty now; tidy it if so.
			if parent := filepath.Dir(dataDir); parent != filepath.Clean(cfg.PZRoot) && sameOrUnder(cfg.PZRoot, parent) {
				_ = os.Remove(parent)
			}
			detail += " World, game files and logs at " + dataDir + " deleted."
			a.store.ForgetServer(srv.ID)
		} else {
			detail += " Deleting " + dataDir + " failed part-way: " + dataErr.Error()
		}
	} else if srv.PZPath != "" {
		detail += " World and game files kept at " + srv.PZPath + "."
	}
	a.event(store.Event{Kind: "admin.action", Severity: store.SevWarn, Source: "ui", Actor: actor(r),
		Server: srv.Name, Message: srv.Name + " deleted", Detail: detail})
	a.rescan()

	if dataErr != nil {
		httpError(w, http.StatusInternalServerError, srv.Name+" was deleted, but its data was not all removed: "+
			dataErr.Error()+". What is left is in "+dataDir+".")
		return
	}
	ok(w, map[string]any{"message": srv.Name + " deleted.", "trash": dest, "keptData": dataDir == "" && srv.PZPath != "",
		"dataDir": srv.PZPath})
}

// removeServer drops a server and its schedules from the config and stops
// watching it.
func (a *App) removeServer(id string) error {
	_, err := a.cfg.Update(func(c *config.Config) error {
		out := c.Servers[:0]
		for _, s := range c.Servers {
			if s.ID != id {
				out = append(out, s)
			}
		}
		c.Servers = append([]config.Server{}, out...)
		// Schedules pointing at a deleted server would silently never run.
		tasks := c.Schedules[:0]
		for _, t := range c.Schedules {
			if t.ServerID != id {
				tasks = append(tasks, t)
			}
		}
		c.Schedules = append([]config.Task{}, tasks...)
		return nil
	})
	a.dropRCON(id)
	a.syncMonitors()
	return err
}
