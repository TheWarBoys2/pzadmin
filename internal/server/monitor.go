package server

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math/rand"
	"strings"
	"time"

	"github.com/TheWarBoys2/pzadmin/internal/arcane"
	"github.com/TheWarBoys2/pzadmin/internal/config"
	"github.com/TheWarBoys2/pzadmin/internal/notify"
	"github.com/TheWarBoys2/pzadmin/internal/pz"
	"github.com/TheWarBoys2/pzadmin/internal/rcon"
	"github.com/TheWarBoys2/pzadmin/internal/store"
)

// monitorSupervisor keeps one monitor goroutine per enabled server, starting and
// stopping them as the configuration changes.
//
// Each server gets its own goroutine so one offline server, waiting out a
// five second dial timeout, never delays the checks of the others.
func (a *App) monitorSupervisor() {
	defer a.wg.Done()
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			a.syncMonitors()
		case <-a.stop:
			return
		}
	}
}

func (a *App) syncMonitors() {
	cfg := a.cfg.Get()
	want := map[string]config.Server{}
	for _, s := range cfg.Servers {
		if s.Enabled && !s.Missing {
			want[s.ID] = s
		}
	}

	a.mu.Lock()
	for id, cancel := range a.monitors {
		if _, keep := want[id]; !keep {
			cancel()
			delete(a.monitors, id)
		}
	}
	var toStart []config.Server
	for id, s := range want {
		if _, running := a.monitors[id]; running {
			continue
		}
		ctx, cancel := context.WithCancel(a.ctx)
		if !a.spawn(func() { a.monitorServer(ctx, id) }) {
			cancel()
			continue
		}
		a.monitors[id] = cancel
		toStart = append(toStart, s)
	}
	// Prune status for servers that no longer exist.
	for id := range a.status {
		exists := false
		for _, s := range cfg.Servers {
			if s.ID == id {
				exists = true
				break
			}
		}
		if !exists {
			delete(a.status, id)
		}
	}
	a.mu.Unlock()

	for _, s := range toStart {
		log.Printf("monitoring %s (%s:%d)", s.Name, s.Host, s.RCONPort)
	}
}

// monitorServer probes one server on its own schedule until the context ends.
func (a *App) monitorServer(ctx context.Context, serverID string) {
	// Stagger startup so several servers do not all probe on the same tick.
	jitter := time.Duration(rand.Intn(1500)) * time.Millisecond
	select {
	case <-time.After(jitter):
	case <-ctx.Done():
		return
	}

	for {
		s, ok := a.cfg.Server(serverID)
		if !ok || !s.Enabled {
			return
		}
		a.probe(ctx, s)
		a.pollLogs(s)
		a.checkPendingRestart(ctx, s)

		interval := time.Duration(a.cfg.Get().Interface.PollSeconds) * time.Second
		if interval < 3*time.Second {
			interval = 10 * time.Second
		}
		select {
		case <-time.After(interval):
		case <-ctx.Done():
			return
		}
	}
}

func classifyRCONError(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, rcon.ErrAuthFailed):
		return "auth"
	case strings.Contains(err.Error(), "connection refused"):
		return "refused"
	case strings.Contains(err.Error(), "timeout"), strings.Contains(err.Error(), "i/o timeout"):
		return "timeout"
	case strings.Contains(err.Error(), "no such host"):
		return "dns"
	default:
		return "other"
	}
}

// friendlyRCONError turns a Go error into something an operator can act on.
func friendlyRCONError(kind string, s config.Server) string {
	switch kind {
	case "auth":
		return "The RCON password was rejected. Check RCONPassword in the server's .ini."
	case "refused":
		return fmt.Sprintf("Nothing is listening on %s:%d. The server may still be starting, or RCON may be disabled.", s.Host, s.RCONPort)
	case "timeout":
		return "The server did not answer in time. It may be saving, overloaded, or unreachable through the network."
	case "dns":
		return fmt.Sprintf("The host name %q could not be resolved.", s.Host)
	default:
		return "The server could not be reached over RCON."
	}
}

