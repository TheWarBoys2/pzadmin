# Changelog

## Unreleased

### Discord
- Any number of webhooks, each written for staff or for players, with its own
  events and servers. An existing webhook carries over as a staff webhook.
- Player announcements in plain words: when a server actually goes down for a
  restart (and why: scheduled, mod updates, not responding) and when it is
  back. Not every countdown warning. The wording can be changed per event,
  including a role ping.
- A live status message per server, edited in place as it goes online,
  restarts or goes down, optionally listing who is playing.
- Stopping and starting a server are now their own events instead of being
  reported as restarts, and a mod-update restart is announced once, not twice.
- Alerts can no longer ping @everyone through a player's name.
- The settings form knows Project Zomboid's own Discord bot keys
  (`DiscordEnable`, `DiscordToken`, and the Build 42 chat, log and command
  channels). The bot token is treated as a secret, like the RCON password.

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
