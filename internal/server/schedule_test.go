package server

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/TheWarBoys2/pzadmin/internal/config"
)

func TestStepValidation(t *testing.T) {
	good := []config.Step{
		{Kind: "save"},
		{Kind: "restart"},
		{Kind: "backup"},
		{Kind: "wait", Seconds: 60},
		{Kind: "broadcast", Message: "hello"},
		{Kind: "command", Command: "checkModsNeedUpdate"},
		{Kind: "action", Action: "additem", Args: []string{"@each", "Base.Axe", "1"}},
		{Kind: "action", Action: "additem", Args: []string{"@random", "random:Food", "1"}},
		{Kind: "action", Action: "addxp", Args: []string{"@each", "Fitness", "50"}},
		// Randomising a skill is legitimate: a surprise skill bump is exactly
		// the sort of thing a scheduled job is for.
		{Kind: "action", Action: "addxp", Args: []string{"@random", "random:Crafting", "100"}},
	}
	for _, step := range good {
		s := step
		if err := validateStep(&s); err != nil {
			t.Fatalf("%#v should be valid: %v", step, err)
		}
	}

	bad := []struct {
		step config.Step
		why  string
	}{
		{config.Step{Kind: "nonsense"}, "unknown kind"},
		{config.Step{Kind: "wait", Seconds: 0}, "zero wait"},
		{config.Step{Kind: "wait", Seconds: 99999}, "absurd wait"},
		{config.Step{Kind: "broadcast"}, "no message"},
		{config.Step{Kind: "command", Command: "a\nb"}, "two commands"},
		{config.Step{Kind: "action", Action: "notacommand"}, "unknown command"},
		{config.Step{Kind: "action", Action: "additem", Args: []string{"@nobody", "Base.Axe", "1"}}, "bad placeholder"},
		{config.Step{Kind: "action", Action: "additem", Args: []string{"@each"}}, "missing item"},
		{config.Step{Kind: "action", Action: "addxp", Args: []string{"@each", "Fitness", "random:Food"}}, "randomising a number"},
		{config.Step{Kind: "action", Action: "servermsg", Args: []string{"random:Food"}}, "randomising a message"},
	}
	for _, tc := range bad {
		s := tc.step
		if err := validateStep(&s); err == nil {
			t.Fatalf("%s should be refused: %#v", tc.why, tc.step)
		}
	}
}

