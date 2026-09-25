package provision

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/TheWarBoys2/pzadmin/internal/compose"
	"github.com/TheWarBoys2/pzadmin/internal/pz"
	"github.com/TheWarBoys2/pzadmin/internal/stacks"
)

const pinned = "indifferentbroccoli/projectzomboid-server-docker@sha256:0123456789abcdef"

type host struct {
	stacksRoot, dataRoot string
	sourceDir            string
	presetDir            string
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func sandboxText(extra string) string {
	var b strings.Builder
	b.WriteString("SandboxVars = {\n    VERSION = 6,\n")
	for i := 0; i < 30; i++ {
		b.WriteString("    Setting" + string(rune('A'+i%26)) + string(rune('a'+i/26)) + " = 3,\n")
	}
	b.WriteString(extra)
	b.WriteString("}\n")
	return b.String()
}

func newHost(t *testing.T) host {
	t.Helper()
	root := t.TempDir()
	h := host{
		stacksRoot: filepath.Join(root, "home/rick/docker/pzserver"),
		dataRoot:   filepath.Join(root, "srv/zomboid"),
	}
	h.sourceDir = filepath.Join(h.stacksRoot, "pz-coalfield", "Server")
	write(t, filepath.Join(h.sourceDir, "coalfield.ini"),
		"PublicName=Coalfield\nMods=RPProfessionFramework\nWorkshopItems=123\nMap=Coalfield\nRCONPort=27017\nRCONPassword=coalsecret\nDefaultPort=16265\nUDPPort=16266\nPVP=false\n")
	write(t, filepath.Join(h.sourceDir, "coalfield_SandboxVars.lua"), sandboxText("    Zombies = 2,\n"))
	write(t, filepath.Join(h.sourceDir, "coalfield_spawnregions.lua"), "function SpawnRegions() return {} end\n")
	h.presetDir = filepath.Join(h.dataRoot, "coalfield", "projectzomboid", "data", "media", "lua", "shared", "Sandbox")
	write(t, filepath.Join(h.presetDir, "Apocalypse.lua"), sandboxText("    Zombies = 1,\n"))
	write(t, filepath.Join(h.presetDir, "Broken.lua"), "return {}\n")
	return h
}

func (h host) env() Env {
	return Env{
		StacksRoot: h.stacksRoot, DataRoot: h.dataRoot, Image: pinned, PUID: 1000, PGID: 1000,
		Existing: []stacks.Stack{
			{Name: "pz-riverside", Container: "pz-riverside", GamePort: 16261, UDPPort: 16262, RCONPort: 27015},
			{Name: "pz-westpoint", Container: "pz-westpoint", GamePort: 16263, UDPPort: 16264, RCONPort: 27016,
				DataDir: "/srv/zomboid/westpoint/projectzomboid/data"},
		},
	}
}

func (h host) clone(name string) Request {
	return Request{Name: name, Start: StartClone, FromDir: h.sourceDir, FromName: "coalfield", AdminPassword: "letmein"}
}

func (h host) fresh(name string) Request {
	return Request{Name: name, Start: StartDefaults, AdminPassword: "letmein"}
}

func TestDefaultsAreTheCapturedFilesWithoutIdentity(t *testing.T) {
	s, err := DefaultStart()
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"ResetID=", "ServerPlayerID=", "Seed="} {
		if strings.Contains(s.INI, "\n"+key) {
			t.Fatalf("%s must be left for the game to generate", key)
		}
	}
	if strings.Contains(s.INI, "Reset ID determines") || strings.Contains(s.INI, "worldgen seed") {
		t.Fatal("the removed keys' comments should go with them")
	}
	for _, key := range []string{"PVP=true", "PublicName=My PZ Server", "Map=Muldraugh, KY"} {
		if !strings.Contains(s.INI, key) {
			t.Fatalf("missing %s", key)
		}
	}
	f, err := FieldsFor(s)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.INI) < 130 || len(f.Sandbox) < 200 {
		t.Fatalf("expected the full defaults, got %d ini and %d sandbox fields", len(f.INI), len(f.Sandbox))
	}
	byKey := map[string]pz.Field{}
	for _, x := range f.Sandbox {
		byKey[x.Key] = x
	}
	z := byKey["Zombies"]
	if z.Type != pz.FieldChoice || len(z.Options) != 6 || z.Options[0].Label != "Insane" {
		t.Fatalf("sandbox choices should come from the file's own comments: %+v", z)
	}
	for _, x := range f.INI {
		if x.Key == "RCONPort" && x.Locked == "" {
			t.Fatal("RCONPort is set on the first page, so it is locked here")
		}
		if x.Key == "SafetyToggleTimer" && (x.Min == nil || *x.Max != 1000) {
			t.Fatalf("ranges should come from the ini's comments: %+v", x)
		}
	}
}

