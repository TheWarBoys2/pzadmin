package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/TheWarBoys2/pzadmin/internal/config"
	"github.com/TheWarBoys2/pzadmin/internal/fsutil"
	"github.com/TheWarBoys2/pzadmin/internal/store"
)

// The owner is the account made at setup. It lives in config.json, as it
// always has, and can do everything. Other admins are users: the owner makes
// them, ticks what each may do, and can limit them to some servers.
const (
	// permView sees servers, players, logs, activity, charts and backups.
	// Every user has it.
	permView = "view"
	// permControl starts, stops and restarts, runs catalogue commands, makes
	// backups and runs an existing job now: the dashboard buttons.
	permControl = "control"
	// permConsole sends raw RCON lines, which skip the catalogue's argument
	// checks, so it is granted separately, as for API keys.
	permConsole = "console"
	// permMods changes the mod list and approves or rejects mod requests.
	permMods = "mods"
	// permConfig edits server settings and .env, redeploys, restores and
	// deletes backups, and clears player history.
	permConfig = "config"
	// permOwner marks a route only the owner may use. No user can hold it.
	permOwner = "owner"

	maxUsers = 50
)

var userPerms = []string{permView, permControl, permConsole, permMods, permConfig}

// user is one admin account other than the owner.
type user struct {
	ID    string   `json:"id"`
	Name  string   `json:"name"`
	Perms []string `json:"perms"`
	// Servers limits the user to these server IDs. Empty means every server,
	// including ones added later.
	Servers      []string `json:"servers"`
	PasswordHash string   `json:"passwordHash"`
	PasswordSalt string   `json:"passwordSalt"`
	PasswordIter int      `json:"passwordIter"`
	// MustChange is set on a new account and after a reset: the password was
	// made by PZAdmin and handed over by the owner, so it is only good for
	// choosing a real one.
	MustChange bool      `json:"mustChange"`
	Disabled   bool      `json:"disabled"`
	Created    time.Time `json:"created"`
	CreatedBy  string    `json:"createdBy"`
	LastLogin  time.Time `json:"lastLogin"`
}

// principal is who a browser request is from and what they may do.
type principal struct {
	Owner      bool     `json:"owner"`
	UserID     string   `json:"userId,omitempty"`
	Name       string   `json:"name"`
	Perms      []string `json:"perms"`
	Servers    []string `json:"servers"`
	MustChange bool     `json:"mustChangePassword"`
}

var ownerPrincipal = principal{Owner: true, Perms: userPerms, Servers: []string{}}

type ctxPrincipal struct{}

// principalFrom returns who a request is from. A handler reached without a
// browser session (the external API forwards to some of them after checking
// the key itself) is treated as the owner, because the key's own scope and
// server checks have already run.
func principalFrom(r *http.Request) principal {
	if p, ok := r.Context().Value(ctxPrincipal{}).(principal); ok {
		return p
	}
	return ownerPrincipal
}

func (p principal) can(perm string) bool {
	if p.Owner {
		return true
	}
	if perm == permOwner {
		return false
	}
	if perm == permView {
		return true
	}
	for _, v := range p.Perms {
		if v == perm {
			return true
		}
	}
	return false
}

// allServers reports whether the principal sees every server.
func (p principal) allServers() bool {
	return p.Owner || len(p.Servers) == 0
}

func (p principal) allows(serverID string) bool {
	if p.allServers() {
		return true
	}
	for _, id := range p.Servers {
		if id == serverID {
			return true
		}
	}
	return false
}

// seesEvent reports whether an event belongs in this principal's activity.
// Events with no server (sign-ins, settings, keys, users) are the owner's.
func (p principal) seesEvent(e store.Event) bool {
	if p.Owner {
		return true
	}
	return e.ServerID != "" && p.allows(e.ServerID)
}

func (u user) principal() principal {
	return principal{UserID: u.ID, Name: u.Name, Perms: append([]string{}, u.Perms...),
		Servers: append([]string{}, u.Servers...), MustChange: u.MustChange}
}

// userStore keeps the other admins in users.json, next to apikeys.json. Like
// keys they are not in config.json, so they never reach an export or an
// import, and an install with no users is exactly what it was before.
type userStore struct {
	mu   sync.RWMutex
	path string
	byID map[string]*user
	// saveMu is held from snapshot to rename, so saves land in the order
	// their snapshots were taken.
	saveMu sync.Mutex
	// loadErr is set when users.json could not be read at start.
	loadErr error
}

