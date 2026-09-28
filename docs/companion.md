# PZAdmin Companion mod

RCON can tell PZAdmin who is online, but not where they are, how they are
doing, or what time and weather it is in the game. The PZAdmin Companion is a
small server-side Lua mod that fills that gap. It is optional: everything else
in PZAdmin works without it.

**Status: early and untested on a live server.** The mod has been checked
against a stubbed copy of the game's Lua functions (`mod/test/run.lua`), not
yet on a running Build 42 server. Expect a round of fixes the first time it
meets the real game.

## What it adds

- A **Live** tab on each server: every online player's position, health,
  infection, kills, time survived and profession, plus the in-game date, time
  and weather. Press **Start** to update every two seconds.
- A **map** on the Live tab with a pin for each player, if you upload a map
  image (see [The map](#the-map)). Click a pin for that player's actions.
- An **in-game strip** on the server's Overview tab: day, time, weather and
  temperature.
- A `world` field on `GET /api/v1/servers` and `/api/v1/servers/{id}` with the
  date, time and weather, for bots. Player positions and details are never
  part of the API. They are only shown to signed-in admins.

Build 42 only.

## How it works

The mod runs on the game server only. It writes one file, and PZAdmin writes
one:

| File, in the server's `Zomboid/Lua` folder | Written by | Contents |
|---|---|---|
| `pzadmin_companion.json` | the mod | the world and the online players |
| `pzadmin_companion_live.txt` | PZAdmin | a Unix time; live mode runs until then |

By default the mod writes its snapshot **once a minute**. When you press
**Start** on the Live tab, PZAdmin writes the live file with a time twenty
seconds ahead and keeps pushing it forward every eight seconds while the tab
is open, on screen and in the foreground. While that time is in the future
the mod writes **every two seconds**. Stop, leave the tab, hide the browser or
lose your connection, and the time is no longer pushed forward, so the mod
drops back to once a minute on its own within about twenty seconds. Nothing
depends on the browser remembering to say "stop".

For a server PZAdmin created, `Zomboid/Lua` is the `Lua` folder inside the
stack's config folder (`/srv/zomboid/<name>/projectzomboid/config/Lua`). For
a server added by folder, it is the `Lua` folder next to `Logs`. The Live tab
shows the exact path it is looking in.

Nothing is sent over the network. No ports are opened.

## Installing it

Every mod in a server's `Mods=` list must also be on each player's computer,
or the game refuses to let them join. Steam Workshop is how their game fetches
it automatically, so the mod goes on the Workshop once, then onto each server
like any other mod. It does nothing on players' computers.

1. **Upload it** (once). Copy `mod/PZAdminCompanion` from this repository to
   your `Zomboid/Workshop` folder on a PC with Project Zomboid Build 42, so
   you have `Zomboid/Workshop/PZAdminCompanion/Contents/mods/PZAdminCompanion/42/mod.info`.
   Add a 256×256 `preview.png` next to `workshop.txt`. In the game's main
   menu, open **Workshop**, pick PZAdminCompanion and upload it. It is set to
   unlisted in `workshop.txt`; players can still download it from a server
   that uses it.
2. **Add it to each server** on the server's **Mods** tab: paste the Workshop
   item's link and save. The mod ID is `PZAdminCompanion`.
3. **Restart the server.** After about a minute the Overview tab shows the
   in-game strip and the Live tab fills in.

Tell your players. The mod reports where everyone is, and people reasonably
want to know that.

## The map

PZAdmin does not ship a map image, and it never fetches one from the
internet. You upload your own from the Live tab (**Add a map image**). It is
shared by every server and kept in PZAdmin's data folder.

- **Which image:** any top-down map of the world as a JPEG or PNG, up to
  25 MB and 16,384 pixels a side. Around 4,000 to 8,000 pixels wide is a
  good balance; a full-size map at one pixel per tile is about 20,000 pixels
  wide and too heavy to pan smoothly. A screenshot or export from an online
  Build 42 map works.
- **Lining it up (once):** PZAdmin has to know which pixel is which tile.
  Press **Start**, stand somewhere you can recognise on the map, press
  **Line up map**, choose yourself (or type the in-game X and Y) and click
  that exact spot on the map. Then do the same at a second spot far away
  and diagonal from the first, for example the opposite corner of a big
  town. Two spots fix both directions, so any image works, cropped or
  scaled, including one with modded areas.
- **A new image** needs lining up again.
- If pins drift further from the truth the further they are from your two
  spots, the image is probably not a straight top-down map (some map
  exports are drawn at an angle). Use a top-down one.

## Troubleshooting

- **"The PZAdmin Companion mod is not reporting yet"**: the mod has not
  written its file where PZAdmin looks. Check the server's `Mods=` line has
  `PZAdminCompanion`, that the server has restarted since, and search the
  server's `console.txt` for `PZAdminCompanion`.
- **"The folder … does not exist yet"** when pressing Start: the game creates
  `Zomboid/Lua` itself the first time a mod writes there. PZAdmin does not
  create it, because a folder it made would belong to PZAdmin's user, which
  the game may not be able to write to.
- **"PZAdmin could not write …"**: PZAdmin's container needs write access to
  the server's config folder, as it already does to edit the server's ini.
- **Some columns show —**: the mod reports only what the game makes available
  to the server. A value the server cannot see is left out rather than guessed.
