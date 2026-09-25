package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/TheWarBoys2/pzadmin/internal/arcane"
	"github.com/TheWarBoys2/pzadmin/internal/config"
	"github.com/TheWarBoys2/pzadmin/internal/notify"
	"github.com/TheWarBoys2/pzadmin/internal/pz"
	"github.com/TheWarBoys2/pzadmin/internal/rcon"
	"github.com/TheWarBoys2/pzadmin/internal/stacks"
	"github.com/TheWarBoys2/pzadmin/internal/store"
)

// Version is set at build time with -ldflags "-X ...server.Version=1.0.0".
// A plain go build or go run is a development build and says so, rather than
// claiming to be a release it may not match.
var Version = "dev"

// Status is the live state of one server, as shown on the dashboard.
type Status struct {
	ServerID string `json:"serverId"`
	Name     string `json:"name"`
	Enabled  bool   `json:"enabled"`

	Online bool `json:"online"`
	// Restarting is set while PZAdmin has deliberately taken the server down.
	// Without it a restart is indistinguishable from an outage: the board flips
	// to a red "Offline", a down alert fires, and the watchdog can pile a
	// second restart on top of the one already in progress.
	Restarting      bool         `json:"restarting"`
	RestartingSince OptionalTime `json:"restartingSince"`
	RestartReason   string       `json:"restartReason,omitempty"`
	// restartWindow overrides how long Restarting may last; zero is the
	// default window.
	restartWindow time.Duration

	// Deploying is set while Arcane runs docker compose up for the server.
	Deploying bool `json:"deploying,omitempty"`

	// Stopped is set when PZAdmin hard-stopped the server on purpose, so
	// the outage that follows is not reported as one.
	Stopped bool `json:"stopped,omitempty"`

	Players     []string     `json:"players"`
	PlayerCount int          `json:"playerCount"`
	MaxPlayers  int          `json:"maxPlayers,omitempty"`
	LatencyMS   int64        `json:"latencyMs"`
	LastCheck   OptionalTime `json:"lastCheck"`
	LastOnline  OptionalTime `json:"lastOnline"`
	Error       string       `json:"error,omitempty"`
	// ErrorKind lets the UI say something useful instead of showing a raw
	// Go error: "auth", "refused", "timeout" or "other".
	ErrorKind string `json:"errorKind,omitempty"`

	Container       string       `json:"container,omitempty"`
	ContainerState  string       `json:"containerState,omitempty"`
	ContainerUptime int64        `json:"containerUptimeSec,omitempty"`
	StartedAt       OptionalTime `json:"startedAt"`
	RestartCount    int          `json:"restartCount,omitempty"`

	Layout        pz.Layout    `json:"layout"`
	ModsEnabled   int          `json:"modsEnabled"`
	ModsMissing   []string     `json:"modsMissing,omitempty"`
	ModUpdateSeen OptionalTime `json:"modUpdateSeen"`

	SavesBytes    int64        `json:"savesBytes,omitempty"`
	BackupCount   int          `json:"backupCount"`
	BackupBytes   int64        `json:"backupBytes"`
	BackupRunning bool         `json:"backupRunning"`
	LastBackup    OptionalTime `json:"lastBackup"`

	// Recovery bookkeeping, surfaced so the operator can see the watchdog work.
	ConsecutiveFailures int          `json:"consecutiveFailures"`
	RecoveryAttempts    int          `json:"recoveryAttempts"`
	LastRecovery        OptionalTime `json:"lastRecovery"`
	PendingRestartAt    OptionalTime `json:"pendingRestartAt"`
	PendingReason       string       `json:"pendingReason,omitempty"`
}

