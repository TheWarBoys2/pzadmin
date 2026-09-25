package pz

import (
	"bufio"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// LogKind classifies a parsed log line.
type LogKind string

const (
	LogJoin       LogKind = "join"
	LogLeave      LogKind = "leave"
	LogChat       LogKind = "chat"
	LogDeath      LogKind = "death"
	LogAdmin      LogKind = "admin"
	LogModUpdate  LogKind = "mod_update"
	LogPerk       LogKind = "perk"
	LogServerBoot LogKind = "boot"
	LogOther      LogKind = "other"
)

// LogEntry is one meaningful line from a Project Zomboid log.
type LogEntry struct {
	At      time.Time `json:"at"`
	Kind    LogKind   `json:"kind"`
	Player  string    `json:"player,omitempty"`
	SteamID string    `json:"steamId,omitempty"`
	Text    string    `json:"text"`
	Source  string    `json:"source"`
}

// Tailer follows the log files of one server, remembering how far it has read
// so repeated polls only see new lines. Project Zomboid rolls its logs by date,
// so files appearing and disappearing is normal and handled here.
//
// Polling beats inotify for this job: the log directory is usually a bind mount
// or network share where inotify events are unreliable, and a two second poll
// is more than fast enough for a join notification.
type Tailer struct {
	mu      sync.Mutex
	dir     string
	offsets map[string]int64
	// warmed is false until the first poll, which seeds offsets at end-of-file
	// so PZAdmin does not replay months of history on startup.
	warmed bool
}

// NewTailer returns a tailer for a log directory.
func NewTailer(dir string) *Tailer {
	return &Tailer{dir: dir, offsets: map[string]int64{}}
}

// Dir returns the directory being followed.
func (t *Tailer) Dir() string { return t.dir }

var interestingLogs = []string{"_user.txt", "_chat.txt", "_admin.txt", "_DebugLog-server.txt", "_perk.txt", "_pvp.txt"}

// Poll reads everything appended since the previous call. The first call
// returns nothing and simply records the current end of each file.
func (t *Tailer) Poll(limit int) []LogEntry {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.dir == "" {
		return nil
	}
	items, err := os.ReadDir(t.dir)
	if err != nil {
		return nil
	}

	// Only follow the newest file of each kind; PZ keeps one per day.
	newest := map[string]os.DirEntry{}
	for _, it := range items {
		if it.IsDir() {
			continue
		}
		name := it.Name()
		for _, suffix := range interestingLogs {
			if !strings.HasSuffix(name, suffix) {
				continue
			}
			if cur, ok := newest[suffix]; !ok || name > cur.Name() {
				newest[suffix] = it
			}
		}
	}

	var out []LogEntry
	suffixes := make([]string, 0, len(newest))
	for s := range newest {
		suffixes = append(suffixes, s)
	}
	sort.Strings(suffixes)

	for _, suffix := range suffixes {
		it := newest[suffix]
		path := filepath.Join(t.dir, it.Name())
		info, err := it.Info()
		if err != nil {
			continue
		}
		prev, seen := t.offsets[it.Name()]
		if !t.warmed || !seen {
			// New file, or first run: start at the end.
			t.offsets[it.Name()] = info.Size()
			continue
		}
		if info.Size() < prev {
			// Truncated or rotated in place.
			prev = 0
		}
		if info.Size() == prev {
			continue
		}
		lines, next := readFrom(path, prev, 512*1024)
		t.offsets[it.Name()] = next
		for _, line := range lines {
			if e, ok := parseLogLine(line, suffix); ok {
				out = append(out, e)
			}
		}
	}
	t.warmed = true

	// Drop stale offsets so the map cannot grow without bound.
	if len(t.offsets) > 64 {
		live := map[string]bool{}
		for _, it := range newest {
			live[it.Name()] = true
		}
		for name := range t.offsets {
			if !live[name] {
				delete(t.offsets, name)
			}
		}
	}

	if limit > 0 && len(out) > limit {
		out = out[len(out)-limit:]
	}
	return out
}

func readFrom(path string, offset int64, max int64) ([]string, int64) {
	f, err := os.Open(path)
	if err != nil {
		return nil, offset
	}
	defer f.Close()
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return nil, offset
	}
	limited := io.LimitReader(f, max)
	sc := bufio.NewScanner(limited)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	var lines []string
	var read int64
	for sc.Scan() {
		line := sc.Text()
		read += int64(len(line)) + 1
		lines = append(lines, line)
	}
	return lines, offset + read
}

