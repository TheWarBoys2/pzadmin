// Package config holds PZAdmin's on-disk configuration.
//
// Everything the operator can change lives in a single JSON document written
// atomically with 0600 permissions. Secrets (RCON passwords, webhook URLs,
// the metrics token) live in the same document but are stripped by Redact
// before anything is handed to the browser.
package config

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// PBKDF2 work factor. 600k iterations of HMAC-SHA256 is the OWASP 2023
// recommendation. It costs roughly 200ms on a modern CPU, which is fine for a
// login form and painful for an offline cracker.
const pbkdf2Iterations = 600000

// Redacted is substituted for secrets in API responses. When a client sends it
// back in an update it means "keep the existing value".
const Redacted = "__pzadmin_unchanged__"

// Config is the whole persisted state of the application.
type Config struct {
	Version       int    `json:"version"`
	SetupComplete bool   `json:"setupComplete"`
	Username      string `json:"username"`
	PasswordHash  string `json:"passwordHash"`
	PasswordSalt  string `json:"passwordSalt"`
	PasswordIter  int    `json:"passwordIter"`

	// PZRoot is the bulk data root (/srv/zomboid). Every server's data and
	// config folders sit below it. It is set from PZADMIN_DATA_ROOT at startup.
	PZRoot string `json:"pzRoot"`
	// StacksRoot is the folder holding one compose stack per server
	// (~/docker/pzserver). It is set from PZADMIN_STACKS_ROOT at startup.
	StacksRoot string `json:"stacksRoot"`
	// Timezone is an IANA name used for every schedule. Empty means UTC.
	Timezone string `json:"timezone"`

	Servers   []Server  `json:"servers"`
	Schedules []Task    `json:"schedules"`
	Notify    Notify    `json:"notify"`
	Metrics   Metrics   `json:"metrics"`
	Interface Interface `json:"interface"`
}

// Server is one managed Project Zomboid instance.
type Server struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Enabled  bool   `json:"enabled"`
	Notes    string `json:"notes,omitempty"`
	SortHint int    `json:"sortHint,omitempty"`

	Host         string `json:"host"`
	RCONPort     int    `json:"rconPort"`
	RCONPassword string `json:"rconPassword"`

	// PZPath is the folder holding the server's data/ and config/ folders
	// (/srv/zomboid/<name>/projectzomboid). It must resolve inside PZRoot.
	PZPath string `json:"pzPath"`
	// GameRoot is where this server's Project Zomboid installation lives, for
	// the unusual case of it sitting outside the server's own directory.
	// Normally PZAdmin finds it without help.
	GameRoot string `json:"gameRoot,omitempty"`
	// GamePort is informational: shown to users so they can copy a join address.
	GamePort int `json:"gamePort,omitempty"`

	DockerContainer string `json:"dockerContainer"`

	// Everything below is read from the server's stack folder on every scan
	// and is never edited through the interface: the files are the truth.

	// Stack is the stack's folder name under StacksRoot: the server's stable
	// identity across scans.
	Stack       string `json:"stack,omitempty"`
	StackDir    string `json:"stackDir,omitempty"`
	ComposeFile string `json:"composeFile,omitempty"`
	// DataDir is mounted at /project-zomboid, ConfigDir at
	// /project-zomboid-config, ServerDir at /project-zomboid-config/Server.
	DataDir   string `json:"dataDir,omitempty"`
	ConfigDir string `json:"configDir,omitempty"`
	ServerDir string `json:"serverDir,omitempty"`
	// ServerName is SERVER_NAME, the prefix of the ini and lua files.
	ServerName string `json:"serverName,omitempty"`
	// RestartPolicy is the compose restart: value. RCON restarts depend on it.
	RestartPolicy string `json:"restartPolicy,omitempty"`
	// Missing is set when the stack folder has gone. The entry is kept so its
	// history and schedules are not lost to a folder being renamed, but
	// nothing acts on it.
	Missing bool `json:"missing,omitempty"`

	Recovery RecoveryPolicy `json:"recovery"`
	Mods     ModPolicy      `json:"mods"`
	Backup   BackupPolicy   `json:"backup"`
}

// RecoveryPolicy controls the watchdog that restarts a wedged container.
type RecoveryPolicy struct {
	Enabled bool `json:"enabled"`
	// FailuresBeforeRestart is the number of consecutive failed RCON probes
	// tolerated before a restart is attempted.
	FailuresBeforeRestart int `json:"failuresBeforeRestart"`
	// CooldownMinutes is the minimum gap between two recovery attempts.
	CooldownMinutes int `json:"cooldownMinutes"`
	// MaxAttempts caps restarts within one outage. Zero means unlimited.
	MaxAttempts int `json:"maxAttempts"`
}

