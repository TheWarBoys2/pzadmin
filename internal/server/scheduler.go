package server

import (
	"context"
	"fmt"
	"math/rand"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/TheWarBoys2/pzadmin/internal/config"
	"github.com/TheWarBoys2/pzadmin/internal/cronx"
	"github.com/TheWarBoys2/pzadmin/internal/notify"
	"github.com/TheWarBoys2/pzadmin/internal/pz"
	"github.com/TheWarBoys2/pzadmin/internal/store"
)

// scheduler state: which task/minute combinations have already fired.
type fireLog struct {
	mu    sync.Mutex
	fired map[string]time.Time
}

func newFireLog() *fireLog { return &fireLog{fired: map[string]time.Time{}} }

// claim returns true the first time a key is seen for a given minute.
func (f *fireLog) claim(key string, minute time.Time) bool {
	stamp := minute.Format("2006-01-02T15:04")
	k := key + "@" + stamp
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, seen := f.fired[k]; seen {
		return false
	}
	f.fired[k] = time.Now()
	// Forget anything older than two hours so the map cannot grow forever.
	if len(f.fired) > 2000 {
		cutoff := time.Now().Add(-2 * time.Hour)
		for key, at := range f.fired {
			if at.Before(cutoff) {
				delete(f.fired, key)
			}
		}
	}
	return true
}

// schedulerLoop wakes on every minute boundary in the configured timezone.
//
// Ticking on the boundary rather than every thirty seconds means a task fires
// once, promptly, without needing a dedupe window wide enough to hide drift.
func (a *App) schedulerLoop() {
	defer a.wg.Done()
	fired := newFireLog()

	for {
		loc := a.cfg.Location()
		now := time.Now().In(loc)
		next := now.Truncate(time.Minute).Add(time.Minute).Add(500 * time.Millisecond)
		select {
		case <-time.After(time.Until(next)):
			a.runDueTasks(time.Now().In(loc), fired)
		case <-a.stop:
			return
		}
	}
}

func (a *App) runDueTasks(now time.Time, fired *fireLog) {
	cfg := a.cfg.Get()
	for _, task := range cfg.Schedules {
		if !task.Enabled || task.Cron == "" {
			continue
		}
		sched, err := cronx.Parse(task.Cron)
		if err != nil {
			continue
		}
		srv, ok := serverByID(cfg.Servers, task.ServerID)
		if !ok {
			continue
		}

		// Any job can announce a countdown; a restart is only the usual reason.
		if len(task.WarnMinutes) > 0 {
			for _, m := range task.WarnMinutes {
				if m <= 0 {
					continue
				}
				future := now.Add(time.Duration(m) * time.Minute)
				if sched.Match(future) && fired.claim(fmt.Sprintf("%s-warn-%d", task.ID, m), now) {
					a.announce(srv, warningText(task, m))
				}
			}
		}

		if !sched.Match(now) {
			continue
		}
		if !fired.claim(task.ID+"-run", now) {
			continue
		}
		go a.runTask(task, srv)
	}
}

func serverByID(list []config.Server, id string) (config.Server, bool) {
	for _, s := range list {
		if s.ID == id {
			return s, true
		}
	}
	return config.Server{}, false
}

// runTask executes a job's steps in order and records the outcome, so the
// schedules screen can show whether the last run actually worked.
func (a *App) runTask(task config.Task, srv config.Server) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	results, err := a.executeSteps(ctx, task, srv)
	outcome := strings.Join(results, "; ")
	sev := store.SevInfo
	if err != nil {
		outcome = strings.TrimSpace(outcome + " — failed: " + err.Error())
		sev = store.SevError
	}
	if outcome == "" {
		outcome = "nothing to do"
	}

	_, _ = a.cfg.Update(func(c *config.Config) error {
		for i := range c.Schedules {
			if c.Schedules[i].ID == task.ID {
				c.Schedules[i].LastRun = time.Now().Format(time.RFC3339)
				c.Schedules[i].LastResult = truncate(outcome, 300)
			}
		}
		return nil
	})

	a.event(store.Event{
		Kind: "schedule.run", Severity: sev, Source: "schedule",
		ServerID: srv.ID, Server: srv.Name,
		Message: fmt.Sprintf("Scheduled job %q ran", task.Name),
		Detail:  outcome,
		Meta:    map[string]any{"taskId": task.ID, "steps": len(task.Steps)},
	})
}

