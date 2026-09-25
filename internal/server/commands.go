package server

import (
	"fmt"
	"net"
	"regexp"
	"strconv"
	"strings"

	"github.com/TheWarBoys2/pzadmin/internal/rcon"
)

// ParamType tells the UI how to render an argument.
type ParamType string

const (
	ParamText     ParamType = "text"
	ParamPlayer   ParamType = "player"
	ParamNumber   ParamType = "number"
	ParamSelect   ParamType = "select"
	ParamBool     ParamType = "bool"
	ParamLongText ParamType = "longtext"
	// These three are rendered as a browsable picker rather than a text box,
	// filled from the server's own script files so the IDs are always the ones
	// that build actually has, mods included.
	ParamItem    ParamType = "item"
	ParamVehicle ParamType = "vehicle"
	ParamPerk    ParamType = "perk"
)

// Param describes one argument of a command.
type Param struct {
	Name        string    `json:"name"`
	Label       string    `json:"label"`
	Type        ParamType `json:"type"`
	Required    bool      `json:"required"`
	Options     []string  `json:"options,omitempty"`
	Placeholder string    `json:"placeholder,omitempty"`
	Default     string    `json:"default,omitempty"`
	Help        string    `json:"help,omitempty"`
	// Secret values are masked in the event log and in the form.
	Secret bool `json:"secret,omitempty"`
}

// Command is one thing an operator can run against a server.
type Command struct {
	ID      string  `json:"id"`
	Label   string  `json:"label"`
	Group   string  `json:"group"`
	Help    string  `json:"help"`
	Params  []Param `json:"params,omitempty"`
	Danger  bool    `json:"danger,omitempty"`
	Confirm string  `json:"confirm,omitempty"`
	// Audit is the severity recorded in the event log.
	Audit string `json:"-"`
	// Verbs are the command words this entry can be sent as, preferred
	// first. Build always writes the first; when a server's own help shows
	// only a later one, that is sent instead (see Capabilities). Empty means
	// the ID is the verb.
	Verbs []string `json:"verbs"`
	// Verify marks a command whose existence varies between builds. It is
	// only offered once the server's own help has confirmed it.
	Verify bool `json:"verify,omitempty"`
	// legacy entries are kept so saved schedules still run, but are not
	// offered in the UI any more.
	legacy bool

	build func(args []string) (string, error)
}

// verbs returns the command words, falling back to the ID.
func (c Command) verbs() []string {
	if len(c.Verbs) > 0 {
		return c.Verbs
	}
	return []string{c.ID}
}

// Build turns arguments into an RCON command string.
//
// Every argument is looked up through helpers that return an error rather than
// indexing the slice directly: the previous implementation panicked on a short
// argument list, which turned a mistyped form into a 500.
func (c Command) Build(args []string) (string, error) {
	if c.build == nil {
		return "", fmt.Errorf("command %q cannot be run directly", c.ID)
	}
	return c.build(args)
}

// AuditLine is the command as it is recorded in the event log: built the same
// way, with every secret argument masked.
func (c Command) AuditLine(args []string) string {
	masked := append([]string(nil), args...)
	hidden := false
	for i, p := range c.Params {
		if p.Secret && i < len(masked) && strings.TrimSpace(masked[i]) != "" {
			masked[i] = "********"
			hidden = true
		}
	}
	if !hidden {
		return ""
	}
	line, err := c.Build(masked)
	if err != nil {
		return c.ID + " (arguments hidden)"
	}
	return line
}

func arg(args []string, i int) string {
	if i < 0 || i >= len(args) {
		return ""
	}
	return strings.TrimSpace(args[i])
}

func required(args []string, i int, name string) (string, error) {
	v := arg(args, i)
	if v == "" {
		return "", fmt.Errorf("%s is required", name)
	}
	return v, nil
}

