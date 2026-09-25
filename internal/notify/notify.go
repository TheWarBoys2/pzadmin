// Package notify delivers events to outbound webhooks.
//
// The payload is Discord-compatible, which also happens to work with Slack
// bridges and most self-hosted receivers. Delivery is fire-and-forget on a
// bounded queue: a webhook that is slow, down, or rate limiting must never
// delay a server restart.
//
// Each destination has an audience. Staff get every event PZAdmin records,
// with the detail an operator needs. Players get a handful of short
// announcements (restarting, back online) in plain language, because a public
// channel is no place for "Saved and quit over RCON".
package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Audiences a destination can be written for.
const (
	Staff   = "staff"
	Players = "players"
)

// Restart reasons, carried on restart events so player announcements can say
// why without exposing how.
const (
	ReasonScheduled = "scheduled"
	ReasonMods      = "mods"
	ReasonManual    = "manual"
	ReasonCrash     = "crash"
)

// Message is one notification.
type Message struct {
	Kind     string
	Title    string
	Body     string
	Server   string
	ServerID string
	Severity string // info | warn | error | success
	// Reason is why a server is restarting: one of the Reason constants, or
	// empty when there is nothing worth telling players.
	Reason string
	// Minutes is the countdown to a restart announced alongside the event,
	// when there is one.
	Minutes int
	At      time.Time
}

// Destination is one webhook and what it should be sent.
type Destination struct {
	ID       string
	URL      string
	Audience string
	Events   []string
	// Servers limits delivery to these server IDs. Empty means every server.
	Servers []string
	// Messages overrides player wording by event kind.
	Messages map[string]string
}

// Config is the notifier's runtime configuration, refreshed on every send so
// changes in the settings screen take effect immediately.
type Config struct {
	Destinations []Destination
	// MinInterval throttles repeats of the same staff alert.
	MinInterval time.Duration
}

// PlayerTemplates is the default wording of each player announcement.
// {server} is the server's name, {reason} a phrase saying why it is
// restarting (it may be empty), and {minutes} the countdown to a restart.
var PlayerTemplates = map[string]string{
	"server.restart": "🔄 **{server}** is restarting{reason}. It should be back in a few minutes.",
	"server.up":      "✅ **{server}** is back online.",
	"server.down":    "⚠️ **{server}** has gone offline unexpectedly.",
	"server.stop":    "⏸️ **{server}** has been shut down for now.",
	"mods.update":    "📦 Mods have updated. **{server}** will restart in {minutes} minutes to load them.",
}

var reasonPhrases = map[string]string{
	ReasonScheduled: " for its scheduled restart",
	ReasonMods:      " to load mod updates",
	ReasonCrash:     " because it stopped responding",
}

// playerRepeat suppresses a player announcement repeated within this window.
// It is short on purpose: two genuine restarts are minutes apart, and a
// second one must not be swallowed the way a staff throttle would.
const playerRepeat = 60 * time.Second

type delivery struct {
	dest Destination
	msg  Message
}

// Notifier delivers messages to webhooks.
type Notifier struct {
	client *http.Client
	queue  chan delivery

	mu       sync.RWMutex
	cfg      Config
	lastSent map[string]time.Time

	stop chan struct{}
	once sync.Once
	wg   sync.WaitGroup
}

// New starts a notifier with a background delivery worker.
func New() *Notifier {
	n := &Notifier{
		client:   &http.Client{Timeout: 10 * time.Second},
		queue:    make(chan delivery, 128),
		lastSent: map[string]time.Time{},
		stop:     make(chan struct{}),
	}
	n.wg.Add(1)
	go n.run()
	return n
}

// Configure replaces the notifier's settings.
func (n *Notifier) Configure(c Config) {
	if c.MinInterval <= 0 {
		c.MinInterval = 5 * time.Minute
	}
	n.mu.Lock()
	n.cfg = c
	n.mu.Unlock()
}

// Send queues a message for every destination that wants it. It never
// blocks; if the queue is full the message is dropped and the caller carries
// on.
func (n *Notifier) Send(m Message) {
	if m.At.IsZero() {
		m.At = time.Now()
	}
	n.mu.RLock()
	dests := n.cfg.Destinations
	n.mu.RUnlock()
	for _, d := range dests {
		kind := d.kindFor(m)
		if kind == "" || !d.covers(m.ServerID) || n.throttled(d, kind, m) {
			continue
		}
		select {
		case n.queue <- delivery{dest: d, msg: m}:
		default:
		}
	}
}

// kindFor returns the event this message counts as for d, or "" when d does
// not want it.
func (d Destination) kindFor(m Message) string {
	kind := m.Kind
	if d.Audience == Players {
		kind = PlayerKind(m)
	}
	if kind == "" || !strings.HasPrefix(d.URL, "http") {
		return ""
	}
	for _, e := range d.Events {
		if e == kind {
			return kind
		}
	}
	return ""
}