// App holds every long-lived component.
type App struct {
	cfg     *config.Store
	store   *store.Store
	arcane  *arcane.Client
	notify  *notify.Notifier
	backup  *pz.Backupper
	scanner *pz.Scanner
	scripts *pz.ScriptScanner
	custom  *customCatalogue
	// steamBase is overridden by tests; empty means the public Steam API.
	steamBase string
	// discordAPI and discordGateway are overridden by tests; empty means
	// Discord itself.
	discordAPI     string
	discordGateway string
	// gameRoot is a Project Zomboid installation shared by every server, used
	// only when a server has no installation of its own. It comes from
	// PZADMIN_GAME_ROOT.
	gameRoot string
	// gameImage is the pinned image every new stack is written with.
	gameImage string
	// rconHost is where RCON ports published on the Docker host are reached
	// from inside PZAdmin's container.
	rconHost string
	// visible reports whether a host path is inside a folder mounted into
	// PZAdmin at the same path.
	visible stacks.Visible

	discMu   sync.Mutex
	lastScan stacks.Result
	sess     *sessionStore
	keys     *apiKeyStore
	limiter  *loginLimiter
	assets   fs.FS
	dataDir  string

	// apiLimit, apiFail, apiInflight and apiStreams throttle /api/v1; see
	// apiv1.go.
	apiLimit    *rateLimiter
	apiFail     *loginLimiter
	apiInflight sync.Map
	apiStreams  sync.Map
	apiStats    apiCounters

	mu       sync.RWMutex
	status   map[string]*Status
	clients  map[string]*rcon.Client
	tailers  map[string]*pz.Tailer
	monitors map[string]context.CancelFunc

	// caps is what each server's own help says it has; see capabilities.go.
	capMu    sync.Mutex
	caps     map[string]*Capabilities
	capTried map[string]time.Time

	stop chan struct{}
	wg   sync.WaitGroup
	once sync.Once
}

// Options configures a new App.
type Options struct {
	DataDir string
	Assets  fs.FS
	// SteamBase overrides the Steam Web API address, for tests.
	SteamBase string
	// GameRoot is a shared Project Zomboid installation, from PZADMIN_GAME_ROOT.
	GameRoot string

	// DataRoot and StacksRoot are the two folders mounted into PZAdmin at the
	// same path as on the host. Empty keeps whatever the config holds.
	DataRoot   string
	StacksRoot string
	// RCONHost reaches ports published on the Docker host. Default
	// host.docker.internal, which the compose file maps to the host gateway.
	RCONHost string

	// GameImage is the pinned image new stacks use, from PZADMIN_GAME_IMAGE.
	GameImage string

	ArcaneURL    string
	ArcaneEnvID  string
	ArcaneAPIKey string
	// ArcaneHTTP overrides the transport, for tests.
	ArcaneHTTP *http.Client
}

