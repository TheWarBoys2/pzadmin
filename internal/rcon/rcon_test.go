package rcon

import (
	"bufio"
	"bytes"
	"errors"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeServer is a minimal Source RCON server used to exercise the client.
type fakeServer struct {
	ln       net.Listener
	password string
	// respond returns the body the server should send for a command. Splitting
	// is handled by the server based on chunk.
	respond func(cmd string) string
	// chunk, when >0, splits responses into fragments of that size to emulate
	// the 4096-byte packet limit.
	chunk int
	// echoSentinel controls whether the server mirrors the sentinel packet.
	echoSentinel bool

	mu    sync.Mutex
	conns int
	// dropAfter closes the connection after N commands, to test reconnect.
	dropAfter int
}

func newFakeServer(t *testing.T, f *fakeServer) *fakeServer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f.ln = ln
	go f.serve()
	t.Cleanup(func() { _ = ln.Close() })
	return f
}

func (f *fakeServer) addr() (string, int) {
	a := f.ln.Addr().(*net.TCPAddr)
	return "127.0.0.1", a.Port
}

func (f *fakeServer) connCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.conns
}

func (f *fakeServer) serve() {
	for {
		c, err := f.ln.Accept()
		if err != nil {
			return
		}
		f.mu.Lock()
		f.conns++
		f.mu.Unlock()
		go f.handle(c)
	}
}

func (f *fakeServer) handle(c net.Conn) {
	defer c.Close()
	r := bufio.NewReader(c)

	id, typ, body, err := readPacket(r)
	if err != nil || typ != typeAuth {
		return
	}
	if body != f.password {
		// Real Project Zomboid answers a bad password with id -1.
		_ = writePacket(c, -1, typeAuthResp, "")
		return
	}
	_ = writePacket(c, id, typeAuthResp, "")

	handled := 0
	for {
		cid, ctyp, cmd, err := readPacket(r)
		if err != nil {
			return
		}
		if ctyp == typeResponse {
			if f.echoSentinel {
				_ = writePacket(c, cid, typeResponse, "")
			}
			if f.dropAfter > 0 && handled >= f.dropAfter {
				return
			}
			continue
		}
		out := f.respond(cmd)
		if f.chunk > 0 {
			for i := 0; i < len(out); i += f.chunk {
				end := i + f.chunk
				if end > len(out) {
					end = len(out)
				}
				_ = writePacket(c, cid, typeResponse, out[i:end])
			}
		} else {
			_ = writePacket(c, cid, typeResponse, out)
		}
		handled++
	}
}

func clientFor(f *fakeServer, password string) *Client {
	host, port := f.addr()
	return New(Options{
		Host: host, Port: port, Password: password,
		DialTimeout: 2 * time.Second, ReadTimeout: 2 * time.Second,
		DrainTimeout: 150 * time.Millisecond,
	})
}

func TestExecSinglePacket(t *testing.T) {
	f := newFakeServer(t, &fakeServer{
		password:     "pw",
		echoSentinel: true,
		respond:      func(string) string { return "Players connected (1):\n-Rick" },
	})
	c := clientFor(f, "pw")
	defer c.Close()

	got, err := c.Exec("players")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "-Rick") {
		t.Fatalf("unexpected response %q", got)
	}
}

// The original implementation read exactly one packet, silently truncating any
// response over ~4KB. This is the regression test for that.
func TestExecReassemblesMultiPacketResponse(t *testing.T) {
	var big strings.Builder
	big.WriteString("Players connected (400):\n")
	for i := 0; i < 400; i++ {
		big.WriteString("-Survivor")
		big.WriteString(strings.Repeat("x", 20))
		big.WriteByte('\n')
	}
	want := big.String()

	f := newFakeServer(t, &fakeServer{
		password: "pw", echoSentinel: true, chunk: 4000,
		respond: func(string) string { return want },
	})
	c := clientFor(f, "pw")
	defer c.Close()

	got, err := c.Exec("players")
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("truncated response: got %d bytes, want %d", len(got), len(want))
	}
	if n := len(ParsePlayers(got)); n != 400 {
		t.Fatalf("expected 400 players, parsed %d", n)
	}
}