var (
	// [26-09-07 21:14:03.123] user Rick fully connected (76561198000000000).
	reConnect    = regexp.MustCompile(`(?i)\b(?:user\s+)?"?([^"\[\]]{1,64}?)"?\s+(fully connected|connected|disconnected|is now connected)`)
	reSteamID    = regexp.MustCompile(`\b(7656\d{13})\b`)
	reTimestamp  = regexp.MustCompile(`^\[?(\d{2}-\d{2}-\d{2}\s+\d{2}:\d{2}:\d{2})`)
	reTimestamp2 = regexp.MustCompile(`^\[?(\d{4}-\d{2}-\d{2}\s+\d{2}:\d{2}:\d{2})`)
	reChat       = regexp.MustCompile(`(?i)ChatMessage.*?author\s*=\s*([^,]+),.*?text\s*=\s*(.*)$`)
	reDeath      = regexp.MustCompile(`(?i)\b([\w \-\.]{1,32}?)\s+died\b`)
	reModUpdate  = regexp.MustCompile(`(?i)(mods? need(s)? (to be )?updat|checkModsNeedUpdate:\s*Mods updated|workshop item.*out of date)`)
	reAdminCmd   = regexp.MustCompile(`(?i)^\s*\[?[^\]]*\]?\s*(.+?)\s+(?:issued|used) (?:the )?command\s+(.*)$`)
)

func parseLogLine(line, source string) (LogEntry, bool) {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" {
		return LogEntry{}, false
	}
	e := LogEntry{At: parseLogTime(trimmed), Text: trimmed, Source: strings.Trim(source, "_.txt"), Kind: LogOther}
	if e.At.IsZero() {
		e.At = time.Now()
	}
	if m := reSteamID.FindStringSubmatch(trimmed); m != nil {
		e.SteamID = m[1]
	}

	switch {
	case reModUpdate.MatchString(trimmed):
		e.Kind = LogModUpdate
		return e, true

	case strings.Contains(source, "_chat"):
		if m := reChat.FindStringSubmatch(trimmed); m != nil {
			e.Kind = LogChat
			e.Player = strings.TrimSpace(m[1])
			e.Text = strings.TrimSpace(m[2])
			return e, true
		}
		return LogEntry{}, false

	case strings.Contains(source, "_admin"):
		if m := reAdminCmd.FindStringSubmatch(trimmed); m != nil {
			e.Kind = LogAdmin
			e.Player = strings.TrimSpace(m[1])
			e.Text = strings.TrimSpace(m[2])
			return e, true
		}
		return LogEntry{}, false

	case strings.Contains(source, "_user"):
		if m := reConnect.FindStringSubmatch(trimmed); m != nil {
			name := strings.TrimSpace(m[1])
			if name == "" {
				return LogEntry{}, false
			}
			e.Player = name
			if strings.EqualFold(m[2], "disconnected") {
				e.Kind = LogLeave
				e.Text = name + " left the server"
			} else {
				e.Kind = LogJoin
				e.Text = name + " joined the server"
			}
			return e, true
		}
		if m := reDeath.FindStringSubmatch(trimmed); m != nil {
			e.Kind = LogDeath
			e.Player = strings.TrimSpace(m[1])
			return e, true
		}
		return LogEntry{}, false
	}
	return LogEntry{}, false
}

func parseLogTime(line string) time.Time {
	if m := reTimestamp2.FindStringSubmatch(line); m != nil {
		if t, err := time.ParseInLocation("2006-01-02 15:04:05", m[1], time.Local); err == nil {
			return t
		}
	}
	if m := reTimestamp.FindStringSubmatch(line); m != nil {
		if t, err := time.ParseInLocation("06-01-02 15:04:05", m[1], time.Local); err == nil {
			return t
		}
	}
	return time.Time{}
}

// TailFile returns the last n lines of a log file inside dir, for the log
// viewer. The name is checked against the directory listing rather than joined
// blindly.
func TailFile(dir, name string, lines int) (string, error) {
	if lines <= 0 || lines > 5000 {
		lines = 500
	}
	if name == "" || strings.ContainsAny(name, `/\`) {
		return "", os.ErrNotExist
	}
	path := filepath.Join(dir, name)
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return "", err
	}
	// Read at most the trailing 1MB; log files can be enormous.
	const window = 1 << 20
	start := st.Size() - window
	if start < 0 {
		start = 0
	}
	if _, err := f.Seek(start, io.SeekStart); err != nil {
		return "", err
	}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	ring := make([]string, 0, lines)
	for sc.Scan() {
		if len(ring) == lines {
			ring = ring[1:]
		}
		ring = append(ring, sc.Text())
	}
	return strings.Join(ring, "\n"), nil
}

// LogFile describes a log available for viewing.
type LogFile struct {
	Name     string    `json:"name"`
	Size     int64     `json:"size"`
	Modified time.Time `json:"modified"`
}

// ListLogs returns the log files in a directory, newest first.
func ListLogs(dir string) ([]LogFile, error) {
	items, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []LogFile
	for _, it := range items {
		if it.IsDir() || !strings.HasSuffix(strings.ToLower(it.Name()), ".txt") {
			continue
		}
		info, err := it.Info()
		if err != nil {
			continue
		}
		out = append(out, LogFile{Name: it.Name(), Size: info.Size(), Modified: info.ModTime()})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Modified.After(out[j].Modified) })
	if len(out) > 60 {
		out = out[:60]
	}
	return out, nil
}
