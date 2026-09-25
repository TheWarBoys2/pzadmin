package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/TheWarBoys2/pzadmin/internal/config"
	"github.com/TheWarBoys2/pzadmin/internal/store"
)

// The external API lives under /api/v1. It is a contract for scripts and
// bots: its shapes are its own, not the dashboard's, so the interface can
// change without breaking them. It takes an API key and nothing else.

type ctxAPIKey struct{}

func apiKeyFrom(r *http.Request) apiKey {
	k, _ := r.Context().Value(ctxAPIKey{}).(apiKey)
	return k
}

// --- rate limiting ------------------------------------------------------------

// rateClass is one token bucket: PerMin tokens refill a minute, up to Burst.
type rateClass struct {
	Name   string
	PerMin float64
	Burst  float64
}

var (
	rateRead  = rateClass{Name: "read", PerMin: 120, Burst: 30}
	rateWrite = rateClass{Name: "write", PerMin: 20, Burst: 5}
	// rateLifecycle is per key and server: three quick restarts, stops or
	// starts, then one every two minutes, so a looping script cannot keep a
	// server bouncing.
	rateLifecycle = rateClass{Name: "lifecycle", PerMin: 0.5, Burst: 3}
)

const maxAPIStreams = 3

type bucket struct {
	tokens float64
	last   time.Time
}

// rateLimiter is an in-memory token bucket per key. It resets when PZAdmin
// restarts, which is fine for a single process.
type rateLimiter struct {
	mu      sync.Mutex
	buckets map[string]*bucket
}

func newRateLimiter() *rateLimiter {
	return &rateLimiter{buckets: map[string]*bucket{}}
}

// take spends one token. It returns whether the request may go ahead, how
// many whole tokens are left, and how long until the next one if not.
func (l *rateLimiter) take(key string, c rateClass) (bool, int, time.Duration) {
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	b, found := l.buckets[key]
	if !found {
		if len(l.buckets) > 5000 {
			for k, v := range l.buckets {
				if now.Sub(v.last) > 30*time.Minute {
					delete(l.buckets, k)
				}
			}
		}
		b = &bucket{tokens: c.Burst, last: now}
		l.buckets[key] = b
	}
	b.tokens = math.Min(c.Burst, b.tokens+now.Sub(b.last).Minutes()*c.PerMin)
	b.last = now
	if b.tokens >= 1 {
		b.tokens--
		return true, int(b.tokens), 0
	}
	wait := time.Duration((1 - b.tokens) / c.PerMin * float64(time.Minute))
	return false, 0, wait
}

// --- counters for /metrics ----------------------------------------------------

type apiCounters struct {
	mu       sync.Mutex
	requests map[[2]string]uint64 // key name, status code
	limited  map[string]uint64
}

func (c *apiCounters) count(key string, code int) {
	c.mu.Lock()
	if c.requests == nil {
		c.requests = map[[2]string]uint64{}
	}
	c.requests[[2]string{key, strconv.Itoa(code)}]++
	c.mu.Unlock()
}

func (c *apiCounters) ratelimited(key string) {
	c.mu.Lock()
	if c.limited == nil {
		c.limited = map[string]uint64{}
	}
	c.limited[key]++
	c.mu.Unlock()
}

// lines returns the Prometheus lines for both counters, sorted so a scrape is
// stable.
func (c *apiCounters) lines() (requests, limited []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for k, v := range c.requests {
		requests = append(requests, fmt.Sprintf("pzadmin_api_requests_total{key=%q,code=%q} %d", k[0], k[1], v))
	}
	for k, v := range c.limited {
		limited = append(limited, fmt.Sprintf("pzadmin_api_ratelimited_total{key=%q} %d", k, v))
	}
	sort.Strings(requests)
	sort.Strings(limited)
	return requests, limited
}

// --- routing and auth ---------------------------------------------------------

