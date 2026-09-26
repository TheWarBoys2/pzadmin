package server

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/TheWarBoys2/pzadmin/internal/config"
	"github.com/TheWarBoys2/pzadmin/internal/fsutil"
	"github.com/TheWarBoys2/pzadmin/internal/pz"
	"github.com/TheWarBoys2/pzadmin/internal/store"
)

// Perks are the skills addxp accepts. Unlike items and vehicles these are not
// declared in the script files, so the list is fixed. Anything a mod adds can
// be added by hand on the Catalogue screen.
var vanillaPerks = []pz.CatalogueEntry{
	{ID: "Fitness", Name: "Fitness", Category: "Passive", Kind: "perk", Source: "vanilla"},
	{ID: "Strength", Name: "Strength", Category: "Passive", Kind: "perk", Source: "vanilla"},
	{ID: "Sprinting", Name: "Sprinting", Category: "Agility", Kind: "perk", Source: "vanilla"},
	{ID: "Lightfoot", Name: "Lightfooted", Category: "Agility", Kind: "perk", Source: "vanilla"},
	{ID: "Nimble", Name: "Nimble", Category: "Agility", Kind: "perk", Source: "vanilla"},
	{ID: "Sneak", Name: "Sneaking", Category: "Agility", Kind: "perk", Source: "vanilla"},
	{ID: "Axe", Name: "Axe", Category: "Combat", Kind: "perk", Source: "vanilla"},
	{ID: "Blunt", Name: "Long Blunt", Category: "Combat", Kind: "perk", Source: "vanilla"},
	{ID: "SmallBlunt", Name: "Short Blunt", Category: "Combat", Kind: "perk", Source: "vanilla"},
	{ID: "LongBlade", Name: "Long Blade", Category: "Combat", Kind: "perk", Source: "vanilla"},
	{ID: "SmallBlade", Name: "Short Blade", Category: "Combat", Kind: "perk", Source: "vanilla"},
	{ID: "Spear", Name: "Spear", Category: "Combat", Kind: "perk", Source: "vanilla"},
	{ID: "Maintenance", Name: "Maintenance", Category: "Combat", Kind: "perk", Source: "vanilla"},
	{ID: "Aiming", Name: "Aiming", Category: "Firearm", Kind: "perk", Source: "vanilla"},
	{ID: "Reloading", Name: "Reloading", Category: "Firearm", Kind: "perk", Source: "vanilla"},
	{ID: "Woodwork", Name: "Carpentry", Category: "Crafting", Kind: "perk", Source: "vanilla"},
	{ID: "Cooking", Name: "Cooking", Category: "Crafting", Kind: "perk", Source: "vanilla"},
	{ID: "Farming", Name: "Farming", Category: "Crafting", Kind: "perk", Source: "vanilla"},
	{ID: "Doctor", Name: "First Aid", Category: "Crafting", Kind: "perk", Source: "vanilla"},
	{ID: "Electricity", Name: "Electrical", Category: "Crafting", Kind: "perk", Source: "vanilla"},
	{ID: "MetalWelding", Name: "Metalworking", Category: "Crafting", Kind: "perk", Source: "vanilla"},
	{ID: "Mechanics", Name: "Mechanics", Category: "Crafting", Kind: "perk", Source: "vanilla"},
	{ID: "Tailoring", Name: "Tailoring", Category: "Crafting", Kind: "perk", Source: "vanilla"},
	{ID: "Fishing", Name: "Fishing", Category: "Survival", Kind: "perk", Source: "vanilla"},
	{ID: "Trapping", Name: "Trapping", Category: "Survival", Kind: "perk", Source: "vanilla"},
	{ID: "PlantScavenging", Name: "Foraging", Category: "Survival", Kind: "perk", Source: "vanilla"},
}

// customCatalogue holds entries the operator added by hand, for modded content
// PZAdmin cannot see on disk.
type customCatalogue struct {
	mu      sync.RWMutex
	path    string
	Entries []pz.CatalogueEntry `json:"entries"`
	// loadErr is set when catalogue.json could not be read at start.
	loadErr error
}

