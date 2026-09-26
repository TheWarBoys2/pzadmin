package server

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/TheWarBoys2/pzadmin/internal/config"
	"github.com/TheWarBoys2/pzadmin/internal/fsutil"
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
	// saveMu is held from snapshot to rename, so an older snapshot can never
	// be written over a newer one and bring back a signed-out session.
	saveMu sync.Mutex
	// loadErr is set when sessions.json could not be read at start.
	loadErr error
}

func newSessionStore(path string) *sessionStore {
	s := &sessionStore{path: path, byID: map[string]*session{}}
	s.loadErr = s.load()
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

// gc removes expired sessions. lookup only drops an expired session when its
// token is presented again, so without this the list would grow forever.
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
	s.saveMu.Lock()
	defer s.saveMu.Unlock()
	// Marshal under the lock: lookup updates LastSeen in place.
	s.mu.RLock()
	list := make([]*session, 0, len(s.byID))
	for _, v := range s.byID {
		list = append(list, v)
	}
	b, err := json.Marshal(list)
	s.mu.RUnlock()
	if err == nil {
		err = fsutil.WriteFile(s.path, b, 0o600)
	}
	if err != nil {
		log.Printf("saving sessions: %v", err)
	}
}

func (s *sessionStore) load() error {
	var list []*session
	if _, err := fsutil.ReadJSON(s.path, &list); err != nil {
		log.Printf("sessions: %v", err)
		return err
	}
	now := time.Now()
	for _, v := range list {
		if v != nil && now.Before(v.Expires) {
			s.byID[v.TokenHash] = v
		}
	}
	return nil
}

// --- login rate limiting ----------------------------------------------------

// loginLimiter slows down password guessing. Without it an attacker can try
// passwords as fast as the network allows.
type loginLimiter struct {
	mu       sync.Mutex
	failures map[string]*failureRecord
	// free is how many failures pass before any delay; maxDelay caps the
	// back-off.
	free     int
	maxDelay time.Duration
}

type failureRecord struct {
	count    int
	lockedTo time.Time
	last     time.Time
}

// newLoginLimiter limits one address: four free attempts, then 5s, 10s, 20s
// and so on, up to five minutes.
func newLoginLimiter() *loginLimiter {
	return &loginLimiter{failures: map[string]*failureRecord{}, free: 4, maxDelay: 5 * time.Minute}
}

// accountKey is the single key the account-wide limiter counts under.
const accountKey = "*"

// newAccountLimiter limits sign-in attempts from everywhere at once, so
// guesses spread over many addresses are slowed as well. It is generous and
// its delay is short, because anyone can trip it: it has to slow a botnet
// without locking the real administrator out for long.
func newAccountLimiter() *loginLimiter {
	return &loginLimiter{failures: map[string]*failureRecord{}, free: 20, maxDelay: 30 * time.Second}
}

// allow reports whether an attempt may proceed, and how long to wait if not.
// It counts nothing; for sign-in, where the check is slow, use take.
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

// take reports whether an attempt may go ahead and, when it may, counts it
// as a failure straight away; succeed clears it if the attempt turns out to
// be right. Counting before the slow password check, under the same lock as
// the check, means a burst of parallel guesses is limited exactly like the
// same guesses sent one after another.
func (l *loginLimiter) take(key string) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if r, ok := l.failures[key]; ok {
		if time.Since(r.last) > 15*time.Minute {
			delete(l.failures, key)
		} else if time.Now().Before(r.lockedTo) {
			return false, time.Until(r.lockedTo)
		}
	}
	l.recordLocked(key)
	return true, 0
}

func (l *loginLimiter) fail(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.recordLocked(ip)
}

// recordLocked counts one failure. l.mu must be held.
func (l *loginLimiter) recordLocked(ip string) {
	r, ok := l.failures[ip]
	if !ok {
		r = &failureRecord{}
		l.failures[ip] = r
	}
	r.count++
	r.last = time.Now()
	// The first few attempts are free; then back off 5s, 10s, 20s ... up to
	// the cap.
	if r.count > l.free {
		delay := time.Duration(1<<uint(min(r.count-l.free-1, 6))) * 5 * time.Second
		if delay > l.maxDelay {
			delay = l.maxDelay
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

// knownAddresses remembers where the administrator has signed in from, so
// the account-wide limit, which anyone can trip, never locks the real
// administrator out of an address they have used before.
type knownAddresses struct {
	mu   sync.Mutex
	seen map[string]time.Time
}

const knownAddressTTL = 30 * 24 * time.Hour

func (k *knownAddresses) add(ip string) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.seen == nil {
		k.seen = map[string]time.Time{}
	}
	k.seen[ip] = time.Now()
	if len(k.seen) > 1000 {
		for addr, at := range k.seen {
			if time.Since(at) > knownAddressTTL {
				delete(k.seen, addr)
			}
		}
	}
}

func (k *knownAddresses) has(ip string) bool {
	k.mu.Lock()
	defer k.mu.Unlock()
	at, ok := k.seen[ip]
	return ok && time.Since(at) < knownAddressTTL
}

// --- request helpers --------------------------------------------------------

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

// truncate shortens s to at most n characters. It counts characters, not
// bytes, so a Cyrillic player name or an emoji in chat is never cut in half
// and turned into invalid text.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	count := 0
	for i := range s {
		if count == n {
			return s[:i]
		}
		count++
	}
	return s
}
