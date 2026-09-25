package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/TheWarBoys2/pzadmin/internal/config"
	"github.com/TheWarBoys2/pzadmin/internal/pz"
	"github.com/TheWarBoys2/pzadmin/internal/steam"
	"github.com/TheWarBoys2/pzadmin/internal/store"
)

// Mod requests let players ask for a mod, through a bot holding a key with
// the request scope, without that bot being able to change anything. A
// request only waits in a list on the server's Mods tab; the administrator
// approves it there, which adds it through the same path as the Save button.

const (
	modRequestPending  = "pending"
	modRequestApproved = "approved"
	modRequestRejected = "rejected"

	// maxPendingPerServer stops a bot being used to flood the list.
	maxPendingPerServer = 50
	// maxDecidedKept is how many approved and rejected requests are kept, so
	// a bot can still tell someone what became of theirs.
	maxDecidedKept = 200
)

// modRequest is one request for a Workshop item on one server.
type modRequest struct {
	ID         string   `json:"id"`
	ServerID   string   `json:"serverId"`
	WorkshopID string   `json:"workshopId"`
	Title      string   `json:"title"`
	ModIDs     []string `json:"modIds"`
	MapFolders []string `json:"mapFolders"`
	// RequestedBy is whoever the bot says asked, such as a Discord name. It
	// is free text: PZAdmin has no way to check it.
	RequestedBy string `json:"requestedBy"`
	Note        string `json:"note"`
	// Via is the key the request came in through.
	Via       string       `json:"via"`
	Status    string       `json:"status"`
	Created   time.Time    `json:"created"`
	Decided   OptionalTime `json:"decided"`
	DecidedBy string       `json:"decidedBy,omitempty"`
	// Reason is the administrator's note when rejecting.
	Reason string `json:"reason,omitempty"`
}

type modRequestStore struct {
	mu   sync.Mutex
	path string
	list []*modRequest
}

func newModRequestStore(path string) *modRequestStore {
	s := &modRequestStore{path: path}
	if b, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(b, &s.list)
	}
	return s
}

// saveLocked writes the list. The caller holds mu.
func (s *modRequestStore) saveLocked() {
	b, err := json.Marshal(s.list)
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return
	}
	tmp := s.path + ".tmp"
	if os.WriteFile(tmp, b, 0o600) == nil {
		_ = os.Rename(tmp, s.path)
	}
}

var (
	errModRequestDuplicate = errors.New("that mod has already been requested for this server and is waiting for approval")
	errModRequestFull      = errors.New("this server already has 50 requests waiting; ask the admin to work through them first")
)

// add stores a new pending request, refusing a second pending request for the
// same item on the same server. On a duplicate it returns the existing one.
func (s *modRequestStore) add(req modRequest) (modRequest, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	pending := 0
	for _, v := range s.list {
		if v.ServerID != req.ServerID || v.Status != modRequestPending {
			continue
		}
		if v.WorkshopID == req.WorkshopID {
			return *v, errModRequestDuplicate
		}
		pending++
	}
	if pending >= maxPendingPerServer {
		return modRequest{}, errModRequestFull
	}
	for {
		req.ID = config.RandomToken(4)
		if s.findLocked(req.ID) == nil {
			break
		}
	}
	req.Status = modRequestPending
	req.Created = time.Now()
	cp := req
	s.list = append(s.list, &cp)
	s.saveLocked()
	return req, nil
}

func (s *modRequestStore) findLocked(id string) *modRequest {
	for _, v := range s.list {
		if v.ID == id {
			return v
		}
	}
	return nil
}

func (s *modRequestStore) get(id string) (modRequest, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if v := s.findLocked(id); v != nil {
		return *v, true
	}
	return modRequest{}, false
}

// decide marks a pending request approved or rejected, and trims old
// decisions. It fails if someone else decided it first.
func (s *modRequestStore) decide(id, status, by, reason string) (modRequest, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v := s.findLocked(id)
	if v == nil {
		return modRequest{}, errors.New("that request no longer exists")
	}
	if v.Status != modRequestPending {
		return *v, errors.New("that request has already been " + v.Status)
	}
	v.Status = status
	v.Decided = At(time.Now())
	v.DecidedBy = by
	v.Reason = reason

	decided := 0
	for i := len(s.list) - 1; i >= 0; i-- {
		if s.list[i].Status == modRequestPending {
			continue
		}
		decided++
		if decided > maxDecidedKept {
			s.list = append(s.list[:i], s.list[i+1:]...)
		}
	}
	s.saveLocked()
	return *v, nil
}

// forServer lists a server's requests, pending first and then newest first.
// An empty status means all of them.
func (s *modRequestStore) forServer(serverID, status string) []modRequest {
	s.mu.Lock()
	out := []modRequest{}
	for _, v := range s.list {
		if v.ServerID == serverID && (status == "" || v.Status == status) {
			out = append(out, *v)
		}
	}
	s.mu.Unlock()
	sort.SliceStable(out, func(i, j int) bool {
		pi, pj := out[i].Status == modRequestPending, out[j].Status == modRequestPending
		if pi != pj {
			return pi
		}
		return out[i].Created.After(out[j].Created)
	})
	return out
}