func (a *App) probe(ctx context.Context, s config.Server) {
	layout := detectLayout(s)
	start := time.Now()
	client := a.rconFor(s)

	type result struct {
		out string
		err error
	}
	done := make(chan result, 1)
	go func() {
		out, err := client.Exec("players")
		done <- result{out, err}
	}()

	var res result
	select {
	case res = <-done:
	case <-ctx.Done():
		return
	case <-time.After(20 * time.Second):
		res = result{err: errors.New("rcon: probe timed out")}
	}

	latency := time.Since(start).Milliseconds()
	prev := a.statusOf(s.ID)
	report := a.scanner.Scan(layout)

	if res.err == nil {
		players := rcon.ParsePlayers(res.out)
		names := make([]string, 0, len(players))
		for _, p := range players {
			names = append(names, p.Name)
		}
		joined, left := a.store.SyncPlayers(s.ID, names)

		st := a.updateStatus(s.ID, func(st *Status) {
			st.Name = s.Name
			st.Enabled = true
			st.Online = true
			st.Stopped = false
			st.Players = names
			st.PlayerCount = len(names)
			st.LatencyMS = latency
			st.LastCheck = TimeNow()
			st.LastOnline = TimeNow()
			st.Error = ""
			st.ErrorKind = ""
			st.Layout = layout
			st.ModsEnabled = countEnabled(report)
			st.ModsMissing = report.Missing
			st.ConsecutiveFailures = 0
			st.RecoveryAttempts = 0
			// It answered, so whatever we did to it is finished.
			st.Restarting = false
			st.RestartingSince = NoTime
			st.RestartReason = ""
			st.restartSource = ""
			st.Container = s.DockerContainer
			st.BackupRunning = a.backup.Running(s.ID)
		})

		if !prev.Online && !prev.LastCheck.IsZero() {
			message := s.Name + " is back online"
			detail := fmt.Sprintf("Reachable again after %s.", humanSince(prev.LastOnline.Time))
			if prev.Restarting {
				message = s.Name + " finished restarting"
				detail = fmt.Sprintf("Back up after %s (%s).",
					humanSince(prev.RestartingSince.Time), orDefault(prev.RestartReason, "restart"))
			}
			a.event(store.Event{
				Kind: "server.up", Severity: store.SevSuccess, Source: "monitor",
				ServerID: s.ID, Server: s.Name, Message: message, Detail: detail,
				Meta: map[string]any{"restartedBy": restartedBy(prev)},
			})
		}
		for _, n := range joined {
			a.event(store.Event{Kind: "player.join", Source: "monitor", ServerID: s.ID, Server: s.Name,
				Message: n + " joined", Meta: map[string]any{"player": n}})
		}
		for _, n := range left {
			a.event(store.Event{Kind: "player.leave", Source: "monitor", ServerID: s.ID, Server: s.Name,
				Message: n + " left", Meta: map[string]any{"player": n}})
		}

		a.store.AddSample(store.Sample{ServerID: s.ID, Online: true, Players: len(names), Latency: latency})
		// A server that has just come up may be running a different build,
		// so its commands are read again.
		a.autoCheckCapabilities(s, !prev.Online)
		a.refreshContainerState(ctx, s)
		a.refreshDiskFacts(s, layout, st)
		return
	}

	kind := classifyRCONError(res.err)
	// A restart that has run past its window is a real failure after all.
	window := prev.restartWindow
	if window <= 0 {
		window = restartWindow
	}
	stalled := prev.Restarting && prev.RestartingSince.Since() > window
	st := a.updateStatus(s.ID, func(st *Status) {
		if stalled {
			st.Restarting = false
			st.RestartingSince = NoTime
			st.RestartReason = ""
			st.restartSource = ""
		}
		st.Name = s.Name
		st.Enabled = true
		st.Online = false
		st.Players = nil
		st.PlayerCount = 0
		st.LatencyMS = 0
		st.LastCheck = TimeNow()
		st.Error = friendlyRCONError(kind, s)
		st.ErrorKind = kind
		st.Layout = layout
		st.ModsEnabled = countEnabled(report)
		st.ModsMissing = report.Missing
		st.ConsecutiveFailures++
		st.Container = s.DockerContainer
		st.BackupRunning = a.backup.Running(s.ID)
	})

	if prev.Online {
		a.store.MarkOffline(s.ID)
		// Do not cry outage over a restart or a stop we asked for.
		if !st.Restarting && !st.Stopped {
			a.event(store.Event{
				Kind: "server.down", Severity: store.SevError, Source: "monitor",
				ServerID: s.ID, Server: s.Name,
				Message: s.Name + " went offline",
				Detail:  st.Error,
			})
		}
	}
	if stalled {
		a.event(store.Event{
			Kind: "server.down", Severity: store.SevError, Source: "monitor",
			ServerID: s.ID, Server: s.Name,
			Message: s.Name + " has not come back from its restart",
			Detail: fmt.Sprintf("Still unreachable %s after the restart began. %s",
				humanSince(prev.RestartingSince.Time), st.Error),
		})
	}
	a.store.AddSample(store.Sample{ServerID: s.ID, Online: false, Players: 0})
	a.refreshContainerState(ctx, s)
	// Recovery decides on the container state just read, not the one from
	// before this probe.
	st.ContainerState = a.statusOf(s.ID).ContainerState
	a.maybeRecover(ctx, s, st)
}

