package provision

import (
	"embed"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/TheWarBoys2/pzadmin/internal/pz"
)

// The defaults are the four files a clean Build 42 dedicated server writes on
// its first boot, with no Server files and no mods. They are kept verbatim so
// that refreshing them after a game update is a straight copy; everything that
// must differ per server is handled below, in code.
//
//go:embed defaults/defaults.ini defaults/defaults_SandboxVars.lua defaults/defaults_spawnregions.lua defaults/defaults_spawnpoints.lua
var defaultFiles embed.FS

// templateName is the SERVER_NAME the defaults were captured under.
const templateName = "defaults"

// perServerINIKeys are generated at random by the game for every server. A
// copy of someone else's would make two servers claim to be the same one, so
// they are removed and the game generates fresh ones on first boot. The world
// seed is in the same position.
var perServerINIKeys = []string{"ResetID", "ServerPlayerID", "Seed"}

// Starting is the set of Server/ files a new server begins from, before any
// change from the wizard.
type Starting struct {
	INI          string
	Sandbox      string
	SpawnRegions string
	SpawnPoints  string
	// From names where they came from, for display.
	From string
}

// DefaultStart returns the captured defaults, with per-server values removed.
func DefaultStart() (Starting, error) {
	read := func(suffix string) (string, error) {
		b, err := defaultFiles.ReadFile("defaults/" + templateName + suffix)
		return string(b), err
	}
	var s Starting
	var err error
	if s.INI, err = read(".ini"); err != nil {
		return s, err
	}
	s.INI = removeINIKeys(s.INI, perServerINIKeys...)
	if s.Sandbox, err = read("_SandboxVars.lua"); err != nil {
		return s, err
	}
	if s.SpawnRegions, err = read("_spawnregions.lua"); err != nil {
		return s, err
	}
	if s.SpawnPoints, err = read("_spawnpoints.lua"); err != nil {
		return s, err
	}
	s.From = templateName
	return s, nil
}

// CloneStart reads another server's Server/ files. Its world identity goes
// with it only if asked: a copy that keeps ResetID and ServerPlayerID tells
// players' clients it is the same server as the original.
func CloneStart(dir, name string) (Starting, error) {
	var s Starting
	b, err := os.ReadFile(filepath.Join(dir, name+".ini"))
	if err != nil {
		return s, fmt.Errorf("the source server has no %s.ini: %w", name, err)
	}
	s.INI = removeINIKeys(string(b), perServerINIKeys...)
	b, err = os.ReadFile(filepath.Join(dir, name+"_SandboxVars.lua"))
	if err != nil {
		return s, fmt.Errorf("the source server has no %s_SandboxVars.lua: %w", name, err)
	}
	s.Sandbox = string(b)
	if b, err := os.ReadFile(filepath.Join(dir, name+"_spawnregions.lua")); err == nil {
		s.SpawnRegions = string(b)
	}
	if b, err := os.ReadFile(filepath.Join(dir, name+"_spawnpoints.lua")); err == nil {
		s.SpawnPoints = string(b)
	}
	s.From = name
	return s, nil
}

// removeINIKeys drops keys along with the comment block that describes them.
func removeINIKeys(text string, keys ...string) string {
	drop := map[string]bool{}
	for _, k := range keys {
		drop[strings.ToLower(k)] = true
	}
	lines := strings.Split(text, "\n")
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		t := strings.TrimSpace(line)
		if eq := strings.IndexByte(t, '='); eq > 0 && !strings.HasPrefix(t, "#") &&
			drop[strings.ToLower(strings.TrimSpace(t[:eq]))] {
			// Take back the comment lines written just above it.
			for len(out) > 0 && strings.HasPrefix(strings.TrimSpace(out[len(out)-1]), "#") {
				out = out[:len(out)-1]
			}
			// And one of the blank lines separating it from the previous key.
			if len(out) > 0 && strings.TrimSpace(out[len(out)-1]) == "" {
				out = out[:len(out)-1]
			}
			continue
		}
		out = append(out, line)
	}
	return strings.Join(out, "\n")
}

// Fields is what the wizard shows for editing.
type Fields struct {
	From    string     `json:"from"`
	INI     []pz.Field `json:"ini"`
	Sandbox []pz.Field `json:"sandbox"`
}

// FieldsFor lists the settings a new server starting from s can be given.
// Keys .env owns are marked locked: they are set on the wizard's first page.
func FieldsFor(s Starting) (Fields, error) {
	ini, err := pz.ParseINI(s.INI)
	if err != nil {
		return Fields{}, err
	}
	sb, err := pz.ParseSandbox(s.Sandbox)
	if err != nil {
		return Fields{}, err
	}
	f := Fields{From: s.From, INI: ini.Fields(), Sandbox: sb.Fields()}
	locked := LockedINIKeys(map[string]string{"MAX_PLAYERS": "set"})
	for i := range f.INI {
		if reason := locked[f.INI[i].Key]; reason != "" {
			f.INI[i].Locked = "Set on the first page of this wizard, and kept in .env."
		}
		if f.INI[i].Secret {
			// A cloned server's secrets are not shown; a blank box keeps them.
			f.INI[i].Value = ""
		}
	}
	if len(f.Sandbox) == 0 {
		return f, errors.New("the sandbox file has no settings")
	}
	return f, nil
}

// applyChanges validates every change against the starting files and
// returns the edited texts. Nothing is written.
func applyChanges(s Starting, iniChanges, sandboxChanges map[string]string) (string, string, error) {
	fields, err := FieldsFor(s)
	if err != nil {
		return "", "", err
	}
	ini, _ := pz.ParseINI(s.INI)
	sb, _ := pz.ParseSandbox(s.Sandbox)

	known := map[string]pz.Field{}
	for _, f := range fields.INI {
		known[f.Key] = f
	}
	for key, value := range iniChanges {
		f, ok := known[key]
		if !ok {
			return "", "", fmt.Errorf("%s is not a server setting", key)
		}
		if f.Locked != "" {
			return "", "", fmt.Errorf("%s is set on the first page, not here", key)
		}
		if f.Secret && value == "" {
			continue
		}
		clean, err := pz.ValidateField(f, value)
		if err != nil {
			return "", "", err
		}
		if err := ini.Set(key, clean); err != nil {
			return "", "", err
		}
	}

	knownSB := map[string]pz.Field{}
	for _, f := range fields.Sandbox {
		knownSB[f.Key] = f
	}
	for key, value := range sandboxChanges {
		f, ok := knownSB[key]
		if !ok {
			return "", "", fmt.Errorf("%s is not a sandbox setting", key)
		}
		clean, err := pz.ValidateField(f, value)
		if err != nil {
			return "", "", err
		}
		if err := sb.Set(key, clean); err != nil {
			return "", "", err
		}
	}
	return ini.Render(), sb.Render(), nil
}
