package server

import (
	"fmt"
	"net/url"
	"regexp"
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
