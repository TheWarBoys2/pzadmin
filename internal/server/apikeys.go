package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/TheWarBoys2/pzadmin/internal/config"
	"github.com/TheWarBoys2/pzadmin/internal/fsutil"
	"github.com/TheWarBoys2/pzadmin/internal/store"
)

const (
	// apiKeyPrefix marks a key so it is recognisable in a script or a secret
	// scanner, and cannot be mistaken for a session token.
	apiKeyPrefix = "pzk_"

	// scopeRead covers every GET under /api/v1. Every key has it.
	scopeRead = "read"
	// scopeRequest asks for a mod to be added to a server. The request waits
	// for the administrator to approve it, so it changes nothing by itself.
	scopeRequest = "request"
	// scopeControl runs catalogue commands, restarts, stops, starts and
	// backups: the things the dashboard buttons do.
	scopeControl = "control"
	// scopeConsole sends raw RCON lines, which skip the catalogue's
	// argument checks, so it is granted separately.
	scopeConsole = "console"

	maxAPIKeys = 50
)

var apiScopes = []string{scopeRead, scopeRequest, scopeControl, scopeConsole}

// apiKey is one key that lets a script or another service call /api/v1 with
// an Authorization: Bearer header instead of a browser session.
type apiKey struct {
	// ID is public: it is part of the key and is shown in the settings list
	// so a key can be recognised without revealing it.
	ID     string   `json:"id"`
	Name   string   `json:"name"`
	Scopes []string `json:"scopes"`
	// Servers limits the key to these server IDs. Empty means every server,
	// including ones added later.
	Servers []string `json:"servers"`
	// KeyHash is stored rather than the key itself, as with sessions, so a
	// leaked apikeys.json cannot be used to call the API. A plain SHA-256 is
	// enough: the key is 256 random bits, not a password a person chose, so
	// there is nothing for key stretching to protect and it would only make
	// every request slow.
	KeyHash   string    `json:"keyHash"`
	Created   time.Time `json:"created"`
	CreatedBy string    `json:"createdBy"`
	// Expires is zero for a key that never expires.
	Expires  time.Time `json:"expires"`
	LastUsed time.Time `json:"lastUsed"`
	LastIP   string    `json:"lastIp"`
}

func (k apiKey) has(scope string) bool {
	for _, s := range k.Scopes {
		if s == scope {
			return true
		}
	}
	return false
}

// allows reports whether the key may see or act on a server.
func (k apiKey) allows(serverID string) bool {
	if len(k.Servers) == 0 {
		return true
	}
	for _, id := range k.Servers {
		if id == serverID {
			return true
		}
	}
	return false
}

func (k apiKey) expired(now time.Time) bool {
	return !k.Expires.IsZero() && now.After(k.Expires)
}

// apiKeyStore keeps the keys and persists them next to sessions.json. They are
// deliberately not in config.json, so they never reach /api/state, an export
// or an import.
type apiKeyStore struct {
	mu     sync.RWMutex
	path   string
	byHash map[string]*apiKey
	dirty  bool
	// saveMu is held from snapshot to rename, so saves land in the order
	// their snapshots were taken. Without it a housekeeping flush that
	// snapshotted before a revoke could be written after it, and the revoked
	// key would come back on the next start.
	saveMu sync.Mutex
	// loadErr is set when apikeys.json could not be read at start.
	loadErr error
}

func newAPIKeyStore(path string) *apiKeyStore {
	s := &apiKeyStore{path: path, byHash: map[string]*apiKey{}}
	s.loadErr = s.load()
	return s
}

// apiKeyRequest is what the settings screen asks for when making a key.
type apiKeyRequest struct {
	Name    string   `json:"name"`
	Scopes  []string `json:"scopes"`
	Servers []string `json:"servers"`
	// ExpiresDays is 0 for never.
	ExpiresDays int `json:"expiresDays"`
}

func validAPIKeyName(name string) error {
	if name == "" {
		return errors.New("give the key a name so you can tell it apart later")
	}
	if len(name) > 40 {
		return errors.New("the name must be 40 characters or fewer")
	}
	for _, r := range name {
		if unicode.IsControl(r) {
			return errors.New("the name cannot contain control characters")
		}
	}
	return nil
}

// normaliseScopes checks the requested scopes and always includes read: a key
// that can restart a server but not see whether it came back is no use.
func normaliseScopes(in []string) ([]string, error) {
	want := map[string]bool{scopeRead: true}
	for _, s := range in {
		s = strings.TrimSpace(s)
		known := false
		for _, k := range apiScopes {
			if s == k {
				known = true
			}
		}
		if !known {
			return nil, errors.New("unknown scope " + s + `; use "read", "request", "control" or "console"`)
		}
		want[s] = true
	}
	out := []string{}
	for _, k := range apiScopes {
		if want[k] {
			out = append(out, k)
		}
	}
	return out, nil
}