func newUserStore(path string) *userStore {
	s := &userStore{path: path, byID: map[string]*user{}}
	s.loadErr = s.load()
	return s
}

// userRequest is what the settings screen sends to add or change a user.
type userRequest struct {
	ID      string   `json:"id,omitempty"`
	Name    string   `json:"name"`
	Perms   []string `json:"perms"`
	Servers []string `json:"servers"`
}

func validUserName(name, owner string) error {
	if len(name) < 2 || len(name) > 64 {
		return errors.New("the username must be between 2 and 64 characters")
	}
	for _, r := range name {
		if unicode.IsControl(r) {
			return errors.New("the username cannot contain control characters")
		}
	}
	if strings.EqualFold(name, owner) {
		return errors.New("that is the owner's username; pick another")
	}
	return nil
}

// normalisePerms checks the ticked permissions and always includes view: an
// admin who can restart a server but not see whether it came back is no use.
func normalisePerms(in []string) ([]string, error) {
	want := map[string]bool{permView: true}
	for _, p := range in {
		p = strings.TrimSpace(p)
		known := false
		for _, k := range userPerms {
			if p == k {
				known = true
			}
		}
		if !known {
			return nil, errors.New("unknown permission " + p +
				`; use "view", "control", "console", "mods" or "config"`)
		}
		want[p] = true
	}
	out := []string{}
	for _, k := range userPerms {
		if want[k] {
			out = append(out, k)
		}
	}
	return out, nil
}

func normaliseServers(in []string, exists func(string) bool) ([]string, error) {
	out := []string{}
	seen := map[string]bool{}
	for _, id := range in {
		if seen[id] {
			continue
		}
		if !exists(id) {
			return nil, errors.New("no such server " + id)
		}
		seen[id] = true
		out = append(out, id)
	}
	return out, nil
}

// temporaryPassword is what the owner hands a new user. 64 random bits
// written as 16 hex characters: long enough that it cannot be guessed in the
// time before it is changed, short enough to read out.
func temporaryPassword() string {
	return config.RandomToken(8)
}

func setUserPassword(u *user, password string) {
	u.PasswordSalt = config.NewSalt()
	u.PasswordIter = 0
	u.PasswordHash = config.HashPassword(password, u.PasswordSalt, 0)
}

// create adds a user and returns it with its temporary password.
func (s *userStore) create(req userRequest, owner, by string, exists func(string) bool) (user, string, error) {
	name := strings.TrimSpace(req.Name)
	if err := validUserName(name, owner); err != nil {
		return user{}, "", err
	}
	perms, err := normalisePerms(req.Perms)
	if err != nil {
		return user{}, "", err
	}
	servers, err := normaliseServers(req.Servers, exists)
	if err != nil {
		return user{}, "", err
	}
	temp := temporaryPassword()
	u := &user{Name: name, Perms: perms, Servers: servers, MustChange: true,
		Created: time.Now(), CreatedBy: by}
	setUserPassword(u, temp)

	s.mu.Lock()
	if len(s.byID) >= maxUsers {
		s.mu.Unlock()
		return user{}, "", fmt.Errorf("there are already %d users; remove one first", maxUsers)
	}
	for _, v := range s.byID {
		if strings.EqualFold(v.Name, name) {
			s.mu.Unlock()
			return user{}, "", errors.New("a user with that name already exists")
		}
	}
	for {
		u.ID = config.RandomToken(4)
		if _, taken := s.byID[u.ID]; !taken {
			break
		}
	}
	s.byID[u.ID] = u
	cp := *u
	s.mu.Unlock()
	if err := s.save(); err != nil {
		s.mu.Lock()
		delete(s.byID, u.ID)
		s.mu.Unlock()
		return user{}, "", fmt.Errorf("the user could not be saved: %w", err)
	}
	return cp, temp, nil
}

// change runs fn on a user and saves. A save error leaves the change in
// place in memory, where it applies until PZAdmin restarts, and is returned
// so the caller can say so.
func (s *userStore) change(id string, fn func(*user) error) (user, error) {
	s.mu.Lock()
	u, ok := s.byID[id]
	if !ok {
		s.mu.Unlock()
		return user{}, errNoSuchUser
	}
	next := *u
	if err := fn(&next); err != nil {
		s.mu.Unlock()
		return user{}, err
	}
	*u = next
	s.mu.Unlock()
	return next, s.save()
}

