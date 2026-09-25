// Package store persists everything that is not configuration: the audit and
// activity log, the player registry, and player-count history.
//
// It deliberately uses append-only JSONL files with daily rotation rather than
// a database. There is no driver to keep current, the files are readable with
// tail and grep when something goes wrong at 2am, and retention is a matter of
// deleting old files. Recent data is kept in memory so the UI never waits on
// disk to render.
package store

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Severity classifies an event for display and filtering.
type Severity string

const (
	SevInfo    Severity = "info"
	SevWarn    Severity = "warn"
	SevError   Severity = "error"
	SevSuccess Severity = "success"
)

// Event is one thing that happened, whether triggered by an operator, by a
// schedule, or by the server itself.
type Event struct {
	At       time.Time      `json:"at"`
	Kind     string         `json:"kind"`
	Severity Severity       `json:"severity"`
	ServerID string         `json:"serverId,omitempty"`
	Server   string         `json:"server,omitempty"`
	Actor    string         `json:"actor,omitempty"`
	Source   string         `json:"source"` // ui | api | schedule | monitor | system
	Message  string         `json:"message"`
	Detail   string         `json:"detail,omitempty"`
	Meta     map[string]any `json:"meta,omitempty"`
}

// Player is one known player and their accumulated history.
type Player struct {
	ServerID    string    `json:"serverId"`
	Name        string    `json:"name"`
	SteamID     string    `json:"steamId,omitempty"`
	FirstSeen   time.Time `json:"firstSeen"`
	LastSeen    time.Time `json:"lastSeen"`
	Sessions    int       `json:"sessions"`
	PlaytimeSec int64     `json:"playtimeSec"`
	Online      bool      `json:"online"`
	OnlineSince time.Time `json:"onlineSince,omitempty"`
	Note        string    `json:"note,omitempty"`
	Banned      bool      `json:"banned,omitempty"`
}

// Playtime returns total playtime including the current session.
func (p Player) Playtime() time.Duration {
	d := time.Duration(p.PlaytimeSec) * time.Second
	if p.Online && !p.OnlineSince.IsZero() {
		d += time.Since(p.OnlineSince)
	}
	return d
}

// Sample is one point of player-count history.
type Sample struct {
	At       time.Time `json:"at"`
	ServerID string    `json:"serverId"`
	Online   bool      `json:"online"`
	Players  int       `json:"players"`
	Latency  int64     `json:"latencyMs"`
}

// Store owns the data directory.
type Store struct {
	dir        string
	retainDays int

	mu      sync.RWMutex
	recent  []Event  // ring buffer, oldest first
	samples []Sample // ring buffer, oldest first
	players map[string]*Player

	writeMu      sync.Mutex
	playersDirty bool
	lastFlush    time.Time

	subMu sync.Mutex
	subs  map[int]chan Event
	nextS int
}

const (
	maxRecentEvents = 1000
	maxSamples      = 20000 // roughly a week of one-minute samples for four servers
)

// Open prepares the store, loading the player registry and recent history.
func Open(dir string, retainDays int) (*Store, error) {
	if retainDays <= 0 {
		retainDays = 30
	}
	for _, sub := range []string{"", "events", "metrics"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o755); err != nil {
			return nil, err
		}
	}
	s := &Store{
		dir:        dir,
		retainDays: retainDays,
		players:    map[string]*Player{},
		subs:       map[int]chan Event{},
	}
	s.loadPlayers()
	s.loadRecentEvents()
	s.loadRecentSamples()
	return s, nil
}

func key(serverID, name string) string { return serverID + "\x00" + strings.ToLower(name) }

// --- events -----------------------------------------------------------------

