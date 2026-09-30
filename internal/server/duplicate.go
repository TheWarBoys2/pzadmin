package server

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/TheWarBoys2/pzadmin/internal/compose"
	"github.com/TheWarBoys2/pzadmin/internal/config"
	"github.com/TheWarBoys2/pzadmin/internal/provision"
	"github.com/TheWarBoys2/pzadmin/internal/pz"
	"github.com/TheWarBoys2/pzadmin/internal/store"
)

// Duplicating a server: a new stack that is a full copy of an existing one,
// world, characters, accounts and all, on its own ports and folders.
//
// Unlike a template or "Copy an existing server", the copy keeps the source's
// ResetID, ServerPlayerID and Seed. Players' characters in the copied world
// belong to those IDs, and the map to that seed, so new ones would make
// everybody start again.
//
// The world must not change while it is read, so the source must be stopped
// and stays held (no backup, restore, reset, start or deploy through PZAdmin)
// until the copy is done. The copy runs in the background; the browser polls
// its progress. Its new folders are only written as a stack once the world
// has been copied in full, so a failed or half-done copy is never discovered
// as a server, and whatever it created is removed.

// dupSkip are the top-level folders of a server's config folder a copy leaves
// out: logs are the source's history, Server/ comes from the stack folder,
// and backups/ is the game's own start-up backups, which can be large.
var dupSkip = map[string]bool{"Logs": true, "Server": true, "backups": true}

// dupStatus is a copy's progress, as the browser sees it.
type dupStatus struct {
	ID       string    `json:"id"`
	SourceID string    `json:"sourceId"`
	Source   string    `json:"source"`
	Name     string    `json:"name"`
	State    string    `json:"state"` // copying | done | failed
	Total    int64     `json:"total"`
	Copied   int64     `json:"copied"`
	Error    string    `json:"error,omitempty"`
	ServerID string    `json:"serverId,omitempty"`
	Command  string    `json:"command,omitempty"`
	Started  time.Time `json:"started"`
}

type dupJob struct {
	dupStatus
	copied atomic.Int64
}

type dupJobs struct {
	mu   sync.Mutex
	jobs map[string]*dupJob
	// busy maps a source server ID to the job copying it.
	busy map[string]string
}

func newDupJobs() *dupJobs {
	return &dupJobs{jobs: map[string]*dupJob{}, busy: map[string]string{}}
}

// copying reports whether a server's world is being copied right now.
func (d *dupJobs) copying(serverID string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.busy[serverID] != ""
}

func (d *dupJobs) snapshot(id string) (dupStatus, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	j, found := d.jobs[id]
	if !found {
		return dupStatus{}, false
	}
	out := j.dupStatus
	out.Copied = j.copied.Load()
	return out, true
}

func (d *dupJobs) finish(j *dupJob, state, errText, serverID, command string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	j.State, j.Error, j.ServerID, j.Command = state, errText, serverID, command
	delete(d.busy, j.SourceID)
	// Finished jobs are kept a while for the browser to read, then dropped.
	for id, old := range d.jobs {
		if old.State != "copying" && time.Since(old.Started) > 24*time.Hour {
			delete(d.jobs, id)
		}
	}
}

// dupRefusal says why a server cannot be started or deployed right now
// because it is being copied, or "".
func (a *App) dupRefusal(serverID, name string) string {
	if a.dups.copying(serverID) {
		return name + " is being duplicated. It stays stopped until the copy finishes, so the copy is not half " +
			"old world, half new."
	}
	return ""
}

// dupSource finds a server that can be duplicated, and its config folder.
func (a *App) dupSource(id string) (config.Server, string, string) {
	srv, found := a.cfg.Server(id)
	if !found || srv.Missing || srv.StackDir == "" || srv.ServerDir == "" || srv.ServerName == "" {
		return srv, "", "no such server"
	}
	st, ok := a.stackFor(srv)
	if !ok || st.ConfigDir == "" {
		return srv, "", srv.Name + " has no config folder mounted, so there is no world to copy"
	}
	if _, err := os.Stat(st.ConfigDir); err != nil {
		return srv, "", "PZAdmin cannot see " + st.ConfigDir + ": " + err.Error()
	}
	return srv, st.ConfigDir, ""
}