// create makes a new key and returns it in full. This is the only time the
// key itself exists outside the caller.
func (s *apiKeyStore) create(req apiKeyRequest, createdBy string, serverExists func(string) bool) (string, apiKey, error) {
	name := strings.TrimSpace(req.Name)
	if err := validAPIKeyName(name); err != nil {
		return "", apiKey{}, err
	}
	scopes, err := normaliseScopes(req.Scopes)
	if err != nil {
		return "", apiKey{}, err
	}
	servers := []string{}
	seen := map[string]bool{}
	for _, id := range req.Servers {
		if seen[id] {
			continue
		}
		if !serverExists(id) {
			return "", apiKey{}, errors.New("no such server " + id)
		}
		seen[id] = true
		servers = append(servers, id)
	}
	if req.ExpiresDays < 0 || req.ExpiresDays > 3650 {
		return "", apiKey{}, errors.New("expiry must be between 0 (never) and 3650 days")
	}

	now := time.Now()
	k := &apiKey{
		Name:      name,
		Scopes:    scopes,
		Servers:   servers,
		Created:   now,
		CreatedBy: createdBy,
	}
	if req.ExpiresDays > 0 {
		k.Expires = now.Add(time.Duration(req.ExpiresDays) * 24 * time.Hour)
	}

	s.mu.Lock()
	if len(s.byHash) >= maxAPIKeys {
		s.mu.Unlock()
		return "", apiKey{}, errors.New("there are already 50 keys; revoke one you no longer use first")
	}
	for _, v := range s.byHash {
		if strings.EqualFold(v.Name, name) {
			s.mu.Unlock()
			return "", apiKey{}, errors.New("a key with that name already exists")
		}
	}
	var token string
	for {
		k.ID = config.RandomToken(4)
		if !s.idTakenLocked(k.ID) {
			break
		}
	}
	token = apiKeyPrefix + k.ID + "_" + config.RandomToken(32)
	k.KeyHash = hashToken(token)
	s.byHash[k.KeyHash] = k
	cp := *k
	s.mu.Unlock()
	if err := s.save(); err != nil {
		// A key that is not on disk would vanish at the next restart, so do
		// not hand it out.
		s.mu.Lock()
		delete(s.byHash, k.KeyHash)
		s.mu.Unlock()
		return "", apiKey{}, fmt.Errorf("the key could not be saved: %w", err)
	}
	return token, cp, nil
}

func (s *apiKeyStore) idTakenLocked(id string) bool {
	for _, v := range s.byHash {
		if v.ID == id {
			return true
		}
	}
	return false
}

// lookup finds the key a request presented and records that it was used. An
// expired key is not found.
func (s *apiKeyStore) lookup(token, ip string) (apiKey, bool) {
	if !strings.HasPrefix(token, apiKeyPrefix) {
		return apiKey{}, false
	}
	h := hashToken(token)
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	k, ok := s.byHash[h]
	if !ok || k.expired(now) {
		return apiKey{}, false
	}
	// Usage is kept in memory and written out by housekeeping, so a busy
	// script does not write the file on every request.
	if now.Sub(k.LastUsed) > time.Minute || k.LastIP != ip {
		s.dirty = true
	}
	k.LastUsed = now
	k.LastIP = ip
	return *k, true
}

// flush writes pending last-used times. Called from housekeeping and on close.
func (s *apiKeyStore) flush() {
	s.mu.Lock()
	dirty := s.dirty
	s.dirty = false
	s.mu.Unlock()
	if dirty && s.save() != nil {
		// Try again at the next flush.
		s.mu.Lock()
		s.dirty = true
		s.mu.Unlock()
	}
}

// active reports whether a key still exists and has not expired, so a long
// open stream stops when its key is revoked.
func (s *apiKeyStore) active(id string) bool {
	now := time.Now()
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, v := range s.byHash {
		if v.ID == id {
			return !v.expired(now)
		}
	}
	return false
}

// revoke removes a key by its public id, returning what was removed. The key
// stops working at once either way; a save error means it would come back
// after a restart.
func (s *apiKeyStore) revoke(id string) (apiKey, bool, error) {
	s.mu.Lock()
	var found *apiKey
	for h, v := range s.byHash {
		if v.ID == id {
			found = v
			delete(s.byHash, h)
			break
		}
	}
	s.mu.Unlock()
	if found == nil {
		return apiKey{}, false, nil
	}
	return *found, true, s.save()
}

// revokeAll removes every key and returns how many there were.
func (s *apiKeyStore) revokeAll() (int, error) {
	s.mu.Lock()
	n := len(s.byHash)
	s.byHash = map[string]*apiKey{}
	s.mu.Unlock()
	return n, s.save()
}

// list returns the keys for the settings screen, without their hashes.
func (s *apiKeyStore) list() []apiKey {
	s.mu.RLock()
	out := make([]apiKey, 0, len(s.byHash))
	for _, v := range s.byHash {
		cp := *v
		cp.KeyHash = ""
		out = append(out, cp)
	}
	s.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool { return out[i].Created.Before(out[j].Created) })
	return out
}

