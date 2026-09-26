package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"path/filepath"
	"strings"
	"time"

	"github.com/TheWarBoys2/pzadmin/internal/config"
	"github.com/TheWarBoys2/pzadmin/internal/fsutil"
	"github.com/TheWarBoys2/pzadmin/internal/notify"
)

// The status message is one Discord message per server in a channel, edited
// when something about the server changes, so players can glance at a pinned
// post instead of scrolling a stream of alerts.
//
// Edits are only sent when what the card says has changed. Times are written
// as Discord timestamps (<t:…:R>), which every reader's client renders as
// "3 hours ago" by itself, so an uptime that ticks on does not cost an edit a
// minute.

// liveStatusEvery is how often cards are compared with the servers' state.
// Comparing is free; a request is only made when a card would say something
// different.
const liveStatusEvery = 20 * time.Second

// liveCard is one message PZAdmin owns in a channel.
type liveCard struct {
	MessageID string `json:"messageId"`
	// URL is where the message lives: the webhook it was posted through, or
	// "channel:<id>" for the bot. When the operator points the channel
	// somewhere else, the old message is removed through this.
	URL string `json:"url"`
	// Hash is what the card last said, so an unchanged card is not re-sent.
	Hash string `json:"hash"`
}

// liveBoard is every card PZAdmin owns, by webhook ID and server ID. It is
// only touched by the live status loop.
type liveBoard struct {
	path  string
	Cards map[string]map[string]*liveCard `json:"cards"`
	// Names is the status-dot state of each channel PZAdmin renames, by
	// webhook ID.
	Names   map[string]*nameState `json:"names,omitempty"`
	backoff map[string]time.Time
	// loadErr is set when livestatus.json could not be read at start.
	loadErr error
}

func loadLiveBoard(path string) *liveBoard {
	b := &liveBoard{path: path, Cards: map[string]map[string]*liveCard{}, backoff: map[string]time.Time{}}
	if _, err := fsutil.ReadJSON(path, b); err != nil {
		log.Printf("live status: %v", err)
		b.loadErr = err
	}
	if b.Cards == nil {
		b.Cards = map[string]map[string]*liveCard{}
	}
	return b
}

// save writes the board with the same care as the config: it holds webhook
// addresses, which are secrets.
func (b *liveBoard) save() {
	data, err := json.MarshalIndent(b, "", "  ")
	if err != nil {
		return
	}
	if err := fsutil.WriteFile(b.path, data, 0o600); err != nil {
		log.Printf("live status: %v", err)
	}
}

func (a *App) liveStatusLoop() {
	defer a.wg.Done()
	board := loadLiveBoard(filepath.Join(a.dataDir, "livestatus.json"))
	a.reportLoadError(board.loadErr)
	ticker := time.NewTicker(liveStatusEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			ctx, cancel := context.WithTimeout(context.Background(), liveStatusEvery*3)
			a.syncLiveStatus(ctx, board)
			if a.syncChannelNames(ctx, board) {
				board.save()
			}
			cancel()
		case <-a.stop:
			// Leave nothing claiming a server is up while nobody is watching.
			// Kept short: Docker kills a container that takes 10s to stop.
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			a.pauseLiveStatus(ctx, board)
			cancel()
			return
		}
	}
}

