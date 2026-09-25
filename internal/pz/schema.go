package pz

import (
	"os"
	"sort"
	"strconv"
	"strings"
)

// Applies says what has to happen before a setting takes effect.
type Applies string

const (
	// AppliesOnReload means the running server picks the change up when it is
	// told to reload its options.
	AppliesOnReload Applies = "reload"
	// AppliesOnRestart means the value is read once at startup.
	AppliesOnRestart Applies = "restart"
	// AppliesOnNewWorld means existing saves keep the old value.
	AppliesOnNewWorld Applies = "world"
)

// FieldType tells the interface how to render a setting.
type FieldType string

const (
	FieldBool   FieldType = "bool"
	FieldInt    FieldType = "int"
	FieldFloat  FieldType = "float"
	FieldText   FieldType = "text"
	FieldList   FieldType = "list"
	FieldChoice FieldType = "choice"
)

// Field is one editable setting.
type Field struct {
	Key     string    `json:"key"`
	Value   string    `json:"value"`
	Type    FieldType `json:"type"`
	Group   string    `json:"group"`
	Help    string    `json:"help,omitempty"`
	Applies Applies   `json:"applies"`
	Min     *int      `json:"min,omitempty"`
	Max     *int      `json:"max,omitempty"`
	Choices []string  `json:"choices,omitempty"`
	// Options label the choices, when the game's own comment names them.
	Options  []Option `json:"options,omitempty"`
	Secret   bool     `json:"secret,omitempty"`
	Multi    bool     `json:"multiline,omitempty"`
	Line     int      `json:"line"`
	Advanced bool     `json:"advanced,omitempty"`
	// Locked explains why a setting cannot be edited here, when it is owned
	// by something else (the image rewrites it from .env on every boot).
	Locked string `json:"locked,omitempty"`
}

// meta is the curated description of a setting we are confident about.
//
// Everything not listed here is still shown and edited: its type is inferred
// from the value on disk, so a build that adds new options keeps working
// without a code change. Only the description, grouping and the restart rule
// come from this table, and nothing is invented for options not in it.
type meta struct {
	group   string
	help    string
	applies Applies
	typ     FieldType
	min     *int
	max     *int
	choices []string
	secret  bool
	multi   bool
}

func ptr(v int) *int { return &v }

