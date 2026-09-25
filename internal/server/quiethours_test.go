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
		"no start":   {"enabled": true, "start": "", "end": "08:00"},
		"bad end":    {"enabled": true, "start": "23:00", "end": "25:00"},
		"same times": {"enabled": true, "start": "08:00", "end": "08:00"},
	} {
		rec := c.do(http.MethodPost, "/api/settings", map[string]any{"notify": map[string]any{"quietHours": quiet}})
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: expected 400, got %d %s", name, rec.Code, rec.Body.String())
		}
	}

	rec := c.do(http.MethodPost, "/api/settings", map[string]any{"notify": map[string]any{
		"quietHours": map[string]any{"enabled": true, "start": "22:30", "end": "07:00"}}})
	if rec.Code != http.StatusOK {
		t.Fatalf("save failed: %d %s", rec.Code, rec.Body.String())
	}
	want := config.QuietHours{Enabled: true, Start: "22:30", End: "07:00"}
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

func TestQuietHoursHoldBackOnlyRoutineScheduledNotices(t *testing.T) {
	app, _, _ := newTestApp(t)
	setQuiet := func(covering bool) {
		now := time.Now().In(app.cfg.Location())
		start, end := now.Add(-time.Hour), now.Add(time.Hour)
		if !covering {
			start, end = now.Add(time.Hour), now.Add(2*time.Hour)
		}
		_, err := app.cfg.Update(func(c *config.Config) error {
			c.Notify.QuietHours = config.QuietHours{Enabled: true, Start: start.Format("15:04"), End: end.Format("15:04")}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}

	scheduledRestart := store.Event{Kind: "server.restart", Source: "schedule"}
	scheduledUp := store.Event{Kind: "server.up", Source: "monitor", Meta: map[string]any{"scheduled": true}}
	scheduledBackup := store.Event{Kind: "backup.done", Source: "schedule"}
	loud := map[string]store.Event{
		"manual restart":   {Kind: "server.restart", Source: "ui"},
		"api restart":      {Kind: "server.restart", Source: "api"},
		"recovery":         {Kind: "server.up", Source: "monitor", Meta: map[string]any{"scheduled": false}},
		"outage":           {Kind: "server.down", Source: "monitor"},
		"failed backup":    {Kind: "backup.failed", Source: "schedule"},
		"crash back again": {Kind: "server.recovered", Source: "monitor"},
	}

	setQuiet(true)
	for _, e := range []store.Event{scheduledRestart, scheduledUp, scheduledBackup} {
		if !app.quietNotice(e) {
			t.Errorf("%s from %s should be held back during quiet hours", e.Kind, e.Source)
		}
	}
	for name, e := range loud {
		if app.quietNotice(e) {
			t.Errorf("%s should still post during quiet hours", name)
		}
	}
	if res, err := app.executeStep(t.Context(), config.Step{Kind: "discord", Webhook: "anything"}, config.Server{}); err != nil || res != "skipped, quiet hours" {
		t.Errorf("a Discord step should be skipped during quiet hours: %q %v", res, err)
	}

	setQuiet(false)
	if app.quietNotice(scheduledRestart) || app.quietNotice(scheduledUp) {
		t.Error("scheduled notices should post outside quiet hours")
	}
}