func requiredNumber(args []string, i int, name string) (string, error) {
	v, err := required(args, i, name)
	if err != nil {
		return "", err
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return "", fmt.Errorf("%s must be a whole number", name)
	}
	return strconv.Itoa(n), nil
}

func boolFlag(args []string, i int, name string) (string, error) {
	v := strings.ToLower(arg(args, i))
	switch v {
	case "true", "yes", "on", "1":
		return "true", nil
	case "false", "no", "off", "0":
		return "false", nil
	case "":
		return "", fmt.Errorf("%s is required", name)
	}
	return "", fmt.Errorf("%s must be true or false", name)
}

func q(s string) string { return rcon.Quote(s) }

var playerParam = Param{Name: "player", Label: "Player", Type: ParamPlayer, Required: true}

// commandList is the full catalogue. Grouping drives the UI layout.
var commandList = []Command{
	// --- world ---
	{ID: "save", Label: "Save the world", Group: "World", Audit: "info",
		Help:  "Flushes everything to disk. Always worth doing before a restart.",
		build: func([]string) (string, error) { return "save", nil }},
	{ID: "servermsg", Label: "Broadcast a message", Group: "World", Audit: "info",
		Help:   "Shows a message to everyone currently playing.",
		Params: []Param{{Name: "message", Label: "Message", Type: ParamLongText, Required: true, Placeholder: "Restarting in 10 minutes"}},
		build: func(a []string) (string, error) {
			m, err := required(a, 0, "message")
			if err != nil {
				return "", err
			}
			return "servermsg " + q(m), nil
		}},
	{ID: "quit", Label: "Save and quit (restarts)", Group: "World", Danger: true, Audit: "warn",
		Confirm: "This saves the world and exits the game. With restart: unless-stopped, Docker starts it again straight away. To keep it down, use Stop instead.",
		Help:    "Saves and exits the game process; the container's restart policy brings it back. This is how PZAdmin restarts servers.",
		build:   func([]string) (string, error) { return "quit", nil }},
	{ID: "reloadoptions", Label: "Reload server options", Group: "World", Audit: "info",
		Help:  "Applies changes made to the .ini without a restart. Not every setting can be reloaded live.",
		build: func([]string) (string, error) { return "reloadoptions", nil }},
	{ID: "showoptions", Label: "Show server options", Group: "World", Audit: "info",
		Help:  "Prints the server's current settings.",
		build: func([]string) (string, error) { return "showoptions", nil }},
	{ID: "changeoption", Label: "Change an option", Group: "World", Danger: true, Audit: "warn",
		Help: "Changes one server option at runtime.",
		Params: []Param{
			{Name: "option", Label: "Option", Type: ParamText, Required: true, Placeholder: "PVP"},
			{Name: "value", Label: "Value", Type: ParamText, Required: true, Placeholder: "false"},
		},
		build: func(a []string) (string, error) {
			opt, err := required(a, 0, "option")
			if err != nil {
				return "", err
			}
			val, err := required(a, 1, "value")
			if err != nil {
				return "", err
			}
			return "changeoption " + q(opt) + " " + q(val), nil
		}},
	{ID: "checkModsNeedUpdate", Label: "Check for mod updates", Group: "World", Audit: "info",
		Help:  "Asks Steam whether any Workshop mods have new versions. The answer appears in the server log a moment later.",
		build: func([]string) (string, error) { return "checkModsNeedUpdate", nil }},
	{ID: "reloadlua", Label: "Reload a Lua file", Group: "World", Audit: "warn",
		Params: []Param{{Name: "file", Label: "File name", Type: ParamText, Required: true, Placeholder: "ServerOptions.lua"}},
		build: func(a []string) (string, error) {
			f, err := required(a, 0, "file")
			if err != nil {
				return "", err
			}
			return "reloadlua " + q(f), nil
		}},

	// --- moderation ---
	{ID: "kickuser", Label: "Kick", Group: "Moderation", Danger: true, Audit: "warn",
		Help:   "Disconnects a player. They can rejoin immediately.",
		Params: []Param{playerParam, {Name: "reason", Label: "Reason", Type: ParamText, Placeholder: "Shown to the player"}},
		build: func(a []string) (string, error) {
			p, err := required(a, 0, "player")
			if err != nil {
				return "", err
			}
			cmd := "kickuser " + q(p)
			if r := arg(a, 1); r != "" {
				cmd += " -r " + q(r)
			}
			return cmd, nil
		}},
	{ID: "banuser", Label: "Ban", Group: "Moderation", Danger: true, Audit: "warn",
		Confirm: "This bans the account from the server until you unban it.",
		// The IP option comes last so schedules saved before it existed keep
		// their argument positions.
		Params: []Param{playerParam, {Name: "reason", Label: "Reason", Type: ParamText, Placeholder: "Shown to the player"},
			{Name: "ip", Label: "Also ban their IP address", Type: ParamBool, Default: "false",
				Help: "Stops a new account being made from the same connection. Shared connections share the ban."}},
		build: func(a []string) (string, error) {
			p, err := required(a, 0, "player")
			if err != nil {
				return "", err
			}
			cmd := "banuser " + q(p)
			if arg(a, 2) != "" {
				ip, err := boolFlag(a, 2, "also ban their IP address")
				if err != nil {
					return "", err
				}
				if ip == "true" {
					cmd += " -ip"
				}
			}
			if r := arg(a, 1); r != "" {
				cmd += " -r " + q(r)
			}
			return cmd, nil
		}},
	{ID: "unbanuser", Label: "Unban", Group: "Moderation", Audit: "warn",
		Params: []Param{{Name: "player", Label: "Player", Type: ParamPlayer, Required: true}},
		build: func(a []string) (string, error) {
			p, err := required(a, 0, "player")
			if err != nil {
				return "", err
			}
			return "unbanuser " + q(p), nil
		}},
	{ID: "banid", Label: "Ban a Steam ID", Group: "Moderation", Danger: true, Audit: "warn",
		Help:   "Bans by Steam ID, which survives a name change.",
		Params: []Param{{Name: "steamid", Label: "Steam ID", Type: ParamText, Required: true, Placeholder: "76561198000000000"}},
		build: func(a []string) (string, error) {
			id, err := required(a, 0, "Steam ID")
			if err != nil {
				return "", err
			}
			return "banid " + q(id), nil
		}},
	{ID: "unbanid", Label: "Unban a Steam ID", Group: "Moderation", Audit: "warn",
		Params: []Param{{Name: "steamid", Label: "Steam ID", Type: ParamText, Required: true}},
		build: func(a []string) (string, error) {
			id, err := required(a, 0, "Steam ID")
			if err != nil {
				return "", err
			}
			return "unbanid " + q(id), nil
		}},
	{ID: "banip", Label: "Ban an IP address", Group: "Moderation", Danger: true, Audit: "warn", Verify: true,
		Help:   "Bans every account connecting from one address. Ban with \"also ban their IP\" does this for a known player.",
		Params: []Param{{Name: "ip", Label: "IP address", Type: ParamText, Required: true, Placeholder: "203.0.113.7"}},
		build:  ipBuilder("banip")},
	{ID: "unbanip", Label: "Unban an IP address", Group: "Moderation", Audit: "warn", Verify: true,
		Params: []Param{{Name: "ip", Label: "IP address", Type: ParamText, Required: true}},
		build:  ipBuilder("unbanip")},
	{ID: "voiceban", Label: "Voice ban", Group: "Moderation", Audit: "warn",
		Params: []Param{playerParam, {Name: "state", Label: "Voice banned", Type: ParamBool, Required: true, Default: "true"}},
		build: func(a []string) (string, error) {
			p, err := required(a, 0, "player")
			if err != nil {
				return "", err
			}
			v, err := boolFlag(a, 1, "state")
			if err != nil {
				return "", err
			}
			return "voiceban " + q(p) + " -" + v, nil
		}},

	// --- permissions ---
	// setaccesslevel is the one way to change what a player may do. grantadmin
	// and removeadmin did the same thing twice over; they are kept in
	// legacyCommands only so saved schedules that use them still run.
	{ID: "setaccesslevel", Label: "Set access level", Group: "Permissions", Danger: true, Audit: "warn",
		Help: "Sets a player's role. Use admin to make someone an admin, and the normal player role to take it away.",
		Params: []Param{playerParam, {Name: "level", Label: "Level", Type: ParamText, Required: true,
			Options: defaultAccessLevels, Placeholder: "admin",
			Help: "Build 42 calls a normal player \"user\"; Build 41 used \"none\". Build 42 servers can also " +
				"define their own roles, so any role name is accepted. Once the server has been checked, " +
				"the levels its own help lists are suggested first."}},
		build: func(a []string) (string, error) {
			p, err := required(a, 0, "player")
			if err != nil {
				return "", err
			}
			l, err := required(a, 1, "level")
			if err != nil {
				return "", err
			}
			if !accessLevelPattern.MatchString(l) {
				return "", fmt.Errorf("a level is a single word of letters, digits, - or _")
			}
			return "setaccesslevel " + q(p) + " " + q(l), nil
		}},
	{ID: "addusertowhitelist", Label: "Add to whitelist", Group: "Permissions", Audit: "info", Verify: true,
		Help: "Adds a connected player's account to the whitelist. Not every build has this; " +
			"Create an account does the same job for someone who is not connected.",
		Params: []Param{{Name: "player", Label: "Player", Type: ParamPlayer, Required: true}},
		build: func(a []string) (string, error) {
			p, err := required(a, 0, "player")
			if err != nil {
				return "", err
			}
			return "addusertowhitelist " + q(p), nil
		}},
	{ID: "removeuserfromwhitelist", Label: "Remove from whitelist", Group: "Permissions", Danger: true, Audit: "warn",
		Params: []Param{{Name: "player", Label: "Player", Type: ParamPlayer, Required: true}},
		build: func(a []string) (string, error) {
			p, err := required(a, 0, "player")
			if err != nil {
				return "", err
			}
			return "removeuserfromwhitelist " + q(p), nil
		}},
	{ID: "adduser", Label: "Create an account", Group: "Permissions", Audit: "warn",
		Help: "Creates a server account with a password, for whitelisted servers.",
		Params: []Param{
			{Name: "username", Label: "Username", Type: ParamText, Required: true},
			{Name: "password", Label: "Password", Type: ParamText, Required: true, Secret: true},
		},
		build: func(a []string) (string, error) {
			u, err := required(a, 0, "username")
			if err != nil {
				return "", err
			}
			p, err := required(a, 1, "password")
			if err != nil {
				return "", err
			}
			return "adduser " + q(u) + " " + q(p), nil
		}},

	// --- player powers ---
	//
	// RCON has no player of its own, so the self-targeted forms (godmode -true,
	// invisible -true) do nothing from here. Build 42 moved the forms that
	// name a player to godmodeplayer and invisibleplayer; "godmode \"Bob\""
	// is the Build 41 spelling and is only sent when the server's help shows
	// that the *player command does not exist.
	{ID: "godmodeplayer", Label: "God mode", Group: "Player powers", Audit: "warn",
		Help:   "Toggles invulnerability for a player. Run it again to turn it off.",
		Verbs:  []string{"godmodeplayer", "godmodplayer", "godmode", "godmod"},
		Params: []Param{playerParam, explicitState},
		build:  toggleBuilder("godmodeplayer")},
	{ID: "invisibleplayer", Label: "Invisible", Group: "Player powers", Audit: "warn",
		Help:   "Toggles whether zombies can see a player. Run it again to turn it off.",
		Verbs:  []string{"invisibleplayer", "invisible"},
		Params: []Param{playerParam, explicitState},
		build:  toggleBuilder("invisibleplayer")},
	{ID: "noclip", Label: "Noclip", Group: "Player powers", Audit: "warn",
		Help:   "Toggles walking through walls. Run it again to turn it off.",
		Params: []Param{playerParam, explicitState},
		build:  toggleBuilder("noclip")},

	// --- items ---
	{ID: "additem", Label: "Give an item", Group: "Items", Audit: "warn",
		Help: "Item IDs look like Base.Axe or Farming.WateringCan.",
		Params: []Param{playerParam,
			{Name: "item", Label: "Item", Type: ParamItem, Required: true, Placeholder: "Base.Axe"},
			{Name: "count", Label: "How many", Type: ParamNumber, Default: "1"}},
		build: func(a []string) (string, error) {
			p, err := required(a, 0, "player")
			if err != nil {
				return "", err
			}
			item, err := required(a, 1, "item")
			if err != nil {
				return "", err
			}
			cmd := "additem " + q(p) + " " + q(item)
			if c := arg(a, 2); c != "" {
				n, err := strconv.Atoi(c)
				if err != nil || n < 1 {
					return "", fmt.Errorf("how many must be a positive whole number")
				}
				cmd += " " + strconv.Itoa(n)
			}
			return cmd, nil
		}},
	{ID: "addvehicle", Label: "Spawn a vehicle", Group: "Items", Audit: "warn",
		Params: []Param{
			{Name: "script", Label: "Vehicle", Type: ParamVehicle, Required: true, Placeholder: "Base.CarNormal"},
			playerParam},
		build: func(a []string) (string, error) {
			v, err := required(a, 0, "vehicle")
			if err != nil {
				return "", err
			}
			p, err := required(a, 1, "player")
			if err != nil {
				return "", err
			}
			return "addvehicle " + q(v) + " " + q(p), nil
		}},
	{ID: "addxp", Label: "Give XP", Group: "Items", Audit: "warn",
		Help: "Use the internal skill name, such as Woodwork for Carpentry. The player must be online.",
		Params: []Param{playerParam,
			{Name: "perk", Label: "Skill", Type: ParamPerk, Required: true, Placeholder: "Woodwork"},
			{Name: "amount", Label: "XP", Type: ParamNumber, Required: true, Default: "100",
				Help: "Raw experience points, not levels. A skill level is worth thousands."}},
		build: func(a []string) (string, error) {
			p, err := required(a, 0, "player")
			if err != nil {
				return "", err
			}
			perk, err := required(a, 1, "skill")
			if err != nil {
				return "", err
			}
			xp, err := requiredNumber(a, 2, "XP")
			if err != nil {
				return "", err
			}
			// The skill and the amount are one argument joined by "=", and the
			// skill must not be quoted. Passing them as three separate quoted
			// arguments is silently accepted and does nothing.
			if strings.ContainsAny(perk, " \t\"'=") {
				return "", fmt.Errorf("a skill name cannot contain spaces, quotes or =")
			}
			return "addxp " + q(p) + " " + perk + "=" + xp, nil
		}},

	// --- movement ---
	//
	// Only player-to-player. "teleport \"Bob\"" and "teleportto x,y,z" move
	// whoever runs them, and over RCON that is nobody.
	{ID: "teleportplayer", Label: "Teleport a player to another", Group: "Movement", Audit: "warn",
		Verbs:  []string{"teleportplayer", "teleport"},
		Params: []Param{playerParam, {Name: "target", Label: "Destination player", Type: ParamPlayer, Required: true}},
		build: func(a []string) (string, error) {
			p, err := required(a, 0, "player")
			if err != nil {
				return "", err
			}
			target, err := required(a, 1, "destination player")
			if err != nil {
				return "", err
			}
			return "teleportplayer " + q(p) + " " + q(target), nil
		}},

	// --- events ---
	{ID: "startrain", Label: "Start rain", Group: "Weather", Audit: "info",
		Params: []Param{{Name: "intensity", Label: "Intensity 1-100", Type: ParamNumber, Placeholder: "50"}},
		build:  optionalNumberBuilder("startrain", "intensity")},
	{ID: "stoprain", Label: "Stop rain", Group: "Weather", Audit: "info",
		build: func([]string) (string, error) { return "stoprain", nil }},
	{ID: "startstorm", Label: "Start a storm", Group: "Weather", Audit: "info",
		Params: []Param{{Name: "duration", Label: "Duration in game hours", Type: ParamNumber, Placeholder: "2"}},
		build:  optionalNumberBuilder("startstorm", "duration")},
	{ID: "stopweather", Label: "Clear the weather", Group: "Weather", Audit: "info",
		build: func([]string) (string, error) { return "stopweather", nil }},
	{ID: "thunder", Label: "Thunder", Group: "Weather", Audit: "info",
		Params: []Param{{Name: "player", Label: "Near player", Type: ParamPlayer}},
		build:  optionalPlayerBuilder("thunder")},
	{ID: "lightning", Label: "Lightning", Group: "Weather", Audit: "info",
		Params: []Param{{Name: "player", Label: "Near player", Type: ParamPlayer}},
		build:  optionalPlayerBuilder("lightning")},
	{ID: "chopper", Label: "Helicopter event", Group: "Events", Audit: "info",
		Help:  "Draws zombies across the map. Players will notice.",
		build: func([]string) (string, error) { return "chopper", nil }},
	{ID: "gunshot", Label: "Gunshot", Group: "Events", Audit: "info",
		build: func([]string) (string, error) { return "gunshot", nil }},
	{ID: "createhorde", Label: "Spawn a horde near a player", Group: "Zombies", Danger: true, Audit: "warn",
		Confirm: "This spawns zombies next to a player. It cannot be undone.",
		Params: []Param{
			{Name: "count", Label: "How many zombies", Type: ParamNumber, Required: true, Default: "20"},
			{Name: "player", Label: "Near player", Type: ParamPlayer}},
		build: func(a []string) (string, error) {
			n, err := requiredNumber(a, 0, "how many zombies")
			if err != nil {
				return "", err
			}
			cmd := "createhorde " + n
			if p := arg(a, 1); p != "" {
				cmd += " " + q(p)
			}
			return cmd, nil
		}},
	// createhorde2 takes named options, not positions: "createhorde2 10 20 5 30"
	// is answered with its usage text and spawns nothing. It is less settled
	// than createhorde, so it is only offered once the server confirms it.
	{ID: "createhorde2", Label: "Spawn a horde at coordinates", Group: "Zombies", Danger: true, Audit: "warn", Verify: true,
		Confirm: "This spawns zombies at a map location. It cannot be undone.",
		Help:    "Use the in-game debug coordinates. Radius spreads the horde out.",
		Params: []Param{
			{Name: "x", Label: "X", Type: ParamNumber, Required: true},
			{Name: "y", Label: "Y", Type: ParamNumber, Required: true},
			{Name: "radius", Label: "Radius", Type: ParamNumber, Required: true, Default: "10"},
			{Name: "count", Label: "How many zombies", Type: ParamNumber, Required: true, Default: "20"},
			{Name: "z", Label: "Floor (Z)", Type: ParamNumber, Default: "0"}},
		build: func(a []string) (string, error) {
			x, err := requiredNumber(a, 0, "X")
			if err != nil {
				return "", err
			}
			y, err := requiredNumber(a, 1, "Y")
			if err != nil {
				return "", err
			}
			radius, err := requiredNumber(a, 2, "radius")
			if err != nil {
				return "", err
			}
			count, err := requiredNumber(a, 3, "how many zombies")
			if err != nil {
				return "", err
			}
			z := "0"
			if arg(a, 4) != "" {
				if z, err = requiredNumber(a, 4, "floor"); err != nil {
					return "", err
				}
			}
			return "createhorde2 -x " + x + " -y " + y + " -z " + z + " -count " + count + " -radius " + radius, nil
		}},
	// releasesafehouse names the safehouse. Without a title the command
	// releases the caller's own, and over RCON there is no caller.
	{ID: "releasesafehouse", Label: "Release a safehouse", Group: "Events", Danger: true, Audit: "warn",
		Confirm: "The safehouse stops being protected. Anyone can then enter it and take what is inside.",
		Params: []Param{{Name: "title", Label: "Safehouse title", Type: ParamText, Required: true,
			Help: "Exactly as it appears in the admin panel's safehouse list."}},
		build: func(a []string) (string, error) {
			title, err := required(a, 0, "safehouse title")
			if err != nil {
				return "", err
			}
			return "releasesafehouse " + q(title), nil
		}},

	// --- diagnostics ---
	{ID: "players", Label: "List players", Group: "Diagnostics", Audit: "info",
		build: func([]string) (string, error) { return "players", nil }},
	{ID: "stats", Label: "Statistics logging", Group: "Diagnostics", Audit: "info",
		Params: []Param{{Name: "mode", Label: "Mode", Type: ParamSelect, Required: true,
			Options: []string{"none", "file", "console", "all"}, Default: "none"},
			{Name: "period", Label: "Period in seconds", Type: ParamNumber, Placeholder: "10"}},
		build: func(a []string) (string, error) {
			mode, err := required(a, 0, "mode")
			if err != nil {
				return "", err
			}
			cmd := "stats " + mode
			if p := arg(a, 1); p != "" {
				n, err := strconv.Atoi(p)
				if err != nil {
					return "", fmt.Errorf("period must be a whole number of seconds")
				}
				cmd += " " + strconv.Itoa(n)
			}
			return cmd, nil
		}},
	{ID: "log", Label: "Set log level", Group: "Diagnostics", Audit: "info",
		Params: []Param{
			{Name: "type", Label: "Log type", Type: ParamText, Required: true},
			{Name: "level", Label: "Level", Type: ParamText, Required: true}},
		build: func(a []string) (string, error) {
			ty, err := required(a, 0, "log type")
			if err != nil {
				return "", err
			}
			lv, err := required(a, 1, "level")
			if err != nil {
				return "", err
			}
			return "log " + q(ty) + " " + q(lv), nil
		}},
}

