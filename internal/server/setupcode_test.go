package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSetupNeedsTheCodeFromTheLog(t *testing.T) {
	app, handler, _ := newTestApp(t)
	c := &client{t: t, handler: handler}

	for _, code := range []string{"", "WRONG-CODE-1234"} {
		rec := c.do(http.MethodPost, "/api/setup", map[string]string{
			"setupCode": code, "username": "mallory", "password": "a-long-enough-password",
		})
		if rec.Code != http.StatusForbidden {
			t.Fatalf("setup with code %q should be refused, got %d", code, rec.Code)
		}
	}
	if app.cfg.Get().SetupComplete {
		t.Fatal("a wrong code must not create the account")
	}

	// People copy codes out of a terminal: lower case and spaces are fine.
	rec := c.do(http.MethodPost, "/api/setup", map[string]string{
		"setupCode": " test code 0000 ", "username": "rick", "password": "a-long-enough-password",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("setup with the right code should work, got %d %s", rec.Code, rec.Body.String())
	}
	if app.pendingSetupCode() != "" {
		t.Fatal("the setup code should be retired once the account exists")
	}
}

func TestSetupCodeGuessesAreRateLimited(t *testing.T) {
	_, handler, _ := newTestApp(t)
	c := &client{t: t, handler: handler}
	var last int
	for i := 0; i < 10; i++ {
		last = c.do(http.MethodPost, "/api/setup", map[string]string{
			"setupCode": "GUESS", "username": "mallory", "password": "a-long-enough-password",
		}).Code
	}
	if last != http.StatusTooManyRequests {
		t.Fatalf("repeated wrong setup codes should be slowed down, got %d", last)
	}
}

func TestNewSetupCodeShape(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 50; i++ {
		code := newSetupCode()
		if len(code) != 14 || strings.Count(code, "-") != 2 {
			t.Fatalf("unexpected setup code shape %q", code)
		}
		for _, r := range strings.ReplaceAll(code, "-", "") {
			if !strings.ContainsRune(setupAlphabet, r) {
				t.Fatalf("setup code %q uses %q, which is easy to misread", code, r)
			}
		}
		if seen[code] {
			t.Fatalf("setup code %q repeated", code)
		}
		seen[code] = true
	}
}

func TestMetricsAreClosedBeforeSetup(t *testing.T) {
	app, handler, _ := newTestApp(t)
	if app.cfg.Get().Metrics.Token == "" {
		t.Fatal("a fresh install should already have a metrics token")
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("metrics before setup should need a token, got %d", rec.Code)
	}
}

func TestMetricsTokenCanBeReplacedAndOnlyWorksAsAHeader(t *testing.T) {
	app, handler, _ := newTestApp(t)
	c := &client{t: t, handler: handler}
	c.setup("rick", "a-long-enough-password")
	old := app.cfg.Get().Metrics.Token

	rec := c.do(http.MethodPost, "/api/metrics/token", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("replacing the token failed: %d %s", rec.Code, rec.Body.String())
	}
	token, _ := decode(t, rec)["token"].(string)
	if token == "" || token == old {
		t.Fatalf("expected a new token, got %q", token)
	}

	scrape := func(set func(*http.Request)) int {
		req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
		set(req)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec.Code
	}
	if code := scrape(func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+old) }); code != http.StatusUnauthorized {
		t.Fatalf("the old token should stop working, got %d", code)
	}
	if code := scrape(func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+token) }); code != http.StatusOK {
		t.Fatalf("the new token should work, got %d", code)
	}
	if code := scrape(func(r *http.Request) { r.URL.RawQuery = "token=" + token }); code != http.StatusUnauthorized {
		t.Fatalf("a token in the query string should be refused, got %d", code)
	}
}
