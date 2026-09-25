package server

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/TheWarBoys2/pzadmin/internal/config"
)

const (
	sessionCookie = "pzadmin_session"
	csrfCookie    = "pzadmin_csrf"
	csrfHeader    = "X-CSRF-Token"
	sessionTTL    = 7 * 24 * time.Hour
)

// session is one signed-in browser.
type session struct {
	// TokenHash is stored rather than the token itself, so a leaked
	// sessions.json cannot be replayed as a live login.
	TokenHash string    `json:"tokenHash"`
	CSRF      string    `json:"csrf"`
	Username  string    `json:"username"`
	Created   time.Time `json:"created"`
	Expires   time.Time `json:"expires"`
	LastSeen  time.Time `json:"lastSeen"`
	IP        string    `json:"ip"`
	Agent     string    `json:"agent"`
}

// sessionStore keeps live sessions and persists them so a container restart
// does not sign everyone out.
type sessionStore struct {
	mu   sync.RWMutex
	path string
	byID map[string]*session // keyed by token hash
}

func newSessionStore(path string) *sessionStore {
	s := &sessionStore{path: path, byID: map[string]*session{}}
	s.load()
	return s
}

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func (s *sessionStore) create(username, ip, agent string) (token string, sess *session) {
	token = config.RandomToken(32)
	sess = &session{
		TokenHash: hashToken(token),
		CSRF:      config.RandomToken(16),
		Username:  username,
		Created:   time.Now(),
		Expires:   time.Now().Add(sessionTTL),
		LastSeen:  time.Now(),
		IP:        ip,
		Agent:     truncate(agent, 120),
	}
	s.mu.Lock()
	s.byID[sess.TokenHash] = sess
	s.mu.Unlock()
	s.save()
	return token, sess
}

func (s *sessionStore) lookup(token string) (*session, bool) {
	if token == "" {
		return nil, false
	}
	h := hashToken(token)
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.byID[h]
	if !ok {
		return nil, false
	}
	if time.Now().After(sess.Expires) {
		delete(s.byID, h)
		return nil, false
	}
	// Refresh at most once a minute to avoid writing on every request.
	if time.Since(sess.LastSeen) > time.Minute {
		sess.LastSeen = time.Now()
	}
	return sess, true
}

func (s *sessionStore) revoke(token string) {
	s.mu.Lock()
	delete(s.byID, hashToken(token))
	s.mu.Unlock()
	s.save()
}

// revokeAll signs out every browser, used when the password changes.
func (s *sessionStore) revokeAll() {
	s.mu.Lock()
	s.byID = map[string]*session{}
	s.mu.Unlock()
	s.save()
}

// gc removes expired sessions. The original implementation only deleted an
// expired session if that exact token was presented again, so the map grew
// forever.
func (s *sessionStore) gc() int {
	now := time.Now()
	s.mu.Lock()
	removed := 0
	for k, v := range s.byID {
		if now.After(v.Expires) {
			delete(s.byID, k)
			removed++
		}
	}
	s.mu.Unlock()
	if removed > 0 {
		s.save()
	}
	return removed
}

// list returns active sessions for the settings screen.
func (s *sessionStore) list() []session {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]session, 0, len(s.byID))
	for _, v := range s.byID {
		cp := *v
		cp.TokenHash = cp.TokenHash[:12] // enough to identify, not to replay
		cp.CSRF = ""
		out = append(out, cp)
	}
	return out
}

func (s *sessionStore) save() {
	s.mu.RLock()
	list := make([]*session, 0, len(s.byID))
	for _, v := range s.byID {
		list = append(list, v)
	}
	s.mu.RUnlock()
	b, err := json.Marshal(list)
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return
	}
	tmp := s.path + ".tmp"
	if os.WriteFile(tmp, b, 0o600) == nil {
		_ = os.Rename(tmp, s.path)
	}
}

