package server

import (
	"fmt"
	"net/http"
	"path/filepath"
	"strings"

	"github.com/TheWarBoys2/pzadmin/internal/pz"
	"github.com/TheWarBoys2/pzadmin/internal/store"
)

// handleConfigFields returns a server's settings as typed, grouped fields so
// the interface can present a form instead of a text file.
func (a *App) handleConfigFields(w http.ResponseWriter, r *http.Request) {
	srv, found := a.cfg.Server(r.URL.Query().Get("id"))
	if !found {
		httpError(w, http.StatusNotFound, "no such server")
		return
	}
	layout := detectLayout(srv)
	if layout.ConfigDir == "" {
		httpError(w, http.StatusNotFound, "no config directory was detected for this server")
		return
	}
	name := r.URL.Query().Get("file")
	if name == "" {
		httpError(w, http.StatusBadRequest, "which file?")
		return
	}

	fields, kind, err := readFields(layout.ConfigDir, name)
	if err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}
	// The form never carries a secret value. An empty box means "leave it",
	// which is also how the rest of PZAdmin handles passwords.
	locked := a.lockedKeys(srv, name)
	for i := range fields {
		if fields[i].Secret {
			fields[i].Value = ""
		}
		if reason := locked[fields[i].Key]; reason != "" {
			fields[i].Locked = reason
		}
	}

	// Group the fields for display, preserving the order readFields produced.
	type group struct {
		Name   string     `json:"name"`
		Fields []pz.Field `json:"fields"`
	}
	var groups []group
	index := map[string]int{}
	for _, f := range fields {
		i, seen := index[f.Group]
		if !seen {
			index[f.Group] = len(groups)
			groups = append(groups, group{Name: f.Group})
			i = len(groups) - 1
		}
		groups[i].Fields = append(groups[i].Fields, f)
	}

	writeJSON(w, map[string]any{
		"file":   name,
		"kind":   kind,
		"groups": groups,
		"count":  len(fields),
		// Whether the running server can be told to pick changes up without a
		// full restart. Only server options can; sandbox values are read when
		// the world loads.
		"reloadable": kind == "ini",
		"online":     a.statusOf(srv.ID).Online,
	})
}

func readFields(dir, name string) ([]pz.Field, string, error) {
	lower := strings.ToLower(name)
	switch {
	case strings.HasSuffix(lower, "_sandboxvars.lua"):
		path, err := safeConfigPath(dir, name)
		if err != nil {
			return nil, "", err
		}
		sb, err := pz.LoadSandbox(path)
		if err != nil {
			return nil, "", err
		}
		return sb.Fields(), "sandbox", nil
	case strings.HasSuffix(lower, ".ini"):
		path, err := safeConfigPath(dir, name)
		if err != nil {
			return nil, "", err
		}
		ini, err := pz.LoadINI(path)
		if err != nil {
			return nil, "", err
		}
		return ini.Fields(), "ini", nil
	}
	return nil, "", fmt.Errorf("%s cannot be edited as a form; use the file editor", name)
}

