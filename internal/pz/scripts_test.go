package pz

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const vanillaItems = `module Base
{
	item BreadSlices
	{
		DisplayName = Bread Slices,
		DisplayCategory = Food,
		Type = Food,
		Weight = 0.1,
	}

	item Axe
	{
		DisplayName = Axe,
		DisplayCategory = Tool,
		Type = Weapon,
		MaxDamage = 2.5,
	}

	/* a commented block */
	item Screwdriver
	{
		DisplayName	=	Screwdriver,
		Type = Weapon,
	}
}
`

const vanillaVehicles = `module Base
{
	vehicle CarNormal
	{
		mechanicType = 1,
		engineRepairLevel = 3,
		model
		{
			file = CarStationWagon,
			scale = 1.0,
		}
	}

	vehicle Van
	{
		mechanicType = 3,
	}
}
`

const modItems = `module ExpandedHelicopter
{
	item HeliFuel
	{
		DisplayName = Helicopter Fuel,
		DisplayCategory = Fuel,
		Type = Normal,
	}
}
`

func buildGameTree(t *testing.T) (gameRoot string, modDir string) {
	t.Helper()
	root := t.TempDir()

	gameRoot = filepath.Join(root, "pzserver")
	writeFile(t, filepath.Join(gameRoot, "media", "scripts", "items.txt"), vanillaItems)
	writeFile(t, filepath.Join(gameRoot, "media", "scripts", "vehicles", "vehicles.txt"), vanillaVehicles)

	// A Workshop-style mod tree: <workshopID>/mods/<modID>/media/scripts
	modDir = filepath.Join(root, "Workshop")
	writeFile(t, filepath.Join(modDir, "2822286426", "mods", "ExpandedHelicopter", "media", "scripts", "items.txt"), modItems)
	// And a plain mod tree: <modID>/media/scripts
	writeFile(t, filepath.Join(modDir, "PlainMod", "media", "scripts", "stuff.txt"),
		"module PlainMod\n{\n\titem Trinket\n\t{\n\t\tDisplayName = Trinket,\n\t\tType = Normal,\n\t}\n}\n")
	return gameRoot, modDir
}

func find(t *testing.T, list []CatalogueEntry, id string) CatalogueEntry {
	t.Helper()
	for _, e := range list {
		if e.ID == id {
			return e
		}
	}
	t.Fatalf("%s not found in %d entries", id, len(list))
	return CatalogueEntry{}
}

func TestScanReadsVanillaItems(t *testing.T) {
	gameRoot, _ := buildGameTree(t)
	cat := NewScriptScanner(time.Minute).Scan(gameRoot, nil)

	bread := find(t, cat.Items, "Base.BreadSlices")
	if bread.Name != "Bread Slices" || bread.Category != "Food" || bread.Source != "vanilla" {
		t.Fatalf("unexpected entry %#v", bread)
	}
	axe := find(t, cat.Items, "Base.Axe")
	if axe.Category != "Tool" {
		t.Fatalf("DisplayCategory should win over Type: %#v", axe)
	}
	// Without a DisplayCategory the Type is used instead.
	screw := find(t, cat.Items, "Base.Screwdriver")
	if screw.Category != "Weapon" {
		t.Fatalf("expected the Type as a fallback category, got %#v", screw)
	}
	if screw.Name != "Screwdriver" {
		t.Fatalf("tab-separated fields should still parse: %#v", screw)
	}
}

func TestScanReadsVehiclesSeparately(t *testing.T) {
	gameRoot, _ := buildGameTree(t)
	cat := NewScriptScanner(time.Minute).Scan(gameRoot, nil)

	car := find(t, cat.Vehicles, "Base.CarNormal")
	if car.Kind != "vehicle" || car.Category != "Vehicles" {
		t.Fatalf("unexpected vehicle %#v", car)
	}
	find(t, cat.Vehicles, "Base.Van")

	// A nested model block must not be mistaken for another vehicle.
	for _, e := range cat.Vehicles {
		if e.ID == "Base.model" {
			t.Fatal("a nested block was parsed as a vehicle")
		}
	}
	for _, e := range cat.Items {
		if e.Kind == "vehicle" {
			t.Fatal("vehicles must not appear in the item list")
		}
	}
}

func TestScanIncludesModItems(t *testing.T) {
	gameRoot, modDir := buildGameTree(t)
	cat := NewScriptScanner(time.Minute).Scan(gameRoot, []string{modDir})

	heli := find(t, cat.Items, "ExpandedHelicopter.HeliFuel")
	if heli.Source != "ExpandedHelicopter" {
		t.Fatalf("mod items should be attributed to the mod: %#v", heli)
	}
	find(t, cat.Items, "PlainMod.Trinket")

	if len(cat.Sources) < 3 {
		t.Fatalf("expected vanilla and both mods in the source list, got %v", cat.Sources)
	}
}

func TestScanWorksWithoutTheGameFiles(t *testing.T) {
	_, modDir := buildGameTree(t)
	cat := NewScriptScanner(time.Minute).Scan("", []string{modDir})
	if len(cat.Items) == 0 {
		t.Fatal("mod items should still be found when the game install is not mounted")
	}
	for _, e := range cat.Items {
		if e.Source == "vanilla" {
			t.Fatal("nothing should be attributed to vanilla without the game files")
		}
	}
}

