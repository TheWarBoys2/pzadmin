package server

import (
	"context"
	"fmt"
	"net/http"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/TheWarBoys2/pzadmin/internal/pz"
	"github.com/TheWarBoys2/pzadmin/internal/steam"
	"github.com/TheWarBoys2/pzadmin/internal/store"
)

// Project Zomboid needs two lines kept in step:
//
//	Mods=Tsarslib;TrueActionsDancing        the mod IDs from each mod.info
//	WorkshopItems=2447729538;2169435993     the numeric Workshop IDs
//
// A mod ID in Mods= whose Workshop ID is absent from WorkshopItems= never gets
// downloaded, so the server either refuses to start or quietly runs without it.
// That mismatch is the single most common broken-mods symptom, and the pairing
// is exactly what this screen exists to keep honest.

// ModEntry is one mod as the manager sees it.
type ModEntry struct {
	ID         string `json:"id"`
	Name       string `json:"name,omitempty"`
	WorkshopID string `json:"workshopId,omitempty"`
	Category   string `json:"category,omitempty"`
	Version    string `json:"version,omitempty"`
	// Enabled means the mod appears in the Mods= line.
	Enabled bool `json:"enabled"`
	// Installed means a folder for it was found on disk.
	Installed bool `json:"installed"`
	// WorkshopListed means its Workshop ID is in WorkshopItems=.
	WorkshopListed bool `json:"workshopListed"`
	// Problem describes what is wrong with this entry, in plain words.
	Problem  string   `json:"problem,omitempty"`
	Require  []string `json:"require,omitempty"`
	Position int      `json:"position"`
	// Folder is where the mod lives on disk, which is often not its ID.
	Folder string `json:"folder,omitempty"`
	// Siblings are the other mods that arrived in the same Workshop item. One
	// download frequently contains several mods, so one Workshop ID covers all
	// of them and removing it would take the others with it.
	Siblings []string `json:"siblings,omitempty"`
	// DeclaredID is the id= from the mod's own mod.info. That is the only
	// authority on what belongs in the Mods= line, and it is not always what
	// the folder is called or what the operator typed.
	DeclaredID string `json:"declaredId,omitempty"`
	// Fix is a change PZAdmin can make for you, when it is confident.
	Fix *ModFix `json:"fix,omitempty"`
}

// ModFix is a one-click correction offered for a broken entry.
type ModFix struct {
	// Action is "remove" or "rename".
	Action string `json:"action"`
	// To is the replacement ID, for a rename.
	To string `json:"to,omitempty"`
	// Label is the button text.
	Label string `json:"label"`
}

// Bundle is one Workshop item and everything it provides.
type Bundle struct {
	WorkshopID string   `json:"workshopId"`
	Mods       []string `json:"mods"`
	Enabled    []string `json:"enabled"`
	Listed     bool     `json:"listed"`
	Installed  bool     `json:"installed"`
}

// modConfigFile finds the .ini holding the mod lines for a server.
func (a *App) modConfigFile(layout pz.Layout) (string, string, error) {
	if layout.ConfigDir == "" {
		return "", "", fmt.Errorf("no config directory was detected for this server")
	}
	files, err := pz.ListConfigFiles(layout.ConfigDir)
	if err != nil {
		return "", "", err
	}
	// Prefer an ini that already carries a Mods line.
	var fallback string
	for _, f := range files {
		if f.Kind != "ini" {
			continue
		}
		if fallback == "" {
			fallback = f.Name
		}
		ini, err := pz.LoadINI(filepath.Join(layout.ConfigDir, f.Name))
		if err != nil {
			continue
		}
		if _, ok := ini.Get("Mods"); ok {
			return filepath.Join(layout.ConfigDir, f.Name), f.Name, nil
		}
	}
	if fallback == "" {
		return "", "", fmt.Errorf("no server .ini was found for this server")
	}
	return filepath.Join(layout.ConfigDir, fallback), fallback, nil
}

