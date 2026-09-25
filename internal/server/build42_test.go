package server

import (
	"fmt"
	"strings"
	"testing"

	"github.com/TheWarBoys2/pzadmin/internal/config"
)

// These pin the exact lines PZAdmin sends for the commands whose Build 42
// form differs from Build 41, or which were wrong before. The unit tests
// elsewhere prove a builder does what it was written to do; these prove it
// was written to do the right thing.
func TestBuild42CommandCompatibility(t *testing.T) {
	cases := []struct {
		id   string
		args []string
		want string
	}{
		// Acting on another player takes the *player commands.
		{"godmodeplayer", []string{"Bob", "true"}, `godmodeplayer "Bob" -true`},
		{"godmodeplayer", []string{"Bob", "false"}, `godmodeplayer "Bob" -false`},
		{"invisibleplayer", []string{"Bob", "true"}, `invisibleplayer "Bob" -true`},
		{"invisibleplayer", []string{"Bob", "false"}, `invisibleplayer "Bob" -false`},
		{"teleportplayer", []string{"Alice", "Bob"}, `teleportplayer "Alice" "Bob"`},

		// releasesafehouse names the safehouse, quoted like any argument.
		{"releasesafehouse", []string{"My Base"}, `releasesafehouse "My Base"`},
		{"releasesafehouse", []string{`Bob's "Fort"`}, `releasesafehouse "Bob's \"Fort\""`},

		{"setaccesslevel", []string{"Bob", "admin"}, `setaccesslevel "Bob" "admin"`},
		{"setaccesslevel", []string{"Bob", "user"}, `setaccesslevel "Bob" "user"`},

		{"banuser", []string{"Bob", "reason", "true"}, `banuser "Bob" -ip -r "reason"`},
		{"banuser", []string{"Bob", "reason", "false"}, `banuser "Bob" -r "reason"`},
		{"banuser", []string{"Bob", "reason"}, `banuser "Bob" -r "reason"`},
		{"banuser", []string{"Bob", "", "true"}, `banuser "Bob" -ip`},
		{"banip", []string{"203.0.113.7"}, `banip "203.0.113.7"`},
		{"unbanip", []string{" 203.0.113.7 "}, `unbanip "203.0.113.7"`},

		// createhorde2 takes named options; positional numbers only print
		// its usage.
		{"createhorde2", []string{"10500", "9600", "5", "30"}, "createhorde2 -x 10500 -y 9600 -z 0 -count 30 -radius 5"},
		{"createhorde2", []string{"10500", "9600", "5", "30", "1"}, "createhorde2 -x 10500 -y 9600 -z 1 -count 30 -radius 5"},
	}
	for _, tc := range cases {
		cmd, found := LookupCommand(tc.id)
		if !found {
			t.Fatalf("%s is missing from the catalogue", tc.id)
		}
		got, err := cmd.Build(tc.args)
		if err != nil {
			t.Fatalf("%s(%q): %v", tc.id, tc.args, err)
		}
		if got != tc.want {
			t.Errorf("%s(%q)\n got %s\nwant %s", tc.id, tc.args, got, tc.want)
		}
	}
}

func TestSetAccessLevelRefusesSomethingThatIsNotALevel(t *testing.T) {
	cmd, _ := LookupCommand("setaccesslevel")
	for _, bad := range []string{`admin" -x "y`, "two words", ""} {
		if got, err := cmd.Build([]string{"Bob", bad}); err == nil {
			t.Errorf("level %q should be refused, built %q", bad, got)
		}
	}
}

// Nothing in the offered catalogue may be a command that is redundant, does
// nothing over RCON, or targets the caller. RCON has no player, so a command
// that acts on "me" acts on nobody.
func TestStaleCommandsAreNotOffered(t *testing.T) {
	stale := map[string]string{
		"grantadmin":  "use setaccesslevel",
		"removeadmin": "use setaccesslevel",
		"sendpulse":   "streams to an in-game admin client, not to RCON",
		"teleportto":  "moves the caller, and RCON has none",
		"alarm":       "needs an admin standing in a building",
		"godmode":     "the self form; use godmodeplayer",
		"invisible":   "the self form; use invisibleplayer",
		"teleport":    "the self form; use teleportplayer",
	}
	for _, c := range Commands() {
		if why, bad := stale[c.ID]; bad {
			t.Errorf("%s is offered: %s", c.ID, why)
		}
		if c.legacy {
			t.Errorf("%s is legacy and must not be offered", c.ID)
		}
		if c.build == nil {
			t.Errorf("%s has no builder", c.ID)
		}
	}
}

