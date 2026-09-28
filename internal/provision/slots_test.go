package provision

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/TheWarBoys2/pzadmin/internal/stacks"
)

func TestNewServersTakeTheirGamePortsFromEnv(t *testing.T) {
	h := newHost(t)
	env := h.env()
	// The slot range covers 16265 to 16268, which would be next free.
	env.Slots = [2]int{16265, 16268}
	plan, err := Make(h.fresh("pz-fresh"), env)
	if err != nil {
		t.Fatal(err)
	}
	if plan.GamePort != 16269 || plan.UDPPort != 16270 {
		t.Fatalf("a new server's own ports must stay out of the slots, got %d/%d", plan.GamePort, plan.UDPPort)
	}
	if !strings.Contains(plan.Compose, GamePortMapping) || !strings.Contains(plan.Compose, UDPPortMapping) {
		t.Fatalf("compose:\n%s", plan.Compose)
	}
	if _, err := Apply(plan); err != nil {
		t.Fatal(err)
	}
	st := stacks.Inspect(plan.StackDir, filepath.Join(plan.StackDir, "docker-compose.yml"), nil)
	if st.GamePort != 16269 || st.UDPPort != 16270 {
		t.Fatalf("read back %d/%d", st.GamePort, st.UDPPort)
	}
}

func TestUseEnvPortsRewritesOnlyTheGamePortLines(t *testing.T) {
	dir := t.TempDir()
	composeFile := filepath.Join(dir, "docker-compose.yml")
	original := "services:\n  pz:\n    image: x:1\n    env_file:\n      - .env\n    ports:\n" +
		"      - \"27015:27015/tcp\"   # RCON\n" +
		"      - 16301:16261/udp\n" +
		"      - '16302:16262/udp'  # second\n" +
		"    volumes:\n      - /srv/a:/project-zomboid\n"
	write(t, composeFile, original)
	write(t, filepath.Join(dir, ".env"), "SERVER_NAME=a\n")

	res, err := UseEnvPorts(dir, composeFile)
	if err != nil || !res.Changed {
		t.Fatalf("%+v %v", res, err)
	}
	b, _ := os.ReadFile(composeFile)
	want := strings.NewReplacer("16301:16261/udp", GamePortMapping, "'16302:16262/udp'  # second", UDPPortMapping+"   # second").Replace(original)
	if string(b) != want {
		t.Fatalf("got\n%s\nwant\n%s", b, want)
	}
	if kept, _ := os.ReadFile(res.Backup); string(kept) != original {
		t.Fatal("the backup must be the original file")
	}
	// The host ports players used are kept, so nothing moves yet.
	st := stacks.Inspect(dir, composeFile, nil)
	if st.GamePort != 16301 || st.UDPPort != 16302 {
		t.Fatalf("read back %d/%d", st.GamePort, st.UDPPort)
	}
	// Running it again changes nothing.
	if res, err := UseEnvPorts(dir, composeFile); err != nil || res.Changed {
		t.Fatalf("second run: %+v %v", res, err)
	}
}

func TestUseEnvPortsRefusesWhatItCannotRead(t *testing.T) {
	dir := t.TempDir()
	composeFile := filepath.Join(dir, "docker-compose.yml")
	original := "services:\n  pz:\n    image: x:1\n    ports:\n      - \"16261-16262:16261-16262/udp\"\n"
	write(t, composeFile, original)
	write(t, filepath.Join(dir, ".env"), "")
	if _, err := UseEnvPorts(dir, composeFile); err == nil || !strings.Contains(err.Error(), "by hand") {
		t.Fatalf("want a refusal, got %v", err)
	}
	if b, _ := os.ReadFile(composeFile); string(b) != original {
		t.Fatal("a refusal must change nothing")
	}
}
