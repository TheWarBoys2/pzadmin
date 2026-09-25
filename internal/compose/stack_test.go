package compose

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestParseEnvBasics(t *testing.T) {
	e := ParseEnv([]byte(`
# The server's identity.
SERVER_NAME=West Point
RCON_PASSWORD="a password with spaces"
ADMIN='single quoted'
PORT=16261   # inline comment
HASHY=abc#notacomment
export EXPORTED=yes
EMPTY=
`))
	cases := map[string]string{
		"SERVER_NAME":   "West Point",
		"RCON_PASSWORD": "a password with spaces",
		"ADMIN":         "single quoted",
		"PORT":          "16261",
		"HASHY":         "abc#notacomment",
		"EXPORTED":      "yes",
		"EMPTY":         "",
	}
	for k, want := range cases {
		if got := e.Lookup(k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
}

func TestEnvSetPreservesComments(t *testing.T) {
	src := "# what this does\nPORT=16261\n# another note\nNAME=old\n"
	e := ParseEnv([]byte(src))
	e.Set("NAME", "new")
	e.Set("EXTRA", "added")

	got := string(e.Render())
	if !strings.Contains(got, "# what this does") || !strings.Contains(got, "# another note") {
		t.Fatalf("comments were lost:\n%s", got)
	}
	if !strings.Contains(got, "NAME=new") {
		t.Fatalf("NAME not updated:\n%s", got)
	}
	if strings.Contains(got, "NAME=old") {
		t.Fatalf("old value still present:\n%s", got)
	}
	if !strings.Contains(got, "EXTRA=added") {
		t.Fatalf("EXTRA not appended:\n%s", got)
	}
	// Re-reading must give the same values back.
	again := ParseEnv(e.Render())
	if again.Lookup("NAME") != "new" || again.Lookup("PORT") != "16261" {
		t.Fatalf("round trip lost values: %v", again.Map())
	}
}

func TestEnvQuotesWhenNeeded(t *testing.T) {
	e := ParseEnv(nil)
	e.Set("PLAIN", "simple")
	e.Set("SPACED", "two words")
	e.Set("HASH", "a#b")
	out := string(e.Render())
	if !strings.Contains(out, "PLAIN=simple") {
		t.Fatalf("plain value should not be quoted:\n%s", out)
	}
	round := ParseEnv([]byte(out))
	if round.Lookup("SPACED") != "two words" {
		t.Fatalf("SPACED round trip = %q", round.Lookup("SPACED"))
	}
	if round.Lookup("HASH") != "a#b" {
		t.Fatalf("HASH round trip = %q", round.Lookup("HASH"))
	}
}

func TestEnvSetDropsDuplicates(t *testing.T) {
	e := ParseEnv([]byte("A=1\nB=2\nA=3\n"))
	if got := e.Lookup("A"); got != "3" {
		t.Fatalf("the last duplicate should win on read, got %q", got)
	}
	e.Set("A", "9")
	out := string(e.Render())
	if strings.Count(out, "A=") != 1 {
		t.Fatalf("expected one A line:\n%s", out)
	}
	if ParseEnv([]byte(out)).Lookup("A") != "9" {
		t.Fatalf("wrong value after set:\n%s", out)
	}
}

func TestInterpolate(t *testing.T) {
	vars := map[string]string{"PZ_ROOT": "/srv/pz", "EMPTY": ""}
	cases := []struct {
		in   string
		want string
	}{
		{"${PZ_ROOT}/a", "/srv/pz/a"},
		{"$PZ_ROOT/a", "/srv/pz/a"},
		{"${EMPTY:-fallback}", "fallback"},
		{"${EMPTY-fallback}", ""},
		{"${MISSING-fallback}", "fallback"},
		{"${PZ_ROOT:?must be set}", "/srv/pz"},
		{"$$literal", "$literal"},
		{"no vars here", "no vars here"},
	}
	for _, c := range cases {
		got, _ := interpolate(c.in, vars)
		if got != c.want {
			t.Errorf("interpolate(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// An unresolved variable must survive verbatim. Substituting an empty string
// would silently rewrite "${PZ_ROOT}/saves" into "/saves".
func TestInterpolateKeepsUnresolvedVariables(t *testing.T) {
	got, missing := interpolate("${NOPE}/saves", map[string]string{})
	if got != "${NOPE}/saves" {
		t.Fatalf("got %q", got)
	}
	if !reflect.DeepEqual(missing, []string{"NOPE"}) {
		t.Fatalf("missing = %v", missing)
	}
}

func TestParseShortMount(t *testing.T) {
	cases := []struct {
		raw    string
		source string
		target string
		mode   string
		named  bool
	}{
		{"/host/a:/data", "/host/a", "/data", "", false},
		{"./rel:/data:ro", "/base/rel", "/data", "ro", false},
		{"myvolume:/data", "myvolume", "/data", "", true},
		{"/data", "", "/data", "", true},
	}
	for _, c := range cases {
		m, ok := parseShortMount(c.raw, "/base")
		if !ok {
			t.Fatalf("%q did not parse", c.raw)
		}
		if m.Source != c.source || m.Target != c.target || m.Mode != c.mode || m.Named != c.named {
			t.Errorf("%q -> %+v, want source=%q target=%q mode=%q named=%v",
				c.raw, m, c.source, c.target, c.mode, c.named)
		}
	}
}

func TestParseShortPort(t *testing.T) {
	cases := []struct {
		raw       string
		host      string
		container string
		proto     string
		hostIP    string
	}{
		{"16261:16261/udp", "16261", "16261", "udp", ""},
		{"27015:27015", "27015", "27015", "", ""},
		{"8080", "", "8080", "", ""},
		{"127.0.0.1:8080:80", "8080", "80", "", "127.0.0.1"},
	}
	for _, c := range cases {
		p, ok := parseShortPort(c.raw)
		if !ok {
			t.Fatalf("%q did not parse", c.raw)
		}
		if p.Host != c.host || p.Container != c.container || p.Proto != c.proto || p.HostIP != c.hostIP {
			t.Errorf("%q -> %+v", c.raw, p)
		}
	}
}

func TestLoadStack(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, ".env"), "STACK_TZ=Europe/London\n")
	write(t, filepath.Join(dir, "ch.env"), `
SERVER_NAME=West Point
RCON_PASSWORD=secret
RCON_PORT=27015
SERVER_PORT=16261
MAX_PLAYERS=16
`)
	write(t, filepath.Join(dir, "docker-compose.yml"), `
services:
  westpoint:
    image: renegademaster/zomboid-dedicated-server:1.7.0
    container_name: westpoint-zomboid-server
    restart: unless-stopped
    env_file:
      - ch.env
    environment:
      TZ: ${STACK_TZ}
    ports:
      - "16261:16261/udp"
      - "27015:27015/tcp"
    volumes:
      - ./westpoint/projectzomboid:/home/steam/ZomboidDedicatedServer
  unnamed:
    image: alpine
`)

	stack, err := LoadStack(filepath.Join(dir, "docker-compose.yml"))
	if err != nil {
		t.Fatalf("LoadStack: %v", err)
	}
	if len(stack.Services) != 2 {
		t.Fatalf("got %d services", len(stack.Services))
	}
	svc := stack.Services[0]
	if svc.ContainerName != "westpoint-zomboid-server" {
		t.Fatalf("container = %q", svc.ContainerName)
	}
	if svc.Env["SERVER_NAME"] != "West Point" {
		t.Fatalf("env_file not merged: %v", svc.Env)
	}
	if svc.Env["TZ"] != "Europe/London" {
		t.Fatalf("interpolation from .env failed: %v", svc.Env)
	}
	wantSource := filepath.Join(dir, "westpoint", "projectzomboid")
	if len(svc.Volumes) != 1 || svc.Volumes[0].Source != wantSource {
		t.Fatalf("volume = %+v, want source %q", svc.Volumes, wantSource)
	}
	if len(svc.Ports) != 2 || svc.Ports[0].Host != "16261" || svc.Ports[0].Proto != "udp" {
		t.Fatalf("ports = %+v", svc.Ports)
	}
	if len(stack.Warnings) != 0 {
		t.Fatalf("unexpected warnings: %v", stack.Warnings)
	}

	// A service with no container_name gets Compose's default.
	if got := stack.Services[1].ContainerName; got != stack.Project+"-unnamed-1" {
		t.Fatalf("default container name = %q", got)
	}
}

func TestLoadStackWarnsAboutUnresolvedVariables(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "docker-compose.yml"), `
services:
  pz:
    image: alpine
    volumes:
      - ${PZ_DATA}/pz:/data
`)
	stack, err := LoadStack(filepath.Join(dir, "docker-compose.yml"))
	if err != nil {
		t.Fatalf("LoadStack: %v", err)
	}
	if len(stack.Warnings) == 0 {
		t.Fatal("expected a warning about PZ_DATA")
	}
	if !strings.Contains(stack.Warnings[0], "PZ_DATA") {
		t.Fatalf("warning does not name the variable: %q", stack.Warnings[0])
	}
	// The unresolved path must not have been silently turned into /pz.
	if got := stack.Services[0].Volumes[0].Source; !strings.Contains(got, "${PZ_DATA}") {
		t.Fatalf("source = %q, should still show the variable", got)
	}
}

func TestLoadStackRefusesUnreadableYAML(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "docker-compose.yml"), "services:\n  pz:\n    <<: *base\n")
	if _, err := LoadStack(filepath.Join(dir, "docker-compose.yml")); err == nil {
		t.Fatal("expected an error rather than a partial read")
	}
}

func TestFindComposeFilePrecedence(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "docker-compose.yml"), "services: {}\n")
	if got := FindComposeFile(dir); got != filepath.Join(dir, "docker-compose.yml") {
		t.Fatalf("got %q", got)
	}
	write(t, filepath.Join(dir, "compose.yaml"), "services: {}\n")
	if got := FindComposeFile(dir); got != filepath.Join(dir, "compose.yaml") {
		t.Fatalf("compose.yaml should win, got %q", got)
	}
}

func TestWriteFileAtomic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "file.env")
	if err := WriteFileAtomic(path, []byte("A=1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil || string(b) != "A=1\n" {
		t.Fatalf("read back %q, %v", b, err)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v", st.Mode().Perm())
	}
	// No temporary files left behind.
	entries, _ := os.ReadDir(filepath.Dir(path))
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".pzadmin-") {
			t.Fatalf("temporary file left behind: %s", e.Name())
		}
	}
}
