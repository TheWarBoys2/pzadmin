# Other admins

PZAdmin has one **owner**: the account you made at setup. The owner can add
accounts for other admins, choose what each one can do, and limit each one to
some servers. Everyone signs in on the same page with their own username and
password, and everything they do shows in Activity under their own name.

## Adding someone

1. Open **Settings → Users** and choose **Add user**.
2. Type their username, pick a preset or tick what they can do, and choose
   their servers.
3. PZAdmin shows a temporary password once. Send it to them, with the address
   you use to open PZAdmin.
4. When they sign in, PZAdmin asks them to choose their own password before
   anything else. The temporary one stops working.

If they lose it before signing in, open **Manage** next to their name and
choose **Reset password**. That signs them out and makes a new temporary
password. PZAdmin does not send email, so a forgotten password always goes
through the owner.

## What each permission allows

Everyone can **view**: the dashboard, players, logs, activity, charts, the
server's config files (with passwords hidden) and the list of backups.

| Permission | Adds |
|---|---|
| Control | Start, stop and restart; run commands from the command list, which includes kick, ban, giving items and setting access levels; make and verify backups; run an existing scheduled job now; player notes; add to the item catalogue |
| Mods | Change the mod list and load order; approve or reject mod requests |
| Config | Edit the server's ini, sandbox and `.env`; redeploy; download, restore and delete backups; clear player history |
| Console | Send any RCON command |
| Live | See where every player is and how they are doing on the Live tab and map, and turn on live updates (needs the [Companion mod](companion.md)) |

The presets fill in the ticks: **Viewer** is view only, **Operator** adds
Control, **Manager** adds Control, Mods and Config. Console is never part of a
preset, and neither is Live. Only the owner can upload or line up the map
image.

## Only the owner can

- add, change, reset, disable or remove users
- make or revoke API keys
- change PZAdmin's own settings: Discord, webhooks, quiet hours, metrics,
  timezone, import and export
- add, edit, remove or delete servers, create new ones and open the Stack page
- create, edit or delete scheduled jobs
- see every signed-in browser and sign them all out

## Limiting someone to some servers

Untick **All servers** and tick the ones they may use. They see nothing about
the others: not on the dashboard, in Activity, in charts or in the live feed,
and asking for one directly gets "no such server". They also don't see events
that belong to no server, such as sign-ins and settings changes; only the owner
does.

If you delete a server, it is taken off everyone's list. Someone who had only
that server is disabled, not given every server.

## Changing, disabling and removing

- A change to someone's permissions or servers applies from their next click.
  They are not signed out.
- **Disable** signs them out and stops them signing in. Their account and
  history are kept, and **Enable** lets them back in.
- **Remove** signs them out and deletes the account. What they did stays in
  the activity log under their name.
- Changing your own password signs out only your own browsers. **Sign out
  everywhere**, which only the owner has, signs out everyone.

## What to trust people with

The permissions stop someone doing what they weren't given, but what they
were given can still do real damage:

- **Live** shows every player's position in real time. Someone who has it
  can find people in the game, so treat it like the Console.
- **Console** can do anything RCON can, including banning every player or
  wiping access levels. Give it only to someone you would trust with the
  server itself.
- **Config** can break a server with a bad setting, restore an old backup over
  the current world, and download backups, which include the server's config
  folder when backups are set to include it.
- **Control** can kick, ban and give items through the command list, and stop
  a server.

PZAdmin records who did what, so you can find out afterwards, but it cannot
undo it for you.

## Where accounts are kept

Other users are stored in `users.json` in PZAdmin's data volume, next to
`apikeys.json`. Passwords are stored as salted PBKDF2 hashes, the same as the
owner's. Users are not part of a config export or import. The owner account
stays in `config.json`, where it always was, so an install with no other users
works exactly as before. Other users' sign-ins are kept in
`sessions-users.json`, apart from the owner's, so going back to an older
version of PZAdmin ignores both files: other users simply can't sign in, and
none of their sessions can be mistaken for the owner's.

API keys are still made by the owner only. A user cannot make one.