func countEnabled(r pz.ModReport) int {
	n := 0
	for _, m := range r.Mods {
		if m.Enabled {
			n++
		}
	}
	return n
}

// maybeRecover restarts a wedged container, subject to a failure threshold, a
// cooldown, and a cap on attempts within a single outage.
//
// The old implementation fired at exactly the sixth consecutive failure, so a
// single skipped probe meant it never fired at all, and it had no cooldown.
func (a *App) maybeRecover(ctx context.Context, s config.Server, st Status) {
	if !s.Recovery.Enabled || s.DockerContainer == "" {
		return
	}
	// An authentication failure is a configuration mistake. Restarting the
	// container will not fix a wrong password, it will just kick everybody off
	// repeatedly, so the watchdog stays out of it.
	if st.ErrorKind == "auth" {
		return
	}
	// A restart we started is already in hand; piling another one on top would
	// interrupt the world load and could corrupt the save.
	if st.Restarting {
		return
	}
	// Only a running container can be wedged. An exited one was either
	// stopped on purpose, which must stay stopped, or crashed, which Docker's
	// restart policy already handles. Anything else (Arcane unreachable, the
	// container never created) is not something a stop and start can fix.
	if st.ContainerState != "running" {
		return
	}
	threshold := s.Recovery.FailuresBeforeRestart
	if threshold <= 0 {
		threshold = 5
	}
	if st.ConsecutiveFailures < threshold {
		return
	}
	if s.Recovery.MaxAttempts > 0 && st.RecoveryAttempts >= s.Recovery.MaxAttempts {
		return
	}
	cooldown := time.Duration(s.Recovery.CooldownMinutes) * time.Minute
	if cooldown <= 0 {
		cooldown = 10 * time.Minute
	}
	if !st.LastRecovery.IsZero() && st.LastRecovery.Since() < cooldown {
		return
	}

	attempt := st.RecoveryAttempts + 1
	a.updateStatus(s.ID, func(st *Status) {
		st.LastRecovery = TimeNow()
		st.RecoveryAttempts = attempt
	})

	a.event(store.Event{
		Kind: "server.recovered", Severity: store.SevWarn, Source: "monitor",
		ServerID: s.ID, Server: s.Name,
		Message: "Restarting " + s.Name + " automatically",
		Detail: fmt.Sprintf("Attempt %d after %d failed checks. Last error: %s",
			attempt, st.ConsecutiveFailures, st.Error),
		Meta: map[string]any{"reason": notify.ReasonCrash},
	})

	// The watchdog only fires when RCON has stopped answering, so the RCON
	// restart is not an option: the server cannot be asked to quit. This is
	// the one restart that goes through Arcane, as a hard stop and start.
	rctx, cancel := context.WithTimeout(ctx, 4*time.Minute)
	defer cancel()
	a.markRestarting(s.ID, "watchdog")
	err := a.arcane.Stop(rctx, s.DockerContainer)
	if err == nil {
		err = a.arcane.Start(rctx, s.DockerContainer)
	}
	if err != nil {
		a.clearRestarting(s.ID)
		a.event(store.Event{
			Kind: "server.recovered", Severity: store.SevError, Source: "monitor",
			ServerID: s.ID, Server: s.Name,
			Message: "Automatic restart of " + s.Name + " failed",
			Detail:  err.Error(),
		})
		return
	}
	a.dropRCON(s.ID)
}

