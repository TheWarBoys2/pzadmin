package server

import (
	"net/http"
	"testing"
	"time"

	"github.com/TheWarBoys2/pzadmin/internal/config"
	"github.com/TheWarBoys2/pzadmin/internal/store"
)

func TestQuietHoursSettingsAreValidatedAndKept(t *testing.T) {
	app, handler, _ := newTestApp(t)
	c := &client{t: t, handler: handler}
	c.setup("rick", "a-long-enough-password")

	for name, quiet := range map[string]map[string]any{
		"no start":       {"enabled": true, "start": "", "end": "08:00"},
		"bad end":        {"enabled": true, "start": "23:00", "end": "25:00"},
		"same times":     {"enabled": true, "scheduled": true, "start": "08:00", "end": "08:00"},
		"nothing picked": {"enabled": true, "start": "23:00", "end": "08:00"},
	} {
		rec := c.do(http.MethodPost, "/api/settings", map[string]any{"notify": map[string]any{"quietHours": quiet}})
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: expected 400, got %d %s", name, rec.Code, rec.Body.String())
		}
	}

	rec := c.do(http.MethodPost, "/api/settings", map[string]any{"notify": map[string]any{
		"quietHours": map[string]any{"enabled": true, "manual": true, "start": "22:30", "end": "07:00"}}})
	if rec.Code != http.StatusOK {
		t.Fatalf("save failed: %d %s", rec.Code, rec.Body.String())
	}
	want := config.QuietHours{Enabled: true, Manual: true, Start: "22:30", End: "07:00"}
	if got := app.cfg.Get().Notify.QuietHours; got != want {
		t.Fatalf("quiet hours not stored: %#v", got)
	}

	// Other Discord forms do not send quiet hours and must not wipe them.
	rec = c.do(http.MethodPost, "/api/settings", map[string]any{"notify": map[string]any{"minIntervalSeconds": 60}})
	if rec.Code != http.StatusOK || app.cfg.Get().Notify.QuietHours != want {
		t.Fatalf("a throttle-only save should keep quiet hours: %d %#v", rec.Code, app.cfg.Get().Notify.QuietHours)
	}

	// Switching off keeps the times for next time.
	rec = c.do(http.MethodPost, "/api/settings", map[string]any{"notify": map[string]any{
		"quietHours": map[string]any{"enabled": false, "start": "22:30", "end": "07:00"}}})
	if got := app.cfg.Get().Notify.QuietHours; rec.Code != http.StatusOK || got.Enabled || got.Start != "22:30" {
		t.Fatalf("switching off should keep the window: %d %#v", rec.Code, got)
	}
}

func TestQuietHoursHoldBackOnlyWhatIsTicked(t *testing.T) {
	app, _, _ := newTestApp(t)
	setQuiet := func(covering, scheduled, manual bool) {
		now := time.Now().In(app.cfg.Location())
		start, end := now.Add(-time.Hour), now.Add(time.Hour)
		if !covering {
			start, end = now.Add(time.Hour), now.Add(2*time.Hour)
		}
		_, err := app.cfg.Update(func(c *config.Config) error {
			c.Notify.QuietHours = config.QuietHours{Enabled: true, Scheduled: scheduled, Manual: manual,
				Start: start.Format("15:04"), End: end.Format("15:04")}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	upAfter := func(by string) store.Event {
		return store.Event{Kind: "server.up", Source: "monitor", Meta: map[string]any{"restartedBy": by}}
	}
	scheduled := []store.Event{
		{Kind: "server.restart", Source: "schedule"},
		upAfter("schedule"),
		{Kind: "backup.done", Source: "schedule"},
	}
	manual := []store.Event{
		{Kind: "server.restart", Source: "ui"},
		{Kind: "server.restart", Source: "api"},
		{Kind: "server.stop", Source: "ui"},
		{Kind: "server.start", Source: "ui"},
		{Kind: "server.stop", Source: "api"},
		upAfter("ui"),
		upAfter("api"),
	}
	always := map[string]store.Event{
		"watchdog restart": {Kind: "server.restart", Source: "monitor"},
		"back from outage": upAfter(""),
		"outage":           {Kind: "server.down", Source: "monitor"},
		"failed backup":    {Kind: "backup.failed", Source: "schedule"},
		"manual backup":    {Kind: "backup.done", Source: "ui"},
		"recovered":        {Kind: "server.recovered", Source: "monitor"},
	}
	check := func(label string, events []store.Event, want bool) {
		t.Helper()
		for _, e := range events {
			if got := app.quietNotice(e); got != want {
				t.Errorf("%s: %s from %s %v: held back %v, want %v", label, e.Kind, e.Source, e.Meta, got, want)
			}
		}
	}
	discordStep := func() string {
		res, _ := app.executeStep(t.Context(), config.Step{Kind: "discord", Webhook: "anything"}, config.Server{})
		return res
	}

	setQuiet(true, true, false)
	check("scheduled only", scheduled, true)
	check("scheduled only", manual, false)
	if discordStep() != "skipped, quiet hours" {
		t.Error("a Discord step should be skipped when scheduled jobs are quiet")
	}

	setQuiet(true, false, true)
	check("manual only", scheduled, false)
	check("manual only", manual, true)
	if discordStep() == "skipped, quiet hours" {
		t.Error("a Discord step should post when only manual restarts are quiet")
	}

	setQuiet(true, true, true)
	check("both", scheduled, true)
	check("both", manual, true)
	for name, e := range always {
		if app.quietNotice(e) {
			t.Errorf("%s should always post", name)
		}
	}

	setQuiet(false, true, true)
	check("outside the window", scheduled, false)
	check("outside the window", manual, false)
}

func TestRestartSourceIsCarriedToBackOnline(t *testing.T) {
	if got := restartedBy(Status{Restarting: true, restartSource: "ui"}); got != "ui" {
		t.Fatalf("got %q", got)
	}
	if got := restartedBy(Status{restartSource: "ui"}); got != "" {
		t.Fatalf("a server that was not restarting came back from an outage, got %q", got)
	}
}
