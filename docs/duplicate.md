# Duplicating a server

**Duplicate** on the Server setup page makes a new server that is a full copy of an
existing one: its settings, mods, sandbox, and its world with every character,
safehouse and account. The copy gets its own stack folder, data and config
folders, ports and RCON password. Nothing about the original changes.

Use it to try a mod or a rule change on a copy of the real world, or to split
one world into two servers. To make a new server that only shares settings,
without the world, use a [template](templates.md) or the wizard's *Copy an
existing server* instead.

Only the owner can duplicate servers.

## Before you start

- **Stop the server first.** PZAdmin refuses while it's running, because a
  copy of a world that is being written can be half old, half new.
- **It stays stopped until the copy finishes.** PZAdmin won't start, restart,
  deploy, back up, restore or reset it meanwhile. It can't stop you starting it
  from Arcane or on the host, so don't.
- **Space.** The copy needs about as much free space as the world. PZAdmin
  checks there's room for the copy plus 10% before it begins.

## What's copied

Everything in the server's config folder (the one mounted at
`/project-zomboid-config`), except:

- `Logs/`, which is the original's history
- `backups/`, the game's own start-up backups, which can be large
- `Server/`, which comes from the stack folder and is written fresh

The copy keeps the original's `ResetID`, `ServerPlayerID` and `Seed`. That is
on purpose: players' characters in the world belong to those IDs, and new ones
would make everyone start a new character. It's also why the wizard can't make
a duplicate, only this button can.

`.env` keeps the admin username and password, memory, max players and
*update on start*. If the original's admin password can't be used (for example
it's blank), you're asked for one.

If you give the copy a different `SERVER_NAME` (by default it's the new stack
name without `pz-`), the world folder in `Saves/Multiplayer` and the accounts
file in `db/` are renamed to match, as the game finds them by that name.

**Not copied:** the game itself and mod downloads (the data folder). The first
start downloads them again, which takes a while. PZAdmin's own settings for the
server (backups, schedules, Discord) aren't copied either. If the original uses
a port slot, so does the copy.

## While it copies

A progress bar shows how much is done. You can close it; the copy carries on
and Activity says when it's finished or why it failed. Nothing half-copied is
ever shown as a server, because the new stack folder is only written once the
world is copied in full, and a failed copy removes what it made.

If PZAdmin itself restarts in the middle, the copy stops and can't clean up:
the half-made config folder stays in your data folder. Delete it before trying
again with the same name, or pick another name. PZAdmin won't copy into a
folder that already has files in it.

When it's done, start the copy from the dialog (with Arcane), or run the
command it gives you on the host.

## "Permission denied"

The game server usually writes its files as a different user from PZAdmin
(often root). If PZAdmin can't read one, the copy stops and tells you the
`chown` command to run on the host. Setting `PUID` and `PGID` in the server's
`.env` to PZAdmin's user stops it happening again.
