package pz

import (
	"fmt"
	"sort"
	"strings"
)

// Changing a mod list is the single most destructive routine operation on a
// Project Zomboid server, and almost none of the damage shows up when the
// change is made. The ini accepts anything. The server starts. Then, hours
// later, a mod that quietly lost its dependency throws Lua errors mid-session,
// or an orphaned copy of a mod on disk wins the load and the world stops
// matching what anyone configured.
//
// Preflight compares a proposed mod list against what is actually on disk and
// what is configured today, and says what will go wrong before it is written.
// It never edits anything.

// Issue levels. Block does not prevent a change: PZAdmin has no business
// refusing to write a file the operator asked for. It means the interface must
// take an explicit confirmation first, so that "I broke it and I do not know
// when" becomes "I was told and I chose to".
const (
	LevelBlock = "block"
	LevelWarn  = "warn"
	LevelInfo  = "info"
)

// ModIssue is one finding.
type ModIssue struct {
	Level string `json:"level"`
	// Code is stable and machine-readable, for tests and for the interface to
	// group by. The message is for people and may be reworded freely.
	Code    string `json:"code"`
	Mod     string `json:"mod,omitempty"`
	Message string `json:"message"`
	// Paths are directories the operator may want to deal with by hand.
	// PZAdmin never deletes mod files itself.
	Paths []string `json:"paths,omitempty"`
}

// ModPlan is a proposed change to a server's mod configuration.
type ModPlan struct {
	BeforeMods     []string
	BeforeWorkshop []string
	AfterMods      []string
	AfterWorkshop  []string
}

// Blocking reports whether any issue needs an explicit confirmation.
func Blocking(issues []ModIssue) bool {
	for _, i := range issues {
		if i.Level == LevelBlock {
			return true
		}
	}
	return false
}

// modIndex resolves the several names one mod answers to.
//
// The Mods= line may spell a mod by the id= from its mod.info or by the folder
// it happens to live in, and for a Workshop item holding several mods those
// are usually different strings. A dependency declared in mod.info uses the
// author's spelling, which is a third possibility. Every lookup here goes
// through the same index so that all three resolve to one mod.
type modIndex struct {
	byKey map[string]Mod
	all   []Mod
}

func newModIndex(mods []Mod) *modIndex {
	ix := &modIndex{byKey: map[string]Mod{}, all: mods}
	for _, m := range mods {
		if !m.Installed {
			continue
		}
		for _, key := range []string{m.ID, m.Declared, m.Folder} {
			key = normKey(key)
			if key == "" {
				continue
			}
			if _, seen := ix.byKey[key]; !seen {
				ix.byKey[key] = m
			}
		}
	}
	return ix
}

func (ix *modIndex) lookup(name string) (Mod, bool) {
	m, ok := ix.byKey[normKey(name)]
	return m, ok
}

// aliases returns every name a mod ID could legitimately be written as.
func (ix *modIndex) aliases(name string) []string {
	out := []string{normKey(name)}
	if m, ok := ix.lookup(name); ok {
		for _, k := range []string{m.ID, m.Declared, m.Folder} {
			if k := normKey(k); k != "" {
				out = append(out, k)
			}
		}
	}
	return out
}

func normKey(s string) string { return strings.ToLower(strings.TrimSpace(s)) }

