package compose

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// EnvFile is a .env file, kept as its original lines rather than as a map.
//
// These files are documentation as much as configuration: the comment above a
// setting is usually the only explanation of what it does. Editing one through
// a map and writing it back would discard all of that, so every line is
// retained and Set rewrites in place.
type EnvFile struct {
	Path  string
	lines []envLine
}

type envLine struct {
	raw string
	// key is empty for comments and blank lines.
	key   string
	value string
}

// ParseEnv reads the contents of a .env file.
func ParseEnv(src []byte) *EnvFile {
	e := &EnvFile{}
	text := strings.ReplaceAll(string(src), "\r\n", "\n")
	for _, raw := range strings.Split(text, "\n") {
		trimmed := strings.TrimSpace(raw)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			e.lines = append(e.lines, envLine{raw: raw})
			continue
		}
		body := strings.TrimPrefix(trimmed, "export ")
		idx := strings.IndexByte(body, '=')
		if idx <= 0 {
			// Not a setting. Keep it verbatim so nothing is lost.
			e.lines = append(e.lines, envLine{raw: raw})
			continue
		}
		key := strings.TrimSpace(body[:idx])
		value := parseEnvValue(body[idx+1:])
		e.lines = append(e.lines, envLine{raw: raw, key: key, value: value})
	}
	// A trailing newline produces a final empty element; drop it so writing
	// the file back does not grow a blank line on every save.
	if n := len(e.lines); n > 0 && e.lines[n-1].raw == "" && e.lines[n-1].key == "" {
		e.lines = e.lines[:n-1]
	}
	return e
}

// parseEnvValue removes quotes and any inline comment.
func parseEnvValue(v string) string {
	v = strings.TrimLeft(v, " \t")
	if v == "" {
		return ""
	}
	switch v[0] {
	case '\'':
		if end := strings.IndexByte(v[1:], '\''); end >= 0 {
			return v[1 : 1+end]
		}
		return v[1:]
	case '"':
		// Scan for the closing quote honouring backslash escapes. Stopping at
		// the first quote character would cut a value like a\"b in half, and
		// since quoteEnv writes exactly that form for a password containing a
		// quote, the file would not survive its own round trip.
		var out strings.Builder
		for i := 1; i < len(v); i++ {
			c := v[i]
			if c == '\\' && i+1 < len(v) {
				switch v[i+1] {
				case '"':
					out.WriteByte('"')
				case 'n':
					out.WriteByte('\n')
				case '\\':
					out.WriteByte('\\')
				default:
					out.WriteByte('\\')
					out.WriteByte(v[i+1])
				}
				i++
				continue
			}
			if c == '"' {
				return out.String()
			}
			out.WriteByte(c)
		}
		return out.String()
	}
	// Unquoted. A '#' begins a comment only when whitespace precedes it, so
	// that a value which merely contains a hash survives.
	for i := 0; i < len(v); i++ {
		if v[i] == '#' && i > 0 && (v[i-1] == ' ' || v[i-1] == '\t') {
			return strings.TrimRight(v[:i], " \t")
		}
	}
	return strings.TrimRight(v, " \t")
}

// maxEnvBytes bounds what will be read as an env file. The path comes from a
// compose file's env_file key, which is not necessarily pointing at what its
// author thought it was, and a real one is a few hundred bytes.
const maxEnvBytes = 1 << 20

// ReadEnvFile loads a .env file from disk. A missing file is an empty one, so
// that a caller building a new file does not have to special-case it.
func ReadEnvFile(path string) (*EnvFile, error) {
	st, err := os.Stat(path)
	if err == nil && st.Size() > maxEnvBytes {
		return nil, fmt.Errorf("%s is too large to be an environment file", filepath.Base(path))
	}
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &EnvFile{Path: path}, nil
		}
		return nil, err
	}
	e := ParseEnv(b)
	e.Path = path
	return e, nil
}

// Get returns a value and whether it was set.
func (e *EnvFile) Get(key string) (string, bool) {
	for i := len(e.lines) - 1; i >= 0; i-- {
		if e.lines[i].key == key {
			return e.lines[i].value, true
		}
	}
	return "", false
}

// Lookup is Get without the second return, for the common case.
func (e *EnvFile) Lookup(key string) string {
	v, _ := e.Get(key)
	return v
}

// Set writes a value, replacing the existing line if there is one.
func (e *EnvFile) Set(key, value string) {
	for i := range e.lines {
		if e.lines[i].key != key {
			continue
		}
		e.lines[i].value = value
		e.lines[i].raw = key + "=" + quoteEnv(value)
		// Any later duplicate would win on read, so clear them.
		for j := i + 1; j < len(e.lines); j++ {
			if e.lines[j].key == key {
				e.lines = append(e.lines[:j], e.lines[j+1:]...)
				j--
			}
		}
		return
	}
	e.lines = append(e.lines, envLine{raw: key + "=" + quoteEnv(value), key: key, value: value})
}

