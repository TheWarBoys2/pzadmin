# Changelog

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
