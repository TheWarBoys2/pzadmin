package pz

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/TheWarBoys2/pzadmin/internal/fsutil"
)

// INI is a Project Zomboid server .ini file. Project Zomboid writes a flat
// key=value file with comments, and it rewrites the file itself on shutdown, so
// this parser preserves line order, comments and blank lines exactly. Editing a
// key changes only that line.
type INI struct {
	Path  string
	lines []iniLine
	index map[string]int // lower-cased key -> line number
}

type iniLine struct {
	raw   string
	key   string
	value string
	isKV  bool
}

// Setting is one key/value pair exposed to the UI.
type Setting struct {
	Key     string `json:"key"`
	Value   string `json:"value"`
	Line    int    `json:"line"`
	Comment string `json:"comment,omitempty"`
}

// LoadINI reads and parses an .ini file.
func LoadINI(path string) (*INI, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return readINI(path, f)
}

// ParseINI parses ini text that is not (yet) a file.
func ParseINI(text string) (*INI, error) {
	return readINI("", strings.NewReader(text))
}

func readINI(path string, r io.Reader) (*INI, error) {
	ini := &INI{Path: path, index: map[string]int{}}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		raw := sc.Text()
		trimmed := strings.TrimSpace(raw)
		line := iniLine{raw: raw}
		if trimmed != "" && !strings.HasPrefix(trimmed, "#") && !strings.HasPrefix(trimmed, ";") {
			if eq := strings.Index(raw, "="); eq > 0 {
				line.key = strings.TrimSpace(raw[:eq])
				line.value = strings.TrimSpace(raw[eq+1:])
				line.isKV = line.key != ""
			}
		}
		if line.isKV {
			ini.index[strings.ToLower(line.key)] = len(ini.lines)
		}
		ini.lines = append(ini.lines, line)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return ini, nil
}

// commentAbove returns the "#" comment block directly above line index n.
func (i *INI) commentAbove(n int) []string {
	raw := make([]string, len(i.lines))
	for k, l := range i.lines {
		raw[k] = l.raw
	}
	return commentAbove(raw, n, "#")
}

// Get returns a value and whether the key exists.
func (i *INI) Get(key string) (string, bool) {
	idx, ok := i.index[strings.ToLower(key)]
	if !ok {
		return "", false
	}
	return i.lines[idx].value, true
}

// GetInt returns an integer value, or def when absent or unparseable.
func (i *INI) GetInt(key string, def int) int {
	v, ok := i.Get(key)
	if !ok {
		return def
	}
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil {
		return def
	}
	return n
}

// Set updates a key in place, appending it if it does not exist. Values
// containing newlines are rejected: a stray newline would silently corrupt
// every setting after it.
func (i *INI) Set(key, value string) error {
	if strings.ContainsAny(value, "\r\n") {
		return fmt.Errorf("value for %s must not contain line breaks", key)
	}
	if strings.TrimSpace(key) == "" {
		return errors.New("setting name is required")
	}
	if idx, ok := i.index[strings.ToLower(key)]; ok {
		i.lines[idx].value = value
		i.lines[idx].raw = i.lines[idx].key + "=" + value
		return nil
	}
	i.index[strings.ToLower(key)] = len(i.lines)
	i.lines = append(i.lines, iniLine{raw: key + "=" + value, key: key, value: value, isKV: true})
	return nil
}

// Settings returns every key/value pair in file order.
func (i *INI) Settings() []Setting {
	out := make([]Setting, 0, len(i.index))
	for n, l := range i.lines {
		if l.isKV {
			out = append(out, Setting{Key: l.key, Value: l.value, Line: n + 1})
		}
	}
	return out
}

// Render returns the file's full text.
func (i *INI) Render() string {
	var b strings.Builder
	for n, l := range i.lines {
		b.WriteString(l.raw)
		if n < len(i.lines)-1 {
			b.WriteByte('\n')
		}
	}
	return b.String()
}

// Save writes the file atomically, first copying the previous contents into
// backupDir. Project Zomboid rewrites these files itself, so an unrecoverable
// mistake here would cost a world's configuration.
func (i *INI) Save(backupDir string) (string, error) {
	backup, err := BackupFile(i.Path, backupDir)
	if err != nil {
		return "", err
	}
	if err := writeFileAtomic(i.Path, []byte(i.Render()+"\n"), 0o644); err != nil {
		return "", err
	}
	return backup, nil
}

// BackupFile copies path into dir with a timestamped name and returns the copy.
func BackupFile(path, dir string) (string, error) {
	if dir == "" {
		return "", nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", nil
		}
		return "", err
	}
	name := fmt.Sprintf("%s.%s.bak", filepath.Base(path), time.Now().UTC().Format("20060102-150405"))
	dst := filepath.Join(dir, name)
	if err := os.WriteFile(dst, data, 0o644); err != nil {
		return "", err
	}
	pruneBackups(dir, filepath.Base(path), 20)
	return dst, nil
}