func (s *apiKeyStore) save() error {
	s.saveMu.Lock()
	defer s.saveMu.Unlock()
	s.mu.RLock()
	list := make([]*apiKey, 0, len(s.byHash))
	for _, v := range s.byHash {
		list = append(list, v)
	}
	b, err := json.Marshal(list)
	s.mu.RUnlock()
	if err == nil {
		err = fsutil.WriteFile(s.path, b, 0o600)
	}
	if err != nil {
		log.Printf("saving API keys: %v", err)
	}
	return err
}

func (s *apiKeyStore) load() error {
	var list []*apiKey
	if _, err := fsutil.ReadJSON(s.path, &list); err != nil {
		log.Printf("API keys: %v", err)
		return err
	}
	for _, v := range list {
		if v != nil && v.KeyHash != "" {
			s.byHash[v.KeyHash] = v
		}
	}
	return nil
}

// bearerToken returns the token from an Authorization: Bearer header.
func bearerToken(r *http.Request) string {
	scheme, token, found := strings.Cut(r.Header.Get("Authorization"), " ")
	if !found || !strings.EqualFold(scheme, "Bearer") {
		return ""
	}
	return strings.TrimSpace(token)
}

// keyActor is how a key appears in the audit log, so an action taken by a
// script is never confused with one taken by the administrator in a browser.
func keyActor(k apiKey) string {
	return "key:" + k.Name
}

// --- management handlers (browser session only) ------------------------------

func (a *App) handleAPIKeys(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{"keys": a.keys.list(), "max": maxAPIKeys})
}

func (a *App) handleAPIKeyCreate(w http.ResponseWriter, r *http.Request) {
	var p apiKeyRequest
	if !decodeJSON(w, r, &p) {
		return
	}
	exists := func(id string) bool { _, ok := a.cfg.Server(id); return ok }
	token, k, err := a.keys.create(p, actor(r), exists)
	if err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}
	detail := "Scopes: " + strings.Join(k.Scopes, ", ") + "."
	if len(k.Servers) > 0 {
		detail += " Limited to " + a.serverNames(k.Servers) + "."
	}
	if !k.Expires.IsZero() {
		detail += " Expires " + k.Expires.Format("2 Jan 2006") + "."
	}
	a.event(store.Event{Kind: "auth.apikey", Severity: store.SevWarn, Source: source(r), Actor: actor(r),
		Message: "API key created: " + k.Name, Detail: detail})
	k.KeyHash = ""
	ok(w, map[string]any{"key": token, "apiKey": k})
}

func (a *App) handleAPIKeyRevoke(w http.ResponseWriter, r *http.Request) {
	var p struct {
		ID string `json:"id"`
	}
	if !decodeJSON(w, r, &p) {
		return
	}
	k, found, err := a.keys.revoke(p.ID)
	if !found {
		httpError(w, http.StatusNotFound, "that key no longer exists")
		return
	}
	if err != nil {
		a.event(store.Event{Kind: "auth.apikey", Severity: store.SevError, Source: source(r), Actor: actor(r),
			Message: "API key revoked but not saved: " + k.Name,
			Detail:  "It stopped working now, but would work again after a restart. " + err.Error()})
		httpError(w, http.StatusInternalServerError, "the key has stopped working, but the change could not be "+
			"saved, so it would work again after PZAdmin restarts. Check the data folder has space, then revoke "+
			"it again: "+err.Error())
		return
	}
	a.event(store.Event{Kind: "auth.apikey", Severity: store.SevWarn, Source: source(r), Actor: actor(r),
		Message: "API key revoked: " + k.Name})
	ok(w, nil)
}

func (a *App) handleAPIKeyRevokeAll(w http.ResponseWriter, r *http.Request) {
	n, err := a.keys.revokeAll()
	if err != nil {
		a.event(store.Event{Kind: "auth.apikey", Severity: store.SevError, Source: source(r), Actor: actor(r),
			Message: "All API keys revoked but not saved",
			Detail:  pluralCount(n, "key", "keys") + " stopped working now, but would work again after a restart. " + err.Error()})
		httpError(w, http.StatusInternalServerError, "the keys have stopped working, but the change could not be "+
			"saved, so they would work again after PZAdmin restarts. Check the data folder has space, then try "+
			"again: "+err.Error())
		return
	}
	a.event(store.Event{Kind: "auth.apikey", Severity: store.SevWarn, Source: source(r), Actor: actor(r),
		Message: "All API keys revoked", Detail: pluralCount(n, "key", "keys") + " removed."})
	ok(w, map[string]any{"revoked": n})
}

func (a *App) serverNames(ids []string) string {
	names := make([]string, 0, len(ids))
	for _, id := range ids {
		if s, ok := a.cfg.Server(id); ok {
			names = append(names, s.Name)
		} else {
			names = append(names, id)
		}
	}
	return strings.Join(names, ", ")
}

func pluralCount(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return strconv.Itoa(n) + " " + many
}