// ModPolicy controls Workshop update detection.
type ModPolicy struct {
	// WatchUpdates enables log-based detection of "mods need update".
	WatchUpdates bool `json:"watchUpdates"`
	// AutoRestart schedules a warned restart when an update is detected.
	AutoRestart bool `json:"autoRestart"`
	// RestartDelayMinutes is the countdown announced to players.
	RestartDelayMinutes int `json:"restartDelayMinutes"`
}

// BackupPolicy controls PZAdmin's own archive-based backups.
type BackupPolicy struct {
	Enabled bool `json:"enabled"`
	// Keep is the number of archives retained per server.
	Keep int `json:"keep"`
	// IncludeConfig also archives the Server/ directory alongside Saves/.
	IncludeConfig bool `json:"includeConfig"`
}

// Task is a scheduled job: a name, a time, and a list of things to do.
type Task struct {
	ID       string `json:"id"`
	ServerID string `json:"serverId"`
	Name     string `json:"name"`
	Cron     string `json:"cron"`
	Enabled  bool   `json:"enabled"`
	// Steps run in order. A job with several steps is the point: announce,
	// wait, save, then restart is one job, not four that have to be timed to
	// line up by hand.
	Steps []Step `json:"steps"`
	// WarnMinutes announces a countdown before the job runs.
	WarnMinutes []int  `json:"warnMinutes,omitempty"`
	LastRun     string `json:"lastRun,omitempty"`
	LastResult  string `json:"lastResult,omitempty"`

	// Kind, Message and Command are the single-action fields used before
	// multi-step jobs existed. They are read once on load and folded into
	// Steps, then left empty.
	Kind    string `json:"kind,omitempty"`
	Message string `json:"message,omitempty"`
	Command string `json:"command,omitempty"`
}

// Step is one action within a job.
type Step struct {
	// Kind is one of: save, broadcast, restart, backup, command, action, wait.
	Kind string `json:"kind"`
	// Message is the text for a broadcast.
	Message string `json:"message,omitempty"`
	// Command is the raw RCON line for a command step.
	Command string `json:"command,omitempty"`
	// Action is a command ID from the catalogue, for an action step.
	Action string `json:"action,omitempty"`
	// Args are that command's arguments. Two placeholders are resolved when
	// the job runs: a player argument may be "@each", "@random" or a name, and
	// an item or vehicle argument may be "random:<category>".
	Args []string `json:"args,omitempty"`
	// Seconds is the pause for a wait step.
	Seconds int `json:"seconds,omitempty"`
	// ContinueOnError keeps the job going when this step fails.
	ContinueOnError bool `json:"continueOnError,omitempty"`
	// Note is the operator's own label for the step.
	Note string `json:"note,omitempty"`
}

// StepKinds enumerates the valid step kinds.
var StepKinds = []string{"save", "broadcast", "restart", "backup", "command", "action", "wait"}

// Notify configures outbound webhooks (Discord-compatible).
type Notify struct {
	// Webhooks are the destinations. Each has its own audience, events and
	// servers, so staff alerts and player announcements can go to different
	// channels.
	Webhooks []Webhook `json:"webhooks"`
	// MinIntervalSeconds throttles identical repeated staff alerts.
	MinIntervalSeconds int `json:"minIntervalSeconds"`

	// Enabled, WebhookURL and Events are the single webhook used before
	// destinations existed. They are read once on load and folded into
	// Webhooks, then left empty.
	Enabled    bool     `json:"enabled,omitempty"`
	WebhookURL string   `json:"webhookUrl,omitempty"`
	Events     []string `json:"events,omitempty"`
}

// Webhook is one notification destination.
type Webhook struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Enabled bool   `json:"enabled"`
	URL     string `json:"url"`
	// Audience is "staff", which gets every detail, or "players", which gets
	// short announcements written for the people who play on the server.
	Audience string   `json:"audience"`
	Events   []string `json:"events"`
	// Servers limits the webhook to these server IDs. Empty means every server.
	Servers []string `json:"servers"`
	// Messages overrides the wording of player announcements, by event.
	Messages map[string]string `json:"messages,omitempty"`
	// LiveStatus keeps one message per server in the channel and edits it
	// as the server changes, instead of posting a new one. Discord only.
	LiveStatus bool `json:"liveStatus,omitempty"`
	// ListPlayers adds who is online to the live status message.
	ListPlayers bool `json:"listPlayers,omitempty"`
}