// Preflight returns everything questionable about a proposed mod change.
//
// Findings are ordered by severity so the interface can show the worst first
// without sorting them itself.
func Preflight(report ModReport, plan ModPlan) []ModIssue {
	ix := newModIndex(report.Mods)

	after := map[string]bool{}
	for _, id := range plan.AfterMods {
		for _, alias := range ix.aliases(id) {
			after[alias] = true
		}
	}
	before := map[string]bool{}
	for _, id := range plan.BeforeMods {
		for _, alias := range ix.aliases(id) {
			before[alias] = true
		}
	}

	afterWorkshop := map[string]bool{}
	for _, id := range plan.AfterWorkshop {
		afterWorkshop[normKey(id)] = true
	}
	beforeWorkshop := map[string]bool{}
	for _, id := range plan.BeforeWorkshop {
		beforeWorkshop[normKey(id)] = true
	}

	var issues []ModIssue
	add := func(i ModIssue) { issues = append(issues, i) }

	// Removed mods, in the spelling the operator used, for the messages below.
	var removed []string
	for _, id := range plan.BeforeMods {
		if !after[normKey(id)] {
			removed = append(removed, id)
		}
	}

	checkDependencies(ix, plan, after, add)
	checkIncompatible(ix, plan, add)
	checkDuplicateIDs(report, ix, plan, add)
	checkNotInstalled(ix, plan, afterWorkshop, add)
	checkLoadOrder(ix, plan, add)
	checkWorkshopDrift(ix, plan, removed, after, afterWorkshop, beforeWorkshop, add)
	checkOrphanFiles(ix, removed, add)
	checkSaveData(removed, add)

	sort.SliceStable(issues, func(i, j int) bool {
		return levelRank(issues[i].Level) < levelRank(issues[j].Level)
	})
	return issues
}

func levelRank(level string) int {
	switch level {
	case LevelBlock:
		return 0
	case LevelWarn:
		return 1
	default:
		return 2
	}
}

// checkDependencies finds mods that will be left requiring something that is
// no longer enabled. This is the failure that does not announce itself: the
// server starts perfectly and then throws Lua errors once something touches
// the missing code.
func checkDependencies(ix *modIndex, plan ModPlan, after map[string]bool, add func(ModIssue)) {
	for _, id := range plan.AfterMods {
		m, ok := ix.lookup(id)
		if !ok {
			continue
		}
		for _, req := range m.Require {
			if normKey(req) == "" || after[normKey(req)] {
				continue
			}
			add(ModIssue{
				Level: LevelBlock, Code: "dependency-missing", Mod: id,
				Message: fmt.Sprintf(
					"%s requires %s, which will not be in the mod list. The server will still "+
						"start; the errors appear later, in play.", displayName(m, id), req),
			})
		}
	}
}

// checkIncompatible finds pairs the authors have declared cannot coexist.
func checkIncompatible(ix *modIndex, plan ModPlan, add func(ModIssue)) {
	after := map[string]string{} // alias -> the spelling used in the list
	for _, id := range plan.AfterMods {
		for _, alias := range ix.aliases(id) {
			if _, seen := after[alias]; !seen {
				after[alias] = id
			}
		}
	}
	reported := map[string]bool{}
	for _, id := range plan.AfterMods {
		m, ok := ix.lookup(id)
		if !ok {
			continue
		}
		for _, bad := range m.Incompatible {
			other, present := after[normKey(bad)]
			if !present || normKey(other) == normKey(id) {
				continue
			}
			// One complaint per pair, whichever side declares it.
			pair := []string{normKey(id), normKey(other)}
			sort.Strings(pair)
			key := strings.Join(pair, "|")
			if reported[key] {
				continue
			}
			reported[key] = true
			add(ModIssue{
				Level: LevelBlock, Code: "incompatible", Mod: id,
				Message: fmt.Sprintf("%s declares it is incompatible with %s, and both are enabled.",
					displayName(m, id), other),
			})
		}
	}
}