var errNoSuchUser = errors.New("that user no longer exists")

func (s *userStore) remove(id string) (user, error) {
	s.mu.Lock()
	u, ok := s.byID[id]
	if ok {
		delete(s.byID, id)
	}
	s.mu.Unlock()
	if !ok {
		return user{}, errNoSuchUser
	}
	return *u, s.save()
}

func (s *userStore) get(id string) (user, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	u, ok := s.byID[id]
	if !ok {
		return user{}, false
	}
	return *u, true
}

func (s *userStore) byName(name string) (user, bool) {
	name = strings.TrimSpace(name)
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, u := range s.byID {
		if strings.EqualFold(u.Name, name) {
			return *u, true
		}
	}
	return user{}, false
}

// touch records a sign-in. It is saved with the next change, not on its own,
// so a sign-in never waits on a disk write.
func (s *userStore) touch(id string) {
	s.mu.Lock()
	if u, ok := s.byID[id]; ok {
		u.LastLogin = time.Now()
	}
	s.mu.Unlock()
}

// forgetServer drops a deleted server from every user's list. A user left
// with no servers at all is disabled rather than quietly given every server.
func (s *userStore) forgetServer(id string) {
	changed := false
	s.mu.Lock()
	for _, u := range s.byID {
		if len(u.Servers) == 0 {
			continue
		}
		kept := u.Servers[:0:0]
		for _, v := range u.Servers {
			if v != id {
				kept = append(kept, v)
			}
		}
		if len(kept) != len(u.Servers) {
			changed = true
			u.Servers = kept
			if len(kept) == 0 {
				u.Disabled = true
			}
		}
	}
	s.mu.Unlock()
	if changed {
		_ = s.save()
	}
}

// userView is a user as the settings screen sees it: no password hash.
type userView struct {
	ID         string    `json:"id"`
	Name       string    `json:"name"`
	Perms      []string  `json:"perms"`
	Servers    []string  `json:"servers"`
	MustChange bool      `json:"mustChange"`
	Disabled   bool      `json:"disabled"`
	Created    time.Time `json:"created"`
	CreatedBy  string    `json:"createdBy"`
	LastLogin  time.Time `json:"lastLogin"`
}

func (u user) view() userView {
	return userView{ID: u.ID, Name: u.Name, Perms: u.Perms, Servers: u.Servers, MustChange: u.MustChange,
		Disabled: u.Disabled, Created: u.Created, CreatedBy: u.CreatedBy, LastLogin: u.LastLogin}
}

func (s *userStore) list() []userView {
	s.mu.RLock()
	out := make([]userView, 0, len(s.byID))
	for _, u := range s.byID {
		out = append(out, u.view())
	}
	s.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool { return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name) })
	return out
}

func (s *userStore) save() error {
	s.saveMu.Lock()
	defer s.saveMu.Unlock()
	s.mu.RLock()
	list := make([]*user, 0, len(s.byID))
	for _, v := range s.byID {
		list = append(list, v)
	}
	b, err := json.Marshal(list)
	s.mu.RUnlock()
	if err == nil {
		err = fsutil.WriteFile(s.path, b, 0o600)
	}
	if err != nil {
		log.Printf("saving users: %v", err)
	}
	return err
}

func (s *userStore) load() error {
	var list []*user
	if _, err := fsutil.ReadJSON(s.path, &list); err != nil {
		log.Printf("users: %v", err)
		return err
	}
	for _, v := range list {
		if v != nil && v.ID != "" && v.PasswordHash != "" {
			if v.Perms == nil {
				v.Perms = []string{permView}
			}
			if v.Servers == nil {
				v.Servers = []string{}
			}
			s.byID[v.ID] = v
		}
	}
	return nil
}

// --- management handlers (owner only) ---------------------------------------

func (a *App) serverExists(id string) bool {
	_, ok := a.cfg.Server(id)
	return ok
}

func (a *App) handleUsers(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{"users": a.users.list(), "max": maxUsers, "perms": userPerms})
}

func describeAccess(a *App, perms, servers []string) string {
	detail := "Can: " + strings.Join(perms, ", ") + "."
	if len(servers) > 0 {
		detail += " Limited to " + a.serverNames(servers) + "."
	} else {
		detail += " Every server."
	}
	return detail
}

