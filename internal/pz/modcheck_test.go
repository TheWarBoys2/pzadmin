package pz

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func codes(issues []ModIssue) []string {
	out := make([]string, 0, len(issues))
	for _, i := range issues {
		out = append(out, i.Code)
	}
	return out
}

func hasCode(issues []ModIssue, code string) *ModIssue {
	for i := range issues {
		if issues[i].Code == code {
			return &issues[i]
		}
	}
	return nil
}

func report(mods ...Mod) ModReport {
	return ModReport{Mods: mods}
}

func TestPreflightCatchesBrokenDependency(t *testing.T) {
	r := report(
		Mod{ID: "Bandits", Declared: "Bandits", Folder: "Bandits", Installed: true,
			Require: []string{"damnlib"}, Path: "/mods/Bandits"},
		Mod{ID: "damnlib", Declared: "damnlib", Folder: "damnlib", Installed: true, Path: "/mods/damnlib"},
	)
	issues := Preflight(r, ModPlan{
		BeforeMods: []string{"damnlib", "Bandits"},
		AfterMods:  []string{"Bandits"},
	})

	got := hasCode(issues, "dependency-missing")
	if got == nil {
		t.Fatalf("expected a dependency-missing issue, got %v", codes(issues))
	}
	if got.Level != LevelBlock {
		t.Fatalf("dependency breakage should block, got level %q", got.Level)
	}
	if !Blocking(issues) {
		t.Fatal("Blocking should be true")
	}
}

func TestPreflightAcceptsAnIntactList(t *testing.T) {
	r := report(
		Mod{ID: "Bandits", Declared: "Bandits", Folder: "Bandits", Installed: true,
			Require: []string{"damnlib"}},
		Mod{ID: "damnlib", Declared: "damnlib", Folder: "damnlib", Installed: true},
	)
	issues := Preflight(r, ModPlan{
		BeforeMods: []string{"damnlib", "Bandits"},
		AfterMods:  []string{"damnlib", "Bandits"},
	})
	if Blocking(issues) {
		t.Fatalf("an unchanged, valid list should not block: %v", codes(issues))
	}
}

// A dependency may be spelled as the folder name in one place and the declared
// ID in another. Both must resolve to the same mod.
func TestPreflightResolvesFolderAndDeclaredNames(t *testing.T) {
	r := report(
		Mod{ID: "Bandits", Declared: "Bandits", Folder: "Bandits", Installed: true,
			Require: []string{"DamnLib"}},
		Mod{ID: "SomeAuthorFolder", Declared: "damnlib", Folder: "SomeAuthorFolder", Installed: true},
	)
	issues := Preflight(r, ModPlan{
		BeforeMods: []string{"SomeAuthorFolder", "Bandits"},
		AfterMods:  []string{"SomeAuthorFolder", "Bandits"},
	})
	if got := hasCode(issues, "dependency-missing"); got != nil {
		t.Fatalf("dependency is present under another name, should not be reported: %s", got.Message)
	}
}

func TestPreflightFindsDuplicateDeclaredIDs(t *testing.T) {
	r := report(
		Mod{ID: "HorseMod", Declared: "HorseMod", Folder: "HorseMod", Installed: true,
			Path: "/workshop/111/mods/HorseMod"},
		Mod{ID: "HorseModOld", Declared: "HorseMod", Folder: "HorseModOld", Installed: true,
			Path: "/workshop/222/mods/HorseModOld"},
	)
	issues := Preflight(r, ModPlan{
		BeforeMods: []string{"HorseMod"},
		AfterMods:  []string{"HorseMod"},
	})
	got := hasCode(issues, "duplicate-id")
	if got == nil {
		t.Fatalf("expected duplicate-id, got %v", codes(issues))
	}
	if got.Level != LevelBlock {
		t.Fatalf("a duplicate covering an enabled mod should block, got %q", got.Level)
	}
	if len(got.Paths) != 2 {
		t.Fatalf("both folders should be named, got %v", got.Paths)
	}
}

func TestPreflightDuplicateOfDisabledModOnlyWarns(t *testing.T) {
	r := report(
		Mod{ID: "HorseMod", Declared: "HorseMod", Folder: "HorseMod", Installed: true, Path: "/a"},
		Mod{ID: "HorseModOld", Declared: "HorseMod", Folder: "HorseModOld", Installed: true, Path: "/b"},
	)
	issues := Preflight(r, ModPlan{AfterMods: []string{}})
	got := hasCode(issues, "duplicate-id")
	if got == nil || got.Level != LevelWarn {
		t.Fatalf("expected a warning for an unused duplicate, got %v", issues)
	}
}

func TestPreflightIncompatiblePair(t *testing.T) {
	r := report(
		Mod{ID: "A", Declared: "A", Folder: "A", Installed: true, Incompatible: []string{"B"}},
		Mod{ID: "B", Declared: "B", Folder: "B", Installed: true},
	)
	issues := Preflight(r, ModPlan{AfterMods: []string{"A", "B"}})
	got := hasCode(issues, "incompatible")
	if got == nil || got.Level != LevelBlock {
		t.Fatalf("expected a blocking incompatibility, got %v", codes(issues))
	}
	// Reported once, not once per direction.
	count := 0
	for _, i := range issues {
		if i.Code == "incompatible" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("expected one incompatibility issue, got %d", count)
	}
}

