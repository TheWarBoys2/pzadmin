# Changelog

## Unreleased

### Upgrading an existing install
Most of this update needs nothing from you. These do:

- **Backups folder.** Backups can now live in a folder on the host instead of
  inside PZAdmin's volume. To use it, copy the new `volumes` and
  `PZADMIN_BACKUP_DIR` lines from the README's `docker-compose.yml`, run
  `mkdir backups` next to your compose file, then `docker compose up -d`.
  PZAdmin moves your existing backups into the new folder the first time it
  starts. Without these lines, backups stay where they were.
- **Updating is now your choice.** The compose file no longer has
  `pull_policy: always`, so `docker compose up -d` doesn't update PZAdmin on
  its own. Update with `docker compose pull` then `docker compose up -d`.
- **Metrics token.** `/metrics` now only accepts the token in an
  `Authorization: Bearer` header, not in the address. If Prometheus scrapes
  PZAdmin, move the token into the scrape job's `authorization` setting.
- **Behind a reverse proxy?** PZAdmin no longer believes `X-Forwarded-For`
  from anyone, because anyone can send it. Set `PZADMIN_TRUSTED_PROXIES` to
  your proxy's address so sign-in limits and the audit log see real visitor
  addresses (see the README).

### Setup
- A new install asks for a one-time setup code from `docker logs pzadmin`
  before the admin account can be created, so nobody else on the network can
  claim a fresh install first.
- Starting with empty server folders works: the dashboard says there are no
  servers yet and offers to create the first one.
- New servers use a tested, pinned game image by default
  (`indifferentbroccoli/projectzomboid-server-docker:v1.1.9`), so the wizard
  works without setting `PZADMIN_GAME_IMAGE`.
- The log says clearly what doesn't work when Arcane isn't set up.
- A fresh install works with any `user:` in the compose file, not only
  1000:1000.
- The compose file has log rotation and a stop grace period, so PZAdmin always
  gets to finish saving when it's stopped.

### Backups
- `PZADMIN_BACKUP_DIR` puts backups in a folder you choose, as plain files.
- PZAdmin checks at start that it can write to the backups folder, and says
  how to fix it if not, instead of failing at the first scheduled backup.
- Restore no longer leaves files from after the backup mixed into the
  restored world. The archive is read through first, and a damaged one
  changes nothing. The current world, and the settings if the backup has
  them, are moved to `.before-restore` folders, and put back if the restore
  fails. An archive with no world in it is refused.
- Half-written archives from an interrupted backup are cleaned up.

### Security
- Sign-in is limited per address and across all addresses. Each attempt is
  counted before the password is checked, so a burst of guesses sent at once
  is limited the same as guesses sent one by one. Addresses the admin has
  signed in from before aren't held back by the account-wide limit, which a
  stranger could otherwise trip on purpose. At most four password checks run
  at once, so a flood of attempts can't overload the machine.
- Forwarded headers are only trusted from `PZADMIN_TRUSTED_PROXIES`.
- Backups are readable only by PZAdmin's user, since they can hold the
  server's RCON password.
- An imported scheduled job that runs game commands arrives switched off, so
  a config file can't quietly add one.
- Passwords and the game's Discord bot token are hidden in the raw ini
  editor, the same ones the settings form hides, and kept as they are on save.
- The metrics token can be replaced from Settings, and is only accepted in a
  header.
- Sign-in and setup requests have a small size limit, and slow requests time
  out.

### Reliability
- Settings, sessions, API keys, mod requests and player history are saved
  with a write-then-rename, so a crash or full disk can't leave a half-written
  file. A file that can't be read is kept aside and reported, not silently
  replaced.
- Shutting down closes live connections, stops running jobs and backups at
  their next step, and waits for them at most 8 seconds, so the final save
  finishes within Docker's stop timeout. A backup cut short is cleaned up
  next time.
- An import is checked the same way as settings typed in by hand. Scheduled
  jobs that would be refused are left out and listed, an unknown timezone
  refuses the file, and the import dialog says exactly what was changed.
- Long names and messages in other alphabets are shortened without breaking
  characters.

### Interface
- A wrong password or setup code is shown on the form, instead of the page
  reloading.
- An open tab says when PZAdmin has been updated, and new versions of the
  page are never served from a stale cache.
- Works on phones: no sideways scrolling, and tables scroll on their own.
- Form labels are linked to their fields and buttons have names, for screen
  readers.
- When Arcane can't be reached, the message is plain, with the technical
  detail behind a toggle.
- The Control access for API keys says plainly how much it allows.

### Project
- MIT licence, a security policy with private reporting, contributing notes
  and issue forms.
- The README was rewritten to match what PZAdmin actually does, including how
  it was built.
- Built with Go 1.27. CI also checks formatting, runs static analysis, a
  vulnerability scan and the race detector.

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
- With the bot, a server's channel can show 🟢 (up), 🟠 (restarting) or 🔴 (down) in its name. Renames
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
- Mod requests: a bot with a key that has the new **request** access can ask
  for a Workshop mod on a server, by the server's ID or name. Requests wait in
  a Requests section on the server's Mods tab; approving one adds it to the
  load order through the same checks as Save. Duplicates and mods already on
  the server are refused with a clear reason, and a staff webhook can be told
  about new requests.

## 1.0.0-beta.1

First beta.

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