var iniMeta = map[string]meta{
	// --- identity and access ---
	"PublicName":                 {group: "Identity", help: "The name shown in the server browser.", applies: AppliesOnReload},
	"PublicDescription":          {group: "Identity", help: "The description shown in the server browser.", applies: AppliesOnReload, multi: true},
	"Public":                     {group: "Identity", help: "List this server publicly in the in-game browser.", applies: AppliesOnRestart, typ: FieldBool},
	"Password":                   {group: "Access", help: "Password players must enter to join. Leave blank for none.", applies: AppliesOnReload, secret: true},
	"Open":                       {group: "Access", help: "Allow anyone to join. Turn off to run a whitelist.", applies: AppliesOnReload, typ: FieldBool},
	"MaxPlayers":                 {group: "Access", help: "Maximum simultaneous players.", applies: AppliesOnReload, typ: FieldInt, min: ptr(1), max: ptr(100)},
	"AutoCreateUserInWhiteList":  {group: "Access", help: "Add players to the whitelist automatically the first time they join.", applies: AppliesOnReload, typ: FieldBool},
	"DropOffWhiteListAfterDeath": {group: "Access", help: "Remove a player from the whitelist when they die.", applies: AppliesOnReload, typ: FieldBool},

	// --- networking: read once at startup ---
	"DefaultPort":              {group: "Network", help: "UDP game port. Players connect to this.", applies: AppliesOnRestart, typ: FieldInt, min: ptr(1), max: ptr(65535)},
	"UDPPort":                  {group: "Network", help: "Companion UDP port, normally the game port plus one.", applies: AppliesOnRestart, typ: FieldInt, min: ptr(1), max: ptr(65535)},
	"RCONPort":                 {group: "Network", help: "Port PZAdmin connects to. Changing this needs a restart, and you must update PZAdmin's server settings to match.", applies: AppliesOnRestart, typ: FieldInt, min: ptr(1), max: ptr(65535)},
	"RCONPassword":             {group: "Network", help: "Password PZAdmin uses. Changing this needs a restart, and you must update PZAdmin's server settings to match.", applies: AppliesOnRestart, secret: true},
	"ServerBrowserAnnouncedIP": {group: "Network", help: "Address advertised to the server browser. Leave blank to detect automatically.", applies: AppliesOnRestart},

	// --- mods and world: startup only ---
	"Mods":          {group: "Mods", help: "Mod IDs, separated by semicolons. Order matters. Changing this always needs a restart.", applies: AppliesOnRestart, typ: FieldList},
	"WorkshopItems": {group: "Mods", help: "Steam Workshop item IDs, separated by semicolons.", applies: AppliesOnRestart, typ: FieldList},
	"Map":           {group: "World", help: "Map folders in load order. Changing this on an existing world will break it.", applies: AppliesOnNewWorld, typ: FieldList},

	// --- gameplay ---
	"PVP":                 {group: "Gameplay", help: "Allow players to damage each other.", applies: AppliesOnReload, typ: FieldBool},
	"PauseEmpty":          {group: "Gameplay", help: "Freeze the world while nobody is connected.", applies: AppliesOnReload, typ: FieldBool},
	"GlobalChat":          {group: "Gameplay", help: "Allow chat that everyone on the server can see.", applies: AppliesOnReload, typ: FieldBool},
	"SafetySystem":        {group: "Gameplay", help: "Require players to toggle PVP mode before they can hurt each other.", applies: AppliesOnReload, typ: FieldBool},
	"ShowSafety":          {group: "Gameplay", help: "Show an indicator above players with PVP enabled.", applies: AppliesOnReload, typ: FieldBool},
	"SafetyToggleTimer":   {group: "Gameplay", help: "Seconds to switch PVP mode on or off.", applies: AppliesOnReload, typ: FieldInt},
	"SafetyCooldownTimer": {group: "Gameplay", help: "Seconds before PVP mode can be toggled again.", applies: AppliesOnReload, typ: FieldInt},
	"DisplayUserName":     {group: "Gameplay", help: "Show player names above their characters.", applies: AppliesOnReload, typ: FieldBool},
	"AllowCoop":           {group: "Gameplay", help: "Allow splitscreen players.", applies: AppliesOnReload, typ: FieldBool},
	"Voice3D":             {group: "Gameplay", help: "Positional voice chat.", applies: AppliesOnReload, typ: FieldBool},

	// --- saving and housekeeping ---
	"SaveWorldEveryMinutes":  {group: "Saving", help: "How often the world is written to disk.", applies: AppliesOnReload, typ: FieldInt, min: ptr(0), max: ptr(1440)},
	"BackupsCount":           {group: "Saving", help: "How many of the game's own backups to keep. PZAdmin's backups are separate.", applies: AppliesOnRestart, typ: FieldInt},
	"BackupsOnStart":         {group: "Saving", help: "Take one of the game's backups when the server starts.", applies: AppliesOnRestart, typ: FieldBool},
	"BackupsOnVersionChange": {group: "Saving", help: "Take one of the game's backups when the game version changes.", applies: AppliesOnRestart, typ: FieldBool},

	// --- safehouses and factions ---
	"PlayerSafehouse":             {group: "Safehouses", help: "Let players claim safehouses.", applies: AppliesOnReload, typ: FieldBool},
	"AdminSafehouse":              {group: "Safehouses", help: "Restrict safehouse claims to admins.", applies: AppliesOnReload, typ: FieldBool},
	"SafehouseAllowTrepass":       {group: "Safehouses", help: "Allow non-members to enter a safehouse.", applies: AppliesOnReload, typ: FieldBool},
	"SafehouseAllowFire":          {group: "Safehouses", help: "Allow fire inside a safehouse.", applies: AppliesOnReload, typ: FieldBool},
	"SafehouseAllowLoot":          {group: "Safehouses", help: "Allow non-members to loot a safehouse.", applies: AppliesOnReload, typ: FieldBool},
	"SafehouseDaySurvivedToClaim": {group: "Safehouses", help: "Days a player must survive before claiming.", applies: AppliesOnReload, typ: FieldInt},
	"Faction":                     {group: "Safehouses", help: "Allow players to form factions.", applies: AppliesOnReload, typ: FieldBool},
	"FactionDaySurvivedToCreate":  {group: "Safehouses", help: "Days a player must survive before creating a faction.", applies: AppliesOnReload, typ: FieldInt},

	// --- anti-cheat and logging ---
	"AntiCheatProtectionType1": {group: "Anti-cheat", help: "Anti-cheat check. Turn individual checks off only if a mod is triggering false positives.", applies: AppliesOnRestart, typ: FieldBool},
	"DoLuaChecksum":            {group: "Anti-cheat", help: "Kick clients whose Lua files do not match the server's.", applies: AppliesOnRestart, typ: FieldBool},
	"KickFastPlayers":          {group: "Anti-cheat", help: "Kick players moving impossibly fast. Known to misfire; usually left off.", applies: AppliesOnReload, typ: FieldBool},
	"ClientCommandFilter":      {group: "Anti-cheat", help: "Commands clients are not allowed to send.", applies: AppliesOnRestart, multi: true},
	"PerkLogs":                 {group: "Logging", help: "Record skill changes to the perk log.", applies: AppliesOnReload, typ: FieldBool},
}