func splitList(v string) []string {
	parts := strings.Split(v, ";")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// handleModManager returns everything the mod screen needs.
func (a *App) handleModManager(w http.ResponseWriter, r *http.Request) {
	srv, found := a.cfg.Server(r.URL.Query().Get("id"))
	if !found {
		httpError(w, http.StatusNotFound, "no such server")
		return
	}
	layout := detectLayout(srv)
	path, name, err := a.modConfigFile(layout)
	if err != nil {
		httpError(w, http.StatusNotFound, err.Error())
		return
	}
	ini, err := pz.LoadINI(path)
	if err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}

	modsLine, _ := ini.Get("Mods")
	workshopLine, _ := ini.Get("WorkshopItems")
	mapLine, _ := ini.Get("Map")
	order := splitList(modsLine)
	workshop := splitList(workshopLine)

	if r.URL.Query().Get("refresh") == "1" {
		a.scanner.Invalidate(layout.Base)
	}
	report := a.scanner.Scan(layout)
	onDisk := map[string]pz.Mod{}
	for _, m := range report.Mods {
		if m.Installed {
			onDisk[strings.ToLower(m.ID)] = m
		}
	}

	listed := map[string]bool{}
	for _, id := range workshop {
		listed[id] = true
	}

	// Group everything on disk by the Workshop item it came from, because one
	// download often provides several mods. Without that grouping, removing a
	// Workshop ID to tidy up would quietly break the other mods sharing it.
	bundles := map[string][]pz.Mod{}
	for _, m := range report.Mods {
		if m.Installed && m.WorkshopID != "" {
			bundles[m.WorkshopID] = append(bundles[m.WorkshopID], m)
		}
	}

	entries := make([]ModEntry, 0, len(order))
	usedDisk := map[string]bool{}
	for i, id := range order {
		disk, installed := onDisk[strings.ToLower(id)]
		usedDisk[strings.ToLower(id)] = true
		if installed {
			usedDisk[strings.ToLower(disk.ID)] = true
			usedDisk[strings.ToLower(disk.Folder)] = true
		}
		entry := ModEntry{
			ID: id, Enabled: true, Position: i,
			Installed: installed, Name: disk.Name, Category: disk.Category,
			Version: disk.Version, WorkshopID: disk.WorkshopID, Require: disk.Require,
			Folder: disk.Folder,
		}
		for _, sibling := range bundles[entry.WorkshopID] {
			if !strings.EqualFold(sibling.ID, disk.ID) {
				entry.Siblings = append(entry.Siblings, sibling.ID)
			}
		}
		entry.WorkshopListed = entry.WorkshopID != "" && listed[entry.WorkshopID]
		switch {
		case !installed && entry.WorkshopID == "":
			entry.Problem = "Not on disk, and PZAdmin does not know its Workshop ID. " +
				"If this is part of a mod you already have, check the spelling against the " +
				"IDs listed below. Otherwise paste its Workshop link, or remove it."
		case !installed:
			entry.Problem = "Not on disk yet. The server downloads Workshop items on startup."
		case !entry.WorkshopListed && entry.WorkshopID != "":
			entry.Problem = "Its Workshop ID is missing from WorkshopItems, so a fresh install " +
				"would not download it."
		}
		entries = append(entries, entry)
	}

	// Two entries that name the same mod are the commonest mess in a
	// hand-edited Mods= line: one spelling resolves, the other does not, and
	// nothing on screen explains that they are the same thing.
	byNormal := map[string][]int{}
	for i, e := range entries {
		byNormal[normaliseModID(e.ID)] = append(byNormal[normaliseModID(e.ID)], i)
	}

	for i := range entries {
		e := &entries[i]
		if e.Installed {
			// Record what mod.info actually declares, and offer to correct the
			// entry when the operator wrote something else that still resolved.
			if disk, ok := onDisk[strings.ToLower(e.ID)]; ok {
				// Only mod.info can say what a mod's ID is. When there is no
				// mod.info, PZAdmin knows the folder name and nothing more,
				// and must not present that as the declared ID: Workshop
				// folders are usually named after the mod's title, spaces and
				// all, which is exactly what an ID is not.
				e.DeclaredID = disk.Declared
				switch {
				case disk.Declared == "":
					e.Problem = "PZAdmin found this on disk but could not read a mod.info for it, " +
						"so it cannot confirm the ID. The Mod ID printed at the bottom of the " +
						"mod's Workshop page is the one to trust."
				case disk.Declared != e.ID:
					e.Problem = "This is written as " + e.ID + ", but the mod's mod.info declares " +
						disk.Declared + ". Project Zomboid matches on the declared ID."
					e.Fix = &ModFix{Action: "rename", To: disk.Declared, Label: "Use " + disk.Declared}
				}
			}
			continue
		}

		// Not on disk. If another entry names the same mod and does resolve,
		// this one is a leftover duplicate.
		duplicateOf := ""
		for _, j := range byNormal[normaliseModID(e.ID)] {
			if j != i && entries[j].Installed {
				duplicateOf = entries[j].ID
				break
			}
		}
		if duplicateOf != "" {
			e.Problem = "This is the same mod as " + duplicateOf + ", spelled differently. " +
				duplicateOf + " is the one that matches what is on disk, so this entry does nothing."
			e.Fix = &ModFix{Action: "remove", Label: "Remove this duplicate"}
			continue
		}
		if e.WorkshopID != "" {
			continue
		}
		if match := closestModID(e.ID, report.Mods); match != "" {
			e.Problem += " The closest installed mod ID is " + match + ", which is what the mod " +
				"declares in its mod.info."
			e.Fix = &ModFix{Action: "rename", To: match, Label: "Use " + match}
		}
	}

	// Mods sitting on disk that nothing enables: usually a leftover, sometimes
	// a dependency the operator forgot to add.
	var available []ModEntry
	seenAvailable := map[string]bool{}
	for _, m := range report.Mods {
		if !m.Installed {
			continue
		}
		if usedDisk[strings.ToLower(m.ID)] || usedDisk[strings.ToLower(m.Folder)] {
			continue
		}
		if seenAvailable[strings.ToLower(m.ID)] {
			continue
		}
		seenAvailable[strings.ToLower(m.ID)] = true
		entry := ModEntry{
			ID: m.ID, Name: m.Name, WorkshopID: m.WorkshopID, Category: m.Category,
			Version: m.Version, Installed: true, Enabled: false, Folder: m.Folder,
			DeclaredID:     m.ID,
			WorkshopListed: m.WorkshopID != "" && listed[m.WorkshopID],
			Require:        m.Require,
		}
		for _, sibling := range bundles[m.WorkshopID] {
			if !strings.EqualFold(sibling.ID, m.ID) {
				entry.Siblings = append(entry.Siblings, sibling.ID)
			}
		}
		available = append(available, entry)
	}
	sort.Slice(available, func(i, j int) bool {
		return strings.ToLower(available[i].ID) < strings.ToLower(available[j].ID)
	})

	// Workshop IDs listed but not matched to any enabled mod.
	var orphanWorkshop []string
	claimed := map[string]bool{}
	for _, e := range entries {
		if e.WorkshopID != "" {
			claimed[e.WorkshopID] = true
		}
	}
	for _, id := range workshop {
		if !claimed[id] {
			orphanWorkshop = append(orphanWorkshop, id)
		}
	}

	bundleList := make([]Bundle, 0, len(bundles))
	for wsID, provided := range bundles {
		b := Bundle{WorkshopID: wsID, Listed: listed[wsID], Installed: true}
		for _, m := range provided {
			b.Mods = append(b.Mods, m.ID)
			if m.Enabled {
				b.Enabled = append(b.Enabled, m.ID)
			}
		}
		sort.Strings(b.Mods)
		sort.Strings(b.Enabled)
		bundleList = append(bundleList, b)
	}
	sort.Slice(bundleList, func(i, j int) bool {
		// Show the multi-mod items first: those are the ones worth knowing about.
		if len(bundleList[i].Mods) != len(bundleList[j].Mods) {
			return len(bundleList[i].Mods) > len(bundleList[j].Mods)
		}
		return bundleList[i].WorkshopID < bundleList[j].WorkshopID
	})

	info := map[string]pz.Mod{}
	for _, m := range report.Mods {
		info[m.ID] = m
	}
	rules := pz.LoadSortingRules(layout.SortingRulesPath())
	preview := pz.SortLoadOrder(order, info, rules)

	writeJSON(w, map[string]any{
		"file":           name,
		"mods":           entries,
		"available":      available,
		"workshopItems":  workshop,
		"orphanWorkshop": orphanWorkshop,
		"bundles":        bundleList,
		"map":            mapLine,
		"sortPreview":    preview,
		"hasSortRules":   layout.SortingRulesPath() != "",
		"online":         a.statusOf(srv.ID).Online,
	})
}

