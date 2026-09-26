package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/TheWarBoys2/pzadmin/internal/config"
	"github.com/TheWarBoys2/pzadmin/internal/pz"
)

// modServer builds a server whose ini is deliberately in the broken state Rick
// described: a mod enabled in Mods= whose Workshop ID never made it into
// WorkshopItems=, so Steam never downloads it.
func modServer(t *testing.T) (*App, *client, string, string) {
	t.Helper()
	dir := t.TempDir()
	app, err := New(Options{
		SetupCode: testSetupCode,
		DataDir:   dir,
		Assets:    fstest.MapFS{"index.html": &fstest.MapFile{Data: []byte("app")}},
		SteamBase: fakeSteamServer(t),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(app.Close)
	handler := app.Handler()
	c := &client{t: t, handler: handler}
	c.setup("rick", "a-long-enough-password")

	base := filepath.Join(dir, "pzroot", "riverside")
	mustMkdir(t, filepath.Join(base, "Server"))
	mustWrite(t, filepath.Join(base, "Server", "riv.ini"), strings.Join([]string{
		"PublicName=Riverside",
		"Mods=Tsarslib;TrueActionsDancing;GhostMod",
		"WorkshopItems=2392709985",
		"Map=Muldraugh, KY",
	}, "\n"))

	// Two of the three are on disk, in the Workshop layout.
	workshop := filepath.Join(base, "steamapps", "workshop", "content", "108600")
	mustMkdir(t, filepath.Join(workshop, "2392709985", "mods", "Tsarslib"))
	mustWrite(t, filepath.Join(workshop, "2392709985", "mods", "Tsarslib", "mod.info"),
		"name=Tsar's Common Library\nid=Tsarslib\ncategory=resource\n")
	mustMkdir(t, filepath.Join(workshop, "2169435993", "mods", "TrueActionsDancing"))
	mustWrite(t, filepath.Join(workshop, "2169435993", "mods", "TrueActionsDancing", "mod.info"),
		"name=True Actions Dancing\nid=TrueActionsDancing\nrequire=Tsarslib\n")
	// And one on disk that nothing enables.
	mustMkdir(t, filepath.Join(workshop, "111", "mods", "SpareMod"))
	mustWrite(t, filepath.Join(workshop, "111", "mods", "SpareMod", "mod.info"),
		"name=Spare\nid=SpareMod\n")

	if _, err := app.cfg.Update(func(cfg *config.Config) error {
		cfg.PZRoot = filepath.Join(dir, "pzroot")
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	rec := seedServer(t, app, `{"name":"Riverside","host":"127.0.0.1","rconPort":27015,
	  "rconPassword":"x","enabled":false,"pzPath":"riverside"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("save failed: %s", rec.Body.String())
	}
	return app, c, app.cfg.Get().Servers[0].ID, filepath.Join(base, "Server", "riv.ini")
}

func fakeSteamServer(t *testing.T) string {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/ISteamRemoteStorage/GetPublishedFileDetails/v1/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"response":{"result":1,"publishedfiledetails":[{
		  "publishedfileid":"2169435993","result":1,"title":"True Actions Dancing",
		  "description":"Dance.\n\nWorkshop ID: 2169435993\nMod ID: TrueActionsDancing",
		  "time_updated":1700000000,"file_size":"2048","consumer_app_id":108600}]}}`))
	})
	mux.HandleFunc("/ISteamRemoteStorage/GetCollectionDetails/v1/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"response":{"collectiondetails":[{"publishedfileid":"1","result":9}]}}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv.URL
}

func TestModManagerReportsTheWorkshopMismatch(t *testing.T) {
	_, c, id, _ := modServer(t)

	rec := c.do(http.MethodGet, "/api/mods/manage?id="+id, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("manage failed: %s", rec.Body.String())
	}
	var out struct {
		Mods      []ModEntry `json:"mods"`
		Available []ModEntry `json:"available"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	byID := map[string]ModEntry{}
	for _, m := range out.Mods {
		byID[m.ID] = m
	}

	// On disk and listed: no complaint.
	if e := byID["Tsarslib"]; !e.Installed || !e.WorkshopListed || e.Problem != "" {
		t.Fatalf("Tsarslib should be clean: %#v", e)
	}
	// On disk but its Workshop ID never made it into WorkshopItems.
	e := byID["TrueActionsDancing"]
	if !e.Installed || e.WorkshopListed {
		t.Fatalf("TrueActionsDancing should be installed but unlisted: %#v", e)
	}
	if e.WorkshopID != "2169435993" {
		t.Fatalf("the Workshop ID should be read from the folder path: %#v", e)
	}
	if !strings.Contains(e.Problem, "WorkshopItems") {
		t.Fatalf("the problem should name the missing line: %q", e.Problem)
	}
	// Enabled, not on disk, and nothing knows its ID.
	if p := byID["GhostMod"].Problem; !strings.Contains(p, "Not on disk") {
		t.Fatalf("GhostMod should be flagged: %q", p)
	}
	// And the spare mod is offered rather than hidden.
	if len(out.Available) != 1 || out.Available[0].ID != "SpareMod" {
		t.Fatalf("unenabled installed mods should be listed: %#v", out.Available)
	}
}

func TestModApplyWritesBothLinesAndBacksUp(t *testing.T) {
	app, c, id, iniPath := modServer(t)

	rec := c.raw("/api/mods/apply", `{"serverId":"`+id+`",
	  "mods":["Tsarslib","TrueActionsDancing"],
	  "workshopItems":["2392709985","2169435993"]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("apply failed: %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "restart") {
		t.Fatalf("the response must say a restart is needed: %s", rec.Body.String())
	}

	written := readAll(t, iniPath)
	if !strings.Contains(written, "Mods=Tsarslib;TrueActionsDancing") {
		t.Fatalf("mods line wrong:\n%s", written)
	}
	if !strings.Contains(written, "WorkshopItems=2392709985;2169435993") {
		t.Fatalf("workshop line wrong:\n%s", written)
	}
	// Everything else in the file must be untouched.
	if !strings.Contains(written, "PublicName=Riverside") || !strings.Contains(written, "Map=Muldraugh, KY") {
		t.Fatalf("unrelated settings were disturbed:\n%s", written)
	}
	// GhostMod was dropped, and the response says so.
	if !strings.Contains(rec.Body.String(), "GhostMod") {
		t.Fatalf("removals should be reported: %s", rec.Body.String())
	}

	backups, _ := os.ReadDir(filepath.Join(app.backup.Dir(id), "config"))
	if len(backups) == 0 {
		t.Fatal("the previous ini must be kept before a mod change")
	}
}

func TestModApplyRejectsDangerousValues(t *testing.T) {
	_, c, id, iniPath := modServer(t)
	before := readAll(t, iniPath)

	bad := []string{
		`{"serverId":"` + id + `","mods":["Good;Injected"],"workshopItems":[]}`,
		`{"serverId":"` + id + `","mods":["Good\nPVP=true"],"workshopItems":[]}`,
		`{"serverId":"` + id + `","mods":["Good"],"workshopItems":["notanumber"]}`,
		`{"serverId":"` + id + `","mods":["Good"],"workshopItems":["123;456"]}`,
	}
	for _, payload := range bad {
		if rec := c.raw("/api/mods/apply", payload); rec.Code != http.StatusBadRequest {
			t.Fatalf("%s should be refused, got %d %s", payload, rec.Code, rec.Body.String())
		}
	}
	if readAll(t, iniPath) != before {
		t.Fatal("a rejected request must not have written anything")
	}
}

func TestModApplyDeduplicates(t *testing.T) {
	_, c, id, iniPath := modServer(t)
	rec := c.raw("/api/mods/apply", `{"serverId":"`+id+`",
	  "mods":["Tsarslib","tsarslib","Other"],"workshopItems":["111","111"]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("apply failed: %s", rec.Body.String())
	}
	written := readAll(t, iniPath)
	if !strings.Contains(written, "Mods=Tsarslib;Other") {
		t.Fatalf("duplicates should collapse, keeping the first spelling:\n%s", written)
	}
	if !strings.Contains(written, "WorkshopItems=111\n") && !strings.HasSuffix(strings.TrimSpace(written), "WorkshopItems=111") {
		if !strings.Contains(written, "WorkshopItems=111") {
			t.Fatalf("workshop duplicates should collapse:\n%s", written)
		}
	}
}

func TestModSortPreviewDoesNotWrite(t *testing.T) {
	_, c, id, iniPath := modServer(t)
	before := readAll(t, iniPath)

	rec := c.raw("/api/mods/sort",
		`{"serverId":"`+id+`","mods":["TrueActionsDancing","Tsarslib"]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("sort failed: %s", rec.Body.String())
	}
	var out struct {
		Result struct {
			Order   []string `json:"order"`
			Changed bool     `json:"changed"`
		} `json:"result"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)

	// TrueActionsDancing requires Tsarslib, so the library has to move first.
	if !out.Result.Changed || out.Result.Order[0] != "Tsarslib" {
		t.Fatalf("dependency order not applied: %#v", out.Result)
	}
	if readAll(t, iniPath) != before {
		t.Fatal("previewing a sort must not write to the config")
	}
}

func TestModResolveFromAWorkshopLink(t *testing.T) {
	_, c, id, _ := modServer(t)

	rec := c.raw("/api/mods/resolve", `{"serverId":"`+id+`",
	  "text":"https://steamcommunity.com/sharedfiles/filedetails/?id=2169435993"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("resolve failed: %s", rec.Body.String())
	}
	var out struct {
		Items []struct {
			WorkshopID    string   `json:"workshopId"`
			Title         string   `json:"title"`
			ModIDs        []string `json:"modIds"`
			AlreadyListed bool     `json:"alreadyListed"`
			ModIDsKnown   []bool   `json:"modIdsKnown"`
		} `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Items) != 1 {
		t.Fatalf("expected one item, got %d", len(out.Items))
	}
	item := out.Items[0]
	if item.Title != "True Actions Dancing" || item.WorkshopID != "2169435993" {
		t.Fatalf("unexpected item %#v", item)
	}
	if len(item.ModIDs) != 1 || item.ModIDs[0] != "TrueActionsDancing" {
		t.Fatalf("the mod ID should come from the description: %#v", item.ModIDs)
	}
	// It is already in Mods=, so the dialog can say so instead of duplicating.
	if len(item.ModIDsKnown) != 1 || !item.ModIDsKnown[0] {
		t.Fatalf("an already-enabled mod should be marked: %#v", item)
	}
	if item.AlreadyListed {
		t.Fatal("its Workshop ID is not in WorkshopItems, so it is not already listed")
	}
}

func TestModResolveRejectsNonsense(t *testing.T) {
	_, c, id, _ := modServer(t)
	rec := c.raw("/api/mods/resolve", `{"serverId":"`+id+`","text":"have a nice day"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected a helpful refusal, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "Workshop link") {
		t.Fatalf("the message should say what to paste: %s", rec.Body.String())
	}
}

func readAll(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// One Workshop item, three mods, folder names that do not match their IDs.
// Everything must resolve, and the shared Workshop ID must be visible so that
// removing one mod does not look like it should remove the download.
func TestBundledWorkshopItemIsUnderstood(t *testing.T) {
	dir := t.TempDir()
	app, err := New(Options{
		SetupCode: testSetupCode,
		DataDir:   dir,
		Assets:    fstest.MapFS{"index.html": &fstest.MapFile{Data: []byte("app")}},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(app.Close)
	c := &client{t: t, handler: app.Handler()}
	c.setup("rick", "a-long-enough-password")

	base := filepath.Join(dir, "pzroot", "riverside")
	ws := filepath.Join(base, "steamapps", "workshop", "content", "108600", "2392709985", "mods")
	mustMkdir(t, filepath.Join(base, "Server"))
	for folder, info := range map[string]string{
		"TsarsLibFolder": "name=Tsar's Library\nid=Tsarslib\n",
		"Dancing":        "name=Dancing\nid=TrueActionsDancing\n",
		"Extra":          "name=Extra\nid=TrueActionsExtra\n",
	} {
		mustMkdir(t, filepath.Join(ws, folder))
		mustWrite(t, filepath.Join(ws, folder, "mod.info"), info)
	}
	mustWrite(t, filepath.Join(base, "Server", "riv.ini"),
		"Mods=Tsarslib;TrueActionsDancing;Tsars_Lib\nWorkshopItems=2392709985\n")

	if _, err := app.cfg.Update(func(cfg *config.Config) error {
		cfg.PZRoot = filepath.Join(dir, "pzroot")
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	seedServer(t, app, `{"name":"Riverside","host":"127.0.0.1","rconPort":27015,
	  "rconPassword":"x","enabled":false,"pzPath":"riverside"}`)
	id := app.cfg.Get().Servers[0].ID

	rec := c.do(http.MethodGet, "/api/mods/manage?id="+id, nil)
	var out struct {
		Mods      []ModEntry `json:"mods"`
		Available []ModEntry `json:"available"`
		Bundles   []Bundle   `json:"bundles"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	byID := map[string]ModEntry{}
	for _, m := range out.Mods {
		byID[m.ID] = m
	}

	// Declared IDs resolve even though the folders are named differently.
	for _, id := range []string{"Tsarslib", "TrueActionsDancing"} {
		e := byID[id]
		if !e.Installed {
			t.Fatalf("%s is on disk under a differently named folder and should resolve: %#v", id, e)
		}
		if e.WorkshopID != "2392709985" {
			t.Fatalf("%s should carry the shared Workshop ID: %#v", id, e)
		}
		if e.Problem != "" {
			t.Fatalf("%s should have no problem: %q", id, e.Problem)
		}
	}
	// Siblings are surfaced, so the shared download is visible.
	if len(byID["Tsarslib"].Siblings) != 2 {
		t.Fatalf("the other mods in the same Workshop item should be listed: %#v", byID["Tsarslib"])
	}
	// The folder name is recorded, since it is what is actually on disk.
	if byID["Tsarslib"].Folder != "TsarsLibFolder" {
		t.Fatalf("folder not recorded: %#v", byID["Tsarslib"])
	}

	// The third entry is a typo. It should be flagged, and the near miss named,
	// rather than leaving the operator to hunt through folders.
	typo := byID["Tsars_Lib"]
	if typo.Installed {
		t.Fatalf("Tsars_Lib is not a real mod ID: %#v", typo)
	}
	// Tsarslib is also enabled, so this is recognised as the same mod spelled
	// differently, which is a more useful thing to say than "not found".
	if !strings.Contains(typo.Problem, "Tsarslib") {
		t.Fatalf("the real entry should be named: %q", typo.Problem)
	}
	if typo.Fix == nil {
		t.Fatal("a one-click fix should be offered for a duplicate spelling")
	}

	// TrueActionsExtra is on disk in the same bundle but not enabled.
	found := false
	for _, m := range out.Available {
		if m.ID == "TrueActionsExtra" {
			found = true
			if len(m.Siblings) != 2 {
				t.Fatalf("its bundle siblings should be listed: %#v", m)
			}
		}
	}
	if !found {
		t.Fatalf("the unenabled bundled mod should be offered: %#v", out.Available)
	}

	// And the bundle itself is reported.
	if len(out.Bundles) != 1 || len(out.Bundles[0].Mods) != 3 {
		t.Fatalf("expected one Workshop item providing three mods: %#v", out.Bundles)
	}
	if !out.Bundles[0].Listed {
		t.Fatal("the Workshop item is in WorkshopItems and should be marked as listed")
	}
}

func TestClosestModIDNamesTheNearMiss(t *testing.T) {
	installed := []pz.Mod{
		{ID: "TrueActionsDancing", Declared: "TrueActionsDancing", Folder: "Dancing", Installed: true},
		{ID: "Tsarslib", Declared: "Tsarslib", Folder: "TsarsLibFolder", Installed: true},
		// No mod.info, so nothing is declared and nothing may be suggested.
		{ID: "Mystery Mod", Folder: "Mystery Mod", Installed: true},
	}
	cases := map[string]string{
		"trueactionsdancing":   "TrueActionsDancing",
		"True_Actions_Dancing": "TrueActionsDancing",
		"TsarsLib":             "Tsarslib",
		"tsarslibfolder":       "Tsarslib",
		"SomethingElse":        "",
		"":                     "",
		// A folder-derived name is a guess, not an answer, so it is withheld.
		"MysteryMod": "",
	}
	for want, expect := range cases {
		if got := closestModID(want, installed); got != expect {
			t.Fatalf("closestModID(%q) = %q, want %q", want, got, expect)
		}
	}
}

// Two entries naming the same mod, one of which resolves. PZAdmin has to say
// they are the same thing and which one is authoritative, rather than showing
// one as broken and the other as fine with no explanation.
func TestDuplicateSpellingIsExplained(t *testing.T) {
	dir := t.TempDir()
	app, err := New(Options{
		SetupCode: testSetupCode,
		DataDir:   dir,
		Assets:    fstest.MapFS{"index.html": &fstest.MapFile{Data: []byte("app")}},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(app.Close)
	c := &client{t: t, handler: app.Handler()}
	c.setup("rick", "a-long-enough-password")

	base := filepath.Join(dir, "pzroot", "riverside")
	ws := filepath.Join(base, "steamapps", "workshop", "content", "108600", "2875848298", "mods")
	mustMkdir(t, filepath.Join(base, "Server"))
	mustMkdir(t, filepath.Join(ws, "Improvised Silencers"))
	mustWrite(t, filepath.Join(ws, "Improvised Silencers", "mod.info"),
		"name=Improvised Silencers\nid=Improvised Silencers\n")
	mustWrite(t, filepath.Join(base, "Server", "riv.ini"),
		"Mods=ImprovisedSilencers;Improvised Silencers\nWorkshopItems=2875848298\n")

	if _, err := app.cfg.Update(func(cfg *config.Config) error {
		cfg.PZRoot = filepath.Join(dir, "pzroot")
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	seedServer(t, app, `{"name":"Riverside","host":"127.0.0.1","rconPort":27015,
	  "rconPassword":"x","enabled":false,"pzPath":"riverside"}`)
	id := app.cfg.Get().Servers[0].ID

	rec := c.do(http.MethodGet, "/api/mods/manage?id="+id, nil)
	var out struct {
		Mods []ModEntry `json:"mods"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	byID := map[string]ModEntry{}
	for _, m := range out.Mods {
		byID[m.ID] = m
	}

	// The spelling that matches disk is the good one, and it says so.
	good := byID["Improvised Silencers"]
	if !good.Installed || good.WorkshopID != "2875848298" {
		t.Fatalf("the matching entry should resolve: %#v", good)
	}
	if good.DeclaredID != "Improvised Silencers" {
		t.Fatalf("the declared ID should be reported so the authority is visible: %#v", good)
	}
	if good.Problem != "" {
		t.Fatalf("the correct entry should have no complaint: %q", good.Problem)
	}

	// The other is named as a duplicate of it, with a one-click removal.
	dup := byID["ImprovisedSilencers"]
	if dup.Installed {
		t.Fatalf("the unmatched spelling should not resolve: %#v", dup)
	}
	if !strings.Contains(dup.Problem, "same mod as Improvised Silencers") {
		t.Fatalf("the duplicate should be explained: %q", dup.Problem)
	}
	if dup.Fix == nil || dup.Fix.Action != "remove" {
		t.Fatalf("a removal should be offered: %#v", dup.Fix)
	}
}

// When the entry resolves via the folder but the mod declares a different ID,
// say so: Project Zomboid matches on the declared ID.
func TestEntryMatchingByFolderIsCorrected(t *testing.T) {
	dir := t.TempDir()
	app, err := New(Options{
		SetupCode: testSetupCode,
		DataDir:   dir,
		Assets:    fstest.MapFS{"index.html": &fstest.MapFile{Data: []byte("app")}},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(app.Close)
	c := &client{t: t, handler: app.Handler()}
	c.setup("rick", "a-long-enough-password")

	base := filepath.Join(dir, "pzroot", "riverside")
	mustMkdir(t, filepath.Join(base, "Server"))
	mustMkdir(t, filepath.Join(base, "mods", "TsarsLibFolder"))
	mustWrite(t, filepath.Join(base, "mods", "TsarsLibFolder", "mod.info"),
		"name=Tsar's Library\nid=Tsarslib\n")
	mustWrite(t, filepath.Join(base, "Server", "riv.ini"), "Mods=TsarsLibFolder\n")

	if _, err := app.cfg.Update(func(cfg *config.Config) error {
		cfg.PZRoot = filepath.Join(dir, "pzroot")
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	seedServer(t, app, `{"name":"Riverside","host":"127.0.0.1","rconPort":27015,
	  "rconPassword":"x","enabled":false,"pzPath":"riverside"}`)
	id := app.cfg.Get().Servers[0].ID

	rec := c.do(http.MethodGet, "/api/mods/manage?id="+id, nil)
	var out struct {
		Mods []ModEntry `json:"mods"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)

	e := out.Mods[0]
	if e.DeclaredID != "Tsarslib" {
		t.Fatalf("the declared ID should be surfaced: %#v", e)
	}
	if e.Fix == nil || e.Fix.Action != "rename" || e.Fix.To != "Tsarslib" {
		t.Fatalf("a rename to the declared ID should be offered: %#v", e.Fix)
	}
	if !strings.Contains(e.Problem, "declares Tsarslib") {
		t.Fatalf("the explanation should name the declared ID: %q", e.Problem)
	}
}

// Build 42 keeps mod.info in a version folder. Reading only the mod's root
// found nothing, so the ID was taken from the folder name — which for a
// Workshop download is the author's title, spaces and all. PZAdmin then
// reported that invented ID as authoritative and flagged the real one as
// broken, which is precisely backwards.
func TestBuild42ModInfoIsFound(t *testing.T) {
	dir := t.TempDir()
	app, err := New(Options{
		SetupCode: testSetupCode,
		DataDir:   dir,
		Assets:    fstest.MapFS{"index.html": &fstest.MapFile{Data: []byte("app")}},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(app.Close)
	c := &client{t: t, handler: app.Handler()}
	c.setup("rick", "a-long-enough-password")

	base := filepath.Join(dir, "pzroot", "riverside")
	ws := filepath.Join(base, "steamapps", "workshop", "content", "108600", "2875848298", "mods")
	mustMkdir(t, filepath.Join(base, "Server"))
	mustMkdir(t, filepath.Join(ws, "Improvised Silencers", "42"))
	mustMkdir(t, filepath.Join(ws, "Improvised Silencers", "common"))
	mustWrite(t, filepath.Join(ws, "Improvised Silencers", "42", "mod.info"),
		"name=Improvised Silencers\nid=ImprovisedSilencers\n")
	mustWrite(t, filepath.Join(base, "Server", "riv.ini"),
		"Mods=ImprovisedSilencers\nWorkshopItems=2875848298\n")

	if _, err := app.cfg.Update(func(cfg *config.Config) error {
		cfg.PZRoot = filepath.Join(dir, "pzroot")
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	seedServer(t, app, `{"name":"Riverside","host":"127.0.0.1","rconPort":27015,
	  "rconPassword":"x","enabled":false,"pzPath":"riverside"}`)
	id := app.cfg.Get().Servers[0].ID

	rec := c.do(http.MethodGet, "/api/mods/manage?id="+id, nil)
	var out struct {
		Mods []ModEntry `json:"mods"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Mods) != 1 {
		t.Fatalf("expected one entry, got %#v", out.Mods)
	}
	e := out.Mods[0]
	if !e.Installed {
		t.Fatalf("the Build 42 mod should resolve: %#v", e)
	}
	if e.DeclaredID != "ImprovisedSilencers" {
		t.Fatalf("the declared ID must come from 42/mod.info: %#v", e)
	}
	if e.Folder != "Improvised Silencers" {
		t.Fatalf("the folder should be reported separately: %#v", e)
	}
	if e.Problem != "" {
		t.Fatalf("the correct ID should not be flagged: %q", e.Problem)
	}
	if e.Fix != nil {
		t.Fatalf("nothing should be offered to 'fix': %#v", e.Fix)
	}
}

// With no mod.info anywhere, PZAdmin knows the folder name and nothing else.
// It must say so rather than presenting the folder as the declared ID.
func TestMissingModInfoIsAdmittedNotGuessed(t *testing.T) {
	dir := t.TempDir()
	app, err := New(Options{
		SetupCode: testSetupCode,
		DataDir:   dir,
		Assets:    fstest.MapFS{"index.html": &fstest.MapFile{Data: []byte("app")}},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(app.Close)
	c := &client{t: t, handler: app.Handler()}
	c.setup("rick", "a-long-enough-password")

	base := filepath.Join(dir, "pzroot", "riverside")
	ws := filepath.Join(base, "steamapps", "workshop", "content", "108600", "111", "mods")
	mustMkdir(t, filepath.Join(base, "Server"))
	mustMkdir(t, filepath.Join(ws, "Some Mod Title", "media"))
	mustWrite(t, filepath.Join(base, "Server", "riv.ini"),
		"Mods=Some Mod Title\nWorkshopItems=111\n")

	if _, err := app.cfg.Update(func(cfg *config.Config) error {
		cfg.PZRoot = filepath.Join(dir, "pzroot")
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	seedServer(t, app, `{"name":"Riverside","host":"127.0.0.1","rconPort":27015,
	  "rconPassword":"x","enabled":false,"pzPath":"riverside"}`)
	id := app.cfg.Get().Servers[0].ID

	rec := c.do(http.MethodGet, "/api/mods/manage?id="+id, nil)
	var out struct {
		Mods []ModEntry `json:"mods"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)

	e := out.Mods[0]
	if e.DeclaredID != "" {
		t.Fatalf("with no mod.info there is no declared ID to report: %#v", e)
	}
	if !strings.Contains(e.Problem, "Workshop page") {
		t.Fatalf("it should point at the authority it does not have: %q", e.Problem)
	}
	if e.Fix != nil {
		t.Fatalf("nothing should be offered as a fix when nothing is known: %#v", e.Fix)
	}
}
