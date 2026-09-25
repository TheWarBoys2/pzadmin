package store

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func open(t *testing.T) (*Store, string) {
	t.Helper()
	dir := t.TempDir()
	s, err := Open(dir, 30)
	if err != nil {
		t.Fatal(err)
	}
	return s, dir
}

func TestSyncPlayersReportsJoinsAndLeaves(t *testing.T) {
	s, _ := open(t)

	joined, left := s.SyncPlayers("srv", []string{"Rick", "Steve"})
	if len(joined) != 2 || len(left) != 0 {
		t.Fatalf("first sync: joined=%v left=%v", joined, left)
	}

	// A repeat poll with the same players must be silent.
	joined, left = s.SyncPlayers("srv", []string{"Rick", "Steve"})
	if len(joined) != 0 || len(left) != 0 {
		t.Fatalf("steady state should report nothing: joined=%v left=%v", joined, left)
	}

	joined, left = s.SyncPlayers("srv", []string{"Rick"})
	if len(joined) != 0 || len(left) != 1 || left[0] != "Steve" {
		t.Fatalf("expected Steve to leave: joined=%v left=%v", joined, left)
	}

	joined, _ = s.SyncPlayers("srv", []string{"Rick", "Steve"})
	if len(joined) != 1 || joined[0] != "Steve" {
		t.Fatalf("expected Steve to rejoin: %v", joined)
	}
	for _, p := range s.Players("srv") {
		if p.Name == "Steve" && p.Sessions != 2 {
			t.Fatalf("Steve should have 2 sessions, got %d", p.Sessions)
		}
	}
}

func TestPlaytimeAccumulatesOnLeave(t *testing.T) {
	s, _ := open(t)
	s.SyncPlayers("srv", []string{"Rick"})

	// Backdate the session so a measurable amount of playtime accrues.
	s.mu.Lock()
	s.players[key("srv", "Rick")].OnlineSince = time.Now().Add(-90 * time.Minute)
	s.mu.Unlock()

	s.SyncPlayers("srv", nil)
	p := s.Players("srv")[0]
	if p.Online {
		t.Fatal("player should be offline")
	}
	if p.PlaytimeSec < 5300 || p.PlaytimeSec > 5500 {
		t.Fatalf("expected roughly 90 minutes of playtime, got %ds", p.PlaytimeSec)
	}
}

// An outage must not credit players with playtime they did not have.
func TestMarkOfflineClosesSessions(t *testing.T) {
	s, _ := open(t)
	s.SyncPlayers("srv", []string{"Rick", "Steve"})
	left := s.MarkOffline("srv")
	if len(left) != 2 {
		t.Fatalf("expected both players closed out, got %v", left)
	}
	for _, p := range s.Players("srv") {
		if p.Online {
			t.Fatalf("%s still marked online", p.Name)
		}
	}
	if again := s.MarkOffline("srv"); len(again) != 0 {
		t.Fatal("marking offline twice must be a no-op")
	}
}

func TestPlayersSurviveRestart(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir, 30)
	if err != nil {
		t.Fatal(err)
	}
	s.SyncPlayers("srv", []string{"Rick"})
	s.SetSteamID("srv", "Rick", "76561198000000001")
	if err := s.SetNote("srv", "Rick", "builds excellent bases"); err != nil {
		t.Fatal(err)
	}
	s.Flush()

	reopened, err := Open(dir, 30)
	if err != nil {
		t.Fatal(err)
	}
	list := reopened.Players("srv")
	if len(list) != 1 {
		t.Fatalf("expected one player after reload, got %d", len(list))
	}
	if list[0].SteamID != "76561198000000001" || list[0].Note != "builds excellent bases" {
		t.Fatalf("player detail lost: %#v", list[0])
	}
	if list[0].Online {
		t.Fatal("nobody can still be online after PZAdmin restarts")
	}
}

func TestForgetServerRemovesOnlyThatServer(t *testing.T) {
	s, _ := open(t)
	s.SyncPlayers("a", []string{"Rick"})
	s.SyncPlayers("b", []string{"Steve"})
	s.ForgetServer("a")
	if len(s.Players("a")) != 0 {
		t.Fatal("deleted server's players should be gone")
	}
	if len(s.Players("b")) != 1 {
		t.Fatal("other servers must be untouched")
	}
}

