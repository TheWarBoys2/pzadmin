package compose

import "testing"

// Whatever Set writes, ParseEnv must read back unchanged. A password is the
// value most likely to contain the characters that break this, and it is also
// the one where a silent corruption is hardest to notice: the server simply
// refuses the RCON login later.
func TestEnvRoundTripsAwkwardValues(t *testing.T) {
	values := map[string]string{
		"PLAIN":     "simple",
		"SPACED":    "two words",
		"QUOTED":    `a"b`,
		"BACKSLASH": `a\b`,
		"BOTH":      `he said \"no\"`,
		"HASH":      "pass#word",
		"DOLLAR":    "not$interpolated",
		"NEWLINE":   "line one\nline two",
		"EMPTY":     "",
		"SINGLE":    "it's",
		"EQUALS":    "a=b=c",
	}
	e := ParseEnv(nil)
	for k, v := range values {
		e.Set(k, v)
	}
	back := ParseEnv(e.Render())
	for k, want := range values {
		if got := back.Lookup(k); got != want {
			t.Errorf("%s round tripped to %q, want %q", k, got, want)
		}
	}
}

// Values written by hand, in the forms people actually use.
func TestEnvReadsHandWrittenValues(t *testing.T) {
	e := ParseEnv([]byte(`
A="he said \"no\""
B="a\\b"
C="trailing" ignored
D='single \" stays'
`))
	for k, want := range map[string]string{
		"A": `he said "no"`,
		"B": `a\b`,
		"C": "trailing",
		"D": `single \" stays`,
	} {
		if got := e.Lookup(k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
}
