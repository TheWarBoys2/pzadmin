package provision

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/TheWarBoys2/pzadmin/internal/compose"
	"github.com/TheWarBoys2/pzadmin/internal/stacks"
)

// The game port mappings as PZAdmin writes them: host and container side
// both read .env, so moving a server to other ports is a .env change and
// a recreate. The image writes the same two variables into the ini on boot.
const (
	GamePortMapping = `"${DEFAULT_PORT}:${DEFAULT_PORT}/udp"`
	UDPPortMapping  = `"${UDP_PORT}:${UDP_PORT}/udp"`
)

// SetEnvPorts writes DEFAULT_PORT and UDP_PORT into a stack's .env. It
// returns the previous file and whether anything changed.
func SetEnvPorts(stackDir string, game, udp int) (previous []byte, changed bool, err error) {
	path := filepath.Join(stackDir, ".env")
	previous, err = os.ReadFile(path)
	if err != nil {
		return nil, false, err
	}
	env := compose.ParseEnv(previous)
	want := map[string]string{"DEFAULT_PORT": strconv.Itoa(game), "UDP_PORT": strconv.Itoa(udp)}
	for k, v := range want {
		if old, _ := env.Get(k); old != v {
			env.Set(k, v)
			changed = true
		}
	}
	if !changed {
		return previous, false, nil
	}
	if err := compose.WriteFileAtomic(path, env.Render(), 0o600); err != nil {
		return nil, false, err
	}
	return previous, true, nil
}

// UseEnvPortsResult says what UseEnvPorts did.
type UseEnvPortsResult struct {
	Changed bool
	// Backup is the copy of the compose file as it was, when it changed.
	Backup string
}

// UseEnvPorts rewrites a stack's two game port mappings to read .env, and
// writes the ports it had into .env so nothing moves until a slot is picked.
// Only the two mapping lines change; comments and everything else are kept.
// It refuses, changing nothing, when it cannot tell which lines they are.
func UseEnvPorts(stackDir, composeFile string) (UseEnvPortsResult, error) {
	var res UseEnvPortsResult
	st := stacks.Inspect(stackDir, composeFile, nil)
	raw, err := os.ReadFile(composeFile)
	if err != nil {
		return res, err
	}
	env, err := compose.ReadEnvFile(filepath.Join(stackDir, ".env"))
	if err != nil {
		return res, err
	}
	// The container side is what .env says the game listens on.
	gameIn := atoiOr(env.Lookup("DEFAULT_PORT"), stacks.DefaultGamePort)
	udpIn := atoiOr(env.Lookup("UDP_PORT"), stacks.DefaultUDPPort)

	lines := strings.Split(string(raw), "\n")
	var gameLine, udpLine = -1, -1
	for i, line := range lines {
		t := strings.TrimSpace(line)
		if !strings.HasPrefix(t, "- ") {
			continue
		}
		item := strings.TrimSpace(strings.TrimPrefix(t, "- "))
		if strings.Contains(item, "${DEFAULT_PORT}") {
			gameLine = -2
			continue
		}
		if strings.Contains(item, "${UDP_PORT}") {
			udpLine = -2
			continue
		}
		container, ok := udpMappingTarget(item)
		if !ok {
			continue
		}
		switch container {
		case gameIn:
			if gameLine != -1 {
				return res, errors.New("the compose file maps the game port more than once; change it by hand")
			}
			gameLine = i
		case udpIn:
			if udpLine != -1 {
				return res, errors.New("the compose file maps the UDP port more than once; change it by hand")
			}
			udpLine = i
		}
	}
	if gameLine == -2 && udpLine == -2 {
		return res, nil
	}
	if gameLine < 0 || udpLine < 0 {
		return res, fmt.Errorf("could not find both game port mappings (%d/udp and %d/udp) under ports: in %s. "+
			"Change them by hand to %s and %s", gameIn, udpIn, filepath.Base(composeFile), GamePortMapping, UDPPortMapping)
	}

	// .env first: with the mappings reading it, it must hold the ports the
	// server has now. They are the host side, which players connect to.
	game, udp := st.GamePort, st.UDPPort
	if game == 0 {
		game = gameIn
	}
	if udp == 0 {
		udp = udpIn
	}
	if _, _, err := SetEnvPorts(stackDir, game, udp); err != nil {
		return res, err
	}

	lines[gameLine] = replaceItem(lines[gameLine], GamePortMapping)
	lines[udpLine] = replaceItem(lines[udpLine], UDPPortMapping)
	backup := composeFile + ".before-slots-" + time.Now().UTC().Format("20060102-150405")
	if err := os.WriteFile(backup, raw, 0o644); err != nil {
		return res, fmt.Errorf("could not keep a copy of the compose file: %w", err)
	}
	if err := compose.WriteFileAtomic(composeFile, []byte(strings.Join(lines, "\n")), 0o644); err != nil {
		return res, err
	}
	after := stacks.Inspect(stackDir, composeFile, nil)
	if after.GamePort != game || after.UDPPort != udp {
		_ = compose.WriteFileAtomic(composeFile, raw, 0o644)
		return res, errors.New("the rewritten compose file did not read back as expected, so it was put back as it was")
	}
	res.Changed, res.Backup = true, backup
	return res, nil
}

// udpMappingTarget reads a short-syntax port item and returns its container
// port when it is a single UDP port mapping.
func udpMappingTarget(item string) (int, bool) {
	if i := strings.Index(item, " #"); i >= 0 {
		item = item[:i]
	}
	item = strings.Trim(strings.TrimSpace(item), `"'`)
	body, proto, found := strings.Cut(item, "/")
	if !found || proto != "udp" {
		return 0, false
	}
	parts := strings.Split(body, ":")
	n, err := strconv.Atoi(parts[len(parts)-1])
	if err != nil || n <= 0 {
		return 0, false
	}
	return n, true
}

// replaceItem swaps a sequence item's value, keeping its indent and comment.
func replaceItem(line, value string) string {
	dash := strings.Index(line, "- ")
	comment := ""
	if i := strings.Index(line, " #"); i > dash {
		comment = "   " + strings.TrimSpace(line[i:])
	}
	return line[:dash+2] + value + comment
}

func atoiOr(v string, def int) int {
	if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && n > 0 {
		return n
	}
	return def
}
