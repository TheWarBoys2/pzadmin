package server

import (
	"context"
	"errors"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/TheWarBoys2/pzadmin/internal/arcane"
	"github.com/TheWarBoys2/pzadmin/internal/config"
	"github.com/TheWarBoys2/pzadmin/internal/store"
)

// Arcane scans its projects directory, nested folders included, so every
// stack folder is an Arcane project, including one PZAdmin has just written
// and nobody has started. Deploying it through Arcane is `docker compose up
// -d` without a terminal: it creates the container the first time, and
// recreates it after the compose file or .env changes.
//
// Arcane runs Compose inside its own container, where the stack folder has a
// different path. That is harmless for absolute bind sources and fatal for
// relative ones, which would resolve inside Arcane's path and have Docker
// create empty folders on the host. Relative sources are already refused by
// discovery; the deploy is refused whenever the stack is not ready.

// deployTimeout bounds one deploy. Pulling a missing game image is the slow
// part; the game itself downloads after the container starts.
const deployTimeout = 20 * time.Minute

// firstStartWindow is how long a server may take to come up after a deploy
// before it counts as an outage: a first start downloads the game and mods.
const firstStartWindow = 60 * time.Minute

// projectMatches reports whether an Arcane project is a server's stack
// folder. The folder name must match, and the path Arcane reports must end in
// the stacks folder's own name and the stack's, so a same-named project
// elsewhere under Arcane is never taken for it.
func projectMatches(p arcane.Project, stacksRoot, stack string) bool {
	if stack == "" || p.DirName != stack {
		return false
	}
	rel := strings.Trim(filepath.ToSlash(p.RelativePath), "/")
	base := filepath.Base(stacksRoot)
	return rel == stack || rel == base+"/"+stack || strings.HasSuffix(rel, "/"+base+"/"+stack)
}

// allowProject is the project allowlist: only stacks of discovered servers.
func (a *App) allowProject(p arcane.Project) bool {
	cfg := a.cfg.Get()
	for _, s := range cfg.Servers {
		if !s.Missing && projectMatches(p, cfg.StacksRoot, s.Stack) {
			return true
		}
	}
	return false
}

// findProject locates a server's project, waiting briefly for Arcane to
// notice a folder written moments ago.
func (a *App) findProject(ctx context.Context, srv config.Server) (arcane.Project, error) {
	root := a.cfg.Get().StacksRoot
	deadline := time.Now().Add(45 * time.Second)
	for {
		list, err := a.arcane.Projects(ctx)
		if err != nil {
			return arcane.Project{}, err
		}
		var found []arcane.Project
		for _, p := range list {
			if projectMatches(p, root, srv.Stack) {
				found = append(found, p)
			}
		}
		switch {
		case len(found) == 1:
			return found[0], nil
		case len(found) > 1:
			return arcane.Project{}, errors.New("Arcane lists more than one project for " + srv.Stack)
		}
		if time.Now().After(deadline) {
			return arcane.Project{}, errors.New("Arcane does not list " + srv.StackDir + " as a project. " +
				"Its projects directory must contain the stacks folder")
		}
		select {
		case <-ctx.Done():
			return arcane.Project{}, ctx.Err()
		case <-time.After(3 * time.Second):
		}
	}
}

// handleStackDeploy runs docker compose up for a server through Arcane. It
// answers at once; the result arrives as an event, because a first deploy
// that pulls the image can take minutes.
func (a *App) handleStackDeploy(w http.ResponseWriter, r *http.Request) {
	var p struct {
		ServerID string `json:"serverId"`
	}
	if !decodeJSON(w, r, &p) {
		return
	}
	srv, found := a.cfg.Server(p.ServerID)
	if !found || srv.Missing || srv.StackDir == "" {
		httpError(w, http.StatusNotFound, "no such server")
		return
	}
	if !a.arcane.Configured() {
		httpError(w, http.StatusConflict, "Arcane is not configured. Run this on the host instead: "+recreateCommand(srv))
		return
	}
	// Compose uses the file as it is now, so check the file as it is now.
	if st, ok := a.stackFor(srv); !ok || !st.Ready() {
		problems := []string{"its stack folder could not be read"}
		if ok {
			problems = st.Problems
		}
		writeStatusJSON(w, http.StatusConflict, map[string]any{
			"error":    "Not deploying " + srv.Name + ": " + strings.Join(problems, "; "),
			"problems": problems,
		})
		return
	}
	if a.statusOf(srv.ID).Deploying {
		httpError(w, http.StatusConflict, srv.Name+" is already being deployed")
		return
	}

	who := actor(r)
	a.updateStatus(srv.ID, func(st *Status) { st.Deploying = true; st.Stopped = false })
	a.event(store.Event{Kind: "admin.action", Severity: store.SevInfo, Source: "ui", Actor: who,
		ServerID: srv.ID, Server: srv.Name, Message: "Deploying " + srv.Name,
		Detail: "docker compose up -d through Arcane."})

	started := a.spawn(func() {
		defer a.updateStatus(srv.ID, func(st *Status) { st.Deploying = false })
		ctx, cancel := context.WithTimeout(a.ctx, deployTimeout)
		defer cancel()

		project, err := a.findProject(ctx, srv)
		output := ""
		if err == nil {
			// A server that has never booted downloads the game on its
			// first start, which easily outlasts the usual window.
			window := restartWindow
			if st, ok := a.stackFor(srv); ok && (!st.Configured || !st.Installed) {
				window = firstStartWindow
			}
			a.markRestartingFor(srv.ID, "deploying", window)
			output, err = a.arcane.Deploy(ctx, project)
		}
		if err != nil {
			a.clearRestarting(srv.ID)
			a.event(store.Event{Kind: "admin.action", Severity: store.SevError, Source: "ui", Actor: who,
				ServerID: srv.ID, Server: srv.Name, Message: "Deploying " + srv.Name + " failed",
				Detail: err.Error() + tailNote(output)})
			return
		}
		a.rescan()
		a.event(store.Event{Kind: "admin.action", Severity: store.SevSuccess, Source: "ui", Actor: who,
			ServerID: srv.ID, Server: srv.Name, Message: srv.Name + " deployed",
			Detail: "Compose finished. The server is starting; a first start downloads the game and mods." +
				tailNote(output)})
	})
	if !started {
		a.updateStatus(srv.ID, func(st *Status) { st.Deploying = false })
		httpError(w, http.StatusServiceUnavailable, "PZAdmin is shutting down")
		return
	}

	ok(w, map[string]any{"message": "Deploying " + srv.Name + " through Arcane. The result appears in Activity."})
}

func tailNote(output string) string {
	output = strings.TrimSpace(output)
	if output == "" {
		return ""
	}
	return "\n\n" + output
}