// Unset removes a setting, leaving surrounding comments alone.
func (e *EnvFile) Unset(key string) {
	out := e.lines[:0]
	for _, l := range e.lines {
		if l.key == key {
			continue
		}
		out = append(out, l)
	}
	e.lines = out
}

// Keys returns every key in file order.
func (e *EnvFile) Keys() []string {
	var out []string
	seen := map[string]bool{}
	for _, l := range e.lines {
		if l.key == "" || seen[l.key] {
			continue
		}
		seen[l.key] = true
		out = append(out, l.key)
	}
	return out
}

// Map returns the settings as a map.
func (e *EnvFile) Map() map[string]string {
	out := map[string]string{}
	for _, l := range e.lines {
		if l.key != "" {
			out[l.key] = l.value
		}
	}
	return out
}

// Clone returns an independent copy.
func (e *EnvFile) Clone() *EnvFile {
	cp := &EnvFile{Path: e.Path, lines: make([]envLine, len(e.lines))}
	copy(cp.lines, e.lines)
	return cp
}

// Render returns the file's text.
func (e *EnvFile) Render() []byte {
	var b strings.Builder
	for _, l := range e.lines {
		b.WriteString(l.raw)
		b.WriteByte('\n')
	}
	return []byte(b.String())
}

// quoteEnv quotes a value only when it needs it.
func quoteEnv(v string) string {
	if v == "" {
		return ""
	}
	if strings.ContainsAny(v, " \t\"'#$\n\\") {
		r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)
		return `"` + r.Replace(v) + `"`
	}
	return v
}

// interpolate expands ${VAR}, ${VAR:-default}, ${VAR-default} and $VAR from
// the given values, which is what Compose does to a compose file using the
// project's .env.
//
// Unresolved variables are left exactly as written and reported, because
// substituting an empty string would turn "${PZ_ROOT}/saves" into "/saves" and
// point PZAdmin at a path that has nothing to do with the server.
func interpolate(s string, vars map[string]string) (string, []string) {
	var missing []string
	var b strings.Builder
	for i := 0; i < len(s); {
		c := s[i]
		if c != '$' {
			b.WriteByte(c)
			i++
			continue
		}
		if i+1 < len(s) && s[i+1] == '$' {
			// $$ is an escaped dollar.
			b.WriteByte('$')
			i += 2
			continue
		}
		if i+1 < len(s) && s[i+1] == '{' {
			end := strings.IndexByte(s[i+2:], '}')
			if end < 0 {
				b.WriteString(s[i:])
				break
			}
			expr := s[i+2 : i+2+end]
			value, ok := resolveVar(expr, vars)
			if !ok {
				missing = append(missing, varName(expr))
				b.WriteString(s[i : i+3+end])
			} else {
				b.WriteString(value)
			}
			i += 3 + end
			continue
		}
		// Bare $VAR.
		j := i + 1
		for j < len(s) && (s[j] == '_' || isAlnum(s[j])) {
			j++
		}
		if j == i+1 {
			b.WriteByte('$')
			i++
			continue
		}
		name := s[i+1 : j]
		if value, ok := vars[name]; ok {
			b.WriteString(value)
		} else {
			missing = append(missing, name)
			b.WriteString(s[i:j])
		}
		i = j
	}
	sort.Strings(missing)
	return b.String(), dedupeStrings(missing)
}

func resolveVar(expr string, vars map[string]string) (string, bool) {
	for _, sep := range []string{":-", "-", ":?", "?", ":+", "+"} {
		idx := strings.Index(expr, sep)
		if idx <= 0 {
			continue
		}
		name := expr[:idx]
		arg := expr[idx+len(sep):]
		value, present := vars[name]
		switch sep {
		case ":-":
			if value == "" {
				return arg, true
			}
			return value, true
		case "-":
			if !present {
				return arg, true
			}
			return value, true
		case ":+", "+":
			if value != "" {
				return arg, true
			}
			return "", true
		case ":?", "?":
			if value == "" {
				return "", false
			}
			return value, true
		}
	}
	value, present := vars[expr]
	return value, present
}

func varName(expr string) string {
	for _, sep := range []string{":-", "-", ":?", "?", ":+", "+"} {
		if idx := strings.Index(expr, sep); idx > 0 {
			return expr[:idx]
		}
	}
	return expr
}

func isAlnum(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
}

func dedupeStrings(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := in[:0]
	var last string
	for i, v := range in {
		if i == 0 || v != last {
			out = append(out, v)
		}
		last = v
	}
	return out
}

// WriteFileAtomic writes data to path via a temporary file in the same
// directory, so a crash mid-write cannot leave a half-written compose file
// that Docker would then refuse to read.
func WriteFileAtomic(path string, data []byte, perm os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".pzadmin-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(name, perm); err != nil {
		return err
	}
	if err := os.Rename(name, path); err != nil {
		return fmt.Errorf("replace %s: %w", path, err)
	}
	return nil
}