// New builds the application.
func New(opts Options) (*App, error) {
	cfgStore, err := config.Open(filepath.Join(opts.DataDir, "config.json"))
	if err != nil {
		return nil, err
	}
	cfg := cfgStore.Get()

	st, err := store.Open(filepath.Join(opts.DataDir, "history"), cfg.Interface.RetainDays)
	if err != nil {
		return nil, err
	}

	a := &App{
		cfg:       cfgStore,
		store:     st,
		notify:    notify.New(),
		backup:    pz.NewBackupper(filepath.Join(opts.DataDir, "backups")),
		scanner:   pz.NewScanner(2 * time.Minute),
		scripts:   pz.NewScriptScanner(30 * time.Minute),
		custom:    loadCustomCatalogue(filepath.Join(opts.DataDir, "catalogue.json")),
		sess:      newSessionStore(filepath.Join(opts.DataDir, "sessions.json")),
		keys:      newAPIKeyStore(filepath.Join(opts.DataDir, "apikeys.json")),
		apiLimit:  newRateLimiter(),
		apiFail:   newLoginLimiter(),
		limiter:   newLoginLimiter(),
		assets:    opts.Assets,
		dataDir:   opts.DataDir,
		status:    map[string]*Status{},
		clients:   map[string]*rcon.Client{},
		tailers:   map[string]*pz.Tailer{},
		monitors:  map[string]context.CancelFunc{},
		caps:      map[string]*Capabilities{},
		capTried:  map[string]time.Time{},
		stop:      make(chan struct{}),
		steamBase: opts.SteamBase,
		gameRoot:  strings.TrimSpace(opts.GameRoot),
		rconHost:  strings.TrimSpace(opts.RCONHost),
		gameImage: strings.TrimSpace(opts.GameImage),
	}
	if a.rconHost == "" {
		a.rconHost = "host.docker.internal"
	}

	// The mounted folders are fixed by the compose file, so the environment
	// is the authority; the stored config just follows it.
	if opts.DataRoot != "" || opts.StacksRoot != "" {
		if _, err := cfgStore.Update(func(c *config.Config) error {
			if opts.DataRoot != "" {
				c.PZRoot = filepath.Clean(opts.DataRoot)
			}
			if opts.StacksRoot != "" {
				c.StacksRoot = filepath.Clean(opts.StacksRoot)
			}
			return nil
		}); err != nil {
			return nil, err
		}
		cfg = cfgStore.Get()
	}
	a.visible = stacks.UnderAny(cfg.PZRoot, cfg.StacksRoot)

	a.arcane = arcane.New(arcane.Options{
		URL:           opts.ArcaneURL,
		EnvironmentID: opts.ArcaneEnvID,
		APIKey:        opts.ArcaneAPIKey,
		Allow:         cfgStore.ContainerAllowed,
		AllowProject:  a.allowProject,
		HTTP:          opts.ArcaneHTTP,
	})
	a.applyNotifyConfig(cfg)
	return a, nil
}

// Start launches the background workers.
func (a *App) Start() {
	a.rescan()
	a.syncMonitors()
	a.wg.Add(5)
	go a.discoveryLoop()
	go a.schedulerLoop()
	go a.housekeepingLoop()
	go a.monitorSupervisor()
	go a.liveStatusLoop()

	a.event(store.Event{
		Kind: "system.start", Severity: store.SevInfo, Source: "system",
		Message: "PZAdmin " + Version + " started",
	})
}

// Close stops everything and flushes state to disk.
func (a *App) Close() {
	a.once.Do(func() {
		close(a.stop)
		a.mu.Lock()
		for _, cancel := range a.monitors {
			cancel()
		}
		a.monitors = map[string]context.CancelFunc{}
		for _, c := range a.clients {
			c.Close()
		}
		a.clients = map[string]*rcon.Client{}
		a.mu.Unlock()

		a.wg.Wait()
		// Close out any live sessions so playtime is not credited while
		// PZAdmin is not running to observe it.
		for _, s := range a.cfg.Get().Servers {
			a.store.MarkOffline(s.ID)
		}
		a.event(store.Event{Kind: "system.stop", Source: "system", Message: "PZAdmin stopped"})
		a.store.Flush()
		a.keys.flush()
		a.notify.Close()
	})
}

func (a *App) applyNotifyConfig(cfg config.Config) {
	var dests []notify.Destination
	for _, w := range cfg.Notify.Webhooks {
		t := a.targetFor(cfg, w)
		if !w.Enabled || !t.Valid() {
			continue
		}
		name, avatar := postedAs(cfg, w)
		dests = append(dests, notify.Destination{
			ID: w.ID, URL: t.Webhook, ChannelID: t.ChannelID, BotToken: t.BotToken, API: t.API, Audience: w.Audience,
			Events: w.Events, Servers: w.Servers, Messages: w.Messages,
			Username: name, AvatarURL: avatar,
		})
	}
	a.notify.Configure(notify.Config{
		Destinations: dests,
		MinInterval:  time.Duration(cfg.Notify.MinIntervalSeconds) * time.Second,
	})
}

