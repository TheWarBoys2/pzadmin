package server

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// addUser makes a user as the owner and returns its id and temporary
// password.
func addUser(t *testing.T, owner *client, req map[string]any) (string, string) {
	t.Helper()
	rec := owner.do(http.MethodPost, "/api/users/create", req)
	if rec.Code != http.StatusOK {
		t.Fatalf("create user: %d %s", rec.Code, rec.Body.String())
	}
	out := decode(t, rec)
	pw, _ := out["password"].(string)
	u, _ := out["user"].(map[string]any)
	id, _ := u["id"].(string)
	if pw == "" || id == "" {
		t.Fatalf("unexpected create response: %s", rec.Body.String())
	}
	return id, pw
}

// signInUser signs a user in and sets their own password.
func signInUser(t *testing.T, handler http.Handler, name, temp string) *client {
	t.Helper()
	c := &client{t: t, handler: handler}
	rec := c.do(http.MethodPost, "/api/login", map[string]string{"username": name, "password": temp})
	if rec.Code != http.StatusOK {
		t.Fatalf("user sign-in: %d %s", rec.Code, rec.Body.String())
	}
	if decode(t, rec)["mustChangePassword"] != true {
		t.Fatalf("a new user should have to choose a password: %s", rec.Body.String())
	}
	rec = c.do(http.MethodPost, "/api/password", map[string]string{"current": temp, "new": "glenn-chosen-password"})
	if rec.Code != http.StatusOK {
		t.Fatalf("choose password: %d %s", rec.Code, rec.Body.String())
	}
	return c
}

func TestUserMustChooseAPassword(t *testing.T) {
	_, handler, owner, _, _ := apiTestApp(t)
	_, temp := addUser(t, owner, map[string]any{"name": "glenn"})

	c := &client{t: t, handler: handler}
	c.do(http.MethodPost, "/api/login", map[string]string{"username": "Glenn", "password": temp})
	rec := c.do(http.MethodGet, "/api/state", nil)
	if rec.Code != http.StatusForbidden || decode(t, rec)["mustChangePassword"] != true {
		t.Fatalf("a temporary password should only allow choosing a real one, got %d %s", rec.Code, rec.Body.String())
	}
	if rec := c.do(http.MethodGet, "/api/me", nil); rec.Code != http.StatusOK {
		t.Fatalf("/api/me should work before the password is chosen, got %d", rec.Code)
	}
	rec = c.do(http.MethodPost, "/api/password", map[string]string{"current": temp, "new": temp})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("keeping the temporary password should be refused, got %d", rec.Code)
	}
	rec = c.do(http.MethodPost, "/api/password", map[string]string{"current": temp, "new": "glenn-chosen-password"})
	if rec.Code != http.StatusOK || decode(t, rec)["signedOut"] != false {
		t.Fatalf("choosing the first password should keep this browser signed in: %d %s", rec.Code, rec.Body.String())
	}
	if rec := c.do(http.MethodGet, "/api/state", nil); rec.Code != http.StatusOK {
		t.Fatalf("after choosing a password the dashboard should load, got %d", rec.Code)
	}
	// The temporary password no longer works.
	other := &client{t: t, handler: handler}
	if rec := other.do(http.MethodPost, "/api/login", map[string]string{"username": "glenn", "password": temp}); rec.Code != http.StatusUnauthorized {
		t.Fatalf("the temporary password should stop working, got %d", rec.Code)
	}
}

