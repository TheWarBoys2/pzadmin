package compose

import (
	"reflect"
	"testing"
)

func mustParse(t *testing.T, src string) *Node {
	t.Helper()
	n, err := Parse([]byte(src))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return n
}

func TestParseNestedMappings(t *testing.T) {
	n := mustParse(t, `
services:
  pz:
    image: renegade/zomboid:1.2.3
    container_name: westpoint-zomboid-server
    restart: unless-stopped
`)
	svc := n.Get("services").Get("pz")
	if svc == nil {
		t.Fatal("services.pz missing")
	}
	if got := svc.Get("image").Str(); got != "renegade/zomboid:1.2.3" {
		t.Fatalf("image = %q", got)
	}
	if got := svc.Get("container_name").Str(); got != "westpoint-zomboid-server" {
		t.Fatalf("container_name = %q", got)
	}
}

func TestParseKeyOrderIsPreserved(t *testing.T) {
	n := mustParse(t, "a: 1\nb: 2\nc: 3\n")
	if !reflect.DeepEqual(n.Keys, []string{"a", "b", "c"}) {
		t.Fatalf("keys = %v", n.Keys)
	}
}

func TestParseSequenceOfScalars(t *testing.T) {
	n := mustParse(t, `
volumes:
  - /host/a:/container/a
  - /host/b:/container/b:ro
`)
	got := n.Get("volumes").Strs()
	want := []string{"/host/a:/container/a", "/host/b:/container/b:ro"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("volumes = %v, want %v", got, want)
	}
}

func TestParseSequenceOfMappings(t *testing.T) {
	n := mustParse(t, `
volumes:
  - type: bind
    source: /host/a
    target: /data
  - type: volume
    source: named
    target: /var
`)
	items := n.Get("volumes").Items
	if len(items) != 2 {
		t.Fatalf("got %d items", len(items))
	}
	if got := items[0].Get("source").Str(); got != "/host/a" {
		t.Fatalf("source = %q", got)
	}
	if got := items[1].Get("type").Str(); got != "volume" {
		t.Fatalf("type = %q", got)
	}
}

// A dash followed by several spaces still lines its continuation up with the
// first key, not with the dash.
func TestParseWideSequenceIndent(t *testing.T) {
	n := mustParse(t, `
list:
  -   name: a
      value: b
`)
	item := n.Get("list").Items[0]
	if got := item.Get("value").Str(); got != "b" {
		t.Fatalf("value = %q (item %+v)", got, item)
	}
}

func TestParseQuotedValues(t *testing.T) {
	n := mustParse(t, `
ports:
  - "27015:16261/udp"
  - '8080:80'
name: "a value with: a colon"
hash: "not#a#comment"
`)
	if got := n.Get("ports").Strs(); !reflect.DeepEqual(got, []string{"27015:16261/udp", "8080:80"}) {
		t.Fatalf("ports = %v", got)
	}
	if got := n.Get("name").Str(); got != "a value with: a colon" {
		t.Fatalf("name = %q", got)
	}
	if got := n.Get("hash").Str(); got != "not#a#comment" {
		t.Fatalf("hash = %q", got)
	}
}

func TestParseStripsComments(t *testing.T) {
	n := mustParse(t, `
# leading comment
image: alpine   # trailing comment
url: http://example.com/#fragment
`)
	if got := n.Get("image").Str(); got != "alpine" {
		t.Fatalf("image = %q", got)
	}
	if got := n.Get("url").Str(); got != "http://example.com/#fragment" {
		t.Fatalf("url = %q", got)
	}
}

func TestParseEmptyValue(t *testing.T) {
	n := mustParse(t, "environment:\n  TZ:\n  PUID: 1000\n")
	env := n.Get("environment")
	if env.Get("TZ").Str() != "" {
		t.Fatal("TZ should be empty")
	}
	if got := env.Get("PUID").Str(); got != "1000" {
		t.Fatalf("PUID = %q", got)
	}
}