// Webhook audiences.
const (
	AudienceStaff   = "staff"
	AudiencePlayers = "players"
)

// Metrics configures the Prometheus endpoint.
type Metrics struct {
	Enabled bool   `json:"enabled"`
	Token   string `json:"token"`
}

// Interface holds presentation preferences.
type Interface struct {
	// PollSeconds is how often each server is probed over RCON.
	PollSeconds int `json:"pollSeconds"`
	// RetainDays is how long event and metric history is kept on disk.
	RetainDays int `json:"retainDays"`
}

// NotifyEvents enumerates every event a staff webhook can be sent.
var NotifyEvents = []string{
	"server.down", "server.up", "server.recovered", "server.restart",
	"server.stop", "server.start",
	"player.join", "player.leave", "mods.update", "backup.done", "backup.failed",
	"admin.action",
}

// PlayerEvents enumerates the events a player webhook can be sent. The rest
// are staff business.
var PlayerEvents = []string{"server.restart", "server.up", "server.down", "server.stop", "mods.update"}

// DefaultStaffEvents is what a new staff webhook is sent.
var DefaultStaffEvents = []string{"server.down", "server.up", "server.recovered", "mods.update", "backup.failed"}

// DefaultPlayerEvents is what a new player webhook is sent: when a server
// actually goes down for a restart and when it is back, not every warning.
var DefaultPlayerEvents = []string{"server.restart", "server.up"}

// Defaults returns a Config suitable for a brand new installation.
func Defaults() Config {
	return Config{
		Version:      9,
		PZRoot:       "/srv/zomboid",
		Timezone:     "UTC",
		PasswordIter: pbkdf2Iterations,
		Servers:      []Server{},
		Schedules:    []Task{},
		Notify: Notify{
			Webhooks:           []Webhook{},
			MinIntervalSeconds: 300,
		},
		Metrics:   Metrics{Enabled: true},
		Interface: Interface{PollSeconds: 10, RetainDays: 30},
	}
}

// DefaultRecovery returns a conservative watchdog policy.
func DefaultRecovery() RecoveryPolicy {
	return RecoveryPolicy{FailuresBeforeRestart: 5, CooldownMinutes: 10, MaxAttempts: 3}
}

// DefaultBackup returns a sensible retention policy.
func DefaultBackup() BackupPolicy {
	return BackupPolicy{Keep: 10, IncludeConfig: true}
}

// Store loads and persists a Config, serialising writes.
type Store struct {
	mu   sync.RWMutex
	path string
	cfg  Config
}

// Open reads the config at path, creating defaults when it does not exist.
func Open(path string) (*Store, error) {
	s := &Store{path: path}
	b, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		s.cfg = Defaults()
		return s, nil
	case err != nil:
		return nil, err
	}
	cfg := Defaults()
	if err := json.Unmarshal(b, &cfg); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	s.cfg = Normalise(cfg)
	return s, nil
}

// Normalise fills in zero values that would otherwise break behaviour.
func Normalise(c Config) Config {
	d := Defaults()
	if c.PZRoot == "" {
		c.PZRoot = d.PZRoot
	}
	if c.Timezone == "" {
		c.Timezone = d.Timezone
	}
	if c.PasswordIter <= 0 {
		c.PasswordIter = pbkdf2Iterations
	}
	if c.Interface.PollSeconds < 3 {
		c.Interface.PollSeconds = d.Interface.PollSeconds
	}
	if c.Interface.RetainDays <= 0 {
		c.Interface.RetainDays = d.Interface.RetainDays
	}
	if c.Notify.MinIntervalSeconds <= 0 {
		c.Notify.MinIntervalSeconds = d.Notify.MinIntervalSeconds
	}
	if c.Servers == nil {
		c.Servers = []Server{}
	}
	if c.Schedules == nil {
		c.Schedules = []Task{}
	}
	c.Notify = normaliseNotify(c.Notify)
	for i := range c.Schedules {
		t := &c.Schedules[i]
		// Fold a pre-multi-step task into a single step so nothing an operator
		// already configured stops running after an upgrade.
		if len(t.Steps) == 0 && t.Kind != "" {
			t.Steps = []Step{{Kind: t.Kind, Message: t.Message, Command: t.Command}}
		}
		t.Kind, t.Message, t.Command = "", "", ""
		if t.Steps == nil {
			t.Steps = []Step{}
		}
		for j := range t.Steps {
			if t.Steps[j].Args == nil {
				t.Steps[j].Args = []string{}
			}
		}
	}
	for i := range c.Servers {
		s := &c.Servers[i]
		if s.RCONPort == 0 {
			s.RCONPort = 27015
		}
		if s.Host == "" {
			s.Host = "127.0.0.1"
		}
		if s.Recovery.FailuresBeforeRestart <= 0 {
			s.Recovery.FailuresBeforeRestart = 5
		}
		if s.Recovery.CooldownMinutes <= 0 {
			s.Recovery.CooldownMinutes = 10
		}
		if s.Backup.Keep <= 0 {
			s.Backup.Keep = 10
		}
		if s.Mods.RestartDelayMinutes <= 0 {
			s.Mods.RestartDelayMinutes = 10
		}
	}
	c.Version = 8
	return c
}

