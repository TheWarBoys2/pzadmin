package discord

import (
	"bufio"
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeGateway speaks just enough of Discord's gateway to test Identify.
// reply is sent after the identify arrives.
func fakeGateway(t *testing.T, reply string, gotToken *string) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !isWebSocketUpgrade(r) {
			http.Error(w, "not a websocket", http.StatusBadRequest)
			return
		}
		sum := sha1.Sum([]byte(r.Header.Get("Sec-WebSocket-Key") + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
		conn, rw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.Close()
		_, _ = rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n" +
			"Sec-WebSocket-Accept: " + base64.StdEncoding.EncodeToString(sum[:]) + "\r\n\r\n")
		serverSend(rw.Writer, `{"op":10,"d":{"heartbeat_interval":41250}}`)
		_ = rw.Flush()

		msg := serverRead(t, rw.Reader)
		var identify struct {
			Op int `json:"op"`
			D  struct {
				Token string `json:"token"`
			} `json:"d"`
		}
		if err := json.Unmarshal(msg, &identify); err != nil || identify.Op != 2 {
			t.Errorf("expected an identify, got %s", msg)
			return
		}
		*gotToken = identify.D.Token
		// Split the reply over two frames to exercise reassembly.
		half := len(reply) / 2
		serverFrame(rw.Writer, false, 0x1, []byte(reply[:half]))
		serverFrame(rw.Writer, true, 0x0, []byte(reply[half:]))
		_ = rw.Flush()
		_, _ = io.Copy(io.Discard, rw) // until the client closes
	}))
	t.Cleanup(srv.Close)
	return srv
}

func serverSend(w *bufio.Writer, text string) { serverFrame(w, true, 0x1, []byte(text)) }

// serverFrame writes an unmasked frame, as a server does.
func serverFrame(w *bufio.Writer, fin bool, op byte, payload []byte) {
	b0 := op
	if fin {
		b0 |= 0x80
	}
	_ = w.WriteByte(b0)
	switch n := len(payload); {
	case n < 126:
		_ = w.WriteByte(byte(n))
	default:
		_ = w.WriteByte(126)
		var ext [2]byte
		binary.BigEndian.PutUint16(ext[:], uint16(n))
		_, _ = w.Write(ext[:])
	}
	_, _ = w.Write(payload)
}

func serverRead(t *testing.T, r *bufio.Reader) []byte {
	ws := &wsConn{r: r}
	_, _, payload, err := ws.frame()
	if err != nil {
		t.Fatal(err)
	}
	return payload
}

func TestIdentify(t *testing.T) {
	var got string
	gw := fakeGateway(t, `{"op":0,"t":"READY","s":1,"d":{"user":{"username":"Knox Radio"}}}`, &got)
	c := New("bot-token")
	c.Gateway = "ws" + strings.TrimPrefix(gw.URL, "http") + "/?v=10&encoding=json"
	if err := c.Identify(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got != "bot-token" {
		t.Fatalf("identified with %q", got)
	}
}

func TestIdentifyRejected(t *testing.T) {
	var got string
	gw := fakeGateway(t, `{"op":9,"d":false}`, &got)
	c := New("wrong")
	c.Gateway = "ws" + strings.TrimPrefix(gw.URL, "http")
	if err := c.Identify(context.Background()); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("expected the token to be refused, got %v", err)
	}
}

func TestTextChannelsAndRename(t *testing.T) {
	var renamed map[string]string
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bot tok" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch r.Method + " " + r.URL.Path {
		case "GET /users/@me":
			_, _ = io.WriteString(w, `{"id":"1","username":"Knox Radio"}`)
		case "GET /users/@me/guilds":
			_, _ = io.WriteString(w, `[{"id":"100000000000000001","name":"Zomboid Crew"}]`)
		case "GET /guilds/100000000000000001/channels":
			_, _ = io.WriteString(w, `[
			  {"id":"9","name":"Servers","type":4,"position":1},
			  {"id":"11","name":"riverside","type":0,"parent_id":"9","position":2},
			  {"id":"10","name":"louisville","type":0,"parent_id":"9","position":1},
			  {"id":"12","name":"general","type":0,"position":0},
			  {"id":"13","name":"Voice","type":2,"position":3}]`)
		case "PATCH /channels/11":
			_ = json.NewDecoder(r.Body).Decode(&renamed)
			_, _ = io.WriteString(w, `{}`)
		case "PATCH /channels/12":
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = io.WriteString(w, `{"retry_after": 312.5}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer api.Close()
	c := New("tok")
	c.API = api.URL

	if me, err := c.Me(context.Background()); err != nil || me.Username != "Knox Radio" {
		t.Fatalf("me: %#v %v", me, err)
	}
	chans, err := c.TextChannels(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, ch := range chans {
		names = append(names, ch.Category+"/"+ch.Name)
	}
	if got := strings.Join(names, ","); got != "/general,Servers/louisville,Servers/riverside" {
		t.Fatalf("channels in the wrong order or unfiltered: %s", got)
	}
	if chans[0].Guild != "Zomboid Crew" {
		t.Fatalf("guild name missing: %#v", chans[0])
	}

	if err := c.RenameChannel(context.Background(), "11", "🟢-riverside"); err != nil || renamed["name"] != "🟢-riverside" {
		t.Fatalf("rename: %v %v", renamed, err)
	}
	var limited *RateLimited
	if err := c.RenameChannel(context.Background(), "12", "x"); !errors.As(err, &limited) || limited.RetryAfter.Seconds() != 312.5 {
		t.Fatalf("expected Discord's wait to be read, got %v", err)
	}
	bad := New("bad")
	bad.API = api.URL
	if _, err := bad.Me(context.Background()); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("expected unauthorised, got %v", err)
	}
}

func TestWebhookChannel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.RawQuery != "" {
			t.Errorf("the query should be dropped: %s", r.URL.RawQuery)
		}
		_, _ = io.WriteString(w, `{"id":"5","channel_id":"123456789012345678"}`)
	}))
	defer srv.Close()
	id, err := WebhookChannel(context.Background(), srv.Client(), srv.URL+"/api/webhooks/5/tok?thread_id=1")
	if err != nil || id != "123456789012345678" {
		t.Fatalf("got %q %v", id, err)
	}
}

func TestDots(t *testing.T) {
	for in, want := range map[string]string{
		"riverside":    "riverside",
		"🟢-riverside":  "riverside",
		"🔴-riverside":  "riverside",
		"🟢┃riverside":  "riverside",
		"🟡 riverside":  "riverside",
		"river-🟢-side": "river-🟢-side",
		"🔴🔴-riverside": "🔴-riverside",
	} {
		if got := BaseName(in); got != want {
			t.Errorf("BaseName(%q) = %q, want %q", in, got, want)
		}
	}
	if got := WithDot(DotOnline, "🔴-riverside"); got != "🟢-riverside" {
		t.Fatalf("got %q", got)
	}
	if !IsID("123456789012345678") || IsID("abc") || IsID("12") {
		t.Fatal("IsID")
	}
}