func TestPreflightWorkshopIDLeftBehind(t *testing.T) {
	r := report(
		Mod{ID: "Ladders", Declared: "Ladders", Folder: "Ladders", Installed: true,
			WorkshopID: "3629835761", Path: "/workshop/3629835761/mods/Ladders"},
	)
	issues := Preflight(r, ModPlan{
		BeforeMods:     []string{"Ladders"},
		BeforeWorkshop: []string{"3629835761"},
		AfterMods:      []string{},
		AfterWorkshop:  []string{"3629835761"},
	})
	if hasCode(issues, "workshop-orphan") == nil {
		t.Fatalf("expected workshop-orphan, got %v", codes(issues))
	}
	if hasCode(issues, "files-remain") == nil {
		t.Fatalf("expected files-remain with the folder path, got %v", codes(issues))
	}
}

// A Workshop item holding several mods stays legitimately listed while any one
// of them is still enabled.
func TestPreflightKeepsSharedWorkshopItemQuiet(t *testing.T) {
	r := report(
		Mod{ID: "69mini", Declared: "69mini", Folder: "69mini", Installed: true, WorkshopID: "2937786633"},
		Mod{ID: "69mini_MrBean", Declared: "69mini_MrBean", Folder: "69mini_MrBean", Installed: true,
			WorkshopID: "2937786633"},
	)
	issues := Preflight(r, ModPlan{
		BeforeMods:     []string{"69mini", "69mini_MrBean"},
		BeforeWorkshop: []string{"2937786633"},
		AfterMods:      []string{"69mini"},
		AfterWorkshop:  []string{"2937786633"},
	})
	if got := hasCode(issues, "workshop-orphan"); got != nil {
		t.Fatalf("another mod from the item is still enabled: %s", got.Message)
	}
}

func TestPreflightWorkshopRemovedWhileModEnabled(t *testing.T) {
	r := report(
		Mod{ID: "Ladders", Declared: "Ladders", Folder: "Ladders", Installed: true,
			WorkshopID: "3629835761"},
	)
	issues := Preflight(r, ModPlan{
		BeforeMods:     []string{"Ladders"},
		BeforeWorkshop: []string{"3629835761"},
		AfterMods:      []string{"Ladders"},
		AfterWorkshop:  []string{},
	})
	if hasCode(issues, "workshop-removed") == nil {
		t.Fatalf("expected workshop-removed, got %v", codes(issues))
	}
}

func TestPreflightLoadOrder(t *testing.T) {
	r := report(
		Mod{ID: "Bandits", Declared: "Bandits", Folder: "Bandits", Installed: true,
			Require: []string{"damnlib"}},
		Mod{ID: "damnlib", Declared: "damnlib", Folder: "damnlib", Installed: true},
	)
	issues := Preflight(r, ModPlan{AfterMods: []string{"Bandits", "damnlib"}})
	got := hasCode(issues, "load-order")
	if got == nil || got.Level != LevelWarn {
		t.Fatalf("expected a load-order warning, got %v", codes(issues))
	}
	if Blocking(issues) {
		t.Fatal("a load order problem is fixable with Sort and should not block")
	}
}

// Adding a mod and its Workshop item together is the normal way to install
// one. The files are legitimately absent at that moment.
func TestPreflightNewWorkshopModIsNotAWarning(t *testing.T) {
	issues := Preflight(report(), ModPlan{
		AfterMods:     []string{"NewMod"},
		AfterWorkshop: []string{"123456"},
	})
	got := hasCode(issues, "not-installed-yet")
	if got == nil || got.Level != LevelInfo {
		t.Fatalf("expected an informational note, got %v", issues)
	}
	if hasCode(issues, "not-installed") != nil {
		t.Fatal("should not also warn that nothing will fetch it")
	}
}

func TestPreflightModWithNothingToFetchItWarns(t *testing.T) {
	issues := Preflight(report(), ModPlan{AfterMods: []string{"NewMod"}})
	got := hasCode(issues, "not-installed")
	if got == nil || got.Level != LevelWarn {
		t.Fatalf("expected a warning, got %v", issues)
	}
}

func TestPreflightMentionsSaveDataOnAnyRemoval(t *testing.T) {
	r := report(Mod{ID: "A", Declared: "A", Folder: "A", Installed: true})
	issues := Preflight(r, ModPlan{BeforeMods: []string{"A"}, AfterMods: []string{}})
	if hasCode(issues, "save-data") == nil {
		t.Fatalf("expected the save-data note, got %v", codes(issues))
	}
	issues = Preflight(r, ModPlan{BeforeMods: []string{"A"}, AfterMods: []string{"A"}})
	if hasCode(issues, "save-data") != nil {
		t.Fatal("nothing was removed, so there should be no save-data note")
	}
}