// Append records an event, publishes it to live subscribers and writes it to
// today's log. Write failures are tolerated: losing an audit line is bad, but
// stalling a restart because the disk is full is worse.
func (s *Store) Append(e Event) {
	if e.At.IsZero() {
		e.At = time.Now()
	}
	if e.Severity == "" {
		e.Severity = SevInfo
	}
	if e.Source == "" {
		e.Source = "system"
	}

	s.mu.Lock()
	s.recent = append(s.recent, e)
	if len(s.recent) > maxRecentEvents {
		s.recent = s.recent[len(s.recent)-maxRecentEvents:]
	}
	s.mu.Unlock()

	s.publish(e)

	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	path := filepath.Join(s.dir, "events", "events-"+e.At.Format("2006-01-02")+".jsonl")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	line, err := json.Marshal(e)
	if err != nil {
		return
	}
	_, _ = f.Write(append(line, '\n'))
}

// Events returns recent events, newest first, optionally filtered.
func (s *Store) Events(serverID, kind string, limit int) []Event {
	if limit <= 0 || limit > maxRecentEvents {
		limit = 200
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Event, 0, limit)
	for i := len(s.recent) - 1; i >= 0 && len(out) < limit; i-- {
		e := s.recent[i]
		if serverID != "" && e.ServerID != serverID {
			continue
		}
		if kind != "" && e.Kind != kind {
			continue
		}
		out = append(out, e)
	}
	return out
}

// Subscribe returns a channel of live events plus a cancel function. The
// channel is buffered and lossy: a slow browser must never block the monitor.
func (s *Store) Subscribe() (<-chan Event, func()) {
	ch := make(chan Event, 64)
	s.subMu.Lock()
	id := s.nextS
	s.nextS++
	s.subs[id] = ch
	s.subMu.Unlock()
	return ch, func() {
		s.subMu.Lock()
		if c, ok := s.subs[id]; ok {
			delete(s.subs, id)
			close(c)
		}
		s.subMu.Unlock()
	}
}

func (s *Store) publish(e Event) {
	s.subMu.Lock()
	defer s.subMu.Unlock()
	for _, ch := range s.subs {
		select {
		case ch <- e:
		default: // subscriber is behind; drop rather than block
		}
	}
}

func (s *Store) loadRecentEvents() {
	files, _ := filepath.Glob(filepath.Join(s.dir, "events", "events-*.jsonl"))
	sort.Strings(files)
	if len(files) > 3 {
		files = files[len(files)-3:]
	}
	for _, path := range files {
		f, err := os.Open(path)
		if err != nil {
			continue
		}
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for sc.Scan() {
			var e Event
			if json.Unmarshal(sc.Bytes(), &e) == nil {
				s.recent = append(s.recent, e)
			}
		}
		f.Close()
	}
	if len(s.recent) > maxRecentEvents {
		s.recent = s.recent[len(s.recent)-maxRecentEvents:]
	}
}

// --- metrics ----------------------------------------------------------------

// AddSample records one status probe result.
func (s *Store) AddSample(sm Sample) {
	if sm.At.IsZero() {
		sm.At = time.Now()
	}
	s.mu.Lock()
	s.samples = append(s.samples, sm)
	if len(s.samples) > maxSamples {
		s.samples = s.samples[len(s.samples)-maxSamples:]
	}
	s.mu.Unlock()

	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	path := filepath.Join(s.dir, "metrics", "metrics-"+sm.At.Format("2006-01-02")+".jsonl")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	if line, err := json.Marshal(sm); err == nil {
		_, _ = f.Write(append(line, '\n'))
	}
}

// History returns samples for a server within the last window, thinned to at
// most points entries so a chart request never returns megabytes.
func (s *Store) History(serverID string, window time.Duration, points int) []Sample {
	if points <= 0 {
		points = 240
	}
	cutoff := time.Now().Add(-window)

	s.mu.RLock()
	var filtered []Sample
	for _, sm := range s.samples {
		if sm.At.Before(cutoff) {
			continue
		}
		if serverID != "" && sm.ServerID != serverID {
			continue
		}
		filtered = append(filtered, sm)
	}
	s.mu.RUnlock()

	if len(filtered) <= points {
		return filtered
	}
	// Bucket into equal time slices and take the peak of each: for a player
	// count the busiest moment is the interesting one, and averaging hides it.
	bucketSize := window / time.Duration(points)
	out := make([]Sample, 0, points)
	var current Sample
	var currentBucket int64 = -1
	for _, sm := range filtered {
		b := sm.At.UnixNano() / int64(bucketSize)
		if b != currentBucket {
			if currentBucket != -1 {
				out = append(out, current)
			}
			currentBucket = b
			current = sm
			continue
		}
		if sm.Players > current.Players {
			current.Players = sm.Players
		}
		current.At = sm.At
		current.Online = current.Online || sm.Online
		if sm.Latency > current.Latency {
			current.Latency = sm.Latency
		}
	}
	if currentBucket != -1 {
		out = append(out, current)
	}
	return out
}