// A job with no steps does nothing and would look like it was working.
func TestScheduleRequiresSteps(t *testing.T) {
	app, handler, _ := newTestApp(t)
	c := &client{t: t, handler: handler}
	c.setup("rick", "a-long-enough-password")
	seedServer(t, app, `{"name":"Riverside","host":"127.0.0.1","rconPort":27015,"rconPassword":"x","enabled":false}`)
	id := app.cfg.Get().Servers[0].ID

	rec := c.raw("/api/schedules",
		`{"tasks":[{"serverId":"`+id+`","name":"Empty","cron":"0 4 * * *","enabled":true,"steps":[]}]}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("a job with no steps should be refused, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "no steps") {
		t.Fatalf("the message should say what is wrong: %s", rec.Body.String())
	}
}

func TestMultiStepScheduleRoundTrips(t *testing.T) {
	app, handler, _ := newTestApp(t)
	c := &client{t: t, handler: handler}
	c.setup("rick", "a-long-enough-password")
	seedServer(t, app, `{"name":"Riverside","host":"127.0.0.1","rconPort":27015,"rconPassword":"x","enabled":false}`)
	id := app.cfg.Get().Servers[0].ID

	payload := `{"tasks":[{"serverId":"` + id + `","name":"Nightly restart","cron":"0 4 * * *",
	  "enabled":true,"warnMinutes":[15,5,1],"steps":[
	    {"kind":"broadcast","message":"Restarting shortly","note":"Warn everyone"},
	    {"kind":"wait","seconds":60},
	    {"kind":"save"},
	    {"kind":"backup"},
	    {"kind":"restart"}]}]}`
	if rec := c.raw("/api/schedules", payload); rec.Code != http.StatusOK {
		t.Fatalf("save failed: %s", rec.Body.String())
	}

	task := app.cfg.Get().Schedules[0]
	if len(task.Steps) != 5 {
		t.Fatalf("expected 5 steps, got %d", len(task.Steps))
	}
	if task.Steps[0].Note != "Warn everyone" || task.Steps[1].Seconds != 60 {
		t.Fatalf("step detail lost: %#v", task.Steps)
	}

	// And it comes back to the browser intact.
	body := c.do(http.MethodGet, "/api/state", nil).Body.String()
	if !strings.Contains(body, "Warn everyone") {
		t.Fatal("steps should be visible in the state response")
	}
}

// A legacy task posted by an older client is folded into a step rather than
// rejected, so an upgrade in progress cannot lose someone's schedule.
func TestLegacyTaskPayloadIsAccepted(t *testing.T) {
	app, handler, _ := newTestApp(t)
	c := &client{t: t, handler: handler}
	c.setup("rick", "a-long-enough-password")
	seedServer(t, app, `{"name":"Riverside","host":"127.0.0.1","rconPort":27015,"rconPassword":"x","enabled":false}`)
	id := app.cfg.Get().Servers[0].ID

	rec := c.raw("/api/schedules",
		`{"tasks":[{"serverId":"`+id+`","name":"Old style","cron":"0 4 * * *","enabled":true,"kind":"save"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("a legacy payload should still be accepted: %s", rec.Body.String())
	}
	task := app.cfg.Get().Schedules[0]
	if len(task.Steps) != 1 || task.Steps[0].Kind != "save" {
		t.Fatalf("not migrated: %#v", task)
	}
}

// The placeholders are the point of a customisable job: check they expand.
func TestActionStepResolvesPlaceholders(t *testing.T) {
	app, _, _ := newTestApp(t)
	srv := config.Server{ID: "a", Name: "Riverside"}
	cmd, _ := LookupCommand("additem")

	// A named player passes straight through.
	args, err := app.resolveArgs(cmd, []string{"Rick", "Base.Axe", "1"}, "", 0, srv)
	if err != nil {
		t.Fatal(err)
	}
	if args[0] != "Rick" || args[1] != "Base.Axe" {
		t.Fatalf("unexpected args %v", args)
	}

	// The resolved target replaces the placeholder.
	args, err = app.resolveArgs(cmd, []string{"@each", "Base.Axe", "1"}, "Steve", 0, srv)
	if err != nil {
		t.Fatal(err)
	}
	if args[0] != "Steve" {
		t.Fatalf("the target should have been substituted: %v", args)
	}

	// Short argument lists are padded rather than panicking.
	if _, err := app.resolveArgs(cmd, []string{"Rick"}, "", 0, srv); err != nil {
		t.Fatalf("a short list should be padded: %v", err)
	}
}

func TestRandomFromCatalogueReportsAnEmptyPool(t *testing.T) {
	app, _, _ := newTestApp(t)
	if _, err := app.cfg.Update(func(c *config.Config) error {
		c.Servers = []config.Server{{ID: "a", Name: "Riverside"}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	srv, _ := app.cfg.Server("a")
	// No game files are configured, so there is nothing to draw from and the
	// error has to say so rather than producing an empty item ID.
	if _, err := app.randomFromCatalogue(srv, "item", "Food"); err == nil {
		t.Fatal("an empty pool should be an error, not a blank item")
	} else if !strings.Contains(err.Error(), "Food") {
		t.Fatalf("the message should name the category: %v", err)
	}
}

func TestExecuteStepWait(t *testing.T) {
	app, _, _ := newTestApp(t)
	srv := config.Server{ID: "a", Name: "Riverside"}
	out, err := app.executeStep(context.Background(), config.Step{Kind: "wait", Seconds: 1}, srv)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "waited") {
		t.Fatalf("unexpected result %q", out)
	}

	// A cancelled job must not sit in a wait.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := app.executeStep(ctx, config.Step{Kind: "wait", Seconds: 30}, srv); err == nil {
		t.Fatal("a cancelled context should abandon the wait")
	}
}

func TestWarningTextDependsOnWhatTheJobDoes(t *testing.T) {
	restart := config.Task{Name: "Nightly", Steps: []config.Step{{Kind: "save"}, {Kind: "restart"}}}
	if got := warningText(restart, 5); !strings.Contains(got, "restart in 5 minutes") {
		t.Fatalf("a restart job should warn about a restart: %q", got)
	}
	other := config.Task{Name: "Loot drop", Steps: []config.Step{{Kind: "action", Action: "additem"}}}
	if got := warningText(other, 1); !strings.Contains(got, "Loot drop in 1 minute") {
		t.Fatalf("a non-restart job should use its own name: %q", got)
	}
}