func loadCustomCatalogue(path string) *customCatalogue {
	c := &customCatalogue{path: path}
	if _, err := fsutil.ReadJSON(path, c); err != nil {
		log.Printf("catalogue: %v", err)
		c.Entries = nil
		c.loadErr = err
	}
	return c
}

func (c *customCatalogue) list() []pz.CatalogueEntry {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return append([]pz.CatalogueEntry(nil), c.Entries...)
}

func (c *customCatalogue) add(e pz.CatalogueEntry) error {
	e.ID = strings.TrimSpace(e.ID)
	e.Name = strings.TrimSpace(e.Name)
	e.Category = strings.TrimSpace(e.Category)
	if e.ID == "" {
		return fmt.Errorf("enter the ID exactly as the game expects it")
	}
	if strings.ContainsAny(e.ID, " \t\r\n\"'") {
		return fmt.Errorf("an ID cannot contain spaces or quotes")
	}
	switch e.Kind {
	case "item", "vehicle", "perk":
	default:
		return fmt.Errorf("choose whether this is an item, a vehicle or a skill")
	}
	if e.Kind != "perk" && !strings.Contains(e.ID, ".") {
		return fmt.Errorf("item and vehicle IDs look like Module.Name, for example Base.Axe")
	}
	if e.Category == "" {
		e.Category = "Custom"
	}
	e.Source = "custom"

	c.mu.Lock()
	defer c.mu.Unlock()
	for i, existing := range c.Entries {
		if existing.ID == e.ID && existing.Kind == e.Kind {
			c.Entries[i] = e
			return c.saveLocked()
		}
	}
	if len(c.Entries) >= 5000 {
		return fmt.Errorf("the custom list is full")
	}
	c.Entries = append(c.Entries, e)
	return c.saveLocked()
}

func (c *customCatalogue) remove(id, kind string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := c.Entries[:0]
	found := false
	for _, e := range c.Entries {
		if e.ID == id && e.Kind == kind {
			found = true
			continue
		}
		out = append(out, e)
	}
	if !found {
		return fmt.Errorf("no custom entry with that ID")
	}
	c.Entries = append([]pz.CatalogueEntry{}, out...)
	return c.saveLocked()
}

func (c *customCatalogue) saveLocked() error {
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return fsutil.WriteFile(c.path, b, 0o600)
}

// catalogueFor builds the merged catalogue for one server.
func (a *App) catalogueFor(serverID string, refresh bool) (map[string]any, error) {
	srv, found := a.cfg.Server(serverID)
	if !found {
		return nil, fmt.Errorf("no such server")
	}
	layout := detectLayout(srv)
	gameRoot := a.gameRootFor(srv)
	if refresh {
		a.scripts.Invalidate()
	}
	scanned := a.scripts.Scan(gameRoot, layout.ModDirs)

	// Start from empty slices, not nil: a nil slice marshals as null and every
	// caller then has to guard against it.
	items := append([]pz.CatalogueEntry{}, scanned.Items...)
	vehicles := append([]pz.CatalogueEntry{}, scanned.Vehicles...)
	perks := append([]pz.CatalogueEntry{}, vanillaPerks...)

	for _, e := range a.custom.list() {
		switch e.Kind {
		case "vehicle":
			vehicles = append(vehicles, e)
		case "perk":
			perks = append(perks, e)
		default:
			items = append(items, e)
		}
	}
	sortCatalogue(items)
	sortCatalogue(vehicles)
	sortCatalogue(perks)

	// Explain what the picker is working from, so an empty list is never a
	// mystery.
	note := ""
	switch {
	case gameRoot == "" && len(layout.ModDirs) == 0:
		note = "PZAdmin cannot find this server's Project Zomboid installation, so this list only " +
			"contains entries you added by hand. It looks for a media/scripts folder inside the " +
			"server's own directory, which is where a per-server install normally sits. If yours " +
			"is somewhere else, set Game files in this server's Edit server dialog."
	case gameRoot == "":
		note = "Only mod items are listed: PZAdmin found this server's mods but not its media/scripts " +
			"folder, so the vanilla items are missing. Set the game files path on the server if the " +
			"installation is outside the server's own directory. The scan report below lists every " +
			"directory that was looked at."
	case len(scanned.Items) == 0:
		note = "PZAdmin read the game files but found no item definitions in them. Open the scan " +
			"report below to see exactly which directories were looked at."
	}

	return map[string]any{
		"items":      items,
		"vehicles":   vehicles,
		"perks":      perks,
		"sources":    append([]string{}, scanned.Sources...),
		"gameRoot":   gameRoot,
		"perServer":  gameRoot != "" && gameRoot == pz.Detect(srv.PZPath).GameDir,
		"sharedRoot": a.gameRoot != "" && gameRoot != "" && !strings.HasPrefix(gameRoot, filepath.Clean(srv.PZPath)+string(filepath.Separator)) && gameRoot != filepath.Clean(srv.PZPath),
		"scanned":    scanned.Scanned,
		"scannedAt":  scanned.ScannedAt,
		"truncated":  scanned.Truncated,
		"note":       note,
		"customOnly": gameRoot == "" && len(scanned.Items) == 0,
	}, nil
}

