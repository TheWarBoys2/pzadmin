// Package discord talks to Discord's HTTP API as a bot: who the bot is,
// which channels it can see, and renaming a channel. Posting messages is the
// notify package's job, for webhooks and bots alike.
//
// Nothing here keeps a connection open. PZAdmin is not a chat bot; it only
// makes the odd request when something changes. The one exception is
// Identify, which Discord requires a bot to have done once before it may
// post, and which is over the moment it succeeds.
package discord

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// DefaultAPI is Discord's API root.
const DefaultAPI = "https://discord.com/api/v10"

// DefaultGateway is where a bot identifies.
const DefaultGateway = "wss://gateway.discord.gg/?v=10&encoding=json"

// Client makes requests as one bot.
type Client struct {
	Token string
	// API and Gateway are overridden by tests.
	API     string
	Gateway string
	HTTP    *http.Client
}

// New returns a client for a bot token.
func New(token string) *Client {
	return &Client{Token: strings.TrimSpace(token), API: DefaultAPI, Gateway: DefaultGateway,
		HTTP: &http.Client{Timeout: 15 * time.Second}}
}

// ErrUnauthorized means Discord does not accept the token.
var ErrUnauthorized = errors.New("Discord did not accept the bot token")

// ErrForbidden means the bot lacks a permission for what was asked.
var ErrForbidden = errors.New("the bot does not have permission for that")

// ErrNotFound means the channel or message does not exist, or the bot cannot
// see it.
var ErrNotFound = errors.New("the bot cannot find that channel")

// RateLimited reports that Discord asked for a pause.
type RateLimited struct{ RetryAfter time.Duration }

func (r *RateLimited) Error() string {
	return "Discord asked to wait " + r.RetryAfter.Round(time.Second).String()
}

func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimSuffix(c.API, "/")+path, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bot "+c.Token)
	req.Header.Set("User-Agent", "DiscordBot (https://github.com/TheWarBoys2/pzadmin, 1)")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if err := StatusError(resp); err != nil {
		return err
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(out)
}

// StatusError turns a Discord error response into one of this package's
// errors, or nil for success.
func StatusError(resp *http.Response) error {
	if resp.StatusCode < 300 {
		return nil
	}
	switch resp.StatusCode {
	case http.StatusUnauthorized:
		return ErrUnauthorized
	case http.StatusForbidden:
		return ErrForbidden
	case http.StatusNotFound:
		return ErrNotFound
	case http.StatusTooManyRequests:
		wait := 5 * time.Second
		var body struct {
			RetryAfter float64 `json:"retry_after"`
		}
		if json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&body) == nil && body.RetryAfter > 0 {
			wait = time.Duration(body.RetryAfter * float64(time.Second))
		} else if s, err := strconv.ParseFloat(resp.Header.Get("Retry-After"), 64); err == nil && s > 0 {
			wait = time.Duration(s * float64(time.Second))
		}
		return &RateLimited{RetryAfter: wait}
	}
	var body struct {
		Message string `json:"message"`
	}
	_ = json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&body)
	if body.Message != "" {
		return fmt.Errorf("Discord said: %s", body.Message)
	}
	return fmt.Errorf("Discord returned %s", resp.Status)
}

// User is the bot's own account.
type User struct {
	ID       string `json:"id"`
	Username string `json:"username"`
}

// Me returns the bot's account, which is how a token is checked.
func (c *Client) Me(ctx context.Context) (User, error) {
	var u User
	err := c.do(ctx, http.MethodGet, "/users/@me", nil, &u)
	return u, err
}

// Guild is a Discord server the bot is in.
type Guild struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// Channel is a channel the bot can post in.
type Channel struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Type    int    `json:"type"`
	GuildID string `json:"guild_id"`
	// Guild and Category are filled in for display.
	Guild    string `json:"guild,omitempty"`
	Category string `json:"category,omitempty"`
	ParentID string `json:"parent_id,omitempty"`
	Position int    `json:"position"`
}

