package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/TheWarBoys2/pzadmin/internal/config"
	"github.com/TheWarBoys2/pzadmin/internal/provision"
	"github.com/TheWarBoys2/pzadmin/internal/store"
)

// Port slots let a handful of forwarded ports serve any number of servers,
// as long as only a few run at once. Nothing about a slot is stored: a
// server holds a slot while its container is up and publishes that slot's
// ports, which is read from the stack folder and the container's state, so
// a restart of PZAdmin, a host reboot or a start from Arcane itself can
// never leave the bookkeeping wrong.
//
// Moving a server to another slot is a .env change (DEFAULT_PORT and
// UDP_PORT, which the compose file's port mappings read), followed by
// `docker compose up -d` through Arcane so Docker publishes the new ports.
// With the ports unchanged the same command just starts the container.

// holdsPorts reports whether a server's container has, or is about to have,
// its ports published. A container PZAdmin cannot see is counted as up,
// since starting a second server on its ports would fail anyway.
func holdsPorts(st Status) bool {
	if st.Deploying || st.Restarting || st.Online {
		return true
	}
	if st.Stopped {
		return false
	}
	switch st.ContainerState {
	case "running", "restarting", "paused", "removing", "unavailable":
		return true
	}
	return false
}

// slotNumber returns the slot whose game port a server publishes, or zero.
func slotNumber(slots config.PortSlots, s config.Server) int {
	for _, sl := range slots.Slots() {
		if s.GamePort == sl.GamePort || s.GamePort == sl.UDPPort ||
			(s.UDPPort != 0 && (s.UDPPort == sl.GamePort || s.UDPPort == sl.UDPPort)) {
			return sl.Number
		}
	}
	return 0
}

// slotHeld returns the slot a server is holding right now, or zero.
func slotHeld(slots config.PortSlots, s config.Server, st Status) int {
	if s.Missing || !holdsPorts(st) {
		return 0
	}
	return slotNumber(slots, s)
}

// slotHolders maps each busy slot to the server holding it, leaving out one
// server (the one about to start). A server that does not use slots but
// publishes ports in the range still blocks that slot.
func (a *App) slotHolders(except string) map[int]config.Server {
	cfg := a.cfg.Get()
	out := map[int]config.Server{}
	for _, s := range cfg.Servers {
		if s.ID == except {
			continue
		}
		if n := slotHeld(cfg.PortSlots, s, a.statusOf(s.ID)); n > 0 {
			if _, taken := out[n]; !taken {
				out[n] = s
			}
		}
	}
	return out
}

// pickSlot chooses a free slot for srv, preferring the one it used last so
// its join address stays the same whenever it can.
func pickSlot(slots config.PortSlots, srv config.Server, busy map[int]config.Server) (config.Slot, bool) {
	all := slots.Slots()
	if last := slotNumber(slots, srv); last > 0 {
		if _, taken := busy[last]; !taken {
			return all[last-1], true
		}
	}
	for _, sl := range all {
		if _, taken := busy[sl.Number]; !taken {
			return sl, true
		}
	}
	return config.Slot{}, false
}

// slotsBusyMessage explains a refused start, naming only servers p may see.
func slotsBusyMessage(slots config.PortSlots, busy map[int]config.Server, p principal) string {
	parts := make([]string, 0, len(busy))
	for _, sl := range slots.Slots() {
		s, ok := busy[sl.Number]
		if !ok {
			continue
		}
		name := "another server"
		if p.allows(s.ID) {
			name = s.Name
		}
		parts = append(parts, name+" on "+strconv.Itoa(sl.GamePort))
	}
	lead := "The port slot is in use"
	if slots.Count > 1 {
		lead = "All " + strconv.Itoa(slots.Count) + " port slots are in use"
	}
	return lead + ": " + strings.Join(parts, ", ") + ". Stop one of them first."
}