// PlayerKind maps an event onto the announcement players would see for it,
// or "" when there is nothing to tell them. A watchdog restart is a restart
// as far as players are concerned, and a mod update only matters to them
// when it comes with a countdown.
func PlayerKind(m Message) string {
	switch m.Kind {
	case "server.recovered":
		// The attempt, not the report that it failed.
		if m.Severity == "warn" {
			return "server.restart"
		}
		return ""
	case "server.restart":
		// A restart that failed to start is staff business; if the server
		// then drops, server.down says so.
		if m.Severity == "error" {
			return ""
		}
	case "mods.update":
		if m.Minutes <= 0 {
			return ""
		}
	}
	if _, ok := PlayerTemplates[m.Kind]; ok {
		return m.Kind
	}
	return ""
}

func (d Destination) covers(serverID string) bool {
	if len(d.Servers) == 0 {
		return true
	}
	for _, id := range d.Servers {
		if id == serverID {
			return true
		}
	}
	return false
}

// throttled suppresses repeats of the same event for the same server. A server
// flapping every ten seconds should not produce 360 messages an hour.
func (n *Notifier) throttled(d Destination, kind string, m Message) bool {
	server := m.ServerID
	if server == "" {
		server = m.Server
	}
	k := d.ID + "|" + kind + "|" + server
	n.mu.Lock()
	defer n.mu.Unlock()
	window := n.cfg.MinInterval
	if d.Audience == Players {
		window = playerRepeat
	}
	if last, ok := n.lastSent[k]; ok && time.Since(last) < window {
		return true
	}
	n.lastSent[k] = time.Now()
	if len(n.lastSent) > 500 {
		cutoff := time.Now().Add(-time.Hour)
		for key, t := range n.lastSent {
			if t.Before(cutoff) {
				delete(n.lastSent, key)
			}
		}
	}
	return false
}

func (n *Notifier) run() {
	defer n.wg.Done()
	for {
		select {
		case d := <-n.queue:
			n.deliver(d)
		case <-n.stop:
			return
		}
	}
}

// Test sends a message immediately and reports the outcome, for the "send a
// test notification" button.
func (n *Notifier) Test(d Destination) error {
	if !strings.HasPrefix(d.URL, "http") {
		return fmt.Errorf("the webhook address must start with http:// or https://")
	}
	if d.Audience == Players {
		return n.post(d.URL, map[string]any{
			"content":          "👋 This channel will get server announcements from PZAdmin.",
			"allowed_mentions": map[string]any{"parse": []string{}},
		})
	}
	return n.post(d.URL, staffPayload(Message{
		Kind: "test", Title: "PZAdmin test notification",
		Body: "If you can read this, notifications are working.", Severity: "success", At: time.Now(),
	}))
}

func (n *Notifier) deliver(d delivery) {
	var payload map[string]any
	if d.dest.Audience == Players {
		payload = playerPayload(d.dest, d.msg)
	} else {
		payload = staffPayload(d.msg)
	}
	// One retry: most failures are a momentary network blip.
	if err := n.post(d.dest.URL, payload); err != nil {
		time.Sleep(3 * time.Second)
		_ = n.post(d.dest.URL, payload)
	}
}

var colours = map[string]int{
	"info":    0x8a8272,
	"success": 0x7fa650,
	"warn":    0xd38b3a,
	"error":   0xc4553f,
}

// Colour returns the embed colour for a severity.
func Colour(severity string) int {
	if c, ok := colours[severity]; ok {
		return c
	}
	return colours["info"]
}

func staffPayload(m Message) map[string]any {
	title := m.Title
	if m.Server != "" {
		title = m.Server + " — " + title
	}
	return map[string]any{
		"username": "PZAdmin",
		"embeds": []map[string]any{{
			"title":       title,
			"description": m.Body,
			"color":       Colour(m.Severity),
			"timestamp":   m.At.UTC().Format(time.RFC3339),
			"footer":      map[string]any{"text": "PZAdmin"},
		}},
		// Player names end up in staff alerts, and a player called @everyone
		// must not be able to ping the whole server.
		"allowed_mentions": map[string]any{"parse": []string{}},
	}
}

// playerPayload is a plain message rather than an embed: it reads like an
// announcement, and a role mention written into the wording (<@&id>) pings.
// No username is sent, so the name and avatar set on the webhook in Discord
// are the ones players see.
func playerPayload(d Destination, m Message) map[string]any {
	return map[string]any{
		"content": RenderPlayer(d.Messages, m),
		// Roles the operator wrote into their wording may ping; @everyone
		// and user mentions may not.
		"allowed_mentions": map[string]any{"parse": []string{"roles"}},
	}
}

// RenderPlayer fills in the player wording for m, preferring the operator's
// own over the default.
func RenderPlayer(custom map[string]string, m Message) string {
	kind := PlayerKind(m)
	tmpl := strings.TrimSpace(custom[kind])
	if tmpl == "" {
		tmpl = PlayerTemplates[kind]
	}
	out := strings.NewReplacer(
		"{server}", m.Server,
		"{reason}", reasonPhrases[m.Reason],
		"{minutes}", strconv.Itoa(m.Minutes),
	).Replace(tmpl)
	return truncate(out, 2000)
}

