package server

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTemplateFromServerMakesANewServer(t *testing.T) {
	app, handler, stacksRoot, dataRoot := discoveryApp(t, func(o *Options) { o.GameImage = testImage })
	addStack(t, stacksRoot, dataRoot, "pz-coalfield", "unless-stopped")
	app.rescan()
	c := &client{t: t, handler: handler}
	c.setup("rick", "a-long-enough-password")

	// Save the server as it is today.
	rec := c.do(http.MethodPost, "/api/templates/create", map[string]any{
		"name": "Coalfield rules", "description": "PVP on", "start": "clone", "fromServerId": "pz-coalfield",
		"basics": map[string]any{"maxPlayers": 12, "memoryGb": 6},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("create template: %d %s", rec.Code, rec.Body.String())
	}
	var created struct {
		Template templateSummary `json:"template"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &created)
	id := created.Template.ID
	if id == "" || created.Template.Mods != 1 || created.Template.CreatedFrom != "pz-coalfield" {
		t.Fatalf("summary: %+v", created.Template)
	}

	// The source's RCON password is not kept.
	raw, err := os.ReadFile(filepath.Join(app.dataDir, "templates.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "RCONPassword=pw") {
		t.Fatalf("the template kept the source's RCON password: %s", raw)
	}

	// A second template with the same name is refused.
	rec = c.do(http.MethodPost, "/api/templates/create", map[string]any{
		"name": "coalfield RULES", "start": "defaults",
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("duplicate name: %d %s", rec.Code, rec.Body.String())
	}

	// The wizard lists it and reads its settings.
	rec = c.do(http.MethodGet, "/api/stack/new", nil)
	if !strings.Contains(rec.Body.String(), `"name":"Coalfield rules"`) {
		t.Fatalf("templates not offered: %s", rec.Body.String())
	}
	rec = c.do(http.MethodGet, "/api/stack/wizard/fields?start=template&template="+id, nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"from":"the Coalfield rules template"`) {
		t.Fatalf("fields: %d %s", rec.Code, rec.Body.String())
	}

	// Editing the template changes what new servers start from.
	rec = c.do(http.MethodPost, "/api/templates/update", map[string]any{
		"id": id, "name": "Coalfield rules", "description": "PVP off now",
		"ini": map[string]string{"PVP": "false"}, "basics": map[string]any{"maxPlayers": 12},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("update: %d %s", rec.Code, rec.Body.String())
	}

	// A server made from it, with a tweak of its own.
	body := map[string]any{"name": "pz-western3", "start": "template", "templateId": id,
		"adminPassword": "letmein", "sandbox": map[string]string{"Zombies": "4"}}
	rec = c.do(http.MethodPost, "/api/stack/create", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("create from template: %d %s", rec.Code, rec.Body.String())
	}
	ini, _ := os.ReadFile(filepath.Join(stacksRoot, "pz-western3", "Server", "western3.ini"))
	sandbox, _ := os.ReadFile(filepath.Join(stacksRoot, "pz-western3", "Server", "western3_SandboxVars.lua"))
	if !strings.Contains(string(ini), "PVP=false") || !strings.Contains(string(ini), "Mods=A") ||
		strings.Contains(string(ini), "RCONPassword=pw\n") || !strings.Contains(string(sandbox), "Zombies = 4") {
		t.Fatalf("files:\n%s\n%s", ini, sandbox)
	}

	// Deleting it leaves the server alone.
	rec = c.do(http.MethodPost, "/api/templates/delete", map[string]any{"id": id})
	if rec.Code != http.StatusOK {
		t.Fatalf("delete: %d %s", rec.Code, rec.Body.String())
	}
	if _, err := os.Stat(filepath.Join(stacksRoot, "pz-western3", "Server", "western3.ini")); err != nil {
		t.Fatal(err)
	}
	rec = c.do(http.MethodGet, "/api/templates", nil)
	if strings.Contains(rec.Body.String(), "Coalfield") {
		t.Fatalf("still listed: %s", rec.Body.String())
	}
}

func TestTemplateFromWizardChoicesSurvivesARestart(t *testing.T) {
	app, handler, _, _ := discoveryApp(t, func(o *Options) { o.GameImage = testImage })
	c := &client{t: t, handler: handler}
	c.setup("rick", "a-long-enough-password")

	rec := c.do(http.MethodPost, "/api/templates/create", map[string]any{
		"name": "Hard mode", "start": "defaults",
		"ini": map[string]string{"PVP": "false"}, "sandbox": map[string]string{"Zombies": "1"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"createdFrom":"the game defaults"`) {
		t.Fatalf("created from: %s", rec.Body.String())
	}
	// Bad settings are refused the same way the wizard refuses them.
	rec = c.do(http.MethodPost, "/api/templates/create", map[string]any{
		"name": "Broken", "start": "defaults", "ini": map[string]string{"NoSuchSetting": "1"},
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad setting: %d %s", rec.Code, rec.Body.String())
	}

	again := newTemplateStore(filepath.Join(app.dataDir, "templates.json"))
	list := again.all()
	if again.loadErr != nil || len(list) != 1 || list[0].Name != "Hard mode" ||
		!strings.Contains(list[0].Files.INI, "PVP=false") {
		t.Fatalf("reloaded: %v %+v", again.loadErr, list)
	}
}

func TestTemplatesAreTheOwnersAlone(t *testing.T) {
	_, handler, _, _ := discoveryApp(t, func(o *Options) { o.GameImage = testImage })
	owner := &client{t: t, handler: handler}
	owner.setup("rick", "a-long-enough-password")
	_, temp := addUser(t, owner, map[string]any{"name": "glenn"})
	glenn := signInUser(t, handler, "glenn", temp)
	for _, path := range []string{"/api/templates/create", "/api/templates/update", "/api/templates/delete"} {
		if rec := glenn.do(http.MethodPost, path, map[string]any{"name": "x", "start": "defaults"}); rec.Code != http.StatusForbidden {
			t.Fatalf("%s: %d %s", path, rec.Code, rec.Body.String())
		}
	}
	if rec := glenn.do(http.MethodGet, "/api/templates", nil); rec.Code != http.StatusForbidden {
		t.Fatalf("list: %d", rec.Code)
	}
}
