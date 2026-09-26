package server

import (
	"context"
	"errors"
	"net/http"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"time"

	"github.com/TheWarBoys2/pzadmin/internal/arcane"
	"github.com/TheWarBoys2/pzadmin/internal/config"
	"github.com/TheWarBoys2/pzadmin/internal/stacks"
	"github.com/TheWarBoys2/pzadmin/internal/store"
)

// Servers are not added by hand. Each subfolder of the stacks root with a
// compose file is a server, and everything PZAdmin needs to know about it is
// read from that folder on every scan. What the operator sets in PZAdmin
// (display name, watchdog, backups, mod policy, schedules) is kept across
// scans; what the files say is overwritten from the files.

// discoveryInterval is how often the stacks root is rescanned. A scan reads
// a handful of small files per server, so this is cheap.
const discoveryInterval = time.Minute

func (a *App) discoveryLoop() {
	defer a.wg.Done()
	t := time.NewTicker(discoveryInterval)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			a.rescan()
		case <-a.stop:
			return
		}
	}
}

// rescan scans the stacks root and folds the result into the config.
func (a *App) rescan() stacks.Result {
	cfg := a.cfg.Get()
	res := stacks.Scan(cfg.StacksRoot, a.visible)

	a.discMu.Lock()
	a.lastScan = res
	a.discMu.Unlock()

	var added, missing []string
	changedIDs := map[string]bool{}
	updated, err := a.cfg.Update(func(c *config.Config) error {
		before := make(map[string]config.Server, len(c.Servers))
		for _, s := range c.Servers {
			before[s.ID] = s
		}

		seen := map[string]bool{}
		for _, st := range res.Stacks {
			if st.Container == "" {
				// Unreadable or ambiguous stacks are shown with their
				// problems but never turned into a server: there is no
				// container to act on.
				continue
			}
			idx := -1
			for i := range c.Servers {
				if c.Servers[i].Stack == st.Name {
					idx = i
					break
				}
			}
			if idx < 0 {
				c.Servers = append(c.Servers, config.Server{
					ID:       uniqueID(c.Servers, st.Name),
					Name:     st.Name,
					Enabled:  true,
					Recovery: config.DefaultRecovery(),
					Backup:   config.DefaultBackup(),
				})
				idx = len(c.Servers) - 1
				added = append(added, st.Name)
			}
			applyStack(&c.Servers[idx], st, c.PZRoot, a.rconHost, a.visible)
			seen[c.Servers[idx].ID] = true
		}
		for i := range c.Servers {
			s := &c.Servers[i]
			if !seen[s.ID] && !s.Missing {
				s.Missing = true
				missing = append(missing, s.Name)
			}
		}
		for _, s := range c.Servers {
			if prev, ok := before[s.ID]; !ok || !reflect.DeepEqual(prev, s) {
				changedIDs[s.ID] = true
			}
		}
		if len(changedIDs) == 0 {
			return errUnchanged
		}
		return nil
	})
	if err != nil && !errors.Is(err, errUnchanged) {
		res.Warnings = append(res.Warnings, "Could not save discovered servers: "+err.Error())
		return res
	}
	if len(changedIDs) == 0 {
		return res
	}
	_ = updated
	for id := range changedIDs {
		a.dropRCON(id)
	}
	a.syncMonitors()
	for _, name := range added {
		a.event(store.Event{Kind: "admin.action", Severity: store.SevInfo, Source: "system",
			Server: name, Message: "Found server " + name, Detail: "Read from its stack folder."})
	}
	for _, name := range missing {
		a.event(store.Event{Kind: "admin.action", Severity: store.SevWarn, Source: "system",
			Server: name, Message: "Stack folder for " + name + " has gone",
			Detail: "PZAdmin stops acting on it. Its history and schedules are kept."})
	}
	return res
}

// errUnchanged aborts a config update that would write nothing new.
var errUnchanged = errors.New("unchanged")

// applyStack copies everything the files say onto a server entry.
//
// Folders outside the managed folders are dropped rather than stored: with
// same-path mounts such a path would name something inside PZAdmin's own
// container, and nothing PZAdmin writes may land there.
func applyStack(s *config.Server, st stacks.Stack, dataRoot, rconHost string, visible stacks.Visible) {
	for _, dir := range []*string{&st.DataDir, &st.ConfigDir, &st.ServerDir} {
		if *dir != "" && (visible == nil || !visible(*dir)) {
			*dir = ""
		}
	}
	s.Missing = false
	s.Stack = st.Name
	s.StackDir = st.Dir
	s.ComposeFile = st.ComposeFile
	s.DockerContainer = st.Container
	s.ServerName = st.ServerName
	s.RestartPolicy = st.Restart
	s.DataDir = st.DataDir
	s.ConfigDir = st.ConfigDir
	s.ServerDir = st.ServerDir
	s.Host = rconHost
	s.RCONPort = st.RCONPort
	s.RCONPassword = st.RCONPassword
	s.GamePort = st.GamePort
	s.PZPath = serverBase(st, dataRoot)
}

// serverBase is the folder holding both data/ and config/, which is where
// PZAdmin's layout detection finds saves, logs, mods and the installation.
func serverBase(st stacks.Stack, dataRoot string) string {
	if st.DataDir != "" && st.ConfigDir != "" {
		if p := filepath.Dir(st.DataDir); p == filepath.Dir(st.ConfigDir) && sameOrUnder(dataRoot, p) {
			return p
		}
	}
	if st.ConfigDir != "" {
		return st.ConfigDir
	}
	return st.DataDir
}

var idUnsafe = regexp.MustCompile(`[^a-z0-9_-]+`)