func pruneBackups(dir, prefix string, keep int) {
	items, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	var matches []string
	for _, it := range items {
		if !it.IsDir() && strings.HasPrefix(it.Name(), prefix+".") && strings.HasSuffix(it.Name(), ".bak") {
			matches = append(matches, it.Name())
		}
	}
	if len(matches) <= keep {
		return
	}
	sort.Strings(matches)
	for _, name := range matches[:len(matches)-keep] {
		_ = os.Remove(filepath.Join(dir, name))
	}
}

// writeFileAtomic replaces a game config file. An existing file keeps its own
// permissions: the server .ini holds the RCON and join passwords, so a file
// the operator made private must not come back world-readable. mode is only
// used for a new file.
func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	if st, err := os.Stat(path); err == nil {
		mode = st.Mode().Perm()
	}
	return fsutil.WriteFile(path, data, mode)
}

// ConfigFile describes an editable file in a server's config directory.
type ConfigFile struct {
	Name     string    `json:"name"`
	Kind     string    `json:"kind"` // ini | sandbox | spawn | other
	Size     int64     `json:"size"`
	Modified time.Time `json:"modified"`
	Editable bool      `json:"editable"`
}

// ListConfigFiles returns the editable files in a server's config directory.
func ListConfigFiles(configDir string) ([]ConfigFile, error) {
	items, err := os.ReadDir(configDir)
	if err != nil {
		return nil, err
	}
	var out []ConfigFile
	for _, it := range items {
		if it.IsDir() {
			continue
		}
		lower := strings.ToLower(it.Name())
		kind := ""
		switch {
		case strings.HasSuffix(lower, "_sandboxvars.lua"):
			kind = "sandbox"
		case strings.HasSuffix(lower, "_spawnregions.lua"):
			kind = "spawn"
		case strings.HasSuffix(lower, ".ini"):
			kind = "ini"
		case strings.HasSuffix(lower, ".lua"):
			kind = "other"
		default:
			continue
		}
		info, err := it.Info()
		if err != nil {
			continue
		}
		out = append(out, ConfigFile{
			Name: it.Name(), Kind: kind, Size: info.Size(),
			Modified: info.ModTime(), Editable: info.Size() < 2<<20,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Kind != out[j].Kind {
			return out[i].Kind < out[j].Kind
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}

// ReadConfigFile returns the text of one file in configDir. The name must be a
// bare filename; anything with a separator is refused.
func ReadConfigFile(configDir, name string) (string, error) {
	path, err := configFilePath(configDir, name)
	if err != nil {
		return "", err
	}
	st, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if st.Size() > 2<<20 {
		return "", errors.New("file is too large to edit in the browser")
	}
	b, err := os.ReadFile(path)
	return string(b), err
}

// WriteConfigFile replaces one file in configDir, backing up the original.
// Lua files are checked for balanced braces first, because an unbalanced
// SandboxVars.lua stops the server booting with no useful error.
func WriteConfigFile(configDir, name, content, backupDir string) (string, error) {
	path, err := configFilePath(configDir, name)
	if err != nil {
		return "", err
	}
	if strings.HasSuffix(strings.ToLower(name), ".lua") {
		if err := checkLuaBraces(content); err != nil {
			return "", err
		}
	}
	backup, err := BackupFile(path, backupDir)
	if err != nil {
		return "", err
	}
	normalised := strings.ReplaceAll(content, "\r\n", "\n")
	if !strings.HasSuffix(normalised, "\n") {
		normalised += "\n"
	}
	if err := writeFileAtomic(path, []byte(normalised), 0o644); err != nil {
		return "", err
	}
	return backup, nil
}

func configFilePath(configDir, name string) (string, error) {
	if configDir == "" {
		return "", errors.New("this server has no detected config directory")
	}
	if name == "" || strings.ContainsAny(name, `/\`) || name == "." || name == ".." {
		return "", fmt.Errorf("invalid file name %q", name)
	}
	lower := strings.ToLower(name)
	if !strings.HasSuffix(lower, ".ini") && !strings.HasSuffix(lower, ".lua") {
		return "", errors.New("only .ini and .lua files can be edited")
	}
	return filepath.Join(configDir, name), nil
}

// checkLuaBraces performs a cheap structural check on a Lua table file.
func checkLuaBraces(src string) error {
	depth := 0
	inString := byte(0)
	inComment := false
	for i := 0; i < len(src); i++ {
		c := src[i]
		if inComment {
			if c == '\n' {
				inComment = false
			}
			continue
		}
		if inString != 0 {
			if c == '\\' {
				i++
				continue
			}
			if c == inString {
				inString = 0
			}
			continue
		}
		switch {
		case c == '-' && i+1 < len(src) && src[i+1] == '-':
			inComment = true
		case c == '"' || c == '\'':
			inString = c
		case c == '{':
			depth++
		case c == '}':
			depth--
			if depth < 0 {
				return fmt.Errorf("unbalanced braces: unexpected } at byte %d", i)
			}
		}
	}
	if depth != 0 {
		return fmt.Errorf("unbalanced braces: %d unclosed { remaining", depth)
	}
	return nil
}