func (a *App) routeAPIv1(mux *http.ServeMux) {
	handle := func(pattern, scope string, class rateClass, h http.HandlerFunc) {
		mux.Handle(pattern, a.apiAuth(scope, class, h))
	}
	handle("GET /api/v1/key", scopeRead, rateRead, a.apiWhoAmI)
	handle("GET /api/v1/commands", scopeRead, rateRead, a.apiCommands)
	handle("GET /api/v1/servers", scopeRead, rateRead, a.apiServers)
	handle("GET /api/v1/servers/{id}", scopeRead, rateRead, a.apiServerOne)
	handle("GET /api/v1/servers/{id}/players", scopeRead, rateRead, a.apiPlayers)
	handle("GET /api/v1/events", scopeRead, rateRead, a.apiEvents)
	handle("GET /api/v1/stream", scopeRead, rateRead, a.apiStream)

	handle("POST /api/v1/servers/{id}/actions", scopeControl, rateWrite, a.apiForward(a.handleAction))
	handle("POST /api/v1/servers/{id}/lifecycle", scopeControl, rateWrite, a.apiLifecycle)
	handle("POST /api/v1/servers/{id}/backups", scopeControl, rateWrite, a.apiForward(a.handleBackupCreate))
	handle("POST /api/v1/servers/{id}/console", scopeConsole, rateWrite, a.apiForward(a.handleConsole))

	// Anything else under /api/v1 is a JSON 404, rather than the app shell
	// the catch-all static handler would send.
	mux.HandleFunc("/api/v1/", func(w http.ResponseWriter, r *http.Request) {
		httpError(w, http.StatusNotFound, "no such API endpoint; check the method and the path")
	})
}

// apiAuth checks the key, its scope and its rate limit, then runs next with
// the key recorded as the actor.
func (a *App) apiAuth(scope string, class rateClass, next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		ip := clientIP(r)
		if allowed, wait := a.apiFail.allow(ip); !allowed {
			setRetryAfter(w, wait)
			httpError(w, http.StatusTooManyRequests, "too many requests with a bad key; wait and try again")
			return
		}
		token := bearerToken(r)
		if token == "" {
			w.Header().Set("WWW-Authenticate", `Bearer realm="pzadmin"`)
			httpError(w, http.StatusUnauthorized, "send an API key as Authorization: Bearer pzk_...")
			return
		}
		k, found := a.keys.lookup(token, ip)
		if !found {
			a.apiFail.fail(ip)
			w.Header().Set("WWW-Authenticate", `Bearer realm="pzadmin", error="invalid_token"`)
			httpError(w, http.StatusUnauthorized, "that API key is not valid, has expired or was revoked")
			return
		}

		rec := &statusRecorder{ResponseWriter: w, code: http.StatusOK}
		defer func() { a.apiStats.count(k.Name, rec.code) }()

		if !k.has(scope) {
			httpError(rec, http.StatusForbidden, "this key does not have the "+scope+" scope")
			return
		}
		allowed, remaining, wait := a.apiLimit.take(k.ID+"/"+class.Name, class)
		w.Header().Set("X-RateLimit-Limit", strconv.Itoa(int(class.PerMin)))
		w.Header().Set("X-RateLimit-Remaining", strconv.Itoa(remaining))
		if !allowed {
			a.apiStats.ratelimited(k.Name)
			setRetryAfter(w, wait)
			httpError(rec, http.StatusTooManyRequests, "rate limit reached; wait and try again")
			return
		}

		ctx := context.WithValue(r.Context(), ctxUser{}, keyActor(k))
		ctx = context.WithValue(ctx, ctxSource{}, "api")
		ctx = context.WithValue(ctx, ctxAPIKey{}, k)
		next(rec, r.WithContext(ctx))
	})
}

func setRetryAfter(w http.ResponseWriter, wait time.Duration) {
	secs := int(math.Ceil(wait.Seconds()))
	if secs < 1 {
		secs = 1
	}
	w.Header().Set("Retry-After", strconv.Itoa(secs))
}