// handleModSort returns the order the dependency rules imply, without writing
// anything: the operator sees what would move before agreeing to it.
func (a *App) handleModSort(w http.ResponseWriter, r *http.Request) {
	var p struct {
		ServerID string   `json:"serverId"`
		Mods     []string `json:"mods"`
	}
	if !decodeJSON(w, r, &p) {
		return
	}
	srv, found := a.cfg.Server(p.ServerID)
	if !found {
		httpError(w, http.StatusNotFound, "no such server")
		return
	}
	layout := detectLayout(srv)
	report := a.scanner.Scan(layout)
	info := map[string]pz.Mod{}
	for _, m := range report.Mods {
		info[m.ID] = m
	}
	result := pz.SortLoadOrder(p.Mods, info, pz.LoadSortingRules(layout.SortingRulesPath()))
	writeJSON(w, map[string]any{"result": result})
}

// handleModResolve turns pasted Workshop links into something addable.
func (a *App) handleModResolve(w http.ResponseWriter, r *http.Request) {
	var p struct {
		ServerID string `json:"serverId"`
		Text     string `json:"text"`
	}
	if !decodeJSON(w, r, &p) {
		return
	}
	ids := steam.ParseIDs(p.Text)
	if len(ids) == 0 {
		httpError(w, http.StatusBadRequest,
			"No Workshop ID found in that. Paste a Workshop link, or the numeric ID.")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 25*time.Second)
	defer cancel()
	client := steam.New(a.steamBase)

	// A single ID might be a collection rather than an item; expanding it is
	// what makes pasting a modpack link work.
	if len(ids) == 1 {
		if children, err := client.Collection(ctx, ids[0]); err == nil && len(children) > 0 {
			ids = children
		}
	}

	items, err := client.Details(ctx, ids)
	if err != nil {
		httpError(w, http.StatusBadGateway, err.Error())
		return
	}

	// Mark anything already configured so the dialog does not offer duplicates.
	existingMods := map[string]bool{}
	existingWorkshop := map[string]bool{}
	if srv, found := a.cfg.Server(p.ServerID); found {
		if path, _, err := a.modConfigFile(detectLayout(srv)); err == nil {
			if ini, err := pz.LoadINI(path); err == nil {
				v, _ := ini.Get("Mods")
				for _, id := range splitList(v) {
					existingMods[strings.ToLower(id)] = true
				}
				v, _ = ini.Get("WorkshopItems")
				for _, id := range splitList(v) {
					existingWorkshop[id] = true
				}
			}
		}
	}

	out := make([]map[string]any, 0, len(items))
	for _, item := range items {
		known := make([]bool, len(item.ModIDs))
		for i, id := range item.ModIDs {
			known[i] = existingMods[strings.ToLower(id)]
		}
		note := ""
		switch {
		case item.Missing:
			note = "Steam has no such item. It may have been removed from the Workshop."
		case item.AppID != 0 && item.AppID != steam.ProjectZomboidAppID:
			note = "This is not a Project Zomboid item."
		case len(item.ModIDs) == 0:
			note = "The description does not list a Mod ID. PZAdmin will add the Workshop ID " +
				"so the server downloads it, then pick the mod ID up off disk afterwards."
		}
		out = append(out, map[string]any{
			"workshopId":    item.WorkshopID,
			"title":         item.Title,
			"modIds":        item.ModIDs,
			"mapFolders":    item.MapFolders,
			"updatedAt":     item.UpdatedAt,
			"sizeBytes":     item.SizeBytes,
			"missing":       item.Missing,
			"appId":         item.AppID,
			"alreadyListed": existingWorkshop[item.WorkshopID],
			"modIdsKnown":   known,
			"note":          note,
		})
	}
	writeJSON(w, map[string]any{"items": out})
}

