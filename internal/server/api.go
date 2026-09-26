package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/TheWarBoys2/pzadmin/internal/config"
	"github.com/TheWarBoys2/pzadmin/internal/cronx"
	"github.com/TheWarBoys2/pzadmin/internal/notify"
	"github.com/TheWarBoys2/pzadmin/internal/pz"
	"github.com/TheWarBoys2/pzadmin/internal/store"
)

// detectLayout finds a server's folders. Saves, logs, mods and the game
// installation are found under PZPath, which holds both data/ and config/.
// The ini and lua files are the exception: they live in the stack folder's
// Server/, which is bind mounted over config/Server. On the host that
// config/Server is only an empty mount point (or stale files from before the
// split), so it must never be used.
func detectLayout(s config.Server) pz.Layout {
	l := pz.Detect(s.PZPath)
	if s.ServerDir != "" {
		l.ConfigDir = s.ServerDir
	}
	if l.GameDir == "" && s.DataDir != "" {
		l.GameDir = pz.DetectGameRoot(s.DataDir)
	}
	return l
}

// --- session lifecycle ------------------------------------------------------

// handleBootstrap tells an unauthenticated browser what to render first.
func (a *App) handleBootstrap(w http.ResponseWriter, r *http.Request) {
	cfg := a.cfg.Get()
	authed := false
	if c, err := r.Cookie(sessionCookie); err == nil {
		_, authed = a.sess.lookup(c.Value)
	}
	writeJSON(w, map[string]any{
		"setupComplete":   cfg.SetupComplete,
		"authenticated":   authed,
		"version":         Version,
		"build":           a.assetBuild,
		"timezone":        cfg.Timezone,
		"serverTime":      time.Now().Format(time.RFC3339),
		"secureTransport": requestIsSecure(r),
	})
}

func (a *App) handleSetup(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		httpError(w, http.StatusMethodNotAllowed, "use POST")
		return
	}
	ip := clientIP(r)
	if allowed, wait := a.limiter.allow(ip); !allowed {
		w.Header().Set("Retry-After", strconv.Itoa(int(wait.Seconds())+1))
		httpError(w, http.StatusTooManyRequests,
			fmt.Sprintf("too many wrong setup codes, try again in %ds", int(wait.Seconds())+1))
		return
	}
	var p struct {
		SetupCode string `json:"setupCode"`
		Username  string `json:"username"`
		Password  string `json:"password"`
		Timezone  string `json:"timezone"`
	}
	if !decodeJSONLimit(w, r, &p, smallBody) {
		return
	}
	if a.cfg.Get().SetupComplete {
		httpError(w, http.StatusConflict, "PZAdmin is already set up")
		return
	}
	// The code is checked before anything else, so someone without it learns
	// nothing about the password rules or the timezone list.
	if !a.setupCodeOK(p.SetupCode) {
		a.limiter.fail(ip)
		httpError(w, http.StatusForbidden,
			"that setup code is not right. PZAdmin prints it in its log when it starts: run docker logs pzadmin")
		return
	}
	if err := validateCredentials(p.Username, p.Password); err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}
	tz := strings.TrimSpace(p.Timezone)
	if tz != "" {
		if _, err := time.LoadLocation(tz); err != nil {
			httpError(w, http.StatusBadRequest, "unknown timezone "+strconv.Quote(tz))
			return
		}
	}

	salt := config.NewSalt()
	if _, err := a.cfg.Update(func(c *config.Config) error {
		if c.SetupComplete {
			return fmt.Errorf("PZAdmin is already set up")
		}
		c.Username = strings.TrimSpace(p.Username)
		c.PasswordSalt = salt
		c.PasswordHash = config.HashPassword(p.Password, salt, 0)
		c.SetupComplete = true
		if tz != "" {
			c.Timezone = tz
		}
		return nil
	}); err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}

	a.clearSetupCode()
	a.limiter.succeed(ip)
	token, sess := a.sess.create(strings.TrimSpace(p.Username), ip, r.UserAgent())
	setSessionCookies(w, r, token, sess)
	a.event(store.Event{Kind: "auth.setup", Severity: store.SevSuccess, Source: "ui",
		Actor: p.Username, Message: "Administrator account created"})
	ok(w, map[string]any{"csrf": sess.CSRF})
}

func validateCredentials(username, password string) error {
	username = strings.TrimSpace(username)
	if len(username) < 2 || len(username) > 64 {
		return fmt.Errorf("the username must be between 2 and 64 characters")
	}
	if len(password) < 10 {
		return fmt.Errorf("the password must be at least 10 characters")
	}
	if len(password) > 256 {
		return fmt.Errorf("the password must be under 256 characters")
	}
	if strings.EqualFold(password, username) {
		return fmt.Errorf("the password cannot be the same as the username")
	}
	for _, weak := range []string{"password", "admin", "12345678", "zomboid", "changeme"} {
		if strings.EqualFold(strings.TrimSpace(password), weak) {
			return fmt.Errorf("that password is too easy to guess")
		}
	}
	return nil
}

func (a *App) handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		httpError(w, http.StatusMethodNotAllowed, "use POST")
		return
	}
	ip := clientIP(r)
	allowed, wait := a.limiter.allow(ip)
	if allowed {
		allowed, wait = a.accountLimit.allow(accountKey)
	}
	if !allowed {
		w.Header().Set("Retry-After", strconv.Itoa(int(wait.Seconds())+1))
		httpError(w, http.StatusTooManyRequests,
			fmt.Sprintf("too many failed attempts, try again in %ds", int(wait.Seconds())+1))
		return
	}
	var p struct{ Username, Password string }
	if !decodeJSONLimit(w, r, &p, smallBody) {
		return
	}

	select {
	case a.hashSlots <- struct{}{}:
		defer func() { <-a.hashSlots }()
	case <-r.Context().Done():
		return
	}
	cfg := a.cfg.Get()
	userOK := strings.EqualFold(strings.TrimSpace(p.Username), cfg.Username)
	// The hash is computed either way so a wrong username and a wrong password
	// take the same amount of time.
	passOK := config.VerifyPassword(p.Password, cfg.PasswordSalt, cfg.PasswordHash, cfg.PasswordIter)
	if !cfg.SetupComplete || !userOK || !passOK {
		a.limiter.fail(ip)
		a.accountLimit.fail(accountKey)
		a.store.Append(store.Event{Kind: "auth.failed", Severity: store.SevWarn, Source: "ui",
			Message: "Failed sign-in attempt", Detail: "from " + ip})
		httpError(w, http.StatusUnauthorized, "incorrect username or password")
		return
	}

	a.limiter.succeed(ip)
	a.accountLimit.succeed(accountKey)
	token, sess := a.sess.create(cfg.Username, ip, r.UserAgent())
	setSessionCookies(w, r, token, sess)
	a.store.Append(store.Event{Kind: "auth.login", Severity: store.SevInfo, Source: "ui",
		Actor: cfg.Username, Message: "Signed in", Detail: "from " + ip})
	ok(w, map[string]any{"csrf": sess.CSRF})
}

func (a *App) handleLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		a.sess.revoke(c.Value)
	}
	clearSessionCookies(w, r)
	ok(w, nil)
}

