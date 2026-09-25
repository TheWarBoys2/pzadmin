package pz

import (
	"bufio"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Mod is one mod either declared in the server config or found on disk.
type Mod struct {
	// ID is what belongs in the Mods= line. It comes from the id= field in
	// mod.info, which is frequently NOT the folder name: a Workshop item can
	// hold several mods, and their folders are named however the author liked.
	ID string `json:"id"`
	// Folder is the directory the mod actually lives in, kept so a mod can be
	// matched by either name.
	Folder string `json:"folder,omitempty"`
	// Path is the absolute directory the mod occupies on disk. It is what
	// makes an orphaned mod actionable: knowing a stale copy exists is no use
	// without being able to say where.
	Path string `json:"path,omitempty"`
	// Declared is the id= the mod states in its own mod.info. ID carries the
	// spelling used in the Mods= line, which is not always the same thing, so
	// both are kept: the declared one is the authority.
	Declared string `json:"declared,omitempty"`
	// Name is the human title from mod.info, when available.
	Name string `json:"name,omitempty"`
	// WorkshopID is the numeric Steam Workshop item, when known.
	WorkshopID string `json:"workshopId,omitempty"`
	// Enabled means the mod appears in the server's Mods= line.
	Enabled bool `json:"enabled"`
	// Installed means a matching folder was found on disk.
	Installed bool `json:"installed"`
	// UpdatedAt is the newest modification time in the mod's folder.
	UpdatedAt time.Time `json:"updatedAt,omitempty"`
	// Description and Category come from mod.info, for the manager listing.
	Description string `json:"description,omitempty"`
	Category    string `json:"category,omitempty"`
	Version     string `json:"version,omitempty"`
	URL         string `json:"url,omitempty"`

	// Ordering rules, all declared by the mod author in mod.info. Project
	// Zomboid loads mods in the order given by the Mods= line, so a mod that
	// depends on another must appear after it; these fields are what makes
	// sorting the list automatically possible rather than guesswork.
	Require      []string `json:"require,omitempty"`
	LoadAfter    []string `json:"loadAfter,omitempty"`
	LoadBefore   []string `json:"loadBefore,omitempty"`
	Incompatible []string `json:"incompatible,omitempty"`
	LoadFirst    bool     `json:"loadFirst,omitempty"`
	LoadLast     bool     `json:"loadLast,omitempty"`
}

// ModReport is the result of scanning one server's mods.
type ModReport struct {
	Mods []Mod `json:"mods"`
	// Missing lists mods enabled in config but not present on disk. This is the
	// single most common reason a modded server refuses to start.
	Missing []string `json:"missing"`
	// WorkshopIDs is the parsed WorkshopItems= line.
	WorkshopIDs []string `json:"workshopIds"`
	// Duplicates lists every installed folder that shares its declared mod ID
	// with another folder.
	//
	// Mods above holds one entry per mod, because that is what the interface
	// needs to list. Collapsing them there is right; losing them entirely is
	// not. Project Zomboid loads whichever of two same-named folders it
	// reaches first, so an old copy left behind after a mod moved Workshop
	// items silently wins and nothing anywhere says which one is in play. That
	// only shows up as a duplicate if the duplicates survive the scan.
	Duplicates []Mod     `json:"duplicates,omitempty"`
	ScannedAt  time.Time `json:"scannedAt"`
}

// Scanner caches mod scans. Walking a Workshop tree on every status poll is
// pointless disk churn, so results are reused until a watched directory's
// modification time changes, or until the cache entry ages out.
type Scanner struct {
	mu    sync.Mutex
	cache map[string]*modCacheEntry
	ttl   time.Duration
}

type modCacheEntry struct {
	report    ModReport
	signature string
	expires   time.Time
}

// NewScanner returns a scanner with the given cache lifetime.
func NewScanner(ttl time.Duration) *Scanner {
	if ttl <= 0 {
		ttl = 5 * time.Minute
	}
	return &Scanner{cache: map[string]*modCacheEntry{}, ttl: ttl}
}

// Scan returns the mod report for a layout, using the cache when nothing on
// disk has changed.
func (s *Scanner) Scan(l Layout) ModReport {
	if !l.Valid() {
		return ModReport{ScannedAt: time.Now()}
	}
	sig := signature(l)

	s.mu.Lock()
	if e, ok := s.cache[l.Base]; ok && e.signature == sig && time.Now().Before(e.expires) {
		report := e.report
		s.mu.Unlock()
		return report
	}
	s.mu.Unlock()

	report := scan(l)

	s.mu.Lock()
	s.cache[l.Base] = &modCacheEntry{report: report, signature: sig, expires: time.Now().Add(s.ttl)}
	s.mu.Unlock()
	return report
}

// Invalidate drops the cached scan for a layout.
func (s *Scanner) Invalidate(base string) {
	s.mu.Lock()
	delete(s.cache, base)
	s.mu.Unlock()
}

// signature is a cheap fingerprint of the directories that affect a scan.
func signature(l Layout) string {
	var b strings.Builder
	stamp := func(p string) {
		if st, err := os.Stat(p); err == nil {
			b.WriteString(p)
			b.WriteByte(':')
			b.WriteString(st.ModTime().UTC().Format(time.RFC3339Nano))
			b.WriteByte('|')
		}
	}
	stamp(l.ConfigDir)
	for _, d := range l.ModDirs {
		stamp(d)
	}
	// The .ini files themselves decide which mods are enabled.
	if items, err := os.ReadDir(l.ConfigDir); err == nil {
		for _, it := range items {
			if strings.HasSuffix(strings.ToLower(it.Name()), ".ini") {
				stamp(filepath.Join(l.ConfigDir, it.Name()))
			}
		}
	}
	return b.String()
}

func scan(l Layout) ModReport {
	report := ModReport{ScannedAt: time.Now()}
	enabled, workshop := enabledMods(l.ConfigDir)
	report.WorkshopIDs = workshop

	// Index every installed mod under both the ID it declares and the folder it
	// lives in. The Mods= line uses the declared ID, but operators and older
	// guides sometimes write the folder name, and for a bundled Workshop item
	// the two are usually different. Looking up only one of them made every
	// bundled mod appear twice: once as enabled-but-missing and once as
	// installed-but-unused.
	installed := map[string]Mod{}
	var installedList []Mod
	register := func(key string, m Mod) {
		key = strings.ToLower(strings.TrimSpace(key))
		if key == "" {
			return
		}
		if _, seen := installed[key]; !seen {
			installed[key] = m
		}
	}
	for _, dir := range l.ModDirs {
		for _, m := range installedIn(dir) {
			installedList = append(installedList, m)
			register(m.ID, m)
			register(m.Folder, m)
		}
	}

	byID := map[string]*Mod{}
	order := []string{}
	for _, id := range enabled {
		key := strings.ToLower(id)
		m := &Mod{ID: id, Enabled: true}
		if found, ok := installed[key]; ok {
			// Take everything mod.info gave us, not a hand-picked few fields:
			// the ordering rules live here and dropping them silently disabled
			// dependency sorting.
			copy := found
			// Carry through what mod.info actually declared, which describeMod
			// leaves empty when there is no mod.info to read. Copying found.ID
			// here would have put the folder name back in as a declaration.
			copy.Declared = found.Declared
			// ID keeps the spelling from the Mods= line so the interface can
			// show what is actually configured; Declared says what it should
			// be.
			copy.ID = id
			copy.Enabled = true
			copy.Installed = true
			m = &copy
		} else {
			report.Missing = append(report.Missing, id)
		}
		byID[key] = m
		order = append(order, key)
	}
	// Installed but not enabled. Walk the list rather than the index, so a mod
	// registered under two keys is only offered once.
	for _, found := range installedList {
		key := strings.ToLower(found.ID)
		if _, ok := byID[key]; ok {
			continue
		}
		if _, ok := byID[strings.ToLower(found.Folder)]; ok {
			continue
		}
		cp := found
		byID[key] = &cp
		order = append(order, key)
	}
	sort.Strings(order)
	for _, key := range order {
		report.Mods = append(report.Mods, *byID[key])
	}
	sort.Slice(report.Mods, func(i, j int) bool {
		if report.Mods[i].Enabled != report.Mods[j].Enabled {
			return report.Mods[i].Enabled
		}
		return strings.ToLower(report.Mods[i].ID) < strings.ToLower(report.Mods[j].ID)
	})
	report.Duplicates = findDuplicates(installedList)
	return report
}

// findDuplicates groups every installed folder by the ID its mod.info declares
// and returns the members of any group with more than one distinct folder.
func findDuplicates(installed []Mod) []Mod {
	byDeclared := map[string][]Mod{}
	for _, m := range installed {
		declared := strings.ToLower(strings.TrimSpace(m.Declared))
		if declared == "" || m.Path == "" {
			continue
		}
		group := byDeclared[declared]
		for _, seen := range group {
			if seen.Path == m.Path {
				// The same folder registered twice, not two folders.
				declared = ""
				break
			}
		}
		if declared == "" {
			continue
		}
		byDeclared[declared] = append(group, m)
	}

	var out []Mod
	keys := make([]string, 0, len(byDeclared))
	for k, group := range byDeclared {
		if len(group) > 1 {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	for _, k := range keys {
		group := byDeclared[k]
		sort.Slice(group, func(i, j int) bool { return group[i].Path < group[j].Path })
		out = append(out, group...)
	}
	return out
}

// enabledMods reads Mods= and WorkshopItems= from every .ini in configDir.
func enabledMods(configDir string) (mods, workshop []string) {
	items, err := os.ReadDir(configDir)
	if err != nil {
		return nil, nil
	}
	seenMod := map[string]bool{}
	seenWS := map[string]bool{}
	for _, it := range items {
		if it.IsDir() || !strings.HasSuffix(strings.ToLower(it.Name()), ".ini") {
			continue
		}
		ini, err := LoadINI(filepath.Join(configDir, it.Name()))
		if err != nil {
			continue
		}
		if v, ok := ini.Get("Mods"); ok {
			for _, id := range splitList(v) {
				if !seenMod[strings.ToLower(id)] {
					seenMod[strings.ToLower(id)] = true
					mods = append(mods, id)
				}
			}
		}
		if v, ok := ini.Get("WorkshopItems"); ok {
			for _, id := range splitList(v) {
				if !seenWS[id] {
					seenWS[id] = true
					workshop = append(workshop, id)
				}
			}
		}
	}
	return mods, workshop
}

func splitList(v string) []string {
	var out []string
	for _, part := range strings.Split(v, ";") {
		part = strings.TrimSpace(part)
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

// installedIn finds mods below dir. A Workshop content directory nests mods one
// level deeper, under <workshopID>/mods/<folder>, and one Workshop item often
// contains several mods, so every folder there is inspected rather than assumed
// to be a single mod.
func installedIn(dir string) []Mod {
	var out []Mod
	items, err := os.ReadDir(dir)
	if err != nil {
		return out
	}
	for _, it := range items {
		if !it.IsDir() {
			continue
		}
		path := filepath.Join(dir, it.Name())

		if nested, err := os.ReadDir(filepath.Join(path, "mods")); err == nil {
			for _, n := range nested {
				if !n.IsDir() {
					continue
				}
				m := describeMod(filepath.Join(path, "mods", n.Name()), n.Name())
				m.WorkshopID = it.Name()
				out = append(out, m)
			}
			continue
		}
		out = append(out, describeMod(path, it.Name()))
	}
	return out
}

// modInfoPath finds a mod's mod.info.
//
// Build 41 puts it at the mod's root. Build 42 puts it in a version folder
// alongside a common/ directory, so <mod>/42/mod.info is the normal case now,
// and a mod supporting both builds has one in each. Looking only at the root
// meant a Build 42 mod appeared to have no mod.info at all, and its ID was
// then taken from the folder name — which for a Workshop download is usually
// the author's display name, spaces and all. That produced confident, wrong
// mod IDs.
func modInfoPath(path string) string {
	if fileExists(filepath.Join(path, "mod.info")) {
		return filepath.Join(path, "mod.info")
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		return ""
	}
	// Prefer the highest numbered build folder, then common.
	best, bestVersion := "", -1
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		candidate := filepath.Join(path, e.Name(), "mod.info")
		if !fileExists(candidate) {
			continue
		}
		version := -1
		if n, err := strconv.Atoi(strings.SplitN(e.Name(), ".", 2)[0]); err == nil {
			version = n
		} else if strings.EqualFold(e.Name(), "common") {
			version = 0
		}
		if version > bestVersion {
			best, bestVersion = candidate, version
		}
	}
	return best
}

func describeMod(path, folder string) Mod {
	// ID falls back to the folder so the mod is still listed, but Declared is
	// left empty unless mod.info really states one: the interface must not
	// claim a folder name is what the mod declares.
	m := Mod{ID: folder, Folder: folder, Path: path, Installed: true}
	if st, err := os.Stat(path); err == nil {
		m.UpdatedAt = st.ModTime()
	}
	infoPath := modInfoPath(path)
	if infoPath == "" {
		return m
	}
	f, err := os.Open(infoPath)
	if err != nil {
		return m
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		eq := strings.Index(line, "=")
		if eq <= 0 {
			continue
		}
		key := strings.ToLower(strings.TrimSpace(line[:eq]))
		value := strings.TrimSpace(line[eq+1:])
		switch key {
		case "name":
			m.Name = value
		case "id":
			if value != "" {
				m.ID = value
				m.Declared = value
			}
		case "description":
			m.Description = value
		case "category":
			m.Category = value
		case "modversion":
			m.Version = value
		case "url":
			m.URL = value
		case "require":
			m.Require = splitModList(value)
		case "loadafter":
			m.LoadAfter = splitModList(value)
		case "loadbefore":
			m.LoadBefore = splitModList(value)
		case "incompatiblemods":
			m.Incompatible = splitModList(value)
		case "loadfirst":
			m.LoadFirst = isOn(value)
		case "loadlast":
			m.LoadLast = isOn(value)
		}
	}
	return m
}

// splitModList reads a comma or semicolon separated list of mod IDs.
func splitModList(v string) []string {
	fields := strings.FieldsFunc(v, func(r rune) bool { return r == ',' || r == ';' })
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		if f = strings.TrimSpace(f); f != "" {
			out = append(out, f)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// isOn reads the loadFirst/loadLast flag, which takes "on", "category" or
// "off" rather than a boolean.
func isOn(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "on", "true", "1", "category":
		return true
	}
	return false
}