// apiServerFor resolves the {id} in the path. A server the key may not see is
// reported as missing, so a limited key cannot learn which servers exist.
func (a *App) apiServerFor(w http.ResponseWriter, r *http.Request) (config.Server, bool) {
	id := r.PathValue("id")
	srv, found := a.cfg.Server(id)
	if !found || !apiKeyFrom(r).allows(id) {
		httpError(w, http.StatusNotFound, "no such server")
		return config.Server{}, false
	}
	return srv, true
}

// --- read endpoints -------------------------------------------------------------

func (a *App) apiWhoAmI(w http.ResponseWriter, r *http.Request) {
	k := apiKeyFrom(r)
	writeJSON(w, map[string]any{
		"id": k.ID, "name": k.Name, "scopes": k.Scopes, "servers": k.Servers,
		"created": k.Created, "expires": At(k.Expires), "version": Version,
	})
}

func (a *App) apiCommands(w http.ResponseWriter, r *http.Request) {
	out := []Command{}
	for _, c := range Commands() {
		if !c.legacy {
			out = append(out, c)
		}
	}
	writeJSON(w, map[string]any{"commands": out})
}

// apiServer is one server as the API describes it.
type apiServer struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Enabled bool   `json:"enabled"`
	Missing bool   `json:"missing,omitempty"`

	// State is one word for the server as a whole: online, offline,
	// restarting, stopped, deploying or unknown.
	State      string `json:"state"`
	Online     bool   `json:"online"`
	Restarting bool   `json:"restarting"`
	Stopped    bool   `json:"stopped"`
	Deploying  bool   `json:"deploying"`

	Players     []string `json:"players"`
	PlayerCount int      `json:"playerCount"`
	MaxPlayers  int      `json:"maxPlayers"`

	Description string `json:"description"`
	Address     string `json:"address"`
	Port        int    `json:"port"`

	LatencyMS  int64        `json:"latencyMs"`
	LastCheck  OptionalTime `json:"lastCheck"`
	LastOnline OptionalTime `json:"lastOnline"`
	StartedAt  OptionalTime `json:"startedAt"`
	UptimeSec  int64        `json:"uptimeSec"`
	Error      string       `json:"error,omitempty"`
	ErrorKind  string       `json:"errorKind,omitempty"`

	ModsEnabled int      `json:"modsEnabled"`
	ModsMissing []string `json:"modsMissing"`

	BackupCount   int          `json:"backupCount"`
	LastBackup    OptionalTime `json:"lastBackup"`
	BackupRunning bool         `json:"backupRunning"`

	PendingRestartAt OptionalTime `json:"pendingRestartAt"`
	PendingReason    string       `json:"pendingReason,omitempty"`
}

func apiServerView(s config.Server, st Status) apiServer {
	v := apiServer{
		ID: s.ID, Name: s.Name, Enabled: s.Enabled, Missing: s.Missing,
		Online: st.Online, Restarting: st.Restarting, Stopped: st.Stopped, Deploying: st.Deploying,
		Players: st.Players, PlayerCount: st.PlayerCount, MaxPlayers: st.MaxPlayers,
		Description: s.Public.Description, Address: s.Public.Address, Port: s.Public.Port,
		LatencyMS: st.LatencyMS, LastCheck: st.LastCheck, LastOnline: st.LastOnline,
		StartedAt: st.StartedAt, UptimeSec: st.ContainerUptime,
		Error: st.Error, ErrorKind: st.ErrorKind,
		ModsEnabled: st.ModsEnabled, ModsMissing: st.ModsMissing,
		BackupCount: st.BackupCount, LastBackup: st.LastBackup, BackupRunning: st.BackupRunning,
		PendingRestartAt: st.PendingRestartAt, PendingReason: st.PendingReason,
	}
	if v.Port == 0 {
		v.Port = s.GamePort
	}
	if v.Players == nil {
		v.Players = []string{}
	}
	if v.ModsMissing == nil {
		v.ModsMissing = []string{}
	}
	switch {
	case st.Deploying:
		v.State = "deploying"
	case st.Restarting:
		v.State = "restarting"
	case st.Stopped:
		v.State = "stopped"
	case st.Online:
		v.State = "online"
	case st.LastCheck.IsZero():
		v.State = "unknown"
	default:
		v.State = "offline"
	}
	return v
}

