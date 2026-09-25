package server

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/TheWarBoys2/pzadmin/internal/compose"
	"github.com/TheWarBoys2/pzadmin/internal/config"
	"github.com/TheWarBoys2/pzadmin/internal/provision"
	"github.com/TheWarBoys2/pzadmin/internal/stacks"
	"github.com/TheWarBoys2/pzadmin/internal/store"
)

// createRequest is provision.Request with the clone source named by server
// ID rather than by path: the browser never gets to choose a folder to read.
type createRequest struct {
	provision.Request
	FromServerID string `json:"fromServerId,omitempty"`
}

// provisionEnv gathers what a plan needs from the current state.
func (a *App) provisionEnv() provision.Env {
	cfg := a.cfg.Get()
	a.discMu.Lock()
	existing := append([]stacks.Stack(nil), a.lastScan.Stacks...)
	a.discMu.Unlock()
	return provision.Env{
		StacksRoot: cfg.StacksRoot,
		DataRoot:   cfg.PZRoot,
		Image:      a.gameImage,
		Existing:   existing,
		Visible:    a.visible,
		PUID:       os.Getuid(),
		PGID:       os.Getgid(),
	}
}

// resolveRequest turns the browser's request into a provision.Request.
func (a *App) resolveRequest(in createRequest) (provision.Request, string) {
	req := in.Request
	if req.Start == provision.StartClone {
		src, found := a.cfg.Server(in.FromServerID)
		if !found || src.Missing || src.ServerDir == "" {
			return req, "pick an existing server to copy"
		}
		req.FromDir, req.FromName = src.ServerDir, src.ServerName
	}
	return req, ""
}

// handleStackWizardFields lists the settings a new server can be given
// before its first boot: the captured defaults, or a copy of another
// server's files.
func (a *App) handleStackWizardFields(w http.ResponseWriter, r *http.Request) {
	in := createRequest{FromServerID: r.URL.Query().Get("from")}
	in.Start = r.URL.Query().Get("start")
	req, problem := a.resolveRequest(in)
	if problem != "" {
		httpError(w, http.StatusBadRequest, problem)
		return
	}
	start, err := req.Starting()
	if err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}
	fields, err := provision.FieldsFor(start)
	if err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, fields)
}

// handleStackNew describes what a new server can be made from.
func (a *App) handleStackNew(w http.ResponseWriter, r *http.Request) {
	cfg := a.cfg.Get()
	type source struct {
		ServerID   string `json:"serverId"`
		Name       string `json:"name"`
		ServerName string `json:"serverName"`
	}
	var sources []source
	for _, s := range config.SortServers(cfg.Servers) {
		if s.Missing || s.ServerDir == "" {
			continue
		}
		if _, err := os.Stat(filepath.Join(s.ServerDir, s.ServerName+".ini")); err != nil {
			continue
		}
		sources = append(sources, source{ServerID: s.ID, Name: s.Name, ServerName: s.ServerName})
	}
	writeJSON(w, map[string]any{
		"image":       a.gameImage,
		"imagePinned": stacks.ImagePinned(a.gameImage),
		"stacksRoot":  cfg.StacksRoot,
		"dataRoot":    cfg.PZRoot,
		"sources":     sources,
		"stopGrace":   provision.StopGrace,
	})
}

func (a *App) planFromRequest(w http.ResponseWriter, r *http.Request) (*provision.Plan, bool) {
	var in createRequest
	if !decodeJSON(w, r, &in) {
		return nil, false
	}
	req, problem := a.resolveRequest(in)
	if problem != "" {
		httpError(w, http.StatusBadRequest, problem)
		return nil, false
	}
	// Plan against a fresh scan, so ports and folders taken a moment ago
	// count.
	a.rescan()
	plan, err := provision.Make(req, a.provisionEnv())
	if err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return nil, false
	}
	return plan, true
}