func (a *App) refreshContainerState(ctx context.Context, s config.Server) {
	if s.DockerContainer == "" {
		a.updateStatus(s.ID, func(st *Status) {
			st.ContainerState = ""
			st.ContainerUptime = 0
			st.StartedAt = NoTime
		})
		return
	}
	dctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	d, err := a.arcane.Inspect(dctx, s.DockerContainer)
	if err != nil {
		a.updateStatus(s.ID, func(st *Status) {
			switch {
			case errors.Is(err, arcane.ErrNotAllowed):
				st.ContainerState = "not managed"
			case errors.Is(err, arcane.ErrNotFound):
				// The stack exists but has never been brought up.
				st.ContainerState = "not created"
			default:
				st.ContainerState = "unavailable"
			}
		})
		return
	}
	a.updateStatus(s.ID, func(st *Status) {
		st.ContainerState = d.State
		st.StartedAt = At(d.StartedAt)
		if d.Running && !d.StartedAt.IsZero() {
			st.ContainerUptime = int64(time.Since(d.StartedAt).Seconds())
		} else {
			st.ContainerUptime = 0
		}
	})
}

// refreshDiskFacts updates backup and save-size figures. These involve walking
// directories, so they run at most every five minutes rather than every probe.
func (a *App) refreshDiskFacts(s config.Server, layout pz.Layout, st Status) {
	if st.LastBackup.Since() < 5*time.Minute && st.BackupCount > 0 {
		return
	}
	archives := a.backup.List(s.ID)
	var total int64
	var last time.Time
	for _, ar := range archives {
		total += ar.Size
		if ar.CreatedAt.After(last) {
			last = ar.CreatedAt
		}
	}
	var savesBytes int64
	if layout.SavesDir != "" {
		savesBytes, _, _ = pz.DirSize(layout.SavesDir, 200000)
	}
	a.updateStatus(s.ID, func(st *Status) {
		st.BackupCount = len(archives)
		st.BackupBytes = total
		st.LastBackup = At(last)
		st.SavesBytes = savesBytes
	})
}

// --- log following ----------------------------------------------------------

func (a *App) tailerFor(s config.Server, layout pz.Layout) *pz.Tailer {
	a.mu.Lock()
	defer a.mu.Unlock()
	t, ok := a.tailers[s.ID]
	if ok && t.Dir() == layout.LogsDir {
		return t
	}
	t = pz.NewTailer(layout.LogsDir)
	a.tailers[s.ID] = t
	return t
}

// pollLogs turns new log lines into events. This is where Steam IDs, chat and
// mod-update notices come from: RCON alone cannot see any of it.
func (a *App) pollLogs(s config.Server) {
	layout := detectLayout(s)
	if layout.LogsDir == "" {
		return
	}
	entries := a.tailerFor(s, layout).Poll(200)
	for _, e := range entries {
		switch e.Kind {
		case pz.LogJoin, pz.LogLeave:
			if e.SteamID != "" && e.Player != "" {
				a.store.SetSteamID(s.ID, e.Player, e.SteamID)
			}
			// The RCON poll already reports joins and leaves; the log only adds
			// the Steam ID, so no duplicate event is recorded here.
		case pz.LogChat:
			a.store.Append(store.Event{
				At: e.At, Kind: "player.chat", Source: "monitor", ServerID: s.ID, Server: s.Name,
				Message: e.Player + ": " + truncate(e.Text, 300), Meta: map[string]any{"player": e.Player},
			})
		case pz.LogDeath:
			a.event(store.Event{
				At: e.At, Kind: "player.death", Severity: store.SevWarn, Source: "monitor",
				ServerID: s.ID, Server: s.Name, Message: e.Player + " died",
			})
		case pz.LogAdmin:
			a.store.Append(store.Event{
				At: e.At, Kind: "ingame.admin", Severity: store.SevWarn, Source: "monitor",
				ServerID: s.ID, Server: s.Name,
				Message: e.Player + " ran " + truncate(e.Text, 200) + " in game",
			})
		case pz.LogModUpdate:
			a.handleModUpdate(s, e)
		}
	}
}

