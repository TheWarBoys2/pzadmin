These are the files a clean Build 42 dedicated server writes on its first boot,
with no Server files and no mods, captured from a throwaway stack. PZAdmin
starts new servers from them. Keep them verbatim: per-server values (ResetID,
ServerPlayerID, Seed, ports, RCON password) are removed or replaced in code, so
refreshing them after a game update is a straight copy of the four files.
