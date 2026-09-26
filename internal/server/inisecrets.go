package server

import (
	"path/filepath"
	"strings"

	"github.com/TheWarBoys2/pzadmin/internal/pz"
)

// The raw config editor never shows a server's passwords. On the way out
// their values are replaced with hiddenValue; on the way back in, a line that
// still says hiddenValue gets the value from the file on disk. A line the
// operator changed is written as typed, so the editor can still set a
// password.
const hiddenValue = "<hidden, unchanged>"

// secretINIKeys are the server .ini settings that hold a password or token:
// the ones the settings form hides, so the two can never disagree.
var secretINIKeys = pz.SecretKeys()

func isServerINI(name string) bool {
	return strings.EqualFold(filepath.Ext(name), ".ini")
}

// mapSecretLines calls fn for each password line and replaces its value with
// what fn returns. Line endings, comments and everything else are kept.
func mapSecretLines(text string, fn func(key, value string) string) string {
	lines := strings.SplitAfter(text, "\n")
	for i, line := range lines {
		body := strings.TrimRight(line, "\r\n")
		ending := line[len(body):]
		trimmed := strings.TrimSpace(body)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") || strings.HasPrefix(trimmed, ";") {
			continue
		}
		eq := strings.IndexByte(body, '=')
		if eq <= 0 {
			continue
		}
		key := strings.ToLower(strings.TrimSpace(body[:eq]))
		if !secretINIKeys[key] {
			continue
		}
		lines[i] = body[:eq+1] + fn(key, body[eq+1:]) + ending
	}
	return strings.Join(lines, "")
}

// maskINISecrets hides every non-empty password value.
func maskINISecrets(text string) string {
	return mapSecretLines(text, func(_, value string) string {
		if value == "" {
			return value
		}
		return hiddenValue
	})
}

// unmaskINISecrets puts the saved value back wherever the placeholder is left.
func unmaskINISecrets(edited, onDisk string) string {
	saved := iniValues(onDisk)
	return mapSecretLines(edited, func(key, value string) string {
		if value == hiddenValue {
			return saved[key]
		}
		return value
	})
}
