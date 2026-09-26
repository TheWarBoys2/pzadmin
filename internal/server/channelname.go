package server

import (
	"context"
	"errors"
	"log"
	"time"

	"github.com/TheWarBoys2/pzadmin/internal/config"
	"github.com/TheWarBoys2/pzadmin/internal/discord"
)

// Status dots in channel names.
//
// Discord allows a channel to be renamed twice every ten minutes, and a
// restart alone is two changes. So the name follows the server's settled
// state rather than every blip: a restart PZAdmin knows about leaves the dot
// alone, a new state has to hold for a while before it is shown, and when
// the budget is spent the rename waits and then shows whatever is true by
// then. The status message and announcements carry the detail; the dot is
// only there to be glanced at in the channel list.

// dotSettle is how long a new state has to hold before the name follows.
const dotSettle = 90 * time.Second

// renameWindow and renamesPerWindow are Discord's limit on channel renames.
const (
	renameWindow     = 10 * time.Minute
	renamesPerWindow = 2
)

// clock is overridden by tests.
var clock = time.Now

// nameState is what PZAdmin knows about one channel it names.
type nameState struct {
	// Source is the webhook or channel the ID was worked out from, so a
	// changed webhook is looked up again.
	Source    string      `json:"source"`
	ChannelID string      `json:"channelId"`
	Want      string      `json:"want"`
	WantSince time.Time   `json:"wantSince"`
	Shown     string      `json:"shown"`
	Renames   []time.Time `json:"renames"`
	RetryAt   time.Time   `json:"retryAt"`
}

// wantedDot is the dot a server's state calls for, or "" to leave the name
// as it is.
func wantedDot(st Status) string {
	switch {
	case st.Restarting || st.Deploying:
		return discord.DotRestarting // a restart or deploy we asked for
	case st.LastCheck.IsZero() && !st.Stopped:
		return "" // not checked since PZAdmin started
	case st.Online:
		return discord.DotOnline
	}
	return discord.DotOffline
}

// syncChannelNames puts each server's state into its channel's name, as far
// as Discord's rate limit allows. It reports whether the board changed.
func (a *App) syncChannelNames(ctx context.Context, board *liveBoard) bool {
	cfg := a.cfg.Get()
	if board.Names == nil {
		board.Names = map[string]*nameState{}
	}
	changed := false
	token := cfg.Notify.Bot.Token
	now := clock()

	wanted := map[string]bool{}
	for _, h := range cfg.Notify.Webhooks {
		if !h.RenameChannel || !h.Enabled || h.Server == "" || token == "" {
			continue
		}
		srv, found := serverByID(cfg.Servers, h.Server)
		if !found || !srv.Enabled || srv.Missing {
			continue
		}
		wanted[h.ID] = true
		ns := board.Names[h.ID]
		source := a.targetFor(cfg, h).Key()
		if ns == nil || ns.Source != source {
			ns = &nameState{Source: source}
			board.Names[h.ID] = ns
			changed = true
		}
		if now.Before(ns.RetryAt) {
			continue
		}

		dot := wantedDot(a.statusOf(srv.ID))
		if dot == "" {
			continue
		}
		if dot != ns.Want {
			ns.Want, ns.WantSince = dot, now
			changed = true
		}
		if ns.Want == ns.Shown {
			continue
		}
		// The first dot goes up straight away, and so do a restart and the
		// recovery from one, which PZAdmin did on purpose. Anything else has
		// to last before it is worth one of the two renames.
		deliberate := ns.Want == discord.DotRestarting || ns.Shown == discord.DotRestarting
		if ns.Shown != "" && !deliberate && now.Sub(ns.WantSince) < dotSettle {
			continue
		}
		if !ns.budget(now) {
			continue
		}
		if err := a.renameFor(ctx, h, ns, func(name string) string { return discord.WithDot(ns.Want, name) }); err != nil {
			ns.backOff(now, err, h.Name)
		} else {
			ns.Shown = ns.Want
		}
		changed = true
	}

	// Channels no longer named by PZAdmin get their plain name back.
	for id, ns := range board.Names {
		if wanted[id] {
			continue
		}
		if ns.Shown != "" && token != "" && ns.budget(now) {
			h := webhookByID(cfg, id)
			if h != nil && ns.ChannelID != "" {
				if err := a.renameFor(ctx, *h, ns, discord.BaseName); err != nil {
					log.Printf("channel name: could not restore %s: %v", h.Name, err)
				}
			}
		}
		delete(board.Names, id)
		changed = true
	}
	return changed
}

// renameFor renames a channel to rename(its current name). The current name
// is read first, so a name an admin changed by hand is kept.
func (a *App) renameFor(ctx context.Context, h config.Webhook, ns *nameState, rename func(string) string) error {
	cfg := a.cfg.Get()
	client := a.discordClient(cfg.Notify.Bot.Token)
	if ns.ChannelID == "" {
		switch {
		case h.ChannelID != "":
			ns.ChannelID = h.ChannelID
		case h.URL != "":
			id, err := discord.WebhookChannel(ctx, client.HTTP, h.URL)
			if err != nil {
				return err
			}
			ns.ChannelID = id
		default:
			return errors.New("the channel has neither a webhook nor a channel for the bot")
		}
	}
	ch, err := client.GetChannel(ctx, ns.ChannelID)
	if err != nil {
		return err
	}
	name := rename(ch.Name)
	if name == ch.Name {
		return nil
	}
	if err := client.RenameChannel(ctx, ns.ChannelID, name); err != nil {
		return err
	}
	ns.Renames = append(ns.Renames, clock())
	return nil
}

// budget reports whether Discord would allow another rename now.
func (ns *nameState) budget(now time.Time) bool {
	kept := ns.Renames[:0]
	for _, t := range ns.Renames {
		if now.Sub(t) < renameWindow {
			kept = append(kept, t)
		}
	}
	ns.Renames = kept
	return len(ns.Renames) < renamesPerWindow
}

func (ns *nameState) backOff(now time.Time, err error, name string) {
	wait := 10 * time.Minute
	var limited *discord.RateLimited
	if errors.As(err, &limited) {
		wait = limited.RetryAfter
	}
	ns.RetryAt = now.Add(wait)
	msg := err.Error()
	if errors.Is(err, discord.ErrForbidden) {
		msg = "the bot needs the Manage Channels permission on that channel"
	}
	log.Printf("channel name: %s: %s; trying again in %s", name, msg, wait.Round(time.Second))
}