func TestCategoriesAreSortedAndDistinct(t *testing.T) {
	gameRoot, _ := buildGameTree(t)
	cat := NewScriptScanner(time.Minute).Scan(gameRoot, nil)
	cats := cat.Categories("item")
	if len(cats) < 3 {
		t.Fatalf("expected several categories, got %v", cats)
	}
	for i := 1; i < len(cats); i++ {
		if cats[i-1] >= cats[i] {
			t.Fatalf("categories should be sorted and distinct: %v", cats)
		}
	}
	if v := cat.Categories("vehicle"); len(v) != 1 || v[0] != "Vehicles" {
		t.Fatalf("vehicles should be one category, got %v", v)
	}
}

func TestScanIsCachedUntilScriptsChange(t *testing.T) {
	gameRoot, _ := buildGameTree(t)
	s := NewScriptScanner(time.Minute)
	first := s.Scan(gameRoot, nil)
	second := s.Scan(gameRoot, nil)
	if !first.ScannedAt.Equal(second.ScannedAt) {
		t.Fatal("an unchanged tree should be served from cache")
	}

	time.Sleep(10 * time.Millisecond)
	writeFile(t, filepath.Join(gameRoot, "media", "scripts", "extra.txt"),
		"module Base\n{\n\titem Newthing\n\t{\n\t\tType = Normal,\n\t}\n}\n")
	third := s.Scan(gameRoot, nil)
	if third.ScannedAt.Equal(first.ScannedAt) {
		t.Fatal("adding a script file should invalidate the cache")
	}
	find(t, third.Items, "Base.Newthing")
}

func TestDetectGameRoot(t *testing.T) {
	gameRoot, _ := buildGameTree(t)
	if got := DetectGameRoot(gameRoot); got != gameRoot {
		t.Fatalf("a direct install should be detected, got %q", got)
	}
	if got := DetectGameRoot(filepath.Dir(gameRoot)); got != gameRoot {
		t.Fatalf("an install one level down should be detected, got %q", got)
	}
	if got := DetectGameRoot(t.TempDir()); got != "" {
		t.Fatalf("an unrelated directory should return nothing, got %q", got)
	}
}

func TestMalformedScriptsDoNotPanic(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "media", "scripts", "broken.txt"),
		"module Base\n{\n item Unclosed\n {\n  DisplayName = Nope,\n")
	writeFile(t, filepath.Join(dir, "media", "scripts", "empty.txt"), "")
	writeFile(t, filepath.Join(dir, "media", "scripts", "junk.txt"), "}}}{{{ = = = ,,,\n")
	cat := NewScriptScanner(time.Minute).Scan(dir, nil)
	_ = cat // the only requirement is that it returns
}

// buildPerServerInstall reproduces the layout where each server carries its own
// Project Zomboid installation, so the servers can run different builds and mod
// sets without interfering. SteamCMD's force_install_dir leaves the game at
// <dir>/media with a steamapps folder beside it.
func buildPerServerInstall(t *testing.T, name string, extraItem string) (root, base string) {
	t.Helper()
	root = t.TempDir()
	base = filepath.Join(root, name, "projectzomboid")

	writeFile(t, filepath.Join(base, "data", "media", "scripts", "items.txt"),
		"module Base\n{\n\titem Axe\n\t{\n\t\tDisplayName = Axe,\n\t\tDisplayCategory = Tool,\n\t}\n}\n")
	writeFile(t, filepath.Join(base, "data", "media", "scripts", "vehicles", "v.txt"),
		"module Base\n{\n\tvehicle CarNormal\n\t{\n\t\tmechanicType = 1,\n\t}\n}\n")
	writeFile(t, filepath.Join(base, "data", "steamapps", "appmanifest_380870.acf"), "steam")
	writeFile(t, filepath.Join(base, "data", "steamapps", "workshop", "content", "108600",
		"2822286426", "mods", extraItem, "media", "scripts", "m.txt"),
		"module "+extraItem+"\n{\n\titem Thing\n\t{\n\t\tDisplayName = Thing,\n\t}\n}\n")
	writeFile(t, filepath.Join(base, "config", "Server", name+".ini"),
		"PublicName="+name+"\nMods="+extraItem+"\n")
	writeFile(t, filepath.Join(base, "data", "Saves", "Multiplayer", name, "map.bin"), "world")
	writeFile(t, filepath.Join(base, "config", "Logs", "26-09-08_user.txt"), "")
	return root, base
}