func (a *App) handlePassword(w http.ResponseWriter, r *http.Request) {
	var p struct {
		Current string `json:"current"`
		New     string `json:"new"`
	}
	if !decodeJSON(w, r, &p) {
		return
	}
	cfg := a.cfg.Get()
	if !config.VerifyPassword(p.Current, cfg.PasswordSalt, cfg.PasswordHash, cfg.PasswordIter) {
		httpError(w, http.StatusForbidden, "the current password is incorrect")
		return
	}
	if err := validateCredentials(cfg.Username, p.New); err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}
	salt := config.NewSalt()
	if _, err := a.cfg.Update(func(c *config.Config) error {
		c.PasswordSalt = salt
		c.PasswordHash = config.HashPassword(p.New, salt, 0)
		return nil
	}); err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	// Changing the password signs every browser out, including this one.
	a.sess.revokeAll()
	clearSessionCookies(w, r)
	a.event(store.Event{Kind: "auth.password", Severity: store.SevWarn, Source: "ui",
		Actor: actor(r), Message: "Administrator password changed",
		Detail: "All sessions were signed out."})
	ok(w, map[string]any{"signedOut": true})
}

func (a *App) handleSessions(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{"sessions": a.sess.list()})
}

func (a *App) handleRevokeSessions(w http.ResponseWriter, r *http.Request) {
	a.sess.revokeAll()
	clearSessionCookies(w, r)
	a.event(store.Event{Kind: "auth.revoke", Severity: store.SevWarn, Source: "ui",
		Actor: actor(r), Message: "All sessions signed out"})
	ok(w, map[string]any{"signedOut": true})
}

// --- state ------------------------------------------------------------------

func (a *App) handleState(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, a.snapshot())
}

func (a *App) snapshot() map[string]any {
	cfg := a.cfg.Get()
	redacted := config.Redact(cfg)

	tasks := make([]map[string]any, 0, len(cfg.Schedules))
	loc := a.cfg.Location()
	for _, t := range cfg.Schedules {
		entry := map[string]any{"task": t, "describe": cronx.Describe(t.Cron)}
		if s, err := cronx.Parse(t.Cron); err == nil {
			if next := s.Next(time.Now().In(loc)); !next.IsZero() {
				entry["nextRun"] = next.Format(time.RFC3339)
			}
		} else {
			entry["error"] = err.Error()
		}
		tasks = append(tasks, entry)
	}

	arc := a.arcaneState()
	return map[string]any{
		"config":    redacted,
		"servers":   config.SortServers(redacted.Servers),
		"status":    a.allStatus(),
		"schedules": tasks,
		"events":    a.store.Events("", "", 60),
		"players":   a.store.Players(""),
		// "docker" is kept for the current interface; "arcane" carries the detail.
		"docker":     map[string]any{"available": arc["available"], "message": arc["message"], "detail": arc["detail"]},
		"arcane":     arc,
		"storeStats": a.store.Stats(),
		"version":    Version,
		"serverTime": time.Now().Format(time.RFC3339),
		"timezone":   cfg.Timezone,
		"notifyOptions": map[string]any{
			"staffEvents":         config.NotifyEvents,
			"playerEvents":        config.PlayerEvents,
			"defaultStaffEvents":  config.DefaultStaffEvents,
			"defaultPlayerEvents": config.DefaultPlayerEvents,
			"playerTemplates":     notify.PlayerTemplates,
		},
	}
}

// handleStream is a Server-Sent Events feed. It replaces the old five second
// full-state poll: status is pushed as it changes and events arrive live.
func (a *App) handleStream(w http.ResponseWriter, r *http.Request) {
	flusher, canFlush := w.(http.Flusher)
	if !canFlush {
		httpError(w, http.StatusInternalServerError, "streaming is not supported by this connection")
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-store")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no") // stop nginx buffering the stream
	w.WriteHeader(http.StatusOK)

	events, cancel := a.store.Subscribe()
	defer cancel()

	send := func(kind string, payload any) bool {
		b, err := json.Marshal(payload)
		if err != nil {
			return true
		}
		if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", kind, b); err != nil {
			return false
		}
		flusher.Flush()
		return true
	}

	// The build comes first, so a tab that reconnects after an upgrade can
	// tell it is running the previous version's code.
	if !send("hello", map[string]any{"version": Version, "build": a.assetBuild}) {
		return
	}
	if !send("status", a.allStatus()) {
		return
	}

	statusTick := time.NewTicker(2 * time.Second)
	defer statusTick.Stop()
	keepalive := time.NewTicker(20 * time.Second)
	defer keepalive.Stop()

	var last string
	for {
		select {
		case <-r.Context().Done():
			return
		case <-a.streamsDone:
			return
		case e, open := <-events:
			if !open {
				return
			}
			if !send("event", e) {
				return
			}
		case <-statusTick.C:
			all := a.allStatus()
			b, _ := json.Marshal(all)
			// Only push when something actually changed, so an idle dashboard
			// costs nothing.
			if string(b) == last {
				continue
			}
			last = string(b)
			if !send("status", all) {
				return
			}
		case <-keepalive.C:
			if _, err := fmt.Fprint(w, ": keepalive\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

func (a *App) handleCommands(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{"commands": Commands()})
}

func (a *App) handleEvents(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	writeJSON(w, map[string]any{
		"events": a.store.Events(r.URL.Query().Get("serverId"), r.URL.Query().Get("kind"), limit),
	})
}

func (a *App) handleHistory(w http.ResponseWriter, r *http.Request) {
	hours, _ := strconv.Atoi(r.URL.Query().Get("hours"))
	if hours <= 0 || hours > 24*30 {
		hours = 24
	}
	points, _ := strconv.Atoi(r.URL.Query().Get("points"))
	writeJSON(w, map[string]any{
		"samples": a.store.History(r.URL.Query().Get("serverId"), time.Duration(hours)*time.Hour, points),
		"hours":   hours,
	})
}

func (a *App) handlePlayers(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{"players": a.store.Players(r.URL.Query().Get("serverId"))})
}

func (a *App) handlePlayerNote(w http.ResponseWriter, r *http.Request) {
	var p struct {
		ServerID string `json:"serverId"`
		Name     string `json:"name"`
		Note     string `json:"note"`
	}
	if !decodeJSON(w, r, &p) {
		return
	}
	if err := a.store.SetNote(p.ServerID, p.Name, p.Note); err != nil {
		httpError(w, http.StatusNotFound, err.Error())
		return
	}
	ok(w, nil)
}

func (a *App) handlePlayerForget(w http.ResponseWriter, r *http.Request) {
	var p struct{ ServerID string }
	if !decodeJSON(w, r, &p) {
		return
	}
	a.store.ForgetServer(p.ServerID)
	a.event(store.Event{Kind: "admin.action", Severity: store.SevWarn, Source: source(r), Actor: actor(r),
		ServerID: p.ServerID, Message: "Player history cleared"})
	ok(w, nil)
}

// --- servers ----------------------------------------------------------------

// handleServerSave updates what the operator controls about a server. Servers
// are created by discovery, never here, and everything discovery reads from
// the stack folder (container, RCON, ports, folders) is kept from the stored
// entry whatever the request says: the files are the truth, and the way to
// change them is to edit them.
func (a *App) handleServerSave(w http.ResponseWriter, r *http.Request) {
	var in config.Server
	if !decodeJSON(w, r, &in) {
		return
	}
	in.Name = strings.TrimSpace(in.Name)
	if in.ID == "" {
		httpError(w, http.StatusBadRequest, "servers are found from their stack folders, not added by hand. "+
			"Create a subfolder with a compose file under the stacks folder and PZAdmin will pick it up.")
		return
	}
	existing, found := a.cfg.Server(in.ID)
	if !found {
		httpError(w, http.StatusNotFound, "no such server")
		return
	}
	if in.Name == "" {
		in.Name = existing.Name
	}
	if in.Recovery.FailuresBeforeRestart == 0 {
		in.Recovery = config.DefaultRecovery()
	}
	if in.Backup.Keep == 0 {
		in.Backup = config.DefaultBackup()
	}

	public, err := cleanPublicInfo(in.Public)
	if err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}

	next := existing
	next.Public = public
	next.Name = in.Name
	next.Enabled = in.Enabled
	next.Notes = in.Notes
	next.SortHint = in.SortHint
	next.GameRoot = in.GameRoot
	next.Recovery = in.Recovery
	next.Mods = in.Mods
	next.Backup = in.Backup

	updated, err := a.cfg.Update(func(c *config.Config) error {
		for i := range c.Servers {
			if c.Servers[i].ID == next.ID {
				c.Servers[i] = next
				return nil
			}
		}
		return errors.New("the server disappeared while saving")
	})
	if err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}

	a.scanner.Invalidate(next.PZPath)
	a.syncMonitors()
	a.event(store.Event{Kind: "admin.action", Severity: store.SevInfo, Source: source(r), Actor: actor(r),
		ServerID: next.ID, Server: next.Name, Message: "Server settings updated"})

	server, _ := serverByID(updated.Servers, next.ID)
	redacted := config.Redact(config.Config{Servers: []config.Server{server}})
	writeJSON(w, map[string]any{"ok": true, "server": redacted.Servers[0]})
}