// startInSlot starts a slot server: pick a free slot, point .env at it, and
// deploy through Arcane. It answers at once; the deploy reports in Activity.
func (a *App) startInSlot(w http.ResponseWriter, r *http.Request, srv config.Server, reason string) {
	cfg := a.cfg.Get()
	if !a.arcane.Configured() {
		httpError(w, http.StatusConflict, "Starting a server in a port slot needs Arcane, which is not configured.")
		return
	}
	// One pick at a time, so two starts at once cannot take the same slot.
	// The server counts as holding its slot from the moment Deploying is
	// set, which happens before the lock is released.
	a.slotMu.Lock()
	busy := a.slotHolders(srv.ID)
	slot, free := pickSlot(cfg.PortSlots, srv, busy)
	if !free {
		a.slotMu.Unlock()
		writeStatusJSON(w, http.StatusConflict, map[string]any{
			"error": "Not starting " + srv.Name + ". " + slotsBusyMessage(cfg.PortSlots, busy, principalFrom(r)),
		})
		return
	}
	if a.statusOf(srv.ID).Deploying {
		a.slotMu.Unlock()
		httpError(w, http.StatusConflict, srv.Name+" is already being started")
		return
	}
	moved, err := a.moveToSlot(srv, slot)
	if err != nil {
		a.slotMu.Unlock()
		httpError(w, http.StatusConflict, "Not starting "+srv.Name+": "+err.Error())
		return
	}
	a.updateStatus(srv.ID, func(st *Status) {
		st.Deploying = true
		st.Stopped = false
		st.restartSource = source(r)
	})
	a.slotMu.Unlock()

	where := fmt.Sprintf("port slot %d (port %d)", slot.Number, slot.GamePort)
	detail := reason
	if moved != "" {
		detail = strings.TrimSpace(detail + ". Moved from " + moved + ".")
	}
	who := actor(r)
	src := source(r)
	started := a.spawn(func() {
		defer a.updateStatus(srv.ID, func(st *Status) { st.Deploying = false })
		ctx, cancel := context.WithTimeout(a.ctx, deployTimeout)
		defer cancel()
		project, err := a.findProject(ctx, srv)
		output := ""
		if err == nil {
			window := restartWindow
			if st, ok := a.stackFor(srv); ok && (!st.Configured || !st.Installed) {
				window = firstStartWindow
			}
			a.markRestartingFor(srv.ID, "starting", window)
			a.updateStatus(srv.ID, func(st *Status) { st.restartSource = src })
			output, err = a.arcane.Deploy(ctx, project)
		}
		if err != nil {
			a.clearRestarting(srv.ID)
			a.event(store.Event{Kind: "server.start", Severity: store.SevError, Source: src, Actor: who,
				ServerID: srv.ID, Server: srv.Name, Message: "Starting " + srv.Name + " in " + where + " failed",
				Detail: err.Error() + tailNote(output)})
			return
		}
		a.event(store.Event{Kind: "server.start", Severity: store.SevInfo, Source: src, Actor: who,
			ServerID: srv.ID, Server: srv.Name, Message: srv.Name + " started in " + where, Detail: detail})
	})
	if !started {
		a.updateStatus(srv.ID, func(st *Status) { st.Deploying = false })
		httpError(w, http.StatusServiceUnavailable, "PZAdmin is shutting down")
		return
	}
	ok(w, map[string]any{"message": "Starting " + srv.Name + " in " + where + ". The result appears in Activity.",
		"slot": slot})
}

