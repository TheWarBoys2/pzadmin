# PZAdmin

[![Build](https://github.com/TheWarBoys2/pzadmin/actions/workflows/docker.yml/badge.svg)](https://github.com/TheWarBoys2/pzadmin/actions/workflows/docker.yml)
[![Licence: MIT](https://img.shields.io/badge/licence-MIT-blue.svg)](LICENSE)

A web control panel for Project Zomboid dedicated servers running in Docker.

Watch every server from one dashboard. Restart on a schedule, manage mods and
settings, back up worlds and run admin commands without SSH or a raw RCON
client.

Built for servers on the
[`indifferentbroccoli/projectzomboid-server-docker`](https://github.com/indifferentbroccoli/projectzomboid-server-docker)
image, one Compose stack per server. Starting and stopping containers goes
through [Arcane](https://getarcane.app), so PZAdmin never needs the Docker
socket.

**Status: beta.** I run it on my own servers, but it has not
had many other users yet. Expect rough edges, and please
[open an issue](https://github.com/TheWarBoys2/pzadmin/issues) when you find one.

---

## How it was built

PZAdmin's code was written by an AI coding assistant (Claude). I decided what
it should do, tried each change on my own servers and asked for fixes when
something was wrong. Before this first public release, the whole codebase was
reviewed for security, correctness and ease of setup, again with AI help, and
the problems found were fixed. Every change runs through an automated test
suite: Go and frontend tests, a race detector, static analysis and a
vulnerability scan.

AI-written code can still have mistakes a human would not make. If you find
something wrong, especially anything security related, please report it (see
[SECURITY.md](SECURITY.md)). Contributions and reviews are very welcome.

---

## Features

**Servers**
- Live dashboard: who's online, uptime, RCON latency and problems, updated as they happen
- Start, stop, restart and deploy, plus a watchdog that can recover a frozen server
- A wizard that creates new servers with ports, paths and admin account set up for you
- Finds your existing servers on its own by reading the stack folders

**Mods**
- Add mods from a Workshop link, ID or whole collection
- Automatic load order from each mod's declared dependencies
- Catches missing dependencies, duplicate IDs and `Mods=` / `WorkshopItems=` drift before you save
- Build 41 and Build 42 layouts

**Settings**
- Form editor for the server ini and `SandboxVars.lua` that says which changes apply on reload and which need a restart
- Raw file editor for everything else. The previous version is always kept.

**Players and commands**
- Player history: sessions, playtime and Steam IDs
- Give items, vehicles and XP from searchable lists built from your server's own files, modded items included
- A full RCON console

**Automation**
- Scheduled jobs made of steps, e.g. *announce → wait 1 min → save → back up → restart*
- Mod update detection, with an optional countdown and automatic restart
- Backups with retention, download and restore, kept as plain files in a folder you choose
- Discord: staff alerts, player announcements when a server restarts and comes back, a status message per server, and Discord posts from scheduled jobs
- Prometheus metrics and an audit log of every action
- An HTTP API with its own keys, for bots and scripts ([docs/api.md](docs/api.md))

---

## Requirements

- A Linux host with Docker and Docker Compose. PZAdmin itself is light: about
  15 MB of memory with no servers, more with several servers and their
  history. Images are published for amd64 and arm64.
- Game servers on `indifferentbroccoli/projectzomboid-server-docker`, one
  Compose stack per server ([layout below](#how-pzadmin-finds-your-servers)).
  PZAdmin reads that image's `.env` settings to find ports and passwords, so
  other images are not supported.
- [Arcane](https://getarcane.app) for starting, stopping and creating servers.
  PZAdmin runs without it, with fewer features: see
  [What works without Arcane](#what-works-without-arcane).

---

## Install

**1.** Make a folder for PZAdmin on the server and put this `docker-compose.yml` in it:

```yaml
# PZAdmin. Put this file in an empty folder on the server, change the lines
# marked CHANGE, then in that folder run:
#   mkdir backups && docker compose up -d
# Open http://<host>:27815 and enter the setup code from: docker logs pzadmin

services:
  pzadmin:
    image: ghcr.io/thewarboys2/pzadmin:latest   # update: docker compose pull && docker compose up -d
    container_name: pzadmin
    restart: unless-stopped
    user: "1000:1000"            # CHANGE if needed: must own your server folders (id -u, id -g)
    ports:
      - "27815:27815"
    extra_hosts:
      - "host.docker.internal:host-gateway"   # reaches each server's RCON port on the host
    volumes:
      - pzadmin-data:/data       # settings, accounts, history
      - ./backups:/backups       # world backups, as plain files next to this compose file
      # CHANGE: your two server folders. Same path on both sides of the colon.
      - /srv/zomboid:/srv/zomboid
      - /home/youruser/docker/pzserver:/home/youruser/docker/pzserver
    environment:
      PZADMIN_DATA_ROOT: /srv/zomboid                     # CHANGE: same as above
      PZADMIN_STACKS_ROOT: /home/youruser/docker/pzserver # CHANGE: same as above
      PZADMIN_BACKUP_DIR: /backups
      PZADMIN_ARCANE_URL: http://host.docker.internal:3552 # CHANGE if Arcane is on another machine
      PZADMIN_ARCANE_ENV_ID: "0"
      PZADMIN_ARCANE_API_KEY: your-arcane-api-key         # CHANGE
      TZ: Europe/London          # times in the container log; schedules use the timezone set in PZAdmin
      # Optional: the game image new servers are created with. Leave it out to
      # use the version this PZAdmin release was tested with.
      # PZADMIN_GAME_IMAGE: indifferentbroccoli/projectzomboid-server-docker:<tag>
      # Optional: only if PZAdmin sits behind a reverse proxy. The proxy's
      # address or network, so sign-in limits see real visitor addresses.
      # PZADMIN_TRUSTED_PROXIES: 172.18.0.0/16
    stop_grace_period: 30s       # time to finish saving on docker compose down
    logging:
      driver: json-file
      options: { max-size: "10m", max-file: "3" }
    read_only: true
    cap_drop: [ALL]
    security_opt: [no-new-privileges:true]

volumes:
  pzadmin-data:
```

**2.** Edit the lines marked `CHANGE`:

| Setting | What to put |
| --- | --- |
| Server folders | The folder holding your server stacks, and the folder holding their game data. Use the same path on both sides of the `:` and in the matching environment variable. On a new machine with no servers yet, make two empty folders and use those. |
| `user` | The user that owns your server folders (`id -u`:`id -g`). Usually `1000:1000`. |
| `PZADMIN_ARCANE_URL` | Leave it if Arcane runs on this machine. Otherwise use Arcane's IP address, because names only your LAN knows won't resolve inside the container. |
| `PZADMIN_ARCANE_API_KEY` | See [Arcane setup](#arcane-setup). Leave the Arcane lines out to run without it. |
| `TZ` | Your timezone. It should match your game servers', because PZAdmin reads the times in their log files in this timezone. |

**3.** In the same folder, make the backups folder and start PZAdmin:

```sh
mkdir backups
docker compose up -d
```

Make `backups` before the first start. If Docker has to create it, it belongs
to root and PZAdmin can't write to it. PZAdmin tells you in its log and its
activity log if that happens; `sudo chown 1000:1000 backups` fixes it (use
your `user` numbers).

**4.** Open **http://your-server:27815**. PZAdmin asks for a setup code, so
nobody else on your network can claim it before you do. Get the code with:

```sh
docker logs pzadmin
```

Enter it, choose your username and password, and you're in. With no servers
yet, the dashboard offers to create your first one.

### Updating

```sh
docker compose pull
docker compose up -d
```

Your settings live in the `pzadmin-data` volume and your backups in
`./backups`, so both survive. Then refresh the page in your browser. An open
tab also tells you when the web interface has changed and needs a reload.

`latest` follows the newest build of the main branch. To stay on one version,
replace `latest` with a release tag from the
[releases page](https://github.com/TheWarBoys2/pzadmin/releases), such as
`v1.0.0`, or `v1.0` to get fixes but no bigger changes. The [changelog](CHANGELOG.md) lists what changed.

### Uninstalling

`docker compose down -v` removes PZAdmin and its settings, accounts and
history. Your backups stay in the `backups` folder until you delete it
yourself. Your game servers and their worlds are never touched.

---

## Arcane setup

PZAdmin never gets access to the Docker socket, which would give it control of
every container on the machine. It asks Arcane to start and stop containers
instead, using a key limited to exactly what it needs.

1. In Arcane, create a role with these permissions:

   | Permission | Used for |
   | --- | --- |
   | `containers:list`, `containers:read` | Server status |
   | `containers:start`, `containers:stop` | Start and stop |
   | `projects:list`, `projects:deploy` | Deploying and creating servers |
   | `containers:logs` *(optional)* | Container log view |
   | `containers:delete` *(optional)* | Deleting servers |

2. Create a user with that role, generate an API key and put it in `docker-compose.yml`.
3. Make sure Arcane's projects directory contains your stacks folder. Arcane
   searches subfolders, so mounting `~/docker` covers `~/docker/pzserver/*`.

PZAdmin only asks Arcane about containers belonging to servers it found in
your stacks folder. Everything else on the machine is off limits to it.

### What works without Arcane

Restarts don't use Arcane. PZAdmin saves the world and tells the server to
quit over RCON, and Docker's `restart: unless-stopped` brings it back. So
without Arcane you still have:

- status, players and the RCON console
- restarts, scheduled jobs and mod-update restarts
- mods, settings, backups and restore
- Discord, the API and metrics

These need Arcane: start, stop, deploy, deleting servers, container logs,
container uptime, and the watchdog's recovery of a frozen server. The
new-server wizard still writes a new server's files without Arcane, but you
start it yourself with `docker compose up -d` in its folder. The watchdog's
recovery needs Arcane because a frozen server no longer answers RCON, so it
can't be asked to quit and has to be stopped and started from outside.

---

## How PZAdmin finds your servers

Every folder in the stacks folder that contains a compose file is treated as a server:

```
/home/you/docker/pzserver/          ← PZADMIN_STACKS_ROOT
└── pz-myserver/
    ├── docker-compose.yml          the game server
    ├── .env                        SERVER_NAME, ports, RCON_PASSWORD, PUID, PGID…
    └── Server/                     ini and lua files

/srv/zomboid/                       ← PZADMIN_DATA_ROOT
└── myserver/projectzomboid/
    ├── data/                       world, mods, logs
    └── config/
```

Ports, the RCON password and paths are read from the files, so there's nothing
to enter by hand. Each game server needs:

- `restart: unless-stopped`, which is how restarts bring it back
- `stop_grace_period: 120s` or at least 60s, so it has time to save
- `env_file: .env` and the RCON port published on the host
- absolute paths for every volume
- a pinned image version, not `latest`
- `PUID`/`PGID` matching PZAdmin's `user`

The **Stack** page checks each server against this list and tells you exactly
what's wrong. Servers created with the wizard meet all of it from the start.
They use `indifferentbroccoli/projectzomboid-server-docker:v1.1.9`, the version
this release was tested with, unless you set `PZADMIN_GAME_IMAGE`.

---

## Configuration

Everything is set in `docker-compose.yml`. Settings with a default can be left out.

| Variable | Default | |
| --- | --- | --- |
| `PZADMIN_STACKS_ROOT` | required | Folder of server stacks |
| `PZADMIN_DATA_ROOT` | required | Folder of server game data |
| `PZADMIN_BACKUP_DIR` | `/data/backups` | Where backups are kept. The compose file above sets `/backups` and mounts `./backups` there. |
| `PZADMIN_ARCANE_URL` | none | Arcane's address. Without the three Arcane settings, see [What works without Arcane](#what-works-without-arcane). |
| `PZADMIN_ARCANE_API_KEY` | none | Arcane API key |
| `PZADMIN_ARCANE_ENV_ID` | none | Arcane environment, `0` for the local one |
| `PZADMIN_GAME_IMAGE` | `indifferentbroccoli/projectzomboid-server-docker:v1.1.9` | Game image new servers are created with |
| `PZADMIN_TRUSTED_PROXIES` | none | Reverse proxies to trust, see [Behind a reverse proxy](#behind-a-reverse-proxy) |
| `PZADMIN_RCON_HOST` | `host.docker.internal` | Where RCON ports are reached |
| `PZADMIN_GAME_ROOT` | none | A shared Project Zomboid install, only for servers without their own |
| `TZ` | `UTC` | Timezone of the container: log timestamps, and reading times in the game servers' logs. Schedules use the timezone you pick in **Settings**, not this. |

Webhooks, metrics, backups and schedules are configured in the web interface.

---

## Backups

A backup is a `.tar.gz` of the server's `Saves` folder, and its `Server`
config folder if you tick that. They're kept in `./backups/<server id>/` next
to your compose file, so you can copy them to another disk or machine like
any other file. Set how many to keep per server under **Edit server**; the
oldest are deleted beyond that.

PZAdmin asks the server to save first, but a backup taken while players are
online can still catch a chunk mid-write. The safest backup is a scheduled job
of *announce → save → wait → back up*, or one taken while the server is
stopped. Use **Verify** to check an archive reads back cleanly.

**Restore** needs the server stopped. PZAdmin reads the whole archive first
and changes nothing if it's damaged. Then it puts the archive's world in place
of the current one, and the server settings too if the backup includes them.
Nothing is deleted: the current world is moved to `Saves.before-restore` and
the current settings to `Server.before-restore`, next to the originals. These
replace the copies from an earlier restore only once the new restore has
worked, and if it fails, everything is put back. Delete the
`.before-restore` folders once you're happy. The settings folder is replaced
as a whole, so a settings file added after the backup is only in
`Server.before-restore` afterwards.

Upgrading from an earlier PZAdmin that kept backups inside its volume: once
`PZADMIN_BACKUP_DIR` is set, PZAdmin moves the old archives into the new
folder by itself the next time it starts, and notes it in the activity log.

---

## Discord

PZAdmin posts to Discord through webhooks, or through a bot if you have one. Everything is on the **Discord** page in
the sidebar.

**Give each server its own channel.** In Discord, open the channel's settings, then Integrations → Webhooks →
New Webhook → Copy Webhook URL. You don't need to name it or give it a picture there. On the Discord page, click
**Set up channel** next to the server and paste the link. That channel then gets:

- short, plain announcements when the server actually goes down for a restart (and why: scheduled, mod updates,
  stopped responding, or the reason you typed when you pressed Restart or Stop) and when it's back. Countdown
  warnings stay in game. You can change the wording and ping a role by writing `<@&role ID>` into it.
- a **status message**, edited only when something changes: the server comes online, restarts or goes down, or the
  player count moves. Pin it. It shows the server's description and join address, which you type into the server's
  **Public info**, because PZAdmin only sees the server from inside Docker.

Players only see the servers whose channels they can read.

**Name and picture come from PZAdmin.** Messages are posted as the name and picture set under
**How PZAdmin appears in Discord**. A server's own channel uses the server's name, and any channel can have its own
name and picture. The picture has to be a link Discord can reach, such as an image uploaded to Discord.

**Or use a bot.** Webhooks need no bot, but if you'd rather not make a webhook per channel, or you already run a
bot, connect it under **Discord bot** on the same page:

1. At [discord.com/developers](https://discord.com/developers/applications): New Application → Bot → Reset Token, and
   copy the token.
2. OAuth2 → URL Generator: tick **bot**, then View Channels, Send Messages, Embed Links and Read Message History, plus
   **Manage Channels** if you want the status dot. Open the generated link to invite the bot.
3. Paste the token into PZAdmin and click **Connect**.

Each channel can then send with the bot, and you pick the channel from a list. Messages sent by the bot use the
bot's own name and picture. PZAdmin never keeps a connection to Discord open: it only brings a new bot online once
when you connect it, because Discord won't let a bot post before it has been online.

**Status dot in the channel name.** With the bot connected, a server's own channel can show 🟢 (up), 🟠 (restarting)
or 🔴 (down) in front of its name. This works whether the channel sends with a webhook or the bot, as long as the bot
has Manage Channels there. Discord only allows two renames per channel every ten minutes. A restart or deploy
PZAdmin does shows 🟠 straight away and 🟢 once the server answers again, which uses both. An outage or recovery has to
last a minute and a half before it shows, so a short blip doesn't spend a rename. The status
message and announcements carry the detail. Turn the dot off and the channel gets its plain name back.

**Staff and shared channels** cover several servers at once: a staff channel gets outages, watchdog restarts, failed
backups and admin actions with full detail; a shared channel announces several servers in one place.

**Scheduled jobs** can post too: add a **Post to Discord** step, choose the channel and write the message.
`{server}` and `{players}` are filled in when the job runs.

**Quiet hours** keep Discord quiet overnight. Switch them on under **Quiet hours** on the Discord page, pick the
times in PZAdmin's timezone (23:00 until 08:00 runs overnight), and tick what to keep quiet: **Scheduled jobs**, **Restarts,
stops and starts I do myself** (from the dashboard or the API), or both. Ticked actions still happen and players in game
still see any countdown, but their restart, stop, start and back-online posts aren't sent; for scheduled jobs that also covers backups and **Post
to Discord** steps. Crashes, outages and failed backups always post.

Project Zomboid also has its own Discord bot, which relays in-game chat. It's set in the server ini
(`DiscordEnable`, `DiscordToken`, and on Build 42 `DiscordChatChannel`, `DiscordLogChannel` and
`DiscordCommandChannel`), and the settings form has a Discord group for it. The two work side by side: the game's bot
relays chat, and PZAdmin announces restarts. Anyone who can post in the command channel can run admin commands, so
keep that channel to staff.

## API

Bots and scripts can read server status, players and the event log, and run
commands, restarts and backups, through the API at `/api/v1`. Make a key under
**Settings → API keys**, choosing what it may do and which servers it covers.
The key is shown once. See [docs/api.md](docs/api.md) for every endpoint.


---

## Metrics

PZAdmin serves Prometheus metrics at `/metrics`: player counts, uptime, RCON
latency, backup sizes, missing mods and API use. They're on by default and
protected by a token. Click **Make a new token** under **Settings → Metrics**,
copy it, and put it in your scrape job:

```yaml
scrape_configs:
  - job_name: pzadmin
    static_configs: [{ targets: ["your-server:27815"] }]
    authorization: { credentials: <your metrics token> }
```

The token is shown once when you make it; making another replaces it. Untick
**Publish metrics** to turn `/metrics` off.

---

## Security

- **No Docker socket.** PZAdmin controls containers only through Arcane, only
  for servers it has found, and only with the permissions above.
- **Locked-down container.** Runs as your user with a read-only filesystem, no
  capabilities and no privilege escalation.
- **Secrets stay on the server.** RCON passwords, webhook URLs and tokens are
  never sent to the browser or included in exports.
- **One admin account**, set up with a one-time code from the log, with
  rate-limited sign-in, CSRF protection and an audit log.
- **API keys are scoped and hashed.** A key only reaches `/api/v1`, never
  settings, the password or other keys, and PZAdmin keeps only a hash of it.

PZAdmin serves plain HTTP. On your LAN, or over Tailscale or a VPN, that's
fine. **Don't port-forward it to the internet.** If you want it reachable from
outside, put it behind a reverse proxy with HTTPS, or better, a VPN.

To make it reachable from the host machine only, publish the port as
`"127.0.0.1:27815:27815"`.

### Behind a reverse proxy

A reverse proxy such as Caddy, Nginx Proxy Manager or Traefik adds HTTPS.
PZAdmin works behind one without extra settings, with one catch: every visitor
then seems to come from the proxy's address, so the sign-in rate limit, the
audit log and session list see one address for everyone.

To fix that, tell PZAdmin the proxy's address with `PZADMIN_TRUSTED_PROXIES`,
as an address or range, for example `172.18.0.0/16` for a proxy on a Docker
network. PZAdmin then reads the visitor's address and whether they used HTTPS
from the proxy's `X-Forwarded-For` and `X-Forwarded-Proto` headers. Those
headers are ignored from anyone else, because anyone can send them.

The live dashboard uses a long-lived event stream. PZAdmin sends the header
that tells Nginx not to buffer it, and Caddy and Traefik pass it through
as is, so there is usually nothing to set. If the dashboard only updates when
you refresh, turn off response buffering for PZAdmin in your proxy.

---

## Troubleshooting

**PZAdmin won't start.** Run `docker logs pzadmin`, which says why. It's
usually a server folder path that doesn't match the real folder, or a data
volume owned by another user because the `user:` line changed after the
first start. The log gives the exact command to fix the owner; the volume's
name, from `docker volume ls`, ends in `pzadmin-data`.

**I lost the setup code.** It's in `docker logs pzadmin`. It changes every
time PZAdmin restarts until setup is finished, so use the newest one.

**"Backups cannot be saved".** The `backups` folder belongs to another user,
usually root because Docker made it. Run `sudo chown 1000:1000 backups` in
PZAdmin's folder, using your `user` numbers, then restart PZAdmin.

**"Arcane is unavailable … no such host".** The container can't resolve
Arcane's hostname. Use `http://host.docker.internal:<port>` or Arcane's IP address.

**A server shows "Offline" but it's running.** PZAdmin can't reach its RCON
port. Check the port is published in the server's compose file and that your
firewall allows traffic from Docker's network.

**A setting didn't take effect.** Changes to a server's compose file or `.env`
need the container recreated, with **Deploy** or `docker compose up -d`. A
restart isn't enough.

**"Mods not installed on disk".** Usually a mod listed in `Mods=` whose
Workshop ID is missing from `WorkshopItems=`, so it never downloads. The mod
manager can add the missing IDs for you.

**The page looks broken after an update.** Refresh it. If that doesn't help,
do a hard refresh (Ctrl+Shift+R, or Cmd+Shift+R on a Mac).

---

## Development

PZAdmin is a single Go binary with no dependencies outside the standard
library. The web interface is plain JavaScript with no build step, embedded
in the binary. You need Go 1.27, and Node.js for the frontend tests.

```sh
go test ./...                    # full suite, including frontend tests if node is installed
go vet ./...

# run your checkout in Docker
docker compose -f docker-compose.yml -f docker-compose.dev.yml up -d --build
```

See [CONTRIBUTING.md](CONTRIBUTING.md) for the checks CI runs.

<details>
<summary>Project layout</summary>

```
main.go                 entry point
internal/server         HTTP API, monitoring, scheduler, restarts
internal/stacks         server discovery and mount checks
internal/provision      new-server wizard, .env editing
internal/pz             ini, sandbox, mods, logs, backups, item catalogue
internal/arcane         Arcane API client
internal/compose        compose and .env parsing
internal/…              rcon, steam, store, cronx, notify, config, fsutil
web/                    the interface; web/testkit holds its tests
```

</details>

<details>
<summary>Releases and images</summary>

GitHub Actions tests, builds (amd64 and arm64) and publishes to
`ghcr.io/thewarboys2/pzadmin`. Nothing is published if a test fails.

| Tag | Published on | |
| --- | --- | --- |
| `latest` | every push to `main` | moves |
| `v1.0.0` | Git tag `v1.0.0` | fixed |
| `v1.0` | newest `v1.0.x`, not pre-releases | moves |
| `sha-abcdef1` | every build | fixed |

To release: `git tag v1.0.0 && git push origin v1.0.0`. A pre-release tag such
as `v1.0.0-beta.2` publishes only its own tag.

</details>

---

## Licence

MIT, see [LICENSE](LICENSE). Use it, change it, share it.

Project Zomboid is a trademark of The Indie Stone. PZAdmin is a fan project
and isn't affiliated with or endorsed by The Indie Stone, Arcane or the
authors of the game server image.
