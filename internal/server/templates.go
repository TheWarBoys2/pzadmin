package server

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"log"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/TheWarBoys2/pzadmin/internal/fsutil"
	"github.com/TheWarBoys2/pzadmin/internal/provision"
	"github.com/TheWarBoys2/pzadmin/internal/pz"
	"github.com/TheWarBoys2/pzadmin/internal/store"
)

// A server template is a saved starting point for the new-server wizard: the
// Server/ files (settings, sandbox, mods and spawn points) plus the few basics
// the wizard asks for first. Picking one fills the wizard in; anything can
// still be changed there before the server is written, so servers that are
// "the same but slightly different" start from one template.
//
// Templates never hold a world, the RCON password or the per-server IDs: the
// wizard generates those for every new server as it always has.
type serverTemplate struct {
	ID          string             `json:"id"`
	Name        string             `json:"name"`
	Description string             `json:"description,omitempty"`
	CreatedFrom string             `json:"createdFrom,omitempty"`
	Created     time.Time          `json:"created"`
	Updated     time.Time          `json:"updated"`
	Files       provision.Starting `json:"files"`
	Basics      templateBasics     `json:"basics"`
}

// templateBasics are the first-page values a template fills in. Zero means
// the wizard's own default.
type templateBasics struct {
	MaxPlayers    int   `json:"maxPlayers,omitempty"`
	MemoryGB      int   `json:"memoryGb,omitempty"`
	UpdateOnStart *bool `json:"updateOnStart,omitempty"`
}

func (b templateBasics) valid() error {
	if b.MaxPlayers < 0 || b.MaxPlayers > 100 {
		return errors.New("max players must be between 1 and 100")
	}
	if b.MemoryGB < 0 || b.MemoryGB > 64 {
		return errors.New("memory must be between 1 and 64 GB")
	}
	return nil
}

// starting returns the template's files, labelled with its name.
func (t *serverTemplate) starting() provision.Starting {
	s := t.Files
	s.Label = "the " + t.Name + " template"
	return s
}

const maxTemplates = 100

type templateStore struct {
	mu   sync.Mutex
	path string
	list []*serverTemplate
	// loadErr is set when templates.json could not be read at start.
	loadErr error
}

func newTemplateStore(path string) *templateStore {
	s := &templateStore{path: path}
	if _, err := fsutil.ReadJSON(path, &s.list); err != nil {
		log.Printf("templates: %v", err)
		s.list, s.loadErr = nil, err
	}
	return s
}

func (s *templateStore) saveLocked() error {
	// The join password and tokens in a template's ini are the operator's
	// own, as in any server's files, so the file is owner-only like the rest
	// of /data.
	return fsutil.WriteJSON(s.path, s.list, 0o600)
}

// all returns copies, sorted by name.
func (s *templateStore) all() []serverTemplate {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]serverTemplate, 0, len(s.list))
	for _, t := range s.list {
		out = append(out, *t)
	}
	sort.Slice(out, func(i, j int) bool { return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name) })
	return out
}

func (s *templateStore) get(id string) (serverTemplate, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, t := range s.list {
		if t.ID == id {
			return *t, true
		}
	}
	return serverTemplate{}, false
}

func cleanTemplateName(name, description string) (string, string, error) {
	name = strings.TrimSpace(name)
	description = strings.TrimSpace(description)
	if name == "" {
		return "", "", errors.New("give the template a name")
	}
	if utf8.RuneCountInString(name) > 60 {
		return "", "", errors.New("keep the name to 60 characters")
	}
	if utf8.RuneCountInString(description) > 300 {
		return "", "", errors.New("keep the description to 300 characters")
	}
	return name, description, nil
}

// withoutRCONPassword blanks the source server's RCON password: every new
// server gets its own, so a template has no use for it.
func withoutRCONPassword(files provision.Starting) (provision.Starting, error) {
	ini, err := pz.ParseINI(files.INI)
	if err != nil {
		return files, err
	}
	if _, found := ini.Get("RCONPassword"); found {
		if err := ini.Set("RCONPassword", ""); err != nil {
			return files, err
		}
		files.INI = ini.Render()
	}
	return files, nil
}

