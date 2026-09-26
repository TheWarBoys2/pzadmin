package config

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Known-answer tests for PBKDF2-HMAC-SHA256. If these pass, the hand-rolled
// implementation is byte-identical to the reference and existing password
// hashes will keep verifying across releases.
func TestPBKDF2KnownAnswers(t *testing.T) {
	cases := []struct {
		pass, salt string
		iter, dk   int
		want       string
	}{
		{"password", "salt", 1, 32, "120fb6cffcf8b32c43e7225256c4f837a86548c92ccc35480805987cb70be17b"},
		{"password", "salt", 2, 32, "ae4d0c95af6b46d32d0adff928f06dd02a303f8ef3c251dfd6e2d85a95474c43"},
		{"password", "salt", 4096, 32, "c5e478d59288c841aa530db6845c4c8d962893a001ce4e11a4963873aa98134a"},
		{"passwd", "salt", 1, 64, "55ac046e56e3089fec1691c22544b605f94185216dde0465e68b9d57c20dacbc49ca9cccf179b645991664b39d77ef317c71b845b1e30bd509112041d3a19783"},
	}
	for _, c := range cases {
		got := hex.EncodeToString(pbkdf2(sha256.New, []byte(c.pass), []byte(c.salt), c.iter, c.dk))
		if got != c.want {
			t.Fatalf("pbkdf2(%q,%q,%d,%d) = %s, want %s", c.pass, c.salt, c.iter, c.dk, got, c.want)
		}
	}
}

func TestPasswordRoundTrip(t *testing.T) {
	salt := NewSalt()
	h := HashPassword("correct horse", salt, 2048)
	if !VerifyPassword("correct horse", salt, h, 2048) {
		t.Fatal("correct password must verify")
	}
	if VerifyPassword("correct hors", salt, h, 2048) {
		t.Fatal("wrong password must not verify")
	}
	if HashPassword("correct horse", NewSalt(), 2048) == h {
		t.Fatal("different salts must produce different hashes")
	}
}

func TestRedactRemovesEverySecret(t *testing.T) {
	c := Defaults()
	c.PasswordHash = "hash"
	c.PasswordSalt = "salt"
	c.Servers = []Server{{ID: "a", RCONPassword: "rconsecret"}}
	c.Notify.WebhookURL = "https://discord.example/hook"
	c.Notify.Webhooks = []Webhook{{ID: "p", URL: "https://discord.example/players"}}
	c.Metrics.Token = "metricsecret"
	c.Notify.Bot.Token = "bottokensecret"

	blob := marshalForTest(t, Redact(c))
	for _, secret := range []string{"hash", "salt", "rconsecret", "discord.example", "metricsecret", "bottokensecret"} {
		if strings.Contains(blob, secret) {
			t.Fatalf("redacted config still contains %q: %s", secret, blob)
		}
	}
	// Export must not even carry the placeholder.
	if strings.Contains(marshalForTest(t, Export(c)), Redacted) {
		t.Fatal("export must strip secrets entirely, not placeholder them")
	}
}

func TestUnredactKeepsExistingSecrets(t *testing.T) {
	prev := Defaults()
	prev.Servers = []Server{{ID: "a", RCONPassword: "original"}}
	prev.Username = "rick"
	prev.PasswordHash = "h"

	next := Redact(prev)
	next.Servers[0].Name = "renamed"
	Unredact(&next, prev)

	if next.Servers[0].RCONPassword != "original" {
		t.Fatalf("placeholder should restore the stored password, got %q", next.Servers[0].RCONPassword)
	}
	if next.Servers[0].Name != "renamed" {
		t.Fatal("non-secret edits must survive unredaction")
	}
	if next.Username != "rick" || next.PasswordHash != "h" {
		t.Fatal("credentials must never be overwritten by a config update")
	}
}

