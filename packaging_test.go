package main

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// docker-compose.yml is the file people copy from the README and run as is:
// no .env, no clone, no build. docker-compose.dev.yml only swaps the image for
// a local build.
const composeFile = "docker-compose.yml"

// Copy, edit, docker compose up -d: nothing may need a .env to interpolate.
func TestComposeNeedsNoEnvFile(t *testing.T) {
	compose := readOrSkip(t, composeFile)
	if strings.Contains(compose, "${") || strings.Contains(compose, "env_file") {
		t.Error("docker-compose.yml must work without a .env file")
	}
	if !strings.Contains(compose, "image: ghcr.io/thewarboys2/pzadmin:latest") {
		t.Error("docker-compose.yml should run the published image")
	}
	// Updating is a choice the operator makes with docker compose pull, not
	// something that happens on every up -d.
	if strings.Contains(compose, "pull_policy: always") {
		t.Error("docker-compose.yml should not pull on every start")
	}
	if strings.Contains(compose, "build:") {
		t.Error("docker-compose.yml runs the published image; building belongs in docker-compose.dev.yml")
	}
	if dev := readOrSkip(t, "docker-compose.dev.yml"); !strings.Contains(dev, "build: .") || !strings.Contains(dev, "image: pzadmin:local") {
		t.Error("docker-compose.dev.yml builds a local image")
	}
}

// The README carries the same file, so copying from either gives the same thing.
func TestReadmeShowsTheComposeFile(t *testing.T) {
	compose := readOrSkip(t, composeFile)
	readme := readOrSkip(t, "README.md")
	if !strings.Contains(readme, "```yaml\n"+compose+"```") {
		t.Error("README.md must contain docker-compose.yml verbatim in a yaml block")
	}
}

// No socket, no proxy, no agent: Arcane does the container work.
func TestComposeHasNoDockerSocket(t *testing.T) {
	compose := readOrSkip(t, composeFile)
	for _, banned := range []string{"docker.sock", "docker-proxy", "DOCKER_HOST", "PZADMIN_DOCKER_SOCKET", "spool"} {
		if strings.Contains(compose, banned) {
			t.Errorf("compose file still mentions %s", banned)
		}
	}
	if n := strings.Count(compose, "\n  pzadmin:\n"); n != 1 {
		t.Error("expected exactly one service, pzadmin")
	}
}

// Paths in the servers' compose files are opened as written, which only works
// if each managed folder is mounted at the same path inside as outside.
func TestManagedFoldersAreMountedAtTheSamePath(t *testing.T) {
	compose := readOrSkip(t, composeFile)
	for _, v := range []string{"PZADMIN_DATA_ROOT", "PZADMIN_STACKS_ROOT"} {
		m := regexp.MustCompile(v + `: (/\S+)`).FindStringSubmatch(compose)
		if m == nil {
			t.Errorf("%s is not set to an absolute path", v)
			continue
		}
		if !strings.Contains(compose, "- "+m[1]+":"+m[1]+"\n") {
			t.Errorf("%s (%s) is not mounted at the same path inside the container", v, m[1])
		}
	}
	if !strings.Contains(compose, "host.docker.internal:host-gateway") {
		t.Error("RCON on the host is unreachable without the host-gateway mapping")
	}
}

// Backups are plain files in a folder next to the compose file, so they can
// be copied off the machine and survive removing PZAdmin's volume.
func TestBackupsAreBindMounted(t *testing.T) {
	compose := readOrSkip(t, composeFile)
	for _, want := range []string{"- ./backups:/backups", "PZADMIN_BACKUP_DIR: /backups", "mkdir backups"} {
		if !strings.Contains(compose, want) {
			t.Errorf("compose file is missing %q", want)
		}
	}
}

func TestComposeKeepsTheSecuritySettings(t *testing.T) {
	compose := readOrSkip(t, composeFile)
	for _, want := range []string{
		"read_only: true", "no-new-privileges:true", "cap_drop: [ALL]", `user: "`, "pzadmin-data:/data",
	} {
		if !strings.Contains(compose, want) {
			t.Errorf("compose file is missing %q", want)
		}
	}
	for _, banned := range []string{"privileged:", "cap_add:", "network_mode: host", "pid: host"} {
		if strings.Contains(compose, banned) {
			t.Errorf("compose file contains %q", banned)
		}
	}
}

// The version comes from the build, never from a number typed into a file,
// which goes stale after a release or two. A plain go build says "dev".
func TestVersionIsNotHardCoded(t *testing.T) {
	src := readOrSkip(t, "internal/server/server.go")
	if !strings.Contains(src, `var Version = "dev"`) {
		t.Error(`server.go should default to Version = "dev" and have the real one set by -ldflags`)
	}
	dockerfile := readOrSkip(t, "Dockerfile")
	if !strings.Contains(dockerfile, "ARG VERSION=dev") ||
		!strings.Contains(dockerfile, "-X github.com/TheWarBoys2/pzadmin/internal/server.Version=${VERSION}") {
		t.Error("the Dockerfile must pass VERSION into the binary, defaulting to dev")
	}
	release := regexp.MustCompile(`\b\d+\.\d+\.\d+\b`)
	for _, line := range strings.Split(readOrSkip(t, composeFile), "\n") {
		if strings.Contains(line, "VERSION") || strings.Contains(line, "image:") {
			if !strings.HasPrefix(strings.TrimSpace(line), "#") && release.MatchString(line) {
				t.Errorf("compose file hard-codes a version: %s", strings.TrimSpace(line))
			}
		}
	}
}

// Nothing that could hold a secret may enter the build context.
func TestDockerignoreIsAnAllowlist(t *testing.T) {
	ignore := readOrSkip(t, ".dockerignore")
	lines := strings.Split(ignore, "\n")
	first := ""
	for _, l := range lines {
		if l = strings.TrimSpace(l); l != "" && !strings.HasPrefix(l, "#") {
			first = l
			break
		}
	}
	if first != "*" {
		t.Fatalf(".dockerignore must start by excluding everything, starts with %q", first)
	}
	for _, l := range lines {
		l = strings.TrimSpace(l)
		if l == "!.env" || l == "!data" || l == "!data/" || l == "!backups/" || l == "!.git" {
			t.Errorf(".dockerignore lets %s into the build context", l[1:])
		}
	}
}

func readOrSkip(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Skipf("%s not present in this build context", path)
	}
	return string(b)
}