func TestUsersFileHasNoPasswords(t *testing.T) {
	app, _, owner, _, _ := apiTestApp(t)
	_, temp := addUser(t, owner, map[string]any{"name": "glenn"})
	rec := owner.do(http.MethodGet, "/api/users", nil)
	if strings.Contains(rec.Body.String(), temp) || strings.Contains(rec.Body.String(), "passwordHash") {
		t.Fatalf("the user list should carry no password material: %s", rec.Body.String())
	}
	b, err := os.ReadFile(filepath.Join(app.dataDir, "users.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), temp) {
		t.Fatal("users.json holds the temporary password in plain text")
	}
	if rec := owner.do(http.MethodGet, "/api/export", nil); strings.Contains(rec.Body.String(), "glenn") {
		t.Fatal("users must not be part of a config export")
	}
}

func TestUserNameRules(t *testing.T) {
	_, _, owner, _, _ := apiTestApp(t)
	addUser(t, owner, map[string]any{"name": "glenn"})
	for _, name := range []string{"Rick", "GLENN", "x", ""} {
		if rec := owner.do(http.MethodPost, "/api/users/create", map[string]any{"name": name}); rec.Code != http.StatusBadRequest {
			t.Fatalf("name %q should be refused, got %d", name, rec.Code)
		}
	}
	if rec := owner.do(http.MethodPost, "/api/users/create", map[string]any{"name": "maggie", "perms": []string{"admin"}}); rec.Code != http.StatusBadRequest {
		t.Fatalf("an unknown permission should be refused, got %d", rec.Code)
	}
}

func TestOwnerOnlyRoutes(t *testing.T) {
	_, handler, owner, _, _ := apiTestApp(t)
	_, temp := addUser(t, owner, map[string]any{"name": "glenn",
		"perms": []string{"control", "console", "mods", "config"}})
	c := signInUser(t, handler, "glenn", temp)

	for _, path := range []string{"/api/users", "/api/keys", "/api/export", "/api/sessions", "/api/browse", "/api/stack"} {
		if rec := c.do(http.MethodGet, path, nil); rec.Code != http.StatusForbidden {
			t.Fatalf("GET %s should be owner only, got %d", path, rec.Code)
		}
	}
	for _, path := range []string{"/api/users/create", "/api/keys/create", "/api/settings", "/api/import",
		"/api/server/delete", "/api/schedules", "/api/sessions/revoke", "/api/discord/bot"} {
		if rec := c.do(http.MethodPost, path, map[string]any{}); rec.Code != http.StatusForbidden {
			t.Fatalf("POST %s should be owner only, got %d", path, rec.Code)
		}
	}
}

func TestUserServerLimitAndPermissions(t *testing.T) {
	app, handler, owner, riverside, muldraugh := apiTestApp(t)
	id, temp := addUser(t, owner, map[string]any{"name": "glenn", "servers": []string{riverside}})
	c := signInUser(t, handler, "glenn", temp)

	// The dashboard shows only Riverside.
	state := decode(t, c.do(http.MethodGet, "/api/state", nil))
	servers, _ := state["servers"].([]any)
	if len(servers) != 1 || servers[0].(map[string]any)["id"] != riverside {
		t.Fatalf("a limited user should see only their server, got %v", servers)
	}
	cfg, _ := state["config"].(map[string]any)
	if n, _ := cfg["notify"].(map[string]any); len(n["webhooks"].([]any)) != 0 {
		t.Fatalf("a user should not be sent PZAdmin's webhooks: %v", n)
	}
	status, _ := state["status"].([]any)
	for _, s := range status {
		if s.(map[string]any)["serverId"] == muldraugh {
			t.Fatal("a limited user was sent another server's status")
		}
	}

	// The other server does not exist as far as they can tell.
	if rec := c.do(http.MethodGet, "/api/server/detail?id="+muldraugh, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("another server's detail should be not found, got %d", rec.Code)
	}
	if rec := c.do(http.MethodGet, "/api/events?serverId="+muldraugh, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("another server's events should be not found, got %d", rec.Code)
	}
	if rec := c.do(http.MethodGet, "/api/server/detail?id="+riverside, nil); rec.Code != http.StatusOK {
		t.Fatalf("their own server's detail should load, got %d %s", rec.Code, rec.Body.String())
	}

	// View only: the buttons are refused.
	body := map[string]any{"serverId": riverside, "action": "save"}
	if rec := c.do(http.MethodPost, "/api/lifecycle", body); rec.Code != http.StatusForbidden {
		t.Fatalf("a view-only user should not restart, got %d", rec.Code)
	}

	// Given control, they can press them on their server but not the other.
	if rec := owner.do(http.MethodPost, "/api/users/update", map[string]any{"id": id, "perms": []string{"control"},
		"servers": []string{riverside}}); rec.Code != http.StatusOK {
		t.Fatalf("update user: %d %s", rec.Code, rec.Body.String())
	}
	rec := c.do(http.MethodPost, "/api/action", map[string]any{"serverId": riverside, "action": "save"})
	if rec.Code == http.StatusForbidden || rec.Code == http.StatusNotFound {
		t.Fatalf("control on their own server should pass the permission check, got %d %s", rec.Code, rec.Body.String())
	}
	rec = c.do(http.MethodPost, "/api/action", map[string]any{"serverId": muldraugh, "action": "save"})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("control on another server should be not found, got %d", rec.Code)
	}
	// A body naming the server twice resolves the same way for the check and
	// the handler, so it cannot smuggle the other server past the check.
	twice := json.RawMessage(`{"serverId":"` + riverside + `","ServerID":"` + muldraugh + `","action":"save"}`)
	rec = c.do(http.MethodPost, "/api/action", twice)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("a body naming another server a second time should not get through, got %d", rec.Code)
	}
	if rec := c.do(http.MethodPost, "/api/console", map[string]any{"serverId": riverside, "command": "players"}); rec.Code != http.StatusForbidden {
		t.Fatalf("console needs its own permission, got %d", rec.Code)
	}

	// Their activity has none of PZAdmin's own events, such as sign-ins.
	events := decode(t, c.do(http.MethodGet, "/api/events", nil))["events"].([]any)
	for _, e := range events {
		ev := e.(map[string]any)
		if ev["serverId"] != riverside {
			t.Fatalf("a limited user was shown an event from elsewhere: %v", ev)
		}
	}
	// The owner's log names the user as the actor.
	found := false
	for _, e := range app.store.Events(riverside, "", 50) {
		if e.Actor == "glenn" {
			found = true
		}
	}
	if !found {
		t.Fatal("the user's action should be logged under their name")
	}
}

