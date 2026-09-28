package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
)

// access is what a dashboard route needs from the signed-in account: a
// permission, and where to find the server it acts on so a user limited to
// some servers can be kept to them.
//
// Every dashboard route is registered with one. The zero value is owner
// only, so a route added without thinking about users is safe by default.
type access struct {
	perm string
	from serverFrom
}

type serverFrom int

const (
	// fromNone: the route is not about one server, or the handler filters
	// what it returns itself.
	fromNone serverFrom = iota
	// fromQueryID and fromQueryServerID read ?id= and ?serverId=.
	fromQueryID
	fromQueryServerID
	// fromQueryServerIDOptional reads ?serverId= when it is there. Without it
	// the handler returns every server's data, filtered to the user's.
	fromQueryServerIDOptional
	// fromBodyServerID reads "serverId" from the JSON body.
	fromBodyServerID
	// fromBodyModRequest reads a mod request's "id" and uses its server.
	fromBodyModRequest
	// fromBodySchedule reads a job's "id" and uses its server.
	fromBodySchedule
)

var ownerOnly = access{perm: permOwner}

func needs(perm string, from serverFrom) access { return access{perm: perm, from: from} }

// errServerNotAllowed is shown as "no such server", so a user limited to some
// servers learns nothing about the others.
var errServerNotAllowed = errors.New("no such server")

// principalFor turns a session into the account behind it, read fresh on
// every request so a change to a user applies from their next click.
func (a *App) principalFor(sess *session) (principal, bool) {
	if sess.UserID == "" {
		p := ownerPrincipal
		p.Name = a.cfg.Get().Username
		return p, true
	}
	u, ok := a.users.get(sess.UserID)
	if !ok || u.Disabled {
		return principal{}, false
	}
	return u.principal(), true
}

// permit checks a request against its route's access. It returns the HTTP
// status and message to refuse with, or 0.
func (a *App) permit(p principal, rule access, r *http.Request) (int, string) {
	if rule.perm == "" {
		rule.perm = permOwner
	}
	if !p.can(rule.perm) {
		if rule.perm == permOwner {
			return http.StatusForbidden, "only the owner can do this"
		}
		return http.StatusForbidden, "your account does not have the " + rule.perm + " permission"
	}
	if p.allServers() || rule.from == fromNone {
		return 0, ""
	}
	id, err := a.routeServer(rule.from, r)
	if err != nil {
		return http.StatusBadRequest, "could not read the request: " + err.Error()
	}
	if id == "" && rule.from == fromQueryServerIDOptional {
		return 0, ""
	}
	if !p.allows(id) {
		return http.StatusNotFound, errServerNotAllowed.Error()
	}
	return 0, ""
}

// routeServer finds the server a request is about. A JSON body is read and
// put back, so the handler decodes exactly the bytes checked here.
func (a *App) routeServer(from serverFrom, r *http.Request) (string, error) {
	switch from {
	case fromQueryID:
		return r.URL.Query().Get("id"), nil
	case fromQueryServerID, fromQueryServerIDOptional:
		return r.URL.Query().Get("serverId"), nil
	}
	b, err := io.ReadAll(io.LimitReader(r.Body, 8<<20+1))
	r.Body.Close()
	if err != nil {
		return "", err
	}
	r.Body = io.NopCloser(bytes.NewReader(b))
	// Decoded the same way the handlers decode, so a body that names two
	// servers resolves to the same one here and there.
	var p struct {
		ServerID string `json:"serverId"`
		ID       string `json:"id"`
	}
	if len(bytes.TrimSpace(b)) > 0 {
		if err := json.Unmarshal(b, &p); err != nil {
			return "", err
		}
	}
	switch from {
	case fromBodyServerID:
		return p.ServerID, nil
	case fromBodyModRequest:
		if req, ok := a.modRequests.get(p.ID); ok {
			return req.ServerID, nil
		}
		return "", nil
	case fromBodySchedule:
		for _, t := range a.cfg.Get().Schedules {
			if t.ID == p.ID {
				return t.ServerID, nil
			}
		}
		return "", nil
	}
	return "", nil
}
