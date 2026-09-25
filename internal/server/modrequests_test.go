package server

import (
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
)

func TestModRequestNeedsTheRequestScope(t *testing.T) {
	app, c, id, _ := modServer(t)
	h := app.Handler()
	readOnly, _ := createKey(t, c, map[string]any{"name": "reader"})
	control, _ := createKey(t, c, map[string]any{"name": "controller", "scopes": []string{"control"}})
	body := `{"workshopId":"2169435993","requestedBy":"Rick"}`

	for _, key := range []string{readOnly, control} {
		if rec := bearer(t, h, key, http.MethodPost, "/api/v1/servers/"+id+"/mod-requests", body); rec.Code != http.StatusForbidden {
			t.Fatalf("a key without the request scope got %d: %s", rec.Code, rec.Body.String())
		}
	}
}

func TestModRequestWaitsThenApprovalAddsIt(t *testing.T) {
	app, c, id, iniPath := modServer(t)
	h := app.Handler()
	key, _ := createKey(t, c, map[string]any{"name": "discord-cog", "scopes": []string{"request"}})

	// By name, in any case, with a pasted link.
	rec := bearer(t, h, key, http.MethodPost, "/api/v1/servers/"+url.PathEscape("riverside")+"/mod-requests",
		`{"workshopId":"https://steamcommunity.com/sharedfiles/filedetails/?id=2169435993","requestedBy":"Rick#0001","note":"for the dance party"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("request: %d %s", rec.Code, rec.Body.String())
	}
	out := decode(t, rec)
	req := out["request"].(map[string]any)
	if req["title"] != "True Actions Dancing" || req["status"] != "pending" || req["via"] != "discord-cog" {
		t.Fatalf("unexpected request: %v", req)
	}

	// Nothing is written until it is approved.
	if b, _ := os.ReadFile(iniPath); strings.Contains(string(b), "2169435993") {
		t.Fatal("a request must not change the ini")
	}

	// Asking again is a duplicate, and says which request it matches.
	rec = bearer(t, h, key, http.MethodPost, "/api/v1/servers/"+id+"/mod-requests",
		`{"workshopId":"2169435993","requestedBy":"Glenn"}`)
	if rec.Code != http.StatusConflict || decode(t, rec)["reason"] != "duplicate" {
		t.Fatalf("duplicate: %d %s", rec.Code, rec.Body.String())
	}

	// The dashboard lists it.
	rec = c.do(http.MethodGet, "/api/mods/requests?id="+id, nil)
	if !strings.Contains(rec.Body.String(), "Rick#0001") {
		t.Fatalf("list: %s", rec.Body.String())
	}

	rec = c.do(http.MethodPost, "/api/mods/requests/approve", map[string]any{"id": req["id"], "confirm": true})
	if rec.Code != http.StatusOK {
		t.Fatalf("approve: %d %s", rec.Code, rec.Body.String())
	}
	b, _ := os.ReadFile(iniPath)
	if !strings.Contains(string(b), "WorkshopItems=2392709985;2169435993") ||
		strings.Count(string(b), "TrueActionsDancing") != 1 {
		t.Fatalf("approval should add the Workshop ID once and keep the mod once:\n%s", b)
	}

	// The key sees the outcome.
	rec = bearer(t, h, key, http.MethodGet, "/api/v1/servers/"+id+"/mod-requests?status=approved", "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"status":"approved"`) {
		t.Fatalf("list approved: %d %s", rec.Code, rec.Body.String())
	}

	// And it cannot be approved twice.
	rec = c.do(http.MethodPost, "/api/mods/requests/approve", map[string]any{"id": req["id"]})
	if rec.Code != http.StatusConflict {
		t.Fatalf("second approve: %d %s", rec.Code, rec.Body.String())
	}

	// Once installed, asking for it again says so.
	rec = bearer(t, h, key, http.MethodPost, "/api/v1/servers/"+id+"/mod-requests",
		`{"workshopId":"2169435993","requestedBy":"Glenn"}`)
	if rec.Code != http.StatusConflict || decode(t, rec)["reason"] != "installed" {
		t.Fatalf("installed: %d %s", rec.Code, rec.Body.String())
	}
}

func TestModRequestReject(t *testing.T) {
	app, c, id, _ := modServer(t)
	key, _ := createKey(t, c, map[string]any{"name": "cog", "scopes": []string{"request"}})
	rec := bearer(t, app.Handler(), key, http.MethodPost, "/api/v1/servers/"+id+"/mod-requests",
		`{"workshopId":"2169435993","requestedBy":"Negan"}`)
	reqID := decode(t, rec)["request"].(map[string]any)["id"]

	rec = c.do(http.MethodPost, "/api/mods/requests/reject", map[string]any{"id": reqID, "reason": "Too heavy"})
	if rec.Code != http.StatusOK {
		t.Fatalf("reject: %d %s", rec.Code, rec.Body.String())
	}
	got := app.modRequests.forServer(id, "rejected")
	if len(got) != 1 || got[0].Reason != "Too heavy" || got[0].DecidedBy != "rick" {
		t.Fatalf("rejected: %#v", got)
	}
}

func TestModRequestValidation(t *testing.T) {
	app, c, id, _ := modServer(t)
	h := app.Handler()
	key, _ := createKey(t, c, map[string]any{"name": "cog", "scopes": []string{"request"}})
	for _, body := range []string{
		`{"workshopId":"not a link","requestedBy":"Rick"}`,
		`{"workshopId":"2169435993"}`,
		`{"workshopId":"2169435993","requestedBy":"Ri\u0000ck"}`,
		`{"workshopId":"2169435993","requestedBy":"Rick","extra":1}`,
	} {
		if rec := bearer(t, h, key, http.MethodPost, "/api/v1/servers/"+id+"/mod-requests", body); rec.Code != http.StatusBadRequest {
			t.Fatalf("%s: got %d %s", body, rec.Code, rec.Body.String())
		}
	}
	if rec := bearer(t, h, key, http.MethodPost, "/api/v1/servers/nowhere/mod-requests",
		`{"workshopId":"2169435993","requestedBy":"Rick"}`); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown server: %d", rec.Code)
	}
}

func TestModRequestKeyLimitedToOtherServerSeesNothing(t *testing.T) {
	app, c, id, _ := modServer(t)
	seedServer(t, app, `{"name":"Muldraugh","enabled":false,"host":"127.0.0.1","rconPort":1,"rconPassword":"x"}`)
	var other string
	for _, s := range app.cfg.Get().Servers {
		if s.ID != id {
			other = s.ID
		}
	}
	key, _ := createKey(t, c, map[string]any{"name": "cog", "scopes": []string{"request"}, "servers": []string{other}})
	rec := bearer(t, app.Handler(), key, http.MethodPost, "/api/v1/servers/Riverside/mod-requests",
		`{"workshopId":"2169435993","requestedBy":"Rick"}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("by name on a server outside the key: %d %s", rec.Code, rec.Body.String())
	}
}