func (a *App) apiServerList(k apiKey) []apiServer {
	out := []apiServer{}
	for _, s := range config.SortServers(a.cfg.Get().Servers) {
		if k.allows(s.ID) {
			out = append(out, apiServerView(s, a.statusOf(s.ID)))
		}
	}
	return out
}

func (a *App) apiServers(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{"servers": a.apiServerList(apiKeyFrom(r))})
}

func (a *App) apiServerOne(w http.ResponseWriter, r *http.Request) {
	srv, found := a.apiServerFor(w, r)
	if !found {
		return
	}
	writeJSON(w, apiServerView(srv, a.statusOf(srv.ID)))
}

// apiPlayer leaves out Steam IDs and the operator's private notes.
type apiPlayer struct {
	Name        string       `json:"name"`
	Online      bool         `json:"online"`
	OnlineSince OptionalTime `json:"onlineSince"`
	FirstSeen   OptionalTime `json:"firstSeen"`
	LastSeen    OptionalTime `json:"lastSeen"`
	Sessions    int          `json:"sessions"`
	PlaytimeSec int64        `json:"playtimeSec"`
	Banned      bool         `json:"banned"`
}

func (a *App) apiPlayers(w http.ResponseWriter, r *http.Request) {
	srv, found := a.apiServerFor(w, r)
	if !found {
		return
	}
	out := []apiPlayer{}
	for _, p := range a.store.Players(srv.ID) {
		v := apiPlayer{
			Name: p.Name, Online: p.Online, FirstSeen: At(p.FirstSeen), LastSeen: At(p.LastSeen),
			Sessions: p.Sessions, PlaytimeSec: int64(p.Playtime().Seconds()), Banned: p.Banned,
		}
		if p.Online {
			v.OnlineSince = At(p.OnlineSince)
		}
		out = append(out, v)
	}
	writeJSON(w, map[string]any{"serverId": srv.ID, "players": out})
}

// apiEventVisible keeps sign-in records (with their IP addresses) away from
// keys, and keeps a key limited to some servers to those servers' events.
func apiEventVisible(k apiKey, e store.Event) bool {
	if strings.HasPrefix(e.Kind, "auth.") {
		return false
	}
	if len(k.Servers) > 0 && (e.ServerID == "" || !k.allows(e.ServerID)) {
		return false
	}
	return true
}

func (a *App) apiEvents(w http.ResponseWriter, r *http.Request) {
	k := apiKeyFrom(r)
	q := r.URL.Query()
	serverID := q.Get("server")
	if serverID != "" {
		if _, found := a.cfg.Server(serverID); !found || !k.allows(serverID) {
			httpError(w, http.StatusNotFound, "no such server")
			return
		}
	}
	limit := 50
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			httpError(w, http.StatusBadRequest, "limit must be a positive number")
			return
		}
		limit = min(n, 500)
	}
	out := []store.Event{}
	for _, e := range a.store.Events(serverID, q.Get("kind"), 1000) {
		if len(out) >= limit {
			break
		}
		if apiEventVisible(k, e) {
			out = append(out, e)
		}
	}
	writeJSON(w, map[string]any{"events": out})
}

