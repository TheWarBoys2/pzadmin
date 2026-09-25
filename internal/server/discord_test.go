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
	"time"

	"github.com/TheWarBoys2/pzadmin/internal/config"
	"github.com/TheWarBoys2/pzadmin/internal/store"
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

// A scheduled job can post to a chosen Discord channel, the channel cannot be
// removed while a job uses it, and a job cannot point at one that is gone.
func TestDiscordScheduleStep(t *testing.T) {
	app, handler, _ := newTestApp(t)
	c := &client{t: t, handler: handler}
	c.setup("rick", "a-long-enough-password")
	discord := newFakeDiscord(t)
	seedServer(t, app, `{"id":"a","name":"Riverside","enabled":true}`)
	if _, err := app.cfg.Update(func(cfg *config.Config) error {
		cfg.Notify.Webhooks = []config.Webhook{
			{ID: "chat", Name: "riverside-chat", Enabled: true, URL: discord.srv.URL + "/api/webhooks/1/x",
				Audience: config.AudiencePlayers},
			{ID: "off", Name: "old", Enabled: false, URL: discord.srv.URL + "/api/webhooks/2/y",
				Audience: config.AudiencePlayers},
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	job := func(webhook string) string {
		return `{"tasks":[{"serverId":"a","name":"Event","cron":"0 20 * * 5","enabled":true,"steps":[
		  {"kind":"discord","webhook":"` + webhook + `","message":"Night event on {server}, {players} online"}]}]}`
	}
	if rec := c.raw("/api/schedules", job("missing")); rec.Code != http.StatusBadRequest {
		t.Fatalf("a step pointing at no channel should be refused, got %d", rec.Code)
	}
	if rec := c.raw("/api/schedules", job("chat")); rec.Code != http.StatusOK {
		t.Fatalf("save failed: %s", rec.Body.String())
	}

	app.updateStatus("a", func(st *Status) { st.PlayerCount = 3 })
	step := app.cfg.Get().Schedules[0].Steps[0]
	srv, _ := app.cfg.Server("a")
	out, err := app.executeStep(context.Background(), step, srv)
	if err != nil || out != "posted to riverside-chat" {
		t.Fatalf("step failed: %q %v", out, err)
	}
	discord.mu.Lock()
	posted := len(discord.reqs)
	discord.mu.Unlock()
	if posted != 1 {
		t.Fatalf("expected one post, got %d", posted)
	}

	step.Webhook = "off"
	if _, err := app.executeStep(context.Background(), step, srv); err == nil || !strings.Contains(err.Error(), "switched off") {
		t.Fatalf("a switched-off channel should fail the step, got %v", err)
	}

	// Removing the channel the job uses is refused.
	rec := c.do(http.MethodPost, "/api/settings", map[string]any{"notify": map[string]any{
		"minIntervalSeconds": 300, "webhooks": []any{},
	}})
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "Event") {
		t.Fatalf("removing a channel a job uses should be refused naming the job: %d %s", rec.Code, rec.Body.String())
	}
}

func TestPublicInfoIsSavedAndShownOnTheCard(t *testing.T) {
	app, handler, _ := newTestApp(t)
	c := &client{t: t, handler: handler}
	c.setup("rick", "a-long-enough-password")
	seedServer(t, app, `{"id":"a","name":"Riverside","enabled":true,"gamePort":16261}`)

	rec := c.do(http.MethodPost, "/api/server/save", map[string]any{
		"id": "a", "name": "Riverside", "enabled": true,
		"public": map[string]any{"description": "  Hardcore, 8 slots  ", "address": "https://play.example.com:16300/"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("save failed: %s", rec.Body.String())
	}
	srv, _ := app.cfg.Server("a")
	if srv.Public.Description != "Hardcore, 8 slots" || srv.Public.Address != "play.example.com" || srv.Public.Port != 16300 {
		t.Fatalf("public info not cleaned up: %#v", srv.Public)
	}

	card := liveCardFor(srv, Status{Online: true, PlayerCount: 1}, false)
	if !strings.HasPrefix(card.Description, "Hardcore, 8 slots\n") || !strings.Contains(card.Description, "`play.example.com:16300`") {
		t.Fatalf("unexpected card:\n%s", card.Description)
	}
	srv.Public.Port = 0
	if got := joinAddress(srv); got != "play.example.com:16261" {
		t.Fatalf("a blank port should fall back to the game port, got %q", got)
	}
	srv.Public.Address = ""
	if strings.Contains(liveCardFor(srv, Status{Online: true}, false).Description, "Join") {
		t.Fatal("no address, no join line")
	}

	for _, bad := range []map[string]any{
		{"address": "play example com"},
		{"address": "play.example.com", "port": 70000},
		{"address": "host:notaport"},
	} {
		rec := c.do(http.MethodPost, "/api/server/save", map[string]any{"id": "a", "name": "Riverside", "public": bad})
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%v should be refused, got %d", bad, rec.Code)
		}
	}
}

func TestPostedAs(t *testing.T) {
	cfg := config.Config{Servers: []config.Server{{ID: "a", Name: "Riverside"}}}
	check := func(h config.Webhook, wantName, wantAvatar string) {
		t.Helper()
		name, avatar := postedAs(cfg, h)
		if name != wantName || avatar != wantAvatar {
			t.Errorf("got %q %q, want %q %q", name, avatar, wantName, wantAvatar)
		}
	}
	check(config.Webhook{}, "PZAdmin", "")
	check(config.Webhook{Server: "a"}, "Riverside", "")
	cfg.Notify.Identity = config.Identity{Name: "Knox Radio", AvatarURL: "https://cdn.example/radio.png"}
	check(config.Webhook{}, "Knox Radio", "https://cdn.example/radio.png")
	check(config.Webhook{Server: "a"}, "Riverside", "https://cdn.example/radio.png")
	check(config.Webhook{Server: "a", Identity: config.Identity{Name: "Riverside Radio", AvatarURL: "https://cdn.example/r.png"}},
		"Riverside Radio", "https://cdn.example/r.png")
}

func TestServerChannelsAndIdentityAreValidated(t *testing.T) {
	app, handler, _ := newTestApp(t)
	c := &client{t: t, handler: handler}
	c.setup("rick", "a-long-enough-password")
	seedServer(t, app, `{"id":"a","name":"Riverside","enabled":true}`)
	seedServer(t, app, `{"id":"b","name":"Louisville","enabled":true}`)
	save := func(notify map[string]any) *httptest.ResponseRecorder {
		notify["minIntervalSeconds"] = 300
		return c.do(http.MethodPost, "/api/settings", map[string]any{"notify": notify})
	}
	own := func(server string, extra map[string]any) map[string]any {
		h := map[string]any{"enabled": true, "url": "https://discord.com/api/webhooks/1/" + server,
			"server": server, "events": []string{"server.up"}, "servers": []string{"a", "b"}}
		for k, v := range extra {
			h[k] = v
		}
		return h
	}

	rec := save(map[string]any{
		"identity": map[string]any{"name": " Knox Radio ", "avatarUrl": "https://cdn.example/radio.png"},
		"webhooks": []any{own("a", nil), own("gone", nil)},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("save failed: %s", rec.Body.String())
	}
	n := app.cfg.Get().Notify
	if n.Identity.Name != "Knox Radio" {
		t.Fatalf("identity not saved: %#v", n.Identity)
	}
	if len(n.Webhooks) != 1 {
		t.Fatalf("a channel for a server that does not exist should be dropped: %#v", n.Webhooks)
	}
	h := n.Webhooks[0]
	if h.Name != "Riverside" || h.Audience != config.AudiencePlayers || len(h.Servers) != 1 || h.Servers[0] != "a" {
		t.Fatalf("a server's channel should be named after it and cover only it: %#v", h)
	}

	// A throttle-only save keeps the identity.
	if rec := c.do(http.MethodPost, "/api/settings", map[string]any{"notify": map[string]any{"minIntervalSeconds": 60}}); rec.Code != http.StatusOK {
		t.Fatal(rec.Body.String())
	}
	if app.cfg.Get().Notify.Identity.Name != "Knox Radio" {
		t.Fatal("a throttle-only save should keep the identity")
	}

	for name, notify := range map[string]map[string]any{
		"two channels for one server": {"webhooks": []any{own("a", nil), own("a", nil)}},
		"banned global name":          {"identity": map[string]any{"name": "Discord Bot"}, "webhooks": []any{}},
		"banned channel name":         {"webhooks": []any{own("a", map[string]any{"identity": map[string]any{"name": "clyde"}})}},
		"picture not https": {"webhooks": []any{own("a", map[string]any{
			"identity": map[string]any{"avatarUrl": "http://cdn.example/x.png"}})}},
	} {
		if rec := save(notify); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: expected 400, got %d %s", name, rec.Code, rec.Body.String())
		}
	}
}

// Every message a server's channel gets is posted under the server's name,
// including its status message.
func TestServerChannelPostsUnderTheServersName(t *testing.T) {
	app, _, dir := newTestApp(t)
	var mu sync.Mutex
	var bodies []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var m map[string]any
		_ = json.NewDecoder(r.Body).Decode(&m)
		mu.Lock()
		bodies = append(bodies, m)
		mu.Unlock()
		_, _ = io.WriteString(w, `{"id":"9"}`)
	}))
	defer srv.Close()
	seedServer(t, app, `{"id":"a","name":"Riverside","enabled":true}`)
	if _, err := app.cfg.Update(func(c *config.Config) error {
		c.Notify.Identity.AvatarURL = "https://cdn.example/logo.png"
		c.Notify.Webhooks = []config.Webhook{{ID: "r", Name: "Riverside", Enabled: true, URL: srv.URL + "/api/webhooks/1/x",
			Server: "a", Servers: []string{"a"}, Audience: config.AudiencePlayers,
			Events: []string{"server.up"}, LiveStatus: true}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	app.applyNotifyConfig(app.cfg.Get())
	app.updateStatus("a", func(st *Status) { st.Online, st.LastCheck = true, TimeNow() })
	app.syncLiveStatus(context.Background(), loadLiveBoard(filepath.Join(dir, "livestatus.json")))
	app.event(store.Event{Kind: "server.up", Severity: store.SevSuccess, ServerID: "a", Server: "Riverside"})

	deadline := time.Now().Add(3 * time.Second)
	for {
		mu.Lock()
		n := len(bodies)
		mu.Unlock()
		if n >= 2 || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(bodies) != 2 {
		t.Fatalf("expected a status message and an announcement, got %d", len(bodies))
	}
	for _, b := range bodies {
		if b["username"] != "Riverside" || b["avatar_url"] != "https://cdn.example/logo.png" {
			t.Fatalf("posted as %v / %v", b["username"], b["avatar_url"])
		}
	}
}

// fakeBotAPI is enough of Discord's API for the bot: one channel whose name
// can be read and changed, and a webhook that says it belongs to it.
type fakeBotAPI struct {
	mu      sync.Mutex
	srv     *httptest.Server
	name    string
	renames int
	limited bool
}

func newFakeBotAPI(t *testing.T) *fakeBotAPI {
	f := &fakeBotAPI{name: "riverside"}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		switch r.Method + " " + r.URL.Path {
		case "GET /api/webhooks/1/x":
			_, _ = io.WriteString(w, `{"channel_id":"123456789012345678"}`)
		case "GET /channels/123456789012345678":
			_ = json.NewEncoder(w).Encode(map[string]string{"id": "123456789012345678", "name": f.name})
		case "PATCH /channels/123456789012345678":
			if f.limited {
				w.WriteHeader(http.StatusTooManyRequests)
				_, _ = io.WriteString(w, `{"retry_after": 300}`)
				return
			}
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			f.name = body["name"]
			f.renames++
			_, _ = io.WriteString(w, `{}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeBotAPI) state() (string, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.name, f.renames
}

func TestChannelNameFollowsSettledState(t *testing.T) {
	app, _, dir := newTestApp(t)
	api := newFakeBotAPI(t)
	app.discordAPI = api.srv.URL
	now := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	clock = func() time.Time { return now }
	t.Cleanup(func() { clock = time.Now })

	seedServer(t, app, `{"id":"a","name":"Riverside","enabled":true}`)
	if _, err := app.cfg.Update(func(c *config.Config) error {
		c.Notify.Bot = config.Bot{Token: "tok"}
		// A webhook channel: the bot only renames it, and finds its ID from
		// the webhook.
		c.Notify.Webhooks = []config.Webhook{{ID: "r", Name: "Riverside", Enabled: true,
			URL: api.srv.URL + "/api/webhooks/1/x", Server: "a", Servers: []string{"a"},
			Audience: config.AudiencePlayers, RenameChannel: true}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	board := loadLiveBoard(filepath.Join(dir, "livestatus.json"))
	sync := func(advance time.Duration) (string, int) {
		now = now.Add(advance)
		app.syncChannelNames(context.Background(), board)
		return api.state()
	}
	set := func(fn func(*Status)) { app.updateStatus("a", fn) }

	// Not checked yet: the name is left alone.
	if name, n := sync(0); name != "riverside" || n != 0 {
		t.Fatalf("an unchecked server should not be named: %q %d", name, n)
	}
	// The first state shows straight away.
	set(func(st *Status) { st.Online, st.LastCheck = true, TimeNow() })
	if name, _ := sync(0); name != "🟢-riverside" {
		t.Fatalf("got %q", name)
	}

	// A restart PZAdmin asked for does not touch the name.
	app.markRestarting("a", "scheduled job")
	set(func(st *Status) { st.Online = false })
	sync(time.Minute)
	app.clearRestarting("a")
	set(func(st *Status) { st.Online = true })
	if name, n := sync(time.Minute); name != "🟢-riverside" || n != 1 {
		t.Fatalf("a restart should leave the name alone: %q after %d renames", name, n)
	}

	// A blip shorter than the settle time does not either.
	set(func(st *Status) { st.Online = false })
	sync(30 * time.Second)
	set(func(st *Status) { st.Online = true })
	if name, n := sync(30 * time.Second); name != "🟢-riverside" || n != 1 {
		t.Fatalf("a short blip should not rename: %q %d", name, n)
	}

	// A real outage does, once it has lasted.
	set(func(st *Status) { st.Online = false })
	sync(0)
	if name, _ := sync(dotSettle); name != "🔴-riverside" {
		t.Fatalf("an outage should show red, got %q", name)
	}

	// That was the second rename in ten minutes: coming back has to wait,
	// and then shows what is true by then.
	set(func(st *Status) { st.Online = true })
	if name, _ := sync(dotSettle); name != "🔴-riverside" {
		t.Fatalf("the rate limit should hold the name, got %q", name)
	}
	if name, n := sync(renameWindow); name != "🟢-riverside" || n != 3 {
		t.Fatalf("once allowed, the name should catch up: %q %d", name, n)
	}

	// Discord saying wait is respected.
	api.mu.Lock()
	api.limited = true
	api.mu.Unlock()
	set(func(st *Status) { st.Online = false })
	sync(0)
	sync(dotSettle)
	api.mu.Lock()
	api.limited = false
	api.mu.Unlock()
	if name, _ := sync(time.Minute); name != "🟢-riverside" {
		t.Fatalf("a rate-limited rename should wait out Discord's retry_after, got %q", name)
	}
	if name, _ := sync(5 * time.Minute); name != "🔴-riverside" {
		t.Fatalf("after the wait the rename should go through, got %q", name)
	}

	// Turning it off restores the plain name.
	if _, err := app.cfg.Update(func(c *config.Config) error {
		c.Notify.Webhooks[0].RenameChannel = false
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if name, _ := sync(renameWindow); name != "riverside" {
		t.Fatalf("switching off should restore the name, got %q", name)
	}
}

// Connecting a bot checks the token, brings the bot online once, and keeps
// the token out of everything sent to the browser.
func TestBotConnect(t *testing.T) {
	app, handler, _ := newTestApp(t)
	c := &client{t: t, handler: handler}
	c.setup("rick", "a-long-enough-password")
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bot good-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/users/@me":
			_, _ = io.WriteString(w, `{"id":"42","username":"Knox Radio"}`)
		case "/users/@me/guilds":
			_, _ = io.WriteString(w, `[{"id":"100000000000000001","name":"Zomboid Crew"}]`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer api.Close()
	app.discordAPI = api.URL
	// No gateway to reach here: connecting still works, with a warning.
	app.discordGateway = "ws://127.0.0.1:1"

	if rec := c.do(http.MethodPost, "/api/discord/bot", map[string]string{"token": "wrong"}); rec.Code != http.StatusBadRequest {
		t.Fatalf("a bad token should be refused, got %d", rec.Code)
	}
	rec := c.do(http.MethodPost, "/api/discord/bot", map[string]string{"token": "Bot good-token"})
	if rec.Code != http.StatusOK {
		t.Fatalf("connect failed: %s", rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "Knox Radio") || !strings.Contains(body, "Zomboid Crew") || !strings.Contains(body, "could not come online") {
		t.Fatalf("unexpected response: %s", body)
	}
	if got := app.cfg.Get().Notify.Bot; got.Token != "good-token" || got.Name != "Knox Radio" {
		t.Fatalf("bot not saved: %#v", got)
	}
	for _, path := range []string{"/api/state", "/api/export"} {
		if strings.Contains(c.do(http.MethodGet, path, nil).Body.String(), "good-token") {
			t.Fatalf("%s leaked the bot token", path)
		}
	}

	// A settings save cannot change or drop the bot.
	if rec := c.do(http.MethodPost, "/api/settings", map[string]any{"notify": map[string]any{
		"minIntervalSeconds": 300, "webhooks": []any{}, "bot": map[string]string{"token": "evil"}}}); rec.Code != http.StatusOK {
		t.Fatal(rec.Body.String())
	}
	if app.cfg.Get().Notify.Bot.Token != "good-token" {
		t.Fatal("the settings form must not be able to change the bot token")
	}

	// A channel through the bot needs a channel ID; with one it saves.
	rec = c.do(http.MethodPost, "/api/settings", map[string]any{"notify": map[string]any{
		"minIntervalSeconds": 300, "webhooks": []any{map[string]any{
			"name": "Chat", "enabled": true, "audience": "players", "channelId": "123456789012345678",
			"url": "https://discord.com/api/webhooks/9/ignored", "liveStatus": true, "events": []string{"server.up"}}}}})
	if rec.Code != http.StatusOK {
		t.Fatalf("a bot channel should save: %s", rec.Body.String())
	}
	if h := app.cfg.Get().Notify.Webhooks[0]; h.URL != "" || h.ChannelID != "123456789012345678" {
		t.Fatalf("a bot channel should not keep a webhook address: %#v", h)
	}

	// The bot cannot be removed while a channel uses it.
	if rec := c.do(http.MethodPost, "/api/discord/bot/remove", map[string]any{}); rec.Code != http.StatusBadRequest ||
		!strings.Contains(rec.Body.String(), "Chat") {
		t.Fatalf("removing a bot in use should be refused naming the channel: %d %s", rec.Code, rec.Body.String())
	}
}

func TestBotOnlyOptionsNeedTheBot(t *testing.T) {
	app, handler, _ := newTestApp(t)
	c := &client{t: t, handler: handler}
	c.setup("rick", "a-long-enough-password")
	seedServer(t, app, `{"id":"a","name":"Riverside","enabled":true}`)
	for name, hook := range map[string]map[string]any{
		"channel ID without a bot": {"enabled": true, "channelId": "123456789012345678", "audience": "players"},
		"rename without a bot": {"enabled": true, "url": "https://discord.com/api/webhooks/1/a", "server": "a",
			"renameChannel": true},
	} {
		rec := c.do(http.MethodPost, "/api/settings", map[string]any{"notify": map[string]any{
			"minIntervalSeconds": 300, "webhooks": []any{hook}}})
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: expected 400, got %d", name, rec.Code)
		}
	}
	if _, err := app.cfg.Update(func(c *config.Config) error { c.Notify.Bot.Token = "tok"; return nil }); err != nil {
		t.Fatal(err)
	}
	for name, hook := range map[string]map[string]any{
		"not a channel ID": {"enabled": true, "channelId": "general", "audience": "players"},
		"rename on a shared channel": {"enabled": true, "url": "https://discord.com/api/webhooks/1/a",
			"renameChannel": true, "audience": "players"},
	} {
		rec := c.do(http.MethodPost, "/api/settings", map[string]any{"notify": map[string]any{
			"minIntervalSeconds": 300, "webhooks": []any{hook}}})
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: expected 400, got %d", name, rec.Code)
		}
	}
}