// Servers that never echo the sentinel must still work, via the drain timeout.
func TestExecWorksWithoutSentinelEcho(t *testing.T) {
	f := newFakeServer(t, &fakeServer{
		password: "pw", echoSentinel: false, chunk: 4000,
		respond: func(string) string { return strings.Repeat("a", 9000) },
	})
	c := clientFor(f, "pw")
	defer c.Close()

	got, err := c.Exec("showoptions")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 9000 {
		t.Fatalf("got %d bytes, want 9000", len(got))
	}
}

func TestAuthFailureIsReportedNotTimedOut(t *testing.T) {
	f := newFakeServer(t, &fakeServer{
		password: "correct", echoSentinel: true,
		respond: func(string) string { return "" },
	})
	c := clientFor(f, "wrong")
	defer c.Close()

	start := time.Now()
	_, err := c.Exec("players")
	if !errors.Is(err, ErrAuthFailed) {
		t.Fatalf("expected ErrAuthFailed, got %v", err)
	}
	if time.Since(start) > time.Second {
		t.Fatal("auth failure should be immediate, not a timeout")
	}
}

func TestConnectionIsReusedThenReestablished(t *testing.T) {
	f := newFakeServer(t, &fakeServer{
		password: "pw", echoSentinel: true, dropAfter: 2,
		respond: func(string) string { return "ok" },
	})
	c := clientFor(f, "pw")
	defer c.Close()

	for i := 0; i < 2; i++ {
		if _, err := c.Exec("save"); err != nil {
			t.Fatalf("command %d: %v", i, err)
		}
	}
	if got := f.connCount(); got != 1 {
		t.Fatalf("expected the connection to be pooled, saw %d connections", got)
	}
	// The server hung up; the client must reconnect rather than fail.
	if _, err := c.Exec("save"); err != nil {
		t.Fatalf("expected transparent reconnect, got %v", err)
	}
	if got := f.connCount(); got != 2 {
		t.Fatalf("expected exactly one reconnect, saw %d connections", got)
	}
}

func TestParsePlayers(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []string
	}{
		{"hyphen no space", "Players connected (2):\n-Rick\n-Steve", []string{"Rick", "Steve"}},
		{"hyphen with space", "Players connected (1):\n- Rick", []string{"Rick"}},
		{"empty server", "Players connected (0):", nil},
		{"no players wording", "No players connected", nil},
		{"bare names", "Rick\nSteve", []string{"Rick", "Steve"}},
		{"crlf", "Players connected (1):\r\n-Rick\r\n", []string{"Rick"}},
		{"trailing detail", "Players connected (1):\n-Rick\t127.0.0.1", []string{"Rick"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ParsePlayers(tc.in)
			if len(got) != len(tc.want) {
				t.Fatalf("got %#v, want %#v", got, tc.want)
			}
			for i := range got {
				if got[i].Name != tc.want[i] {
					t.Fatalf("player %d = %q, want %q", i, got[i].Name, tc.want[i])
				}
			}
		})
	}
}

func TestQuoteEscapes(t *testing.T) {
	if got := Quote(`say "hi"`); got != `"say \"hi\""` {
		t.Fatalf("got %s", got)
	}
	if got := Quote("line\nbreak"); strings.Contains(got, "\n") {
		t.Fatalf("newlines must not survive quoting: %q", got)
	}
}

func TestPacketRoundTrip(t *testing.T) {
	var b bytes.Buffer
	if err := writePacket(&b, 42, typeExec, "players"); err != nil {
		t.Fatal(err)
	}
	id, typ, body, err := readPacket(&b)
	if err != nil {
		t.Fatal(err)
	}
	if id != 42 || typ != typeExec || body != "players" {
		t.Fatalf("got %d %d %q", id, typ, body)
	}
}

func TestReadPacketRejectsImplausibleSize(t *testing.T) {
	// Length prefix of 3 is below the protocol minimum.
	if _, _, _, err := readPacket(bytes.NewReader([]byte{3, 0, 0, 0})); err == nil {
		t.Fatal("expected an error for an undersized packet")
	}
}

func TestOfflineServerReturnsPromptError(t *testing.T) {
	c := New(Options{Host: "127.0.0.1", Port: 1, Password: "x", DialTimeout: 500 * time.Millisecond})
	if _, err := c.Exec("players"); err == nil {
		t.Fatal("expected a connection error")
	}
}
