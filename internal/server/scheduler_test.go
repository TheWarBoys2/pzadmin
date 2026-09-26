package server

import (
	"testing"
	"time"

	"github.com/TheWarBoys2/pzadmin/internal/config"
)

// runDueTasks runs each enabled job whose time has come, once per minute,
// and leaves disabled and not-yet-due jobs alone.
func TestSchedulerRunsDueJobsOnce(t *testing.T) {
	app, _, _ := newTestApp(t)
	// RCON on port 1 is refused, so a save step fails at once; the run is
	// still recorded, which is what this test looks for.
	seedServer(t, app, `{"id":"srv1","name":"Riverside","host":"127.0.0.1","rconPort":1,"rconPassword":"x","enabled":false}`)
	save := []config.Step{{Kind: "save"}}
	if _, err := app.cfg.Update(func(c *config.Config) error {
		c.Schedules = []config.Task{
			{ID: "due", ServerID: "srv1", Name: "Due", Cron: "30 4 * * *", Enabled: true, Steps: save},
			{ID: "off", ServerID: "srv1", Name: "Disabled", Cron: "30 4 * * *", Enabled: false, Steps: save},
			{ID: "later", ServerID: "srv1", Name: "Later", Cron: "31 4 * * *", Enabled: true, Steps: save},
			{ID: "gone", ServerID: "nope", Name: "Orphan", Cron: "30 4 * * *", Enabled: true, Steps: save},
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	lastRun := func(id string) string {
		for _, task := range app.cfg.Get().Schedules {
			if task.ID == id {
				return task.LastRun
			}
		}
		t.Fatalf("no task %s", id)
		return ""
	}
	runs := func() int {
		return len(app.store.Events("srv1", "schedule.run", 50))
	}

	fired := newFireLog()
	at := time.Date(2026, 9, 26, 4, 30, 0, 0, time.UTC)
	app.runDueTasks(at, fired)

	deadline := time.Now().Add(5 * time.Second)
	for lastRun("due") == "" {
		if time.Now().After(deadline) {
			t.Fatal("the due job never ran")
		}
		time.Sleep(20 * time.Millisecond)
	}

	// The same minute again, as happens if the clock is nudged: no second run.
	app.runDueTasks(at.Add(20*time.Second), fired)
	time.Sleep(200 * time.Millisecond)
	if n := runs(); n != 1 {
		t.Fatalf("expected exactly one run, got %d", n)
	}
	if lastRun("off") != "" || lastRun("later") != "" || lastRun("gone") != "" {
		t.Fatal("only the enabled, due job with a server should have run")
	}
}