// normaliseNotify folds the old single webhook into the list and fills in
// what each webhook needs to be delivered.
func normaliseNotify(n Notify) Notify {
	if n.WebhookURL != "" && len(n.Webhooks) == 0 {
		events := n.Events
		if events == nil {
			events = DefaultStaffEvents
		}
		// server.restart used to cover stops and starts as well, so anyone
		// who asked for restarts keeps hearing about them.
		if contains(events, "server.restart") {
			for _, k := range []string{"server.stop", "server.start"} {
				if !contains(events, k) {
					events = append(events, k)
				}
			}
		}
		n.Webhooks = []Webhook{{
			ID: "alerts", Name: "Alerts", Enabled: n.Enabled, URL: n.WebhookURL,
			Audience: AudienceStaff, Events: events,
		}}
	}
	n.Enabled, n.WebhookURL, n.Events = false, "", nil
	if n.Webhooks == nil {
		n.Webhooks = []Webhook{}
	}
	for i := range n.Webhooks {
		w := &n.Webhooks[i]
		if w.ID == "" {
			w.ID = RandomToken(6)
		}
		if w.Audience != AudiencePlayers {
			w.Audience = AudienceStaff
		}
		if w.Events == nil {
			w.Events = []string{}
		}
		if w.Servers == nil {
			w.Servers = []string{}
		}
	}
	return n
}

func contains(list []string, v string) bool {
	for _, s := range list {
		if s == v {
			return true
		}
	}
	return false
}

// Get returns a deep copy of the current configuration.
func (s *Store) Get() Config {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return clone(s.cfg)
}

// Location resolves the configured timezone, falling back to UTC.
func (s *Store) Location() *time.Location {
	s.mu.RLock()
	name := s.cfg.Timezone
	s.mu.RUnlock()
	if name == "" {
		return time.UTC
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		return time.UTC
	}
	return loc
}

// Update applies fn to a copy of the config and persists the result. The
// mutation runs under the write lock so read-modify-write races are impossible.
func (s *Store) Update(fn func(*Config) error) (Config, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := clone(s.cfg)
	if err := fn(&next); err != nil {
		return Config{}, err
	}
	next = Normalise(next)
	if err := writeAtomic(s.path, next); err != nil {
		return Config{}, err
	}
	s.cfg = next
	return clone(next), nil
}

// Server returns a single server by ID.
func (s *Store) Server(id string) (Server, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, sv := range s.cfg.Servers {
		if sv.ID == id {
			return sv, true
		}
	}
	return Server{}, false
}

// EnabledServers returns every server the monitor should probe.
func (s *Store) EnabledServers() []Server {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Server, 0, len(s.cfg.Servers))
	for _, sv := range s.cfg.Servers {
		if sv.Enabled {
			out = append(out, sv)
		}
	}
	return out
}

