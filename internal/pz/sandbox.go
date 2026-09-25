package pz

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"
)

// SandboxVars.lua is a Lua table, but a very regular one:
//
//	SandboxVars = {
//	    Zombies = 3,
//	    ZombieLore = {
//	        Speed = 2,
//	    },
//	}
//
// Rather than parse Lua properly, this addresses settings by line, the same way
// the INI editor does. A value is rewritten in place, so indentation, comments,
// key order and anything this code does not understand all survive untouched.
// That is the safe trade: an unbalanced SandboxVars.lua stops the server
// booting with no useful error.

// Sandbox is a parsed SandboxVars file.
type Sandbox struct {
	Path  string
	lines []string
	// index maps a dotted path such as "ZombieLore.Speed" to a line number.
	index map[string]int
	order []string
}

var (
	reSandboxAssign = regexp.MustCompile(`^(\s*)([A-Za-z_][A-Za-z0-9_]*)\s*=\s*(.*?)\s*$`)
)

// LoadSandbox reads a SandboxVars-style Lua file.
func LoadSandbox(path string) (*Sandbox, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return readSandbox(path, f)
}

// ParseSandbox parses SandboxVars text that is not (yet) a file.
func ParseSandbox(text string) (*Sandbox, error) {
	return readSandbox("", strings.NewReader(text))
}

func readSandbox(path string, r io.Reader) (*Sandbox, error) {
	sb := &Sandbox{Path: path, index: map[string]int{}}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)

	var path0 []string
	for sc.Scan() {
		raw := sc.Text()
		sb.lines = append(sb.lines, raw)
		lineNo := len(sb.lines) - 1

		code := raw
		if i := strings.Index(code, "--"); i >= 0 {
			code = code[:i]
		}
		trimmed := strings.TrimSpace(code)
		if trimmed == "" {
			continue
		}

		// A closing brace pops one level of nesting.
		if strings.HasPrefix(trimmed, "}") {
			if len(path0) > 0 {
				path0 = path0[:len(path0)-1]
			}
			continue
		}

		m := reSandboxAssign.FindStringSubmatch(code)
		if m == nil {
			continue
		}
		key, value := m[2], strings.TrimSuffix(strings.TrimSpace(m[3]), ",")

		if strings.HasPrefix(strings.TrimSpace(value), "{") {
			// The top-level "SandboxVars = {" is a container, not a group name
			// worth showing, so it is not added to the path.
			if len(path0) == 0 && strings.EqualFold(key, "SandboxVars") {
				path0 = append(path0, "")
				continue
			}
			path0 = append(path0, key)
			continue
		}

		full := key
		if prefix := strings.Trim(strings.Join(path0, "."), "."); prefix != "" {
			full = prefix + "." + key
		}
		if _, seen := sb.index[full]; !seen {
			sb.order = append(sb.order, full)
		}
		sb.index[full] = lineNo
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return sb, nil
}

// Fields returns the sandbox settings as editable fields.
//
// Sandbox values are read when a world is created or loaded, so a change needs
// a server restart before it has any effect; nothing here can be applied live.
func (s *Sandbox) Fields() []Field {
	out := make([]Field, 0, len(s.order))
	for _, key := range s.order {
		lineNo := s.index[key]
		m := reSandboxAssign.FindStringSubmatch(stripLuaComment(s.lines[lineNo]))
		if m == nil {
			continue
		}
		value := strings.TrimSuffix(strings.TrimSpace(m[3]), ",")
		group := "General"
		short := key
		if i := strings.LastIndex(key, "."); i >= 0 {
			group = key[:i]
			short = key[i+1:]
		}
		f := Field{
			Key:     key,
			Value:   unquoteLua(value),
			Type:    inferLuaType(value),
			Group:   group,
			Applies: AppliesOnRestart,
			Line:    lineNo + 1,
		}
		// The game's own comment first; the short curated note only where
		// the file carries none.
		applyDoc(&f, parseDoc(commentAbove(s.lines, lineNo, "--")))
		if f.Help == "" {
			f.Help = sandboxHelp[short]
		}
		out = append(out, f)
	}
	return out
}

