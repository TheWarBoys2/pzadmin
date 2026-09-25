package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/TheWarBoys2/pzadmin/internal/config"
	"github.com/TheWarBoys2/pzadmin/internal/store"
)

const (
	// apiKeyPrefix marks a key so it is recognisable in a script or a secret
	// scanner, and cannot be mistaken for a session token.
	apiKeyPrefix = "pzk_"

	scopeRead = "read"
	scopeFull = "full"

	maxAPIKeys = 50
)

// apiKey is one key that lets a script or another service call the API with
// an Authorization: Bearer header instead of a browser session.
type apiKey struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Scope string `json:"scope"`
	// KeyHash is stored rather than the key itself, as with sessions, so a
	// leaked apikeys.json cannot be used to call the API.
	KeyHash  string    `json:"keyHash"`
	Created  time.Time `json:"created"`
	LastUsed time.Time `json:"lastUsed"`
	LastIP   string    `json:"lastIp"`
}

// apiKeyStore keeps the keys and persists them next to sessions.json.
type apiKeyStore struct {
	mu     sync.RWMutex
	path   string
	byHash map[string]*apiKey
}

func newAPIKeyStore(path string) *apiKeyStore {
	s := &apiKeyStore{path: path, byHash: map[string]*apiKey{}}
	s.load()
	return s
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

// create makes a new key and returns it in full. This is the only time the
// key itself exists outside the caller.
func (s *apiKeyStore) create(name, scope string) (string, apiKey, error) {
	name = strings.TrimSpace(name)
	if err := validAPIKeyName(name); err != nil {
		return "", apiKey{}, err
	}
	if scope != scopeRead && scope != scopeFull {
		return "", apiKey{}, errors.New(`scope must be "read" or "full"`)
	}
	token := apiKeyPrefix + config.RandomToken(32)
	k := &apiKey{
		ID:      config.RandomToken(6),
		Name:    name,
		Scope:   scope,
		KeyHash: hashToken(token),
		Created: time.Now(),
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
	s.byHash[k.KeyHash] = k
	cp := *k
	s.mu.Unlock()
	s.save()
	return token, cp, nil
}

// lookup finds the key a request presented and records that it was used.
func (s *apiKeyStore) lookup(token, ip string) (apiKey, bool) {
	if !strings.HasPrefix(token, apiKeyPrefix) {
		return apiKey{}, false
	}
	h := hashToken(token)
	s.mu.Lock()
	k, ok := s.byHash[h]
	if !ok {
		s.mu.Unlock()
		return apiKey{}, false
	}
	// Persist usage at most once a minute so a busy script does not write on
	// every request.
	dirty := time.Since(k.LastUsed) > time.Minute || k.LastIP != ip
	k.LastUsed = time.Now()
	k.LastIP = ip
	cp := *k
	s.mu.Unlock()
	if dirty {
		s.save()
	}
	return cp, true
}

// revoke removes a key by its public id, returning what was removed.
func (s *apiKeyStore) revoke(id string) (apiKey, bool) {
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
		return apiKey{}, false
	}
	s.save()
	return *found, true
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

func (s *apiKeyStore) save() {
	s.mu.RLock()
	list := make([]*apiKey, 0, len(s.byHash))
	for _, v := range s.byHash {
		list = append(list, v)
	}
	b, err := json.Marshal(list)
	s.mu.RUnlock()
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return
	}
	tmp := s.path + ".tmp"
	if os.WriteFile(tmp, b, 0o600) == nil {
		_ = os.Rename(tmp, s.path)
	}
}

func (s *apiKeyStore) load() {
	b, err := os.ReadFile(s.path)
	if err != nil {
		return
	}
	var list []*apiKey
	if json.Unmarshal(b, &list) != nil {
		return
	}
	for _, v := range list {
		if v != nil && v.KeyHash != "" {
			s.byHash[v.KeyHash] = v
		}
	}
}

// bearerToken returns the token from an Authorization: Bearer header, and
// whether the header was present at all.
func bearerToken(r *http.Request) (string, bool) {
	h := r.Header.Get("Authorization")
	if h == "" {
		return "", false
	}
	scheme, token, found := strings.Cut(h, " ")
	if !found || !strings.EqualFold(scheme, "Bearer") {
		return "", true
	}
	return strings.TrimSpace(token), true
}

// --- handlers ---------------------------------------------------------------

func (a *App) handleAPIKeys(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{"keys": a.keys.list()})
}

func (a *App) handleAPIKeyCreate(w http.ResponseWriter, r *http.Request) {
	var p struct {
		Name  string `json:"name"`
		Scope string `json:"scope"`
	}
	if !decodeJSON(w, r, &p) {
		return
	}
	token, k, err := a.keys.create(p.Name, p.Scope)
	if err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}
	a.event(store.Event{Kind: "auth.apikey", Severity: store.SevWarn, Source: "ui", Actor: actor(r),
		Message: "API key created: " + k.Name, Detail: "Scope: " + scopeLabel(k.Scope) + "."})
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
	k, found := a.keys.revoke(p.ID)
	if !found {
		httpError(w, http.StatusNotFound, "that key no longer exists")
		return
	}
	a.event(store.Event{Kind: "auth.apikey", Severity: store.SevWarn, Source: "ui", Actor: actor(r),
		Message: "API key revoked: " + k.Name})
	ok(w, nil)
}

func scopeLabel(scope string) string {
	if scope == scopeFull {
		return "full access"
	}
	return "read only"
}

// keyActor is how a key appears in the audit log, so an action taken by a
// script is never confused with one taken by the administrator in a browser.
func keyActor(k apiKey) string {
	return k.Name + " (API key)"
}