// Schedules saved by earlier versions name commands by their old IDs. They
// must still resolve, to the corrected command.
func TestOldCommandIDsStillResolve(t *testing.T) {
	for old, want := range map[string]string{
		"godmode": `godmodeplayer "Rick"`, "invisible": `invisibleplayer "Rick"`,
		"grantadmin": `grantadmin "Rick"`, "removeadmin": `removeadmin "Rick"`,
	} {
		cmd, found := LookupCommand(old)
		if !found {
			t.Fatalf("%s no longer resolves", old)
		}
		if got, _ := cmd.Build([]string{"Rick"}); got != want {
			t.Errorf("%s built %q, want %q", old, got, want)
		}
	}
	cmd, _ := LookupCommand("teleport")
	if got, _ := cmd.Build([]string{"Alice", "Bob"}); got != `teleportplayer "Alice" "Bob"` {
		t.Errorf("teleport built %q", got)
	}
}

func TestAddUserPasswordIsMaskedForTheLog(t *testing.T) {
	cmd, _ := LookupCommand("adduser")
	got := cmd.AuditLine([]string{"newbie", "hunter2"})
	if strings.Contains(got, "hunter2") || !strings.Contains(got, `"newbie"`) {
		t.Fatalf("audit line %q", got)
	}
	kick, _ := LookupCommand("kickuser")
	if kick.AuditLine([]string{"Rick", "afk"}) != "" {
		t.Fatal("a command with no secret has nothing to mask")
	}
}

// --- capability detection ------------------------------------------------------

// fakeHelp answers "help" with a listing and "help <verb>" per verb.
type fakeHelp struct {
	listing string
	single  map[string]string
	calls   []string
}

func (f *fakeHelp) Exec(cmd string) (string, error) {
	f.calls = append(f.calls, cmd)
	if cmd == "help" {
		return f.listing, nil
	}
	verb := strings.TrimPrefix(cmd, "help ")
	if out, ok := f.single[verb]; ok {
		return out, nil
	}
	return "Unknown command /" + verb, nil
}

func listing(verbs ...string) string {
	var b strings.Builder
	b.WriteString("List of server commands :\n")
	for _, v := range verbs {
		fmt.Fprintf(&b, "* %s : Help for %s. Use: /%s\n", v, v, v)
	}
	return b.String()
}

var build41Verbs = []string{
	"additem", "adduser", "addusertowhitelist", "addvehicle", "addxp", "banid", "banuser", "changeoption",
	"checkModsNeedUpdate", "chopper", "createhorde", "createhorde2", "godmod", "godmode", "grantadmin",
	"gunshot", "help", "invisible", "kickuser", "lightning", "log", "noclip", "players", "quit",
	"releasesafehouse", "reloadlua", "reloadoptions", "removeadmin", "removeuserfromwhitelist", "save",
	"sendpulse", "servermsg", "setaccesslevel", "showoptions", "startrain", "startstorm", "stats",
	"stoprain", "stopweather", "teleport", "teleportto", "thunder", "unbanid", "unbanuser", "voiceban",
}

var build42Verbs = append(append([]string{}, build41Verbs...),
	"godmodeplayer", "invisibleplayer", "teleportplayer", "banip", "unbanip")

func TestCapabilitiesOnABuild42Server(t *testing.T) {
	f := &fakeHelp{listing: listing(build42Verbs...)}
	caps, err := checkCapabilities(f, commandList)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"godmodeplayer", "invisibleplayer", "teleportplayer", "banip", "createhorde2", "save"} {
		if cc := caps.state(id); cc.State != CapVerified || cc.Verb != LookupMust(t, id).verbs()[0] {
			t.Errorf("%s: %+v", id, cc)
		}
	}
	if caps.Listed != len(build42Verbs) {
		t.Errorf("listed %d", caps.Listed)
	}
	// Everything is in the listing, so no single-command probes are needed.
	if len(f.calls) != 1 {
		t.Errorf("calls %v", f.calls)
	}
}