// sandboxHelp covers only the handful of settings whose meaning is not obvious
// from the name. Everything else is shown without a description rather than
// with a guessed one.
var sandboxHelp = map[string]string{
	"Zombies":                    "Population multiplier. Lower numbers mean fewer zombies.",
	"Distribution":               "Urban zombie distribution.",
	"DayLength":                  "Length of an in-game day.",
	"StartYear":                  "Year the world begins in.",
	"StartMonth":                 "Month the world begins in.",
	"StartDay":                   "Day the world begins in.",
	"StartTime":                  "Time of day the world begins at.",
	"WaterShut":                  "How long before the water supply fails.",
	"ElecShut":                   "How long before the power supply fails.",
	"XpMultiplier":               "Multiplier applied to all experience gained.",
	"ZombieAttractionMultiplier": "How far noise carries to zombies.",
	"Speed":                      "Zombie movement speed.",
	"Strength":                   "Zombie strength.",
	"Toughness":                  "How much damage zombies take before dying.",
	"Cognition":                  "How well zombies navigate.",
	"Memory":                     "How long zombies remember a target.",
	"Sight":                      "Zombie sight range.",
	"Hearing":                    "Zombie hearing range.",
	"CorpseSicknessRate":         "How quickly nearby corpses make players ill.",
}

func stripLuaComment(line string) string {
	if i := strings.Index(line, "--"); i >= 0 {
		return line[:i]
	}
	return line
}

func inferLuaType(value string) FieldType {
	v := strings.TrimSpace(value)
	switch strings.ToLower(v) {
	case "true", "false":
		return FieldBool
	}
	if strings.HasPrefix(v, `"`) || strings.HasPrefix(v, "'") {
		return FieldText
	}
	if _, err := strconv.Atoi(v); err == nil {
		return FieldInt
	}
	if _, err := strconv.ParseFloat(v, 64); err == nil {
		return FieldFloat
	}
	return FieldText
}

func unquoteLua(v string) string {
	v = strings.TrimSpace(v)
	if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') && v[len(v)-1] == v[0] {
		return v[1 : len(v)-1]
	}
	return v
}

// Set rewrites one value in place. The key must already exist: adding new
// sandbox settings is not something to do blind, and the game ignores keys it
// does not know anyway.
func (s *Sandbox) Set(key, value string) error {
	lineNo, ok := s.index[key]
	if !ok {
		return fmt.Errorf("unknown sandbox setting %q", key)
	}
	if strings.ContainsAny(value, "\r\n") {
		return fmt.Errorf("%s must not contain line breaks", key)
	}
	line := s.lines[lineNo]
	m := reSandboxAssign.FindStringSubmatch(stripLuaComment(line))
	if m == nil {
		return fmt.Errorf("could not rewrite %s", key)
	}
	indent, name, old := m[1], m[2], strings.TrimSpace(m[3])

	// Preserve the original literal style: a quoted value stays quoted.
	trailing := ""
	if strings.HasSuffix(old, ",") {
		trailing = ","
		old = strings.TrimSuffix(old, ",")
	}
	rendered := value
	old = strings.TrimSpace(old)
	if len(old) >= 2 && (old[0] == '"' || old[0] == '\'') {
		quote := string(old[0])
		rendered = quote + strings.ReplaceAll(value, quote, "\\"+quote) + quote
	}

	comment := ""
	if i := strings.Index(line, "--"); i >= 0 {
		comment = " " + strings.TrimSpace(line[i:])
	}
	s.lines[lineNo] = indent + name + " = " + rendered + trailing + comment
	return nil
}

// Render returns the whole file.
func (s *Sandbox) Render() string { return strings.Join(s.lines, "\n") }

// Save writes the file atomically, keeping a copy of the previous version.
func (s *Sandbox) Save(backupDir string) (string, error) {
	if err := checkLuaBraces(s.Render()); err != nil {
		return "", err
	}
	backup, err := BackupFile(s.Path, backupDir)
	if err != nil {
		return "", err
	}
	content := s.Render()
	if !strings.HasSuffix(content, "\n") {
		content += "\n"
	}
	if err := writeFileAtomic(s.Path, []byte(content), 0o644); err != nil {
		return "", err
	}
	return backup, nil
}
