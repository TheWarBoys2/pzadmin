package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/TheWarBoys2/pzadmin/internal/config"
	"github.com/TheWarBoys2/pzadmin/internal/fsutil"
)

// The companion is an optional server-side Lua mod, shipped from mod/ in this
// repository. It writes the world and the online players to a JSON file in
// the server's Zomboid/Lua folder once a minute, or every two seconds while
// the live flag file holds a time in the future. See docs/companion.md.
const (
	companionSnapshotFile = "pzadmin_companion.json"
	companionLiveFile     = "pzadmin_companion_live.txt"
	// companionLiveLease is how far ahead each renewal sets the live flag.
	// The Live tab renews it every few seconds, so closing the tab ends live
	// mode within this long.
	companionLiveLease = 20 * time.Second
	// companionMaxBytes bounds the snapshot read. A few hundred players with
	// every field is well under 200 KB.
	companionMaxBytes = 2 << 20
)

// companionWorld is the in-game world. Pointers keep "not reported" apart
// from zero, which is a real hour, temperature or rain level.
type companionWorld struct {
	Year          *int     `json:"year,omitempty"`
	Month         *int     `json:"month,omitempty"`
	Day           *int     `json:"day,omitempty"`
	Hour          *int     `json:"hour,omitempty"`
	Minute        *int     `json:"minute,omitempty"`
	DayNumber     *int     `json:"dayNumber,omitempty"`
	WorldAgeHours *float64 `json:"worldAgeHours,omitempty"`
	Season        string   `json:"season,omitempty"`
	TemperatureC  *float64 `json:"temperatureC,omitempty"`
	Rain          *float64 `json:"rain,omitempty"`
	Snow          *float64 `json:"snow,omitempty"`
	Fog           *float64 `json:"fog,omitempty"`
	WindKph       *float64 `json:"windKph,omitempty"`
}

// companionPlayer is one online player as the server sees them.
type companionPlayer struct {
	Username      string   `json:"username"`
	Name          string   `json:"name,omitempty"`
	X             *int     `json:"x,omitempty"`
	Y             *int     `json:"y,omitempty"`
	Z             *int     `json:"z,omitempty"`
	Health        *float64 `json:"health,omitempty"`
	Infected      *bool    `json:"infected,omitempty"`
	Dead          *bool    `json:"dead,omitempty"`
	Asleep        *bool    `json:"asleep,omitempty"`
	InVehicle     *bool    `json:"inVehicle,omitempty"`
	Kills         *int     `json:"kills,omitempty"`
	HoursSurvived *float64 `json:"hoursSurvived,omitempty"`
	Profession    string   `json:"profession,omitempty"`
}

type companionSnapshot struct {
	Schema        int               `json:"schema"`
	ModVersion    string            `json:"modVersion"`
	GeneratedAtMs int64             `json:"generatedAtMs"`
	IntervalMs    int64             `json:"intervalMs"`
	Live          bool              `json:"live"`
	World         companionWorld    `json:"world"`
	Players       []companionPlayer `json:"players"`
}

// companionView is what the dashboard is sent about one server's companion.
type companionView struct {
	// Dir is the Zomboid/Lua folder PZAdmin looks in; empty when it cannot
	// be worked out from the server's folders.
	Dir string `json:"dir"`
	// Found is true once the mod has written a snapshot.
	Found bool `json:"found"`
	// Fresh is true when the snapshot is recent enough to show as current.
	Fresh     bool               `json:"fresh"`
	AgeSec    int64              `json:"ageSec"`
	Snapshot  *companionSnapshot `json:"snapshot,omitempty"`
	LiveUntil OptionalTime       `json:"liveUntil"`
	Error     string             `json:"error,omitempty"`
}

// companionDir returns the server's Zomboid/Lua folder, where Project
// Zomboid's getFileWriter writes. For a stack that is the config mount;
// for a server added by folder it is the parent of Logs, which always sits
// directly in the Zomboid folder.
func companionDir(s config.Server) string {
	root := s.ConfigDir
	if root == "" {
		l := detectLayout(s)
		switch {
		case l.LogsDir != "":
			root = filepath.Dir(l.LogsDir)
		case l.ConfigDir != "" && filepath.Base(l.ConfigDir) == "Server":
			root = filepath.Dir(l.ConfigDir)
		}
	}
	if root == "" {
		return ""
	}
	return filepath.Join(root, "Lua")
}

var errCompanionPartial = errors.New("the snapshot was being written")

// readCompanionOnce reads and parses the snapshot. The mod ends every write
// with a newline, so a file without one was caught mid-write.
func readCompanionOnce(path string) (*companionSnapshot, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(io.LimitReader(f, companionMaxBytes+1)); err != nil {
		return nil, err
	}
	if buf.Len() > companionMaxBytes {
		return nil, fmt.Errorf("%s is larger than %d bytes", path, companionMaxBytes)
	}
	data := buf.Bytes()
	if len(data) == 0 || data[len(data)-1] != '\n' {
		return nil, errCompanionPartial
	}
	var snap companionSnapshot
	if err := json.Unmarshal(data, &snap); err != nil {
		return nil, errCompanionPartial
	}
	if snap.Players == nil {
		snap.Players = []companionPlayer{}
	}
	return &snap, nil
}

