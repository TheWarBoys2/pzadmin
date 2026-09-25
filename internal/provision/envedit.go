package provision

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/TheWarBoys2/pzadmin/internal/compose"
)

// INIOwnedByEnv maps the ini keys the image rewrites from its environment on
// every boot (scripts/start.sh) to the .env variable that sets them. Editing
// one of these in the ini does nothing lasting: the next boot puts the .env
// value back. MaxPlayers is only rewritten when MAX_PLAYERS is set.
var INIOwnedByEnv = map[string]string{
	"DefaultPort":  "DEFAULT_PORT",
	"UDPPort":      "UDP_PORT",
	"RCONPort":     "RCON_PORT",
	"RCONPassword": "RCON_PASSWORD",
	"MaxPlayers":   "MAX_PLAYERS",
}

// LockedINIKeys returns the ini keys owned by .env for a given environment,
// keyed by ini key, each with the explanation shown in the editor.
func LockedINIKeys(env map[string]string) map[string]string {
	out := map[string]string{}
	for key, envKey := range INIOwnedByEnv {
		if envKey == "MAX_PLAYERS" && strings.TrimSpace(env[envKey]) == "" {
			continue
		}
		out[key] = "Set by " + envKey + " in the stack's .env, which the image writes into the ini on every boot. " +
			"Change it there and recreate the container."
	}
	return out
}

// EnvField is one editable .env variable.
type EnvField struct {
	Key    string `json:"key"`
	Value  string `json:"value"`
	Secret bool   `json:"secret,omitempty"`
	Help   string `json:"help"`
}

type envRule struct {
	help     string
	secret   bool
	validate func(string) error
}

var (
	secretRe = regexp.MustCompile(`^[A-Za-z0-9._@%+=-]{8,64}$`)
	branchRe = regexp.MustCompile(`^[A-Za-z0-9._-]{0,40}$`)
)

func intRange(lo, hi int, allowEmpty bool) func(string) error {
	return func(v string) error {
		if v == "" && allowEmpty {
			return nil
		}
		n, err := strconv.Atoi(v)
		if err != nil || n < lo || n > hi {
			return fmt.Errorf("must be a whole number from %d to %d", lo, hi)
		}
		return nil
	}
}

func boolValue(v string) error {
	if v != "true" && v != "false" {
		return errors.New("must be true or false")
	}
	return nil
}

// editableEnv is deliberately short. Ports and SERVER_NAME are left out:
// ports also live in the compose file's port mappings, and SERVER_NAME names
// the ini and lua files, so changing either in .env alone breaks the server.
// PUID and PGID must match PZAdmin's own user. ADMIN_USERNAME only matters on
// the first boot.
var editableEnv = map[string]envRule{
	"RCON_PASSWORD": {help: "RCON password. Also written into the ini on boot.", secret: true,
		validate: func(v string) error {
			if !secretRe.MatchString(v) {
				return errors.New("must be 8 to 64 characters of letters, digits and . _ @ % + = -")
			}
			return nil
		}},
	"ADMIN_PASSWORD": {help: "Used to create the in-game admin account on the first boot. Once the account " +
		"exists, change its password in game; changing it here does not.", secret: true,
		validate: ValidAdminPassword},
	"MAX_PLAYERS":     {help: "Also written into the ini on boot.", validate: intRange(1, 100, true)},
	"MEMORY_XMX_GB":   {help: "Java heap maximum, in GB.", validate: intRange(1, 64, false)},
	"MEMORY_XMS_GB":   {help: "Java heap starting size, in GB. Blank lets Java decide.", validate: intRange(1, 64, true)},
	"UPDATE_ON_START": {help: "Run a Steam update on every container start, including every restart.", validate: boolValue},
	"SERVER_BRANCH": {help: "Steam beta branch. Blank for the default branch.",
		validate: func(v string) error {
			if !branchRe.MatchString(v) {
				return errors.New("must be letters, digits, . _ or -")
			}
			return nil
		}},
	"STEAM_VAC": {help: "Valve Anti-Cheat.", validate: boolValue},
	"VM_ARGS": {help: "Extra JVM arguments.",
		validate: func(v string) error {
			if len(v) > 500 || strings.ContainsAny(v, "\"'`$\\") {
				return errors.New("must be under 500 characters, without quotes, $ or backslashes")
			}
			return nil
		}},
}

// EnvFields returns the editable variables in a stack's .env. Secret values
// are never returned.
func EnvFields(stackDir string) ([]EnvField, error) {
	env, err := compose.ReadEnvFile(filepath.Join(stackDir, ".env"))
	if err != nil {
		return nil, err
	}
	keys := make([]string, 0, len(editableEnv))
	for k := range editableEnv {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]EnvField, 0, len(keys))
	for _, k := range keys {
		rule := editableEnv[k]
		v := env.Lookup(k)
		if rule.secret {
			v = ""
		}
		out = append(out, EnvField{Key: k, Value: v, Secret: rule.secret, Help: rule.help})
	}
	return out, nil
}

// EditEnv validates and applies changes to a stack's .env. A blank secret
// means "leave it". It returns the previous file content, for a backup, and
// the keys that actually changed.
func EditEnv(stackDir string, changes map[string]string) (previous []byte, changed []string, err error) {
	path := filepath.Join(stackDir, ".env")
	previous, err = os.ReadFile(path)
	if err != nil {
		return nil, nil, err
	}
	env := compose.ParseEnv(previous)

	// Validate everything before writing anything.
	final := map[string]string{}
	for k, v := range changes {
		rule, ok := editableEnv[k]
		if !ok {
			return nil, nil, fmt.Errorf("%s cannot be changed from PZAdmin", k)
		}
		v = strings.TrimSpace(v)
		if strings.ContainsAny(v, "\r\n") {
			return nil, nil, fmt.Errorf("%s must be one line", k)
		}
		if rule.secret && v == "" {
			continue
		}
		if err := rule.validate(v); err != nil {
			return nil, nil, fmt.Errorf("%s %v", k, err)
		}
		if old, _ := env.Get(k); old == v {
			continue
		}
		final[k] = v
	}
	xmx, xms := env.Lookup("MEMORY_XMX_GB"), env.Lookup("MEMORY_XMS_GB")
	if v, ok := final["MEMORY_XMX_GB"]; ok {
		xmx = v
	}
	if v, ok := final["MEMORY_XMS_GB"]; ok {
		xms = v
	}
	if a, err1 := strconv.Atoi(xmx); err1 == nil {
		if b, err2 := strconv.Atoi(xms); err2 == nil && b > a {
			return nil, nil, errors.New("MEMORY_XMS_GB cannot be larger than MEMORY_XMX_GB")
		}
	}
	if len(final) == 0 {
		return previous, nil, nil
	}
	for k, v := range final {
		env.Set(k, v)
		changed = append(changed, k)
	}
	sort.Strings(changed)
	if err := compose.WriteFileAtomic(path, env.Render(), 0o600); err != nil {
		return nil, nil, err
	}
	return previous, changed, nil
}
