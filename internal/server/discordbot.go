package server

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/TheWarBoys2/pzadmin/internal/config"
	"github.com/TheWarBoys2/pzadmin/internal/discord"
	"github.com/TheWarBoys2/pzadmin/internal/store"
)

// discordClient is a bot client pointed at Discord, or at a test's stand-in.
func (a *App) discordClient(token string) *discord.Client {
	c := discord.New(token)
	if a.discordAPI != "" {
		c.API = a.discordAPI
	}
	if a.discordGateway != "" {
		c.Gateway = a.discordGateway
	}
	return c
}

// handleBotConnect checks a bot token with Discord and saves it. Blank or
// the placeholder re-checks the saved one.
func (a *App) handleBotConnect(w http.ResponseWriter, r *http.Request) {
	var p struct {
		Token string `json:"token"`
	}
	if !decodeJSON(w, r, &p) {
		return
	}
	token := strings.TrimSpace(p.Token)
	// People paste the "Bot " prefix from examples.
	token = strings.TrimSpace(strings.TrimPrefix(token, "Bot "))
	if token == "" || token == config.Redacted {
		token = a.cfg.Get().Notify.Bot.Token
	}
	if token == "" {
		httpError(w, http.StatusBadRequest, "paste the bot's token first")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 40*time.Second)
	defer cancel()
	client := a.discordClient(token)
	me, err := client.Me(ctx)
	if errors.Is(err, discord.ErrUnauthorized) {
		httpError(w, http.StatusBadRequest, "Discord did not accept that token. Copy it again from the Bot page "+
			"of your application in the Discord developer portal (Reset Token).")
		return
	}
	if err != nil {
		httpError(w, http.StatusBadGateway, "could not reach Discord: "+err.Error())
		return
	}
	guilds, err := client.Guilds(ctx)
	if err != nil {
		httpError(w, http.StatusBadGateway, "could not list the bot's servers: "+err.Error())
		return
	}

	// Discord only lets a bot post once it has been online at least once.
	// A bot someone already runs has been; doing it again is harmless.
	warning := ""
	if err := client.Identify(ctx); err != nil {
		if errors.Is(err, discord.ErrUnauthorized) {
			httpError(w, http.StatusBadRequest, "Discord refused to let the bot come online with that token")
			return
		}
		warning = "The bot is saved, but it could not come online once (" + err.Error() + "). " +
			"If it has never been online, Discord will refuse its messages until it has."
	}

	updated, err := a.cfg.Update(func(c *config.Config) error {
		c.Notify.Bot = config.Bot{Token: token, ID: me.ID, Name: me.Username}
		return nil
	})
	if err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	a.applyNotifyConfig(updated)
	a.event(store.Event{Kind: "admin.action", Severity: store.SevInfo, Source: "ui", Actor: actor(r),
		Message: "Discord bot connected", Detail: "As " + me.Username + ", in " + guildNames(guilds)})

	names := make([]string, 0, len(guilds))
	for _, g := range guilds {
		names = append(names, g.Name)
	}
	ok(w, map[string]any{"name": me.Username, "id": me.ID, "guilds": names, "warning": warning})
}

func guildNames(gs []discord.Guild) string {
	if len(gs) == 0 {
		return "no servers yet"
	}
	names := make([]string, 0, len(gs))
	for _, g := range gs {
		names = append(names, g.Name)
	}
	return strings.Join(names, ", ")
}

// handleBotRemove forgets the bot, unless a channel still depends on it.
func (a *App) handleBotRemove(w http.ResponseWriter, r *http.Request) {
	var p struct{}
	if !decodeJSON(w, r, &p) {
		return
	}
	var using []string
	for _, h := range a.cfg.Get().Notify.Webhooks {
		if h.ChannelID != "" || h.RenameChannel {
			using = append(using, h.Name)
		}
	}
	if len(using) > 0 {
		httpError(w, http.StatusBadRequest, "these channels still use the bot: "+strings.Join(using, ", ")+
			". Switch them to a webhook, or turn off the status dot, first.")
		return
	}
	updated, err := a.cfg.Update(func(c *config.Config) error {
		c.Notify.Bot = config.Bot{}
		return nil
	})
	if err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	a.applyNotifyConfig(updated)
	a.event(store.Event{Kind: "admin.action", Severity: store.SevWarn, Source: "ui", Actor: actor(r),
		Message: "Discord bot removed"})
	ok(w, map[string]any{"message": "Bot removed."})
}

// handleBotChannels lists the channels the bot can post in, for the picker.
func (a *App) handleBotChannels(w http.ResponseWriter, r *http.Request) {
	token := a.cfg.Get().Notify.Bot.Token
	if token == "" {
		httpError(w, http.StatusBadRequest, "connect the bot first")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	chans, err := a.discordClient(token).TextChannels(ctx)
	if err != nil {
		httpError(w, http.StatusBadGateway, "could not list the bot's channels: "+err.Error())
		return
	}
	writeJSON(w, map[string]any{"channels": chans})
}