func (s *Store) loadRecentSamples() {
	files, _ := filepath.Glob(filepath.Join(s.dir, "metrics", "metrics-*.jsonl"))
	sort.Strings(files)
	if len(files) > 8 {
		files = files[len(files)-8:]
	}
	for _, path := range files {
		f, err := os.Open(path)
		if err != nil {
			continue
		}
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for sc.Scan() {
			var sm Sample
			if json.Unmarshal(sc.Bytes(), &sm) == nil {
				s.samples = append(s.samples, sm)
			}
		}
		f.Close()
	}
	if len(s.samples) > maxSamples {
		s.samples = s.samples[len(s.samples)-maxSamples:]
	}
}

// --- players ----------------------------------------------------------------

// SyncPlayers reconciles the registry against the list currently connected,
// returning the players who joined and left since the previous call.
//
// This is where playtime and session counts come from, so the registry is only
// written to disk when something actually changed, rather than on every poll.
func (s *Store) SyncPlayers(serverID string, names []string) (joined, left []string) {
	now := time.Now()
	present := map[string]bool{}
	for _, n := range names {
		n = strings.TrimSpace(n)
		if n != "" {
			present[strings.ToLower(n)] = true
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	for _, n := range names {
		n = strings.TrimSpace(n)
		if n == "" {
			continue
		}
		k := key(serverID, n)
		p, ok := s.players[k]
		if !ok {
			p = &Player{ServerID: serverID, Name: n, FirstSeen: now}
			s.players[k] = p
		}
		p.Name = n
		p.LastSeen = now
		if !p.Online {
			p.Online = true
			p.OnlineSince = now
			p.Sessions++
			joined = append(joined, n)
			s.playersDirty = true
		}
	}

	for k, p := range s.players {
		if p.ServerID != serverID || !p.Online {
			continue
		}
		if present[strings.ToLower(p.Name)] {
			continue
		}
		if !p.OnlineSince.IsZero() {
			p.PlaytimeSec += int64(now.Sub(p.OnlineSince).Seconds())
		}
		p.Online = false
		p.OnlineSince = time.Time{}
		p.LastSeen = now
		left = append(left, p.Name)
		s.playersDirty = true
		_ = k
	}

	if len(joined) > 0 || len(left) > 0 {
		s.savePlayersLocked()
	}
	return joined, left
}

// MarkOffline clears the online flag for every player on a server, used when
// the server itself goes down so playtime is not credited during an outage.
func (s *Store) MarkOffline(serverID string) []string {
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	var left []string
	for _, p := range s.players {
		if p.ServerID != serverID || !p.Online {
			continue
		}
		if !p.OnlineSince.IsZero() {
			p.PlaytimeSec += int64(now.Sub(p.OnlineSince).Seconds())
		}
		p.Online = false
		p.OnlineSince = time.Time{}
		left = append(left, p.Name)
		s.playersDirty = true
	}
	if len(left) > 0 {
		s.savePlayersLocked()
	}
	return left
}

// SetSteamID attaches a Steam ID learned from the server logs.
func (s *Store) SetSteamID(serverID, name, steamID string) {
	if steamID == "" || name == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.players[key(serverID, name)]
	if !ok {
		p = &Player{ServerID: serverID, Name: name, FirstSeen: time.Now(), LastSeen: time.Now()}
		s.players[key(serverID, name)] = p
	}
	if p.SteamID != steamID {
		p.SteamID = steamID
		s.playersDirty = true
	}
}

// SetNote stores an operator note against a player.
func (s *Store) SetNote(serverID, name, note string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.players[key(serverID, name)]
	if !ok {
		return fmt.Errorf("unknown player %q", name)
	}
	if len(note) > 500 {
		note = note[:500]
	}
	p.Note = note
	s.playersDirty = true
	s.savePlayersLocked()
	return nil
}

// SetBanned records that a ban was issued through PZAdmin. The game server owns
// the real ban list; this is a local hint for the UI.
func (s *Store) SetBanned(serverID, name string, banned bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p, ok := s.players[key(serverID, name)]; ok {
		p.Banned = banned
		s.playersDirty = true
		s.savePlayersLocked()
	}
}

// Players returns the registry for a server, or for all servers when empty.
func (s *Store) Players(serverID string) []Player {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Player, 0, len(s.players))
	for _, p := range s.players {
		if serverID != "" && p.ServerID != serverID {
			continue
		}
		out = append(out, *p)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Online != out[j].Online {
			return out[i].Online
		}
		return out[i].LastSeen.After(out[j].LastSeen)
	})
	return out
}