func (a *App) handleServerDelete(w http.ResponseWriter, r *http.Request) {
	var p struct {
		ID         string `json:"id"`
		ForgetData bool   `json:"forgetData"`
	}
	if !decodeJSON(w, r, &p) {
		return
	}
	srv, exists := a.cfg.Server(p.ID)
	if !exists {
		httpError(w, http.StatusNotFound, "no such server")
		return
	}
	// A server whose stack folder still exists would be found again on the
	// next scan, so removing it here would do nothing lasting. Disable it,
	// or move the folder out of the stacks folder.
	if !srv.Missing {
		httpError(w, http.StatusConflict, srv.Name+" still has a stack folder at "+srv.StackDir+
			". Disable it instead, or move that folder out of the stacks folder first.")
		return
	}
	if err := a.removeServer(p.ID); err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if p.ForgetData {
		a.store.ForgetServer(p.ID)
	}
	a.event(store.Event{Kind: "admin.action", Severity: store.SevWarn, Source: source(r), Actor: actor(r),
		Server: srv.Name, Message: "Server removed from PZAdmin",
		Detail: "Game files and backups on disk were left untouched."})
	ok(w, nil)
}

func (a *App) handleTestConnection(w http.ResponseWriter, r *http.Request) {
	var in config.Server
	if !decodeJSON(w, r, &in) {
		return
	}
	if in.RCONPassword == config.Redacted || in.RCONPassword == "" {
		if existing, found := a.cfg.Server(in.ID); found {
			in.RCONPassword = existing.RCONPassword
		}
	}
	if in.Host == "" {
		in.Host = "127.0.0.1"
	}
	if in.RCONPort == 0 {
		in.RCONPort = 27015
	}

	start := time.Now()
	client := a.rconFor(config.Server{ID: "test-" + in.ID, Host: in.Host, RCONPort: in.RCONPort, RCONPassword: in.RCONPassword})
	out, err := client.Exec("players")
	client.Close()
	a.dropRCON("test-" + in.ID)

	if err != nil {
		kind := classifyRCONError(err)
		writeJSON(w, map[string]any{
			"ok": false, "errorKind": kind,
			"error": friendlyRCONError(kind, in),
		})
		return
	}

	layout := pz.Detect(in.PZPath)
	writeJSON(w, map[string]any{
		"ok": true, "latencyMs": time.Since(start).Milliseconds(),
		"players": len(strings.Fields(out)), "response": truncate(out, 2000),
		"layout": layout, "layoutValid": layout.Valid(),
	})
}

func (a *App) handleServerDetail(w http.ResponseWriter, r *http.Request) {
	srv, found := a.cfg.Server(r.URL.Query().Get("id"))
	if !found {
		httpError(w, http.StatusNotFound, "no such server")
		return
	}
	layout := detectLayout(srv)
	report := a.scanner.Scan(layout)

	var configFiles []pz.ConfigFile
	if layout.ConfigDir != "" {
		configFiles, _ = pz.ListConfigFiles(layout.ConfigDir)
	}
	var logs []pz.LogFile
	if layout.LogsDir != "" {
		logs, _ = pz.ListLogs(layout.LogsDir)
	}

	settings := map[string]string{}
	if layout.ConfigDir != "" {
		if files, err := pz.ListConfigFiles(layout.ConfigDir); err == nil {
			for _, f := range files {
				if f.Kind != "ini" {
					continue
				}
				if ini, err := pz.LoadINI(layout.ConfigDir + "/" + f.Name); err == nil {
					for _, key := range []string{"PublicName", "MaxPlayers", "PVP", "Open", "Password",
						"DefaultPort", "PauseEmpty", "GlobalChat", "SafetySystem"} {
						if v, has := ini.Get(key); has {
							if key == "Password" {
								v = maskSecret(v)
							}
							settings[key] = v
						}
					}
					break
				}
			}
		}
	}

	writeJSON(w, map[string]any{
		"server":      config.Redact(config.Config{Servers: []config.Server{srv}}).Servers[0],
		"status":      a.statusOf(srv.ID),
		"layout":      layout,
		"mods":        report,
		"configFiles": configFiles,
		"logs":        logs,
		"settings":    settings,
		"backups":     a.backup.List(srv.ID),
		"players":     a.store.Players(srv.ID),
		"events":      a.store.Events(srv.ID, "", 100),
	})
}

func maskSecret(v string) string {
	if v == "" {
		return ""
	}
	return strings.Repeat("•", len(v))
}

// --- actions ----------------------------------------------------------------

func (a *App) handleAction(w http.ResponseWriter, r *http.Request) {
	var p struct {
		ServerID string   `json:"serverId"`
		Action   string   `json:"action"`
		Args     []string `json:"args"`
	}
	if !decodeJSON(w, r, &p) {
		return
	}
	srv, found := a.cfg.Server(p.ServerID)
	if !found {
		httpError(w, http.StatusNotFound, "no such server")
		return
	}
	cmd, found := LookupCommand(p.Action)
	if !found {
		httpError(w, http.StatusBadRequest, "unknown command "+strconv.Quote(p.Action))
		return
	}
	line, err := cmd.Build(p.Args)
	if err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}
	if line, err = a.resolveVerb(srv, cmd, line); err != nil {
		httpError(w, http.StatusConflict, err.Error())
		return
	}

	out, err := a.rconFor(srv).Exec(line)
	sev := store.SevInfo
	if cmd.Audit == "warn" {
		sev = store.SevWarn
	}
	// A password typed into "Create an account" must not end up in the
	// event log, which is exported and shown to every operator.
	logged, loggedArgs := line, any(p.Args)
	if masked := cmd.AuditLine(p.Args); masked != "" {
		logged, _ = a.resolveVerb(srv, cmd, masked)
		loggedArgs = "(hidden)"
	}
	detail := logged
	if err != nil {
		sev = store.SevError
		detail = logged + " — " + err.Error()
	}
	a.event(store.Event{
		Kind: "admin.action", Severity: sev, Source: source(r), Actor: actor(r),
		ServerID: srv.ID, Server: srv.Name,
		Message: cmd.Label, Detail: detail,
		Meta: map[string]any{"command": cmd.ID, "args": loggedArgs},
	})
	if err != nil {
		kind := classifyRCONError(err)
		httpError(w, http.StatusBadGateway, friendlyRCONError(kind, srv))
		return
	}

	// Keep the local ban hint in step with what was actually run.
	switch cmd.ID {
	case "banuser":
		a.store.SetBanned(srv.ID, arg(p.Args, 0), true)
	case "unbanuser":
		a.store.SetBanned(srv.ID, arg(p.Args, 0), false)
	}
	ok(w, map[string]any{"response": strings.TrimSpace(out), "command": logged})
}

