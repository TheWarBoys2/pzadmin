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
	"github.com/TheWarBoys2/pzadmin/internal/config"
	"github.com/TheWarBoys2/pzadmin/internal/pz"
	"github.com/TheWarBoys2/pzadmin/internal/store"
)

// Resetting a world: deleting Saves/Multiplayer so the server builds a fresh
// map on its next start. For trying out a new mod list or sandbox rules.
//
// Only the world goes. Server settings, sandbox rules, mods, accounts and
// bans (db/), and PZAdmin's own backups stay. By default a backup is taken
// first with the server's usual backup settings, so a reset can be undone
// with an ordinary restore.

// handleWorldInfo says what a reset would delete.
func (a *App) handleWorldInfo(w http.ResponseWriter, r *http.Request) {
	srv, found := a.cfg.Server(r.URL.Query().Get("id"))
	if !found {
		httpError(w, http.StatusNotFound, "no such server")
		return
	}
	writeJSON(w, pz.World(detectLayout(srv)))
}

func (a *App) handleWorldReset(w http.ResponseWriter, r *http.Request) {
	var p struct {
		ServerID string `json:"serverId"`
		Confirm  string `json:"confirm"`
		// Backup is a pointer so a request that leaves it out still gets
		// a backup: skipping one has to be asked for.
		Backup *bool `json:"backup"`
	}
	if !decodeJSON(w, r, &p) {
		return
	}
	srv, found := a.cfg.Server(p.ServerID)
	if !found {
		httpError(w, http.StatusNotFound, "no such server")
		return
	}
	if strings.TrimSpace(p.Confirm) != srv.Name {
		httpError(w, http.StatusBadRequest, "Type the server's name, "+srv.Name+", to confirm.")
		return
	}
	if msg := a.worldInUse(r.Context(), srv); msg != "" {
		httpError(w, http.StatusConflict, msg)
		return
	}
	layout := detectLayout(srv)
	if layout.SavesDir == "" {
		httpError(w, http.StatusBadRequest, "PZAdmin could not find this server's Saves folder")
		return
	}
	before := pz.World(layout)
	if !before.Exists {
		httpError(w, http.StatusBadRequest, pz.ErrNoWorld.Error())
		return
	}

	backup := p.Backup == nil || *p.Backup
	// Checked before the backup, so a world PZAdmin may not delete is not
	// backed up for nothing, and a refused reset changes nothing at all.
	if err := pz.CheckWorld(layout, backup); err != nil {
		httpError(w, http.StatusConflict, worldAccessHelp(err, srv, layout))
		return
	}
	backupNote := "No backup was taken first."
	var archive string
	if backup {
		keep := srv.Backup.Keep
		if keep <= 0 {
			keep = 10
		}
		res, err := a.backup.Create(a.ctx, srv.ID, layout, srv.Backup.IncludeConfig, keep, "Before world reset")
		if errors.Is(err, pz.ErrBackupRunning) {
			httpError(w, http.StatusConflict, "a backup or restore is already running for "+srv.Name+"; wait for it to finish")
			return
		}
		if err != nil {
			a.event(store.Event{Kind: "backup.failed", Severity: store.SevError, Source: source(r), Actor: actor(r),
				ServerID: srv.ID, Server: srv.Name, Message: "Backup before world reset failed", Detail: err.Error()})
			httpError(w, http.StatusInternalServerError, "The backup failed, so the world was not touched: "+err.Error())
			return
		}
		archive = res.Archive.Name
		backupNote = "Backed up first to " + archive + "."
	}

	info, err := a.backup.ResetWorld(srv.ID, layout)
	if errors.Is(err, pz.ErrBackupRunning) {
		httpError(w, http.StatusConflict, "a backup or restore is already running for "+srv.Name+"; wait for it to finish")
		return
	}
	if err != nil {
		var denied *pz.AccessError
		if errors.As(err, &denied) {
			httpError(w, http.StatusConflict, worldAccessHelp(err, srv, layout)+" "+backupNote)
			return
		}
		a.event(store.Event{Kind: "world.reset.failed", Severity: store.SevError, Source: source(r), Actor: actor(r),
			ServerID: srv.ID, Server: srv.Name, Message: "World reset failed", Detail: err.Error() + " " + backupNote})
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	a.scanner.Invalidate(layout.Base)

	detail := fmt.Sprintf("Deleted %s (%s, %d files). %s", info.Dir, humanBytes(info.Bytes), info.Files, backupNote)
	if len(info.Worlds) > 0 {
		detail = fmt.Sprintf("Deleted %s: %s (%s, %d files). %s", info.Dir, strings.Join(info.Worlds, ", "),
			humanBytes(info.Bytes), info.Files, backupNote)
	}
	a.event(store.Event{Kind: "world.reset", Severity: store.SevWarn, Source: source(r), Actor: actor(r),
		ServerID: srv.ID, Server: srv.Name, Message: "Reset the world on " + srv.Name, Detail: detail})
	ok(w, map[string]any{"backup": archive, "deleted": info,
		"message": "World deleted. " + backupNote + " Start the server to generate a new world."})
}

// worldInUse returns why the world cannot be touched right now, or "".
// A container that is up but not yet answering RCON is still writing the
// world, so Docker is asked too when PZAdmin can reach it.
func (a *App) worldInUse(ctx context.Context, srv config.Server) string {
	st := a.statusOf(srv.ID)
	if st.Online || st.Restarting || st.Deploying {
		return srv.Name + " is running. Stop it first."
	}
	if srv.DockerContainer == "" {
		return ""
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	details, err := a.arcane.Inspect(ctx, srv.DockerContainer)
	switch {
	case err == nil:
		if details.Running {
			return srv.Name + "'s container is still running. Stop it first."
		}
	case errors.Is(err, arcane.ErrNotFound), errors.Is(err, arcane.ErrNotConfigured), errors.Is(err, arcane.ErrNotAllowed):
		// Docker is out of PZAdmin's reach for this server; RCON said it is down.
	default:
		return "PZAdmin cannot check " + srv.Name + "'s container, so it will not touch the world: " + err.Error()
	}
	return ""
}

// worldAccessHelp turns a permission problem into the commands that fix it.
// Server folders are mounted at the same path inside PZAdmin as on the host,
// so the path shown is the one to use there.
func worldAccessHelp(err error, srv config.Server, layout pz.Layout) string {
	var denied *pz.AccessError
	if !errors.As(err, &denied) {
		return err.Error()
	}
	uid, gid := os.Getuid(), os.Getgid()
	msg := fmt.Sprintf("%s. Nothing was changed. The game server wrote its world as a different user from PZAdmin "+
		"(often root). To fix it, run this on the host, then try again: sudo chown -R %d:%d %q",
		denied.Error(), uid, gid, layout.SavesDir)
	env := "the server's .env file"
	if srv.StackDir != "" {
		env = filepath.Join(srv.StackDir, ".env")
	}
	return msg + fmt.Sprintf(". So the game stops doing it, set PUID=%d and PGID=%d in %s and redeploy; "+
		"if the game's image ignores those, the chown is needed before each reset.", uid, gid, env)
}
