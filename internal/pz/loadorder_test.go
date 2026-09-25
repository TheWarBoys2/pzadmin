package pz

import (
	"path/filepath"
	"strings"
	"testing"
)

func mods(list ...Mod) map[string]Mod {
	out := map[string]Mod{}
	for _, m := range list {
		out[m.ID] = m
	}
	return out
}

func TestSortPlacesDependenciesFirst(t *testing.T) {
	// A common real shape: a library everything needs, added last by hand.
	order := []string{"FancyHair", "TrueActionsDancing", "Tsarslib"}
	info := mods(
		Mod{ID: "Tsarslib"},
		Mod{ID: "TrueActionsDancing", Require: []string{"Tsarslib"}},
		Mod{ID: "FancyHair", Require: []string{"Tsarslib"}},
	)
	report := SortLoadOrder(order, info, nil)

	if !report.Changed {
		t.Fatal("the library has to move ahead of its dependants")
	}
	if report.Order[0] != "Tsarslib" {
		t.Fatalf("expected Tsarslib first, got %v", report.Order)
	}
	if len(report.Order) != 3 {
		t.Fatalf("nothing may be dropped: %v", report.Order)
	}
	// The two dependants had no constraint between them, so their relative
	// order should be exactly what the operator had.
	if indexOf(report.Order, "FancyHair") > indexOf(report.Order, "TrueActionsDancing") {
		t.Fatalf("unconstrained mods should keep their relative order: %v", report.Order)
	}
}

func TestSortLeavesAWorkingListAlone(t *testing.T) {
	order := []string{"Tsarslib", "TrueActionsDancing", "FancyHair"}
	info := mods(
		Mod{ID: "Tsarslib"},
		Mod{ID: "TrueActionsDancing", Require: []string{"Tsarslib"}},
		Mod{ID: "FancyHair", Require: []string{"Tsarslib"}},
	)
	report := SortLoadOrder(order, info, nil)
	if report.Changed {
		t.Fatalf("an already valid order should not be reshuffled: %v", report.Order)
	}
}

func TestSortWithNoRulesIsANoOp(t *testing.T) {
	order := []string{"C", "A", "B"}
	report := SortLoadOrder(order, mods(Mod{ID: "A"}, Mod{ID: "B"}, Mod{ID: "C"}), nil)
	if report.Changed {
		t.Fatalf("nothing declared any order, so nothing should move: %v", report.Order)
	}
	if report.Rules != 0 {
		t.Fatalf("no constraints should have been applied, got %d", report.Rules)
	}
}

func TestLoadAfterAndLoadBefore(t *testing.T) {
	order := []string{"Patch", "Base", "UI"}
	info := mods(
		Mod{ID: "Base"},
		Mod{ID: "Patch", LoadAfter: []string{"Base"}},
		Mod{ID: "UI", LoadBefore: []string{"Patch"}},
	)
	report := SortLoadOrder(order, info, nil)
	if indexOf(report.Order, "Base") > indexOf(report.Order, "Patch") {
		t.Fatalf("loadAfter ignored: %v", report.Order)
	}
	if indexOf(report.Order, "UI") > indexOf(report.Order, "Patch") {
		t.Fatalf("loadBefore ignored: %v", report.Order)
	}
}

func TestLoadFirstAndLoadLast(t *testing.T) {
	order := []string{"A", "Core", "B", "Translations"}
	info := mods(
		Mod{ID: "Core", LoadFirst: true},
		Mod{ID: "Translations", LoadLast: true},
		Mod{ID: "A"}, Mod{ID: "B"},
	)
	report := SortLoadOrder(order, info, nil)
	if report.Order[0] != "Core" {
		t.Fatalf("loadFirst should win: %v", report.Order)
	}
	if report.Order[len(report.Order)-1] != "Translations" {
		t.Fatalf("loadLast should win: %v", report.Order)
	}
}

func TestMissingRequirementIsReportedNotInvented(t *testing.T) {
	order := []string{"TrueActionsDancing"}
	info := mods(Mod{ID: "TrueActionsDancing", Require: []string{"Tsarslib"}})
	report := SortLoadOrder(order, info, nil)

	found := false
	for _, issue := range report.Issues {
		if issue.Kind == "missing" && issue.Other == "Tsarslib" {
			found = true
			if !strings.Contains(issue.Detail, "Tsarslib") {
				t.Fatal("the message should name the missing mod")
			}
		}
	}
	if !found {
		t.Fatalf("a missing requirement must be reported: %#v", report.Issues)
	}
	if len(report.Order) != 1 {
		t.Fatal("the mod itself must not be dropped")
	}
}