// saveFailed tells the owner a change works now but would be lost at the next
// restart, which for a removal or a disable matters.
func saveFailed(w http.ResponseWriter, err error) {
	httpError(w, http.StatusInternalServerError, "the change applies now, but could not be saved, so it "+
		"would be undone when PZAdmin restarts. Check the data folder has space, then make the change again: "+
		err.Error())
}

func (a *App) handleUserCreate(w http.ResponseWriter, r *http.Request) {
	var p userRequest
	if !decodeJSON(w, r, &p) {
		return
	}
	u, temp, err := a.users.create(p, a.cfg.Get().Username, actor(r), a.serverExists)
	if err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}
	a.event(store.Event{Kind: "auth.user", Severity: store.SevWarn, Source: source(r), Actor: actor(r),
		Message: "User added: " + u.Name, Detail: describeAccess(a, u.Perms, u.Servers)})
	ok(w, map[string]any{"user": u.view(), "password": temp})
}

func (a *App) handleUserUpdate(w http.ResponseWriter, r *http.Request) {
	var p userRequest
	if !decodeJSON(w, r, &p) {
		return
	}
	perms, err := normalisePerms(p.Perms)
	if err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}
	servers, err := normaliseServers(p.Servers, a.serverExists)
	if err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}
	// Permissions are read on every request, so a change applies from the
	// user's next click without signing them out.
	u, err := a.users.change(p.ID, func(u *user) error {
		u.Perms, u.Servers = perms, servers
		return nil
	})
	if errors.Is(err, errNoSuchUser) {
		httpError(w, http.StatusNotFound, err.Error())
		return
	}
	a.event(store.Event{Kind: "auth.user", Severity: store.SevWarn, Source: source(r), Actor: actor(r),
		Message: "User changed: " + u.Name, Detail: describeAccess(a, u.Perms, u.Servers)})
	if err != nil {
		saveFailed(w, err)
		return
	}
	ok(w, map[string]any{"user": u.view()})
}

func (a *App) handleUserDisable(w http.ResponseWriter, r *http.Request) {
	var p struct {
		ID       string `json:"id"`
		Disabled bool   `json:"disabled"`
	}
	if !decodeJSON(w, r, &p) {
		return
	}
	u, err := a.users.change(p.ID, func(u *user) error {
		u.Disabled = p.Disabled
		return nil
	})
	if errors.Is(err, errNoSuchUser) {
		httpError(w, http.StatusNotFound, err.Error())
		return
	}
	msg := "User enabled: " + u.Name
	if p.Disabled {
		a.sess.revokeUser(u.ID)
		msg = "User disabled: " + u.Name
	}
	a.event(store.Event{Kind: "auth.user", Severity: store.SevWarn, Source: source(r), Actor: actor(r), Message: msg})
	if err != nil {
		saveFailed(w, err)
		return
	}
	ok(w, map[string]any{"user": u.view()})
}

func (a *App) handleUserReset(w http.ResponseWriter, r *http.Request) {
	var p struct {
		ID string `json:"id"`
	}
	if !decodeJSON(w, r, &p) {
		return
	}
	temp := temporaryPassword()
	u, err := a.users.change(p.ID, func(u *user) error {
		setUserPassword(u, temp)
		u.MustChange = true
		return nil
	})
	if errors.Is(err, errNoSuchUser) {
		httpError(w, http.StatusNotFound, err.Error())
		return
	}
	a.sess.revokeUser(u.ID)
	if err != nil {
		// The old password stops working when PZAdmin restarts either way,
		// but the new one would not survive a restart, so do not hand it out.
		saveFailed(w, err)
		return
	}
	a.event(store.Event{Kind: "auth.user", Severity: store.SevWarn, Source: source(r), Actor: actor(r),
		Message: "Password reset for " + u.Name, Detail: "They were signed out and must choose a new password."})
	ok(w, map[string]any{"user": u.view(), "password": temp})
}

func (a *App) handleUserRemove(w http.ResponseWriter, r *http.Request) {
	var p struct {
		ID string `json:"id"`
	}
	if !decodeJSON(w, r, &p) {
		return
	}
	u, err := a.users.remove(p.ID)
	if errors.Is(err, errNoSuchUser) {
		httpError(w, http.StatusNotFound, err.Error())
		return
	}
	a.sess.revokeUser(u.ID)
	a.event(store.Event{Kind: "auth.user", Severity: store.SevWarn, Source: source(r), Actor: actor(r),
		Message: "User removed: " + u.Name})
	if err != nil {
		saveFailed(w, err)
		return
	}
	ok(w, nil)
}