func TestFreshServerFromDefaultsWithChanges(t *testing.T) {
	h := newHost(t)
	req := h.fresh("pz-fresh")
	req.INI = map[string]string{"PublicName": "Fresh Start", "PVP": "false", "Mods": "ModA;ModB", "WorkshopItems": "111;222"}
	req.Sandbox = map[string]string{"Zombies": "2", "ZombieLore.Speed": "3"}
	plan, err := Make(req, h.env())
	if err != nil {
		t.Fatal(err)
	}
	if plan.StartedFrom != "defaults" || plan.ChangedINI != 4 || plan.ChangedSandbox != 2 {
		t.Fatalf("%+v", plan)
	}
	if _, err := Apply(plan); err != nil {
		t.Fatal(err)
	}
	ini, _ := pz.LoadINI(filepath.Join(plan.ServerDir, "fresh.ini"))
	for key, want := range map[string]string{
		"PublicName": "Fresh Start", "PVP": "false", "Mods": "ModA;ModB", "WorkshopItems": "111;222",
		"RCONPort": "27017", "DefaultPort": "16265", "MaxPlayers": "32",
	} {
		if v, _ := ini.Get(key); v != want {
			t.Fatalf("%s = %q, want %q", key, v, want)
		}
	}
	if v, _ := ini.Get("RCONPassword"); v == "" {
		t.Fatal("the new server needs its own RCON password")
	}
	sb, _ := pz.LoadSandbox(filepath.Join(plan.ServerDir, "fresh_SandboxVars.lua"))
	got := map[string]string{}
	for _, f := range sb.Fields() {
		got[f.Key] = f.Value
	}
	if got["Zombies"] != "2" || got["ZombieLore.Speed"] != "3" {
		t.Fatalf("sandbox changes: %v %v", got["Zombies"], got["ZombieLore.Speed"])
	}
	regions, _ := os.ReadFile(filepath.Join(plan.ServerDir, "fresh_spawnregions.lua"))
	if !strings.Contains(string(regions), "fresh_spawnpoints.lua") || strings.Contains(string(regions), "defaults_spawnpoints") {
		t.Fatalf("spawn regions must name the new spawn points file:\n%s", regions)
	}
	env, _ := compose.ReadEnvFile(filepath.Join(plan.StackDir, ".env"))
	if env.Lookup("ADMIN_PASSWORD") != "letmein" {
		t.Fatal("the admin password is the one the operator chose")
	}
}

func TestWizardChangesAreValidated(t *testing.T) {
	h := newHost(t)
	for name, change := range map[string]func(*Request){
		"unknown ini key":       func(r *Request) { r.INI = map[string]string{"NotASetting": "1"} },
		"locked ini key":        func(r *Request) { r.INI = map[string]string{"RCONPort": "1"} },
		"bad bool":              func(r *Request) { r.INI = map[string]string{"PVP": "maybe"} },
		"out of range":          func(r *Request) { r.INI = map[string]string{"SafetyToggleTimer": "5000"} },
		"not an option":         func(r *Request) { r.Sandbox = map[string]string{"Zombies": "9"} },
		"unknown sandbox key":   func(r *Request) { r.Sandbox = map[string]string{"Nope": "1"} },
		"newline":               func(r *Request) { r.INI = map[string]string{"PublicName": "a\nRCONPassword=x"} },
		"no admin password":     func(r *Request) { r.AdminPassword = "" },
		"admin password with $": func(r *Request) { r.AdminPassword = "pa$$word" },
	} {
		req := h.fresh("pz-x")
		change(&req)
		if _, err := Make(req, h.env()); err == nil {
			t.Errorf("%s: should be refused", name)
		}
	}
}