func (a *App) handleConsole(w http.ResponseWriter, r *http.Request) {
	var p struct {
		ServerID string `json:"serverId"`
		Command  string `json:"command"`
	}
	if !decodeJSON(w, r, &p) {
		return
	}
	srv, found := a.cfg.Server(p.ServerID)
	if !found {
		httpError(w, http.StatusNotFound, "no such server")
		return
	}
	line, err := ValidateRawCommand(p.Command)
	if err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}

	out, execErr := a.rconFor(srv).Exec(line)
	sev := store.SevWarn
	detail := line
	if execErr != nil {
		sev = store.SevError
		detail = line + " — " + execErr.Error()
	}
	// Every console command is audited, without exception: this is the one
	// place an operator can do anything at all.
	a.event(store.Event{
		Kind: "admin.console", Severity: sev, Source: source(r), Actor: actor(r),
		ServerID: srv.ID, Server: srv.Name,
		Message: "Console: " + truncate(line, 120), Detail: detail,
	})
	if execErr != nil {
		httpError(w, http.StatusBadGateway, friendlyRCONError(classifyRCONError(execErr), srv))
		return
	}
	ok(w, map[string]any{"response": out})
}

// handleLifecycle covers restart, stop and start of the underlying container.
func (a *App) handleLifecycle(w http.ResponseWriter, r *http.Request) {
	var p struct {
		ServerID string `json:"serverId"`
		Action   string `json:"action"` // restart | stop | start | cancel-pending
		Reason   string `json:"reason"`
	}
	if !decodeJSON(w, r, &p) {
		return
	}
	srv, found := a.cfg.Server(p.ServerID)
	if !found {
		httpError(w, http.StatusNotFound, "no such server")
		return
	}
	// What the operator typed is told to players, so it is kept short and on
	// one line.
	note := strings.Join(strings.Fields(p.Reason), " ")
	if len([]rune(note)) > 200 {
		httpError(w, http.StatusBadRequest, "keep the reason under 200 characters")
		return
	}
	reason := "requested by " + actor(r)
	if note != "" {
		reason += ": " + note
	}
	ctx, cancel := context.WithTimeout(r.Context(), 4*time.Minute)
	defer cancel()

	switch p.Action {
	case "restart":
		if err := a.restartServer(ctx, srv, source(r), reason, note); err != nil {
			httpError(w, http.StatusBadGateway, err.Error())
			return
		}
		ok(w, map[string]any{"message": "Restart issued."})

	case "stop":
		if srv.DockerContainer == "" {
			httpError(w, http.StatusBadRequest, "no Docker container is configured for this server")
			return
		}
		if _, err := a.rconFor(srv).Exec("save"); err == nil {
			time.Sleep(2 * time.Second)
		}
		a.markRestarting(srv.ID, "stopping")
		// Hard stop through Arcane. restart: unless-stopped leaves a
		// deliberately stopped container down, which is the point.
		if err := a.arcane.Stop(ctx, srv.DockerContainer); err != nil {
			a.clearRestarting(srv.ID)
			httpError(w, http.StatusBadGateway, err.Error())
			return
		}
		// A stop is meant to stay stopped, so do not hold the restart state,
		// and mark it so the outage that follows is not reported.
		a.clearRestarting(srv.ID)
		// The container is down, so the game is too; waiting for the next
		// monitor tick to notice would leave it shown as running, and a
		// delete refused, for up to a minute.
		a.updateStatus(srv.ID, func(st *Status) {
			st.Stopped = true
			st.Online = false
			st.Players = nil
			st.PlayerCount = 0
			st.ContainerState = "exited"
		})
		a.dropRCON(srv.ID)
		a.event(store.Event{Kind: "server.stop", Severity: store.SevWarn, Source: source(r), Actor: actor(r),
			ServerID: srv.ID, Server: srv.Name, Message: srv.Name + " stopped", Detail: reason,
			Meta: map[string]any{"note": note}})
		ok(w, map[string]any{"message": "Stopped."})

	case "start":
		if srv.DockerContainer == "" {
			httpError(w, http.StatusBadRequest, "no Docker container is configured for this server")
			return
		}
		// Check what a start will actually mount. Start reuses the existing
		// container, whose mounts were fixed when it was created, so those
		// come from Arcane; the compose file only matters after a recreate.
		problems, drift, notCreated := a.startPreflight(ctx, srv)
		if notCreated {
			writeStatusJSON(w, http.StatusConflict, map[string]any{
				"error":   srv.Name + " has no container yet. Create it on the host with: " + recreateCommand(srv),
				"command": recreateCommand(srv),
			})
			return
		}
		if len(problems) > 0 {
			writeStatusJSON(w, http.StatusConflict, map[string]any{
				"error":    "Not starting " + srv.Name + ": " + strings.Join(problems, "; "),
				"problems": problems,
			})
			return
		}
		a.markRestarting(srv.ID, "starting")
		a.updateStatus(srv.ID, func(st *Status) { st.Stopped = false; st.restartSource = source(r) })
		if err := a.arcane.Start(ctx, srv.DockerContainer); err != nil {
			a.clearRestarting(srv.ID)
			httpError(w, http.StatusBadGateway, err.Error())
			return
		}
		detail := reason
		msg := "Started."
		if drift != "" {
			detail += ". Note: " + drift
			msg = "Started. Note: " + drift + "."
		}
		a.event(store.Event{Kind: "server.start", Severity: store.SevInfo, Source: source(r), Actor: actor(r),
			ServerID: srv.ID, Server: srv.Name, Message: srv.Name + " started", Detail: detail})
		ok(w, map[string]any{"message": msg, "drift": drift})

	case "cancel-pending":
		a.updateStatus(srv.ID, func(st *Status) {
			st.PendingRestartAt = NoTime
			st.PendingReason = ""
		})
		a.announce(srv, "The scheduled restart has been cancelled.")
		a.event(store.Event{Kind: "admin.action", Severity: store.SevInfo, Source: source(r), Actor: actor(r),
			ServerID: srv.ID, Server: srv.Name, Message: "Pending restart cancelled"})
		ok(w, map[string]any{"message": "Pending restart cancelled."})

	default:
		httpError(w, http.StatusBadRequest, "unknown action "+strconv.Quote(p.Action))
	}
}

// --- filesystem -------------------------------------------------------------