// handleStackPlan previews a new server without writing anything.
func (a *App) handleStackPlan(w http.ResponseWriter, r *http.Request) {
	plan, ok := a.planFromRequest(w, r)
	if !ok {
		return
	}
	writeJSON(w, map[string]any{"plan": plan.Masked()})
}

// handleStackCreate writes a new server's stack. The container itself is
// created by `docker compose up -d`, which the response spells out: Arcane
// has no endpoint for that. Until then the server's files can be edited like
// any other's, which is how settings and mods are set before the first boot.
func (a *App) handleStackCreate(w http.ResponseWriter, r *http.Request) {
	plan, ok := a.planFromRequest(w, r)
	if !ok {
		return
	}
	res, err := provision.Apply(plan)
	if err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	a.rescan()
	serverID := ""
	for _, s := range a.cfg.Get().Servers {
		if s.Stack == plan.Name {
			serverID = s.ID
		}
	}
	a.event(store.Event{Kind: "admin.action", Severity: store.SevInfo, Source: source(r), Actor: actor(r),
		ServerID: serverID, Server: plan.Name, Message: "Created stack " + plan.Name,
		Detail: "Not started yet. Run: " + plan.Command})
	writeJSON(w, map[string]any{
		"plan":     res.Plan.Masked(),
		"serverId": serverID,
		"command":  plan.Command,
		"message": "Written. Run the command on the host to create the container; the first start " +
			"downloads the game and any mods.",
	})
}

// handleStackEnv returns the editable part of a server's .env.
func (a *App) handleStackEnv(w http.ResponseWriter, r *http.Request) {
	srv, found := a.cfg.Server(r.URL.Query().Get("id"))
	if !found || srv.StackDir == "" {
		httpError(w, http.StatusNotFound, "no such server")
		return
	}
	fields, err := provision.EnvFields(srv.StackDir)
	if err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, map[string]any{"fields": fields, "command": recreateCommand(srv)})
}

// handleStackEnvSave edits a server's .env.
func (a *App) handleStackEnvSave(w http.ResponseWriter, r *http.Request) {
	var p struct {
		ServerID string            `json:"serverId"`
		Changes  map[string]string `json:"changes"`
	}
	if !decodeJSON(w, r, &p) {
		return
	}
	srv, found := a.cfg.Server(p.ServerID)
	if !found || srv.StackDir == "" || srv.Missing {
		httpError(w, http.StatusNotFound, "no such server")
		return
	}
	previous, changed, err := provision.EditEnv(srv.StackDir, p.Changes)
	if err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}
	if len(changed) == 0 {
		ok(w, map[string]any{"changed": []string{}, "message": "Nothing changed."})
		return
	}
	backupDir := filepath.Join(a.backup.Dir(srv.ID), "env")
	if err := os.MkdirAll(backupDir, 0o700); err == nil {
		_ = os.WriteFile(filepath.Join(backupDir, ".env-"+time.Now().UTC().Format("20060102-150405")), previous, 0o600)
	}
	a.rescan()
	a.event(store.Event{Kind: "config.edit", Severity: store.SevWarn, Source: source(r), Actor: actor(r),
		ServerID: srv.ID, Server: srv.Name, Message: "Edited .env: " + strings.Join(changed, ", "),
		Detail: "Takes effect when the container is recreated. The previous file was kept."})
	ok(w, map[string]any{
		"changed": changed,
		"command": recreateCommand(srv),
		"message": "Saved. The running container still uses the old values until it is recreated: " + recreateCommand(srv),
	})
}

func recreateCommand(s config.Server) string {
	return "cd " + s.StackDir + " && docker compose up -d"
}

// lockedKeys returns the ini keys a server's .env owns, for its main ini only.
func (a *App) lockedKeys(srv config.Server, file string) map[string]string {
	if srv.StackDir == "" || srv.ServerName == "" || !strings.EqualFold(file, srv.ServerName+".ini") {
		return nil
	}
	env, err := compose.ReadEnvFile(filepath.Join(srv.StackDir, ".env"))
	if err != nil {
		return nil
	}
	return provision.LockedINIKeys(env.Map())
}
