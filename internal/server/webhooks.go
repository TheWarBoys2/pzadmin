package server

import (
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/TheWarBoys2/pzadmin/internal/config"
	"github.com/TheWarBoys2/pzadmin/internal/notify"
)

// settingsError is a settings form the operator has to correct, as opposed
// to a failure to save it.
type settingsError struct{ msg string }

func (e *settingsError) Error() string { return e.msg }

func invalidf(format string, args ...any) error {
	return &settingsError{fmt.Sprintf(format, args...)}
}

var webhookID = regexp.MustCompile(`^[a-z0-9-]{1,32}$`)

// maxTemplate keeps custom wording inside Discord's 2000 character message
// limit once the server name is filled in.
const maxTemplate = 1500

// validateWebhooks checks the webhook list from the settings form and returns
// it cleaned up. Addresses the browser sent back as the placeholder are
// restored from prev.
func validateWebhooks(next, prev []config.Webhook, servers []config.Server) ([]config.Webhook, error) {
	if len(next) > 20 {
		return nil, invalidf("at most 20 webhooks can be configured")
	}
	known := map[string]bool{}
	for _, s := range servers {
		known[s.ID] = true
	}
	out := make([]config.Webhook, 0, len(next))
	next = append([]config.Webhook(nil), next...)
	config.UnredactWebhooks(next, prev)
	seen := map[string]bool{}
	for _, h := range next {
		if !webhookID.MatchString(h.ID) {
			h.ID = config.RandomToken(6)
		}
		if seen[h.ID] {
			return nil, invalidf("two webhooks share the ID %q", h.ID)
		}
		seen[h.ID] = true

		switch h.Audience {
		case config.AudienceStaff, config.AudiencePlayers:
		case "":
			h.Audience = config.AudienceStaff
		default:
			return nil, invalidf("unknown audience %q", h.Audience)
		}
		h.Name = strings.TrimSpace(h.Name)
		if h.Name == "" {
			h.Name = map[string]string{
				config.AudienceStaff: "Staff alerts", config.AudiencePlayers: "Player announcements",
			}[h.Audience]
		}
		if len([]rune(h.Name)) > 80 {
			return nil, invalidf("the webhook name %q is too long", truncate(h.Name, 40))
		}

		h.URL = strings.TrimSpace(h.URL)
		if h.URL != "" {
			u, err := url.Parse(h.URL)
			if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
				return nil, invalidf("%s: the webhook address must start with https://", h.Name)
			}
		}
		if h.Enabled && h.URL == "" {
			return nil, invalidf("%s: add a webhook address or switch it off", h.Name)
		}

		allowed := config.NotifyEvents
		if h.Audience == config.AudiencePlayers {
			allowed = config.PlayerEvents
		}
		events := []string{}
		for _, e := range h.Events {
			if !containsString(allowed, e) {
				return nil, invalidf("%s: %q is not an event this webhook can be sent", h.Name, e)
			}
			if !containsString(events, e) {
				events = append(events, e)
			}
		}
		h.Events = events

		// A server deleted since the form loaded is simply dropped.
		scoped := []string{}
		for _, id := range h.Servers {
			if known[id] && !containsString(scoped, id) {
				scoped = append(scoped, id)
			}
		}
		h.Servers = scoped

		messages := map[string]string{}
		if h.Audience == config.AudiencePlayers {
			for kind, text := range h.Messages {
				text = strings.TrimSpace(text)
				if text == "" {
					continue
				}
				if !containsString(config.PlayerEvents, kind) {
					return nil, invalidf("%s: %q has no player wording", h.Name, kind)
				}
				if len([]rune(text)) > maxTemplate {
					return nil, invalidf("%s: the wording for %s is too long", h.Name, kind)
				}
				messages[kind] = text
			}
		}
		h.Messages = nil
		if len(messages) > 0 {
			h.Messages = messages
		}

		if h.LiveStatus && !notify.IsDiscord(h.URL) {
			return nil, invalidf("%s: live status edits its message in place, which only a Discord webhook "+
				"(https://discord.com/api/webhooks/…) can do", h.Name)
		}
		if !h.LiveStatus {
			h.ListPlayers = false
		}
		out = append(out, h)
	}
	return out, nil
}

func containsString(list []string, v string) bool {
	for _, s := range list {
		if s == v {
			return true
		}
	}
	return false
}

// cleanPublicInfo checks what players will be shown about a server.
func cleanPublicInfo(p config.PublicInfo) (config.PublicInfo, error) {
	p.Description = strings.TrimSpace(p.Description)
	if len([]rune(p.Description)) > 1000 {
		return p, invalidf("keep the public description under 1000 characters")
	}
	p.Address = strings.TrimSpace(p.Address)
	// People paste "host:port" or a URL; take it apart rather than refuse it.
	p.Address = strings.TrimPrefix(strings.TrimPrefix(p.Address, "http://"), "https://")
	p.Address = strings.TrimSuffix(p.Address, "/")
	if host, port, ok := strings.Cut(p.Address, ":"); ok && p.Port == 0 && !strings.Contains(port, ":") {
		n, err := strconv.Atoi(port)
		if err != nil {
			return p, invalidf("the join address %q has a port that is not a number", p.Address)
		}
		p.Address, p.Port = host, n
	}
	if len(p.Address) > 253 || strings.ContainsAny(p.Address, " \t/<>@`*_~|") {
		return p, invalidf("the join address should be a host name or IP, like play.example.com or 203.0.113.7")
	}
	if p.Port < 0 || p.Port > 65535 {
		return p, invalidf("the join port must be between 1 and 65535")
	}
	return p, nil
}

// webhookByID finds a configured webhook.
func webhookByID(cfg config.Config, id string) *config.Webhook {
	for i := range cfg.Notify.Webhooks {
		if cfg.Notify.Webhooks[i].ID == id {
			return &cfg.Notify.Webhooks[i]
		}
	}
	return nil
}

// webhooksInUse refuses to drop a webhook a scheduled job still posts to,
// which would otherwise only come to light when the job failed.
func webhooksInUse(hooks []config.Webhook, tasks []config.Task) error {
	kept := map[string]bool{}
	for _, h := range hooks {
		kept[h.ID] = true
	}
	for _, t := range tasks {
		for _, step := range t.Steps {
			if step.Kind == "discord" && !kept[step.Webhook] {
				return invalidf("the scheduled job %q posts to a channel you removed. Change that step first", t.Name)
			}
		}
	}
	return nil
}