// handleModUpdate reacts to Project Zomboid reporting that Workshop mods have
// changed. Left alone, a modded server will refuse new connections until it is
// restarted, so this is worth acting on promptly.
func (a *App) handleModUpdate(s config.Server, e pz.LogEntry) {
	prev := a.statusOf(s.ID)
	if prev.ModUpdateSeen.Since() < 30*time.Minute {
		return
	}
	a.updateStatus(s.ID, func(st *Status) { st.ModUpdateSeen = TimeNow() })

	if !s.Mods.WatchUpdates {
		return
	}
	detail := "Project Zomboid reported that Workshop mods have been updated. The server needs a restart to load them."
	if !s.Mods.AutoRestart {
		a.event(store.Event{
			Kind: "mods.update", Severity: store.SevWarn, Source: "monitor",
			ServerID: s.ID, Server: s.Name,
			Message: "Mod updates available for " + s.Name, Detail: detail,
		})
		return
	}

	delay := time.Duration(s.Mods.RestartDelayMinutes) * time.Minute
	if delay <= 0 {
		delay = 10 * time.Minute
	}
	at := time.Now().Add(delay)
	a.updateStatus(s.ID, func(st *Status) {
		st.PendingRestartAt = At(at)
		st.PendingReason = "mod update"
	})
	a.event(store.Event{
		Kind: "mods.update", Severity: store.SevWarn, Source: "monitor",
		ServerID: s.ID, Server: s.Name,
		Message: "Mod updates available for " + s.Name,
		Detail:  fmt.Sprintf("%s Restarting automatically at %s.", detail, at.In(a.cfg.Location()).Format("15:04")),
		Meta:    map[string]any{"restartIn": int(delay.Minutes())},
	})
	a.announce(s, fmt.Sprintf("Mods have updated. The server will restart in %d minutes.", int(delay.Minutes())))
}

// checkPendingRestart runs the countdown for a scheduled mod-update restart,
// announcing it to players as it approaches.
func (a *App) checkPendingRestart(ctx context.Context, s config.Server) {
	st := a.statusOf(s.ID)
	if st.PendingRestartAt.IsZero() {
		return
	}
	remaining := st.PendingRestartAt.Until()
	if remaining > 0 {
		for _, mark := range []int{5, 2, 1} {
			d := time.Duration(mark) * time.Minute
			if remaining <= d && remaining > d-time.Duration(a.cfg.Get().Interface.PollSeconds)*time.Second {
				a.announce(s, fmt.Sprintf("Server restart in %d minute%s.", mark, plural(mark)))
			}
		}
		return
	}

	a.updateStatus(s.ID, func(st *Status) {
		st.PendingRestartAt = NoTime
		st.PendingReason = ""
	})
	reason := st.PendingReason
	if reason == "" {
		reason = "scheduled"
	}
	// restartServer records the restart itself, so it is not announced twice.
	if err := a.restartServer(ctx, s, "monitor", "auto ("+reason+")", ""); err != nil {
		a.event(store.Event{
			Kind: "server.restart", Severity: store.SevError, Source: "monitor",
			ServerID: s.ID, Server: s.Name,
			Message: "Restart of " + s.Name + " failed", Detail: err.Error(),
		})
	}
}

// announce sends an in-game broadcast, ignoring failures: if the server is not
// answering there is nobody to tell.
func (a *App) announce(s config.Server, msg string) {
	client := a.rconFor(s)
	if _, err := client.Exec("servermsg " + rcon.Quote(msg)); err != nil {
		return
	}
}

// restartWindow is how long a server may stay in the "Restarting" state before
// PZAdmin gives up waiting and reports it as a genuine outage. Project Zomboid
// takes a while to load a large world, and every restart is now a fresh
// container start, which runs the image's Steam validate and Workshop
// downloads when UPDATE_ON_START is on, so this is generous.
const restartWindow = 15 * time.Minute

// markRestarting flags a server as deliberately down, so the board shows a
// restart in progress rather than an outage and the watchdog stands off.
func (a *App) markRestarting(serverID, reason string) {
	a.markRestartingFor(serverID, reason, restartWindow)
}