// checkDuplicateIDs finds two installed folders claiming the same mod ID.
//
// Project Zomboid loads whichever it reaches first. An old copy left behind
// after a mod moved Workshop items will therefore silently win, and nothing in
// the game or the config says so.
func checkDuplicateIDs(report ModReport, ix *modIndex, plan ModPlan, add func(ModIssue)) {
	// Both lists are needed. ix.all holds one entry per mod, so a second
	// folder claiming the same ID is not in it — that is exactly what
	// Duplicates carries. Reading only one of them finds nothing.
	byDeclared := map[string][]Mod{}
	seenPath := map[string]bool{}
	for _, m := range append(append([]Mod{}, ix.all...), report.Duplicates...) {
		if !m.Installed || normKey(m.Declared) == "" || m.Path == "" {
			continue
		}
		if seenPath[m.Path] {
			continue
		}
		seenPath[m.Path] = true
		byDeclared[normKey(m.Declared)] = append(byDeclared[normKey(m.Declared)], m)
	}

	enabled := map[string]bool{}
	for _, id := range plan.AfterMods {
		enabled[normKey(id)] = true
	}

	keys := make([]string, 0, len(byDeclared))
	for k := range byDeclared {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	for _, k := range keys {
		group := byDeclared[k]
		if len(group) < 2 {
			continue
		}
		paths := make([]string, 0, len(group))
		for _, m := range group {
			if m.Path != "" {
				paths = append(paths, m.Path)
			}
		}
		sort.Strings(paths)

		level := LevelWarn
		if enabled[k] {
			level = LevelBlock
		}
		add(ModIssue{
			Level: level, Code: "duplicate-id", Mod: group[0].Declared, Paths: paths,
			Message: fmt.Sprintf(
				"%d installed folders all declare the mod ID %s. Project Zomboid loads whichever "+
					"it finds first, so which one you get is not something the config decides. "+
					"Remove the copies you do not want.", len(group), group[0].Declared),
		})
	}
}

// checkNotInstalled reports enabled mods with no folder on disk.
//
// This is deliberately not blocking. Adding a Workshop item and its mod ID
// together, then restarting so steamcmd downloads it, is the normal way to
// install a mod, and the files genuinely are absent at the moment the config
// is written.
func checkNotInstalled(ix *modIndex, plan ModPlan, afterWorkshop map[string]bool, add func(ModIssue)) {
	for _, id := range plan.AfterMods {
		if _, ok := ix.lookup(id); ok {
			continue
		}
		if len(afterWorkshop) > 0 {
			add(ModIssue{
				Level: LevelInfo, Code: "not-installed-yet", Mod: id,
				Message: fmt.Sprintf(
					"%s is not on disk yet. If it belongs to one of the Workshop items listed, "+
						"it downloads on the next start.", id),
			})
			continue
		}
		add(ModIssue{
			Level: LevelWarn, Code: "not-installed", Mod: id,
			Message: fmt.Sprintf(
				"%s is not installed and no Workshop items are listed, so nothing will fetch it. "+
					"The server will refuse to start.", id),
		})
	}
}

// checkLoadOrder finds mods listed before something they must load after.
func checkLoadOrder(ix *modIndex, plan ModPlan, add func(ModIssue)) {
	position := map[string]int{}
	for i, id := range plan.AfterMods {
		for _, alias := range ix.aliases(id) {
			if _, seen := position[alias]; !seen {
				position[alias] = i
			}
		}
	}

	for i, id := range plan.AfterMods {
		m, ok := ix.lookup(id)
		if !ok {
			continue
		}
		earlier := append(append([]string{}, m.Require...), m.LoadAfter...)
		for _, dep := range earlier {
			at, present := position[normKey(dep)]
			if !present || at < i {
				continue
			}
			add(ModIssue{
				Level: LevelWarn, Code: "load-order", Mod: id,
				Message: fmt.Sprintf(
					"%s is listed before %s but has to load after it. Use Sort to fix the order.",
					displayName(m, id), dep),
			})
		}
		for _, dep := range m.LoadBefore {
			at, present := position[normKey(dep)]
			if !present || at > i {
				continue
			}
			add(ModIssue{
				Level: LevelWarn, Code: "load-order", Mod: id,
				Message: fmt.Sprintf(
					"%s is listed after %s but has to load before it. Use Sort to fix the order.",
					displayName(m, id), dep),
			})
		}
	}
}

// checkWorkshopDrift finds the two halves of the configuration disagreeing.
//
// Mods= and WorkshopItems= are edited as one thought and stored as two lines,
// and the failures from letting them drift apart are both silent and opposite:
// a stale Workshop ID keeps re-downloading files you deleted, and a missing
// one deletes files the mod list still needs.
func checkWorkshopDrift(
	ix *modIndex,
	plan ModPlan,
	removed []string,
	after map[string]bool,
	afterWorkshop, beforeWorkshop map[string]bool,
	add func(ModIssue),
) {
	// A removed mod whose Workshop item is still listed.
	seen := map[string]bool{}
	for _, id := range removed {
		m, ok := ix.lookup(id)
		if !ok || m.WorkshopID == "" {
			continue
		}
		if !afterWorkshop[normKey(m.WorkshopID)] || seen[m.WorkshopID] {
			continue
		}
		// Another mod from the same Workshop item may still be in use, in
		// which case the item belongs in the list and there is nothing wrong.
		stillUsed := false
		for _, other := range ix.all {
			if other.WorkshopID != m.WorkshopID || normKey(other.ID) == normKey(m.ID) {
				continue
			}
			if after[normKey(other.ID)] || after[normKey(other.Declared)] || after[normKey(other.Folder)] {
				stillUsed = true
				break
			}
		}
		if stillUsed {
			continue
		}
		seen[m.WorkshopID] = true
		add(ModIssue{
			Level: LevelWarn, Code: "workshop-orphan", Mod: id,
			Message: fmt.Sprintf(
				"Workshop item %s is still listed but nothing from it is enabled any more. "+
					"steamcmd keeps downloading it, so deleting the folder will not make it stay "+
					"deleted. Remove the Workshop ID as well.", m.WorkshopID),
		})
	}

	// A Workshop item dropped while one of its mods stays enabled.
	for id := range beforeWorkshop {
		if afterWorkshop[id] {
			continue
		}
		for _, m := range ix.all {
			if normKey(m.WorkshopID) != id {
				continue
			}
			if !after[normKey(m.ID)] && !after[normKey(m.Declared)] && !after[normKey(m.Folder)] {
				continue
			}
			add(ModIssue{
				Level: LevelWarn, Code: "workshop-removed", Mod: m.ID,
				Message: fmt.Sprintf(
					"%s is still enabled but Workshop item %s has been removed from the list. "+
						"Its files disappear the next time the server updates and it will then "+
						"refuse to start.", displayName(m, m.ID), m.WorkshopID),
			})
			break
		}
	}
}

// checkOrphanFiles points at directories left behind by a removal, and gives
// the ordering that makes deleting them stick.
func checkOrphanFiles(ix *modIndex, removed []string, add func(ModIssue)) {
	var paths []string
	var names []string
	for _, id := range removed {
		m, ok := ix.lookup(id)
		if !ok || m.Path == "" {
			continue
		}
		paths = append(paths, m.Path)
		names = append(names, id)
	}
	if len(paths) == 0 {
		return
	}
	sort.Strings(paths)
	add(ModIssue{
		Level: LevelInfo, Code: "files-remain", Paths: paths,
		Message: fmt.Sprintf(
			"The files for %s stay on disk. That is usually fine, but if you mean to delete "+
				"them, save this change first so the Workshop IDs are gone, restart, and only "+
				"then remove the folders. Delete them first and steamcmd downloads them again.",
			strings.Join(names, ", ")),
	})
}

// checkSaveData states the one thing no tool can fix.
func checkSaveData(removed []string, add func(ModIssue)) {
	if len(removed) == 0 {
		return
	}
	add(ModIssue{
		Level: LevelInfo, Code: "save-data",
		Message: "Items, vehicles and world objects from a removed mod are already written into " +
			"the save. Removing the mod does not remove them, and what happens to them ranges " +
			"from disappearing quietly to breaking the chunk they are in. Take a backup first.",
	})
}

func displayName(m Mod, fallback string) string {
	if strings.TrimSpace(m.Name) != "" {
		return m.Name
	}
	return fallback
}