func (s *sessionStore) load() {
	b, err := os.ReadFile(s.path)
	if err != nil {
		return
	}
	var list []*session
	if json.Unmarshal(b, &list) != nil {
		return
	}
	now := time.Now()
	for _, v := range list {
		if v != nil && now.Before(v.Expires) {
			s.byID[v.TokenHash] = v
		}
	}
}

// --- login rate limiting ----------------------------------------------------

// loginLimiter slows down password guessing. Without it an attacker can try
// passwords as fast as the network allows.
type loginLimiter struct {
	mu       sync.Mutex
	failures map[string]*failureRecord
}

type failureRecord struct {
	count    int
	lockedTo time.Time
	last     time.Time
}

func newLoginLimiter() *loginLimiter {
	return &loginLimiter{failures: map[string]*failureRecord{}}
}

// allow reports whether an attempt may proceed, and how long to wait if not.
func (l *loginLimiter) allow(ip string) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	r, ok := l.failures[ip]
	if !ok {
		return true, 0
	}
	// Forget a quiet client after fifteen minutes.
	if time.Since(r.last) > 15*time.Minute {
		delete(l.failures, ip)
		return true, 0
	}
	if time.Now().Before(r.lockedTo) {
		return false, time.Until(r.lockedTo)
	}
	return true, 0
}

func (l *loginLimiter) fail(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	r, ok := l.failures[ip]
	if !ok {
		r = &failureRecord{}
		l.failures[ip] = r
	}
	r.count++
	r.last = time.Now()
	// The first four attempts are free; then back off 5s, 10s, 20s ... 5 min.
	if r.count > 4 {
		delay := time.Duration(1<<uint(min(r.count-5, 6))) * 5 * time.Second
		if delay > 5*time.Minute {
			delay = 5 * time.Minute
		}
		r.lockedTo = time.Now().Add(delay)
	}
	if len(l.failures) > 5000 {
		for k, v := range l.failures {
			if time.Since(v.last) > 15*time.Minute {
				delete(l.failures, k)
			}
		}
	}
}

func (l *loginLimiter) succeed(ip string) {
	l.mu.Lock()
	delete(l.failures, ip)
	l.mu.Unlock()
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// --- request helpers --------------------------------------------------------

func clientIP(r *http.Request) string {
	// Trust a forwarding header only when it is present; PZAdmin is expected to
	// sit behind the operator's own reverse proxy on a private network.
	if v := r.Header.Get("X-Forwarded-For"); v != "" {
		parts := strings.Split(v, ",")
		return strings.TrimSpace(parts[0])
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// requestIsSecure reports whether the browser reached us over TLS, so the
// session cookie can carry the Secure flag when it is meaningful and omit it on
// plain-HTTP LAN access where it would break login entirely.
func requestIsSecure(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	return strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

func setSessionCookies(w http.ResponseWriter, r *http.Request, token string, sess *session) {
	secure := requestIsSecure(r)
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: token, Path: "/",
		HttpOnly: true, Secure: secure, SameSite: http.SameSiteStrictMode,
		MaxAge: int(sessionTTL.Seconds()),
	})
	// The CSRF cookie is deliberately readable by scripts: the browser echoes it
	// back in a header, which an attacker on another origin cannot do.
	http.SetCookie(w, &http.Cookie{
		Name: csrfCookie, Value: sess.CSRF, Path: "/",
		HttpOnly: false, Secure: secure, SameSite: http.SameSiteStrictMode,
		MaxAge: int(sessionTTL.Seconds()),
	})
}

func clearSessionCookies(w http.ResponseWriter, r *http.Request) {
	secure := requestIsSecure(r)
	for _, name := range []string{sessionCookie, csrfCookie} {
		http.SetCookie(w, &http.Cookie{
			Name: name, Value: "", Path: "/", MaxAge: -1,
			HttpOnly: name == sessionCookie, Secure: secure, SameSite: http.SameSiteStrictMode,
		})
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