// markRestartingFor is markRestarting with a longer window, for starts that
// download the game or a pile of mods first.
func (a *App) markRestartingFor(serverID, reason string, window time.Duration) {
	a.updateStatus(serverID, func(st *Status) {
		st.Restarting = true
		st.RestartingSince = TimeNow()
		st.RestartReason = reason
		st.restartWindow = window
		st.restartSource = ""
	})
}

// clearRestarting ends the restart window.
func (a *App) clearRestarting(serverID string) {
	a.updateStatus(serverID, func(st *Status) {
		st.Restarting = false
		st.RestartingSince = NoTime
		st.RestartReason = ""
		st.restartWindow = 0
		st.restartSource = ""
	})
}

// restartServer restarts a server over RCON only: save, then quit, and let
// the container's restart policy (unless-stopped) bring it back. Nothing here
// touches Arcane or Docker, so scheduled and manual restarts keep working
// while Arcane is down. Player warnings happen before this is called.
//
// Without a restart policy that revives the container, quit is a stop, so
// that case is refused rather than leaving the server down.
//
// note is what the operator typed as the reason, if anything; players are
// told it.
func (a *App) restartServer(ctx context.Context, s config.Server, source, reason, note string) error {
	if !restartPolicyRevives(s.RestartPolicy) {
		return fmt.Errorf("%s has restart: %q in its compose file, so quitting would leave it down. "+
			"Set restart: unless-stopped and recreate the container", s.Name, s.RestartPolicy)
	}
	a.markRestarting(s.ID, reason)
	a.updateStatus(s.ID, func(st *Status) { st.restartSource = source })
	client := a.rconFor(s)
	if _, err := client.Exec("save"); err != nil {
		log.Printf("restart %s: save failed (continuing to quit, which also saves): %v", s.Name, err)
	} else {
		// Give Zomboid a moment to finish flushing before asking it to exit.
		select {
		case <-time.After(3 * time.Second):
		case <-ctx.Done():
			a.clearRestarting(s.ID)
			return ctx.Err()
		}
	}
	if _, err := client.Exec("quit"); err != nil {
		a.clearRestarting(s.ID)
		return fmt.Errorf("the server did not accept the quit command: %w", err)
	}
	a.dropRCON(s.ID)
	a.event(store.Event{
		Kind: "server.restart", Severity: store.SevWarn, Source: source,
		ServerID: s.ID, Server: s.Name,
		Message: s.Name + " restarting",
		Detail:  "Saved and quit over RCON; Docker's restart policy brings it back. Reason: " + reason,
		Meta:    map[string]any{"reason": restartReason(source, reason), "note": note},
	})
	return nil
}

// restartedBy is who asked for the restart or start a server has just come back from,
// or "" when it came back from an outage or anything else.
func restartedBy(prev Status) string {
	if !prev.Restarting {
		return ""
	}
	return prev.restartSource
}

// restartReason sorts a restart into the reasons players are told about.
func restartReason(source, reason string) string {
	switch {
	case source == "schedule":
		return notify.ReasonScheduled
	case strings.Contains(reason, "mod update"):
		return notify.ReasonMods
	case source == "ui" || source == "api":
		return notify.ReasonManual
	}
	return ""
}

// restartPolicyRevives reports whether Docker restarts a container that
// exited on its own. on-failure does not: quit exits cleanly with code 0.
func restartPolicyRevives(policy string) bool {
	switch strings.TrimSpace(policy) {
	case "unless-stopped", "always":
		return true
	}
	return false
}

// housekeepingLoop performs periodic maintenance.
func (a *App) housekeepingLoop() {
	defer a.wg.Done()
	ticker := time.NewTicker(10 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			a.sess.gc()
			a.keys.flush()
			a.store.Flush()
			if n := a.store.Prune(); n > 0 {
				log.Printf("housekeeping: removed %d expired history files", n)
			}
		case <-a.stop:
			return
		}
	}
}

func humanSince(t time.Time) string {
	if t.IsZero() {
		return "an unknown period"
	}
	d := time.Since(t).Round(time.Second)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh %dm", int(d.Hours()), int(d.Minutes())%60)
	default:
		return fmt.Sprintf("%dd %dh", int(d.Hours())/24, int(d.Hours())%24)
	}
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

func orDefault(v, fallback string) string {
	if strings.TrimSpace(v) == "" {
		return fallback
	}
	return v
}
