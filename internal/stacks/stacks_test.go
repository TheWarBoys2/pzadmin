package stacks

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type tree struct {
	root, stacks, data string
}

func newTree(t *testing.T) tree {
	t.Helper()
	root := t.TempDir()
	tr := tree{root: root, stacks: filepath.Join(root, "home/rick/docker/pzserver"), data: filepath.Join(root, "srv/zomboid")}
	must(t, os.MkdirAll(tr.stacks, 0o755))
	must(t, os.MkdirAll(tr.data, 0o755))
	return tr
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func write(t *testing.T, path, content string) {
	t.Helper()
	must(t, os.MkdirAll(filepath.Dir(path), 0o755))
	must(t, os.WriteFile(path, []byte(content), 0o644))
}

// addServer lays out one server the way the real host does.
func (tr tree) addServer(t *testing.T, name string, withINI bool) string {
	t.Helper()
	dir := filepath.Join(tr.stacks, name)
	data := filepath.Join(tr.data, name, "projectzomboid", "data")
	cfg := filepath.Join(tr.data, name, "projectzomboid", "config")
	write(t, filepath.Join(data, "start-server.sh"), "#!/bin/sh\n")
	write(t, filepath.Join(cfg, "Logs", "x.txt"), "")
	write(t, filepath.Join(dir, ".env"), "SERVER_NAME="+name+"\nRCON_PORT=27015\nRCON_PASSWORD=envpass\nDEFAULT_PORT=16261\nUDP_PORT=16262\n")
	write(t, filepath.Join(dir, "Server", name+"_SandboxVars.lua"), "SandboxVars = {}\n")
	if withINI {
		write(t, filepath.Join(dir, "Server", name+".ini"), "RCONPort=27015\nRCONPassword=envpass\n")
	}
	write(t, filepath.Join(dir, "docker-compose.yml"), `services:
  pz:
    image: indifferentbroccoli/projectzomboid-server-docker:1.1.9
    container_name: pz-`+name+`
    restart: unless-stopped
    stop_grace_period: 120s
    env_file:
      - .env
    ports:
      - "16301:16261/udp"
      - "16302:16262/udp"
      - "27101:27015/tcp"
    volumes:
      - `+data+`:/project-zomboid
      - `+cfg+`:/project-zomboid-config
      - `+filepath.Join(dir, "Server")+`:/project-zomboid-config/Server
`)
	return dir
}

func TestScanReadsEverythingFromTheFiles(t *testing.T) {
	tr := newTree(t)
	tr.addServer(t, "coalfield", true)
	res := Scan(tr.stacks, UnderAny(tr.stacks, tr.data))
	if len(res.Stacks) != 1 {
		t.Fatalf("stacks: %+v warnings %v", res.Stacks, res.Warnings)
	}
	s := res.Stacks[0]
	if !s.Ready() {
		t.Fatalf("problems: %v", s.Problems)
	}
	if s.Name != "coalfield" || s.Container != "pz-coalfield" || s.ServerName != "coalfield" {
		t.Fatalf("identity: %+v", s)
	}
	if s.RCONPort != 27101 || s.RCONContainerPort != 27015 || s.RCONPassword != "envpass" {
		t.Fatalf("rcon: %d/%d %q", s.RCONPort, s.RCONContainerPort, s.RCONPassword)
	}
	if s.GamePort != 16301 || s.UDPPort != 16302 {
		t.Fatalf("game ports: %d %d", s.GamePort, s.UDPPort)
	}
	if !strings.HasSuffix(s.ServerDir, "pzserver/coalfield/Server") || !strings.HasSuffix(s.DataDir, "projectzomboid/data") {
		t.Fatalf("dirs: %s %s", s.ServerDir, s.DataDir)
	}
	if !s.Installed || !s.Configured {
		t.Fatalf("installed=%v configured=%v", s.Installed, s.Configured)
	}
	if len(s.Warnings) != 0 {
		t.Fatalf("a clean stack should have no warnings: %v", s.Warnings)
	}
}

// The ini reflects the running container; .env reflects the next recreate.
func TestINIWinsForRCONAndDriftIsReported(t *testing.T) {
	tr := newTree(t)
	dir := tr.addServer(t, "knight", true)
	write(t, filepath.Join(dir, ".env"), "SERVER_NAME=knight\nRCON_PORT=27015\nRCON_PASSWORD=newpass\n")
	write(t, filepath.Join(dir, "Server", "knight.ini"), "RCONPort=27015\nRCONPassword=oldpass\n")
	s := Scan(tr.stacks, UnderAny(tr.stacks, tr.data)).Stacks[0]
	if s.RCONPassword != "oldpass" {
		t.Fatalf("want the ini's password, got %q", s.RCONPassword)
	}
	if !containsText(s.Warnings, "recreate") {
		t.Fatalf("drift not reported: %v", s.Warnings)
	}
}

func TestNotYetBootedServerUsesEnv(t *testing.T) {
	tr := newTree(t)
	tr.addServer(t, "fresh", false)
	s := Scan(tr.stacks, UnderAny(tr.stacks, tr.data)).Stacks[0]
	if s.Configured || s.RCONPassword != "envpass" || !s.Ready() {
		t.Fatalf("%+v", s)
	}
}

// The failure this whole check exists for.
func TestMissingOrEmptyBindSourceBlocksStart(t *testing.T) {
	tr := newTree(t)
	dir := tr.addServer(t, "muldraugh", true)
	data := filepath.Join(tr.data, "muldraugh", "projectzomboid", "data")

	must(t, os.RemoveAll(data))
	s := Inspect(dir, filepath.Join(dir, "docker-compose.yml"), UnderAny(tr.stacks, tr.data))
	if s.Ready() || !containsText(s.Problems, "does not exist") {
		t.Fatalf("missing source not caught: %v", s.Problems)
	}

	must(t, os.MkdirAll(data, 0o755))
	s = Inspect(dir, filepath.Join(dir, "docker-compose.yml"), UnderAny(tr.stacks, tr.data))
	if s.Ready() || !containsText(s.Problems, "is empty") {
		t.Fatalf("empty source not caught: %v", s.Problems)
	}

	// A provisioning marker is enough to pass.
	write(t, filepath.Join(data, ".pzadmin"), "provisioned\n")
	s = Inspect(dir, filepath.Join(dir, "docker-compose.yml"), UnderAny(tr.stacks, tr.data))
	if !s.Ready() {
		t.Fatalf("marked folder should pass: %v", s.Problems)
	}
}

func TestSourceOutsideMountedFoldersIsUnverifiable(t *testing.T) {
	tr := newTree(t)
	dir := tr.addServer(t, "testing", true)
	s := Inspect(dir, filepath.Join(dir, "docker-compose.yml"), UnderAny(tr.stacks))
	if s.Ready() || !containsText(s.Problems, "outside the folders mounted") {
		t.Fatalf("%v", s.Problems)
	}
}

func TestRelativeSourceIsRefused(t *testing.T) {
	tr := newTree(t)
	dir := tr.addServer(t, "rel", true)
	b, _ := os.ReadFile(filepath.Join(dir, "docker-compose.yml"))
	fixed := strings.Replace(string(b), filepath.Join(dir, "Server")+":", "./Server:", 1)
	write(t, filepath.Join(dir, "docker-compose.yml"), fixed)
	s := Inspect(dir, filepath.Join(dir, "docker-compose.yml"), UnderAny(tr.stacks, tr.data))
	if s.Ready() || !containsText(s.Problems, "relative") {
		t.Fatalf("%v", s.Problems)
	}
}

func TestUnpublishedRCONAndMissingPasswordAreProblems(t *testing.T) {
	tr := newTree(t)
	dir := tr.addServer(t, "closed", false)
	b, _ := os.ReadFile(filepath.Join(dir, "docker-compose.yml"))
	write(t, filepath.Join(dir, "docker-compose.yml"), strings.Replace(string(b), `      - "27101:27015/tcp"`+"\n", "", 1))
	write(t, filepath.Join(dir, ".env"), "SERVER_NAME=closed\n")
	s := Inspect(dir, filepath.Join(dir, "docker-compose.yml"), UnderAny(tr.stacks, tr.data))
	if !containsText(s.Problems, "not published") || !containsText(s.Problems, "No RCON password") {
		t.Fatalf("%v", s.Problems)
	}
}

func TestRestartPolicyWarning(t *testing.T) {
	tr := newTree(t)
	dir := tr.addServer(t, "norestart", true)
	b, _ := os.ReadFile(filepath.Join(dir, "docker-compose.yml"))
	write(t, filepath.Join(dir, "docker-compose.yml"), strings.Replace(string(b), "    restart: unless-stopped\n", "", 1))
	s := Inspect(dir, filepath.Join(dir, "docker-compose.yml"), UnderAny(tr.stacks, tr.data))
	if !containsText(s.Warnings, "unless-stopped") {
		t.Fatalf("%v", s.Warnings)
	}
}

func TestFoldersWithoutComposeAndHiddenFoldersAreSkipped(t *testing.T) {
	tr := newTree(t)
	tr.addServer(t, "a", true)
	must(t, os.MkdirAll(filepath.Join(tr.stacks, "notes"), 0o755))
	write(t, filepath.Join(tr.stacks, ".trash", "docker-compose.yml"), "services: {}\n")
	res := Scan(tr.stacks, UnderAny(tr.stacks, tr.data))
	if len(res.Stacks) != 1 || res.Stacks[0].Name != "a" {
		t.Fatalf("%+v", res.Stacks)
	}
}

func TestMissingRootIsAWarningNotAPanic(t *testing.T) {
	res := Scan(filepath.Join(t.TempDir(), "nope"), nil)
	if len(res.Warnings) == 0 {
		t.Fatal("expected a warning")
	}
}

func containsText(list []string, sub string) bool {
	for _, s := range list {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

func TestImagePinning(t *testing.T) {
	for ref, want := range map[string]bool{
		"indifferentbroccoli/projectzomboid-server-docker":               false,
		"indifferentbroccoli/projectzomboid-server-docker:latest":        false,
		"indifferentbroccoli/projectzomboid-server-docker:1.1.9":         true,
		"indifferentbroccoli/projectzomboid-server-docker@sha256:abc123": true,
		"registry.local:5000/pz":                                         false,
		"registry.local:5000/pz:2":                                       true,
		"":                                                               false,
	} {
		if got := ImagePinned(ref); got != want {
			t.Errorf("%q: %v, want %v", ref, got, want)
		}
	}
}

func TestUnpinnedImageAndShortGraceWarn(t *testing.T) {
	tr := newTree(t)
	dir := tr.addServer(t, "loose", true)
	b, _ := os.ReadFile(filepath.Join(dir, "docker-compose.yml"))
	c := strings.Replace(string(b), ":1.1.9", "", 1)
	c = strings.Replace(c, "stop_grace_period: 120s", "stop_grace_period: 30s", 1)
	write(t, filepath.Join(dir, "docker-compose.yml"), c)
	s := Inspect(dir, filepath.Join(dir, "docker-compose.yml"), UnderAny(tr.stacks, tr.data))
	if s.ImagePinned || !containsText(s.Warnings, "not pinned") || !containsText(s.Warnings, "stop_grace_period is \"30s\"") {
		t.Fatalf("%v", s.Warnings)
	}
	if !s.Ready() {
		t.Fatal("warnings must not block a start")
	}
}