func TestDisableResetAndRemoveSignOut(t *testing.T) {
	_, handler, owner, _, _ := apiTestApp(t)
	id, temp := addUser(t, owner, map[string]any{"name": "glenn"})
	c := signInUser(t, handler, "glenn", temp)

	if rec := owner.do(http.MethodPost, "/api/users/disable", map[string]any{"id": id, "disabled": true}); rec.Code != http.StatusOK {
		t.Fatalf("disable: %d %s", rec.Code, rec.Body.String())
	}
	if rec := c.do(http.MethodGet, "/api/state", nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("a disabled user should be signed out, got %d", rec.Code)
	}
	again := &client{t: t, handler: handler}
	if rec := again.do(http.MethodPost, "/api/login", map[string]string{"username": "glenn", "password": "glenn-chosen-password"}); rec.Code != http.StatusForbidden {
		t.Fatalf("a disabled user should not sign in, got %d", rec.Code)
	}
	owner.do(http.MethodPost, "/api/users/disable", map[string]any{"id": id, "disabled": false})

	c = &client{t: t, handler: handler}
	c.do(http.MethodPost, "/api/login", map[string]string{"username": "glenn", "password": "glenn-chosen-password"})
	rec := owner.do(http.MethodPost, "/api/users/reset", map[string]any{"id": id})
	if rec.Code != http.StatusOK {
		t.Fatalf("reset: %d %s", rec.Code, rec.Body.String())
	}
	if rec := c.do(http.MethodGet, "/api/state", nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("a reset should sign the user out, got %d", rec.Code)
	}
	temp2, _ := decode(t, rec)["password"].(string)
	c = signInUser(t, handler, "glenn", temp2)

	if rec := owner.do(http.MethodPost, "/api/users/remove", map[string]any{"id": id}); rec.Code != http.StatusOK {
		t.Fatalf("remove: %d", rec.Code)
	}
	if rec := c.do(http.MethodGet, "/api/state", nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("a removed user should be signed out, got %d", rec.Code)
	}
}

func TestOwnerPasswordChangeLeavesUsersSignedIn(t *testing.T) {
	_, handler, owner, _, _ := apiTestApp(t)
	_, temp := addUser(t, owner, map[string]any{"name": "glenn"})
	c := signInUser(t, handler, "glenn", temp)
	rec := owner.do(http.MethodPost, "/api/password", map[string]string{"current": "a-long-enough-password", "new": "another-long-password"})
	if rec.Code != http.StatusOK {
		t.Fatalf("owner password: %d %s", rec.Code, rec.Body.String())
	}
	if rec := c.do(http.MethodGet, "/api/state", nil); rec.Code != http.StatusOK {
		t.Fatalf("the owner changing their password should not sign other users out, got %d", rec.Code)
	}
}

func TestDeletedServerLeavesUserLists(t *testing.T) {
	s := newUserStore(filepath.Join(t.TempDir(), "users.json"))
	exists := func(string) bool { return true }
	only, _, _ := s.create(userRequest{Name: "glenn", Servers: []string{"a"}}, "rick", "rick", exists)
	both, _, _ := s.create(userRequest{Name: "maggie", Servers: []string{"a", "b"}}, "rick", "rick", exists)
	s.forgetServer("a")
	if u, _ := s.get(only.ID); !u.Disabled || len(u.Servers) != 0 {
		t.Fatalf("a user left with no servers should be disabled, not given every server: %+v", u)
	}
	if u, _ := s.get(both.ID); u.Disabled || len(u.Servers) != 1 || u.Servers[0] != "b" {
		t.Fatalf("a user with other servers should keep them: %+v", u)
	}
}

// A version from before users existed treats every session in sessions.json
// as the administrator's, so a user's session must never be written there.
func TestUserSessionsKeptApartFromTheOwners(t *testing.T) {
	app, handler, owner, _, _ := apiTestApp(t)
	_, temp := addUser(t, owner, map[string]any{"name": "glenn"})
	signInUser(t, handler, "glenn", temp)
	b, err := os.ReadFile(filepath.Join(app.dataDir, "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "glenn") {
		t.Fatal("a user's session was written to sessions.json")
	}
	b, err = os.ReadFile(filepath.Join(app.dataDir, "sessions-users.json"))
	if err != nil || !strings.Contains(string(b), "glenn") {
		t.Fatalf("a user's session should be in sessions-users.json: %v %s", err, b)
	}
	// Both come back after a restart.
	s := newSessionStore(filepath.Join(app.dataDir, "sessions.json"))
	if len(s.list()) != 2 {
		t.Fatalf("expected the owner's and glenn's sessions after reload, got %d", len(s.list()))
	}
}