// Channel types a bot can post a message into.
const (
	channelText         = 0
	channelCategory     = 4
	channelAnnouncement = 5
)

// Guilds lists the Discord servers the bot has been invited to.
func (c *Client) Guilds(ctx context.Context) ([]Guild, error) {
	var gs []Guild
	err := c.do(ctx, http.MethodGet, "/users/@me/guilds", nil, &gs)
	return gs, err
}

// TextChannels lists every channel the bot could post in, across its
// servers, in the order Discord shows them.
func (c *Client) TextChannels(ctx context.Context) ([]Channel, error) {
	guilds, err := c.Guilds(ctx)
	if err != nil {
		return nil, err
	}
	var out []Channel
	for _, g := range guilds {
		var all []Channel
		if err := c.do(ctx, http.MethodGet, "/guilds/"+url.PathEscape(g.ID)+"/channels", nil, &all); err != nil {
			return nil, err
		}
		categories := map[string]Channel{}
		for _, ch := range all {
			if ch.Type == channelCategory {
				categories[ch.ID] = ch
			}
		}
		var chans []Channel
		for _, ch := range all {
			if ch.Type != channelText && ch.Type != channelAnnouncement {
				continue
			}
			ch.Guild = g.Name
			ch.GuildID = g.ID
			ch.Category = categories[ch.ParentID].Name
			chans = append(chans, ch)
		}
		sort.SliceStable(chans, func(i, j int) bool {
			ci, cj := categories[chans[i].ParentID].Position, categories[chans[j].ParentID].Position
			if (chans[i].ParentID == "") != (chans[j].ParentID == "") {
				return chans[i].ParentID == ""
			}
			if ci != cj {
				return ci < cj
			}
			return chans[i].Position < chans[j].Position
		})
		out = append(out, chans...)
	}
	return out, nil
}

// GetChannel reads one channel.
func (c *Client) GetChannel(ctx context.Context, id string) (Channel, error) {
	var ch Channel
	err := c.do(ctx, http.MethodGet, "/channels/"+url.PathEscape(id), nil, &ch)
	return ch, err
}

// RenameChannel changes a channel's name. Discord allows this twice per ten
// minutes per channel; the caller has to pace itself.
func (c *Client) RenameChannel(ctx context.Context, id, name string) error {
	return c.do(ctx, http.MethodPatch, "/channels/"+url.PathEscape(id),
		map[string]any{"name": name}, nil)
}

// WebhookChannel asks Discord which channel a webhook posts into. A webhook
// address is its own credential, so this needs no bot token.
func WebhookChannel(ctx context.Context, client *http.Client, webhook string) (string, error) {
	u, err := url.Parse(webhook)
	if err != nil {
		return "", err
	}
	u.RawQuery = ""
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return "", err
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if err := StatusError(resp); err != nil {
		return "", err
	}
	var hook struct {
		ChannelID string `json:"channel_id"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&hook); err != nil || hook.ChannelID == "" {
		return "", fmt.Errorf("the webhook did not say which channel it belongs to")
	}
	return hook.ChannelID, nil
}

var snowflake = regexp.MustCompile(`^[0-9]{15,21}$`)

// IsID reports whether s looks like a Discord ID.
func IsID(s string) bool { return snowflake.MatchString(s) }

// --- status dots in channel names -------------------------------------------

// Dots PZAdmin puts at the front of a channel name.
const (
	DotOnline     = "🟢"
	DotOffline    = "🔴"
	DotRestarting = "🟠"
)

var dotPrefix = regexp.MustCompile(`^(?:🟢|🔴|🟡|⚫|⚪|🟠)[\s\-_|┃・•]*`)

// BaseName is a channel's name without a status dot PZAdmin (or anyone)
// put in front of it.
func BaseName(name string) string {
	return dotPrefix.ReplaceAllString(name, "")
}

// WithDot is the channel name showing a status. Discord turns the space into
// a hyphen in text channels.
func WithDot(dot, name string) string {
	return dot + "-" + BaseName(name)
}
