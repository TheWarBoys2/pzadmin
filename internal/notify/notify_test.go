package notify

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// hook records every request a webhook receives.
type hook struct {
	mu   sync.Mutex
	got  []map[string]any
	reqs []string
	srv  *httptest.Server
	// status, when set, is returned instead of success.
	status int
}

func newHook(t *testing.T) *hook {
	h := &hook{}
	h.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var m map[string]any
		_ = json.Unmarshal(body, &m)
		h.mu.Lock()
		h.got = append(h.got, m)
		h.reqs = append(h.reqs, r.Method+" "+r.URL.RequestURI())
		status := h.status
		h.mu.Unlock()
		if status != 0 {
			w.WriteHeader(status)
			return
		}
		if r.URL.Query().Get("wait") == "true" {
			_, _ = io.WriteString(w, `{"id":"1001"}`)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(h.srv.Close)
	return h
}

func (h *hook) payloads() []map[string]any {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]map[string]any(nil), h.got...)
}

func (h *hook) requests() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.reqs...)
}

func waitFor(t *testing.T, h *hook, n int) []map[string]any {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if got := h.payloads(); len(got) >= n {
			return got
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("expected %d deliveries, got %d", n, len(h.payloads()))
	return nil
}

func content(m map[string]any) string {
	s, _ := m["content"].(string)
	return s
}

// Staff and players read different channels and get different messages from
// the same event.
func TestRoutesEachEventByAudience(t *testing.T) {
	staff, players := newHook(t), newHook(t)
	n := New()
	defer n.Close()
	n.Configure(Config{Destinations: []Destination{
		{ID: "s", URL: staff.srv.URL, Audience: Staff, Events: []string{"server.restart", "backup.failed"}},
		{ID: "p", URL: players.srv.URL, Audience: Players, Events: []string{"server.restart", "server.up"}},
	}})

	n.Send(Message{Kind: "server.restart", Title: "Riverside restarting", Server: "Riverside", ServerID: "a",
		Body: "Saved and quit over RCON", Severity: "warn", Reason: ReasonMods})
	n.Send(Message{Kind: "backup.failed", Title: "Backup failed", Server: "Riverside", ServerID: "a", Severity: "error"})

	got := waitFor(t, players, 1)
	if text := content(got[0]); text != "🔄 **Riverside** is restarting to load mod updates. It should be back in a few minutes." {
		t.Fatalf("unexpected player wording %q", text)
	}
	if strings.Contains(content(got[0]), "RCON") {
		t.Fatal("players must not see staff detail")
	}
	if _, named := got[0]["username"]; named {
		t.Fatal("player messages should use the name set on the webhook in Discord")
	}

	staffGot := waitFor(t, staff, 2)
	if _, ok := staffGot[0]["embeds"]; !ok {
		t.Fatal("staff alerts are embeds")
	}
	time.Sleep(50 * time.Millisecond)
	if len(players.payloads()) != 1 {
		t.Fatal("a backup failure is not player business")
	}
}

func TestScopesDestinationsToServers(t *testing.T) {
	h := newHook(t)
	n := New()
	defer n.Close()
	n.Configure(Config{Destinations: []Destination{
		{ID: "p", URL: h.srv.URL, Audience: Players, Events: []string{"server.up"}, Servers: []string{"b"}},
	}})
	n.Send(Message{Kind: "server.up", Server: "Riverside", ServerID: "a", Severity: "success"})
	n.Send(Message{Kind: "server.up", Server: "Louisville", ServerID: "b", Severity: "success"})
	got := waitFor(t, h, 1)
	time.Sleep(50 * time.Millisecond)
	if len(h.payloads()) != 1 || !strings.Contains(content(got[0]), "Louisville") {
		t.Fatalf("only the scoped server should be announced: %v", h.payloads())
	}
}

func TestPlayerKind(t *testing.T) {
	cases := []struct {
		m    Message
		want string
	}{
		{Message{Kind: "server.restart", Severity: "warn"}, "server.restart"},
		{Message{Kind: "server.restart", Severity: "error"}, ""},
		{Message{Kind: "server.recovered", Severity: "warn"}, "server.restart"},
		{Message{Kind: "server.recovered", Severity: "error"}, ""},
		{Message{Kind: "mods.update", Severity: "warn"}, ""},
		{Message{Kind: "mods.update", Severity: "warn", Minutes: 10}, "mods.update"},
		{Message{Kind: "server.down", Severity: "error"}, "server.down"},
		{Message{Kind: "player.join"}, ""},
		{Message{Kind: "admin.action"}, ""},
	}
	for _, c := range cases {
		if got := PlayerKind(c.m); got != c.want {
			t.Errorf("%s/%s/%d: got %q, want %q", c.m.Kind, c.m.Severity, c.m.Minutes, got, c.want)
		}
	}
}

// A watchdog restart is still a restart to a player, and says why.
func TestWatchdogRestartIsAnnouncedAsARestart(t *testing.T) {
	text := RenderPlayer(nil, Message{Kind: "server.recovered", Severity: "warn", Server: "Riverside", Reason: ReasonCrash})
	if !strings.Contains(text, "is restarting because it stopped responding") {
		t.Fatalf("unexpected wording %q", text)
	}
}

func TestCustomWording(t *testing.T) {
	custom := map[string]string{"server.up": "<@&42> {server} is up, get in here"}
	text := RenderPlayer(custom, Message{Kind: "server.up", Server: "Riverside"})
	if text != "<@&42> Riverside is up, get in here" {
		t.Fatalf("got %q", text)
	}
	text = RenderPlayer(custom, Message{Kind: "mods.update", Server: "Riverside", Minutes: 5})
	if !strings.Contains(text, "in 5 minutes") {
		t.Fatalf("unset wording should fall back to the default: %q", text)
	}
}

// Nothing a player types, and nothing an operator did not write, may ping.
func TestMentionsAreRestricted(t *testing.T) {
	staff := staffPayload(Message{Kind: "player.join", Title: "@everyone joined"})
	if parse := staff["allowed_mentions"].(map[string]any)["parse"].([]string); len(parse) != 0 {
		t.Fatalf("staff alerts must not ping: %v", parse)
	}
	player := playerPayload(Destination{}, Message{Kind: "server.up", Server: "Riverside"})
	if parse := player["allowed_mentions"].(map[string]any)["parse"].([]string); len(parse) != 1 || parse[0] != "roles" {
		t.Fatalf("player announcements may ping roles only: %v", parse)
	}
}

// Two different announcements are both delivered, and a second genuine
// restart a few minutes later is not swallowed by the staff throttle.
func TestPlayerRepeatWindowIsShort(t *testing.T) {
	h := newHook(t)
	n := New()
	defer n.Close()
	n.Configure(Config{MinInterval: time.Hour, Destinations: []Destination{
		{ID: "p", URL: h.srv.URL, Audience: Players, Events: []string{"server.restart", "server.up"}},
	}})
	m := Message{Kind: "server.restart", Server: "Riverside", ServerID: "a", Severity: "warn"}
	n.Send(m)
	n.Send(m) // an echo of the same restart
	n.Send(Message{Kind: "server.up", Server: "Riverside", ServerID: "a", Severity: "success"})
	waitFor(t, h, 2)

	// Pretend the first restart was a while ago.
	n.mu.Lock()
	for k := range n.lastSent {
		n.lastSent[k] = time.Now().Add(-2 * playerRepeat)
	}
	n.mu.Unlock()
	n.Send(m)
	waitFor(t, h, 3)
	time.Sleep(50 * time.Millisecond)
	if got := len(h.payloads()); got != 3 {
		t.Fatalf("expected 3 announcements, got %d", got)
	}
}

func TestCards(t *testing.T) {
	h := newHook(t)
	n := New()
	defer n.Close()
	ctx := context.Background()
	url := h.srv.URL + "/api/webhooks/1/abc?thread_id=77"

	id, err := n.PostCard(ctx, url, Card{Title: "Riverside", Description: "🟢 Online"})
	if err != nil || id != "1001" {
		t.Fatalf("post: %q %v", id, err)
	}
	if err := n.EditCard(ctx, url, id, Card{Title: "Riverside", Description: "🔴 Offline"}); err != nil {
		t.Fatal(err)
	}
	if err := n.DeleteCard(ctx, url, id); err != nil {
		t.Fatal(err)
	}
	reqs := h.requests()
	want := []string{
		"POST /api/webhooks/1/abc?thread_id=77&wait=true",
		"PATCH /api/webhooks/1/abc/messages/1001?thread_id=77",
		"DELETE /api/webhooks/1/abc/messages/1001?thread_id=77",
	}
	if strings.Join(reqs, "\n") != strings.Join(want, "\n") {
		t.Fatalf("requests:\n%s\nwant:\n%s", strings.Join(reqs, "\n"), strings.Join(want, "\n"))
	}

	h.mu.Lock()
	h.status = http.StatusNotFound
	h.mu.Unlock()
	if err := n.EditCard(ctx, url, id, Card{}); !errors.Is(err, ErrGone) {
		t.Fatalf("a deleted message should be reported as gone, got %v", err)
	}
	if err := n.DeleteCard(ctx, url, id); err != nil {
		t.Fatalf("deleting a message that is already gone is fine, got %v", err)
	}

	h.mu.Lock()
	h.status = http.StatusTooManyRequests
	h.mu.Unlock()
	var limited *RateLimited
	if err := n.EditCard(ctx, url, id, Card{}); !errors.As(err, &limited) {
		t.Fatalf("expected a rate limit, got %v", err)
	}
}

func TestIsDiscord(t *testing.T) {
	for url, want := range map[string]bool{
		"https://discord.com/api/webhooks/1/abc":        true,
		"https://discordapp.com/api/webhooks/1/abc":     true,
		"https://canary.discord.com/api/webhooks/1/abc": true,
		"http://discord.com/api/webhooks/1/abc":         false,
		"https://discord.com.evil.example/api/webhooks": false,
		"https://hooks.slack.com/services/x":            false,
		"https://discord.com/channels/1":                false,
	} {
		if got := IsDiscord(url); got != want {
			t.Errorf("IsDiscord(%q) = %v, want %v", url, got, want)
		}
	}
}

// What the operator typed when restarting or stopping is what players read.
func TestOperatorReasonIsShownToPlayers(t *testing.T) {
	text := RenderPlayer(nil, Message{Kind: "server.restart", Severity: "warn", Server: "Riverside",
		Reason: ReasonManual, Note: "installing a new map"})
	if !strings.Contains(text, "is restarting (installing a new map).") {
		t.Fatalf("got %q", text)
	}
	text = RenderPlayer(nil, Message{Kind: "server.stop", Server: "Riverside", Note: "maintenance"})
	if text != "⏸️ **Riverside** has been shut down for now (maintenance)." {
		t.Fatalf("got %q", text)
	}
	text = RenderPlayer(nil, Message{Kind: "server.stop", Server: "Riverside"})
	if text != "⏸️ **Riverside** has been shut down for now." {
		t.Fatalf("without a reason nothing is added, got %q", text)
	}
}