// syncLiveStatus brings every card in line with the servers, posting the
// missing ones and removing the ones no longer wanted.
func (a *App) syncLiveStatus(ctx context.Context, board *liveBoard) {
	cfg := a.cfg.Get()
	wanted := map[string]map[string]bool{}
	for _, h := range cfg.Notify.Webhooks {
		if !h.Enabled || !h.LiveStatus || !a.targetFor(cfg, h).Valid() {
			continue
		}
		wanted[h.ID] = map[string]bool{}
		for _, s := range config.SortServers(cfg.Servers) {
			if s.Enabled && !s.Missing && webhookCovers(h, s.ID) {
				wanted[h.ID][s.ID] = true
			}
		}
	}

	changed := false
	for _, h := range cfg.Notify.Webhooks {
		if wanted[h.ID] == nil || time.Now().Before(board.backoff[h.ID]) {
			continue
		}
		for _, s := range config.SortServers(cfg.Servers) {
			if !wanted[h.ID][s.ID] {
				continue
			}
			st := a.statusOf(s.ID)
			// Not probed since PZAdmin started: say nothing rather than
			// guess, and leave whatever the card said before.
			if st.LastCheck.IsZero() && !st.Stopped {
				continue
			}
			card := liveCardFor(s, st, h.ListPlayers)
			card.Username, card.AvatarURL = postedAs(cfg, h)
			wrote, err := board.put(ctx, a.notify, a.targetFor(cfg, h), h.ID, s.ID, card)
			changed = changed || wrote
			if err != nil {
				board.fail(h, err)
				break
			}
		}
	}

	for hookID, cards := range board.Cards {
		for serverID, c := range cards {
			if wanted[hookID][serverID] {
				continue
			}
			// Switched off, out of scope or deleted: take the card down
			// rather than leave it frozen on whatever it last said.
			if c.MessageID != "" && c.URL != "" {
				if err := a.notify.DeleteCard(ctx, a.targetFromKey(cfg, c.URL), c.MessageID); err != nil {
					log.Printf("live status: could not remove an old status message: %v", err)
				}
			}
			delete(cards, serverID)
			changed = true
		}
		if len(cards) == 0 {
			delete(board.Cards, hookID)
		}
	}
	if changed {
		board.save()
	}
}

// put makes one card say what card says, reporting whether the board changed.
func (b *liveBoard) put(ctx context.Context, n *notify.Notifier, t notify.Target, hookID, serverID string, card notify.Card) (bool, error) {
	hash := cardHash(card)
	if b.Cards[hookID] == nil {
		b.Cards[hookID] = map[string]*liveCard{}
	}
	cur := b.Cards[hookID][serverID]
	if cur != nil && cur.URL == t.Key() && cur.Hash == hash {
		return false, nil
	}
	card.At = time.Now()

	// The channel now points somewhere else: the old message goes, and a new
	// one is posted where the operator wants it.
	if cur != nil && cur.URL != t.Key() {
		old := notify.Target{Webhook: cur.URL}
		if strings.HasPrefix(cur.URL, "channel:") {
			old = notify.Target{ChannelID: strings.TrimPrefix(cur.URL, "channel:"), BotToken: t.BotToken, API: t.API}
		}
		_ = n.DeleteCard(ctx, old, cur.MessageID)
		delete(b.Cards[hookID], serverID)
		cur = nil
	}
	if cur != nil {
		err := n.EditCard(ctx, t, cur.MessageID, card)
		if err == nil {
			cur.Hash = hash
			return true, nil
		}
		if !errors.Is(err, notify.ErrGone) {
			return false, err
		}
		// Someone deleted the message. Post a fresh one.
		delete(b.Cards[hookID], serverID)
	}
	id, err := n.PostCard(ctx, t, card)
	if err != nil {
		return true, err
	}
	b.Cards[hookID][serverID] = &liveCard{MessageID: id, URL: t.Key(), Hash: hash}
	return true, nil
}

// fail backs off from a webhook that refused an update, so a deleted webhook
// or a rate limit does not mean a request every tick.
func (b *liveBoard) fail(h config.Webhook, err error) {
	wait := time.Minute
	var limited *notify.RateLimited
	switch {
	case errors.As(err, &limited):
		wait = limited.RetryAfter
	case errors.Is(err, notify.ErrGone):
		wait = 10 * time.Minute
	}
	b.backoff[h.ID] = time.Now().Add(wait)
	log.Printf("live status: %s: %v; trying again in %s", h.Name, err, wait.Round(time.Second))
}

