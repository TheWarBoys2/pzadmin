package discord

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha1"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Identify connects to Discord's gateway as the bot once and disconnects.
//
// Discord will not let a bot post a message until it has done this at least
// once. A bot someone already runs has, but a bot made just for PZAdmin has
// not, and PZAdmin never otherwise holds a gateway connection. Doing it
// once when the bot is connected in the settings saves pulling in a
// WebSocket library for a single handshake.
func (c *Client) Identify(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	ws, err := dialWebSocket(ctx, c.Gateway)
	if err != nil {
		return fmt.Errorf("could not reach Discord's gateway: %w", err)
	}
	defer ws.close()
	if deadline, ok := ctx.Deadline(); ok {
		_ = ws.conn.SetDeadline(deadline)
	}

	identified := false
	for {
		msg, err := ws.read()
		if err != nil {
			return fmt.Errorf("Discord's gateway closed the connection: %w", err)
		}
		var ev struct {
			Op int             `json:"op"`
			T  string          `json:"t"`
			D  json.RawMessage `json:"d"`
		}
		if err := json.Unmarshal(msg, &ev); err != nil {
			return fmt.Errorf("unexpected message from Discord's gateway: %w", err)
		}
		switch {
		case ev.Op == 10 && !identified: // Hello
			identified = true
			identify := map[string]any{"op": 2, "d": map[string]any{
				"token": c.Token, "intents": 0,
				"properties": map[string]string{"os": "linux", "browser": "pzadmin", "device": "pzadmin"},
			}}
			b, _ := json.Marshal(identify)
			if err := ws.write(b); err != nil {
				return err
			}
		case ev.Op == 0 && ev.T == "READY":
			return nil
		case ev.Op == 9: // Invalid session
			return ErrUnauthorized
		}
	}
}

// --- the smallest WebSocket client that works --------------------------------

type wsConn struct {
	conn net.Conn
	r    *bufio.Reader
}

var errWSClosed = errors.New("closed")

func dialWebSocket(ctx context.Context, raw string) (*wsConn, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, err
	}
	host := u.Host
	var conn net.Conn
	d := &net.Dialer{}
	switch u.Scheme {
	case "wss":
		if u.Port() == "" {
			host += ":443"
		}
		conn, err = (&tls.Dialer{NetDialer: d, Config: &tls.Config{ServerName: u.Hostname()}}).DialContext(ctx, "tcp", host)
	case "ws":
		if u.Port() == "" {
			host += ":80"
		}
		conn, err = d.DialContext(ctx, "tcp", host)
	default:
		return nil, fmt.Errorf("not a WebSocket address: %s", raw)
	}
	if err != nil {
		return nil, err
	}
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}

	nonce := make([]byte, 16)
	_, _ = rand.Read(nonce)
	key := base64.StdEncoding.EncodeToString(nonce)
	path := u.RequestURI()
	req := "GET " + path + " HTTP/1.1\r\nHost: " + u.Host + "\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n" +
		"Sec-WebSocket-Key: " + key + "\r\nSec-WebSocket-Version: 13\r\nUser-Agent: PZAdmin\r\n\r\n"
	if _, err := io.WriteString(conn, req); err != nil {
		conn.Close()
		return nil, err
	}
	r := bufio.NewReader(conn)
	resp, err := http.ReadResponse(r, &http.Request{Method: http.MethodGet})
	if err != nil {
		conn.Close()
		return nil, err
	}
	resp.Body.Close()
	sum := sha1.Sum([]byte(key + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
	if resp.StatusCode != http.StatusSwitchingProtocols ||
		resp.Header.Get("Sec-WebSocket-Accept") != base64.StdEncoding.EncodeToString(sum[:]) {
		conn.Close()
		return nil, fmt.Errorf("the gateway refused the connection (%s)", resp.Status)
	}
	return &wsConn{conn: conn, r: r}, nil
}

// read returns the next complete text or binary message, answering pings.
func (w *wsConn) read() ([]byte, error) {
	var msg []byte
	for {
		fin, op, payload, err := w.frame()
		if err != nil {
			return nil, err
		}
		switch op {
		case 0x8:
			return nil, errWSClosed
		case 0x9:
			if err := w.send(0xA, payload); err != nil {
				return nil, err
			}
			continue
		case 0xA:
			continue
		}
		msg = append(msg, payload...)
		if len(msg) > 8<<20 {
			return nil, errors.New("message too large")
		}
		if fin {
			return msg, nil
		}
	}
}

func (w *wsConn) frame() (fin bool, op byte, payload []byte, err error) {
	var head [2]byte
	if _, err = io.ReadFull(w.r, head[:]); err != nil {
		return
	}
	fin = head[0]&0x80 != 0
	op = head[0] & 0x0F
	masked := head[1]&0x80 != 0
	n := uint64(head[1] & 0x7F)
	switch n {
	case 126:
		var ext [2]byte
		if _, err = io.ReadFull(w.r, ext[:]); err != nil {
			return
		}
		n = uint64(binary.BigEndian.Uint16(ext[:]))
	case 127:
		var ext [8]byte
		if _, err = io.ReadFull(w.r, ext[:]); err != nil {
			return
		}
		n = binary.BigEndian.Uint64(ext[:])
	}
	if n > 8<<20 {
		err = errors.New("frame too large")
		return
	}
	var mask [4]byte
	if masked {
		if _, err = io.ReadFull(w.r, mask[:]); err != nil {
			return
		}
	}
	payload = make([]byte, n)
	if _, err = io.ReadFull(w.r, payload); err != nil {
		return
	}
	if masked {
		for i := range payload {
			payload[i] ^= mask[i%4]
		}
	}
	return
}

func (w *wsConn) write(text []byte) error { return w.send(0x1, text) }

// send writes one frame. A client must mask everything it sends.
func (w *wsConn) send(op byte, payload []byte) error {
	var buf []byte
	buf = append(buf, 0x80|op)
	switch n := len(payload); {
	case n < 126:
		buf = append(buf, 0x80|byte(n))
	case n <= 0xFFFF:
		buf = append(buf, 0x80|126, byte(n>>8), byte(n))
	default:
		buf = append(buf, 0x80|127)
		buf = binary.BigEndian.AppendUint64(buf, uint64(n))
	}
	var mask [4]byte
	_, _ = rand.Read(mask[:])
	buf = append(buf, mask[:]...)
	for i, b := range payload {
		buf = append(buf, b^mask[i%4])
	}
	_, err := w.conn.Write(buf)
	return err
}

func (w *wsConn) close() {
	_ = w.send(0x8, []byte{0x03, 0xE8}) // 1000, normal closure
	_ = w.conn.Close()
}

// isWebSocketUpgrade is used by tests standing in for the gateway.
func isWebSocketUpgrade(r *http.Request) bool {
	return strings.EqualFold(r.Header.Get("Upgrade"), "websocket")
}
