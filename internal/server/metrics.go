package server

import (
	"crypto/subtle"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// handleMetrics exposes Prometheus text-format metrics.
//
// The endpoint sits outside the session check so a scraper can reach it, but it
// is protected by a bearer token when one is configured. The token is generated
// at setup, so this is not open by default.
func (a *App) handleMetrics(w http.ResponseWriter, r *http.Request) {
	cfg := a.cfg.Get()
	if !cfg.Metrics.Enabled {
		http.NotFound(w, r)
		return
	}
	if cfg.Metrics.Token != "" && !metricsTokenOK(r, cfg.Metrics.Token) {
		w.Header().Set("WWW-Authenticate", `Bearer realm="pzadmin"`)
		httpError(w, http.StatusUnauthorized, "a metrics token is required")
		return
	}

	var b strings.Builder
	write := func(name, help, typ string, lines ...string) {
		fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s %s\n", name, help, name, typ)
		for _, l := range lines {
			b.WriteString(l)
			b.WriteByte('\n')
		}
	}

	statuses := a.allStatus()
	var up, players, latency, uptime, backups, backupBytes, missing, failures []string
	for _, s := range statuses {
		lbl := fmt.Sprintf(`{server=%q,id=%q}`, s.Name, s.ServerID)
		up = append(up, fmt.Sprintf("pzadmin_server_up%s %d", lbl, boolToInt(s.Online)))
		players = append(players, fmt.Sprintf("pzadmin_players_online%s %d", lbl, s.PlayerCount))
		latency = append(latency, fmt.Sprintf("pzadmin_rcon_latency_ms%s %d", lbl, s.LatencyMS))
		uptime = append(uptime, fmt.Sprintf("pzadmin_container_uptime_seconds%s %d", lbl, s.ContainerUptime))
		backups = append(backups, fmt.Sprintf("pzadmin_backups_total%s %d", lbl, s.BackupCount))
		backupBytes = append(backupBytes, fmt.Sprintf("pzadmin_backup_bytes%s %d", lbl, s.BackupBytes))
		missing = append(missing, fmt.Sprintf("pzadmin_mods_missing%s %d", lbl, len(s.ModsMissing)))
		failures = append(failures, fmt.Sprintf("pzadmin_consecutive_failures%s %d", lbl, s.ConsecutiveFailures))
	}

	write("pzadmin_server_up", "1 when the server answered its last RCON probe", "gauge", up...)
	write("pzadmin_players_online", "Players currently connected", "gauge", players...)
	write("pzadmin_rcon_latency_ms", "Round-trip time of the last RCON probe", "gauge", latency...)
	write("pzadmin_container_uptime_seconds", "Seconds since the Docker container started", "gauge", uptime...)
	write("pzadmin_backups_total", "Number of PZAdmin archives retained", "gauge", backups...)
	write("pzadmin_backup_bytes", "Disk used by PZAdmin archives", "gauge", backupBytes...)
	write("pzadmin_mods_missing", "Mods enabled in config but not installed on disk", "gauge", missing...)
	write("pzadmin_consecutive_failures", "Consecutive failed RCON probes", "gauge", failures...)

	stats := a.store.Stats()
	write("pzadmin_known_players", "Players PZAdmin has ever seen", "gauge",
		fmt.Sprintf("pzadmin_known_players %d", stats.Players))
	write("pzadmin_history_bytes", "Disk used by PZAdmin history", "gauge",
		fmt.Sprintf("pzadmin_history_bytes %d", stats.DiskBytes))
	write("pzadmin_build_info", "PZAdmin build information", "gauge",
		fmt.Sprintf("pzadmin_build_info{version=%q} 1", Version))
	write("pzadmin_scrape_time_seconds", "Unix time of this scrape", "gauge",
		fmt.Sprintf("pzadmin_scrape_time_seconds %d", time.Now().Unix()))

	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	_, _ = w.Write([]byte(b.String()))
}

func metricsTokenOK(r *http.Request, want string) bool {
	given := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if given == "" {
		given = r.URL.Query().Get("token")
	}
	return subtle.ConstantTimeCompare([]byte(strings.TrimSpace(given)), []byte(want)) == 1
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