// event records an event and mirrors it to the webhooks.
func (a *App) event(e store.Event) {
	a.store.Append(e)
	sev := string(e.Severity)
	if sev == "" {
		sev = "info"
	}
	m := notify.Message{
		Kind: e.Kind, Title: e.Message, Body: e.Detail,
		Server: e.Server, ServerID: e.ServerID, Severity: sev, At: e.At,
	}
	if reason, ok := e.Meta["reason"].(string); ok {
		m.Reason = reason
	}
	if note, ok := e.Meta["note"].(string); ok {
		m.Note = note
	}
	if minutes, ok := e.Meta["restartIn"].(int); ok {
		m.Minutes = minutes
	}
	a.notify.Send(m)
}

// rconFor returns the pooled RCON client for a server, rebuilding it if the
// connection details changed.
func (a *App) rconFor(s config.Server) *rcon.Client {
	key := fmt.Sprintf("%s|%s|%d|%s", s.ID, s.Host, s.RCONPort, s.RCONPassword)
	a.mu.Lock()
	defer a.mu.Unlock()
	if c, ok := a.clients[key]; ok {
		return c
	}
	// Drop any stale client for this server ID.
	for k, c := range a.clients {
		if strings.HasPrefix(k, s.ID+"|") {
			c.Close()
			delete(a.clients, k)
		}
	}
	c := rcon.New(rcon.Options{
		Host: s.Host, Port: s.RCONPort, Password: s.RCONPassword,
		DialTimeout: 4 * time.Second, ReadTimeout: 12 * time.Second,
	})
	a.clients[key] = c
	return c
}

func (a *App) dropRCON(serverID string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for k, c := range a.clients {
		if strings.HasPrefix(k, serverID+"|") {
			c.Close()
			delete(a.clients, k)
		}
	}
}

// statusOf returns a copy of a server's status.
func (a *App) statusOf(id string) Status {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if s, ok := a.status[id]; ok {
		return *s
	}
	return Status{ServerID: id}
}

// allStatus returns every server's status in configured order.
func (a *App) allStatus() []Status {
	cfg := a.cfg.Get()
	a.mu.RLock()
	defer a.mu.RUnlock()
	out := make([]Status, 0, len(cfg.Servers))
	for _, s := range config.SortServers(cfg.Servers) {
		if st, ok := a.status[s.ID]; ok {
			cp := *st
			cp.Name = s.Name
			cp.Enabled = s.Enabled
			out = append(out, cp)
		} else {
			out = append(out, Status{ServerID: s.ID, Name: s.Name, Enabled: s.Enabled})
		}
	}
	return out
}

func (a *App) updateStatus(id string, fn func(*Status)) Status {
	a.mu.Lock()
	st, ok := a.status[id]
	if !ok {
		st = &Status{ServerID: id}
		a.status[id] = st
	}
	fn(st)
	cp := *st
	a.mu.Unlock()
	return cp
}

// --- HTTP -------------------------------------------------------------------

