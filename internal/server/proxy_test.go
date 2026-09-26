package server

import (
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestForwardingHeadersAreIgnoredWithoutATrustedProxy(t *testing.T) {
	var p proxyTrust
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.RemoteAddr = "203.0.113.9:4000"
	r.Header.Set("X-Forwarded-For", "1.2.3.4")
	r.Header.Set("X-Forwarded-Proto", "https")
	ip, secure := p.resolve(r)
	if ip != "203.0.113.9" || secure {
		t.Fatalf("got %s secure=%v; a client must not be able to choose its own address or HTTPS", ip, secure)
	}
}

func TestTrustedProxyChainIsWalkedFromTheRight(t *testing.T) {
	p, err := parseTrustedProxies("172.18.0.0/16, 10.0.0.5")
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name, remote, xff, want string
	}{
		{"one proxy", "172.18.0.2:1", "198.51.100.7", "198.51.100.7"},
		{"client-written entry is ignored", "172.18.0.2:1", "6.6.6.6, 198.51.100.7", "198.51.100.7"},
		{"two trusted hops", "172.18.0.2:1", "198.51.100.7, 10.0.0.5", "198.51.100.7"},
		{"untrusted peer", "203.0.113.9:1", "198.51.100.7", "203.0.113.9"},
		{"no header", "172.18.0.2:1", "", "172.18.0.2"},
		{"garbage", "172.18.0.2:1", "not-an-ip", "172.18.0.2"},
	}
	for _, c := range cases {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.RemoteAddr = c.remote
		if c.xff != "" {
			r.Header.Set("X-Forwarded-For", c.xff)
		}
		if got, _ := p.resolve(r); got != c.want {
			t.Errorf("%s: got %s, want %s", c.name, got, c.want)
		}
	}
}

func TestForwardedProtoOnlyFromATrustedProxy(t *testing.T) {
	p, _ := parseTrustedProxies("172.18.0.2")
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.RemoteAddr = "172.18.0.2:1"
	r.Header.Set("X-Forwarded-Proto", "https")
	if _, secure := p.resolve(r); !secure {
		t.Fatal("a trusted proxy's X-Forwarded-Proto should be believed")
	}
	r.TLS = &tls.ConnectionState{}
	r.RemoteAddr = "203.0.113.9:1"
	if _, secure := p.resolve(r); !secure {
		t.Fatal("a direct TLS connection is secure")
	}
}

func TestParseTrustedProxiesRejectsNonsense(t *testing.T) {
	for _, bad := range []string{"nope", "10.0.0.0/99", "1.2.3"} {
		if _, err := parseTrustedProxies(bad); err == nil {
			t.Errorf("%q should be refused", bad)
		}
	}
	if p, err := parseTrustedProxies(""); err != nil || len(p.prefixes) != 0 {
		t.Fatal("an empty list trusts nobody")
	}
}

// The attack the release review reproduced: rotating X-Forwarded-For used to
// give unlimited password guesses.
func TestSpoofedForwardedForCannotDodgeTheSignInLimit(t *testing.T) {
	_, handler, _ := newTestApp(t)
	c := &client{t: t, handler: handler}
	c.setup("rick", "a-long-enough-password")

	var codes []int
	for i := 0; i < 8; i++ {
		req := httptest.NewRequest(http.MethodPost, "/api/login",
			strings.NewReader(`{"username":"rick","password":"wrong-password-here"}`))
		req.Header.Set("X-Forwarded-For", "198.51.100."+string(rune('1'+i)))
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		codes = append(codes, rec.Code)
	}
	if codes[len(codes)-1] != http.StatusTooManyRequests {
		t.Fatalf("a rotating X-Forwarded-For must not reset the limit, got %v", codes)
	}
}

func TestAccountLimitSlowsGuessesFromManyAddresses(t *testing.T) {
	l := newAccountLimiter()
	for i := 0; i < 20; i++ {
		l.fail(accountKey)
	}
	if ok, _ := l.allow(accountKey); !ok {
		t.Fatal("twenty failures should still be allowed")
	}
	l.fail(accountKey)
	ok, wait := l.allow(accountKey)
	if ok || wait > 30*time.Second {
		t.Fatalf("the 21st failure should slow sign-in briefly, got ok=%v wait=%v", ok, wait)
	}
}

func TestSignInBodyIsCapped(t *testing.T) {
	_, handler, _ := newTestApp(t)
	c := &client{t: t, handler: handler}
	c.setup("rick", "a-long-enough-password")
	big := `{"username":"rick","password":"` + strings.Repeat("x", 8<<10) + `"}`
	req := httptest.NewRequest(http.MethodPost, "/api/login", strings.NewReader(big))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("an oversized sign-in body should be refused, got %d", rec.Code)
	}
}