func TestCloneKeepsSettingsButNotIdentity(t *testing.T) {
	h := newHost(t)
	write(t, filepath.Join(h.sourceDir, "coalfield.ini"),
		"PublicName=Coalfield\nMods=RPProfessionFramework\nWorkshopItems=123\nMap=Coalfield\nRCONPort=27017\n"+
			"RCONPassword=coalsecret\nDefaultPort=16265\nUDPPort=16266\nPVP=false\nResetID=42\nServerPlayerID=43\nSeed=abc\n")
	req := h.clone("pz-western2")
	req.INI = map[string]string{"PublicName": "Western 2"}
	plan, err := Make(req, h.env())
	if err != nil {
		t.Fatal(err)
	}
	if plan.GamePort != 16265 || plan.RCONPort != 27017 {
		t.Fatalf("ports should be the next free ones: %d %d", plan.GamePort, plan.RCONPort)
	}
	res, err := Apply(plan)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Stack.Ready() || len(res.Stack.Warnings) != 0 {
		t.Fatalf("generated stack: %v %v", res.Stack.Problems, res.Stack.Warnings)
	}
	ini, _ := pz.LoadINI(filepath.Join(plan.ServerDir, "western2.ini"))
	if v, _ := ini.Get("Mods"); v != "RPProfessionFramework" {
		t.Fatal("a clone keeps the mod list")
	}
	if v, _ := ini.Get("PublicName"); v != "Western 2" {
		t.Fatal("wizard changes apply to a clone too")
	}
	for _, k := range []string{"ResetID", "ServerPlayerID", "Seed"} {
		if _, ok := ini.Get(k); ok {
			t.Fatalf("a clone must not claim to be the original: %s was copied", k)
		}
	}
	if v, _ := ini.Get("RCONPassword"); v == "coalsecret" {
		t.Fatal("the clone must not inherit the source server's RCON password")
	}
	for _, f := range []string{"western2_SandboxVars.lua", "western2_spawnregions.lua"} {
		if _, err := os.Stat(filepath.Join(plan.ServerDir, f)); err != nil {
			t.Fatalf("%s not written", f)
		}
	}
	for _, d := range []string{plan.DataDir, plan.ConfigDir} {
		if _, err := os.Stat(filepath.Join(d, Marker)); err != nil {
			t.Fatalf("%s has no marker", d)
		}
	}
	info, _ := os.Stat(filepath.Join(plan.StackDir, ".env"))
	if info.Mode().Perm() != 0o600 {
		t.Fatalf(".env mode %v", info.Mode().Perm())
	}
}