// Handler returns the fully wired HTTP handler.
func (a *App) Handler() http.Handler {
	mux := http.NewServeMux()

	// Public endpoints.
	mux.HandleFunc("/api/bootstrap", a.handleBootstrap)
	mux.HandleFunc("/api/setup", a.handleSetup)
	mux.HandleFunc("/api/login", a.handleLogin)
	mux.HandleFunc("/api/logout", a.handleLogout)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprintln(w, "ok")
	})
	mux.HandleFunc("/metrics", a.handleMetrics)

	// Authenticated endpoints.
	get := func(path string, h http.HandlerFunc) { mux.Handle(path, a.auth(h, false)) }
	post := func(path string, h http.HandlerFunc) { mux.Handle(path, a.auth(h, true)) }

	get("/api/state", a.handleState)
	get("/api/stream", a.handleStream)
	get("/api/commands", a.handleCommands)
	get("/api/server/capabilities", a.handleCapabilities)
	get("/api/events", a.handleEvents)
	get("/api/history", a.handleHistory)
	get("/api/players", a.handlePlayers)
	get("/api/browse", a.handleBrowse)
	get("/api/containers", a.handleContainers)
	get("/api/server/detail", a.handleServerDetail)
	get("/api/server/config", a.handleConfigFiles)
	get("/api/server/logs", a.handleLogs)
	get("/api/server/mods", a.handleMods)
	get("/api/catalogue", a.handleCatalogue)
	get("/api/mods/manage", a.handleModManager)
	get("/api/server/config/fields", a.handleConfigFields)
	get("/api/backups", a.handleBackupList)
	get("/api/backups/download", a.handleBackupDownload)
	get("/api/export", a.handleExport)
	get("/api/sessions", a.handleSessions)
	get("/api/stack", a.handleStack)
	post("/api/stack/rescan", a.handleStackRescan)
	get("/api/stack/new", a.handleStackNew)
	get("/api/stack/wizard/fields", a.handleStackWizardFields)
	post("/api/stack/plan", a.handleStackPlan)
	post("/api/stack/create", a.handleStackCreate)
	post("/api/stack/deploy", a.handleStackDeploy)
	get("/api/stack/env", a.handleStackEnv)
	post("/api/stack/env/save", a.handleStackEnvSave)

	post("/api/settings", a.handleSettings)
	post("/api/password", a.handlePassword)
	post("/api/server/save", a.handleServerSave)
	post("/api/server/delete", a.handleServerDelete)
	post("/api/server/destroy", a.handleServerDestroy)
	post("/api/server/test", a.handleTestConnection)
	post("/api/server/config/save", a.handleConfigSave)
	post("/api/server/config/apply", a.handleConfigApply)
	post("/api/mods/resolve", a.handleModResolve)
	post("/api/mods/sort", a.handleModSort)
	post("/api/mods/apply", a.handleModApply)
	post("/api/mods/preflight", a.handleModPreflight)
	post("/api/catalogue/add", a.handleCatalogueAdd)
	post("/api/catalogue/remove", a.handleCatalogueRemove)
	post("/api/action", a.handleAction)
	post("/api/console", a.handleConsole)
	post("/api/lifecycle", a.handleLifecycle)
	post("/api/server/capabilities/check", a.handleCapabilitiesCheck)
	post("/api/schedules", a.handleSchedules)
	post("/api/schedules/run", a.handleScheduleRun)
	post("/api/backups/create", a.handleBackupCreate)
	post("/api/backups/verify", a.handleBackupVerify)
	post("/api/backups/restore", a.handleBackupRestore)
	post("/api/backups/delete", a.handleBackupDelete)
	post("/api/player/note", a.handlePlayerNote)
	post("/api/player/forget", a.handlePlayerForget)
	post("/api/notify/test", a.handleNotifyTest)
	post("/api/discord/bot", a.handleBotConnect)
	post("/api/discord/bot/remove", a.handleBotRemove)
	get("/api/discord/channels", a.handleBotChannels)
	post("/api/import", a.handleImport)
	post("/api/sessions/revoke", a.handleRevokeSessions)
	get("/api/keys", a.handleAPIKeys)
	post("/api/keys/create", a.handleAPIKeyCreate)
	post("/api/keys/revoke", a.handleAPIKeyRevoke)
	post("/api/keys/revoke-all", a.handleAPIKeyRevokeAll)

	// The external API. It takes an API key only, never a session cookie,
	// and the routes above take a session only, never a key: a key cannot
	// reach settings, the password or key management.
	a.routeAPIv1(mux)

	mux.HandleFunc("/", a.handleStatic)
	return securityHeaders(a.logRequests(mux))
}