// forgetServer drops a deleted server's requests.
func (s *modRequestStore) forgetServer(serverID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	kept := s.list[:0]
	for _, v := range s.list {
		if v.ServerID != serverID {
			kept = append(kept, v)
		}
	}
	s.list = kept
	s.saveLocked()
}

// cleanRequestText trims a free-text field and refuses control characters,
// which have no business in a name or a note and could garble the event log.
func cleanRequestText(v string, max int, what string) (string, error) {
	v = strings.TrimSpace(v)
	if len([]rune(v)) > max {
		return "", errors.New(what + " must be " + strconv.Itoa(max) + " characters or fewer")
	}
	for _, r := range v {
		if unicode.IsControl(r) {
			return "", errors.New(what + " cannot contain control characters")
		}
	}
	return v, nil
}

// installedWorkshopIDs reads WorkshopItems= for a server. It returns nil if
// the ini cannot be read, so a request is not refused just because the
// server's files are not mounted yet.
func (a *App) installedWorkshopIDs(srv config.Server) map[string]bool {
	path, _, err := a.modConfigFile(detectLayout(srv))
	if err != nil {
		return nil
	}
	ini, err := pz.LoadINI(path)
	if err != nil {
		return nil
	}
	v, _ := ini.Get("WorkshopItems")
	out := map[string]bool{}
	for _, id := range splitList(v) {
		out[id] = true
	}
	return out
}

// --- /api/v1 -----------------------------------------------------------------------

// apiModRequestCreate takes a Workshop link or ID and who asked for it.
func (a *App) apiModRequestCreate(w http.ResponseWriter, r *http.Request) {
	srv, found := a.apiServerFor(w, r)
	if !found {
		return
	}
	var p struct {
		WorkshopID  string `json:"workshopId"`
		RequestedBy string `json:"requestedBy"`
		Note        string `json:"note"`
	}
	defer r.Body.Close()
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil && !errors.Is(err, io.EOF) {
		httpError(w, http.StatusBadRequest, "could not read the request: "+err.Error())
		return
	}

	ids := steam.ParseIDs(p.WorkshopID)
	if len(ids) != 1 {
		httpError(w, http.StatusBadRequest, "send one Workshop link or numeric ID in workshopId")
		return
	}
	wsID := ids[0]
	requestedBy, err := cleanRequestText(p.RequestedBy, 100, "requestedBy")
	if err == nil && requestedBy == "" {
		err = errors.New("say who asked for it in requestedBy, such as their Discord name")
	}
	if err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}
	note, err := cleanRequestText(p.Note, 300, "note")
	if err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}

	if a.installedWorkshopIDs(srv)[wsID] {
		writeStatusJSON(w, http.StatusConflict, map[string]any{
			"error": "that Workshop item is already on " + srv.Name, "reason": "installed",
		})
		return
	}

	req := modRequest{
		ServerID: srv.ID, WorkshopID: wsID, RequestedBy: requestedBy, Note: note,
		Via: apiKeyFrom(r).Name, ModIDs: []string{}, MapFolders: []string{},
	}
	// Ask Steam what the item is, so the admin sees a title rather than a
	// number. If Steam cannot be reached the request is still taken: the
	// admin can look it up when approving.
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	items, err := steam.New(a.steamBase).Details(ctx, []string{wsID})
	cancel()
	if err == nil && len(items) == 1 {
		item := items[0]
		switch {
		case item.Missing:
			httpError(w, http.StatusBadRequest, "Steam has no Workshop item "+wsID+"; check the link")
			return
		case item.AppID != 0 && item.AppID != steam.ProjectZomboidAppID:
			httpError(w, http.StatusBadRequest, "Workshop item "+wsID+" is not a Project Zomboid mod")
			return
		}
		req.Title = item.Title
		if item.ModIDs != nil {
			req.ModIDs = item.ModIDs
		}
		if item.MapFolders != nil {
			req.MapFolders = item.MapFolders
		}
	}

	saved, err := a.modRequests.add(req)
	switch {
	case errors.Is(err, errModRequestDuplicate):
		writeStatusJSON(w, http.StatusConflict, map[string]any{
			"error": err.Error(), "reason": "duplicate", "request": saved,
		})
		return
	case err != nil:
		writeStatusJSON(w, http.StatusConflict, map[string]any{"error": err.Error(), "reason": "full"})
		return
	}

	title := saved.Title
	if title == "" {
		title = "Workshop item " + saved.WorkshopID
	}
	detail := "Requested by " + saved.RequestedBy + "."
	if saved.Note != "" {
		detail += " " + saved.Note
	}
	a.event(store.Event{
		Kind: "mods.request", Severity: store.SevInfo, Source: source(r), Actor: actor(r),
		ServerID: srv.ID, Server: srv.Name,
		Message: "Mod requested for " + srv.Name + ": " + title, Detail: detail,
	})
	writeStatusJSON(w, http.StatusCreated, map[string]any{"request": saved})
}

