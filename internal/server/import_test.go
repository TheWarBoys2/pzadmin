package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"unicode/utf8"
)

// An imported file goes through the same checks as the schedule editor. A job
// the editor would refuse is left out and named, not saved to fail at 4am.
func TestImportChecksSchedulesLikeTheEditor(t *testing.T) {
	app, handler, _ := newTestApp(t)
	c := &client{t: t, handler: handler}
	c.setup("rick", "a-long-enough-password")
	seedServer(t, app, `{"id":"srv1","stack":"riverside","name":"Riverside","host":"127.0.0.1","rconPort":27015,"enabled":false}`)

	file := `{"servers":[{"id":"old1","stack":"riverside","name":"Riverside"},
	                     {"id":"old2","stack":"elsewhere","name":"Muldraugh"}],
	  "timezone":"Europe/London",
	  "schedules":[
	    {"id":"a","serverId":"old1","name":"Nightly save","cron":"0 4 * * *","enabled":true,"steps":[{"kind":"save"}]},
	    {"id":"b","serverId":"old1","name":"Legacy","cron":"0 5 * * *","enabled":true,"kind":"restart"},
	    {"id":"c","serverId":"old1","name":"Broken cron","cron":"not a cron","enabled":true,"steps":[{"kind":"save"}]},
	    {"id":"d","serverId":"old1","name":"Nothing to do","cron":"0 6 * * *","enabled":true,"steps":[]},
	    {"id":"e","serverId":"old1","name":"Bad step","cron":"0 7 * * *","enabled":true,"steps":[{"kind":"wait","seconds":0}]},
	    {"id":"f","serverId":"old2","name":"Other server","cron":"0 8 * * *","enabled":true,"steps":[{"kind":"save"}]}
	  ]}`
	rec := c.raw("/api/import", file)
	if rec.Code != http.StatusOK {
		t.Fatalf("import failed: %d %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Servers          int      `json:"servers"`
		SkippedServers   []string `json:"skippedServers"`
		Schedules        int      `json:"schedules"`
		SkippedSchedules []string `json:"skippedSchedules"`
		Summary          string   `json:"summary"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Servers != 1 || len(resp.SkippedServers) != 1 || resp.SkippedServers[0] != "Muldraugh" {
		t.Fatalf("server matching: %+v", resp)
	}
	if resp.Schedules != 2 || len(resp.SkippedSchedules) != 4 {
		t.Fatalf("expected 2 kept and 4 left out: %+v", resp)
	}
	for _, name := range []string{"Broken cron", "Nothing to do", "Bad step", "Other server"} {
		if !strings.Contains(strings.Join(resp.SkippedSchedules, "\n"), name) {
			t.Fatalf("%q should be named as left out: %v", name, resp.SkippedSchedules)
		}
	}

	cfg := app.cfg.Get()
	if cfg.Timezone != "Europe/London" {
		t.Fatalf("timezone not applied: %q", cfg.Timezone)
	}
	if len(cfg.Schedules) != 2 {
		t.Fatalf("stored schedules: %#v", cfg.Schedules)
	}
	for _, task := range cfg.Schedules {
		if task.ServerID != "srv1" {
			t.Fatalf("job should point at the local server: %#v", task)
		}
		if len(task.Steps) != 1 {
			t.Fatalf("job should have its step, legacy ones folded: %#v", task)
		}
	}
}

// A file with a timezone this system does not know is refused whole: the
// schedules in it were written for that zone.
func TestImportRefusesAnUnknownTimezone(t *testing.T) {
	app, handler, _ := newTestApp(t)
	c := &client{t: t, handler: handler}
	c.setup("rick", "a-long-enough-password")
	seedServer(t, app, `{"id":"srv1","stack":"riverside","name":"Riverside","host":"127.0.0.1","rconPort":27015,"enabled":false}`)
	before := app.cfg.Get().Timezone

	rec := c.raw("/api/import", `{"servers":[{"id":"x","stack":"riverside","name":"Renamed"}],"timezone":"Mars/Olympus"}`)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "Mars/Olympus") {
		t.Fatalf("expected a refusal naming the zone, got %d %s", rec.Code, rec.Body.String())
	}
	cfg := app.cfg.Get()
	if cfg.Timezone != before || cfg.Servers[0].Name != "Riverside" {
		t.Fatal("a refused import must change nothing")
	}
}

// A file with no schedules leaves the current ones alone.
func TestImportWithoutSchedulesKeepsTheCurrentOnes(t *testing.T) {
	app, handler, _ := newTestApp(t)
	c := &client{t: t, handler: handler}
	c.setup("rick", "a-long-enough-password")
	seedServer(t, app, `{"id":"srv1","stack":"riverside","name":"Riverside","host":"127.0.0.1","rconPort":27015,"enabled":false}`)
	if rec := c.raw("/api/schedules",
		`{"tasks":[{"serverId":"srv1","name":"Keep me","cron":"0 4 * * *","enabled":true,"steps":[{"kind":"save"}]}]}`); rec.Code != http.StatusOK {
		t.Fatal(rec.Body.String())
	}
	if rec := c.raw("/api/import", `{"servers":[{"id":"x","stack":"riverside","name":"Riverside"}]}`); rec.Code != http.StatusOK {
		t.Fatal(rec.Body.String())
	}
	if got := app.cfg.Get().Schedules; len(got) != 1 || got[0].Name != "Keep me" {
		t.Fatalf("schedules should be untouched: %#v", got)
	}
}

func TestTruncateNeverSplitsACharacter(t *testing.T) {
	cases := []struct {
		in   string
		n    int
		want string
	}{
		{"hello", 10, "hello"},
		{"hello", 3, "hel"},
		{"Привет, выжившие", 6, "Привет"},
		{"🧟🧟🧟", 2, "🧟🧟"},
		{"", 5, ""},
	}
	for _, tc := range cases {
		got := truncate(tc.in, tc.n)
		if got != tc.want || !utf8.ValidString(got) {
			t.Fatalf("truncate(%q, %d) = %q, want %q", tc.in, tc.n, got, tc.want)
		}
	}
}
