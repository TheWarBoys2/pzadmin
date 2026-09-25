package pz

import (
	"bufio"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Project Zomboid defines every item and vehicle in plain text under
// media/scripts, in blocks like:
//
//	module Base {
//	    item BreadSlices {
//	        DisplayName = Bread Slices,
//	        DisplayCategory = Food,
//	        Type = Food,
//	    }
//	}
//
// Reading those files is better than shipping a list copied from a wiki: it is
// exactly right for the build the server is running, it picks up every modded
// item automatically, and it never goes stale. The module name and the block
// name together form the ID that RCON expects, so "Base.BreadSlices".

// CatalogueEntry is one spawnable thing.
type CatalogueEntry struct {
	// ID is what RCON wants, e.g. "Base.Axe".
	ID string `json:"id"`
	// Name is the human display name where the script provides one.
	Name string `json:"name"`
	// Category groups the entry in the picker: DisplayCategory when the script
	// sets one, otherwise the item Type.
	Category string `json:"category"`
	// Source is "vanilla", a mod folder name, or "custom".
	Source string `json:"source"`
	// Kind is "item" or "vehicle".
	Kind string `json:"kind"`
}

// Catalogue is the result of a scan.
type Catalogue struct {
	Items    []CatalogueEntry `json:"items"`
	Vehicles []CatalogueEntry `json:"vehicles"`
	// Sources lists where entries came from, for the UI to explain itself.
	Sources   []string  `json:"sources"`
	ScannedAt time.Time `json:"scannedAt"`
	// Truncated is set when a scan hit its file budget.
	Truncated bool `json:"truncated"`
	// Scanned records every directory looked at and what came out of it. When
	// something is missing from the pickers this is the only way to tell
	// whether the files were not found, not parsed, or not there.
	Scanned []ScanPath `json:"scanned"`
}

// ScanPath is one directory the scanner looked at.
type ScanPath struct {
	Path     string `json:"path"`
	Source   string `json:"source"`
	Files    int    `json:"files"`
	Items    int    `json:"items"`
	Vehicles int    `json:"vehicles"`
	Exists   bool   `json:"exists"`
	// Empty counts files that were read but yielded no blocks at all. Plenty of
	// script files legitimately hold no items, so this is context rather than a
	// fault, but "1004 files read, 0 items" is unambiguous.
	Empty int `json:"empty"`
	// SampleFile and Sample are filled in when a directory held script files
	// but nothing was recognised in them. Without this, "no items" cannot be
	// told apart from "the parser did not understand the format", and only the
	// person with the files can settle it.
	SampleFile string `json:"sampleFile,omitempty"`
	Sample     string `json:"sample,omitempty"`
}

// Categories returns the distinct categories present, sorted, for a given kind.
func (c Catalogue) Categories(kind string) []string {
	seen := map[string]bool{}
	list := c.Items
	if kind == "vehicle" {
		list = c.Vehicles
	}
	for _, e := range list {
		if e.Category != "" {
			seen[e.Category] = true
		}
	}
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// ScriptScanner reads and caches script catalogues.
type ScriptScanner struct {
	mu    sync.Mutex
	cache map[string]*scriptCacheEntry
	ttl   time.Duration
}

type scriptCacheEntry struct {
	catalogue Catalogue
	signature string
	expires   time.Time
}

// NewScriptScanner returns a scanner with the given cache lifetime.
func NewScriptScanner(ttl time.Duration) *ScriptScanner {
	if ttl <= 0 {
		ttl = 30 * time.Minute
	}
	return &ScriptScanner{cache: map[string]*scriptCacheEntry{}, ttl: ttl}
}

// maxScriptFiles bounds a scan. A vanilla install has a few hundred script
// files; a heavily modded server can have thousands, and this stops a runaway
// directory from stalling a request.
const maxScriptFiles = 40000

// Scan reads every script directory it can find. gameRoot is the Project
// Zomboid installation (the directory containing media/scripts); it may be
// empty, in which case only mod scripts are read.
func (s *ScriptScanner) Scan(gameRoot string, modDirs []string) Catalogue {
	key := gameRoot + "\x00" + strings.Join(modDirs, "\x00")
	sig := scriptSignature(gameRoot, modDirs)

	s.mu.Lock()
	if e, ok := s.cache[key]; ok && e.signature == sig && time.Now().Before(e.expires) {
		out := e.catalogue
		s.mu.Unlock()
		return out
	}
	s.mu.Unlock()

	cat := scanScripts(gameRoot, modDirs)

	s.mu.Lock()
	s.cache[key] = &scriptCacheEntry{catalogue: cat, signature: sig, expires: time.Now().Add(s.ttl)}
	s.mu.Unlock()
	return cat
}

// Invalidate drops any cached scan.
func (s *ScriptScanner) Invalidate() {
	s.mu.Lock()
	s.cache = map[string]*scriptCacheEntry{}
	s.mu.Unlock()
}

func scriptSignature(gameRoot string, modDirs []string) string {
	var b strings.Builder
	stamp := func(p string) {
		st, err := os.Stat(p)
		if err != nil {
			return
		}
		b.WriteString(p)
		b.WriteByte(':')
		// Nanosecond precision matters: two edits in the same second would look
		// identical at RFC3339's one-second resolution and the cache would
		// serve a stale scan.
		b.WriteString(st.ModTime().UTC().Format(time.RFC3339Nano))
		b.WriteByte('|')
	}
	if gameRoot != "" {
		stamp(filepath.Join(gameRoot, "media", "scripts"))
	}
	for _, d := range modDirs {
		stamp(d)
	}
	return b.String()
}

func scanScripts(gameRoot string, modDirs []string) Catalogue {
	cat := Catalogue{ScannedAt: time.Now()}
	budget := maxScriptFiles

	items := map[string]CatalogueEntry{}
	vehicles := map[string]CatalogueEntry{}

	absorb := func(dir, source string) {
		record := ScanPath{Path: dir, Source: source}
		if dir == "" {
			return
		}
		if st, err := os.Stat(dir); err != nil || !st.IsDir() {
			cat.Scanned = append(cat.Scanned, record)
			return
		}
		record.Exists = true
		firstFile := ""
		_ = filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if budget <= 0 {
				cat.Truncated = true
				return filepath.SkipAll
			}
			if d.IsDir() || !strings.HasSuffix(strings.ToLower(d.Name()), ".txt") {
				return nil
			}
			budget--
			record.Files++
			blocks := parseScriptFile(path, source)
			if len(blocks) == 0 {
				record.Empty++
				if firstFile == "" {
					firstFile = path
				}
			}
			for _, e := range blocks {
				if e.Kind == "vehicle" {
					if _, exists := vehicles[e.ID]; !exists {
						vehicles[e.ID] = e
						record.Vehicles++
					}
				} else if _, exists := items[e.ID]; !exists {
					items[e.ID] = e
					record.Items++
				}
			}
			return nil
		})
		if record.Files > 0 && record.Items == 0 && record.Vehicles == 0 && firstFile != "" {
			record.SampleFile = filepath.Base(firstFile)
			record.Sample = headOf(firstFile, 25)
		}
		cat.Scanned = append(cat.Scanned, record)
		if record.Files > 0 {
			cat.Sources = append(cat.Sources, source)
		}
	}

	if gameRoot != "" {
		absorb(filepath.Join(gameRoot, "media", "scripts"), "vanilla")
	}

	// Mods carry their scripts in more than one place. Build 41 puts them at
	// <mod>/media/scripts. Build 42 splits a mod into a shared common/ folder
	// and one folder per build (42, 42.13, ...), each with its own media/, so
	// a Build 42 mod has nothing at all under <mod>/media and looking only
	// there found none of its items.
	for _, dir := range modDirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			base := filepath.Join(dir, entry.Name())
			absorbMod(base, entry.Name(), absorb)
			// Workshop layout: <workshopID>/mods/<modID>/...
			if nested, err := os.ReadDir(filepath.Join(base, "mods")); err == nil {
				for _, n := range nested {
					if n.IsDir() {
						absorbMod(filepath.Join(base, "mods", n.Name()), n.Name(), absorb)
					}
				}
			}
		}
	}

	for _, e := range items {
		cat.Items = append(cat.Items, e)
	}
	for _, e := range vehicles {
		cat.Vehicles = append(cat.Vehicles, e)
	}
	sortEntries(cat.Items)
	sortEntries(cat.Vehicles)
	sort.Strings(cat.Sources)
	cat.Sources = dedupe(cat.Sources)
	return cat
}

