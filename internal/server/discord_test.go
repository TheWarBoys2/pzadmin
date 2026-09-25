package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/TheWarBoys2/pzadmin/internal/config"
)

func TestWebhookSettingsRoundTripWithoutLeakingAddresses(t *testing.T) {
	app, handler, _ := newTestApp(t)
	c := &client{t: t, handler: handler}
	c.setup("rick", "a-long-enough-password")
	seedServer(t, app, `{"id":"a","name":"Riverside","enabled":true}`)

	staffURL := "https://discord.com/api/webhooks/1/staff-secret"
	playersURL := "https://discord.com/api/webhooks/2/players-secret"
	rec := c.do(http.MethodPost, "/api/settings", map[string]any{"notify": map[string]any{
		"minIntervalSeconds": 300,
		"webhooks": []map[string]any{
			{"name": "Staff", "enabled": true, "url": staffURL, "audience": "staff",
				"events": []string{"server.down", "backup.failed"}, "servers": []string{}},
			{"name": "", "enabled": true, "url": playersURL, "audience": "players",
				"events": []string{"server.restart", "server.up"}, "servers": []string{"a", "gone"},
				"liveStatus": true, "listPlayers": true,
				"messages": map[string]string{"server.up": "{server} is up!", "server.down": "  "}},
		},
	}})
	if rec.Code != http.StatusOK {
		t.Fatalf("save failed: %d %s", rec.Code, rec.Body.String())
	}
	for _, path := range []string{"/api/state", "/api/export"} {
		body := c.do(http.MethodGet, path, nil).Body.String()
		if strings.Contains(body, "staff-secret") || strings.Contains(body, "players-secret") {
			t.Fatalf("%s leaked a webhook address", path)
		}
	}
	if strings.Contains(rec.Body.String(), "secret") {
		t.Fatal("the save response echoed a webhook address")
	}

	hooks := app.cfg.Get().Notify.Webhooks
	if len(hooks) != 2 || hooks[0].URL != staffURL || hooks[1].URL != playersURL {
		t.Fatalf("addresses not stored: %#v", hooks)
	}
	p := hooks[1]
	if p.Name != "Player announcements" || p.ID == "" {
		t.Fatalf("a blank name should get a default and every webhook an ID: %#v", p)
	}
	if len(p.Servers) != 1 || p.Servers[0] != "a" {
		t.Fatalf("an unknown server should be dropped: %v", p.Servers)
	}
	if len(p.Messages) != 1 || p.Messages["server.up"] != "{server} is up!" {
		t.Fatalf("blank wording should be dropped: %v", p.Messages)
	}

	// Saving the form as the browser received it keeps the addresses.
	var state struct {
		Config config.Config `json:"config"`
	}
	if err := json.Unmarshal(c.do(http.MethodGet, "/api/state", nil).Body.Bytes(), &state); err != nil {
		t.Fatal(err)
	}
	state.Config.Notify.Webhooks[0].Name = "Staff room"
	rec = c.do(http.MethodPost, "/api/settings", map[string]any{"notify": state.Config.Notify})
	if rec.Code != http.StatusOK {
		t.Fatalf("re-save failed: %s", rec.Body.String())
	}
	hooks = app.cfg.Get().Notify.Webhooks
	if hooks[0].Name != "Staff room" || hooks[0].URL != staffURL || hooks[1].URL != playersURL || hooks[1].ID != p.ID {
		t.Fatalf("re-saving should keep addresses and IDs: %#v", hooks)
	}

	// The old single-webhook form still works and leaves the list alone.
	rec = c.do(http.MethodPost, "/api/settings", map[string]any{"notify": map[string]any{"minIntervalSeconds": 60}})
	if rec.Code != http.StatusOK || len(app.cfg.Get().Notify.Webhooks) != 2 {
		t.Fatalf("a throttle-only save should keep the webhooks: %d %s", rec.Code, rec.Body.String())
	}
}

func TestWebhookSettingsAreValidated(t *testing.T) {
	app, handler, _ := newTestApp(t)
	c := &client{t: t, handler: handler}
	c.setup("rick", "a-long-enough-password")

	for name, hook := range map[string]map[string]any{
		"staff-only event for players": {"enabled": true, "url": "https://discord.com/api/webhooks/1/a",
			"audience": "players", "events": []string{"backup.failed"}},
		"unknown event": {"enabled": true, "url": "https://discord.com/api/webhooks/1/a",
			"audience": "staff", "events": []string{"server.exploded"}},
		"enabled without address": {"enabled": true, "url": "", "audience": "staff"},
		"not a web address":       {"enabled": true, "url": "discord.com/api/webhooks/1/a", "audience": "staff"},
		"live status off Discord": {"enabled": true, "url": "https://hooks.slack.com/services/x",
			"audience": "players", "liveStatus": true},
		"unknown audience": {"enabled": true, "url": "https://discord.com/api/webhooks/1/a", "audience": "everyone"},
	} {
		rec := c.do(http.MethodPost, "/api/settings", map[string]any{"notify": map[string]any{
			"minIntervalSeconds": 300, "webhooks": []any{hook},
		}})
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: expected 400, got %d %s", name, rec.Code, rec.Body.String())
		}
	}
	if n := len(app.cfg.Get().Notify.Webhooks); n != 0 {
		t.Fatalf("nothing invalid should have been saved, got %d webhooks", n)
	}
}

