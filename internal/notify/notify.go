// Package notify delivers events to an outbound webhook.
//
// The payload is Discord-compatible, which also happens to work with Slack
// bridges and most self-hosted receivers. Delivery is fire-and-forget on a
// bounded queue: a webhook that is slow, down, or rate limiting must never
// delay a server restart.
package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Message is one notification.
type Message struct {
	Kind     string
	Title    string
	Body     string
	Server   string
	Severity string // info | warn | error | success
	At       time.Time
}

// Config is the notifier's runtime configuration, refreshed on every send so
// changes in the settings screen take effect immediately.
type Config struct {
	Enabled     bool
	URL         string
	Events      []string
	MinInterval time.Duration
}

// Notifier delivers messages to a webhook.
type Notifier struct {
	client *http.Client
	queue  chan Message

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
		queue:    make(chan Message, 128),
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

// Enabled reports whether a webhook is configured and switched on.
func (n *Notifier) Enabled() bool {
	n.mu.RLock()
	defer n.mu.RUnlock()
	return n.cfg.Enabled && strings.HasPrefix(n.cfg.URL, "http")
}

// Send queues a message. It never blocks; if the queue is full the message is
// dropped and the caller carries on.
func (n *Notifier) Send(m Message) {
	if !n.Enabled() || !n.wants(m.Kind) {
		return
	}
	if m.At.IsZero() {
		m.At = time.Now()
	}
	if n.throttled(m) {
		return
	}
	select {
	case n.queue <- m:
	default:
	}
}

func (n *Notifier) wants(kind string) bool {
	n.mu.RLock()
	defer n.mu.RUnlock()
	if len(n.cfg.Events) == 0 {
		return true
	}
	for _, e := range n.cfg.Events {
		if e == kind {
			return true
		}
	}
	return false
}

// throttled suppresses repeats of the same event for the same server. A server
// flapping every ten seconds should not produce 360 messages an hour.
func (n *Notifier) throttled(m Message) bool {
	k := m.Kind + "|" + m.Server
	n.mu.Lock()
	defer n.mu.Unlock()
	if last, ok := n.lastSent[k]; ok && time.Since(last) < n.cfg.MinInterval {
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
		case m := <-n.queue:
			n.deliver(m)
		case <-n.stop:
			return
		}
	}
}

// Test sends a message immediately and reports the outcome, for the "send a
// test notification" button.
func (n *Notifier) Test(url string) error {
	if !strings.HasPrefix(url, "http") {
		return fmt.Errorf("the webhook address must start with http:// or https://")
	}
	return n.post(url, Message{
		Kind: "test", Title: "PZAdmin test notification",
		Body: "If you can read this, notifications are working.", Severity: "success", At: time.Now(),
	})
}

func (n *Notifier) deliver(m Message) {
	n.mu.RLock()
	url := n.cfg.URL
	n.mu.RUnlock()
	if url == "" {
		return
	}
	// One retry: most failures are a momentary network blip.
	if err := n.post(url, m); err != nil {
		time.Sleep(3 * time.Second)
		_ = n.post(url, m)
	}
}

var colours = map[string]int{
	"info":    0x8a8272,
	"success": 0x7fa650,
	"warn":    0xd38b3a,
	"error":   0xc4553f,
}

func (n *Notifier) post(url string, m Message) error {
	colour, ok := colours[m.Severity]
	if !ok {
		colour = colours["info"]
	}
	title := m.Title
	if m.Server != "" {
		title = m.Server + " — " + title
	}
	payload := map[string]any{
		"username": "PZAdmin",
		"embeds": []map[string]any{{
			"title":       title,
			"description": m.Body,
			"color":       colour,
			"timestamp":   m.At.UTC().Format(time.RFC3339),
			"footer":      map[string]any{"text": "PZAdmin"},
		}},
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := n.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("webhook returned %s", resp.Status)
	}
	return nil
}

// Close stops the delivery worker.
func (n *Notifier) Close() {
	n.once.Do(func() {
		close(n.stop)
		n.wg.Wait()
	})
}