// readCompanion retries a snapshot caught mid-write a few times; the mod
// writes a few kilobytes, so the next attempt almost always sees it whole.
func readCompanion(path string) (*companionSnapshot, error) {
	var err error
	for attempt := 0; attempt < 4; attempt++ {
		var snap *companionSnapshot
		if snap, err = readCompanionOnce(path); !errors.Is(err, errCompanionPartial) {
			return snap, err
		}
		time.Sleep(40 * time.Millisecond)
	}
	return nil, fmt.Errorf("%s could not be read as a complete snapshot; it may be damaged", path)
}

// companionFresh reports whether a snapshot is current: no older than two of
// its own intervals plus some slack for a busy server.
func companionFresh(snap *companionSnapshot, age time.Duration) bool {
	interval := time.Duration(snap.IntervalMs) * time.Millisecond
	if interval <= 0 {
		interval = time.Minute
	}
	return age <= 2*interval+30*time.Second
}

// readLiveFlag returns the time the live flag runs until, or zero.
func readLiveFlag(dir string) time.Time {
	data, err := os.ReadFile(filepath.Join(dir, companionLiveFile))
	if err != nil {
		return time.Time{}
	}
	secs, err := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64)
	if err != nil {
		return time.Time{}
	}
	return time.Unix(secs, 0)
}

func (a *App) companionView(s config.Server) companionView {
	v := companionView{Dir: companionDir(s)}
	if v.Dir == "" {
		v.Error = "PZAdmin cannot tell where this server's Zomboid folder is, so it does not know where to look for the companion's file."
		return v
	}
	if until := readLiveFlag(v.Dir); until.After(time.Now()) {
		v.LiveUntil = At(until)
	}
	path := filepath.Join(v.Dir, companionSnapshotFile)
	snap, err := readCompanion(path)
	if errors.Is(err, fs.ErrNotExist) {
		return v
	}
	if err != nil {
		v.Error = err.Error()
		return v
	}
	v.Found = true
	v.Snapshot = snap
	at := time.UnixMilli(snap.GeneratedAtMs)
	if snap.GeneratedAtMs <= 0 {
		if st, err := os.Stat(path); err == nil {
			at = st.ModTime()
		}
	}
	age := time.Since(at)
	if age < 0 {
		age = 0
	}
	v.AgeSec = int64(age / time.Second)
	v.Fresh = companionFresh(snap, age)
	return v
}

// companionWorldFor returns the in-game world for the external API, or nil
// when there is no current snapshot. Player positions are deliberately not
// part of the API: they stay on the signed-in dashboard.
func (a *App) companionWorldFor(s config.Server) *companionWorld {
	dir := companionDir(s)
	if dir == "" {
		return nil
	}
	path := filepath.Join(dir, companionSnapshotFile)
	st, err := os.Stat(path)
	// Skip the read entirely when the file cannot be current, which is every
	// server without the mod.
	if err != nil || time.Since(st.ModTime()) > 3*time.Minute {
		return nil
	}
	snap, err := readCompanion(path)
	if err != nil {
		return nil
	}
	if !companionFresh(snap, time.Since(time.UnixMilli(snap.GeneratedAtMs))) {
		return nil
	}
	return &snap.World
}

func (a *App) handleCompanion(w http.ResponseWriter, r *http.Request) {
	srv, found := a.cfg.Server(r.URL.Query().Get("id"))
	if !found {
		httpError(w, http.StatusNotFound, "no such server")
		return
	}
	writeJSON(w, a.companionView(srv))
}

// handleCompanionLive starts, renews or ends live mode by writing or removing
// the flag file. The Live tab calls it every few seconds while it is open.
func (a *App) handleCompanionLive(w http.ResponseWriter, r *http.Request) {
	var p struct {
		ServerID string `json:"serverId"`
		On       bool   `json:"on"`
	}
	if !decodeJSON(w, r, &p) {
		return
	}
	srv, found := a.cfg.Server(p.ServerID)
	if !found {
		httpError(w, http.StatusNotFound, "no such server")
		return
	}
	dir := companionDir(srv)
	if dir == "" {
		httpError(w, http.StatusConflict, "PZAdmin cannot tell where this server's Zomboid folder is.")
		return
	}
	flag := filepath.Join(dir, companionLiveFile)
	if !p.On {
		if err := os.Remove(flag); err != nil && !errors.Is(err, fs.ErrNotExist) {
			httpError(w, http.StatusInternalServerError, "Could not remove "+flag+": "+err.Error())
			return
		}
		writeJSON(w, map[string]any{"ok": true, "liveUntil": OptionalTime{}})
		return
	}
	// The game creates Zomboid/Lua itself. Creating it here would give it
	// PZAdmin's owner and permissions, which the game may not be able to
	// write to, so a missing folder is reported instead.
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		httpError(w, http.StatusConflict, "The folder "+dir+" does not exist yet. It appears once the server has started with the PZAdmin Companion mod enabled.")
		return
	}
	until := time.Now().Add(companionLiveLease)
	// World-readable, because the game server usually runs as another user.
	if err := fsutil.WriteFile(flag, []byte(strconv.FormatInt(until.Unix(), 10)+"\n"), 0o644); err != nil {
		httpError(w, http.StatusInternalServerError, "PZAdmin could not write "+flag+": "+err.Error())
		return
	}
	writeJSON(w, map[string]any{"ok": true, "liveUntil": At(until)})
}