// legacyCommands are no longer offered but still resolve, so a schedule saved
// by an earlier version runs instead of failing with "unknown command".
var legacyCommands = []Command{
	{ID: "grantadmin", Label: "Grant admin", Group: "Permissions", Danger: true, Audit: "warn", legacy: true,
		Params: []Param{playerParam},
		build: func(a []string) (string, error) {
			p, err := required(a, 0, "player")
			if err != nil {
				return "", err
			}
			return "grantadmin " + q(p), nil
		}},
	{ID: "removeadmin", Label: "Remove admin", Group: "Permissions", Audit: "warn", legacy: true,
		Params: []Param{playerParam},
		build: func(a []string) (string, error) {
			p, err := required(a, 0, "player")
			if err != nil {
				return "", err
			}
			return "removeadmin " + q(p), nil
		}},
	{ID: "sendpulse", Label: "Performance pulse", Group: "Diagnostics", Audit: "info", legacy: true,
		build: func([]string) (string, error) { return "sendpulse", nil }},
}

// renamedCommands maps IDs earlier versions stored to their replacements.
// The arguments are the same shape, so a saved schedule carries straight over.
var renamedCommands = map[string]string{
	"godmode":   "godmodeplayer",
	"invisible": "invisibleplayer",
	"teleport":  "teleportplayer",
}