func TestPreflightOrdersWorstFirst(t *testing.T) {
	r := report(
		Mod{ID: "Bandits", Declared: "Bandits", Folder: "Bandits", Installed: true,
			Require: []string{"damnlib"}, Path: "/mods/Bandits"},
		Mod{ID: "damnlib", Declared: "damnlib", Folder: "damnlib", Installed: true, Path: "/mods/damnlib"},
	)
	issues := Preflight(r, ModPlan{
		BeforeMods: []string{"damnlib", "Bandits"},
		AfterMods:  []string{"Bandits"},
	})
	if len(issues) < 2 {
		t.Fatalf("expected several issues, got %v", codes(issues))
	}
	if issues[0].Level != LevelBlock {
		t.Fatalf("worst issue should sort first, got %v", codes(issues))
	}
	last := issues[len(issues)-1]
	if last.Level != LevelInfo {
		t.Fatalf("informational issues should sort last, got %q", last.Level)
	}
}

func TestPreflightMessagesAreReadable(t *testing.T) {
	r := report(
		Mod{ID: "Bandits", Name: "Bandits", Declared: "Bandits", Folder: "Bandits", Installed: true,
			Require: []string{"damnlib"}},
		Mod{ID: "damnlib", Declared: "damnlib", Folder: "damnlib", Installed: true},
	)
	issues := Preflight(r, ModPlan{
		BeforeMods: []string{"damnlib", "Bandits"},
		AfterMods:  []string{"Bandits"},
	})
	for _, i := range issues {
		if strings.TrimSpace(i.Message) == "" {
			t.Fatalf("issue %s has no message", i.Code)
		}
		if strings.Contains(i.Message, "%!") {
			t.Fatalf("issue %s has a broken format string: %s", i.Code, i.Message)
		}
	}
}

// The scan collapses a mod that appears under two names into one entry, which
// is right for listing and wrong for this: an old copy of a mod left on disk
// after it moved Workshop items declares the same ID as the current one, and
// Project Zomboid loads whichever it reaches first. This walks the real
// directory scan rather than a hand-built report, because the collapsing
// happens there and a test that skips it proves nothing.
func TestScanKeepsDuplicateFoldersForTheCheck(t *testing.T) {
	base := t.TempDir()
	workshop := filepath.Join(base, "steamapps", "workshop", "content", "108600")

	mod := func(item, folder, id, extra string) {
		dir := filepath.Join(workshop, item, "mods", folder)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		body := "name=" + id + "\nid=" + id + "\n" + extra + "\n"
		if err := os.WriteFile(filepath.Join(dir, "mod.info"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	mod("3171167894", "damnlib", "damnlib", "")
	mod("9999999999", "damnlibOld", "damnlib", "")
	mod("3268487204", "Bandits", "Bandits", "require=damnlib")

	server := filepath.Join(base, "Server")
	if err := os.MkdirAll(server, 0o755); err != nil {
		t.Fatal(err)
	}
	ini := "Mods=damnlib;Bandits\nWorkshopItems=3171167894;3268487204;9999999999\n"
	if err := os.WriteFile(filepath.Join(server, "test.ini"), []byte(ini), 0o600); err != nil {
		t.Fatal(err)
	}

	report := NewScanner(time.Minute).Scan(Detect(base))
	if len(report.Duplicates) != 2 {
		t.Fatalf("both copies should survive the scan, got %d: %+v", len(report.Duplicates), report.Duplicates)
	}

	issues := Preflight(report, ModPlan{
		BeforeMods: []string{"damnlib", "Bandits"},
		AfterMods:  []string{"damnlib", "Bandits"},
	})
	got := hasCode(issues, "duplicate-id")
	if got == nil {
		t.Fatalf("the duplicate should be reported, got %v", codes(issues))
	}
	if got.Level != LevelBlock {
		t.Fatalf("a duplicate of an enabled mod should block, got %q", got.Level)
	}
	if len(got.Paths) != 2 {
		t.Fatalf("both folders should be named so they can be dealt with: %v", got.Paths)
	}
	joined := strings.Join(got.Paths, " ")
	if !strings.Contains(joined, "damnlibOld") || !strings.Contains(joined, "3171167894") {
		t.Fatalf("paths = %v", got.Paths)
	}
}

// One folder registered under both its ID and its folder name is one folder,
// not a duplicate.
func TestScanDoesNotInventDuplicates(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "steamapps", "workshop", "content", "108600", "111", "mods", "SomeFolder")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "mod.info"),
		[]byte("name=Thing\nid=TheModID\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	server := filepath.Join(base, "Server")
	if err := os.MkdirAll(server, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(server, "test.ini"),
		[]byte("Mods=TheModID\nWorkshopItems=111\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	report := NewScanner(time.Minute).Scan(Detect(base))
	if len(report.Duplicates) != 0 {
		t.Fatalf("a single folder is not a duplicate: %+v", report.Duplicates)
	}
	issues := Preflight(report, ModPlan{AfterMods: []string{"TheModID"}})
	if got := hasCode(issues, "duplicate-id"); got != nil {
		t.Fatalf("nothing should be reported: %s", got.Message)
	}
}