// moveToSlot points a server's .env at a slot and checks that the compose
// file follows. It returns the port it moved from, or "" if it was already
// there. Called with slotMu held.
func (a *App) moveToSlot(srv config.Server, slot config.Slot) (string, error) {
	if srv.StackDir == "" || srv.Missing {
		return "", errors.New("its stack folder could not be found")
	}
	previous, changed, err := provision.SetEnvPorts(srv.StackDir, slot.GamePort, slot.UDPPort)
	if err != nil {
		return "", err
	}
	undo := func() {
		if changed {
			_ = os.WriteFile(filepath.Join(srv.StackDir, ".env"), previous, 0o600)
		}
	}
	st, found := a.stackFor(srv)
	if !found {
		undo()
		return "", errors.New("its stack folder could not be read")
	}
	if st.GamePort != slot.GamePort || st.UDPPort != slot.UDPPort {
		undo()
		return "", fmt.Errorf("its compose file publishes fixed game ports, so .env cannot move it. "+
			"Untick and tick \"Use a port slot\" in its settings to let PZAdmin fix that, or change the two "+
			"game port lines under ports: to \"${DEFAULT_PORT}:${DEFAULT_PORT}/udp\" and "+
			"\"${UDP_PORT}:${UDP_PORT}/udp\" (compose reads %d and %d at the moment)", st.GamePort, st.UDPPort)
	}
	if !st.Ready() {
		undo()
		return "", errors.New(strings.Join(st.Problems, "; "))
	}
	if !changed {
		return "", nil
	}
	a.keepEnvBackup(srv, previous)
	a.rescan()
	return strconv.Itoa(srv.GamePort), nil
}

// keepEnvBackup keeps a copy of .env before PZAdmin changes it.
func (a *App) keepEnvBackup(srv config.Server, previous []byte) {
	backupDir := filepath.Join(a.backup.Dir(srv.ID), "env")
	if err := os.MkdirAll(backupDir, 0o700); err == nil {
		_ = os.WriteFile(filepath.Join(backupDir, ".env-"+time.Now().UTC().Format("20060102-150405")), previous, 0o600)
	}
}

// enableSlot gets a server's stack folder ready for slots when "Use a port
// slot" is ticked: the compose file's game port mappings are made to read
// .env, with the old file kept beside it.
func (a *App) enableSlot(srv config.Server) (string, error) {
	cfg := a.cfg.Get()
	if !cfg.PortSlots.Enabled() {
		return "", errors.New("set up port slots in Settings first")
	}
	if srv.StackDir == "" || srv.ComposeFile == "" || srv.Missing {
		return "", errors.New("this server has no stack folder")
	}
	res, err := provision.UseEnvPorts(srv.StackDir, srv.ComposeFile)
	if err != nil {
		return "", err
	}
	if !res.Changed {
		return "", nil
	}
	a.rescan()
	return "Its compose file now takes the game ports from .env. The old file was kept as " +
		filepath.Base(res.Backup) + ".", nil
}

// slotsView is the port slot table for the dashboard, naming only servers p
// may see.
func (a *App) slotsView(p principal) map[string]any {
	cfg := a.cfg.Get()
	if !cfg.PortSlots.Enabled() {
		return map[string]any{"enabled": false}
	}
	busy := a.slotHolders("")
	rows := make([]map[string]any, 0, cfg.PortSlots.Count)
	for _, sl := range cfg.PortSlots.Slots() {
		row := map[string]any{"number": sl.Number, "gamePort": sl.GamePort, "udpPort": sl.UDPPort}
		if s, taken := busy[sl.Number]; taken {
			row["busy"] = true
			if p.allows(s.ID) {
				row["serverId"] = s.ID
				row["server"] = s.Name
			}
		}
		rows = append(rows, row)
	}
	return map[string]any{
		"enabled": true, "firstPort": cfg.PortSlots.FirstPort, "lastPort": cfg.PortSlots.LastPort(),
		"slots": rows,
	}
}

// joinPort is the port players connect to. A slot server's is whichever slot
// it last started in, so a join port typed in for it would go stale.
func joinPort(s config.Server) int {
	if s.UseSlot || s.Public.Port == 0 {
		return s.GamePort
	}
	return s.Public.Port
}

// statusWithSlot is statusOf with the slot filled in.
func (a *App) statusWithSlot(s config.Server) Status {
	st := a.statusOf(s.ID)
	st.Slot = slotHeld(a.cfg.Get().PortSlots, s, st)
	return st
}
