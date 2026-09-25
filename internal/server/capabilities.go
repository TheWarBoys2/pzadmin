package server

import (
	"fmt"
	"log"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/TheWarBoys2/pzadmin/internal/config"
	"github.com/TheWarBoys2/pzadmin/internal/store"
)

// The command catalogue is a static list, and Project Zomboid's commands move
// between builds. The server itself is the authority: its help lists every
// command it has. Capabilities records what one server's help said, so the UI
// can hide what that server does not have and PZAdmin can send the spelling
// that server uses.

// Command states, per catalogue entry.
const (
	// CapVerified: the server lists the preferred command word.
	CapVerified = "verified"
	// CapFallback: the server lists only an older spelling, which is sent.
	CapFallback = "fallback"
	// CapMissing: the server does not have the command.
	CapMissing = "missing"
	// CapUnchecked: no check has been run against this server.
	CapUnchecked = "unchecked"
)

// CommandCapability is what one server's help says about one catalogue entry.
type CommandCapability struct {
	State string `json:"state"`
	// Verb is the command word PZAdmin will send.
	Verb string `json:"verb,omitempty"`
	// Help is the server's own description, which is often the best
	// reference there is for the exact syntax of that build.
	Help string `json:"help,omitempty"`
}

// Capabilities is the result of checking one server.
type Capabilities struct {
	CheckedAt OptionalTime `json:"checkedAt"`
	// Listed is how many commands the server's help listed.
	Listed int `json:"listed"`
	// AccessLevels are the levels setaccesslevel's help names, if it names any.
	AccessLevels []string                     `json:"accessLevels,omitempty"`
	Commands     map[string]CommandCapability `json:"commands"`
	// Unknown are commands the server has that the catalogue does not, so an
	// operator can see what is available through the console.
	Unknown []string `json:"unknown,omitempty"`
}

// state returns the capability of one catalogue command, or unchecked.
func (c *Capabilities) state(id string) CommandCapability {
	if c == nil {
		return CommandCapability{State: CapUnchecked}
	}
	if cc, ok := c.Commands[id]; ok {
		return cc
	}
	return CommandCapability{State: CapUnchecked}
}

// helpLine matches one entry of the help listing: "* godmode : Make a ...".
var helpLine = regexp.MustCompile(`^\s*[*\-]\s*([A-Za-z][A-Za-z0-9_]*)\s*:\s*(.*)$`)

// minListed is the fewest entries a real help listing has. Anything shorter
// is not a listing PZAdmin understands, and hiding commands on the strength
// of it would be worse than hiding nothing.
const minListed = 10

// parseHelp reads the help listing into lower-cased command word -> text.
func parseHelp(out string) map[string]string {
	cmds := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		m := helpLine.FindStringSubmatch(strings.TrimRight(line, "\r"))
		if m == nil {
			continue
		}
		cmds[strings.ToLower(m[1])] = strings.TrimSpace(m[2])
	}
	return cmds
}

// levelsText finds the list of levels in setaccesslevel's help, which reads
// "Current levels: Admin, Moderator, Overseer, GM, Observer." on the builds
// that say.
var levelsText = regexp.MustCompile(`(?i)levels?\s*(?:are|:)\s*([A-Za-z][A-Za-z0-9_\-]*(?:\s*(?:,|or|and)\s*[A-Za-z][A-Za-z0-9_\-]*)+)`)

