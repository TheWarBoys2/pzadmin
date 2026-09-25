# PZAdmin API

PZAdmin has an HTTP API under `/api/v1` for scripts and bots, such as a Discord
bot that shows who is online or restarts a server on command. It is part of the
same container and port as the web interface; there is nothing extra to run.

## Keys

Create a key under **Settings → API keys**. Each key has:

- **A name**, which the event log shows as `key:<name>` for everything it does.
- **Access.** Every key can **read**. **Control** adds commands from the command
  list, restart, stop, start and backups. **Console** adds raw RCON commands;
  only give it to something you trust completely.
- **Servers.** All servers (including ones added later) or a chosen few. A
  server outside the key's list answers `404`, as if it did not exist.
- **Expiry**: never, 30 days, 90 days or a year.

The key is shown once, when it is made. PZAdmin stores only a SHA-256 hash of
it in `/data/apikeys.json`, so a lost key cannot be recovered: revoke it and
make another. Keys are not part of config exports or imports, and changing the
password does not revoke them. **Revoke all** does.

A key cannot sign in to the web interface, change settings or the password, or
make and revoke keys. The web interface's own `/api/...` routes refuse keys, and
`/api/v1` refuses browser sessions.

Keys are sent in a header, so use HTTPS if the API is reached over anything but
your own network.

## Requests

Send the key as a bearer token:

```sh
curl -H "Authorization: Bearer pzk_abcd1234_..." http://pzadmin:27815/api/v1/servers
```

Bodies are JSON. Unknown fields are refused. Errors look like
`{"error": "what went wrong"}`.

| Status | Meaning |
|---|---|
| 401 | No key, or it is wrong, expired or revoked |
| 403 | The key does not have the scope this needs |
| 404 | No such server (or the key may not see it), or no such endpoint |
| 409 | A restart, stop or start is already running for that server |
| 429 | Rate limited; wait for `Retry-After` seconds |
| 502 | The game server did not answer over RCON |

## Rate limits

Per key, in tokens that refill steadily:

| What | Limit |
|---|---|
| Reads (`GET`) | 120 a minute, bursts of 30 |
| Writes (`POST`) | 20 a minute, bursts of 5 |
| Restart, stop and start, per server | 3 in a row, then 1 every 2 minutes |
| Open event streams | 3 at a time |

Every response carries `X-RateLimit-Limit` and `X-RateLimit-Remaining`. Repeated
bad keys from one address are slowed down the same way failed sign-ins are.

## Reading

### `GET /api/v1/key`

The key's own name, scopes, servers and expiry, plus the PZAdmin version.
Useful to check a key works.

### `GET /api/v1/servers`

```json
{
  "servers": [{
    "id": "3f9c...", "name": "Riverside", "enabled": true,
    "state": "online",
    "online": true, "restarting": false, "stopped": false, "deploying": false,
    "players": ["Rick"], "playerCount": 1, "maxPlayers": 32,
    "description": "Vanilla, 6 months later", "address": "pz.example.com", "port": 16261,
    "latencyMs": 4, "lastCheck": "2026-09-25T21:00:00Z", "lastOnline": "2026-09-25T21:00:00Z",
    "startedAt": "2026-09-25T06:00:00Z", "uptimeSec": 54000,
    "modsEnabled": 12, "modsMissing": [],
    "backupCount": 10, "lastBackup": "2026-09-25T18:00:00Z", "backupRunning": false,
    "pendingRestartAt": null
  }]
}
```

`state` is one of `online`, `offline`, `restarting`, `stopped`, `deploying` or
`unknown` (not checked yet). `description`, `address` and `port` are the public
info set for the server's Discord channel; `port` falls back to the game port.
Times are RFC 3339, or `null` when there is none.

### `GET /api/v1/servers/{id}`

One server, in the same shape.

### `GET /api/v1/servers/{id}/players`

Everyone PZAdmin has seen on the server: `name`, `online`, `onlineSince`,
`firstSeen`, `lastSeen`, `sessions`, `playtimeSec` and `banned`. Steam IDs and
the notes you keep on players are left out.

### `GET /api/v1/events?server=&kind=&limit=`

The event log, newest first. `limit` defaults to 50, up to 500. `kind` filters
to one kind, such as `server.restart`, `server.down` or `admin.action`. Sign-in
events are never included, and a key limited to some servers sees only their
events.

### `GET /api/v1/stream`

A [Server-Sent Events](https://developer.mozilla.org/en-US/docs/Web/API/Server-sent_events)
stream. It sends `status` (`{"servers": [...]}` as above) when it opens and
whenever something changes, and `event` for each new event log entry. A comment
line every 20 seconds keeps it open.

### `GET /api/v1/commands`

The command list the `actions` endpoint accepts: each command's `id`, `label`,
`params` and whether it is `danger`ous.

## Control

These need the **control** scope. They do exactly what the matching button in
the web interface does, including the Discord announcements.

### `POST /api/v1/servers/{id}/actions`

```json
{ "action": "servermsg", "args": ["Restart in 5 minutes"] }
```

Runs a command from `/api/v1/commands`, checked the same way as in the web
interface. Returns `{"ok": true, "response": "...", "command": "..."}`.

### `POST /api/v1/servers/{id}/lifecycle`

```json
{ "action": "restart", "reason": "Updating mods" }
```

`action` is `restart`, `stop`, `start` or `cancel-pending`. `reason` is
optional, up to 200 characters, and is shown to players in Discord
announcements. A restart or stop can take a few minutes to answer.

### `POST /api/v1/servers/{id}/backups`

```json
{ "note": "Before the map change" }
```

Saves the world and takes a backup. The body may be empty.

## Console

### `POST /api/v1/servers/{id}/console`

Needs the **console** scope.

```json
{ "command": "players" }
```

Sends one RCON line. Returns `{"ok": true, "response": "..."}`. Every console
command is recorded in the event log.

## Metrics

`/metrics` includes `pzadmin_api_requests_total{key,code}` and
`pzadmin_api_ratelimited_total{key}`.
