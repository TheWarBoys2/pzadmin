package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestOptionalTimeMarshalsZeroAsNull(t *testing.T) {
	b, err := json.Marshal(NoTime)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "null" {
		t.Fatalf("a zero time must marshal as null, got %s", b)
	}

	when := time.Date(2026, 9, 8, 4, 0, 0, 0, time.UTC)
	b, err = json.Marshal(At(when))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "2026-09-08T04:00:00Z") {
		t.Fatalf("a real time must marshal normally, got %s", b)
	}

	var back OptionalTime
	if err := json.Unmarshal([]byte("null"), &back); err != nil {
		t.Fatal(err)
	}
	if !back.IsZero() {
		t.Fatal("null must unmarshal back to the zero time")
	}
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if !back.Time.Equal(when) {
		t.Fatalf("round trip lost the value: %s", back.Time)
	}
}

// A freshly added server has never been probed, backed up, or scheduled for
// restart. Every one of those fields must be absent rather than year one:
// `json:"...,omitempty"` does not skip a zero time.Time, so the browser saw a
// non-empty string and showed a restart banner on a brand new server.
func TestFreshServerReportsNoPhantomTimestamps(t *testing.T) {
	app, handler, _ := newTestApp(t)
	c := &client{t: t, handler: handler}
	c.setup("rick", "a-long-enough-password")

	rec := seedServer(t, app, `{"name":"Riverside","host":"127.0.0.1","rconPort":27015,
	  "rconPassword":"x","enabled":false}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("save failed: %s", rec.Body.String())
	}

	rec = c.do(http.MethodGet, "/api/state", nil)
	body := rec.Body.String()
	if strings.Contains(body, "0001-01-01") {
		t.Fatalf("a zero timestamp reached the browser:\n%s", body)
	}

	var state struct {
		Status []map[string]any `json:"status"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &state); err != nil {
		t.Fatal(err)
	}
	if len(state.Status) != 1 {
		t.Fatalf("expected one server, got %d", len(state.Status))
	}
	for _, field := range []string{"pendingRestartAt", "lastBackup", "lastOnline", "lastRecovery", "lastCheck", "startedAt"} {
		if v := state.Status[0][field]; v != nil {
			t.Fatalf("%s should be null for a server that has never run, got %v", field, v)
		}
	}
}