func TestParseFlowSequence(t *testing.T) {
	n := mustParse(t, `include: [compose/a.yml, "compose/b.yml"]`)
	if got := n.Get("include").Strs(); !reflect.DeepEqual(got, []string{"compose/a.yml", "compose/b.yml"}) {
		t.Fatalf("include = %v", got)
	}
}

func TestParseFlowMapping(t *testing.T) {
	n := mustParse(t, `logging: {driver: json-file, options: {max-size: 10m}}`)
	if got := n.Get("logging").Get("driver").Str(); got != "json-file" {
		t.Fatalf("driver = %q", got)
	}
}

func TestParseBlockScalar(t *testing.T) {
	n := mustParse(t, `
command: |
  line one
  line two
after: yes
`)
	if got := n.Get("command").Str(); got != "line one\nline two" {
		t.Fatalf("command = %q", got)
	}
	if got := n.Get("after").Str(); got != "yes" {
		t.Fatalf("after = %q", got)
	}
}

func TestParseStrsAcceptsALoneScalar(t *testing.T) {
	n := mustParse(t, "env_file: ch.env\n")
	if got := n.Get("env_file").Strs(); !reflect.DeepEqual(got, []string{"ch.env"}) {
		t.Fatalf("env_file = %v", got)
	}
}

// Anything the parser cannot represent honestly must be an error, never a
// guess: a silently misread compose file would send PZAdmin editing the wrong
// server.
func TestParseRefusesWhatItCannotRead(t *testing.T) {
	cases := map[string]string{
		"anchor":        "x: &anchor\n  a: 1\n",
		"alias":         "x: *anchor\n",
		"merge key":     "x:\n  <<: *base\n  a: 1\n",
		"tab indent":    "services:\n\tpz:\n\t  image: a\n",
		"two documents": "a: 1\n---\nb: 2\n",
		"mixed level":   "a: 1\n- item\n",
	}
	for name, src := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse([]byte(src)); err == nil {
				t.Fatalf("expected an error for %s", name)
			}
		})
	}
}

func TestParseEmptyDocument(t *testing.T) {
	n := mustParse(t, "\n# just a comment\n\n")
	if n.Kind != KindMapping || len(n.Keys) != 0 {
		t.Fatalf("expected an empty mapping, got %+v", n)
	}
}

func TestParseRealisticComposeFile(t *testing.T) {
	n := mustParse(t, `
# The Zomboid stack.
services:
  westpoint:
    image: docker.io/renegademaster/zomboid-dedicated-server:1.7.0
    container_name: westpoint-zomboid-server
    restart: unless-stopped
    env_file:
      - ch.env
    ports:
      - "16261:16261/udp"
      - "27015:27015/tcp"
    volumes:
      - ./westpoint/projectzomboid:/home/steam/ZomboidDedicatedServer   # data
    deploy:
      resources:
        limits:
          memory: 12G

  riverside2:
    image: docker.io/renegademaster/zomboid-dedicated-server:1.7.0
    container_name: riverside-zomboid-server
    env_file: [k2.env]
    ports:
      - "16271:16261/udp"
    volumes:
      - ./riverside2/projectzomboid:/home/steam/ZomboidDedicatedServer
`)
	services := n.Get("services")
	if len(services.Keys) != 2 {
		t.Fatalf("expected two services, got %v", services.Keys)
	}
	ch := services.Get("westpoint")
	if got := ch.Get("env_file").Strs(); !reflect.DeepEqual(got, []string{"ch.env"}) {
		t.Fatalf("env_file = %v", got)
	}
	if got := ch.Get("deploy").Get("resources").Get("limits").Get("memory").Str(); got != "12G" {
		t.Fatalf("memory = %q", got)
	}
	k2 := services.Get("riverside2")
	if got := k2.Get("env_file").Strs(); !reflect.DeepEqual(got, []string{"k2.env"}) {
		t.Fatalf("flow env_file = %v", got)
	}
}