func (a *App) handleBrowse(w http.ResponseWriter, r *http.Request) {
	root := a.cfg.Get().PZRoot
	rel := r.URL.Query().Get("path")
	if rel == "" {
		rel = "."
	}
	abs, entries, isServer, err := pz.Browse(root, rel)
	if err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, map[string]any{
		"root": root, "path": rel, "absolute": abs,
		"entries": entries, "isServer": isServer,
		"layout": pz.Detect(abs),
	})
}

func (a *App) handleContainers(w http.ResponseWriter, r *http.Request) {
	available, err := a.arcane.Available()
	if !available {
		msg := "Arcane is not reachable. Container control is disabled."
		if err != nil {
			msg = "Arcane is unavailable: " + err.Error()
		}
		writeJSON(w, map[string]any{"available": false, "message": msg, "containers": []any{}})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	list, err := a.arcane.List(ctx)
	if err != nil {
		writeJSON(w, map[string]any{"available": false, "message": err.Error(), "containers": []any{}})
		return
	}
	writeJSON(w, map[string]any{"available": true, "containers": list})
}

func (a *App) handleConfigFiles(w http.ResponseWriter, r *http.Request) {
	srv, found := a.cfg.Server(r.URL.Query().Get("id"))
	if !found {
		httpError(w, http.StatusNotFound, "no such server")
		return
	}
	layout := detectLayout(srv)
	if layout.ConfigDir == "" {
		httpError(w, http.StatusNotFound, "no config directory was detected for this server")
		return
	}
	name := r.URL.Query().Get("file")
	if name == "" {
		files, err := pz.ListConfigFiles(layout.ConfigDir)
		if err != nil {
			httpError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, map[string]any{"files": files, "dir": layout.ConfigDir})
		return
	}
	content, err := pz.ReadConfigFile(layout.ConfigDir, name)
	if err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}
	if isServerINI(name) {
		content = maskINISecrets(content)
	}
	writeJSON(w, map[string]any{"file": name, "content": content, "dir": layout.ConfigDir})
}

// iniValues reads key=value lines, keyed case-insensitively as the game does.
func iniValues(text string) map[string]string {
	out := map[string]string{}
	for _, line := range strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		if i := strings.IndexByte(line, '='); i > 0 {
			out[strings.ToLower(strings.TrimSpace(line[:i]))] = line[i+1:]
		}
	}
	return out
}

func (a *App) handleConfigSave(w http.ResponseWriter, r *http.Request) {
	var p struct {
		ServerID string `json:"serverId"`
		File     string `json:"file"`
		Content  string `json:"content"`
		Reload   bool   `json:"reload"`
	}
	if !decodeJSON(w, r, &p) {
		return
	}
	srv, found := a.cfg.Server(p.ServerID)
	if !found {
		httpError(w, http.StatusNotFound, "no such server")
		return
	}
	layout := detectLayout(srv)
	if layout.ConfigDir == "" {
		httpError(w, http.StatusBadRequest, "no config directory was detected for this server")
		return
	}
	old := ""
	if b, err := os.ReadFile(filepath.Join(layout.ConfigDir, filepath.Base(p.File))); err == nil {
		old = string(b)
	}
	if isServerINI(p.File) {
		p.Content = unmaskINISecrets(p.Content, old)
	}
	if locked := a.lockedKeys(srv, p.File); len(locked) > 0 {
		before, after := iniValues(old), iniValues(p.Content)
		for key, reason := range locked {
			if before[strings.ToLower(key)] != after[strings.ToLower(key)] {
				httpError(w, http.StatusBadRequest, key+": "+reason)
				return
			}
		}
	}
	backupDir := a.backup.Dir(srv.ID) + "/config"
	backup, err := pz.WriteConfigFile(layout.ConfigDir, p.File, p.Content, backupDir)
	if err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}
	a.scanner.Invalidate(layout.Base)

	detail := "Saved " + p.File
	if backup != "" {
		detail += ". The previous version was kept."
	}
	reloaded := false
	if p.Reload {
		if _, err := a.rconFor(srv).Exec("reloadoptions"); err == nil {
			reloaded = true
			detail += " Options reloaded."
		} else {
			detail += " The server could not be asked to reload; restart it to apply."
		}
	}
	a.event(store.Event{Kind: "config.edit", Severity: store.SevWarn, Source: source(r), Actor: actor(r),
		ServerID: srv.ID, Server: srv.Name, Message: "Edited " + p.File, Detail: detail})
	ok(w, map[string]any{"backup": backup, "reloaded": reloaded, "message": detail})
}