func sortCatalogue(list []pz.CatalogueEntry) {
	sort.SliceStable(list, func(i, j int) bool {
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

// gameRootFor resolves where a server's Project Zomboid installation lives.
//
// A server's own installation always wins. Servers are installed one per
// directory precisely so each can carry its own build and mod set, so a shared
// path would hand one server's item list to all of them. PZADMIN_GAME_ROOT is
// consulted last, as a floor rather than an override: it is better to show the
// item list of some Project Zomboid build than to show none at all.
func (a *App) gameRootFor(srv config.Server) string {
	if srv.GameRoot != "" {
		if found := pz.DetectGameRoot(srv.GameRoot); found != "" {
			return found
		}
	}
	if found := pz.Detect(srv.PZPath).GameDir; found != "" {
		return found
	}
	// Alongside the server's own directory, for a shared install one level up.
	if found := pz.DetectGameRoot(filepath.Dir(srv.PZPath)); found != "" {
		return found
	}
	// Finally the shared mount, if one was configured.
	if a.gameRoot != "" {
		return pz.DetectGameRoot(a.gameRoot)
	}
	return ""
}

func (a *App) handleCatalogue(w http.ResponseWriter, r *http.Request) {
	out, err := a.catalogueFor(r.URL.Query().Get("id"), r.URL.Query().Get("refresh") == "1")
	if err != nil {
		httpError(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, out)
}

func (a *App) handleCatalogueAdd(w http.ResponseWriter, r *http.Request) {
	var p struct {
		ID       string `json:"id"`
		Name     string `json:"name"`
		Category string `json:"category"`
		Kind     string `json:"kind"`
	}
	if !decodeJSON(w, r, &p) {
		return
	}
	entry := pz.CatalogueEntry{ID: p.ID, Name: p.Name, Category: p.Category, Kind: p.Kind}
	if err := a.custom.add(entry); err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}
	a.event(store.Event{Kind: "admin.action", Severity: store.SevInfo, Source: source(r), Actor: actor(r),
		Message: "Added " + p.Kind + " " + p.ID + " to the catalogue"})
	ok(w, map[string]any{"entries": a.custom.list()})
}

func (a *App) handleCatalogueRemove(w http.ResponseWriter, r *http.Request) {
	var p struct {
		ID   string `json:"id"`
		Kind string `json:"kind"`
	}
	if !decodeJSON(w, r, &p) {
		return
	}
	if err := a.custom.remove(p.ID, p.Kind); err != nil {
		httpError(w, http.StatusNotFound, err.Error())
		return
	}
	ok(w, map[string]any{"entries": a.custom.list()})
}