// safeConfigPath refuses anything that is not a bare filename in the config
// directory. The heavy lifting is in pz.ReadConfigFile's guard; this repeats
// the check because this path writes as well as reads.
func safeConfigPath(dir, name string) (string, error) {
	if name == "" || strings.ContainsAny(name, `/\`) || name == "." || name == ".." {
		return "", fmt.Errorf("invalid file name %q", name)
	}
	lower := strings.ToLower(name)
	if !strings.HasSuffix(lower, ".ini") && !strings.HasSuffix(lower, ".lua") {
		return "", fmt.Errorf("only .ini and .lua files can be edited")
	}
	return filepath.Join(dir, name), nil
}

// handleConfigApply writes individual settings rather than a whole file, so a
// form edit cannot clobber anything the operator did not touch.
func (a *App) handleConfigApply(w http.ResponseWriter, r *http.Request) {
	var p struct {
		ServerID string            `json:"serverId"`
		File     string            `json:"file"`
		Changes  map[string]string `json:"changes"`
		Reload   bool              `json:"reload"`
	}
	if !decodeJSON(w, r, &p) {
		return
	}
	srv, found := a.cfg.Server(p.ServerID)
	if !found {
		httpError(w, http.StatusNotFound, "no such server")
		return
	}
	if len(p.Changes) == 0 {
		httpError(w, http.StatusBadRequest, "nothing to change")
		return
	}
	if len(p.Changes) > 500 {
		httpError(w, http.StatusBadRequest, "too many changes in one request")
		return
	}
	layout := detectLayout(srv)
	if layout.ConfigDir == "" {
		httpError(w, http.StatusBadRequest, "no config directory was detected for this server")
		return
	}

	existing, kind, err := readFields(layout.ConfigDir, p.File)
	if err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}
	known := map[string]pz.Field{}
	for _, f := range existing {
		known[f.Key] = f
	}

	// Validate everything before writing anything, so a single bad value
	// cannot leave the file half updated.
	normalised := map[string]string{}
	needsRestart := []string{}
	locked := a.lockedKeys(srv, p.File)
	for key, value := range p.Changes {
		field, ok := known[key]
		if !ok {
			httpError(w, http.StatusBadRequest, fmt.Sprintf("%s is not a setting in this file", key))
			return
		}
		if reason := locked[key]; reason != "" && value != field.Value && !(field.Secret && value == "") {
			httpError(w, http.StatusBadRequest, key+": "+reason)
			return
		}
		clean, err := pz.ValidateField(field, value)
		if err != nil {
			httpError(w, http.StatusBadRequest, err.Error())
			return
		}
		// A blank secret means the operator did not touch it.
		if field.Secret && clean == "" {
			continue
		}
		if clean == field.Value {
			continue // unchanged
		}
		normalised[key] = clean
		if field.Applies != pz.AppliesOnReload {
			needsRestart = append(needsRestart, key)
		}
	}
	if len(normalised) == 0 {
		ok(w, map[string]any{"message": "Nothing changed.", "changed": 0})
		return
	}

	path, err := safeConfigPath(layout.ConfigDir, p.File)
	if err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}
	backupDir := filepath.Join(a.backup.Dir(srv.ID), "config")

	var backup string
	if kind == "sandbox" {
		sb, err := pz.LoadSandbox(path)
		if err != nil {
			httpError(w, http.StatusInternalServerError, err.Error())
			return
		}
		for key, value := range normalised {
			if err := sb.Set(key, value); err != nil {
				httpError(w, http.StatusBadRequest, err.Error())
				return
			}
		}
		if backup, err = sb.Save(backupDir); err != nil {
			httpError(w, http.StatusInternalServerError, err.Error())
			return
		}
	} else {
		ini, err := pz.LoadINI(path)
		if err != nil {
			httpError(w, http.StatusInternalServerError, err.Error())
			return
		}
		for key, value := range normalised {
			if err := ini.Set(key, value); err != nil {
				httpError(w, http.StatusBadRequest, err.Error())
				return
			}
		}
		if backup, err = ini.Save(backupDir); err != nil {
			httpError(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	a.scanner.Invalidate(layout.Base)

	changed := make([]string, 0, len(normalised))
	for key := range normalised {
		changed = append(changed, key)
	}

	// Tell the operator honestly what will and will not take effect now.
	message := fmt.Sprintf("Saved %d setting%s.", len(normalised), plural(len(normalised)))
	reloaded := false
	switch {
	case kind == "sandbox":
		message += " Sandbox settings are read when the world loads, so the server needs a restart."
	case p.Reload:
		if _, err := a.rconFor(srv).Exec("reloadoptions"); err == nil {
			reloaded = true
			message += " The server reloaded its options."
		} else {
			message += " The server could not be asked to reload, so restart it to apply."
		}
	default:
		message += " Reload options or restart to apply."
	}
	// Do not repeat the point for sandbox files, where it applies to everything.
	if len(needsRestart) > 0 && kind != "sandbox" {
		message += " " + strings.Join(needsRestart, ", ") +
			plural2(len(needsRestart), " is", " are") +
			" only read at startup and will not change until the server restarts."
	}

	a.event(store.Event{
		Kind: "config.edit", Severity: store.SevWarn, Source: source(r), Actor: actor(r),
		ServerID: srv.ID, Server: srv.Name,
		Message: fmt.Sprintf("Changed %d setting%s in %s", len(normalised), plural(len(normalised)), p.File),
		Detail:  strings.Join(changed, ", "),
	})

	ok(w, map[string]any{
		"message": message, "changed": len(normalised), "keys": changed,
		"needsRestart": needsRestart, "reloaded": reloaded, "backup": backup,
	})
}

func plural2(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
