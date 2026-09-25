# PZAdmin

A web control panel for Project Zomboid dedicated servers running in Docker.

Watch every server from one dashboard. Restart on a schedule, manage mods and
settings, back up worlds and run admin commands without SSH or a raw RCON
client.

Built for servers on the
[`indifferentbroccoli/projectzomboid-server-docker`](https://github.com/indifferentbroccoli/projectzomboid-server-docker)
image, one Compose stack per server, managed through [Arcane](https://getarcane.app).

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
- Backups with retention, download and restore
- Discord: staff alerts, player announcements when a server restarts and comes back, a status message per server, and Discord posts from scheduled jobs
- Prometheus metrics and an audit log of every action

---

## Requirements

- Linux host with Docker and Docker Compose
- [Arcane](https://getarcane.app), for starting and stopping containers
- Game servers on `indifferentbroccoli/projectzomboid-server-docker`, one Compose stack per server ([layout below](#how-pzadmin-finds-your-servers))

---

## Install

**1.** Create a folder for PZAdmin and a `docker-compose.yml` inside it:

```yaml
# PZAdmin. Copy this file into an empty folder on the server, change the
# lines marked CHANGE, then run:  docker compose up -d
# Open http://<host>:27815 and create the admin account.

services:
  pzadmin:
    image: ghcr.io/thewarboys2/pzadmin:latest
    pull_policy: always          # every "docker compose up -d" gets the newest build
    container_name: pzadmin
    restart: unless-stopped
    user: "1000:1000"            # CHANGE if needed: must own your server folders (id -u, id -g)
    ports:
      - "27815:27815"
    extra_hosts:
      - "host.docker.internal:host-gateway"   # reaches each server's RCON port on the host
    volumes:
      - pzadmin-data:/data
      # CHANGE: your two server folders. Same path on both sides of the colon.
      - /srv/zomboid:/srv/zomboid
      - /home/youruser/docker/pzserver:/home/youruser/docker/pzserver
    environment:
      PZADMIN_DATA_ROOT: /srv/zomboid                     # CHANGE: same as above
      PZADMIN_STACKS_ROOT: /home/youruser/docker/pzserver # CHANGE: same as above
      PZADMIN_ARCANE_URL: http://host.docker.internal:3552 # CHANGE if Arcane is on another machine
      PZADMIN_ARCANE_ENV_ID: "0"
      PZADMIN_ARCANE_API_KEY: your-arcane-api-key         # CHANGE
      TZ: Europe/London
      # Optional: the pinned game image new servers are created with.
      # PZADMIN_GAME_IMAGE: indifferentbroccoli/projectzomboid-server-docker@sha256:...
    read_only: true
    cap_drop: [ALL]
    security_opt: [no-new-privileges:true]

volumes:
  pzadmin-data:
```

**2.** Edit the lines marked `CHANGE`:

| Setting | What to put |
| --- | --- |
| Server folders | The folder holding your server stacks, and the folder holding their game data. Use the same path on both sides of the `:` and in the matching environment variable. |
| `PZADMIN_ARCANE_URL` | Leave it if Arcane runs on this machine. Otherwise use Arcane's IP address, because names only your LAN knows won't resolve inside the container. |
| `PZADMIN_ARCANE_API_KEY` | See [Arcane setup](#arcane-setup). |
| `user` | The user that owns your server folders (`id -u`:`id -g`). Usually `1000:1000`. |

**3.** Start it:

```sh
docker compose up -d
```

Open **http://your-server:27815** and create your admin account.

### Updating

Run `docker compose up -d` again. `pull_policy: always` fetches the latest
image each time. Your settings live in the `pzadmin-data` volume, so they
survive.

To stay on one version, replace `latest` with a release tag such as `v1.0.0`
and remove the `pull_policy` line.

### Uninstalling

`docker compose down -v` removes PZAdmin and everything it stored. Your game
servers and their worlds are never touched.

---

## Arcane setup

PZAdmin never gets access to the Docker socket. It asks Arcane to start and
stop containers instead, using a key limited to exactly what it needs.

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

Restarts don't use Arcane at all. They go over RCON, so they keep working when Arcane is down.

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

---

## Configuration

Everything is set in `docker-compose.yml`. Settings with a default can be left out.

| Variable | Default | |
| --- | --- | --- |
| `PZADMIN_STACKS_ROOT` | required | Folder of server stacks |
| `PZADMIN_DATA_ROOT` | required | Folder of server game data |
| `PZADMIN_ARCANE_URL` | required | Arcane's address |
| `PZADMIN_ARCANE_API_KEY` | required | Arcane API key |
| `PZADMIN_ARCANE_ENV_ID` | required | Arcane environment, `0` for the local one |
| `PZADMIN_GAME_IMAGE` | none | Pinned game image for new servers. Without it, creating servers is disabled. |
| `PZADMIN_RCON_HOST` | `host.docker.internal` | Where RCON ports are reached |
| `TZ` | `UTC` | Time zone for schedules |

Webhooks, metrics, backups and schedules are configured in the web interface.

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

**Status dot in the channel name.** With the bot connected, a server's own channel can show 🟢 or 🔴 in front of
its name. This works whether the channel sends with a webhook or the bot, as long as the bot has Manage Channels
there. Discord only allows two renames per channel every ten minutes, so the dot follows the server's settled state:
a restart you started leaves it alone, and a change has to last a minute and a half before it shows. The status
message and announcements carry the detail. Turn the dot off and the channel gets its plain name back.

**Staff and shared channels** cover several servers at once: a staff channel gets outages, watchdog restarts, failed
backups and admin actions with full detail; a shared channel announces several servers in one place.

**Scheduled jobs** can post too: add a **Post to Discord** step, choose the channel and write the message.
`{server}` and `{players}` are filled in when the job runs.

Project Zomboid also has its own Discord bot, which relays in-game chat. It's set in the server ini
(`DiscordEnable`, `DiscordToken`, and on Build 42 `DiscordChatChannel`, `DiscordLogChannel` and
`DiscordCommandChannel`), and the settings form has a Discord group for it. The two work side by side: the game's bot
relays chat, and PZAdmin announces restarts. Anyone who can post in the command channel can run admin commands, so
keep that channel to staff.

## Security

- **No Docker socket.** PZAdmin controls containers only through Arcane, only
  for servers it has found, and only with the permissions above.
- **Locked-down container.** Runs as your user with a read-only filesystem, no
  capabilities and no privilege escalation.
- **Secrets stay on the server.** RCON passwords, webhook URLs and tokens are
  never sent to the browser or included in exports.
- **One admin account**, with rate-limited sign-in, CSRF protection and an audit log.

Keep it on your LAN or behind Tailscale or a VPN. **Don't port-forward it to
the internet.** To make it reachable from the host only, publish it as
`"127.0.0.1:27815:27815"`.

---

## Troubleshooting

**PZAdmin won't start.** Run `docker logs pzadmin`, which says why. It's
usually a server folder path that doesn't match the real folder.

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

---

## Development

PZAdmin is a single Go binary with no dependencies outside the standard
library. The web interface is embedded in it.

```sh
go test ./...                    # full suite, including frontend tests if node is installed
go vet ./...

# run your checkout in Docker
docker compose -f docker-compose.yml -f docker-compose.dev.yml up -d --build
```

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
internal/…              rcon, steam, store, cronx, notify, config
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
| `v1.0` | newest `v1.0.x` | moves |
| `sha-abcdef1` | every build | fixed |

To release: `git tag v1.0.0 && git push origin v1.0.0`.

</details>

See [CHANGELOG.md](CHANGELOG.md) for what's new.