func TestRefusals(t *testing.T) {
	h := newHost(t)
	env := h.env()
	cases := map[string]struct {
		req Request
		env func(Env) Env
	}{
		"unpinned image":    {h.fresh("pz-a"), func(e Env) Env { e.Image = "indifferentbroccoli/projectzomboid-server-docker"; return e }},
		"latest image":      {h.fresh("pz-a"), func(e Env) Env { e.Image = "x/y:latest"; return e }},
		"bad name":          {h.fresh("PZ Muldraugh"), nil},
		"taken container":   {h.fresh("pz-riverside"), nil},
		"clone without src": {Request{Name: "pz-a", Start: StartClone, AdminPassword: "letmein"}, nil},
		"clone missing ini": {Request{Name: "pz-a", Start: StartClone, FromDir: h.sourceDir, FromName: "nope", AdminPassword: "letmein"}, nil},
		"unknown start":     {Request{Name: "pz-a", Start: "preset", AdminPassword: "letmein"}, nil},
	}
	for name, c := range cases {
		e := env
		if c.env != nil {
			e = c.env(env)
		}
		if _, err := Make(c.req, e); err == nil {
			t.Errorf("%s: should have been refused", name)
		}
	}
	withPort := h.fresh("pz-a")
	withPort.GamePort = 16263
	if _, err := Make(withPort, env); err == nil {
		t.Error("a port in use must be refused")
	}
	for _, dir := range []string{"/etc/zomboid", "srv/zomboid/a", filepath.Join(h.dataRoot, "a b")} {
		r := h.fresh("pz-a")
		r.DataDir = dir
		if _, err := Make(r, env); err == nil {
			t.Errorf("data folder %q should be refused", dir)
		}
	}
	nested := h.fresh("pz-a")
	nested.DataDir, nested.ConfigDir = filepath.Join(h.dataRoot, "a/c/d"), filepath.Join(h.dataRoot, "a/c")
	if _, err := Make(nested, env); err == nil {
		t.Error("data inside config must be refused")
	}
	e := env
	e.Existing = append(e.Existing, stacks.Stack{Name: "pz-other", DataDir: filepath.Join(h.dataRoot, "a", "projectzomboid", "data")})
	if _, err := Make(h.fresh("pz-a"), e); err == nil {
		t.Error("sharing a data folder with another server must be refused")
	}
	if err := os.MkdirAll(filepath.Join(h.stacksRoot, "pz-exists"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := Make(h.fresh("pz-exists"), env); err == nil {
		t.Error("an existing stack folder must be refused")
	}
}

func TestReusedWorldIsNotMarkedAndSurvivesAFailedApply(t *testing.T) {
	h := newHost(t)
	world := filepath.Join(h.dataRoot, "old", "projectzomboid", "config")
	write(t, filepath.Join(world, "Saves", "Multiplayer", "old", "map.bin"), "x")
	req := h.fresh("pz-old")
	req.ConfigDir = world
	plan, err := Make(req, h.env())
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Reuses) != 1 || plan.Reuses[0] != world || len(plan.Warnings) == 0 {
		t.Fatalf("reuse not reported: %+v", plan)
	}
	// Force a failure after the folders exist: a Server/ file that cannot
	// be written.
	plan.ServerFiles = map[string]string{"sub/dir/file": "x"}
	if _, err := Apply(plan); err == nil {
		t.Fatal("expected a failure")
	}
	if _, err := os.Stat(filepath.Join(world, "Saves", "Multiplayer", "old", "map.bin")); err != nil {
		t.Fatal("rollback touched a reused folder")
	}
	if _, err := os.Stat(filepath.Join(world, Marker)); err == nil {
		t.Fatal("a reused folder must not be marked")
	}
	if _, err := os.Stat(plan.StackDir); err == nil {
		t.Fatal("rollback left the stack folder behind")
	}
	if _, err := os.Stat(filepath.Join(h.dataRoot, "old", "projectzomboid", "data")); err == nil {
		t.Fatal("rollback left the created data folder behind")
	}

	plan, err = Make(h.fresh("pz-brandnew"), h.env())
	if err != nil {
		t.Fatal(err)
	}
	plan.ServerFiles = map[string]string{"sub/dir/file": "x"}
	if _, err := Apply(plan); err == nil {
		t.Fatal("expected a failure")
	}
	if _, err := os.Stat(filepath.Join(h.dataRoot, "brandnew")); err == nil {
		t.Fatal("rollback left empty parent folders behind")
	}
}

func TestMaskedPlanHidesSecrets(t *testing.T) {
	h := newHost(t)
	plan, err := Make(h.fresh("pz-m"), h.env())
	if err != nil {
		t.Fatal(err)
	}
	m := plan.Masked()
	all := m.EnvFile
	for _, c := range m.ServerFiles {
		all += c
	}
	if strings.Contains(all, plan.adminPassword) || strings.Contains(all, plan.rconPassword) {
		t.Fatal("secrets leaked into the preview")
	}
	if !strings.Contains(plan.ServerFiles["m.ini"], plan.rconPassword) {
		t.Fatal("masking must not change the original")
	}
}

func TestEditEnv(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, ".env"), "# keep me\nRCON_PASSWORD=oldpassword\nMAX_PLAYERS=32\nMEMORY_XMX_GB=8\nDEFAULT_PORT=16261\n")

	if _, _, err := EditEnv(dir, map[string]string{"DEFAULT_PORT": "16300"}); err == nil {
		t.Fatal("ports are not editable here")
	}
	if _, _, err := EditEnv(dir, map[string]string{"MAX_PLAYERS": "lots"}); err == nil {
		t.Fatal("validation")
	}
	if _, _, err := EditEnv(dir, map[string]string{"MEMORY_XMS_GB": "12"}); err == nil {
		t.Fatal("Xms above Xmx must be refused")
	}
	if _, _, err := EditEnv(dir, map[string]string{"VM_ARGS": "-Dx=$(rm -rf /)"}); err == nil {
		t.Fatal("shell metacharacters in VM_ARGS")
	}

	prev, changed, err := EditEnv(dir, map[string]string{"MAX_PLAYERS": "40", "RCON_PASSWORD": "", "UPDATE_ON_START": "false"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(changed, ",") != "MAX_PLAYERS,UPDATE_ON_START" {
		t.Fatalf("changed: %v", changed)
	}
	if !strings.Contains(string(prev), "MAX_PLAYERS=32") {
		t.Fatal("previous content for the backup")
	}
	b, _ := os.ReadFile(filepath.Join(dir, ".env"))
	text := string(b)
	if !strings.Contains(text, "# keep me") || !strings.Contains(text, "RCON_PASSWORD=oldpassword") ||
		!strings.Contains(text, "MAX_PLAYERS=40") || !strings.Contains(text, "UPDATE_ON_START=false") {
		t.Fatalf("result:\n%s", text)
	}

	fields, _ := EnvFields(dir)
	for _, f := range fields {
		if f.Key == "RCON_PASSWORD" && f.Value != "" {
			t.Fatal("a secret was returned")
		}
	}
}

func TestLockedINIKeys(t *testing.T) {
	locked := LockedINIKeys(map[string]string{"MAX_PLAYERS": ""})
	if _, ok := locked["MaxPlayers"]; ok {
		t.Fatal("MaxPlayers is only owned by .env when MAX_PLAYERS is set")
	}
	for _, k := range []string{"DefaultPort", "UDPPort", "RCONPort", "RCONPassword"} {
		if locked[k] == "" {
			t.Fatalf("%s should be locked", k)
		}
	}
	if LockedINIKeys(map[string]string{"MAX_PLAYERS": "32"})["MaxPlayers"] == "" {
		t.Fatal("MaxPlayers should be locked when set")
	}
}
