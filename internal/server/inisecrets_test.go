package server

import (
	"strings"
	"testing"
)

func TestINIPasswordsAreHiddenAndKept(t *testing.T) {
	disk := "PublicName=Knox\r\nPassword=joinme\r\nRCONPassword=secret\r\n# Password=not-a-setting\r\nRCONPort=27015\r\n"

	shown := maskINISecrets(disk)
	for _, leak := range []string{"joinme", "secret"} {
		if strings.Contains(shown, leak) {
			t.Fatalf("the editor was sent a password: %q", shown)
		}
	}
	if !strings.Contains(shown, "# Password=not-a-setting\r\n") || !strings.Contains(shown, "PublicName=Knox\r\n") {
		t.Fatalf("other lines should be untouched: %q", shown)
	}

	// Saved untouched: the real values come back.
	if got := unmaskINISecrets(shown, disk); got != disk {
		t.Fatalf("an untouched file should round-trip, got %q", got)
	}

	// The operator types a new join password: that one is written as typed.
	edited := strings.Replace(shown, "Password="+hiddenValue, "Password=newpass", 1)
	got := unmaskINISecrets(edited, disk)
	if !strings.Contains(got, "\r\nPassword=newpass\r\n") || !strings.Contains(got, "RCONPassword=secret") {
		t.Fatalf("a changed password is kept as typed and the other restored: %q", got)
	}
}

func TestEmptyPasswordIsShownAsEmpty(t *testing.T) {
	if got := maskINISecrets("Password=\n"); got != "Password=\n" {
		t.Fatalf("an empty password is not a secret: %q", got)
	}
}