// aliasGroups assigns a group to options we have no specific note for, by
// prefix, so the table is still organised rather than one long list.
var groupPrefixes = []struct {
	prefix string
	group  string
}{
	{"Safehouse", "Safehouses"},
	{"Faction", "Safehouses"},
	{"AntiCheat", "Anti-cheat"},
	{"Backups", "Saving"},
	{"Save", "Saving"},
	{"Steam", "Network"},
	{"RCON", "Network"},
	{"Server", "Network"},
	{"UDP", "Network"},
	{"Voice", "Gameplay"},
	{"Chat", "Gameplay"},
	{"Map", "World"},
	{"Mod", "Mods"},
	{"Workshop", "Mods"},
	{"Log", "Logging"},
	{"Sleep", "Gameplay"},
	{"Player", "Gameplay"},
}

func groupFor(key string) string {
	for _, g := range groupPrefixes {
		if strings.HasPrefix(key, g.prefix) {
			return g.group
		}
	}
	return "Other"
}

// inferType works out how to render a value we have no note for.
func inferType(value string) FieldType {
	v := strings.TrimSpace(value)
	switch strings.ToLower(v) {
	case "true", "false":
		return FieldBool
	}
	if v == "" {
		return FieldText
	}
	if _, err := strconv.Atoi(v); err == nil {
		return FieldInt
	}
	if _, err := strconv.ParseFloat(v, 64); err == nil {
		return FieldFloat
	}
	if strings.Contains(v, ";") {
		return FieldList
	}
	return FieldText
}

// Fields turns a parsed .ini into editable fields, grouped and typed.
func (i *INI) Fields() []Field {
	settings := i.Settings()
	out := make([]Field, 0, len(settings))
	for _, s := range settings {
		f := Field{
			Key: s.Key, Value: s.Value, Line: s.Line,
			Applies: AppliesOnReload,
		}
		if m, known := iniMeta[s.Key]; known {
			f.Group = m.group
			f.Help = m.help
			f.Applies = m.applies
			f.Min, f.Max, f.Choices = m.min, m.max, m.choices
			f.Secret = m.secret
			f.Multi = m.multi
			f.Type = m.typ
		} else {
			// No curated note: still editable, just without a description.
			f.Group = groupFor(s.Key)
			f.Advanced = true
		}
		if f.Type == "" {
			f.Type = inferType(s.Value)
		}
		applyDoc(&f, parseDoc(i.commentAbove(s.Line-1)))
		out = append(out, f)
	}
	sort.SliceStable(out, func(a, b int) bool {
		if out[a].Group != out[b].Group {
			return groupRank(out[a].Group) < groupRank(out[b].Group)
		}
		if out[a].Advanced != out[b].Advanced {
			return !out[a].Advanced
		}
		return out[a].Key < out[b].Key
	})
	return out
}

var groupOrder = []string{
	"Identity", "Access", "Gameplay", "Mods", "World", "Saving",
	"Safehouses", "Network", "Anti-cheat", "Logging", "Other",
}

func groupRank(name string) int {
	for i, g := range groupOrder {
		if g == name {
			return i
		}
	}
	return len(groupOrder)
}

// ValidateField checks a proposed value against the field's type and range,
// returning the normalised value to write.
func ValidateField(f Field, value string) (string, error) {
	value = strings.TrimSpace(value)
	switch f.Type {
	case FieldBool:
		switch strings.ToLower(value) {
		case "true", "false":
			return strings.ToLower(value), nil
		}
		return "", &FieldError{f.Key, "must be true or false"}
	case FieldInt:
		n, err := strconv.Atoi(value)
		if err != nil {
			return "", &FieldError{f.Key, "must be a whole number"}
		}
		if f.Min != nil && n < *f.Min {
			return "", &FieldError{f.Key, "must be at least " + strconv.Itoa(*f.Min)}
		}
		if f.Max != nil && n > *f.Max {
			return "", &FieldError{f.Key, "must be at most " + strconv.Itoa(*f.Max)}
		}
		return strconv.Itoa(n), nil
	case FieldFloat:
		v, err := strconv.ParseFloat(value, 64)
		if err != nil {
			return "", &FieldError{f.Key, "must be a number"}
		}
		return strconv.FormatFloat(v, 'g', -1, 64), nil
	case FieldChoice:
		for _, c := range f.Choices {
			if strings.EqualFold(c, value) {
				return c, nil
			}
		}
		return "", &FieldError{f.Key, "must be one of " + strings.Join(f.Choices, ", ")}
	}
	if strings.ContainsAny(value, "\r\n") {
		return "", &FieldError{f.Key, "must not contain line breaks"}
	}
	return value, nil
}

// FieldError describes an invalid setting value.
type FieldError struct {
	Key    string
	Reason string
}

func (e *FieldError) Error() string { return e.Key + " " + e.Reason }

// readFile is a small indirection used by tests.
func readFile(path string) ([]byte, error) { return os.ReadFile(path) }