// A dependency loop has no valid order. It has to be reported rather than
// silently producing an arbitrary one or losing mods.
func TestCycleIsReportedAndNothingIsLost(t *testing.T) {
	order := []string{"A", "B", "C"}
	info := mods(
		Mod{ID: "A", LoadAfter: []string{"B"}},
		Mod{ID: "B", LoadAfter: []string{"A"}},
		Mod{ID: "C"},
	)
	report := SortLoadOrder(order, info, nil)

	if len(report.Order) != 3 {
		t.Fatalf("all mods must survive a cycle: %v", report.Order)
	}
	cycle := false
	for _, issue := range report.Issues {
		if issue.Kind == "cycle" {
			cycle = true
			if !strings.Contains(issue.Detail, "A") || !strings.Contains(issue.Detail, "B") {
				t.Fatalf("the cycle should name its members: %s", issue.Detail)
			}
		}
	}
	if !cycle {
		t.Fatalf("a dependency loop must be reported: %#v", report.Issues)
	}
}

func TestIncompatibleModsAreFlagged(t *testing.T) {
	order := []string{"FastMode", "SlowMode"}
	info := mods(
		Mod{ID: "FastMode", Incompatible: []string{"SlowMode"}},
		Mod{ID: "SlowMode"},
	)
	report := SortLoadOrder(order, info, nil)
	found := false
	for _, issue := range report.Issues {
		if issue.Kind == "incompatible" {
			found = true
		}
	}
	if !found {
		t.Fatalf("a declared conflict must be surfaced: %#v", report.Issues)
	}
}

func TestDuplicateEntriesAreFlagged(t *testing.T) {
	report := SortLoadOrder([]string{"A", "B", "A"}, mods(Mod{ID: "A"}, Mod{ID: "B"}), nil)
	found := false
	for _, issue := range report.Issues {
		if issue.Kind == "duplicate" {
			found = true
		}
	}
	if !found {
		t.Fatalf("a repeated mod must be flagged: %#v", report.Issues)
	}
}

func TestSortIsCaseInsensitiveOnModIDs(t *testing.T) {
	order := []string{"truactionsdancing", "TsarsLib"}
	info := mods(
		Mod{ID: "TsarsLib"},
		Mod{ID: "truactionsdancing", Require: []string{"tsarslib"}},
	)
	report := SortLoadOrder(order, info, nil)
	if report.Order[0] != "TsarsLib" {
		t.Fatalf("casing drift in the ini should not defeat the sort: %v", report.Order)
	}
	// The operator's own spelling must be preserved in the output.
	if report.Order[1] != "truactionsdancing" {
		t.Fatalf("the original spelling should be kept: %v", report.Order)
	}
}

func TestExternalSortingRulesAreApplied(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sorting_rules.txt")
	writeFile(t, path, strings.Join([]string{
		"[LatePatch]",
		"loadAfter = EarlyBase, Middle",
		"[EarlyBase]",
		"loadFirst = on",
		"[Broken]",
		"incompatibleMods=Middle",
	}, "\n"))

	rules := LoadSortingRules(path)
	if len(rules) != 3 {
		t.Fatalf("expected three rule blocks, got %d", len(rules))
	}
	if got := rules["latepatch"].LoadAfter; len(got) != 2 || got[0] != "EarlyBase" {
		t.Fatalf("loadAfter not parsed: %#v", got)
	}
	if !rules["earlybase"].LoadFirst {
		t.Fatal("loadFirst not parsed")
	}

	order := []string{"LatePatch", "Middle", "EarlyBase"}
	report := SortLoadOrder(order, mods(
		Mod{ID: "LatePatch"}, Mod{ID: "Middle"}, Mod{ID: "EarlyBase"}), rules)
	if report.Order[0] != "EarlyBase" {
		t.Fatalf("external rules should apply: %v", report.Order)
	}
	if indexOf(report.Order, "LatePatch") < indexOf(report.Order, "Middle") {
		t.Fatalf("LatePatch should follow Middle: %v", report.Order)
	}
}

func TestModInfoOrderingFieldsAreRead(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "mod.info"), strings.Join([]string{
		"name=Fancy Hair",
		"id=FancyHair",
		"description=Adds hair",
		"category=clothes",
		"modversion=1.2",
		"require=Tsarslib,OtherLib",
		"loadAfter=Tsarslib",
		"loadBefore=SomePatch",
		"incompatibleMods=RivalHair",
		"loadLast=on",
	}, "\n"))

	m := describeMod(dir, "FancyHair")
	if m.Name != "Fancy Hair" || m.Category != "clothes" || m.Version != "1.2" {
		t.Fatalf("basic fields wrong: %#v", m)
	}
	if len(m.Require) != 2 || m.Require[1] != "OtherLib" {
		t.Fatalf("require not parsed: %#v", m.Require)
	}
	if len(m.LoadAfter) != 1 || len(m.LoadBefore) != 1 || len(m.Incompatible) != 1 {
		t.Fatalf("ordering fields not parsed: %#v", m)
	}
	if !m.LoadLast || m.LoadFirst {
		t.Fatalf("load flags wrong: %#v", m)
	}
}

