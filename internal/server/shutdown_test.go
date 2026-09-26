package server

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/TheWarBoys2/pzadmin/internal/pz"
)

// An open browser tab used to hold shutdown for the full timeout, so Docker
// killed PZAdmin before the final save. Streams must end as soon as shutdown
// starts.
func TestShutdownIsNotHeldUpByAnOpenStream(t *testing.T) {
	app, handler, _ := newTestApp(t)
	c := &client{t: t, handler: handler}
	c.setup("rick", "a-long-enough-password")

	ts := httptest.NewUnstartedServer(handler)
	ts.Config.RegisterOnShutdown(app.StopStreams)
	ts.Start()
	defer ts.Close()

	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/stream", nil)
	for _, ck := range c.cookies {
		req.AddCookie(ck)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("stream returned %d", resp.StatusCode)
	}
	// Wait for the first frame so the handler is definitely running.
	if _, err := bufio.NewReader(resp.Body).ReadString('\n'); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	start := time.Now()
	if err := ts.Config.Shutdown(ctx); err != nil {
		t.Fatalf("shutdown did not finish: %v", err)
	}
	if took := time.Since(start); took > 2*time.Second {
		t.Fatalf("shutdown took %s with a stream open", took)
	}
}

// Work started after Close has begun must not run, and must not trip the
// WaitGroup Close is waiting on.
func TestSpawnRefusesWorkOnceClosing(t *testing.T) {
	app, _, _ := newTestApp(t)
	app.Close()
	ran := make(chan struct{}, 1)
	if app.spawn(func() { ran <- struct{}{} }) {
		t.Fatal("spawn should refuse once PZAdmin is closing")
	}
	select {
	case <-ran:
		t.Fatal("the refused work ran anyway")
	case <-time.After(50 * time.Millisecond):
	}
}

// A running job is cancelled by Close, and Close waits for it to record
// that, rather than abandoning it mid-step.
func TestCloseCancelsAndWaitsForRunningWork(t *testing.T) {
	app, _, _ := newTestApp(t)
	finished := make(chan struct{})
	app.spawn(func() {
		<-app.ctx.Done()
		time.Sleep(20 * time.Millisecond)
		close(finished)
	})
	done := make(chan struct{})
	go func() { app.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not return")
	}
	select {
	case <-finished:
	default:
		t.Fatal("Close returned before the running work finished")
	}
}

// Work that ignores cancellation cannot hold shutdown past Docker's stop
// timeout: Close gives up waiting and still does its final save.
func TestCloseDoesNotWaitForeverForStuckWork(t *testing.T) {
	old := shutdownWait
	shutdownWait = 100 * time.Millisecond
	t.Cleanup(func() { shutdownWait = old })

	app, _, _ := newTestApp(t)
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	app.spawn(func() { <-release }) // never looks at app.ctx

	done := make(chan struct{})
	go func() { app.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Close waited for work that never stops")
	}
	found := false
	for _, e := range app.store.Events("", "system.stop", 5) {
		found = found || e.Message == "PZAdmin stopped"
	}
	if !found {
		t.Fatal("the final save did not run")
	}
}

// A backup stops at the next file once PZAdmin is shutting down.
func TestBackupStopsOnShutdown(t *testing.T) {
	root := t.TempDir()
	saves := filepath.Join(root, "Saves")
	if err := os.MkdirAll(saves, 0o755); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		if err := os.WriteFile(filepath.Join(saves, fmt.Sprintf("map_%d.bin", i)), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	b := pz.NewBackupper(t.TempDir())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := b.Create(ctx, "srv1", pz.Layout{SavesDir: saves}, false, 3, ""); !errors.Is(err, pz.ErrStopped) {
		t.Fatalf("expected ErrStopped, got %v", err)
	}
	if list := b.List("srv1"); len(list) != 0 {
		t.Fatalf("a stopped backup must not leave an archive: %#v", list)
	}
}

// Background work, backups included, is cancelled as soon as HTTP shutdown
// starts, not after it has waited for open requests.
func TestBeginShutdownCancelsWorkAtOnce(t *testing.T) {
	app, _, _ := newTestApp(t)
	stopped := make(chan struct{})
	app.spawn(func() {
		<-app.ctx.Done()
		close(stopped)
	})
	app.BeginShutdown()
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("work was not cancelled when shutdown began")
	}
}