func (s *templateStore) add(t serverTemplate) (serverTemplate, error) {
	var err error
	if t.Name, t.Description, err = cleanTemplateName(t.Name, t.Description); err != nil {
		return t, err
	}
	if err := t.Basics.valid(); err != nil {
		return t, err
	}
	if t.Files, err = withoutRCONPassword(t.Files); err != nil {
		return t, err
	}
	t.Files.Label = ""
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		return t, err
	}
	t.ID = hex.EncodeToString(buf)
	t.Created = time.Now().UTC()
	t.Updated = t.Created

	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.list) >= maxTemplates {
		return t, errors.New("there are already 100 templates; delete one first")
	}
	for _, other := range s.list {
		if strings.EqualFold(other.Name, t.Name) {
			return t, errors.New("a template called " + other.Name + " already exists")
		}
	}
	s.list = append(s.list, &t)
	if err := s.saveLocked(); err != nil {
		s.list = s.list[:len(s.list)-1]
		return t, err
	}
	return t, nil
}

// update replaces a template's name, description, basics and, when files is
// not nil, its files.
func (s *templateStore) update(id, name, description string, basics templateBasics, files *provision.Starting) (serverTemplate, error) {
	var err error
	if name, description, err = cleanTemplateName(name, description); err != nil {
		return serverTemplate{}, err
	}
	if err := basics.valid(); err != nil {
		return serverTemplate{}, err
	}
	if files != nil {
		clean, err := withoutRCONPassword(*files)
		if err != nil {
			return serverTemplate{}, err
		}
		clean.Label = ""
		files = &clean
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var target *serverTemplate
	for _, t := range s.list {
		if t.ID == id {
			target = t
		} else if strings.EqualFold(t.Name, name) {
			return serverTemplate{}, errors.New("a template called " + t.Name + " already exists")
		}
	}
	if target == nil {
		return serverTemplate{}, errNoTemplate
	}
	before := *target
	target.Name, target.Description, target.Basics = name, description, basics
	if files != nil {
		target.Files = *files
	}
	target.Updated = time.Now().UTC()
	if err := s.saveLocked(); err != nil {
		*target = before
		return serverTemplate{}, err
	}
	return *target, nil
}

func (s *templateStore) remove(id string) (serverTemplate, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, t := range s.list {
		if t.ID != id {
			continue
		}
		before := s.list
		s.list = append(append([]*serverTemplate{}, s.list[:i]...), s.list[i+1:]...)
		if err := s.saveLocked(); err != nil {
			s.list = before
			return serverTemplate{}, err
		}
		return *t, nil
	}
	return serverTemplate{}, errNoTemplate
}

var errNoTemplate = errors.New("no such template")

// templateSummary is a template as listed: everything but the files.
type templateSummary struct {
	ID          string         `json:"id"`
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	CreatedFrom string         `json:"createdFrom,omitempty"`
	Created     time.Time      `json:"created"`
	Updated     time.Time      `json:"updated"`
	Basics      templateBasics `json:"basics"`
	Mods        int            `json:"mods"`
	Map         string         `json:"map,omitempty"`
}

func summarizeTemplate(t serverTemplate) templateSummary {
	out := templateSummary{ID: t.ID, Name: t.Name, Description: t.Description, CreatedFrom: t.CreatedFrom,
		Created: t.Created, Updated: t.Updated, Basics: t.Basics}
	if ini, err := pz.ParseINI(t.Files.INI); err == nil {
		if mods, _ := ini.Get("Mods"); mods != "" {
			for _, m := range strings.Split(mods, ";") {
				if strings.TrimSpace(m) != "" {
					out.Mods++
				}
			}
		}
		out.Map, _ = ini.Get("Map")
	}
	return out
}

// handleTemplates lists the templates.
func (a *App) handleTemplates(w http.ResponseWriter, r *http.Request) {
	list := a.templates.all()
	out := make([]templateSummary, 0, len(list))
	for _, t := range list {
		out = append(out, summarizeTemplate(t))
	}
	writeJSON(w, map[string]any{"templates": out})
}

// templateRequest saves a template from any starting point the wizard knows,
// with the wizard's changes applied. Saving a server as it is today is
// start "clone" with no changes.
type templateRequest struct {
	ID          string            `json:"id,omitempty"`
	Name        string            `json:"name"`
	Description string            `json:"description"`
	Start       string            `json:"start"`
	FromServer  string            `json:"fromServerId,omitempty"`
	FromTempl   string            `json:"templateId,omitempty"`
	INI         map[string]string `json:"ini,omitempty"`
	Sandbox     map[string]string `json:"sandbox,omitempty"`
	Basics      templateBasics    `json:"basics"`
}

// templateFiles works out the files a template request describes.
func (a *App) templateFiles(in templateRequest) (provision.Starting, string, error) {
	var req createRequest
	req.Start, req.FromServerID, req.TemplateID = in.Start, in.FromServer, in.FromTempl
	resolved, problem := a.resolveRequest(req)
	if problem != "" {
		return provision.Starting{}, "", errors.New(problem)
	}
	start, err := resolved.Starting()
	if err != nil {
		return start, "", err
	}
	from := start.Name()
	if start.From == "defaults" && start.Label == "" {
		from = "the game defaults"
	}
	start, err = provision.WithChanges(start, in.INI, in.Sandbox)
	return start, from, err
}

// handleTemplateCreate saves a new template.
func (a *App) handleTemplateCreate(w http.ResponseWriter, r *http.Request) {
	var in templateRequest
	if !decodeJSON(w, r, &in) {
		return
	}
	files, from, err := a.templateFiles(in)
	if err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}
	t, err := a.templates.add(serverTemplate{Name: in.Name, Description: in.Description, CreatedFrom: from,
		Files: files, Basics: in.Basics})
	if err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}
	a.event(store.Event{Kind: "admin.action", Severity: store.SevInfo, Source: source(r), Actor: actor(r),
		Message: "Saved server template " + t.Name, Detail: "From " + from + "."})
	ok(w, map[string]any{"template": summarizeTemplate(t), "message": "Saved the " + t.Name + " template."})
}