func (n *Notifier) post(url string, payload map[string]any) error {
	resp, err := n.do(context.Background(), http.MethodPost, url, payload)
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

// do sends a request and turns any non-2xx response into an error.
func (n *Notifier) do(ctx context.Context, method, url string, payload any) (*http.Response, error) {
	var body io.Reader
	if payload != nil {
		b, err := json.Marshal(payload)
		if err != nil {
			return nil, err
		}
		body = bytes.NewReader(b)
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		cancel()
		return nil, err
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := n.client.Do(req)
	if err != nil {
		cancel()
		return nil, err
	}
	if resp.StatusCode >= 300 {
		defer cancel()
		defer resp.Body.Close()
		return nil, statusError(resp)
	}
	resp.Body = &cancelOnClose{ReadCloser: resp.Body, cancel: cancel}
	return resp, nil
}

type cancelOnClose struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (c *cancelOnClose) Close() error {
	err := c.ReadCloser.Close()
	c.cancel()
	return err
}

// ErrGone reports that a message or webhook no longer exists.
var ErrGone = errors.New("the message or webhook no longer exists")

// RateLimited reports that Discord asked for a pause.
type RateLimited struct{ RetryAfter time.Duration }

func (r *RateLimited) Error() string {
	return "rate limited for " + r.RetryAfter.Round(time.Second).String()
}

func statusError(resp *http.Response) error {
	switch resp.StatusCode {
	case http.StatusNotFound:
		return ErrGone
	case http.StatusTooManyRequests:
		wait := 5 * time.Second
		if s, err := strconv.ParseFloat(resp.Header.Get("Retry-After"), 64); err == nil && s > 0 {
			wait = time.Duration(s * float64(time.Second))
		}
		return &RateLimited{RetryAfter: wait}
	}
	return fmt.Errorf("webhook returned %s", resp.Status)
}

// --- live status cards ------------------------------------------------------

// Card is a message PZAdmin keeps up to date in place rather than posting
// anew.
type Card struct {
	Title       string
	Description string
	Colour      int
	Footer      string
	At          time.Time
}

func (c Card) payload() map[string]any {
	embed := map[string]any{
		"title":       truncate(c.Title, 256),
		"description": truncate(c.Description, 4000),
		"color":       c.Colour,
	}
	if c.Footer != "" {
		embed["footer"] = map[string]any{"text": truncate(c.Footer, 2048)}
	}
	if !c.At.IsZero() {
		embed["timestamp"] = c.At.UTC().Format(time.RFC3339)
	}
	return map[string]any{
		"embeds":           []map[string]any{embed},
		"allowed_mentions": map[string]any{"parse": []string{}},
	}
}

// IsDiscord reports whether a webhook address is Discord's, which is the only
// kind whose messages can be edited afterwards.
func IsDiscord(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" {
		return false
	}
	switch strings.ToLower(u.Hostname()) {
	case "discord.com", "discordapp.com", "ptb.discord.com", "canary.discord.com":
		return strings.HasPrefix(u.Path, "/api/webhooks/")
	}
	return false
}

// messageURL addresses one message sent through a webhook, keeping any
// thread_id so edits land in the same thread.
func messageURL(webhook, id string) (string, error) {
	u, err := url.Parse(webhook)
	if err != nil {
		return "", err
	}
	u.Path = strings.TrimSuffix(u.Path, "/") + "/messages/" + url.PathEscape(id)
	return u.String(), nil
}

// PostCard posts a new card and returns its message ID.
func (n *Notifier) PostCard(ctx context.Context, webhook string, c Card) (string, error) {
	u, err := url.Parse(webhook)
	if err != nil {
		return "", err
	}
	q := u.Query()
	// Without wait=true Discord answers 204 and never says what it created.
	q.Set("wait", "true")
	u.RawQuery = q.Encode()
	resp, err := n.do(ctx, http.MethodPost, u.String(), c.payload())
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var msg struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&msg); err != nil || msg.ID == "" {
		return "", fmt.Errorf("the webhook did not say which message it created; live status needs a Discord webhook")
	}
	return msg.ID, nil
}

// EditCard replaces the content of a card posted earlier. It returns ErrGone
// when someone has deleted the message.
func (n *Notifier) EditCard(ctx context.Context, webhook, id string, c Card) error {
	target, err := messageURL(webhook, id)
	if err != nil {
		return err
	}
	resp, err := n.do(ctx, http.MethodPatch, target, c.payload())
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

// DeleteCard removes a card. A message that is already gone is not an error.
func (n *Notifier) DeleteCard(ctx context.Context, webhook, id string) error {
	target, err := messageURL(webhook, id)
	if err != nil {
		return err
	}
	resp, err := n.do(ctx, http.MethodDelete, target, nil)
	if errors.Is(err, ErrGone) {
		return nil
	}
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

// Close stops the delivery worker.
func (n *Notifier) Close() {
	n.once.Do(func() {
		close(n.stop)
		n.wg.Wait()
	})
}