// uniqueID makes a readable, stable ID from the stack folder name.
func uniqueID(existing []config.Server, name string) string {
	base := strings.Trim(idUnsafe.ReplaceAllString(strings.ToLower(name), "-"), "-")
	if base == "" {
		base = "server"
	}
	taken := map[string]bool{}
	for _, s := range existing {
		taken[s.ID] = true
	}
	id := base
	for n := 2; taken[id]; n++ {
		id = base + "-" + itoa(n)
	}
	return id
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

// stackFor re-reads one server's stack folder, for checks that must not
// trust a scan up to a minute old.
func (a *App) stackFor(srv config.Server) (stacks.Stack, bool) {
	if srv.StackDir == "" || srv.ComposeFile == "" {
		return stacks.Stack{}, false
	}
	return stacks.Inspect(srv.StackDir, srv.ComposeFile, a.visible), true
}

// sameOrUnder reports whether child is dir or sits inside it.
func sameOrUnder(dir, child string) bool {
	dir = filepath.Clean(dir)
	child = filepath.Clean(child)
	if dir == child {
		return true
	}
	return strings.HasPrefix(child, dir+string(filepath.Separator))
}

// --- HTTP --------------------------------------------------------------------

type stackView struct {
	stacks.Stack
	ServerID string `json:"serverId,omitempty"`
	Ready    bool   `json:"ready"`
}

func (a *App) stackPayload(res stacks.Result) map[string]any {
	cfg := a.cfg.Get()
	byStack := map[string]string{}
	for _, s := range cfg.Servers {
		if s.Stack != "" {
			byStack[s.Stack] = s.ID
		}
	}
	views := make([]stackView, 0, len(res.Stacks))
	for _, st := range res.Stacks {
		views = append(views, stackView{Stack: st, ServerID: byStack[st.Name], Ready: st.Ready()})
	}
	return map[string]any{
		"root":      res.Root,
		"dataRoot":  cfg.PZRoot,
		"stacks":    views,
		"warnings":  res.Warnings,
		"arcane":    a.arcaneState(),
		"rconHost":  a.rconHost,
		"scannedAt": time.Now().Format(time.RFC3339),
	}
}

// arcaneState describes container control for the interface.
func (a *App) arcaneState() map[string]any {
	up, err := a.arcane.Available()
	msg, detail := "", ""
	switch {
	case errors.Is(err, arcane.ErrNotConfigured):
		msg = "Arcane is not set up, so start, stop and deploy are off. Restarts, the console, mods, settings, " +
			"backups and schedules all work without it. To add it, set PZADMIN_ARCANE_URL, " +
			"PZADMIN_ARCANE_ENV_ID and PZADMIN_ARCANE_API_KEY in PZAdmin's docker-compose.yml."
	case err != nil:
		msg = "PZAdmin can't reach Arcane at " + a.arcane.URL() + ", so start, stop and deploy are off " +
			"for now. Check that Arcane is running and the address and API key are right. Restarts still " +
			"work over RCON."
		// The raw error helps with a support question, but it is not the
		// first thing to show.
		detail = err.Error()
	}
	return map[string]any{
		"configured":  a.arcane.Configured(),
		"available":   up,
		"environment": a.arcane.EnvironmentID(),
		"message":     msg,
		"detail":      detail,
	}
}

func (a *App) handleStack(w http.ResponseWriter, r *http.Request) {
	a.discMu.Lock()
	res := a.lastScan
	a.discMu.Unlock()
	if res.Root == "" {
		res = a.rescan()
	}
	writeJSON(w, a.stackPayload(res))
}

func (a *App) handleStackRescan(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, a.stackPayload(a.rescan()))
}

// startPreflight checks the bind sources a start would really mount. It
// prefers the container's actual mounts from Arcane, and falls back to the
// compose file when Arcane cannot say. notCreated is true when the container
// does not exist at all.
func (a *App) startPreflight(ctx context.Context, srv config.Server) (problems []string, drift string, notCreated bool) {
	d, err := a.arcane.Inspect(ctx, srv.DockerContainer)
	switch {
	case errors.Is(err, arcane.ErrNotFound):
		return nil, "", true
	case err == nil:
		for _, m := range d.Mounts {
			if m.Type != "bind" {
				continue
			}
			if problem := stacks.CheckSource(m.Source, a.visible); problem != "" {
				problems = append(problems, m.Destination+": "+problem)
			}
		}
		// Drift is not a reason to refuse: the container is internally
		// consistent. But the operator should know the file and the
		// container disagree.
		if st, ok := a.stackFor(srv); ok {
			drift = mountDrift(d.Mounts, st.Mounts)
		}
		return problems, drift, false
	}
	// Arcane could not inspect it; the start will fail anyway, but check the
	// compose file so a genuine mount problem is not hidden behind that.
	if st, ok := a.stackFor(srv); ok && !st.Ready() {
		return st.Problems, "", false
	}
	return nil, "", false
}

// mountDrift describes compose mounts that differ from the container's.
func mountDrift(actual []arcane.Mount, declared []stacks.Mount) string {
	have := map[string]string{}
	for _, m := range actual {
		have[filepath.Clean(m.Destination)] = filepath.Clean(m.Source)
	}
	var diffs []string
	for _, m := range declared {
		if m.Named {
			continue
		}
		if src, ok := have[m.Target]; ok && src != filepath.Clean(m.Source) {
			diffs = append(diffs, m.Target+" is "+src+" in the container but "+m.Source+" in the compose file")
		}
	}
	if len(diffs) == 0 {
		return ""
	}
	return "the compose file has changed since the container was created (" + strings.Join(diffs, "; ") +
		"); recreate it to apply"
}