func indexOf(list []string, v string) int {
	for i, x := range list {
		if x == v {
			return i
		}
	}
	return -1
}

// The scan has to carry the ordering rules through from mod.info, or the sort
// silently has nothing to work with and reports "no changes needed".
func TestScanCarriesOrderingRulesThrough(t *testing.T) {
	root := t.TempDir()
	base := filepath.Join(root, "server")
	writeFile(t, filepath.Join(base, "Server", "riv.ini"),
		"Mods=TrueActionsDancing;Tsarslib\n")
	writeFile(t, filepath.Join(base, "mods", "Tsarslib", "mod.info"),
		"name=Tsar's Library\nid=Tsarslib\ncategory=resource\n")
	writeFile(t, filepath.Join(base, "mods", "TrueActionsDancing", "mod.info"),
		"name=Dancing\nid=TrueActionsDancing\nrequire=Tsarslib\n")

	l := Detect(base)
	report := NewScanner(0).Scan(l)

	info := map[string]Mod{}
	for _, m := range report.Mods {
		info[m.ID] = m
	}
	if len(info["TrueActionsDancing"].Require) != 1 {
		t.Fatalf("require was lost between mod.info and the report: %#v", info["TrueActionsDancing"])
	}

	sorted := SortLoadOrder([]string{"TrueActionsDancing", "Tsarslib"}, info, nil)
	if !sorted.Changed || sorted.Order[0] != "Tsarslib" {
		t.Fatalf("the dependency should have moved first: %#v", sorted)
	}
}

// One Workshop item frequently contains several mods, and their folder names
// are whatever the author chose — the Mods= line uses the id= from mod.info.
// Matching only on folder name made every such mod appear twice: once as
// enabled-but-missing, once as installed-but-unused.
func TestBundledWorkshopItemResolvesToOneEntryEach(t *testing.T) {
	root := t.TempDir()
	base := filepath.Join(root, "server")
	ws := filepath.Join(base, "steamapps", "workshop", "content", "108600", "2392709985", "mods")

	writeFile(t, filepath.Join(ws, "TsarsLibFolder", "mod.info"),
		"name=Tsar's Common Library\nid=Tsarslib\n")
	writeFile(t, filepath.Join(ws, "Dancing", "mod.info"),
		"name=True Actions Dancing\nid=TrueActionsDancing\nrequire=Tsarslib\n")
	writeFile(t, filepath.Join(ws, "Extra", "mod.info"),
		"name=Extra Animations\nid=TrueActionsExtra\n")
	writeFile(t, filepath.Join(base, "Server", "riv.ini"),
		"Mods=Tsarslib;TrueActionsDancing;TrueActionsExtra\nWorkshopItems=2392709985\n")

	report := NewScanner(0).Scan(Detect(base))

	if len(report.Missing) != 0 {
		t.Fatalf("all three are on disk under one Workshop item: %v", report.Missing)
	}
	if len(report.Mods) != 3 {
		t.Fatalf("each mod should appear exactly once, got %d: %#v", len(report.Mods), report.Mods)
	}
	for _, m := range report.Mods {
		if !m.Enabled || !m.Installed {
			t.Fatalf("%s should be both enabled and installed: %#v", m.ID, m)
		}
		if m.WorkshopID != "2392709985" {
			t.Fatalf("%s should carry the shared Workshop ID: %#v", m.ID, m)
		}
	}
}

// Writing the folder name in Mods= instead of the declared ID still resolves,
// because older guides and hand-edited configs do exactly that.
func TestFolderNameInModsLineStillMatches(t *testing.T) {
	root := t.TempDir()
	base := filepath.Join(root, "server")
	writeFile(t, filepath.Join(base, "mods", "TsarsLibFolder", "mod.info"),
		"name=Tsar's Common Library\nid=Tsarslib\n")
	writeFile(t, filepath.Join(base, "Server", "riv.ini"), "Mods=TsarsLibFolder\n")

	report := NewScanner(0).Scan(Detect(base))
	if len(report.Missing) != 0 {
		t.Fatalf("the folder name should still resolve: %v", report.Missing)
	}
	if len(report.Mods) != 1 {
		t.Fatalf("it must not be counted twice: %#v", report.Mods)
	}
}

// A mod whose folder and ID agree must not be registered twice.
func TestFolderAndIDTheSameIsOneEntry(t *testing.T) {
	root := t.TempDir()
	base := filepath.Join(root, "server")
	writeFile(t, filepath.Join(base, "mods", "SpareMod", "mod.info"), "name=Spare\nid=SpareMod\n")
	writeFile(t, filepath.Join(base, "Server", "riv.ini"), "Mods=\n")

	report := NewScanner(0).Scan(Detect(base))
	if len(report.Mods) != 1 {
		t.Fatalf("expected one entry, got %#v", report.Mods)
	}
	if report.Mods[0].Folder != "SpareMod" {
		t.Fatalf("the folder should be recorded: %#v", report.Mods[0])
	}
}