// absorbMod walks every place a single mod can keep its scripts.
func absorbMod(modPath, source string, absorb func(dir, source string)) {
	// Build 41 layout, and the legacy folder a dual-build mod keeps.
	absorb(filepath.Join(modPath, "media", "scripts"), source)
	// Build 42: shared assets plus one folder per build number.
	absorb(filepath.Join(modPath, "common", "media", "scripts"), source)

	entries, err := os.ReadDir(modPath)
	if err != nil {
		return
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		// Build folders are named after the build: 42, 42.13, 41.
		if name == "" || name[0] < '0' || name[0] > '9' {
			continue
		}
		absorb(filepath.Join(modPath, name, "media", "scripts"), source)
	}
}

func sortEntries(list []CatalogueEntry) {
	sort.Slice(list, func(i, j int) bool {
		if list[i].Category != list[j].Category {
			return list[i].Category < list[j].Category
		}
		a, b := list[i].Name, list[j].Name
		if a == "" {
			a = list[i].ID
		}
		if b == "" {
			b = list[j].ID
		}
		return strings.ToLower(a) < strings.ToLower(b)
	})
}

func dedupe(in []string) []string {
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

// parseScriptFile extracts item and vehicle blocks from one script file.
//
// The format is brace-delimited and line-oriented. Braces sit on their own line
// as often as they sit on the header, so a header is remembered until its
// opening brace arrives. Nested blocks (a vehicle's model, an item's sub-table)
// are counted by depth and ignored.
//
// This deliberately understands only the block type, the block name, the
// enclosing module, and the two fields the picker needs.
func parseScriptFile(path, source string) []CatalogueEntry {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()

	// Skip anything implausibly large; script files are small.
	if st, err := f.Stat(); err == nil && st.Size() > 8<<20 {
		return nil
	}

	var out []CatalogueEntry
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 2*1024*1024)

	var (
		module      string
		pending     string
		blockKind   string
		blockName   string
		displayName string
		category    string
		itemType    string
		depth       int
		blockDepth  int
		inBlock     bool
	)

	flush := func() {
		if !inBlock || blockName == "" {
			return
		}
		mod := module
		if mod == "" {
			mod = "Base"
		}
		// A block name may already be fully qualified, in which case the module
		// must not be prefixed again.
		fullID := mod + "." + blockName
		if strings.Contains(blockName, ".") {
			fullID = blockName
		}
		kind := "item"
		if blockKind == "vehicle" {
			kind = "vehicle"
		}
		cat := category
		if cat == "" {
			cat = itemType
		}
		if cat == "" {
			cat = "Uncategorised"
		}
		if kind == "vehicle" {
			cat = "Vehicles"
		}
		out = append(out, CatalogueEntry{
			ID: fullID, Name: displayName,
			Category: cat, Source: source, Kind: kind,
		})
	}

	// openBlock applies a remembered header to the brace that just opened.
	openBlock := func(header string) {
		depth++
		fields := strings.Fields(header)
		if len(fields) < 2 {
			return
		}
		switch strings.ToLower(fields[0]) {
		case "module":
			if depth == 1 {
				module = fields[1]
			}
		case "item", "vehicle":
			// Normally a block sits inside a module, so at depth 2. Some files
			// declare blocks with no module wrapper at all, in which case the
			// block is at depth 1 and the module is implicitly Base.
			if !inBlock && (depth == 2 || (depth == 1 && module == "")) {
				blockKind = strings.ToLower(fields[0])
				blockName = strings.Join(fields[1:], " ")
				displayName, category, itemType = "", "", ""
				inBlock = true
				blockDepth = depth
			}
		}
	}

	closeBlock := func() {
		if inBlock && depth == blockDepth {
			flush()
			inBlock = false
			blockName, blockKind = "", ""
		}
		depth--
		if depth <= 0 {
			depth = 0
			module = ""
			inBlock = false
		}
	}

	for sc.Scan() {
		line := sc.Text()
		if i := strings.Index(line, "//"); i >= 0 {
			line = line[:i]
		}
		if i := strings.Index(line, "/*"); i >= 0 {
			line = line[:i]
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		// Walk the line so that "module Base { item X {" behaves the same as
		// the same text spread over four lines.
		segment := strings.Builder{}
		for _, r := range line {
			switch r {
			case '{':
				header := strings.TrimSpace(segment.String())
				segment.Reset()
				if header == "" {
					header = pending
				}
				pending = ""
				openBlock(header)
			case '}':
				text := strings.TrimSpace(segment.String())
				segment.Reset()
				if inBlock && text != "" {
					if key, value, ok := scriptField(text); ok {
						applyField(key, value, &displayName, &category, &itemType)
					}
				}
				pending = ""
				closeBlock()
			default:
				segment.WriteRune(r)
			}
		}

		rest := strings.TrimSpace(segment.String())
		if rest == "" {
			continue
		}
		if key, value, ok := scriptField(rest); ok {
			if inBlock {
				applyField(key, value, &displayName, &category, &itemType)
			}
			pending = ""
			continue
		}
		// Not a field, so it is a header waiting for its opening brace.
		pending = rest
	}
	return out
}

func applyField(key, value string, displayName, category, itemType *string) {
	switch strings.ToLower(key) {
	case "displayname":
		*displayName = value
	case "displaycategory":
		*category = value
	case "type":
		*itemType = value
	}
}

// scriptField splits "Key = Value," into its parts.
func scriptField(line string) (string, string, bool) {
	eq := strings.Index(line, "=")
	if eq <= 0 {
		return "", "", false
	}
	key := strings.TrimSpace(line[:eq])
	value := strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(line[eq+1:]), ","))
	if key == "" || strings.ContainsAny(key, "{}") {
		return "", "", false
	}
	return key, value, true
}

