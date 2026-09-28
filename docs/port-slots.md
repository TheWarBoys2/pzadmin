# Port slots

If you have more servers than you ever run at once, port slots let them share
a few forwarded ports instead of each needing its own.

You pick a small range, for example 16261 to 16264. That's two **slots** of
two ports each: 16261 and 16262, then 16263 and 16264. A server set to use a
slot takes a free one when you start it from PZAdmin and gives it back when it
stops. Only that range needs forwarding on your router.

## Setting it up

1. **Settings → Port slots.** Tick *Use port slots*, set the first port and how
   many slots you want, and save. The box tells you which ports to forward.
2. **Each server's settings.** Open a server (Manage → Edit), tick *Use a port
   slot*, and save. Do this for each server that should share the slots.
   Servers you don't tick keep their own ports, as before.
3. **Your router.** Forward the UDP ports in the range to the Docker host. Once
   every server you play on uses a slot, you can remove the forwards for the
   old per-server ports.

Only the owner can change these settings.

## What happens when you start a server

- PZAdmin picks a free slot. If the one the server used last time is free, it
  gets that one again, so its port usually stays the same.
- It writes the slot's ports into the server's `.env` (`DEFAULT_PORT` and
  `UDP_PORT`) and keeps a copy of the old `.env` with the server's backups.
- It runs `docker compose up -d` for the server through Arcane. If the ports
  changed, Docker recreates the container with the new ones. If they didn't,
  it just starts it. The result appears in Activity.
- If every slot is taken, the start is refused and PZAdmin says which servers
  have them, for example *All 2 port slots are in use: Muldraugh on 16261,
  Riverside on 16263. Stop one of them first.*

The dashboard shows which server holds which slot, and each server's row shows
its slot and port.

A server holds its slot while its container is running or starting, so
restarts (scheduled, manual, mod updates and the watchdog) keep it. Stopping
it from PZAdmin frees the slot straight away. Nothing about slots is stored: PZAdmin
works it out each time from the ports in each stack folder and whether the
container is up.

## What changes on disk

The first time you tick *Use a port slot* for a server, PZAdmin changes the two
game port lines under `ports:` in its compose file so they read `.env`:

```yaml
    ports:
      - "${DEFAULT_PORT}:${DEFAULT_PORT}/udp"
      - "${UDP_PORT}:${UDP_PORT}/udp"
      - "27015:27015/tcp"   # RCON: PZAdmin needs this
```

It also writes the server's current ports into `.env`, so nothing moves until
the next start. The old compose file is kept next to it as
`docker-compose.yml.before-slots-<date>`. Nothing else in the file changes.
The image writes `DEFAULT_PORT` and `UDP_PORT` into the server's ini on every
boot, so the game listens on whatever the slot says.

Servers created by the new-server wizard are written this way from the start.
When slots are on, the wizard gives a new server ports of its own outside the
slot range.

If PZAdmin can't find both game port lines (for example, a port range like
`16261-16262:16261-16262/udp`), it changes nothing and tells you to edit them
by hand to the two lines above.

## Things to know

- **The join port can change.** If another server has a server's usual slot,
  it starts on the other one. PZAdmin's Discord status message, the API and
  the Discord cog always show the port it's on now. A player's saved favourite
  in the game can point at the old port. For a slot server, the *Join port* box
  in its settings is ignored for this reason.
- **Starting needs Arcane.** A slot start is a `docker compose up -d`, so the
  Arcane key needs `projects:list` and `projects:deploy`, the same as the
  Deploy button.
- **Starts outside PZAdmin aren't checked.** If you start a slot server from
  Arcane or with `docker compose up -d` yourself, it uses whatever ports its
  `.env` has. PZAdmin still counts it as holding that slot. If that slot was
  already taken, Docker refuses to start it with "port is already allocated".
  After a host reboot, `restart: unless-stopped` brings servers back on the
  ports they had, which never clash.
- **Servers without a slot can block one.** A server that doesn't use slots but
  publishes a port inside the range holds that slot while it runs. Give it
  ports outside the range, or tick *Use a port slot* for it too.
- **Ticking the box on a running server** changes nothing until you next start
  it from PZAdmin. It keeps its old ports until then, and holds no slot.
- **Changing the range.** Servers already running keep their ports until they
  next start. Stop them before you move the range.
- **RCON isn't affected.** Each server keeps its own RCON port. RCON is only
  used by PZAdmin inside your network, so it never needs forwarding.

## Turning it off

Untick *Use port slots* in Settings. Slot servers then start normally, on
whatever ports their `.env` last had. Their compose files keep reading `.env`,
which works the same as fixed ports. To put a compose file back exactly as it
was, copy the `before-slots` file over it.