func TestStoreUpdateIsAtomicAndReloads(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Update(func(c *Config) error {
		c.Servers = append(c.Servers, Server{ID: "x", Name: "Riverside", Enabled: true})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := reopened.Get().Servers; len(got) != 1 || got[0].Name != "Riverside" {
		t.Fatalf("config did not survive a reload: %#v", got)
	}
	// Defaults must be applied on load, not just at creation.
	if reopened.Get().Servers[0].RCONPort != 27015 {
		t.Fatal("normalise should default the RCON port")
	}
}

func TestContainerAllowlist(t *testing.T) {
	dir := t.TempDir()
	s, _ := Open(filepath.Join(dir, "c.json"))
	_, _ = s.Update(func(c *Config) error {
		c.Servers = []Server{{ID: "a", DockerContainer: "pz-riverside"}}
		return nil
	})
	if !s.ContainerAllowed("pz-riverside") {
		t.Fatal("configured container must be allowed")
	}
	if s.ContainerAllowed("portainer") {
		t.Fatal("unconfigured container must be refused")
	}
	if s.ContainerAllowed("") {
		t.Fatal("empty container name must be refused")
	}
}

func marshalForTest(t *testing.T, c Config) string {
	t.Helper()
	b, err := jsonMarshal(c)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// A task saved before multi-step jobs existed must keep working.
func TestLegacyTaskIsMigratedToASingleStep(t *testing.T) {
	c := Defaults()
	c.Schedules = []Task{
		{ID: "a", ServerID: "s", Name: "Nightly restart", Kind: "restart", Cron: "0 4 * * *", Enabled: true},
		{ID: "b", ServerID: "s", Name: "Announce", Kind: "broadcast", Message: "hello", Cron: "0 * * * *"},
		{ID: "c", ServerID: "s", Name: "Custom", Kind: "command", Command: "checkModsNeedUpdate", Cron: "@daily"},
	}
	got := Normalise(c)

	if len(got.Schedules[0].Steps) != 1 || got.Schedules[0].Steps[0].Kind != "restart" {
		t.Fatalf("restart task not migrated: %#v", got.Schedules[0])
	}
	if got.Schedules[1].Steps[0].Message != "hello" {
		t.Fatalf("broadcast message lost: %#v", got.Schedules[1])
	}
	if got.Schedules[2].Steps[0].Command != "checkModsNeedUpdate" {
		t.Fatalf("command lost: %#v", got.Schedules[2])
	}
	for _, task := range got.Schedules {
		if task.Kind != "" || task.Message != "" || task.Command != "" {
			t.Fatalf("the old fields should be cleared after migration: %#v", task)
		}
	}

	// Migrating twice must not duplicate the step.
	again := Normalise(got)
	if len(again.Schedules[0].Steps) != 1 {
		t.Fatalf("migration is not idempotent: %#v", again.Schedules[0])
	}
}

func TestMultiStepTaskSurvivesNormalise(t *testing.T) {
	c := Defaults()
	c.Schedules = []Task{{
		ID: "a", ServerID: "s", Name: "Restart with warning", Cron: "0 4 * * *", Enabled: true,
		Steps: []Step{
			{Kind: "broadcast", Message: "Restarting in 1 minute"},
			{Kind: "wait", Seconds: 60},
			{Kind: "save"},
			{Kind: "restart"},
		},
	}}
	got := Normalise(c)
	if len(got.Schedules[0].Steps) != 4 {
		t.Fatalf("steps lost: %#v", got.Schedules[0])
	}
	if got.Schedules[0].Steps[1].Seconds != 60 {
		t.Fatal("wait duration lost")
	}
}

// A config written before webhooks became a list keeps its single webhook,
// and anyone who asked to hear about restarts still hears about stops and
// starts, which used to be reported as restarts.
func TestLegacyWebhookIsFoldedIntoTheList(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	legacy := `{"version":8,"notify":{"enabled":true,"webhookUrl":"https://discord.com/api/webhooks/1/a",
	  "events":["server.down","server.restart"],"minIntervalSeconds":120}}`
	if err := os.WriteFile(path, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	n := s.Get().Notify
	if n.WebhookURL != "" || n.Events != nil || n.Enabled {
		t.Fatalf("the old fields should be emptied: %#v", n)
	}
	if len(n.Webhooks) != 1 {
		t.Fatalf("expected one webhook, got %#v", n.Webhooks)
	}
	h := n.Webhooks[0]
	if !h.Enabled || h.URL != "https://discord.com/api/webhooks/1/a" || h.Audience != AudienceStaff || h.ID == "" {
		t.Fatalf("webhook not carried over: %#v", h)
	}
	for _, want := range []string{"server.down", "server.restart", "server.stop", "server.start"} {
		if !contains(h.Events, want) {
			t.Fatalf("expected %s in %v", want, h.Events)
		}
	}
	if n.MinIntervalSeconds != 120 {
		t.Fatal("the throttle should be kept")
	}

	// Normalising again changes nothing.
	again := Normalise(s.Get()).Notify
	if len(again.Webhooks) != 1 || again.Webhooks[0].ID != h.ID {
		t.Fatalf("migration should happen once: %#v", again.Webhooks)
	}
}

func TestUnredactWebhooksMatchesByID(t *testing.T) {
	prev := []Webhook{{ID: "a", URL: "https://one"}, {ID: "b", URL: "https://two"}}
	next := []Webhook{{ID: "b", URL: Redacted}, {ID: "c", URL: Redacted}, {ID: "a", URL: "https://new"}}
	UnredactWebhooks(next, prev)
	if next[0].URL != "https://two" || next[1].URL != "" || next[2].URL != "https://new" {
		t.Fatalf("got %#v", next)
	}
}

func TestQuietHoursContains(t *testing.T) {
	at := func(clock string) time.Time {
		v, _ := time.Parse("15:04", clock)
		return time.Date(2026, 9, 25, v.Hour(), v.Minute(), 0, 0, time.UTC)
	}
	overnight := QuietHours{Enabled: true, Start: "23:00", End: "08:00"}
	daytime := QuietHours{Enabled: true, Start: "09:30", End: "17:00"}
	for _, tc := range []struct {
		q     QuietHours
		clock string
		want  bool
	}{
		{overnight, "23:00", true},
		{overnight, "03:00", true},
		{overnight, "07:59", true},
		{overnight, "08:00", false},
		{overnight, "22:59", false},
		{daytime, "09:30", true},
		{daytime, "16:59", true},
		{daytime, "17:00", false},
		{daytime, "03:00", false},
		{QuietHours{Start: "23:00", End: "08:00"}, "03:00", false},
		{QuietHours{Enabled: true, Start: "08:00", End: "08:00"}, "08:00", false},
		{QuietHours{Enabled: true, Start: "nope", End: "08:00"}, "03:00", false},
	} {
		if got := tc.q.Contains(at(tc.clock)); got != tc.want {
			t.Errorf("%+v at %s: got %v, want %v", tc.q, tc.clock, got, tc.want)
		}
	}
}
