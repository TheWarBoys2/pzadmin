package server

import (
	"bufio"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
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