// ForgetServer removes every player record belonging to a deleted server.
func (s *Store) ForgetServer(serverID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, p := range s.players {
		if p.ServerID == serverID {
			delete(s.players, k)
			s.playersDirty = true
		}
	}
	s.savePlayersLocked()
}

func (s *Store) playersPath() string { return filepath.Join(s.dir, "players.json") }

func (s *Store) loadPlayers() {
	b, err := os.ReadFile(s.playersPath())
	if err != nil {
		return
	}
	var list []Player
	if json.Unmarshal(b, &list) != nil {
		return
	}
	for i := range list {
		p := list[i]
		// A player cannot still be online across a restart of PZAdmin.
		p.Online = false
		p.OnlineSince = time.Time{}
		s.players[key(p.ServerID, p.Name)] = &p
	}
}

// savePlayersLocked must be called with the write lock held.
func (s *Store) savePlayersLocked() {
	list := make([]Player, 0, len(s.players))
	for _, p := range s.players {
		list = append(list, *p)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Name < list[j].Name })
	b, err := json.MarshalIndent(list, "", " ")
	if err != nil {
		return
	}
	tmp := s.playersPath() + ".tmp"
	if os.WriteFile(tmp, b, 0o644) == nil {
		_ = os.Rename(tmp, s.playersPath())
	}
	s.playersDirty = false
	s.lastFlush = time.Now()
}

// Flush writes the player registry if it has pending changes.
func (s *Store) Flush() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.playersDirty {
		s.savePlayersLocked()
	}
}

// --- retention --------------------------------------------------------------

// Prune deletes event and metric files older than the retention window.
func (s *Store) Prune() int {
	cutoff := time.Now().AddDate(0, 0, -s.retainDays)
	removed := 0
	for _, sub := range []string{"events", "metrics"} {
		files, _ := filepath.Glob(filepath.Join(s.dir, sub, "*.jsonl"))
		for _, path := range files {
			base := filepath.Base(path)
			idx := strings.LastIndex(base, "-")
			if idx < 0 {
				continue
			}
			datePart := strings.TrimSuffix(base, ".jsonl")
			if len(datePart) < 10 {
				continue
			}
			day, err := time.Parse("2006-01-02", datePart[len(datePart)-10:])
			if err != nil {
				continue
			}
			if day.Before(cutoff) {
				if os.Remove(path) == nil {
					removed++
				}
			}
		}
	}
	return removed
}

// Stats summarises the store for the settings screen.
type Stats struct {
	Players    int   `json:"players"`
	Events     int   `json:"events"`
	Samples    int   `json:"samples"`
	DiskBytes  int64 `json:"diskBytes"`
	RetainDays int   `json:"retainDays"`
}

// Stats returns store statistics.
func (s *Store) Stats() Stats {
	s.mu.RLock()
	st := Stats{Players: len(s.players), Events: len(s.recent), Samples: len(s.samples), RetainDays: s.retainDays}
	s.mu.RUnlock()
	_ = filepath.Walk(s.dir, func(_ string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			st.DiskBytes += info.Size()
		}
		return nil
	})
	return st
}