// auth wraps a handler with session and CSRF checks.
func (a *App) auth(next http.HandlerFunc, mutating bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if mutating && r.Method != http.MethodPost {
			httpError(w, http.StatusMethodNotAllowed, "use POST for this endpoint")
			return
		}
		if !mutating && r.Method != http.MethodGet {
			httpError(w, http.StatusMethodNotAllowed, "use GET for this endpoint")
			return
		}

		cookie, err := r.Cookie(sessionCookie)
		if err != nil || cookie.Value == "" {
			httpError(w, http.StatusUnauthorized, "sign in to continue")
			return
		}
		sess, ok := a.sess.lookup(cookie.Value)
		if !ok {
			clearSessionCookies(w, r)
			httpError(w, http.StatusUnauthorized, "your session has expired")
			return
		}
		if mutating {
			// Double-submit CSRF: an attacker on another origin can make the
			// browser send the cookie, but cannot read it to set the header.
			if r.Header.Get(csrfHeader) != sess.CSRF {
				httpError(w, http.StatusForbidden, "security token mismatch, reload the page")
				return
			}
			if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != "same-origin" && site != "none" {
				httpError(w, http.StatusForbidden, "cross-site requests are not accepted")
				return
			}
		}
		r = r.WithContext(context.WithValue(r.Context(), ctxUser{}, sess.Username))
		next(w, r)
	})
}

type ctxUser struct{}

type ctxSource struct{}

// source is the event log's Source for a request: "api" for an API key,
// "ui" for a browser session.
func source(r *http.Request) string {
	if v, ok := r.Context().Value(ctxSource{}).(string); ok {
		return v
	}
	return "ui"
}

func actor(r *http.Request) string {
	if v, ok := r.Context().Value(ctxUser{}).(string); ok {
		return v
	}
	return "unknown"
}

// securityHeaders applies a strict Content-Security-Policy.
//
// The UI ships its own CSS and JS as separate files with no inline script and
// no external origins, so the policy can be tight enough that even a successful
// injection has nowhere to send data.
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy",
			"default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self' data:; "+
				"font-src 'self'; connect-src 'self'; form-action 'none'; frame-ancestors 'none'; base-uri 'none'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Permissions-Policy", "geolocation=(), microphone=(), camera=(), interest-cohort=()")
		next.ServeHTTP(w, r)
	})
}

func (a *App) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, code: 200}
		next.ServeHTTP(rec, r)
		// Successful reads are noise; log the interesting cases only.
		if rec.code >= 400 || r.Method != http.MethodGet {
			log.Printf("%s %s %s %d %s", clientIP(r), r.Method, r.URL.Path, rec.code, time.Since(start).Round(time.Millisecond))
		}
	})
}

type statusRecorder struct {
	http.ResponseWriter
	code int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.code = code
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (a *App) handleStatic(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		httpError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	name := strings.TrimPrefix(r.URL.Path, "/")
	if name == "" {
		name = "index.html"
	}
	if strings.Contains(name, "..") {
		http.NotFound(w, r)
		return
	}
	data, err := fs.ReadFile(a.assets, name)
	if err != nil {
		// Unknown paths fall back to the app shell so deep links work.
		data, err = fs.ReadFile(a.assets, "index.html")
		if err != nil {
			http.NotFound(w, r)
			return
		}
		name = "index.html"
	}
	switch filepath.Ext(name) {
	case ".html":
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
	case ".css":
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
		w.Header().Set("Cache-Control", "public, max-age=300")
	case ".js":
		w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
		w.Header().Set("Cache-Control", "public, max-age=300")
	case ".svg":
		w.Header().Set("Content-Type", "image/svg+xml")
		w.Header().Set("Cache-Control", "public, max-age=86400")
	}
	_, _ = w.Write(data)
}

// --- response helpers -------------------------------------------------------

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("write json: %v", err)
	}
}

// writeStatusJSON sends a body with a non-200 status. Some refusals carry
// structure the interface has to render — a list of findings the operator must
// read before confirming — and httpError's bare {"error": ...} cannot express
// that.
func writeStatusJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("write json: %v", err)
	}
}

func httpError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	defer r.Body.Close()
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		httpError(w, http.StatusBadRequest, "could not read the request: "+err.Error())
		return false
	}
	return true
}

func ok(w http.ResponseWriter, extra map[string]any) {
	out := map[string]any{"ok": true}
	for k, v := range extra {
		out[k] = v
	}
	writeJSON(w, out)
}