// executeSteps runs each step in turn, stopping at the first failure unless the
// step is marked to continue. It returns one summary line per step attempted,
// so a failure halfway through a job still says what did happen.
func (a *App) executeSteps(ctx context.Context, task config.Task, srv config.Server) ([]string, error) {
	var results []string
	for i, step := range task.Steps {
		label := step.Note
		if label == "" {
			label = fmt.Sprintf("step %d (%s)", i+1, step.Kind)
		}
		result, err := a.executeStep(ctx, step, srv)
		if err != nil {
			results = append(results, label+": failed")
			if step.ContinueOnError {
				continue
			}
			return results, fmt.Errorf("%s: %w", label, err)
		}
		results = append(results, label+": "+result)
		if ctx.Err() != nil {
			return results, ctx.Err()
		}
	}
	return results, nil
}

func (a *App) executeStep(ctx context.Context, step config.Step, srv config.Server) (string, error) {
	client := a.rconFor(srv)
	switch step.Kind {
	case "wait":
		seconds := step.Seconds
		if seconds < 0 {
			seconds = 0
		}
		if seconds > 3600 {
			seconds = 3600
		}
		select {
		case <-time.After(time.Duration(seconds) * time.Second):
		case <-ctx.Done():
			return "", ctx.Err()
		}
		return fmt.Sprintf("waited %ds", seconds), nil

	case "save":
		out, err := client.Exec("save")
		if err != nil {
			return "", err
		}
		return "world saved" + suffix(out), nil

	case "broadcast":
		msg := strings.TrimSpace(step.Message)
		if msg == "" {
			return "", fmt.Errorf("this broadcast has no message")
		}
		if _, err := client.Exec("servermsg " + quote(msg)); err != nil {
			return "", err
		}
		return "broadcast sent", nil

	case "restart":
		if err := a.restartServer(ctx, srv, "schedule", "scheduled job", ""); err != nil {
			return "", err
		}
		return "restart issued", nil

	case "backup":
		layout := detectLayout(srv)
		keep := srv.Backup.Keep
		if keep <= 0 {
			keep = 10
		}
		res, err := a.backup.Create(srv.ID, layout, srv.Backup.IncludeConfig, keep, "scheduled")
		if err != nil {
			a.event(store.Event{
				Kind: "backup.failed", Severity: store.SevError, Source: "schedule",
				ServerID: srv.ID, Server: srv.Name,
				Message: "Backup of " + srv.Name + " failed", Detail: err.Error(),
			})
			return "", err
		}
		a.event(store.Event{
			Kind: "backup.done", Severity: store.SevSuccess, Source: "schedule",
			ServerID: srv.ID, Server: srv.Name,
			Message: fmt.Sprintf("Backed up %s (%s)", srv.Name, humanBytes(res.Archive.Size)),
			Detail:  fmt.Sprintf("%d files in %s.", res.Archive.Files, res.Duration.Round(time.Second)),
		})
		return fmt.Sprintf("archived %d files, %s", res.Archive.Files, humanBytes(res.Archive.Size)), nil

	case "discord":
		h := webhookByID(a.cfg.Get(), step.Webhook)
		switch {
		case h == nil:
			return "", fmt.Errorf("the Discord channel this posts to has been removed")
		case !h.Enabled || h.URL == "":
			return "", fmt.Errorf("the Discord channel %q is switched off", h.Name)
		}
		text := strings.NewReplacer(
			"{server}", srv.Name,
			"{players}", strconv.Itoa(a.statusOf(srv.ID).PlayerCount),
		).Replace(step.Message)
		dctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		if err := a.notify.Announce(dctx, notify.Destination{URL: h.URL, Audience: h.Audience}, text); err != nil {
			return "", err
		}
		return "posted to " + h.Name, nil

	case "command":
		line, err := ValidateRawCommand(step.Command)
		if err != nil {
			return "", err
		}
		out, err := client.Exec(line)
		if err != nil {
			return "", err
		}
		return "ran " + line + suffix(out), nil

	case "action":
		return a.executeActionStep(step, srv)
	}
	return "", fmt.Errorf("unknown step type %q", step.Kind)
}

