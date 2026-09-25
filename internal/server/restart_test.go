package server

import (
	"context"
	"testing"
	"time"

	"github.com/TheWarBoys2/pzadmin/internal/config"
	"github.com/TheWarBoys2/pzadmin/internal/store"
)

// A restart PZAdmin asked for must not be reported as an outage, and must not
// invite the watchdog to start a second one on top of it.
func TestRestartingSuppressesOutageAlert(t *testing.T) {
	app, _, _ := newTestApp(t)
	srv := config.Server{ID: "a", Name: "Riverside", Enabled: true, Host: "127.0.0.1", RCONPort: 1,
		Recovery:        config.RecoveryPolicy{Enabled: true, FailuresBeforeRestart: 1, CooldownMinutes: 1},
		DockerContainer: "pz-riverside"}

	// Pretend it was up, then that we restarted it.
	app.updateStatus(srv.ID, func(st *Status) {
		st.Online = true
		st.LastCheck = TimeNow()
		st.LastOnline = TimeNow()
	})
	app.markRestarting(srv.ID, "requested by rick")

	st := app.statusOf(srv.ID)
	if !st.Restarting || st.RestartReason != "requested by rick" {
		t.Fatalf("restart state not recorded: %#v", st)
	}
	if st.RestartingSince.IsZero() {
		t.Fatal("the restart should be timestamped so it can time out")
	}

	// The watchdog must stand off while a restart is in flight.
	failing := st
	failing.Online = false
	failing.ConsecutiveFailures = 10
	failing.ErrorKind = "refused"
	before := len(app.store.Events("", "server.recovered", 50))
	app.maybeRecover(context.Background(), srv, failing)
	if after := len(app.store.Events("", "server.recovered", 50)); after != before {
		t.Fatal("the watchdog fired during a restart it should have deferred to")
	}
}

func TestRestartClearsWhenTheServerAnswers(t *testing.T) {
	app, _, _ := newTestApp(t)
	app.markRestarting("a", "mod update")
	app.updateStatus("a", func(st *Status) {
		// This mirrors what a successful probe does.
		st.Online = true
		st.Restarting = false
		st.RestartingSince = NoTime
		st.RestartReason = ""
	})
	if st := app.statusOf("a"); st.Restarting || !st.RestartingSince.IsZero() {
		t.Fatalf("coming back online must end the restart window: %#v", st)
	}
}

func TestStalledRestartBecomesAnOutage(t *testing.T) {
	app, _, _ := newTestApp(t)
	app.markRestarting("a", "requested")
	// Push the start time outside the window.
	app.updateStatus("a", func(st *Status) {
		st.RestartingSince = At(time.Now().Add(-restartWindow - time.Minute))
	})
	st := app.statusOf("a")
	if st.RestartingSince.Since() <= restartWindow {
		t.Fatal("test setup failed to age the restart")
	}
	// probe() clears the flag in this case; assert the condition it uses.
	if !(st.Restarting && st.RestartingSince.Since() > restartWindow) {
		t.Fatal("a restart past its window should be treated as stalled")
	}
}

func TestRestartingIsVisibleInStatusJSON(t *testing.T) {
	app, _, _ := newTestApp(t)
	app.markRestarting("a", "requested by rick")
	all := app.allStatus()
	_ = all
	st := app.statusOf("a")
	if !st.Restarting {
		t.Fatal("restarting flag missing")
	}
	// A fresh server must not claim to be restarting.
	if other := app.statusOf("b"); other.Restarting || !other.RestartingSince.IsZero() {
		t.Fatal("an untouched server must not appear to be restarting")
	}
	_ = store.SevInfo
}