// ContainerAllowed reports whether name belongs to a discovered server whose
// stack still exists. The Arcane client checks this before every request, so
// a compromised session cannot touch unrelated containers on the host.
func (s *Store) ContainerAllowed(name string) bool {
	if name == "" {
		return false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, sv := range s.cfg.Servers {
		if !sv.Missing && strings.EqualFold(sv.DockerContainer, name) {
			return true
		}
	}
	return false
}

func clone(c Config) Config {
	b, _ := json.Marshal(c)
	var out Config
	_ = json.Unmarshal(b, &out)
	return out
}

func writeAtomic(path string, c Config) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	// fsync before rename so a power cut cannot leave a truncated config.
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Redact returns a copy of c with every secret replaced by a placeholder. This
// is the only form of the config that may leave the process.
func Redact(c Config) Config {
	out := clone(c)
	out.PasswordHash = ""
	out.PasswordSalt = ""
	for i := range out.Servers {
		if out.Servers[i].RCONPassword != "" {
			out.Servers[i].RCONPassword = Redacted
		}
	}
	// Normalise folds the old field away, but a config that has not been
	// through it must not leak it either.
	out.Notify.WebhookURL = ""
	for i := range out.Notify.Webhooks {
		if out.Notify.Webhooks[i].URL != "" {
			out.Notify.Webhooks[i].URL = Redacted
		}
	}
	if out.Metrics.Token != "" {
		out.Metrics.Token = Redacted
	}
	return out
}

// Export returns a portable config with secrets removed entirely rather than
// placeholdered, so an exported file can be shared without leaking anything.
func Export(c Config) Config {
	out := Redact(c)
	for i := range out.Servers {
		out.Servers[i].RCONPassword = ""
	}
	for i := range out.Notify.Webhooks {
		out.Notify.Webhooks[i].URL = ""
	}
	out.Metrics.Token = ""
	out.Username = ""
	out.SetupComplete = false
	return out
}

// Unredact copies secrets from prev into next wherever next carries the
// placeholder, so the browser never has to know a real password to save a form.
func Unredact(next *Config, prev Config) {
	byID := map[string]Server{}
	for _, s := range prev.Servers {
		byID[s.ID] = s
	}
	for i := range next.Servers {
		if next.Servers[i].RCONPassword == Redacted {
			next.Servers[i].RCONPassword = byID[next.Servers[i].ID].RCONPassword
		}
	}
	UnredactWebhooks(next.Notify.Webhooks, prev.Notify.Webhooks)
	if next.Metrics.Token == Redacted {
		next.Metrics.Token = prev.Metrics.Token
	}
	next.PasswordHash = prev.PasswordHash
	next.PasswordSalt = prev.PasswordSalt
	next.PasswordIter = prev.PasswordIter
	next.Username = prev.Username
	next.SetupComplete = prev.SetupComplete
}

// UnredactWebhooks restores each webhook address the browser sent back as the
// placeholder, matching on ID. A placeholder with no match becomes empty
// rather than being saved as an address.
func UnredactWebhooks(next, prev []Webhook) {
	byID := map[string]string{}
	for _, w := range prev {
		byID[w.ID] = w.URL
	}
	for i := range next {
		if next[i].URL == Redacted {
			next[i].URL = byID[next[i].ID]
		}
	}
}

// --- password hashing -------------------------------------------------------

// HashPassword derives a PBKDF2-HMAC-SHA256 hash of password using salt.
func HashPassword(password, saltHex string, iterations int) string {
	salt, err := hex.DecodeString(saltHex)
	if err != nil {
		salt = []byte(saltHex)
	}
	if iterations <= 0 {
		iterations = pbkdf2Iterations
	}
	return hex.EncodeToString(pbkdf2(sha256.New, []byte(password), salt, iterations, 32))
}

// VerifyPassword compares a candidate password against a stored hash in
// constant time.
func VerifyPassword(password, saltHex, want string, iterations int) bool {
	got := HashPassword(password, saltHex, iterations)
	return subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

// NewSalt returns a fresh 16-byte hex salt.
func NewSalt() string { return RandomToken(16) }

// RandomToken returns n cryptographically random bytes, hex encoded.
func RandomToken(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic("pzadmin: crypto/rand unavailable: " + err.Error())
	}
	return hex.EncodeToString(b)
}

// pbkdf2 is RFC 8018 PBKDF2, implemented here so PZAdmin has no dependencies
// outside the standard library.
func pbkdf2(h func() hash.Hash, password, salt []byte, iter, keyLen int) []byte {
	prf := hmac.New(h, password)
	hashLen := prf.Size()
	blocks := (keyLen + hashLen - 1) / hashLen
	out := make([]byte, 0, blocks*hashLen)
	buf := make([]byte, 4)
	u := make([]byte, hashLen)
	t := make([]byte, hashLen)
	for block := 1; block <= blocks; block++ {
		prf.Reset()
		prf.Write(salt)
		binary.BigEndian.PutUint32(buf, uint32(block))
		prf.Write(buf)
		u = prf.Sum(u[:0])
		copy(t, u)
		for n := 1; n < iter; n++ {
			prf.Reset()
			prf.Write(u)
			u = prf.Sum(u[:0])
			for i := range t {
				t[i] ^= u[i]
			}
		}
		out = append(out, t...)
	}
	return out[:keyLen]
}

// SortServers orders servers for display: enabled first, then sort hint, then name.
func SortServers(in []Server) []Server {
	out := append([]Server(nil), in...)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].SortHint != out[j].SortHint {
			return out[i].SortHint < out[j].SortHint
		}
		return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name)
	})
	return out
}

// jsonMarshal is a small indirection used by tests to serialise a config.
func jsonMarshal(c Config) ([]byte, error) { return json.Marshal(c) }
