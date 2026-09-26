package server

import (
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/TheWarBoys2/pzadmin/internal/config"
	"github.com/TheWarBoys2/pzadmin/internal/discord"
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
//
// hasBot says whether a bot is connected, which channels picked by ID and
// status dots in channel names need.
func validateWebhooks(next, prev []config.Webhook, servers []config.Server, hasBot bool) ([]config.Webhook, error) {
	if len(next) > 20 {
		return nil, invalidf("at most 20 webhooks can be configured")
	}
	known := map[string]bool{}
	names := map[string]string{}
	for _, s := range servers {
		known[s.ID] = true
		names[s.ID] = s.Name
	}
	owned := map[string]bool{}
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

		if h.Server != "" {
			// A server's own channel goes with the server.
			if !known[h.Server] {
				continue
			}
			if owned[h.Server] {
				return nil, invalidf("%s already has its own channel", names[h.Server])
			}
			owned[h.Server] = true
			if strings.TrimSpace(h.Name) == "" {
				h.Name = names[h.Server]
			}
			h.Servers = []string{h.Server}
			if h.Audience == "" {
				h.Audience = config.AudiencePlayers
			}
		}
		identity, err := cleanIdentity(h.Identity, h.Name)
		if err != nil {
			return nil, err
		}
		h.Identity = identity

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
		h.ChannelID = strings.TrimSpace(h.ChannelID)
		if h.ChannelID != "" {
			// Through the bot: the channel is the address.
			if !discord.IsID(h.ChannelID) {
				return nil, invalidf("%s: %q is not a Discord channel ID", h.Name, h.ChannelID)
			}
			if !hasBot {
				return nil, invalidf("%s: connect the bot before choosing a channel for it", h.Name)
			}
			h.URL = ""
		}
		if h.URL != "" {
			u, err := url.Parse(h.URL)
			if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
				return nil, invalidf("%s: the webhook address must start with https://", h.Name)
			}
		}
		if h.Enabled && h.URL == "" && h.ChannelID == "" {
			return nil, invalidf("%s: add a webhook address, choose a channel for the bot, or switch it off", h.Name)
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

		// A server deleted since the form loaded is simply dropped; a
		// server's own channel always covers exactly that server.
		scoped := []string{}
		for _, id := range h.Servers {
			if known[id] && !containsString(scoped, id) {
				scoped = append(scoped, id)
			}
		}
		h.Servers = scoped
		if h.Server != "" {
			h.Servers = []string{h.Server}
		}

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

		if h.LiveStatus && h.ChannelID == "" && !notify.IsDiscord(h.URL) {
			return nil, invalidf("%s: a status message is edited in place, which only a Discord webhook "+
				"(https://discord.com/api/webhooks/…) or the bot can do", h.Name)
		}
		if h.RenameChannel {
			switch {
			case h.Server == "":
				return nil, invalidf("%s: only a server's own channel can show its status in the name", h.Name)
			case !hasBot:
				return nil, invalidf("%s: renaming a channel needs the bot; a webhook cannot do it", h.Name)
			}
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

// postedAs is the name and picture a webhook's messages appear under: its
// own, else the server's name for a server's channel, else the global ones.
func postedAs(cfg config.Config, h config.Webhook) (name, avatar string) {
	name = h.Identity.Name
	if name == "" && h.Server != "" {
		if s, ok := serverByID(cfg.Servers, h.Server); ok {
			name = s.Name
		}
	}
	if name == "" {
		name = cfg.Notify.Identity.Name
	}
	if name == "" {
		name = "PZAdmin"
	}
	avatar = h.Identity.AvatarURL
	if avatar == "" {
		avatar = cfg.Notify.Identity.AvatarURL
	}
	return truncateRunes(name, 80), avatar
}

// cleanIdentity checks a name and picture against what Discord accepts, so a
// mistake shows up in the form rather than as messages that never arrive.
func cleanIdentity(id config.Identity, label string) (config.Identity, error) {
	id.Name = strings.TrimSpace(id.Name)
	id.AvatarURL = strings.TrimSpace(id.AvatarURL)
	if len([]rune(id.Name)) > 80 {
		return id, invalidf("%s: the name shown in Discord must be 80 characters or fewer", label)
	}
	lower := strings.ToLower(id.Name)
	// Discord refuses these in a webhook's name and drops the message.
	for _, banned := range []string{"discord", "clyde"} {
		if strings.Contains(lower, banned) {
			return id, invalidf("%s: Discord does not allow %q in a name", label, banned)
		}
	}
	if id.AvatarURL != "" {
		u, err := url.Parse(id.AvatarURL)
		if err != nil || u.Scheme != "https" || u.Host == "" || len(id.AvatarURL) > 2048 {
			return id, invalidf("%s: the picture must be an https:// link to an image Discord can reach", label)
		}
	}
	return id, nil
}

// targetFor is where a channel's messages go: through the bot when it has a
// channel ID, through its webhook otherwise.
func (a *App) targetFor(cfg config.Config, h config.Webhook) notify.Target {
	if h.ChannelID != "" {
		return notify.Target{ChannelID: h.ChannelID, BotToken: cfg.Notify.Bot.Token, API: a.discordAPI}
	}
	return notify.Target{Webhook: h.URL}
}

// targetFromKey rebuilds a target from what a status message's record kept.
func (a *App) targetFromKey(cfg config.Config, key string) notify.Target {
	if id, ok := strings.CutPrefix(key, "channel:"); ok {
		return notify.Target{ChannelID: id, BotToken: cfg.Notify.Bot.Token, API: a.discordAPI}
	}
	return notify.Target{Webhook: key}
}

// cleanQuietHours checks the quiet hours window. Times are kept even while it
// is switched off, so turning it back on restores the same window.
func cleanQuietHours(q config.QuietHours) (config.QuietHours, error) {
	q.Start, q.End = strings.TrimSpace(q.Start), strings.TrimSpace(q.End)
	if !q.Enabled {
		return q, nil
	}
	start, ok := config.ParseClock(q.Start)
	if !ok {
		return q, invalidf("quiet hours need a start time, like 23:00")
	}
	end, ok := config.ParseClock(q.End)
	if !ok {
		return q, invalidf("quiet hours need an end time, like 08:00")
	}
	if start == end {
		return q, invalidf("quiet hours must start and end at different times")
	}
	if !q.Scheduled && !q.Manual {
		return q, invalidf("choose what quiet hours keep quiet: scheduled jobs, your own restarts, or both")
	}
	return q, nil
}