// DetectGameRoot looks for a Project Zomboid installation at or below base, so
// the operator usually does not have to type a path.
//
// This searches exactly the same places the mod scan searches, and that is the
// point. Before v23 it looked only at base and its immediate children while
// mod directories were found through candidateRoots, which walks a level
// deeper. A tree like <server>/projectzomboid/data/ therefore yielded every
// mod and no vanilla items at all: the pickers filled up with modded content
// and silently omitted the base game. Any change to one search must be made to
// the other.
func DetectGameRoot(base string) string {
	if base == "" {
		return ""
	}
	for _, dir := range gameRootCandidates(base) {
		if isDir(filepath.Join(dir, "media", "scripts")) {
			return dir
		}
	}
	return ""
}

// gameRootCandidates lists, in priority order, every directory that could be
// the root of an installation. Order matters: an exact hit on base must win
// over a nested one, or a server directory that happens to contain a second
// copy of the game could be preferred over the real one.
func gameRootCandidates(base string) []string {
	base = filepath.Clean(base)
	seen := map[string]bool{}
	var out []string
	add := func(p string) {
		p = filepath.Clean(p)
		if seen[p] {
			return
		}
		seen[p] = true
		out = append(out, p)
	}

	// Phase one: every root the layout detector considers, which covers base
	// itself and the config/, data/, projectzomboid/ nesting Docker images
	// use. Shallowest first.
	roots := candidateRoots(base)
	for _, root := range roots {
		add(root)
	}

	// Phase two: any immediate child of base, which catches an install folder
	// with a name nobody anticipated.
	if entries, err := os.ReadDir(base); err == nil {
		for _, e := range entries {
			if e.IsDir() {
				add(filepath.Join(base, e.Name()))
			}
		}
	}

	// Phase three: Steam library layouts. The install sits under
	// steamapps/common/<title>, and the title has changed spelling between
	// releases, so read the directory rather than guessing the name. Last,
	// because a server that also carries a Steam library should still prefer
	// its own install.
	for _, root := range roots {
		for _, lib := range []string{
			filepath.Join(root, "steamapps", "common"),
			filepath.Join(root, "Steam", "steamapps", "common"),
		} {
			entries, err := os.ReadDir(lib)
			if err != nil {
				continue
			}
			for _, e := range entries {
				if e.IsDir() {
					add(filepath.Join(lib, e.Name()))
				}
			}
		}
	}
	return out
}

// headOf returns the first n non-empty lines of a file, for the scan report.
func headOf(path string, n int) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 8192), 128*1024)
	var out []string
	for sc.Scan() && len(out) < n {
		line := strings.TrimRight(sc.Text(), " \t")
		if strings.TrimSpace(line) == "" {
			continue
		}
		if len(line) > 200 {
			line = line[:200] + "…"
		}
		out = append(out, line)
	}
	return strings.Join(out, "\n")
}