// The install lives inside the server's own folder, so nothing has to be
// configured for the item picker to work.
func TestPerServerInstallIsFoundWithoutConfiguration(t *testing.T) {
	_, base := buildPerServerInstall(t, "riverside2", "ModA")

	l := Detect(base)
	if l.GameDir == "" {
		t.Fatal("the server's own game installation should be detected")
	}
	if filepath.Base(l.GameDir) != "data" {
		t.Fatalf("expected the steamcmd install directory, got %s", l.GameDir)
	}
	if l.ConfigDir == "" || l.SavesDir == "" {
		t.Fatalf("the rest of the layout should still resolve: %#v", l)
	}
	if len(l.ModDirs) == 0 {
		t.Fatalf("the workshop content directory should be found: %#v", l)
	}

	cat := NewScriptScanner(time.Minute).Scan(l.GameDir, l.ModDirs)
	find(t, cat.Items, "Base.Axe")
	find(t, cat.Items, "ModA.Thing")
	find(t, cat.Vehicles, "Base.CarNormal")
}

// Two servers, two installs, two mod sets. Each must see only its own.
func TestEachServerSeesItsOwnMods(t *testing.T) {
	_, first := buildPerServerInstall(t, "riverside2", "ModA")
	_, second := buildPerServerInstall(t, "hardcore", "ModB")

	scanner := NewScriptScanner(time.Minute)
	one := Detect(first)
	two := Detect(second)

	catOne := scanner.Scan(one.GameDir, one.ModDirs)
	catTwo := scanner.Scan(two.GameDir, two.ModDirs)

	find(t, catOne.Items, "ModA.Thing")
	find(t, catTwo.Items, "ModB.Thing")
	for _, e := range catOne.Items {
		if e.ID == "ModB.Thing" {
			t.Fatal("a server must not be offered another server's mod items")
		}
	}
	for _, e := range catTwo.Items {
		if e.ID == "ModA.Thing" {
			t.Fatal("a server must not be offered another server's mod items")
		}
	}
}

// The search must not be fooled by the data directories that sit beside it.
func TestDetectGameRootSkipsBulkDirectories(t *testing.T) {
	root := t.TempDir()
	// A decoy media/scripts buried inside Saves must not win.
	writeFile(t, filepath.Join(root, "Saves", "deep", "media", "scripts", "x.txt"), "")
	writeFile(t, filepath.Join(root, "install", "media", "scripts", "items.txt"), "")

	got := DetectGameRoot(root)
	if filepath.Base(got) != "install" {
		t.Fatalf("expected the real install, got %q", got)
	}
}

func TestDetectGameRootPrefersTheShallowestMatch(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "outer", "media", "scripts", "a.txt"), "")
	writeFile(t, filepath.Join(root, "outer", "nested", "media", "scripts", "b.txt"), "")
	if got := DetectGameRoot(root); filepath.Base(got) != "outer" {
		t.Fatalf("expected the shallowest install, got %q", got)
	}
}

func TestDetectGameRootGivesUpQuietly(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "a", "b", "c", "d", "e", "media", "scripts", "x.txt"), "")
	if got := DetectGameRoot(root); got != "" {
		t.Fatalf("a search this deep should be abandoned rather than walking the disk, got %q", got)
	}
}

// Not every script file wraps its blocks in a module, and some name blocks in
// their fully qualified form. Both should still be read.
func TestModuleLessAndQualifiedBlocks(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "media", "scripts", "loose.txt"),
		"item LooseItem\n{\n\tDisplayName = Loose,\n\tDisplayCategory = Tool,\n}\n")
	writeFile(t, filepath.Join(dir, "media", "scripts", "qualified.txt"),
		"module Base\n{\n\titem Base.Qualified\n\t{\n\t\tDisplayName = Qualified,\n\t}\n}\n")

	cat := NewScriptScanner(0).Scan(dir, nil)
	find(t, cat.Items, "Base.LooseItem")
	// The name is already qualified, so it must not become Base.Base.Qualified.
	find(t, cat.Items, "Base.Qualified")
	for _, e := range cat.Items {
		if strings.Count(e.ID, ".") > 1 {
			t.Fatalf("a qualified name was prefixed twice: %s", e.ID)
		}
	}
}

// A directory full of script files that yielded nothing is the one case where
// the scan report has to show its working.
func TestScanReportSamplesUnrecognisedFiles(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "media", "scripts", "strange.txt"),
		"someNewFormat {\n  thing = 1,\n}\n")

	cat := NewScriptScanner(0).Scan(dir, nil)
	var vanilla ScanPath
	for _, sp := range cat.Scanned {
		if sp.Source == "vanilla" {
			vanilla = sp
		}
	}
	if vanilla.Files != 1 {
		t.Fatalf("the file should have been read: %#v", vanilla)
	}
	if vanilla.Items != 0 {
		t.Fatalf("nothing should have been recognised: %#v", vanilla)
	}
	if vanilla.SampleFile != "strange.txt" || !strings.Contains(vanilla.Sample, "someNewFormat") {
		t.Fatalf("the report should sample the file so the format can be identified: %#v", vanilla)
	}
}

// A directory that parsed fine must not carry a sample: it is noise.
func TestScanReportOmitsSampleWhenParsingWorked(t *testing.T) {
	gameRoot, _ := buildGameTree(t)
	cat := NewScriptScanner(0).Scan(gameRoot, nil)
	for _, sp := range cat.Scanned {
		if sp.Items > 0 && sp.Sample != "" {
			t.Fatalf("no sample expected for %s", sp.Path)
		}
	}
}