// executeActionStep runs a catalogue command, expanding the placeholders that
// make a scheduled job useful: a player argument can mean every player online
// or a random one, and an item argument can mean a random pick from a category.
func (a *App) executeActionStep(step config.Step, srv config.Server) (string, error) {
	cmd, found := LookupCommand(step.Action)
	if !found {
		return "", fmt.Errorf("unknown command %q", step.Action)
	}

	playerIndex := -1
	for i, p := range cmd.Params {
		if p.Type == ParamPlayer {
			playerIndex = i
			break
		}
	}

	online := a.statusOf(srv.ID).Players
	targets := []string{""}
	if playerIndex >= 0 && playerIndex < len(step.Args) {
		switch strings.ToLower(strings.TrimSpace(step.Args[playerIndex])) {
		case "@each":
			if len(online) == 0 {
				return "nobody online, skipped", nil
			}
			targets = append([]string(nil), online...)
		case "@random":
			if len(online) == 0 {
				return "nobody online, skipped", nil
			}
			targets = []string{online[rand.Intn(len(online))]}
		default:
			targets = []string{step.Args[playerIndex]}
		}
	}

	client := a.rconFor(srv)
	done := 0
	var lastOut string
	for _, target := range targets {
		args, err := a.resolveArgs(cmd, step.Args, target, playerIndex, srv)
		if err != nil {
			return "", err
		}
		line, err := cmd.Build(args)
		if err != nil {
			return "", err
		}
		if line, err = a.resolveVerb(srv, cmd, line); err != nil {
			return "", err
		}
		out, err := client.Exec(line)
		if err != nil {
			return "", err
		}
		lastOut = out
		done++
	}

	if playerIndex >= 0 && done > 1 {
		return fmt.Sprintf("%s for %d players", cmd.Label, done), nil
	}
	return strings.ToLower(cmd.Label) + suffix(lastOut), nil
}

// resolveArgs substitutes the run-time placeholders into a step's arguments.
func (a *App) resolveArgs(cmd Command, args []string, target string, playerIndex int, srv config.Server) ([]string, error) {
	out := append([]string(nil), args...)
	for len(out) < len(cmd.Params) {
		out = append(out, "")
	}
	if playerIndex >= 0 && playerIndex < len(out) && target != "" {
		out[playerIndex] = target
	}

	for i, param := range cmd.Params {
		if i >= len(out) {
			break
		}
		value := strings.TrimSpace(out[i])
		if !strings.HasPrefix(strings.ToLower(value), "random:") {
			continue
		}
		if param.Type != ParamItem && param.Type != ParamVehicle && param.Type != ParamPerk {
			return nil, fmt.Errorf("%s cannot be randomised", param.Label)
		}
		category := strings.TrimSpace(value[len("random:"):])
		pick, err := a.randomFromCatalogue(srv, string(param.Type), category)
		if err != nil {
			return nil, err
		}
		out[i] = pick
	}
	return out, nil
}

// randomFromCatalogue picks one entry of a kind, optionally from one category.
// An empty or "any" category draws from everything.
func (a *App) randomFromCatalogue(srv config.Server, kind, category string) (string, error) {
	data, err := a.catalogueFor(srv.ID, false)
	if err != nil {
		return "", err
	}
	key := map[string]string{"item": "items", "vehicle": "vehicles", "perk": "perks"}[kind]
	entries, _ := data[key].([]pz.CatalogueEntry)

	var pool []pz.CatalogueEntry
	wantAll := category == "" || strings.EqualFold(category, "any")
	for _, e := range entries {
		if wantAll || strings.EqualFold(e.Category, category) {
			pool = append(pool, e)
		}
	}
	if len(pool) == 0 {
		if wantAll {
			return "", fmt.Errorf("there are no %ss to choose from", kind)
		}
		return "", fmt.Errorf("no %ss in the category %q", kind, category)
	}
	return pool[rand.Intn(len(pool))].ID, nil
}

func suffix(out string) string {
	out = strings.TrimSpace(out)
	if out == "" {
		return ""
	}
	return ": " + truncate(strings.ReplaceAll(out, "\n", " "), 120)
}

func quote(s string) string {
	return `"` + strings.NewReplacer(`"`, `\"`, "\n", " ", "\r", " ").Replace(s) + `"`
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit && exp < 4; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTP"[exp])
}

// warningText is what players see in the countdown before a job runs.
func warningText(task config.Task, minutes int) string {
	restarts := false
	for _, step := range task.Steps {
		if step.Kind == "restart" {
			restarts = true
		}
	}
	if restarts {
		return fmt.Sprintf("Server restart in %d minute%s.", minutes, plural(minutes))
	}
	return fmt.Sprintf("%s in %d minute%s.", task.Name, minutes, plural(minutes))
}