func (a *App) handleLogs(w http.ResponseWriter, r *http.Request) {
	srv, found := a.cfg.Server(r.URL.Query().Get("id"))
	if !found {
		httpError(w, http.StatusNotFound, "no such server")
		return
	}
	layout := detectLayout(srv)

	if name := r.URL.Query().Get("file"); name != "" {
		if layout.LogsDir == "" {
			httpError(w, http.StatusNotFound, "no log directory was detected")
			return
		}
		lines, _ := strconv.Atoi(r.URL.Query().Get("lines"))
		content, err := pz.TailFile(layout.LogsDir, name, lines)
		if err != nil {
			httpError(w, http.StatusBadRequest, "could not read that log file")
			return
		}
		writeJSON(w, map[string]any{"file": name, "content": content})
		return
	}
	if r.URL.Query().Get("source") == "container" {
		if srv.DockerContainer == "" {
			httpError(w, http.StatusBadRequest, "no Docker container is configured for this server")
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		defer cancel()
		lines, _ := strconv.Atoi(r.URL.Query().Get("lines"))
		content, err := a.arcane.Logs(ctx, srv.DockerContainer, lines)
		if err != nil {
			httpError(w, http.StatusBadGateway, err.Error())
			return
		}
		writeJSON(w, map[string]any{"file": "container", "content": content})
		return
	}

	var files []pz.LogFile
	if layout.LogsDir != "" {
		files, _ = pz.ListLogs(layout.LogsDir)
	}
	writeJSON(w, map[string]any{"files": files, "dir": layout.LogsDir,
		"hasContainer": srv.DockerContainer != ""})
}

func (a *App) handleMods(w http.ResponseWriter, r *http.Request) {
	srv, found := a.cfg.Server(r.URL.Query().Get("id"))
	if !found {
		httpError(w, http.StatusNotFound, "no such server")
		return
	}
	layout := detectLayout(srv)
	if r.URL.Query().Get("refresh") == "1" {
		a.scanner.Invalidate(layout.Base)
	}
	writeJSON(w, map[string]any{"mods": a.scanner.Scan(layout), "layout": layout})
}

// --- backups ----------------------------------------------------------------

func (a *App) handleBackupList(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("serverId")
	if _, found := a.cfg.Server(id); !found {
		httpError(w, http.StatusNotFound, "no such server")
		return
	}
	list := a.backup.List(id)
	var total int64
	for _, ar := range list {
		total += ar.Size
	}
	writeJSON(w, map[string]any{"backups": list, "totalBytes": total, "running": a.backup.Running(id)})
}

func (a *App) handleBackupCreate(w http.ResponseWriter, r *http.Request) {
	var p struct {
		ServerID string `json:"serverId"`
		Note     string `json:"note"`
	}
	if !decodeJSON(w, r, &p) {
		return
	}
	srv, found := a.cfg.Server(p.ServerID)
	if !found {
		httpError(w, http.StatusNotFound, "no such server")
		return
	}
	layout := detectLayout(srv)
	if !layout.Valid() && layout.SavesDir == "" {
		httpError(w, http.StatusBadRequest, "PZAdmin could not find this server's files on disk")
		return
	}
	keep := srv.Backup.Keep
	if keep <= 0 {
		keep = 10
	}

	// Flush the world first so the archive is as consistent as it can be
	// without stopping the server.
	saved := false
	if _, err := a.rconFor(srv).Exec("save"); err == nil {
		saved = true
		time.Sleep(2 * time.Second)
	}

	res, err := a.backup.Create(srv.ID, layout, srv.Backup.IncludeConfig, keep, strings.TrimSpace(p.Note))
	if err != nil {
		a.event(store.Event{Kind: "backup.failed", Severity: store.SevError, Source: source(r), Actor: actor(r),
			ServerID: srv.ID, Server: srv.Name, Message: "Backup failed", Detail: err.Error()})
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	a.event(store.Event{Kind: "backup.done", Severity: store.SevSuccess, Source: source(r), Actor: actor(r),
		ServerID: srv.ID, Server: srv.Name,
		Message: fmt.Sprintf("Backed up %s (%s)", srv.Name, humanBytes(res.Archive.Size)),
		Detail:  fmt.Sprintf("%d files in %s.", res.Archive.Files, res.Duration.Round(time.Second))})
	ok(w, map[string]any{"archive": res.Archive, "pruned": res.Pruned, "worldSaved": saved,
		"durationMs": res.Duration.Milliseconds()})
}

func (a *App) handleBackupVerify(w http.ResponseWriter, r *http.Request) {
	var p struct {
		ServerID string `json:"serverId"`
		Name     string `json:"name"`
	}
	if !decodeJSON(w, r, &p) {
		return
	}
	files, bytes, err := a.backup.Verify(p.ServerID, p.Name)
	if err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}
	ok(w, map[string]any{"files": files, "bytes": bytes,
		"message": fmt.Sprintf("Archive is readable: %d files, %s uncompressed.", files, humanBytes(bytes))})
}

func (a *App) handleBackupRestore(w http.ResponseWriter, r *http.Request) {
	var p struct {
		ServerID string `json:"serverId"`
		Name     string `json:"name"`
		Confirm  string `json:"confirm"`
	}
	if !decodeJSON(w, r, &p) {
		return
	}
	srv, found := a.cfg.Server(p.ServerID)
	if !found {
		httpError(w, http.StatusNotFound, "no such server")
		return
	}
	if p.Confirm != srv.Name {
		httpError(w, http.StatusBadRequest, "type the server name exactly to confirm the restore")
		return
	}
	// Restoring over a running world corrupts it, so this is a hard refusal
	// rather than a warning.
	if st := a.statusOf(srv.ID); st.Online {
		httpError(w, http.StatusConflict, "stop the server before restoring a backup")
		return
	}

	n, err := a.backup.Restore(srv.ID, p.Name, detectLayout(srv))
	if err != nil {
		a.event(store.Event{Kind: "backup.failed", Severity: store.SevError, Source: source(r), Actor: actor(r),
			ServerID: srv.ID, Server: srv.Name, Message: "Restore failed", Detail: err.Error()})
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	a.scanner.Invalidate(srv.PZPath)
	a.event(store.Event{Kind: "backup.restore", Severity: store.SevWarn, Source: source(r), Actor: actor(r),
		ServerID: srv.ID, Server: srv.Name,
		Message: "Restored " + srv.Name + " from " + p.Name,
		Detail:  fmt.Sprintf("%d files written.", n)})
	ok(w, map[string]any{"files": n, "message": fmt.Sprintf("Restored %d files. Start the server when you are ready.", n)})
}

func (a *App) handleBackupDelete(w http.ResponseWriter, r *http.Request) {
	var p struct {
		ServerID string `json:"serverId"`
		Name     string `json:"name"`
	}
	if !decodeJSON(w, r, &p) {
		return
	}
	if err := a.backup.Delete(p.ServerID, p.Name); err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}
	a.event(store.Event{Kind: "admin.action", Severity: store.SevWarn, Source: source(r), Actor: actor(r),
		ServerID: p.ServerID, Message: "Deleted backup " + p.Name})
	ok(w, nil)
}

func (a *App) handleBackupDownload(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("serverId")
	name := r.URL.Query().Get("name")
	f, info, err := a.backup.Open(id, name)
	if err != nil {
		httpError(w, http.StatusNotFound, "no such archive")
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", "application/gzip")
	w.Header().Set("Content-Disposition", "attachment; filename="+strconv.Quote(name))
	http.ServeContent(w, r, name, info.ModTime(), f)
}

// --- schedules --------------------------------------------------------------

func (a *App) handleSchedules(w http.ResponseWriter, r *http.Request) {
	var p struct {
		Tasks []config.Task `json:"tasks"`
	}
	if !decodeJSON(w, r, &p) {
		return
	}
	if err := validateTasks(a.cfg.Get(), p.Tasks); err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}

	if _, err := a.cfg.Update(func(c *config.Config) error {
		// Preserve last-run bookkeeping across an edit.
		prev := map[string]config.Task{}
		for _, t := range c.Schedules {
			prev[t.ID] = t
		}
		for i := range p.Tasks {
			if old, found := prev[p.Tasks[i].ID]; found {
				p.Tasks[i].LastRun = old.LastRun
				p.Tasks[i].LastResult = old.LastResult
			}
		}
		c.Schedules = p.Tasks
		return nil
	}); err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	a.event(store.Event{Kind: "admin.action", Severity: store.SevInfo, Source: source(r), Actor: actor(r),
		Message: "Schedules updated", Detail: fmt.Sprintf("%d job(s) configured", len(p.Tasks))})
	ok(w, nil)
}

// validateTasks checks and tidies a list of scheduled jobs in place: names
// trimmed, legacy single actions folded into steps, every step checked, and
// IDs made unique. It is shared by the schedule editor and import, so a job
// that could not be saved by hand cannot arrive in a file either.
func validateTasks(cfg config.Config, tasks []config.Task) error {
	seen := map[string]bool{}
	for i := range tasks {
		if err := validateTask(cfg, &tasks[i]); err != nil {
			return err
		}
		t := &tasks[i]
		if t.ID == "" || seen[t.ID] {
			t.ID = config.RandomToken(8)
		}
		seen[t.ID] = true
	}
	return nil
}

// validateTask checks one job. Its errors name the job, so they can be shown
// as they are.
func validateTask(cfg config.Config, t *config.Task) error {
	t.Name = strings.TrimSpace(t.Name)
	if t.Name == "" {
		return errors.New("every job needs a name")
	}
	if err := cronx.Valid(t.Cron); err != nil {
		return fmt.Errorf("%q: %s", t.Name, err.Error())
	}
	if _, found := serverByID(cfg.Servers, t.ServerID); !found {
		return fmt.Errorf("%q points at a server that no longer exists", t.Name)
	}
	// Fold a legacy single-action task sent by an older client.
	if len(t.Steps) == 0 && t.Kind != "" {
		t.Steps = []config.Step{{Kind: t.Kind, Message: t.Message, Command: t.Command}}
	}
	t.Kind, t.Message, t.Command = "", "", ""
	if len(t.Steps) == 0 {
		return fmt.Errorf("%q has no steps: add at least one thing for it to do", t.Name)
	}
	if len(t.Steps) > 25 {
		return fmt.Errorf("%q has too many steps", t.Name)
	}
	for n := range t.Steps {
		err := validateStep(&t.Steps[n])
		if err == nil && t.Steps[n].Kind == "discord" && webhookByID(cfg, t.Steps[n].Webhook) == nil {
			err = errors.New("the Discord channel it posts to has been removed")
		}
		if err != nil {
			return fmt.Errorf("%q step %d: %s", t.Name, n+1, err.Error())
		}
	}
	return nil
}

func (a *App) handleScheduleRun(w http.ResponseWriter, r *http.Request) {
	var p struct {
		ID string `json:"id"`
	}
	if !decodeJSON(w, r, &p) {
		return
	}
	cfg := a.cfg.Get()
	for _, t := range cfg.Schedules {
		if t.ID != p.ID {
			continue
		}
		srv, found := serverByID(cfg.Servers, t.ServerID)
		if !found {
			httpError(w, http.StatusBadRequest, "that task points at a server that no longer exists")
			return
		}
		a.event(store.Event{Kind: "admin.action", Severity: store.SevInfo, Source: source(r), Actor: actor(r),
			ServerID: srv.ID, Server: srv.Name, Message: "Ran task " + t.Name + " manually"})
		if !a.spawn(func() { a.runTask(t, srv) }) {
			httpError(w, http.StatusServiceUnavailable, "PZAdmin is shutting down")
			return
		}
		ok(w, map[string]any{"message": "Running " + t.Name + " now."})
		return
	}
	httpError(w, http.StatusNotFound, "no such task")
}

// --- settings ---------------------------------------------------------------

func (a *App) handleSettings(w http.ResponseWriter, r *http.Request) {
	var p struct {
		PZRoot    string            `json:"pzRoot"`
		Timezone  string            `json:"timezone"`
		Notify    *config.Notify    `json:"notify"`
		Metrics   *config.Metrics   `json:"metrics"`
		Interface *config.Interface `json:"interface"`
	}
	if !decodeJSON(w, r, &p) {
		return
	}

	if p.Timezone != "" {
		if _, err := time.LoadLocation(p.Timezone); err != nil {
			httpError(w, http.StatusBadRequest,
				"unknown timezone "+strconv.Quote(p.Timezone)+". Use an IANA name such as Europe/London.")
			return
		}
	}
	// The data and stacks folders are fixed by PZAdmin's compose file and
	// set from its environment at startup, so they are not settings.
	p.PZRoot = ""

	prev := a.cfg.Get()
	updated, err := a.cfg.Update(func(c *config.Config) error {
		if p.PZRoot != "" {
			c.PZRoot = p.PZRoot
		}
		if p.Timezone != "" {
			c.Timezone = p.Timezone
		}
		if p.Notify != nil {
			n := *p.Notify
			// A form that only changes the throttle leaves the webhooks and
			// identity alone.
			if n.Webhooks == nil {
				n.Webhooks = prev.Notify.Webhooks
			}
			if n.Identity == (config.Identity{}) && p.Notify.Webhooks == nil {
				n.Identity = prev.Notify.Identity
			}
			// The bot is connected and removed through its own endpoints,
			// which check the token with Discord; the settings form only
			// ever carries it redacted.
			n.Bot = prev.Notify.Bot
			hooks, err := validateWebhooks(n.Webhooks, prev.Notify.Webhooks, c.Servers, n.Bot.Token != "")
			if err != nil {
				return err
			}
			if err := webhooksInUse(hooks, c.Schedules); err != nil {
				return err
			}
			n.Webhooks = hooks
			identity, err := cleanIdentity(n.Identity, "PZAdmin")
			if err != nil {
				return err
			}
			n.Identity = identity
			// Forms that do not show quiet hours leave them as they were.
			if n.QuietHours == (config.QuietHours{}) {
				n.QuietHours = prev.Notify.QuietHours
			}
			quiet, err := cleanQuietHours(n.QuietHours)
			if err != nil {
				return err
			}
			n.QuietHours = quiet
			n.Enabled, n.WebhookURL, n.Events = false, "", nil
			c.Notify = n
		}
		if p.Metrics != nil {
			m := *p.Metrics
			if m.Token == config.Redacted || m.Token == "" {
				m.Token = prev.Metrics.Token
			}
			c.Metrics = m
		}
		if p.Interface != nil {
			c.Interface = *p.Interface
		}
		return nil
	})
	var invalid *settingsError
	if errors.As(err, &invalid) {
		httpError(w, http.StatusBadRequest, invalid.Error())
		return
	}
	if err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	a.applyNotifyConfig(updated)
	a.scripts.Invalidate()
	a.event(store.Event{Kind: "admin.action", Severity: store.SevInfo, Source: source(r), Actor: actor(r),
		Message: "Settings updated"})
	writeJSON(w, map[string]any{"ok": true, "config": config.Redact(updated)})
}

func (a *App) handleNotifyTest(w http.ResponseWriter, r *http.Request) {
	var p struct {
		// ID names a saved webhook, whose address is used when URL is blank
		// or the placeholder the browser was given instead of it.
		ID       string `json:"id"`
		URL      string `json:"url"`
		Audience string `json:"audience"`
		// Server and Identity are the channel as it stands in the form, so
		// the test shows the name and picture real messages will have.
		Server   string          `json:"server"`
		Identity config.Identity `json:"identity"`
		// ChannelID tests posting through the bot.
		ChannelID string `json:"channelId"`
	}
	if !decodeJSON(w, r, &p) {
		return
	}
	cfg := a.cfg.Get()
	if h := webhookByID(cfg, p.ID); h != nil && p.Server == "" && p.Identity == (config.Identity{}) {
		p.Server, p.Identity = h.Server, h.Identity
	}
	name, avatar := postedAs(cfg, config.Webhook{Server: p.Server, Identity: p.Identity})
	dest := notify.Destination{URL: strings.TrimSpace(p.URL), Audience: p.Audience, Username: name, AvatarURL: avatar}
	if id := strings.TrimSpace(p.ChannelID); id != "" {
		if cfg.Notify.Bot.Token == "" {
			httpError(w, http.StatusBadRequest, "connect the bot first")
			return
		}
		t := a.targetFor(cfg, config.Webhook{ChannelID: id})
		dest.URL, dest.ChannelID, dest.BotToken, dest.API = "", t.ChannelID, t.BotToken, t.API
		if err := a.notify.Test(dest); err != nil {
			httpError(w, http.StatusBadGateway, "the bot could not post there: "+err.Error())
			return
		}
		ok(w, map[string]any{"message": "Test message sent."})
		return
	}
	if dest.URL == "" || dest.URL == config.Redacted {
		dest.URL = ""
		for _, h := range cfg.Notify.Webhooks {
			if p.ID != "" && h.ID == p.ID {
				dest.URL = h.URL
			}
		}
	}
	if dest.URL == "" {
		httpError(w, http.StatusBadRequest, "enter a webhook address first")
		return
	}
	if err := a.notify.Test(dest); err != nil {
		httpError(w, http.StatusBadGateway, "the webhook did not accept the message: "+err.Error())
		return
	}
	ok(w, map[string]any{"message": "Test notification sent."})
}

func (a *App) handleExport(w http.ResponseWriter, r *http.Request) {
	cfg := config.Export(a.cfg.Get())
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Disposition", `attachment; filename="pzadmin-config.json"`)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(cfg)
}

func (a *App) handleImport(w http.ResponseWriter, r *http.Request) {
	var incoming config.Config
	defer r.Body.Close()
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<20))
	if err := dec.Decode(&incoming); err != nil {
		httpError(w, http.StatusBadRequest, "that does not look like a PZAdmin export: "+err.Error())
		return
	}
	if len(incoming.Servers) == 0 && len(incoming.Schedules) == 0 {
		httpError(w, http.StatusBadRequest, "the file contains no servers or schedules")
		return
	}
	tz := strings.TrimSpace(incoming.Timezone)
	if tz != "" {
		if _, err := time.LoadLocation(tz); err != nil {
			httpError(w, http.StatusBadRequest,
				"the file's timezone "+strconv.Quote(tz)+" is not one this system knows, so nothing was imported")
			return
		}
	}

	// Servers come from their stack folders, so an import can only carry
	// the operator's own settings onto servers that exist here, matched by
	// stack folder name. Anything the files own is ignored: taking a
	// container name from an uploaded file would put an arbitrary container
	// on the allowlist.
	var (
		matched         int
		skippedServers  []string
		importedJobs    int
		skippedJobs     []string
		schedulesInFile = len(incoming.Schedules) > 0
	)
	_, err := a.cfg.Update(func(c *config.Config) error {
		matched, importedJobs = 0, 0
		skippedServers, skippedJobs = nil, nil
		idMap := map[string]string{}
		for _, in := range incoming.Servers {
			found := false
			for i := range c.Servers {
				cur := &c.Servers[i]
				if (in.Stack != "" && cur.Stack == in.Stack) || (in.Stack == "" && cur.ID == in.ID) {
					cur.Name = firstNonBlank(strings.TrimSpace(in.Name), cur.Name)
					cur.Enabled = in.Enabled
					cur.Notes = in.Notes
					cur.SortHint = in.SortHint
					if in.Recovery.FailuresBeforeRestart > 0 {
						cur.Recovery = in.Recovery
					}
					cur.Mods = in.Mods
					if public, err := cleanPublicInfo(in.Public); err == nil {
						cur.Public = public
					}
					if in.Backup.Keep > 0 {
						cur.Backup = in.Backup
					}
					idMap[in.ID] = cur.ID
					found = true
					matched++
					break
				}
			}
			if !found {
				skippedServers = append(skippedServers, firstNonBlank(in.Name, in.Stack, in.ID))
			}
		}
		if schedulesInFile {
			// The file's schedules replace the current ones, as they do when
			// the schedule editor saves. Each job is checked exactly as the
			// editor checks it; one that would be refused there is left out
			// and named in the reply rather than failing the whole import.
			kept := []config.Task{}
			seen := map[string]bool{}
			for _, sc := range incoming.Schedules {
				id, ok := idMap[sc.ServerID]
				if !ok {
					skippedJobs = append(skippedJobs, fmt.Sprintf("%q: its server is not on this PZAdmin", strings.TrimSpace(sc.Name)))
					continue
				}
				sc.ServerID = id
				sc.LastRun, sc.LastResult = "", ""
				if err := validateTask(*c, &sc); err != nil {
					skippedJobs = append(skippedJobs, err.Error())
					continue
				}
				if sc.ID == "" || seen[sc.ID] {
					sc.ID = config.RandomToken(8)
				}
				seen[sc.ID] = true
				kept = append(kept, sc)
			}
			c.Schedules = kept
			importedJobs = len(kept)
		}
		if tz != "" {
			c.Timezone = tz
		}
		return nil
	})
	if err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	a.syncMonitors()

	detail := fmt.Sprintf("Settings applied to %d server(s).", matched)
	if len(skippedServers) > 0 {
		detail += fmt.Sprintf(" %d had no matching stack folder here and were skipped: %s.",
			len(skippedServers), strings.Join(skippedServers, ", "))
	}
	if schedulesInFile {
		detail += fmt.Sprintf(" Schedules replaced with %d job(s) from the file.", importedJobs)
		if len(skippedJobs) > 0 {
			detail += fmt.Sprintf(" %d job(s) left out: %s.", len(skippedJobs), strings.Join(skippedJobs, "; "))
		}
	}
	a.event(store.Event{Kind: "admin.action", Severity: store.SevWarn, Source: source(r), Actor: actor(r),
		Message: "Configuration imported", Detail: detail})
	ok(w, map[string]any{
		"servers":           matched,
		"skipped":           len(skippedServers),
		"skippedServers":    nonNil(skippedServers),
		"schedulesReplaced": schedulesInFile,
		"schedules":         importedJobs,
		"skippedSchedules":  nonNil(skippedJobs),
		"timezone":          tz,
		"summary":           detail,
	})
}