// handleDuplicateCheck says what a duplicate would copy and whether it can
// go ahead now.
func (a *App) handleDuplicateCheck(w http.ResponseWriter, r *http.Request) {
	srv, configDir, problem := a.dupSource(r.URL.Query().Get("id"))
	if problem != "" {
		httpError(w, http.StatusNotFound, problem)
		return
	}
	bytes, files, truncated := copySize(configDir)
	info := pz.World(detectLayout(srv))
	out := map[string]any{
		"serverId":   srv.ID,
		"name":       srv.Name,
		"serverName": srv.ServerName,
		"configDir":  configDir,
		"bytes":      bytes,
		"files":      files,
		"truncated":  truncated,
		"worlds":     info.Worlds,
		"useSlot":    srv.UseSlot,
		"inUse":      a.worldInUse(r.Context(), srv),
		"copying":    a.dups.copying(srv.ID),
	}
	if env, err := compose.ReadEnvFile(filepath.Join(srv.StackDir, ".env")); err == nil {
		out["adminUsername"] = env.Map()["ADMIN_USERNAME"]
		out["adminPasswordOk"] = provision.ValidAdminPassword(env.Map()["ADMIN_PASSWORD"]) == nil
	}
	writeJSON(w, out)
}

// copySize counts what a duplicate copies.
func copySize(configDir string) (int64, int, bool) {
	var total int64
	var count int
	items, err := os.ReadDir(configDir)
	if err != nil {
		return 0, 0, false
	}
	truncated := false
	for _, it := range items {
		if dupSkip[it.Name()] {
			continue
		}
		p := filepath.Join(configDir, it.Name())
		if !it.IsDir() {
			if fi, err := it.Info(); err == nil && fi.Mode().IsRegular() {
				total += fi.Size()
				count++
			}
			continue
		}
		b, n, t := pz.DirSize(p, 2000000)
		total, count, truncated = total+b, count+n, truncated || t
	}
	return total, count, truncated
}

type duplicateRequest struct {
	ServerID      string `json:"serverId"`
	Name          string `json:"name"`
	ServerName    string `json:"serverName"`
	AdminPassword string `json:"adminPassword"`
}

