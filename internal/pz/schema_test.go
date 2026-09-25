package pz

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestINIFieldsTypeAndClassify(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "riv.ini")
	writeFile(t, path, strings.Join([]string{
		"# comment",
		"PublicName=Riverside",
		"MaxPlayers=16",
		"PVP=true",
		"Mods=ModA;ModB",
		"RCONPassword=secret",
		"SomeFutureOption=1.5",
		"AnotherUnknown=hello",
	}, "\n"))

	ini, err := LoadINI(path)
	if err != nil {
		t.Fatal(err)
	}
	byKey := map[string]Field{}
	for _, f := range ini.Fields() {
		byKey[f.Key] = f
	}

	if got := byKey["MaxPlayers"]; got.Type != FieldInt || got.Min == nil || *got.Min != 1 {
		t.Fatalf("MaxPlayers should be a bounded integer: %#v", got)
	}
	if got := byKey["PVP"]; got.Type != FieldBool || got.Applies != AppliesOnReload {
		t.Fatalf("PVP should be a live-reloadable boolean: %#v", got)
	}
	// Mods and ports are read at startup; saying otherwise would mislead.
	if got := byKey["Mods"]; got.Applies != AppliesOnRestart || got.Type != FieldList {
		t.Fatalf("Mods should be a restart-only list: %#v", got)
	}
	if got := byKey["RCONPassword"]; !got.Secret || got.Applies != AppliesOnRestart {
		t.Fatalf("the RCON password should be secret and restart-only: %#v", got)
	}

	// Unknown options must still be editable, with the type inferred and no
	// invented description.
	future := byKey["SomeFutureOption"]
	if future.Type != FieldFloat || !future.Advanced || future.Help != "" {
		t.Fatalf("an unknown option should be typed but undocumented: %#v", future)
	}
	if byKey["AnotherUnknown"].Type != FieldText {
		t.Fatalf("unexpected type %#v", byKey["AnotherUnknown"])
	}
}

func TestValidateField(t *testing.T) {
	maxPlayers := Field{Key: "MaxPlayers", Type: FieldInt, Min: ptr(1), Max: ptr(100)}
	if _, err := ValidateField(maxPlayers, "16"); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"0", "500", "lots", ""} {
		if _, err := ValidateField(maxPlayers, bad); err == nil {
			t.Fatalf("MaxPlayers=%q should be refused", bad)
		}
	}

	pvp := Field{Key: "PVP", Type: FieldBool}
	if got, err := ValidateField(pvp, "TRUE"); err != nil || got != "true" {
		t.Fatalf("got %q %v", got, err)
	}
	if _, err := ValidateField(pvp, "yes"); err == nil {
		t.Fatal("only true and false are valid booleans in the ini")
	}

	text := Field{Key: "PublicName", Type: FieldText}
	if _, err := ValidateField(text, "line\nbreak"); err == nil {
		t.Fatal("a newline would corrupt every setting after it")
	}
}

const sandboxSample = `SandboxVars = {
    VERSION = 5,
    Zombies = 3,
    Distribution = 1,
    -- a comment
    DayLength = 3,
    ServerName = "Riverside",
    ZombieLore = {
        Speed = 2,
        Strength = 2,
    },
    ZombieConfig = {
        PopulationMultiplier = 1.0,
    },
}
`

func TestSandboxFieldsAreGroupedByTable(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "kal_SandboxVars.lua")
	writeFile(t, path, sandboxSample)

	sb, err := LoadSandbox(path)
	if err != nil {
		t.Fatal(err)
	}
	byKey := map[string]Field{}
	for _, f := range sb.Fields() {
		byKey[f.Key] = f
	}

	if got := byKey["Zombies"]; got.Group != "General" || got.Type != FieldInt {
		t.Fatalf("top level setting wrong: %#v", got)
	}
	if got := byKey["ZombieLore.Speed"]; got.Group != "ZombieLore" || got.Value != "2" {
		t.Fatalf("nested setting wrong: %#v", got)
	}
	if got := byKey["ZombieConfig.PopulationMultiplier"]; got.Type != FieldFloat {
		t.Fatalf("float not detected: %#v", got)
	}
	if got := byKey["ServerName"]; got.Type != FieldText || got.Value != "Riverside" {
		t.Fatalf("quoted string should be unquoted for editing: %#v", got)
	}
	// Sandbox values are read at world load, so nothing here applies live.
	for _, f := range sb.Fields() {
		if f.Applies != AppliesOnRestart {
			t.Fatalf("%s should be marked restart-only, got %s", f.Key, f.Applies)
		}
	}
}

func TestSandboxSetPreservesEverythingElse(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "kal_SandboxVars.lua")
	writeFile(t, path, sandboxSample)

	sb, _ := LoadSandbox(path)
	if err := sb.Set("Zombies", "1"); err != nil {
		t.Fatal(err)
	}
	if err := sb.Set("ZombieLore.Speed", "3"); err != nil {
		t.Fatal(err)
	}
	if err := sb.Set("ServerName", "Riverside 2"); err != nil {
		t.Fatal(err)
	}
	if _, err := sb.Save(filepath.Join(dir, "bak")); err != nil {
		t.Fatal(err)
	}

	out := readFileString(t, path)
	for _, want := range []string{
		"    Zombies = 1,",
		"        Speed = 3,",
		`    ServerName = "Riverside 2",`,
		"-- a comment",
		"    Strength = 2,",
		"    VERSION = 5,",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("expected %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "Zombies = 3") {
		t.Fatal("the old value should be gone")
	}

	// It must still parse and round trip.
	again, err := LoadSandbox(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range again.Fields() {
		if f.Key == "ZombieLore.Speed" && f.Value != "3" {
			t.Fatalf("value did not round trip: %#v", f)
		}
	}
}

func TestSandboxRejectsUnknownKeyAndBadValue(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "s_SandboxVars.lua")
	writeFile(t, path, sandboxSample)
	sb, _ := LoadSandbox(path)

	if err := sb.Set("NotAThing", "1"); err == nil {
		t.Fatal("an unknown key should be refused rather than appended blind")
	}
	if err := sb.Set("Zombies", "1,\n    Evil = 1"); err == nil {
		t.Fatal("a newline would let one field inject another")
	}
}

func readFileString(t *testing.T, path string) string {
	t.Helper()
	b, err := readFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