func parseAccessLevels(help string) []string {
	m := levelsText.FindStringSubmatch(help)
	if m == nil {
		return nil
	}
	var out []string
	seen := map[string]bool{}
	for _, part := range regexp.MustCompile(`\s*(?:,|\bor\b|\band\b)\s*`).Split(m[1], -1) {
		p := strings.ToLower(strings.TrimSpace(part))
		if p != "" && accessLevelPattern.MatchString(p) && !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	return out
}

// probeAbsent reports whether "help <verb>" says the command does not exist.
// Builds answer an unknown name with "Unknown command"; a build that ignores
// the argument and prints the whole list instead is read as that list.
func probeAbsent(verb, out string) (absent bool, help string) {
	out = strings.TrimSpace(out)
	if out == "" {
		return true, ""
	}
	if list := parseHelp(out); len(list) >= minListed {
		text, ok := list[strings.ToLower(verb)]
		return !ok, text
	}
	lower := strings.ToLower(out)
	if strings.Contains(lower, "unknown command") || strings.Contains(lower, "no such command") ||
		strings.Contains(lower, "command not found") {
		return true, ""
	}
	return false, out
}

// execer is the one RCON call the check needs, so tests can script it.
type execer interface {
	Exec(cmd string) (string, error)
}

// checkCapabilities asks a server for its commands and matches the catalogue
// against them.
//
// The listing arrives over RCON as several packets, and a very busy server
// could leave a gap long enough for the tail to be cut off. So a catalogue
// command missing from the listing is not taken as missing until
// "help <verb>" has confirmed it for each of its spellings.
func checkCapabilities(rc execer, catalogue []Command) (*Capabilities, error) {
	out, err := rc.Exec("help")
	if err != nil {
		return nil, err
	}
	listed := parseHelp(out)
	if len(listed) < minListed {
		return nil, fmt.Errorf("the server's help did not list its commands in a form PZAdmin recognises, " +
			"so nothing has been hidden")
	}

	caps := &Capabilities{
		CheckedAt: TimeNow(),
		Listed:    len(listed),
		Commands:  map[string]CommandCapability{},
	}
	known := map[string]bool{}
	probed := map[string]bool{}
	const maxProbes = 40

	for _, cmd := range catalogue {
		verbs := cmd.verbs()
		for _, v := range verbs {
			known[strings.ToLower(v)] = true
		}
		found := -1
		text := ""
		for i, v := range verbs {
			if t, ok := listed[strings.ToLower(v)]; ok {
				found, text = i, t
				break
			}
		}
		if found < 0 {
			for i, v := range verbs {
				lv := strings.ToLower(v)
				if probed[lv] || len(probed) >= maxProbes {
					continue
				}
				probed[lv] = true
				resp, err := rc.Exec("help " + v)
				if err != nil {
					return nil, err
				}
				if absent, t := probeAbsent(v, resp); !absent {
					listed[lv] = t
					found, text = i, t
					break
				}
			}
		}
		switch {
		case found == 0:
			caps.Commands[cmd.ID] = CommandCapability{State: CapVerified, Verb: verbs[0], Help: text}
		case found > 0:
			caps.Commands[cmd.ID] = CommandCapability{State: CapFallback, Verb: verbs[found], Help: text}
		default:
			caps.Commands[cmd.ID] = CommandCapability{State: CapMissing}
		}
		if cmd.ID == "setaccesslevel" {
			caps.AccessLevels = parseAccessLevels(text)
		}
	}
	for verb := range listed {
		if !known[verb] {
			caps.Unknown = append(caps.Unknown, verb)
		}
	}
	sort.Strings(caps.Unknown)
	return caps, nil
}

// --- per-server cache ---------------------------------------------------------

func (a *App) capabilitiesOf(serverID string) *Capabilities {
	a.capMu.Lock()
	defer a.capMu.Unlock()
	return a.caps[serverID]
}

func (a *App) setCapabilities(serverID string, c *Capabilities) {
	a.capMu.Lock()
	defer a.capMu.Unlock()
	if c == nil {
		delete(a.caps, serverID)
		delete(a.capTried, serverID)
		return
	}
	a.caps[serverID] = c
}

// resolveVerb applies a server's capabilities to a built command line: it
// sends the spelling that server has, and refuses a command it has confirmed
// is not there. With no check on record the line is sent as built.
func (a *App) resolveVerb(srv config.Server, cmd Command, line string) (string, error) {
	cc := a.capabilitiesOf(srv.ID).state(cmd.ID)
	switch cc.State {
	case CapMissing:
		return "", fmt.Errorf("%s does not have the %s command (checked against its own help). "+
			"Check the server's commands again after an update, or type it in the console", srv.Name, cmd.verbs()[0])
	case CapFallback:
		return withVerb(line, cmd.verbs()[0], cc.Verb), nil
	}
	return line, nil
}

// capRetry spaces out automatic checks that failed, so a server whose help
// PZAdmin cannot read is not asked on every monitor tick.
const capRetry = 30 * time.Minute

// autoCheckCapabilities checks a server that is answering, once when it comes
// up and again only if there is no result on record. It runs in the server's
// own monitor loop, whose RCON client is already serialised.
func (a *App) autoCheckCapabilities(srv config.Server, cameUp bool) {
	a.capMu.Lock()
	have := a.caps[srv.ID] != nil
	last := a.capTried[srv.ID]
	due := cameUp || (!have && time.Since(last) > capRetry)
	if due {
		a.capTried[srv.ID] = time.Now()
	}
	a.capMu.Unlock()
	if !due {
		return
	}
	caps, err := checkCapabilities(a.rconFor(srv), commandList)
	if err != nil {
		log.Printf("checking the commands of %s: %v", srv.Name, err)
		return
	}
	a.setCapabilities(srv.ID, caps)
}

func (a *App) handleCapabilities(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	if _, found := a.cfg.Server(id); !found {
		httpError(w, http.StatusNotFound, "no such server")
		return
	}
	writeJSON(w, map[string]any{"capabilities": a.capabilitiesOf(id)})
}

func (a *App) handleCapabilitiesCheck(w http.ResponseWriter, r *http.Request) {
	var p struct {
		ServerID string `json:"serverId"`
	}
	if !decodeJSON(w, r, &p) {
		return
	}
	srv, found := a.cfg.Server(p.ServerID)
	if !found {
		httpError(w, http.StatusNotFound, "no such server")
		return
	}
	caps, err := checkCapabilities(a.rconFor(srv), commandList)
	if err != nil {
		if kind := classifyRCONError(err); kind != "other" {
			httpError(w, http.StatusBadGateway, friendlyRCONError(kind, srv))
			return
		}
		httpError(w, http.StatusBadGateway, err.Error())
		return
	}
	a.setCapabilities(srv.ID, caps)
	missing := 0
	for _, c := range caps.Commands {
		if c.State == CapMissing {
			missing++
		}
	}
	a.event(store.Event{Kind: "admin.action", Severity: store.SevInfo, Source: "ui", Actor: actor(r),
		ServerID: srv.ID, Server: srv.Name, Message: "Checked the server's commands",
		Detail: fmt.Sprintf("%d listed by the server; %d catalogue commands not available", caps.Listed, missing)})
	ok(w, map[string]any{"capabilities": caps})
}
