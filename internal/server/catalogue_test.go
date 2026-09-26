package server

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/TheWarBoys2/pzadmin/internal/config"
)

func withCatalogueServer(t *testing.T) (*App, *client, string) {
	t.Helper()
	app, handler, dir := newTestApp(t)
	c := &client{t: t, handler: handler}
	c.setup("rick", "a-long-enough-password")

	// A game install plus a mod, both in the layout PZAdmin expects.
	// The installation lives inside the server's own folder, which is where a
	// per-server install sits and where PZAdmin now looks.
	game := filepath.Join(dir, "pzroot", "riverside", "install")
	mustMkdir(t, filepath.Join(game, "media", "scripts"))
	mustWrite(t, filepath.Join(game, "media", "scripts", "items.txt"),
		"module Base\n{\n\titem Axe\n\t{\n\t\tDisplayName = Axe,\n\t\tDisplayCategory = Tool,\n\t\tType = Weapon,\n\t}\n"+
			"\titem Hammer\n\t{\n\t\tDisplayName = Hammer,\n\t\tDisplayCategory = Tool,\n\t}\n}\n")
	mustWrite(t, filepath.Join(game, "media", "scripts", "vehicles.txt"),
		"module Base\n{\n\tvehicle CarNormal\n\t{\n\t\tmechanicType = 1,\n\t}\n}\n")

	srvDir := filepath.Join(dir, "pzroot", "riverside")
	mustMkdir(t, filepath.Join(srvDir, "Server"))
	mustWrite(t, filepath.Join(srvDir, "Server", "riv.ini"), "PublicName=Riverside\nMods=ModA\nMaxPlayers=16\nPVP=true\nRCONPassword=initial\n")
	mustMkdir(t, filepath.Join(srvDir, "mods", "ModA", "media", "scripts"))
	mustWrite(t, filepath.Join(srvDir, "mods", "ModA", "media", "scripts", "m.txt"),
		"module ModA\n{\n\titem Trinket\n\t{\n\t\tDisplayName = Trinket,\n\t\tType = Normal,\n\t}\n}\n")
	mustWrite(t, filepath.Join(srvDir, "Server", "kal_SandboxVars.lua"),
		"SandboxVars = {\n    Zombies = 3,\n    ZombieLore = {\n        Speed = 2,\n    },\n}\n")

	if _, err := app.cfg.Update(func(c *config.Config) error {
		c.PZRoot = filepath.Join(dir, "pzroot")
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	rec := seedServer(t, app, `{"name":"Riverside","host":"127.0.0.1","rconPort":27015,
	  "rconPassword":"x","enabled":false,"pzPath":"riverside"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("save failed: %s", rec.Body.String())
	}
	return app, c, app.cfg.Get().Servers[0].ID
}

func TestCatalogueReadsGameAndModScripts(t *testing.T) {
	_, c, id := withCatalogueServer(t)

	rec := c.do(http.MethodGet, "/api/catalogue?id="+id, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("catalogue failed: %s", rec.Body.String())
	}
	var out struct {
		Items    []map[string]any `json:"items"`
		Vehicles []map[string]any `json:"vehicles"`
		Perks    []map[string]any `json:"perks"`
		GameRoot string           `json:"gameRoot"`
		Note     string           `json:"note"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}

	ids := map[string]bool{}
	for _, e := range out.Items {
		ids[e["id"].(string)] = true
	}
	for _, want := range []string{"Base.Axe", "Base.Hammer", "ModA.Trinket"} {
		if !ids[want] {
			t.Fatalf("%s missing from the item list: %v", want, ids)
		}
	}
	if len(out.Vehicles) != 1 || out.Vehicles[0]["id"] != "Base.CarNormal" {
		t.Fatalf("unexpected vehicles %v", out.Vehicles)
	}
	if len(out.Perks) < 20 {
		t.Fatalf("the skill list looks short: %d", len(out.Perks))
	}
	if out.GameRoot == "" {
		t.Fatal("the resolved game files path should be reported so the setting is verifiable")
	}
	if out.Note != "" {
		t.Fatalf("no warning should be shown when the scan worked: %q", out.Note)
	}
}

func TestCatalogueExplainsItselfWithoutGameFiles(t *testing.T) {
	app, c, id := withCatalogueServer(t)
	// Remove the installation so only the mods remain.
	if err := os.RemoveAll(filepath.Join(app.cfg.Get().PZRoot, "riverside", "install")); err != nil {
		t.Fatal(err)
	}
	app.scripts.Invalidate()

	rec := c.do(http.MethodGet, "/api/catalogue?id="+id+"&refresh=1", nil)
	var out struct {
		Items []map[string]any `json:"items"`
		Note  string           `json:"note"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)

	// Mod items still come through; vanilla does not, and it says why.
	found := false
	for _, e := range out.Items {
		if e["id"] == "ModA.Trinket" {
			found = true
		}
		if e["id"] == "Base.Axe" {
			t.Fatal("vanilla items should not appear without the game files")
		}
	}
	if !found {
		t.Fatal("mod items should still be listed")
	}
	if out.Note == "" {
		t.Fatal("an incomplete list must explain itself rather than just look broken")
	}
}

func TestCustomCatalogueEntries(t *testing.T) {
	app, c, id := withCatalogueServer(t)

	// Bad entries are refused with an explanation.
	for _, bad := range []string{
		`{"id":"","name":"x","kind":"item","category":""}`,
		`{"id":"NoModulePrefix","name":"x","kind":"item","category":""}`,
		`{"id":"Base.Thing","name":"x","kind":"nonsense","category":""}`,
		`{"id":"Base.With Space","name":"x","kind":"item","category":""}`,
	} {
		if rec := c.raw("/api/catalogue/add", bad); rec.Code != http.StatusBadRequest {
			t.Fatalf("%s should be refused, got %d", bad, rec.Code)
		}
	}

	if rec := c.raw("/api/catalogue/add",
		`{"id":"MyMod.Sword","name":"Great Sword","kind":"item","category":"Weapons"}`); rec.Code != http.StatusOK {
		t.Fatalf("add failed: %s", rec.Body.String())
	}
	// A skill needs no module prefix.
	if rec := c.raw("/api/catalogue/add",
		`{"id":"Blacksmithing","name":"Blacksmithing","kind":"perk","category":"Crafting"}`); rec.Code != http.StatusOK {
		t.Fatalf("perk add failed: %s", rec.Body.String())
	}

	rec := c.do(http.MethodGet, "/api/catalogue?id="+id, nil)
	var out struct {
		Items []map[string]any `json:"items"`
		Perks []map[string]any `json:"perks"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)

	found := false
	for _, e := range out.Items {
		if e["id"] == "MyMod.Sword" && e["source"] == "custom" {
			found = true
		}
	}
	if !found {
		t.Fatal("a custom item should appear in the merged list")
	}

	// It survives a restart.
	reloaded := loadCustomCatalogue(filepath.Join(app.dataDir, "catalogue.json"))
	if len(reloaded.list()) != 2 {
		t.Fatalf("custom entries should persist, got %d", len(reloaded.list()))
	}

	if rec := c.raw("/api/catalogue/remove", `{"id":"MyMod.Sword","kind":"item"}`); rec.Code != http.StatusOK {
		t.Fatalf("remove failed: %s", rec.Body.String())
	}
	if rec := c.raw("/api/catalogue/remove", `{"id":"MyMod.Sword","kind":"item"}`); rec.Code != http.StatusNotFound {
		t.Fatal("removing twice should report that it is gone")
	}
}

func TestConfigFieldsAndApply(t *testing.T) {
	_, c, id := withCatalogueServer(t)

	rec := c.do(http.MethodGet, "/api/server/config/fields?id="+id+"&file=riv.ini", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("fields failed: %s", rec.Body.String())
	}
	var fields struct {
		Kind       string `json:"kind"`
		Reloadable bool   `json:"reloadable"`
		Groups     []struct {
			Name   string           `json:"name"`
			Fields []map[string]any `json:"fields"`
		} `json:"groups"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &fields); err != nil {
		t.Fatal(err)
	}
	if fields.Kind != "ini" || !fields.Reloadable {
		t.Fatalf("unexpected metadata %#v", fields)
	}
	byKey := map[string]map[string]any{}
	for _, g := range fields.Groups {
		for _, f := range g.Fields {
			byKey[f["key"].(string)] = f
		}
	}
	if byKey["MaxPlayers"]["type"] != "int" || byKey["PVP"]["type"] != "bool" {
		t.Fatalf("types not inferred: %v", byKey)
	}
	if byKey["Mods"]["applies"] != "restart" {
		t.Fatal("Mods should be flagged as needing a restart")
	}

	// Applying a change writes only that key.
	rec = c.raw("/api/server/config/apply",
		`{"serverId":"`+id+`","file":"riv.ini","changes":{"MaxPlayers":"32"},"reload":false}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("apply failed: %s", rec.Body.String())
	}
	body := rec.Body.String()
	if !contains(body, "\"changed\":1") {
		t.Fatalf("expected one change, got %s", body)
	}

	// And the change is visible when the fields are read back.
	rec = c.do(http.MethodGet, "/api/server/config/fields?id="+id+"&file=riv.ini", nil)
	if !contains(rec.Body.String(), `"value":"32"`) {
		t.Fatalf("the new value should be readable: %s", rec.Body.String())
	}
}

func TestConfigApplyValidatesBeforeWriting(t *testing.T) {
	_, c, id := withCatalogueServer(t)

	// Out of range, wrong type, and unknown keys are all refused.
	for _, bad := range []string{
		`{"serverId":"` + id + `","file":"riv.ini","changes":{"MaxPlayers":"9999"}}`,
		`{"serverId":"` + id + `","file":"riv.ini","changes":{"PVP":"maybe"}}`,
		`{"serverId":"` + id + `","file":"riv.ini","changes":{"NotASetting":"1"}}`,
		`{"serverId":"` + id + `","file":"../escape.ini","changes":{"A":"1"}}`,
	} {
		if rec := c.raw("/api/server/config/apply", bad); rec.Code != http.StatusBadRequest {
			t.Fatalf("%s should be refused, got %d %s", bad, rec.Code, rec.Body.String())
		}
	}

	// A batch containing one bad value must write nothing at all.
	rec := c.raw("/api/server/config/apply",
		`{"serverId":"`+id+`","file":"riv.ini","changes":{"MaxPlayers":"24","PVP":"maybe"}}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected the batch to be refused, got %d", rec.Code)
	}
	fieldsRec := c.do(http.MethodGet, "/api/server/config/fields?id="+id+"&file=riv.ini", nil)
	if contains(fieldsRec.Body.String(), `"value":"24"`) {
		t.Fatal("a rejected batch must not have written its valid half")
	}
}

func TestSandboxFieldsAreRestartOnly(t *testing.T) {
	_, c, id := withCatalogueServer(t)

	rec := c.do(http.MethodGet, "/api/server/config/fields?id="+id+"&file=kal_SandboxVars.lua", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("sandbox fields failed: %s", rec.Body.String())
	}
	var out struct {
		Kind       string `json:"kind"`
		Reloadable bool   `json:"reloadable"`
		Groups     []struct {
			Name   string           `json:"name"`
			Fields []map[string]any `json:"fields"`
		} `json:"groups"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if out.Kind != "sandbox" || out.Reloadable {
		t.Fatalf("sandbox settings cannot be reloaded live: %#v", out)
	}
	found := false
	for _, g := range out.Groups {
		for _, f := range g.Fields {
			if f["key"] == "ZombieLore.Speed" {
				found = true
				if f["applies"] != "restart" {
					t.Fatal("sandbox values are read at world load")
				}
			}
		}
	}
	if !found {
		t.Fatal("nested sandbox settings should be listed")
	}

	rec = c.raw("/api/server/config/apply",
		`{"serverId":"`+id+`","file":"kal_SandboxVars.lua","changes":{"ZombieLore.Speed":"3"}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("sandbox apply failed: %s", rec.Body.String())
	}
	if !contains(rec.Body.String(), "restart") {
		t.Fatalf("the response should say a restart is needed: %s", rec.Body.String())
	}
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && (func() bool {
		for i := 0; i+len(needle) <= len(haystack); i++ {
			if haystack[i:i+len(needle)] == needle {
				return true
			}
		}
		return false
	})()
}

func TestCatalogueListsAreNeverNull(t *testing.T) {
	app, c, id := withCatalogueServer(t)
	// Remove every source so the lists are genuinely empty.
	root := app.cfg.Get().PZRoot
	if err := os.RemoveAll(filepath.Join(root, "riverside", "install")); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(root, "riverside", "mods")); err != nil {
		t.Fatal(err)
	}
	app.scripts.Invalidate()

	rec := c.do(http.MethodGet, "/api/catalogue?id="+id+"&refresh=1", nil)
	body := rec.Body.String()
	for _, key := range []string{`"vehicles":null`, `"items":null`, `"sources":null`, `"perks":null`} {
		if contains(body, key) {
			t.Fatalf("%s would force every caller to guard against null: %s", key, body)
		}
	}
}

// The game server's own RCON password lives in the ini. The form must not
// carry it to the browser, and leaving the box empty must not blank it.
func TestConfigFormDoesNotLeakSecrets(t *testing.T) {
	_, c, id := withCatalogueServer(t)

	// Put a password in the file first.
	if rec := c.raw("/api/server/config/apply",
		`{"serverId":"`+id+`","file":"riv.ini","changes":{"RCONPassword":"hunter2-the-rcon-password"}}`); rec.Code != http.StatusOK {
		t.Fatalf("apply failed: %s", rec.Body.String())
	}

	rec := c.do(http.MethodGet, "/api/server/config/fields?id="+id+"&file=riv.ini", nil)
	if contains(rec.Body.String(), "hunter2-the-rcon-password") {
		t.Fatalf("the form leaked the RCON password: %s", rec.Body.String())
	}

	// An empty secret means "unchanged", not "erase it".
	if rec := c.raw("/api/server/config/apply",
		`{"serverId":"`+id+`","file":"riv.ini","changes":{"RCONPassword":"","MaxPlayers":"20"}}`); rec.Code != http.StatusOK {
		t.Fatalf("apply failed: %s", rec.Body.String())
	}
	// The raw editor hides passwords too, so check the file itself.
	resp := decode(t, c.do(http.MethodGet, "/api/server/config?id="+id+"&file=riv.ini", nil))
	raw, _ := resp["content"].(string)
	if contains(raw, "hunter2-the-rcon-password") {
		t.Fatalf("the raw editor leaked the RCON password: %s", raw)
	}
	onDisk, err := os.ReadFile(filepath.Join(resp["dir"].(string), "riv.ini"))
	if err != nil {
		t.Fatal(err)
	}
	if !contains(string(onDisk), "hunter2-the-rcon-password") {
		t.Fatalf("an untouched password box must not wipe the stored value: %s", onDisk)
	}
	if !contains(raw, "MaxPlayers=20") {
		t.Fatal("the other change in the same batch should still have been applied")
	}
}