// The game's own Discord bot token is a secret like the RCON password: the
// form never shows it, and leaving the box blank keeps it.
func TestDiscordBotTokenIsASecretInTheSettingsForm(t *testing.T) {
	app, c, id := withCatalogueServer(t)
	ini := filepath.Join(app.cfg.Get().PZRoot, "riverside", "Server", "riv.ini")
	mustWrite(t, ini, "PublicName=Riverside\nDiscordEnable=false\nDiscordToken=bot-token-value\n"+
		"DiscordChatChannel=general\nDiscordCommandChannel=staff\n")

	rec := c.do(http.MethodGet, "/api/server/config/fields?id="+id+"&file=riv.ini", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("fields failed: %s", rec.Body.String())
	}
	body := rec.Body.String()
	if strings.Contains(body, "bot-token-value") {
		t.Fatal("the bot token was sent to the browser")
	}
	if !strings.Contains(body, `"name":"Discord"`) {
		t.Fatalf("expected a Discord group: %s", body)
	}

	rec = c.raw("/api/server/config/apply", `{"serverId":"`+id+`","file":"riv.ini",`+
		`"changes":{"DiscordToken":"","DiscordEnable":"true","DiscordChatChannel":"zomboid-chat"}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("apply failed: %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "DiscordEnable") || !strings.Contains(rec.Body.String(), "only read at startup") {
		t.Fatalf("the bot settings should be flagged as needing a restart: %s", rec.Body.String())
	}
	data, err := os.ReadFile(ini)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"DiscordToken=bot-token-value", "DiscordEnable=true", "DiscordChatChannel=zomboid-chat"} {
		if !strings.Contains(string(data), want) {
			t.Fatalf("expected %q in:\n%s", want, data)
		}
	}
}

// fakeDiscord is enough of Discord's webhook API to exercise live status.
type fakeDiscord struct {
	mu       sync.Mutex
	srv      *httptest.Server
	next     int
	messages map[string]string // id -> description
	reqs     []string
}

func newFakeDiscord(t *testing.T) *fakeDiscord {
	d := &fakeDiscord{messages: map[string]string{}, next: 500}
	d.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		d.mu.Lock()
		defer d.mu.Unlock()
		d.reqs = append(d.reqs, r.Method)
		var body struct {
			Embeds []struct {
				Description string `json:"description"`
			} `json:"embeds"`
		}
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &body)
		desc := ""
		if len(body.Embeds) > 0 {
			desc = body.Embeds[0].Description
		}
		parts := strings.Split(r.URL.Path, "/messages/")
		switch {
		case r.Method == http.MethodPost:
			d.next++
			id := strconv.Itoa(d.next)
			d.messages[id] = desc
			_, _ = io.WriteString(w, `{"id":"`+id+`"}`)
		case len(parts) == 2 && d.messages[parts[1]] == "":
			w.WriteHeader(http.StatusNotFound)
		case r.Method == http.MethodPatch:
			d.messages[parts[1]] = desc
			_, _ = io.WriteString(w, `{}`)
		case r.Method == http.MethodDelete:
			delete(d.messages, parts[1])
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	t.Cleanup(d.srv.Close)
	return d
}

func (d *fakeDiscord) snapshot() (map[string]string, []string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	msgs := map[string]string{}
	for k, v := range d.messages {
		msgs[k] = v
	}
	reqs := append([]string(nil), d.reqs...)
	d.reqs = nil
	return msgs, reqs
}

func TestLiveStatusPostsEditsAndRemovesItsMessage(t *testing.T) {
	app, _, dir := newTestApp(t)
	discord := newFakeDiscord(t)
	seedServer(t, app, `{"id":"a","name":"Riverside","enabled":true}`)
	setHooks := func(live bool) {
		t.Helper()
		if _, err := app.cfg.Update(func(c *config.Config) error {
			c.Notify.Webhooks = []config.Webhook{{ID: "p", Name: "Players", Enabled: true,
				URL: discord.srv.URL + "/api/webhooks/1/x", Audience: config.AudiencePlayers,
				LiveStatus: live, ListPlayers: true}}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	setHooks(true)
	board := loadLiveBoard(filepath.Join(dir, "livestatus.json"))
	ctx := context.Background()

	// Not probed yet: nothing is claimed.
	app.syncLiveStatus(ctx, board)
	if msgs, _ := discord.snapshot(); len(msgs) != 0 {
		t.Fatalf("a server that has not been checked should not get a card yet: %v", msgs)
	}

	app.updateStatus("a", func(st *Status) {
		st.Online, st.LastCheck, st.LastOnline = true, TimeNow(), TimeNow()
		st.Players, st.PlayerCount = []string{"rick", "@everyone"}, 2
	})
	app.syncLiveStatus(ctx, board)
	msgs, reqs := discord.snapshot()
	if len(msgs) != 1 || len(reqs) != 1 || reqs[0] != http.MethodPost {
		t.Fatalf("expected one new message, got %v %v", msgs, reqs)
	}
	for _, text := range msgs {
		if !strings.Contains(text, "Online") || !strings.Contains(text, "2 players") {
			t.Fatalf("unexpected card %q", text)
		}
		if strings.Contains(text, "@everyone") {
			t.Fatalf("a player name must not be able to mention anyone: %q", text)
		}
	}

	// Nothing changed, nothing sent.
	app.syncLiveStatus(ctx, board)
	if _, reqs := discord.snapshot(); len(reqs) != 0 {
		t.Fatalf("an unchanged card should not be re-sent: %v", reqs)
	}

	// A restart edits the same message.
	app.markRestarting("a", "scheduled job")
	app.syncLiveStatus(ctx, board)
	msgs, reqs = discord.snapshot()
	if len(msgs) != 1 || len(reqs) != 1 || reqs[0] != http.MethodPatch {
		t.Fatalf("expected the card to be edited in place, got %v %v", msgs, reqs)
	}
	for _, text := range msgs {
		if !strings.Contains(text, "Restarting") || strings.Contains(text, "scheduled job") {
			t.Fatalf("the card should say restarting, without staff detail: %q", text)
		}
	}

	// The board survives PZAdmin restarting.
	board = loadLiveBoard(filepath.Join(dir, "livestatus.json"))
	app.syncLiveStatus(ctx, board)
	if _, reqs := discord.snapshot(); len(reqs) != 0 {
		t.Fatalf("a reloaded board should know the card is current: %v", reqs)
	}

	// Someone deletes the message in Discord: a new one is posted.
	discord.mu.Lock()
	discord.messages = map[string]string{}
	discord.mu.Unlock()
	app.clearRestarting("a")
	app.syncLiveStatus(ctx, board)
	msgs, reqs = discord.snapshot()
	if len(msgs) != 1 || strings.Join(reqs, ",") != "PATCH,POST" {
		t.Fatalf("a deleted card should be replaced, got %v %v", msgs, reqs)
	}

	// Switching live status off takes the card down.
	setHooks(false)
	app.syncLiveStatus(ctx, board)
	msgs, reqs = discord.snapshot()
	if len(msgs) != 0 || strings.Join(reqs, ",") != "DELETE" {
		t.Fatalf("the card should be removed, got %v %v", msgs, reqs)
	}
}

func TestLiveStatusIsPausedOnShutdown(t *testing.T) {
	app, _, dir := newTestApp(t)
	discord := newFakeDiscord(t)
	seedServer(t, app, `{"id":"a","name":"Riverside","enabled":true}`)
	if _, err := app.cfg.Update(func(c *config.Config) error {
		c.Notify.Webhooks = []config.Webhook{{ID: "p", Enabled: true, URL: discord.srv.URL + "/api/webhooks/1/x",
			Audience: config.AudiencePlayers, LiveStatus: true}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	app.updateStatus("a", func(st *Status) { st.Online, st.LastCheck = true, TimeNow() })
	board := loadLiveBoard(filepath.Join(dir, "livestatus.json"))
	app.syncLiveStatus(context.Background(), board)
	app.pauseLiveStatus(context.Background(), board)
	msgs, _ := discord.snapshot()
	for _, text := range msgs {
		if !strings.Contains(text, "PZAdmin is not running") {
			t.Fatalf("expected the card to say it is not being updated, got %q", text)
		}
	}
	// Next start, the first real status is sent even if it matches the old one.
	app.syncLiveStatus(context.Background(), board)
	if _, reqs := discord.snapshot(); strings.Join(reqs, ",") != "PATCH" {
		t.Fatalf("expected the card to be refreshed, got %v", reqs)
	}
}

func TestRestartReason(t *testing.T) {
	for _, c := range []struct{ source, reason, want string }{
		{"schedule", "scheduled job", "scheduled"},
		{"monitor", "auto (mod update)", "mods"},
		{"ui", "requested by rick", "manual"},
		{"test", "test", ""},
	} {
		if got := restartReason(c.source, c.reason); got != c.want {
			t.Errorf("restartReason(%q, %q) = %q, want %q", c.source, c.reason, got, c.want)
		}
	}
}