// apiStream is a Server-Sent Events feed of server status and new events,
// filtered to what the key may see.
func (a *App) apiStream(w http.ResponseWriter, r *http.Request) {
	k := apiKeyFrom(r)
	flusher, canFlush := w.(http.Flusher)
	if !canFlush {
		httpError(w, http.StatusInternalServerError, "streaming is not supported by this connection")
		return
	}
	v, _ := a.apiStreams.LoadOrStore(k.ID, new(atomic.Int32))
	open := v.(*atomic.Int32)
	if open.Add(1) > maxAPIStreams {
		open.Add(-1)
		httpError(w, http.StatusTooManyRequests,
			fmt.Sprintf("this key already has %d streams open; close one first", maxAPIStreams))
		return
	}
	defer open.Add(-1)

	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
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

	var last string
	pushStatus := func() bool {
		all := a.apiServerList(k)
		b, _ := json.Marshal(all)
		if string(b) == last {
			return true
		}
		last = string(b)
		return send("status", map[string]any{"servers": all})
	}
	if !pushStatus() {
		return
	}

	statusTick := time.NewTicker(2 * time.Second)
	defer statusTick.Stop()
	keepalive := time.NewTicker(20 * time.Second)
	defer keepalive.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-a.stop:
			return
		case e, ok := <-events:
			if !ok {
				return
			}
			if apiEventVisible(k, e) && !send("event", e) {
				return
			}
		case <-statusTick.C:
			if !pushStatus() {
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

// --- write endpoints --------------------------------------------------------------

// readAPIBody reads a JSON object body. An empty body is an empty object.
func readAPIBody(w http.ResponseWriter, r *http.Request) (map[string]json.RawMessage, bool) {
	defer r.Body.Close()
	m := map[string]json.RawMessage{}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	if err := dec.Decode(&m); err != nil && !errors.Is(err, io.EOF) {
		httpError(w, http.StatusBadRequest, "could not read the request: "+err.Error())
		return nil, false
	}
	if _, has := m["serverId"]; has {
		httpError(w, http.StatusBadRequest, "the server comes from the URL; leave serverId out of the body")
		return nil, false
	}
	return m, true
}

// forwardTo runs one of the dashboard's own handlers with the server from the
// URL, so a key and a browser go through exactly the same checks, audit and
// Discord announcements.
func forwardTo(w http.ResponseWriter, r *http.Request, srv config.Server, body map[string]json.RawMessage, next http.HandlerFunc) {
	id, _ := json.Marshal(srv.ID)
	body["serverId"] = id
	b, _ := json.Marshal(body)
	r2 := r.Clone(r.Context())
	r2.Body = io.NopCloser(bytes.NewReader(b))
	r2.ContentLength = int64(len(b))
	next(w, r2)
}

func (a *App) apiForward(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		srv, found := a.apiServerFor(w, r)
		if !found {
			return
		}
		body, ok := readAPIBody(w, r)
		if !ok {
			return
		}
		forwardTo(w, r, srv, body, next)
	}
}

// apiLifecycle adds two limits on top of the write limit: one restart, stop
// or start at a time per server, and a slower bucket per key and server.
func (a *App) apiLifecycle(w http.ResponseWriter, r *http.Request) {
	srv, found := a.apiServerFor(w, r)
	if !found {
		return
	}
	body, ok := readAPIBody(w, r)
	if !ok {
		return
	}
	var action string
	_ = json.Unmarshal(body["action"], &action)
	if action == "restart" || action == "stop" || action == "start" {
		k := apiKeyFrom(r)
		allowed, _, wait := a.apiLimit.take(k.ID+"/lifecycle/"+srv.ID, rateLifecycle)
		if !allowed {
			a.apiStats.ratelimited(k.Name)
			setRetryAfter(w, wait)
			httpError(w, http.StatusTooManyRequests, "too many restarts, stops or starts for this server; wait and try again")
			return
		}
		if _, busy := a.apiInflight.LoadOrStore(srv.ID, true); busy {
			httpError(w, http.StatusConflict, srv.Name+" is already being restarted, stopped or started")
			return
		}
		defer a.apiInflight.Delete(srv.ID)
	}
	forwardTo(w, r, srv, body, a.handleLifecycle)
}