// handleDuplicate plans the copy, checks it can finish, and starts it.
func (a *App) handleDuplicate(w http.ResponseWriter, r *http.Request) {
	var in duplicateRequest
	if !decodeJSON(w, r, &in) {
		return
	}
	src, configDir, problem := a.dupSource(in.ServerID)
	if problem != "" {
		httpError(w, http.StatusNotFound, problem)
		return
	}
	if msg := a.worldInUse(r.Context(), src); msg != "" {
		httpError(w, http.StatusConflict, msg+" A copy of a running world can be half old, half new.")
		return
	}

	env, err := compose.ReadEnvFile(filepath.Join(src.StackDir, ".env"))
	if err != nil {
		httpError(w, http.StatusInternalServerError, "reading "+src.Name+"'s .env: "+err.Error())
		return
	}
	vars := env.Map()
	req := provision.Request{
		Name:          in.Name,
		ServerName:    in.ServerName,
		Start:         provision.StartDuplicate,
		FromDir:       src.ServerDir,
		FromName:      src.ServerName,
		AdminUsername: vars["ADMIN_USERNAME"],
		AdminPassword: vars["ADMIN_PASSWORD"],
	}
	if in.AdminPassword != "" {
		req.AdminPassword = in.AdminPassword
	}
	if provision.ValidAdminPassword(req.AdminPassword) != nil && in.AdminPassword == "" {
		httpError(w, http.StatusBadRequest, src.Name+"'s .env has no usable ADMIN_PASSWORD, so choose one for the copy")
		return
	}
	if n, err := strconv.Atoi(vars["MAX_PLAYERS"]); err == nil {
		req.MaxPlayers = n
	}
	if n, err := strconv.Atoi(vars["MEMORY_XMX_GB"]); err == nil {
		req.MemoryGB = n
	}
	if b, err := strconv.ParseBool(vars["UPDATE_ON_START"]); err == nil {
		req.UpdateOnStart = &b
	}

	a.rescan()
	penv := a.provisionEnv()
	plan, err := provision.Make(req, penv)
	if err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}
	if len(plan.Reuses) > 0 {
		httpError(w, http.StatusBadRequest, strings.Join(plan.Reuses, " and ")+
			" already has files in it. A duplicate needs empty folders; choose another stack name.")
		return
	}

	total, _, _ := copySize(configDir)
	if free := pz.FreeBytes(nearestExisting(plan.ConfigDir)); free >= 0 && total+total/10 > free {
		httpError(w, http.StatusInsufficientStorage, fmt.Sprintf("The copy needs about %s and only %s is free where "+
			"it would go. Free some space first.", humanBytes(total+total/10), humanBytes(free)))
		return
	}

	release, err := a.backup.Hold(src.ID)
	if err != nil {
		httpError(w, http.StatusConflict, "a backup, restore or reset is running for "+src.Name+"; wait for it to finish")
		return
	}
	buf := make([]byte, 8)
	_, _ = rand.Read(buf)
	job := &dupJob{dupStatus: dupStatus{ID: hex.EncodeToString(buf), SourceID: src.ID, Source: src.Name,
		Name: plan.Name, State: "copying", Total: total, Started: time.Now()}}
	a.dups.mu.Lock()
	if a.dups.busy[src.ID] != "" {
		a.dups.mu.Unlock()
		release()
		httpError(w, http.StatusConflict, src.Name+" is already being duplicated")
		return
	}
	a.dups.busy[src.ID] = job.ID
	a.dups.jobs[job.ID] = job
	a.dups.mu.Unlock()

	a.event(store.Event{Kind: "admin.action", Severity: store.SevInfo, Source: source(r), Actor: actor(r),
		ServerID: src.ID, Server: src.Name, Message: "Duplicating " + src.Name + " as " + plan.Name,
		Detail: fmt.Sprintf("Copying about %s from %s. %s stays stopped until it finishes.", humanBytes(total),
			configDir, src.Name)})
	go a.runDuplicate(job, release, src, configDir, plan, source(r), actor(r))
	writeStatusJSON(w, http.StatusAccepted, map[string]any{"job": job.ID,
		"message": "Copying " + src.Name + "'s world. This can take a while for a big world."})
}

// handleDuplicateStatus reports a copy's progress.
func (a *App) handleDuplicateStatus(w http.ResponseWriter, r *http.Request) {
	job, found := a.dups.snapshot(r.URL.Query().Get("job"))
	if !found {
		httpError(w, http.StatusNotFound, "no such copy; PZAdmin may have restarted since it began")
		return
	}
	writeJSON(w, job)
}