// defaultAccessLevels are suggestions for setaccesslevel, not a restriction:
// Build 42 roles are defined per server. Build 42's normal player is "user",
// Build 41's was "none"; both are listed so neither build is left without one.
var defaultAccessLevels = []string{"admin", "moderator", "overseer", "gm", "observer", "user", "none"}

var accessLevelPattern = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_-]{0,31}$`)

func ipBuilder(name string) func([]string) (string, error) {
	return func(a []string) (string, error) {
		v, err := required(a, 0, "IP address")
		if err != nil {
			return "", err
		}
		ip := net.ParseIP(v)
		if ip == nil {
			return "", fmt.Errorf("%q is not an IP address", v)
		}
		return name + " " + q(ip.String()), nil
	}
}

// explicitState is the optional on/off argument for the toggle commands.
//
// godmode, invisible and noclip are toggles: the documented form is the command
// and a player name, and running it again turns the effect off. Appending
// "-true" unconditionally, as PZAdmin used to, gave the server an argument it
// does not expect and the command silently did nothing. The explicit form is
// kept as an option because some builds accept it, but it is off by default.
var explicitState = Param{
	Name: "state", Label: "Force a state", Type: ParamSelect,
	Options: []string{"", "true", "false"}, Default: "",
	Help: "Leave blank to toggle, which is what the command normally does. " +
		"Choosing true or false appends -true or -false, which not every build accepts.",
}

func toggleBuilder(name string) func([]string) (string, error) {
	return func(a []string) (string, error) {
		p, err := required(a, 0, "player")
		if err != nil {
			return "", err
		}
		switch strings.ToLower(arg(a, 1)) {
		case "":
			return name + " " + q(p), nil
		case "true", "false":
			return name + " " + q(p) + " -" + strings.ToLower(arg(a, 1)), nil
		}
		return "", fmt.Errorf("force a state must be left blank, or set to true or false")
	}
}

func optionalNumberBuilder(name, label string) func([]string) (string, error) {
	return func(a []string) (string, error) {
		v := arg(a, 0)
		if v == "" {
			return name, nil
		}
		n, err := strconv.Atoi(v)
		if err != nil {
			return "", fmt.Errorf("%s must be a whole number", label)
		}
		return name + " " + strconv.Itoa(n), nil
	}
}

func optionalPlayerBuilder(name string) func([]string) (string, error) {
	return func(a []string) (string, error) {
		p := arg(a, 0)
		if p == "" {
			return name, nil
		}
		return name + " " + q(p), nil
	}
}

var commandIndex = func() map[string]Command {
	m := make(map[string]Command, len(commandList)+len(legacyCommands))
	for _, c := range append(append([]Command{}, commandList...), legacyCommands...) {
		m[c.ID] = c
	}
	return m
}()

// LookupCommand returns a command by ID, including legacy and renamed IDs.
func LookupCommand(id string) (Command, bool) {
	if to, renamed := renamedCommands[id]; renamed {
		id = to
	}
	c, ok := commandIndex[id]
	return c, ok
}

// Commands returns the catalogue the UI offers. Legacy entries are left out.
func Commands() []Command { return commandList }

// withVerb rewrites the command word at the start of a built line. Arguments
// are left exactly as built.
func withVerb(line, from, to string) string {
	if from == to || !strings.HasPrefix(line, from) {
		return line
	}
	rest := line[len(from):]
	if rest != "" && rest[0] != ' ' {
		return line
	}
	return to + rest
}

// ValidateRawCommand performs the minimal safety check on a free-text console
// command: it must be a single line. Everything else is allowed, because the
// console is for operators who already have full control of the server, and
// every use is written to the audit log.
func ValidateRawCommand(cmd string) (string, error) {
	cmd = strings.TrimSpace(cmd)
	if cmd == "" {
		return "", fmt.Errorf("enter a command")
	}
	if strings.ContainsAny(cmd, "\r\n") {
		return "", fmt.Errorf("run one command at a time")
	}
	if len(cmd) > 4000 {
		return "", fmt.Errorf("command is too long")
	}
	return cmd, nil
}