func TestEventsPersistAndFilter(t *testing.T) {
	dir := t.TempDir()
	s, _ := Open(dir, 30)
	s.Append(Event{Kind: "server.down", ServerID: "a", Message: "down", Severity: SevError, Source: "monitor"})
	s.Append(Event{Kind: "admin.action", ServerID: "b", Message: "kick", Actor: "rick", Source: "ui"})

	if got := s.Events("", "", 10); len(got) != 2 {
		t.Fatalf("expected 2 events, got %d", len(got))
	}
	if got := s.Events("a", "", 10); len(got) != 1 || got[0].ServerID != "a" {
		t.Fatalf("server filter failed: %#v", got)
	}
	if got := s.Events("", "admin.action", 10); len(got) != 1 || got[0].Kind != "admin.action" {
		t.Fatalf("kind filter failed: %#v", got)
	}
	// Newest first.
	if s.Events("", "", 10)[0].Kind != "admin.action" {
		t.Fatal("events should be returned newest first")
	}

	reopened, _ := Open(dir, 30)
	if len(reopened.Events("", "", 10)) != 2 {
		t.Fatal("events must survive a restart")
	}
}

func TestSubscribeReceivesLiveEventsAndDoesNotBlock(t *testing.T) {
	s, _ := open(t)
	ch, cancel := s.Subscribe()
	defer cancel()

	s.Append(Event{Kind: "test", Message: "hello"})
	select {
	case e := <-ch:
		if e.Message != "hello" {
			t.Fatalf("unexpected event %#v", e)
		}
	case <-time.After(time.Second):
		t.Fatal("subscriber did not receive the event")
	}

	// Overfill the buffer: a stalled browser must never block the monitor.
	done := make(chan struct{})
	go func() {
		for i := 0; i < 500; i++ {
			s.Append(Event{Kind: "flood", Message: "x"})
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Append blocked on a slow subscriber")
	}
}

func TestHistoryThinsToRequestedPoints(t *testing.T) {
	s, _ := open(t)
	base := time.Now().Add(-2 * time.Hour)
	for i := 0; i < 600; i++ {
		s.AddSample(Sample{At: base.Add(time.Duration(i) * 12 * time.Second), ServerID: "a", Online: true, Players: i % 7})
	}
	got := s.History("a", 3*time.Hour, 60)
	if len(got) > 62 {
		t.Fatalf("expected thinning to about 60 points, got %d", len(got))
	}
	if len(got) < 30 {
		t.Fatalf("thinning removed too much, got %d", len(got))
	}
	// Peaks must survive thinning: a busy minute is the interesting one.
	max := 0
	for _, sm := range got {
		if sm.Players > max {
			max = sm.Players
		}
	}
	if max != 6 {
		t.Fatalf("peak player count lost in thinning, got %d", max)
	}
}

func TestHistoryFiltersByServerAndWindow(t *testing.T) {
	s, _ := open(t)
	s.AddSample(Sample{At: time.Now().Add(-48 * time.Hour), ServerID: "a", Players: 9})
	s.AddSample(Sample{At: time.Now(), ServerID: "a", Players: 2})
	s.AddSample(Sample{At: time.Now(), ServerID: "b", Players: 5})

	got := s.History("a", time.Hour, 100)
	if len(got) != 1 || got[0].Players != 2 {
		t.Fatalf("window or server filter failed: %#v", got)
	}
}

func TestPruneRemovesOldFilesOnly(t *testing.T) {
	dir := t.TempDir()
	s, _ := Open(dir, 7)
	old := time.Now().AddDate(0, 0, -30).Format("2006-01-02")
	recent := time.Now().Format("2006-01-02")
	oldPath := filepath.Join(dir, "events", "events-"+old+".jsonl")
	newPath := filepath.Join(dir, "events", "events-"+recent+".jsonl")
	os.WriteFile(oldPath, []byte("{}\n"), 0o644)
	os.WriteFile(newPath, []byte("{}\n"), 0o644)

	if n := s.Prune(); n != 1 {
		t.Fatalf("expected to prune 1 file, pruned %d", n)
	}
	if _, err := os.Stat(oldPath); !os.IsNotExist(err) {
		t.Fatal("old file should be gone")
	}
	if _, err := os.Stat(newPath); err != nil {
		t.Fatal("recent file must be kept")
	}
}

func TestStatsCountsDisk(t *testing.T) {
	s, _ := open(t)
	s.Append(Event{Kind: "x", Message: "y"})
	s.SyncPlayers("a", []string{"Rick"})
	st := s.Stats()
	if st.Players != 1 || st.Events != 1 || st.DiskBytes == 0 {
		t.Fatalf("unexpected stats %#v", st)
	}
}