func (a *App) runDuplicate(job *dupJob, release func(), src config.Server, configDir string, plan *provision.Plan, src0, who string) {
	defer release()
	created := topCreated(plan.ConfigDir, a.cfg.Get().PZRoot)
	fail := func(err error) {
		if created != "" {
			_ = os.RemoveAll(created)
		}
		msg := err.Error()
		var denied *os.PathError
		if errors.As(err, &denied) && errors.Is(err, fs.ErrPermission) {
			msg = fmt.Sprintf("PZAdmin could not read %s: permission denied. The game server wrote its files as a "+
				"different user from PZAdmin (often root). Run this on the host, then try again: sudo chown -R %d:%d %q",
				denied.Path, os.Getuid(), os.Getgid(), configDir)
		}
		a.dups.finish(job, "failed", msg, "", "")
		a.event(store.Event{Kind: "admin.action", Severity: store.SevError, Source: src0, Actor: who,
			ServerID: src.ID, Server: src.Name, Message: "Duplicating " + src.Name + " failed",
			Detail: msg + " Nothing was left behind; " + src.Name + " is unchanged."})
	}

	if err := os.MkdirAll(plan.ConfigDir, 0o755); err != nil {
		fail(err)
		return
	}
	if err := copyWorld(configDir, plan.ConfigDir, src.ServerName, plan.ServerName, &job.copied); err != nil {
		fail(err)
		return
	}
	if _, err := provision.Apply(plan); err != nil {
		fail(err)
		return
	}
	a.rescan()
	newID := ""
	for _, s := range a.cfg.Get().Servers {
		if s.Stack == plan.Name {
			newID = s.ID
		}
	}
	if newID != "" && src.UseSlot {
		_, _ = a.cfg.Update(func(c *config.Config) error {
			for i := range c.Servers {
				if c.Servers[i].ID == newID {
					c.Servers[i].UseSlot = true
				}
			}
			return nil
		})
	}
	a.dups.finish(job, "done", "", newID, plan.Command)
	a.event(store.Event{Kind: "admin.action", Severity: store.SevInfo, Source: src0, Actor: who,
		ServerID: newID, Server: plan.Name, Message: "Duplicated " + src.Name + " as " + plan.Name,
		Detail: fmt.Sprintf("Copied %s of world, characters and accounts. Not started yet. Run: %s",
			humanBytes(job.copied.Load()), plan.Command)})
}

// copyWorld copies a server's config folder, less dupSkip, into dst. When
// the SERVER_NAME changes, the world folder in Saves/Multiplayer and the
// accounts database in db/ are renamed to match, as the game looks them up
// by that name.
func copyWorld(src, dst, oldName, newName string, copied *atomic.Int64) error {
	rename := func(rel string) string {
		if oldName == newName {
			return rel
		}
		parts := strings.Split(rel, string(filepath.Separator))
		switch {
		case len(parts) >= 3 && parts[0] == "Saves" && parts[1] == "Multiplayer":
			if parts[2] == oldName || strings.HasPrefix(parts[2], oldName+"_") {
				parts[2] = newName + strings.TrimPrefix(parts[2], oldName)
			}
		case len(parts) == 2 && parts[0] == "db":
			if strings.HasPrefix(parts[1], oldName+".") {
				parts[1] = newName + strings.TrimPrefix(parts[1], oldName)
			}
		}
		return filepath.Join(parts...)
	}
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil || rel == "." {
			return err
		}
		if !strings.Contains(rel, string(filepath.Separator)) && dupSkip[rel] {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		target := filepath.Join(dst, rename(rel))
		info, err := d.Info()
		if err != nil {
			return err
		}
		switch {
		case d.IsDir():
			return os.MkdirAll(target, 0o755)
		case info.Mode()&fs.ModeSymlink != 0:
			link, err := os.Readlink(path)
			if err != nil {
				return err
			}
			return os.Symlink(link, target)
		case info.Mode().IsRegular():
			return copyFile(path, target, info.Mode().Perm(), copied)
		}
		return nil // sockets, pipes and devices have no place in a world
	})
}

func copyFile(src, dst string, perm fs.FileMode, copied *atomic.Int64) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm|0o600)
	if err != nil {
		return err
	}
	_, err = io.Copy(out, &countingReader{r: in, n: copied})
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	return err
}

type countingReader struct {
	r io.Reader
	n *atomic.Int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n.Add(int64(n))
	return n, err
}

// nearestExisting returns dir or its closest ancestor that exists, for
// asking how much space is free there.
func nearestExisting(dir string) string {
	for d := filepath.Clean(dir); ; d = filepath.Dir(d) {
		if _, err := os.Stat(d); err == nil || d == filepath.Dir(d) {
			return d
		}
	}
}

// topCreated returns the highest folder above and including dir, below
// root, that does not exist yet: what to remove if the copy fails.
func topCreated(dir, root string) string {
	top := ""
	for d := filepath.Clean(dir); sameOrUnder(root, d) && d != filepath.Clean(root); d = filepath.Dir(d) {
		if _, err := os.Lstat(d); err == nil {
			break
		}
		top = d
	}
	return top
}