// pauseLiveStatus marks every card as not being kept up to date.
func (a *App) pauseLiveStatus(ctx context.Context, board *liveBoard) {
	cfg := a.cfg.Get()
	names := map[string]string{}
	for _, s := range cfg.Servers {
		names[s.ID] = s.Name
	}
	for _, cards := range board.Cards {
		for serverID, c := range cards {
			if ctx.Err() != nil {
				break
			}
			card := notify.Card{
				Title:       names[serverID],
				Description: "⚪ **Status unavailable**\nPZAdmin is not running, so this message is not being updated.",
				Colour:      notify.Colour("info"),
				Footer:      "Updated by PZAdmin",
				At:          time.Now(),
			}
			if err := a.notify.EditCard(ctx, a.targetFromKey(cfg, c.URL), c.MessageID, card); err == nil {
				// Whatever the server is doing next time, it is news.
				c.Hash = ""
			}
		}
	}
	board.save()
}

func webhookCovers(h config.Webhook, serverID string) bool {
	return len(h.Servers) == 0 || containsString(h.Servers, serverID)
}

// liveCardFor renders what the card for one server should say.
func liveCardFor(s config.Server, st Status, listPlayers bool) notify.Card {
	var lines []string
	colour := notify.Colour("error")
	switch {
	case st.Stopped:
		lines = append(lines, "⚫ **Stopped**")
		colour = notify.Colour("info")
	case st.Restarting:
		lines = append(lines, "🟡 **Restarting**")
		if !st.RestartingSince.IsZero() {
			lines = append(lines, fmt.Sprintf("Went down %s", discordTime(st.RestartingSince.Time)))
		}
		colour = notify.Colour("warn")
	case st.Online:
		lines = append(lines, fmt.Sprintf("🟢 **Online** · %d player%s", st.PlayerCount, plural(st.PlayerCount)))
		if !st.StartedAt.IsZero() {
			lines = append(lines, "Up since "+discordTime(st.StartedAt.Time))
		}
		colour = notify.Colour("success")
	default:
		lines = append(lines, "🔴 **Offline**")
		if !st.LastOnline.IsZero() {
			lines = append(lines, "Last seen "+discordTime(st.LastOnline.Time))
		}
	}
	if !st.PendingRestartAt.IsZero() && !st.Stopped {
		lines = append(lines, "Restart due "+discordTime(st.PendingRestartAt.Time))
	}
	if join := joinAddress(s); join != "" {
		lines = append(lines, "**Join:** `"+join+"`")
	}
	if listPlayers && st.Online && len(st.Players) > 0 {
		names := make([]string, 0, len(st.Players))
		for _, p := range st.Players {
			names = append(names, escapeMarkdown(p))
		}
		lines = append(lines, "", "**Playing:** "+truncateRunes(strings.Join(names, ", "), 1500))
	}
	// The operator's description leads, as it would in a server browser.
	if d := strings.TrimSpace(s.Public.Description); d != "" {
		lines = append([]string{d, ""}, lines...)
	}
	return notify.Card{
		Title:       s.Name,
		Description: strings.Join(lines, "\n"),
		Colour:      colour,
		Footer:      "Updated by PZAdmin",
	}
}

// joinAddress is where players connect, when the operator has said.
func joinAddress(s config.Server) string {
	if s.Public.Address == "" {
		return ""
	}
	port := s.Public.Port
	if port == 0 {
		port = s.GamePort
	}
	if port == 0 {
		return s.Public.Address
	}
	return fmt.Sprintf("%s:%d", s.Public.Address, port)
}

// discordTime is a timestamp each reader's Discord renders relative to now.
func discordTime(t time.Time) string {
	return fmt.Sprintf("<t:%d:R>", t.Unix())
}

// cardHash identifies what a card says, ignoring when it was said and who
// it is posted as, which an edit cannot change anyway.
func cardHash(c notify.Card) string {
	c.At = time.Time{}
	c.Username, c.AvatarURL = "", ""
	b, _ := json.Marshal(c)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:8])
}

var markdownEscaper = strings.NewReplacer(
	`\`, `\\`, `*`, `\*`, `_`, `\_`, "`", "\\`", `~`, `\~`, `|`, `\|`, `>`, `\>`, `#`, `\#`, `<`, `\<`, `@`, "@\u200b",
)

// escapeMarkdown stops a player name from formatting or mentioning anything.
func escapeMarkdown(s string) string { return markdownEscaper.Replace(s) }

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}