func (a *App) apiModRequestList(w http.ResponseWriter, r *http.Request) {
	srv, found := a.apiServerFor(w, r)
	if !found {
		return
	}
	status := r.URL.Query().Get("status")
	switch status {
	case "", modRequestPending, modRequestApproved, modRequestRejected:
	default:
		httpError(w, http.StatusBadRequest, `status must be "pending", "approved" or "rejected"`)
		return
	}
	writeJSON(w, map[string]any{"serverId": srv.ID, "requests": a.modRequests.forServer(srv.ID, status)})
}

// --- dashboard (browser session only) --------------------------------------------

func (a *App) handleModRequests(w http.ResponseWriter, r *http.Request) {
	srv, found := a.cfg.Server(r.URL.Query().Get("id"))
	if !found {
		httpError(w, http.StatusNotFound, "no such server")
		return
	}
	writeJSON(w, map[string]any{"requests": a.modRequests.forServer(srv.ID, "")})
}

// handleModRequestApprove adds the requested item to the end of the load
// order and writes it the way Save does, pre-flight check included.
func (a *App) handleModRequestApprove(w http.ResponseWriter, r *http.Request) {
	var p struct {
		ID      string `json:"id"`
		Confirm bool   `json:"confirm"`
	}
	if !decodeJSON(w, r, &p) {
		return
	}
	req, found := a.modRequests.get(p.ID)
	if !found {
		httpError(w, http.StatusNotFound, "that request no longer exists")
		return
	}
	if req.Status != modRequestPending {
		httpError(w, http.StatusConflict, "that request has already been "+req.Status)
		return
	}
	srv, found := a.cfg.Server(req.ServerID)
	if !found {
		httpError(w, http.StatusNotFound, "no such server")
		return
	}
	path, _, err := a.modConfigFile(detectLayout(srv))
	if err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}
	ini, err := pz.LoadINI(path)
	if err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	modsLine, _ := ini.Get("Mods")
	workshopLine, _ := ini.Get("WorkshopItems")
	mods := append(splitList(modsLine), req.ModIDs...)
	workshop := append(splitList(workshopLine), req.WorkshopID)

	code, out := a.applyModList(r, srv, mods, workshop, nil, p.Confirm)
	if code != http.StatusOK {
		writeStatusJSON(w, code, out)
		return
	}
	decided, err := a.modRequests.decide(req.ID, modRequestApproved, actor(r), "")
	if err == nil {
		out["request"] = decided
		title := req.Title
		if title == "" {
			title = "Workshop item " + req.WorkshopID
		}
		a.event(store.Event{
			Kind: "mods.request.approved", Severity: store.SevInfo, Source: source(r), Actor: actor(r),
			ServerID: srv.ID, Server: srv.Name,
			Message: "Mod request approved for " + srv.Name + ": " + title,
			Detail:  "Requested by " + req.RequestedBy + ".",
		})
	}
	msg := "Added. Restart " + srv.Name + " to load it."
	if len(req.ModIDs) == 0 {
		msg = "Added the Workshop ID. Steam's page does not name the mod ID, so once the server " +
			"has downloaded it, enable it from the installed mods. Restart " + srv.Name + " to download it."
	}
	if len(req.MapFolders) > 0 {
		msg += " It adds a map: add " + strings.Join(req.MapFolders, ", ") + " to the Map setting as well."
	}
	out["message"] = msg
	writeJSON(w, out)
}

func (a *App) handleModRequestReject(w http.ResponseWriter, r *http.Request) {
	var p struct {
		ID     string `json:"id"`
		Reason string `json:"reason"`
	}
	if !decodeJSON(w, r, &p) {
		return
	}
	reason, err := cleanRequestText(p.Reason, 300, "The reason")
	if err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}
	req, err := a.modRequests.decide(p.ID, modRequestRejected, actor(r), reason)
	if err != nil {
		httpError(w, http.StatusConflict, err.Error())
		return
	}
	srv, _ := a.cfg.Server(req.ServerID)
	title := req.Title
	if title == "" {
		title = "Workshop item " + req.WorkshopID
	}
	detail := "Requested by " + req.RequestedBy + "."
	if reason != "" {
		detail += " " + reason
	}
	a.event(store.Event{
		Kind: "mods.request.rejected", Severity: store.SevInfo, Source: source(r), Actor: actor(r),
		ServerID: req.ServerID, Server: srv.Name,
		Message: "Mod request rejected for " + srv.Name + ": " + title, Detail: detail,
	})
	ok(w, map[string]any{"request": req})
}
