# Changelog

## Unreleased

### Discord
- Any number of webhooks, each written for staff or for players, with its own
  events and servers. An existing webhook carries over as a staff webhook.
- Player announcements in plain words: when a server actually goes down for a
  restart (and why: scheduled, mod updates, not responding) and when it is
  back. Not every countdown warning. The wording can be changed per event,
  including a role ping.
- A status message per server, edited only when something changes (it goes
  online, restarts or goes down, or the player count moves), with the
  server's description and join address and optionally who is playing.
- A Discord page in the sidebar, organised by server: set up a server's own
  channel by pasting one webhook link, and see where each server is
  announced. Staff and shared channels sit alongside.
- Messages are posted under a name and picture set in PZAdmin, so a webhook
  needs no setting up in Discord. A server's own channel uses the server's
  name; any channel can have its own name and picture.
- An optional Discord bot, alongside webhooks: connect it once and pick
  each channel from a list instead of pasting a webhook. An existing bot
  works. PZAdmin keeps no connection open; it brings a new bot online once
  when it is connected, which Discord requires before a bot can post.
- With the bot, a server's channel can show 🟢 or 🔴 in its name. Renames
  follow the server's settled state and stay within Discord's limit of two
  per ten minutes; turning it off restores the plain name.
- Restart and Stop ask for an optional reason, which players see in the
  announcement.
- Each server has public info (description, join address and port) typed in
  by hand, since PZAdmin cannot see the address players use.
- Scheduled jobs can post to a chosen Discord channel. A channel a job uses
  cannot be removed until the job is changed.
- Stopping and starting a server are now their own events instead of being
  reported as restarts, and a mod-update restart is announced once, not twice.
- Alerts can no longer ping @everyone through a player's name.
- The settings form knows Project Zomboid's own Discord bot keys
  (`DiscordEnable`, `DiscordToken`, and the Build 42 chat, log and command
  channels). The bot token is treated as a secret, like the RCON password.

### API
- An HTTP API at `/api/v1` for bots and scripts: server status (with the
  public description and join address), players, the event log, a live event
  stream, and commands, restart, stop, start, backups and the RCON console.
- API keys under Settings: each has read, control or console access, can be
  limited to some servers and can expire. A key is shown once and only its
  hash is stored. Keys cannot reach settings, the password or other keys.
- Everything a key does is in the event log as `key:<name>`, and is announced
  in Discord the same as the same action from the web interface.
- Per-key rate limits, and `/metrics` counts API requests per key.

## 1.0.0

First public release.

### Servers
- Live dashboard: who's online, uptime, RCON latency and problems, updated as
  they happen.
- Start, stop, restart and deploy through Arcane, plus a watchdog that can
  recover a frozen server.
- A wizard that creates new servers with ports, paths and admin account set
  up.
- Finds existing servers on its own by reading the stack folders.

### Mods
- Add mods from a Workshop link, ID or whole collection.
- Automatic load order from each mod's declared dependencies.
- Catches missing dependencies, duplicate IDs and `Mods=` / `WorkshopItems=`
  drift before saving.
- Build 41 and Build 42 layouts.

### Settings
- Form editor for the server ini and `SandboxVars.lua` that says which changes
  apply on reload and which need a restart.
- Raw file editor for everything else. The previous version is always kept.

### Players and commands
- Player history: sessions, playtime and Steam IDs.
- Give items, vehicles and XP from searchable lists built from the server's
  own files, modded items included.
- A full RCON console.

### Automation
- Scheduled jobs made of steps, e.g. announce, wait, save, back up, restart.
- Mod update detection, with an optional countdown and automatic restart.
- Backups with retention, download and restore.
- Discord webhook alerts, Prometheus metrics and an audit log of every action.

### Install
- One `docker-compose.yml` with every setting in it, then
  `docker compose up -d`. Running it again is how you update.
- Images for `linux/amd64` and `linux/arm64` at `ghcr.io/thewarboys2/pzadmin`.