// handleModApply writes the Mods and WorkshopItems lines.
//
// This is the most destructive thing in PZAdmin short of restoring a backup: a
// bad mod line stops the server booting. So the whole list is validated first,
// the previous ini is copied aside, and the operator is told plainly that a
// restart is required, because these two settings are read only at startup.
func (a *App) handleModApply(w http.ResponseWriter, r *http.Request) {
	var p struct {
		ServerID      string   `json:"serverId"`
		Mods          []string `json:"mods"`
		WorkshopItems []string `json:"workshopItems"`
		Map           *string  `json:"map"`
		// Confirm acknowledges the blocking findings from the pre-flight. The
		// interface sends it only after showing them.
		Confirm bool `json:"confirm"`
	}
	if !decodeJSON(w, r, &p) {
		return
	}
	srv, found := a.cfg.Server(p.ServerID)
	if !found {
		httpError(w, http.StatusNotFound, "no such server")
		return
	}
	if len(p.Mods) > 500 || len(p.WorkshopItems) > 500 {
		httpError(w, http.StatusBadRequest, "that is more mods than PZAdmin will write in one go")
		return
	}

	mods, err := cleanModIDs(p.Mods)
	if err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}
	workshop, err := cleanWorkshopIDs(p.WorkshopItems)
	if err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}

	layout := detectLayout(srv)
	path, name, err := a.modConfigFile(layout)
	if err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}
	ini, err := pz.LoadINI(path)
	if err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}

	before, _ := ini.Get("Mods")
	beforeWorkshop, _ := ini.Get("WorkshopItems")

	// Check the change against what is on disk before writing anything. A mod
	// list is accepted by the ini whatever state it leaves the server in, so
	// this is the only point at which the damage is still preventable.
	issues := pz.Preflight(a.scanner.Scan(layout), pz.ModPlan{
		BeforeMods:     splitList(before),
		BeforeWorkshop: splitList(beforeWorkshop),
		AfterMods:      mods,
		AfterWorkshop:  workshop,
	})
	if pz.Blocking(issues) && !p.Confirm {
		writeStatusJSON(w, http.StatusConflict, map[string]any{
			"ok":           false,
			"needsConfirm": true,
			"issues":       issues,
			"message":      "This change breaks something. Review the findings, then apply again to go ahead.",
		})
		return
	}

	if err := ini.Set("Mods", strings.Join(mods, ";")); err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := ini.Set("WorkshopItems", strings.Join(workshop, ";")); err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}
	if p.Map != nil {
		if err := ini.Set("Map", strings.TrimSpace(*p.Map)); err != nil {
			httpError(w, http.StatusBadRequest, err.Error())
			return
		}
	}

	backup, err := ini.Save(filepath.Join(a.backup.Dir(srv.ID), "config"))
	if err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	a.scanner.Invalidate(layout.Base)

	added, removed := diffLists(splitList(before), mods)
	detail := fmt.Sprintf("%d mods, %d Workshop items.", len(mods), len(workshop))
	if len(added) > 0 {
		detail += " Added: " + strings.Join(added, ", ") + "."
	}
	if len(removed) > 0 {
		detail += " Removed: " + strings.Join(removed, ", ") + "."
	}

	a.event(store.Event{
		Kind: "config.edit", Severity: store.SevWarn, Source: source(r), Actor: actor(r),
		ServerID: srv.ID, Server: srv.Name,
		Message: "Mod list changed on " + srv.Name, Detail: detail,
	})

	writeJSON(w, map[string]any{
		"ok": true, "file": name, "backup": backup,
		"mods": len(mods), "workshopItems": len(workshop),
		"added": added, "removed": removed,
		"issues": issues,
		"message": "Saved. Mods and Workshop items are only read when the server starts, " +
			"so restart it to apply this.",
	})
}