// handleTemplateUpdate renames a template, changes its basics and, when any
// setting is changed, applies those changes to its files.
func (a *App) handleTemplateUpdate(w http.ResponseWriter, r *http.Request) {
	var in templateRequest
	if !decodeJSON(w, r, &in) {
		return
	}
	current, found := a.templates.get(in.ID)
	if !found {
		httpError(w, http.StatusNotFound, errNoTemplate.Error())
		return
	}
	var files *provision.Starting
	if len(in.INI) > 0 || len(in.Sandbox) > 0 {
		changed, err := provision.WithChanges(current.Files, in.INI, in.Sandbox)
		if err != nil {
			httpError(w, http.StatusBadRequest, err.Error())
			return
		}
		files = &changed
	}
	t, err := a.templates.update(in.ID, in.Name, in.Description, in.Basics, files)
	if err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}
	detail := "Name and basics."
	if files != nil {
		detail = pluralCount(len(in.INI), "server setting", "server settings") + " and " + pluralCount(len(in.Sandbox), "world setting", "world settings") + " changed."
	}
	a.event(store.Event{Kind: "admin.action", Severity: store.SevInfo, Source: source(r), Actor: actor(r),
		Message: "Edited server template " + t.Name, Detail: detail})
	ok(w, map[string]any{"template": summarizeTemplate(t), "message": "Saved the " + t.Name + " template."})
}

// handleTemplateDelete deletes a template. Servers made from it are not
// affected: a template is only read when a server is created.
func (a *App) handleTemplateDelete(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ID string `json:"id"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	t, err := a.templates.remove(in.ID)
	if err != nil {
		httpError(w, http.StatusNotFound, err.Error())
		return
	}
	a.event(store.Event{Kind: "admin.action", Severity: store.SevInfo, Source: source(r), Actor: actor(r),
		Message: "Deleted server template " + t.Name})
	ok(w, map[string]any{"message": "Deleted the " + t.Name + " template. Servers made from it are unchanged."})
}
