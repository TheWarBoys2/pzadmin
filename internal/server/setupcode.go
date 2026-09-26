package server

import (
	"crypto/rand"
	"crypto/subtle"
	"strings"
)

// setupAlphabet leaves out letters and digits that are easy to misread when
// copied from a terminal: 0/O, 1/I/L, 5/S, 8/B.
const setupAlphabet = "ACDEFGHJKMNPQRTUVWXYZ234679"

// newSetupCode returns a code such as "K7QM-4TXC-WN9R": 12 characters from a
// 27-letter alphabet, about 57 bits, which the login limiter makes far too
// many to guess.
func newSetupCode() string {
	buf := make([]byte, 12)
	if _, err := rand.Read(buf); err != nil {
		panic("crypto/rand failed: " + err.Error())
	}
	var b strings.Builder
	for i, v := range buf {
		if i > 0 && i%4 == 0 {
			b.WriteByte('-')
		}
		// 256 is not a multiple of 27, so this is very slightly biased toward
		// the first letters. At 57 bits that costs nothing that matters.
		b.WriteByte(setupAlphabet[int(v)%len(setupAlphabet)])
	}
	return b.String()
}

// normaliseSetupCode lets people type the code in lower case, or with
// spaces instead of dashes, or with no separators at all.
func normaliseSetupCode(s string) string {
	s = strings.ToUpper(s)
	return strings.Map(func(r rune) rune {
		if r == '-' || r == ' ' || r == '\t' {
			return -1
		}
		return r
	}, s)
}

// pendingSetupCode returns the current setup code, or "" once setup is done.
func (a *App) pendingSetupCode() string {
	a.setupMu.Lock()
	defer a.setupMu.Unlock()
	return a.setupCode
}

// setupCodeOK reports whether given matches the current setup code.
func (a *App) setupCodeOK(given string) bool {
	want := a.pendingSetupCode()
	if want == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(normaliseSetupCode(given)), []byte(normaliseSetupCode(want))) == 1
}

// clearSetupCode retires the code once the account exists.
func (a *App) clearSetupCode() {
	a.setupMu.Lock()
	a.setupCode = ""
	a.setupMu.Unlock()
}