func TestCapabilitiesFallBackToTheBuild41Spelling(t *testing.T) {
	f := &fakeHelp{listing: listing(build41Verbs...)}
	caps, err := checkCapabilities(f, commandList)
	if err != nil {
		t.Fatal(err)
	}
	if cc := caps.state("godmodeplayer"); cc.State != CapFallback || cc.Verb != "godmode" {
		t.Fatalf("godmodeplayer on Build 41: %+v", cc)
	}
	if cc := caps.state("teleportplayer"); cc.State != CapFallback || cc.Verb != "teleport" {
		t.Fatalf("teleportplayer on Build 41: %+v", cc)
	}
	if cc := caps.state("banip"); cc.State != CapMissing {
		t.Fatalf("banip on Build 41: %+v", cc)
	}

	app, _, _ := newTestApp(t)
	app.setCapabilities("s1", caps)
	cmd, _ := LookupCommand("godmodeplayer")
	line, _ := cmd.Build([]string{"Bob", "true"})
	got, err := app.resolveVerb(configServer("s1"), cmd, line)
	if err != nil || got != `godmode "Bob" -true` {
		t.Fatalf("resolved %q, %v", got, err)
	}
	ban, _ := LookupCommand("banip")
	line, _ = ban.Build([]string{"203.0.113.7"})
	if _, err := app.resolveVerb(configServer("s1"), ban, line); err == nil {
		t.Fatal("a command the server confirmed it lacks must be refused")
	}
	// With no check on record the line goes as built.
	got, err = app.resolveVerb(configServer("unchecked"), cmd, `godmodeplayer "Bob"`)
	if err != nil || got != `godmodeplayer "Bob"` {
		t.Fatalf("unchecked: %q %v", got, err)
	}
}

// A listing cut short (a busy server pausing between packets) must not hide
// commands: anything missing is asked about one by one first.
func TestCapabilitiesConfirmMissingCommandsOneByOne(t *testing.T) {
	short := build42Verbs[:len(build42Verbs)-12]
	f := &fakeHelp{listing: listing(short...), single: map[string]string{
		"voiceban":        "Block voice from user. Use: /voiceban \"username\" -value",
		"teleportplayer":  "Teleport a player to another.",
		"invisibleplayer": "Make a player invisible.",
	}}
	caps, err := checkCapabilities(f, commandList)
	if err != nil {
		t.Fatal(err)
	}
	if cc := caps.state("voiceban"); cc.State != CapVerified || !strings.Contains(cc.Help, "voiceban") {
		t.Fatalf("voiceban: %+v", cc)
	}
	if cc := caps.state("teleportplayer"); cc.State != CapVerified {
		t.Fatalf("teleportplayer: %+v", cc)
	}
	if cc := caps.state("banip"); cc.State != CapMissing {
		t.Fatalf("banip: %+v", cc)
	}
}

func TestCapabilitiesRefuseAListingTheyCannotRead(t *testing.T) {
	f := &fakeHelp{listing: "Players connected (0):"}
	if _, err := checkCapabilities(f, commandList); err == nil {
		t.Fatal("an unrecognised listing must not be taken as 'the server has nothing'")
	}
}

func TestAccessLevelsAreReadFromTheServersHelp(t *testing.T) {
	got := parseAccessLevels(`Set the access level of a player. Current levels: Admin, Moderator, Overseer, GM, Observer. Use /setaccesslevel "username" "accesslevel"`)
	if strings.Join(got, ",") != "admin,moderator,overseer,gm,observer" {
		t.Fatalf("%v", got)
	}
	if parseAccessLevels("Set the access level of a player.") != nil {
		t.Fatal("no list, no levels")
	}
}

func TestWithVerbOnlyReplacesTheWholeLeadingWord(t *testing.T) {
	if got := withVerb(`teleportplayer "A" "B"`, "teleportplayer", "teleport"); got != `teleport "A" "B"` {
		t.Fatal(got)
	}
	if got := withVerb(`godmodeplayerx "A"`, "godmodeplayer", "godmode"); got != `godmodeplayerx "A"` {
		t.Fatal(got)
	}
}

func LookupMust(t *testing.T, id string) Command {
	t.Helper()
	c, ok := LookupCommand(id)
	if !ok {
		t.Fatalf("no %s", id)
	}
	return c
}

func configServer(id string) config.Server { return config.Server{ID: id, Name: id} }
