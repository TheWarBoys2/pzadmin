# Server templates

A template is a saved starting point for new servers. Use one when you make
servers that are mostly the same, with a few differences each time: the
template holds what they share, and the new-server wizard lets you change the
rest before anything is written.

Templates are on the **Stack** page, under **Templates**. Only the owner can
make, edit, use or delete them, because only the owner can create servers.

## What a template holds

- The server settings (the server's `.ini`), including its mods (`Mods=`,
  `WorkshopItems=`) and `Map=`
- The world settings (`SandboxVars.lua`)
- The spawn regions and spawn points, if the source had them
- Max players, memory and *Update from Steam on every start*

It does **not** hold:

- a world or any save. For a copy of a server's world, see *Copying a server*
  below.
- the RCON password, which is blanked when the template is saved. Every new
  server gets a fresh one.
- the per-server IDs (`ResetID`, `ServerPlayerID`) and the world seed. Every
  server made from a template gets its own, so players' games don't mistake it
  for another server.
- the stack name, `SERVER_NAME`, ports, folders or the in-game admin account.
  The wizard asks for those each time.

A template does keep the join password (`Password=`) and any Discord bot token
(`DiscordToken=`) if the source had them, the same as copying a server does.
Clear them in the template if you don't want them passed on.

## Making one

- **From a server:** on the Stack page, press **Save as template** on the
  server. It saves the server's files as they are now.
- **From the wizard:** on the wizard's last step, press **Save these choices as
  a template**. It saves the starting point with every change you made in the
  wizard, including mods added on the Mods step, plus max players and memory.
  The wizard stays where it was, so you can still create the server.

## Using one

- Press **New server** on the template. The wizard opens with the template
  picked and its max players and memory filled in.
- Or, in the wizard, choose **A template** on the *Starting point* step. Picked
  this way, the template's settings are used but the first page keeps what you
  typed there.

Anything you change in the wizard applies to the new server only. The template
is not changed.

## Editing and deleting

**Edit** opens the template's name, description, basics, server settings and
world settings. Saving changes what servers made from it **from now on**.
Servers already made from it keep their own files: a template is only read
when a server is created.

**Delete** removes the template. Servers made from it are not changed.

## Copying a server

The wizard's other starting point, **Copy an existing server**, reads another
server's settings at the moment you create the new one, without saving a
template. Like a template, it leaves out the world, the RCON password and the
per-server IDs.

## Where templates are kept

In `templates.json` in PZAdmin's data folder (`/data`), readable only by
PZAdmin. They are not part of **Settings → Export**, so back up the data folder
if you want to keep them.