// nonNil keeps an empty list as [] rather than null in JSON.
func nonNil(v []string) []string {
	if v == nil {
		return []string{}
	}
	return v
}

func firstNonBlank(v ...string) string {
	for _, s := range v {
		if strings.TrimSpace(s) != "" {
			return s
		}
	}
	return ""
}

// validateStep checks one step of a scheduled job, so a broken job is rejected
// at save time rather than failing silently at four in the morning.
func validateStep(step *config.Step) error {
	if step.Args == nil {
		step.Args = []string{}
	}
	switch step.Kind {
	case "save", "restart", "backup":
		return nil
	case "wait":
		if step.Seconds < 1 || step.Seconds > 3600 {
			return fmt.Errorf("a wait must be between 1 second and 1 hour")
		}
		return nil
	case "broadcast":
		if strings.TrimSpace(step.Message) == "" {
			return fmt.Errorf("a broadcast needs a message")
		}
		return nil
	case "discord":
		step.Message = strings.TrimSpace(step.Message)
		if step.Message == "" {
			return fmt.Errorf("a Discord post needs a message")
		}
		if len([]rune(step.Message)) > maxTemplate {
			return fmt.Errorf("a Discord post must be under %d characters", maxTemplate)
		}
		if step.Webhook == "" {
			return fmt.Errorf("choose which Discord channel to post to")
		}
		return nil
	case "command":
		_, err := ValidateRawCommand(step.Command)
		return err
	case "action":
		cmd, found := LookupCommand(step.Action)
		if !found {
			return fmt.Errorf("unknown command %q", step.Action)
		}
		// Check the arguments build, substituting a stand-in for anything that
		// is only resolved when the job actually runs.
		probe := append([]string(nil), step.Args...)
		for len(probe) < len(cmd.Params) {
			probe = append(probe, "")
		}
		for i, param := range cmd.Params {
			value := strings.TrimSpace(probe[i])
			switch {
			case param.Type == ParamPlayer && strings.HasPrefix(value, "@"):
				if value != "@each" && value != "@random" {
					return fmt.Errorf("%s must be a player name, @each or @random", param.Label)
				}
				probe[i] = "placeholder"
			case strings.HasPrefix(strings.ToLower(value), "random:"):
				if param.Type != ParamItem && param.Type != ParamVehicle && param.Type != ParamPerk {
					return fmt.Errorf("%s cannot be randomised", param.Label)
				}
				// The stand-in has to look like the real thing: a skill name
				// is bare, an item or vehicle is Module.Name.
				if param.Type == ParamPerk {
					probe[i] = "Placeholder"
				} else {
					probe[i] = "Base.Placeholder"
				}
			}
		}
		if _, err := cmd.Build(probe); err != nil {
			return err
		}
		return nil
	}
	return fmt.Errorf("unknown step type %q", step.Kind)
}