// cleanModIDs validates and de-duplicates a mod list.
func cleanModIDs(in []string) ([]string, error) {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, raw := range in {
		id := strings.TrimSpace(raw)
		if id == "" {
			continue
		}
		// A semicolon would split one entry into two, and the rest would
		// corrupt the line or the file.
		if strings.ContainsAny(id, ";\r\n=") {
			return nil, fmt.Errorf("%q is not a valid mod ID", raw)
		}
		if len(id) > 128 {
			return nil, fmt.Errorf("that mod ID is implausibly long")
		}
		if seen[strings.ToLower(id)] {
			continue
		}
		seen[strings.ToLower(id)] = true
		out = append(out, id)
	}
	return out, nil
}

func cleanWorkshopIDs(in []string) ([]string, error) {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, raw := range in {
		id := strings.TrimSpace(raw)
		if id == "" {
			continue
		}
		for _, r := range id {
			if r < '0' || r > '9' {
				return nil, fmt.Errorf("%q is not a Workshop ID: they are numbers only", raw)
			}
		}
		if seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out, nil
}

func diffLists(before, after []string) (added, removed []string) {
	had := map[string]bool{}
	for _, v := range before {
		had[strings.ToLower(v)] = true
	}
	has := map[string]bool{}
	for _, v := range after {
		has[strings.ToLower(v)] = true
		if !had[strings.ToLower(v)] {
			added = append(added, v)
		}
	}
	for _, v := range before {
		if !has[strings.ToLower(v)] {
			removed = append(removed, v)
		}
	}
	return added, removed
}

// closestModID finds an installed mod whose ID differs from want only in case,
// punctuation or spacing. A mod ID typed by hand into the ini is the likeliest
// explanation for one that cannot be found, and naming the near miss saves the
// operator hunting through folders.
// normaliseModID reduces an ID to its letters and digits, so that spacing,
// case and punctuation differences between two spellings of the same mod
// collapse to one key.
func normaliseModID(v string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(v) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func closestModID(want string, installed []pz.Mod) string {
	normalise := normaliseModID
	target := normalise(want)
	if target == "" {
		return ""
	}
	for _, m := range installed {
		if !m.Installed {
			continue
		}
		if normalise(m.ID) == target || normalise(m.Folder) == target {
			// Suggest the declared ID where there is one. Suggesting a folder
			// name would be handing back a guess dressed up as an answer.
			if m.Declared != "" {
				return m.Declared
			}
			return ""
		}
	}
	return ""
}

// handleModPreflight reports what a proposed mod list would break, without
// writing anything.
//
// The same check runs inside Apply, which is what actually protects the
// server. This exists so the interface can show the findings while the
// operator is still editing, rather than only after they commit.
func (a *App) handleModPreflight(w http.ResponseWriter, r *http.Request) {
	var p struct {
		ServerID      string   `json:"serverId"`
		Mods          []string `json:"mods"`
		WorkshopItems []string `json:"workshopItems"`
	}
	if !decodeJSON(w, r, &p) {
		return
	}
	srv, found := a.cfg.Server(p.ServerID)
	if !found {
		httpError(w, http.StatusNotFound, "no such server")
		return
	}
	if len(p.Mods) > 500 || len(p.WorkshopItems) > 500 {
		httpError(w, http.StatusBadRequest, "that is more mods than PZAdmin will check in one go")
		return
	}

	mods, err := cleanModIDs(p.Mods)
	if err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}
	workshop, err := cleanWorkshopIDs(p.WorkshopItems)
	if err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}

	layout := detectLayout(srv)
	var before, beforeWorkshop string
	if path, _, err := a.modConfigFile(layout); err == nil {
		if ini, err := pz.LoadINI(path); err == nil {
			before, _ = ini.Get("Mods")
			beforeWorkshop, _ = ini.Get("WorkshopItems")
		}
	}

	issues := pz.Preflight(a.scanner.Scan(layout), pz.ModPlan{
		BeforeMods:     splitList(before),
		BeforeWorkshop: splitList(beforeWorkshop),
		AfterMods:      mods,
		AfterWorkshop:  workshop,
	})
	writeJSON(w, map[string]any{
		"issues":   issues,
		"blocking": pz.Blocking(issues),
	})
}
