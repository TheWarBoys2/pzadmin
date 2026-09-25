// Package rcon speaks the Source RCON protocol to a Project Zomboid server.
//
// Two details matter and are commonly got wrong:
//
//   - Responses longer than 4096 bytes are split across several packets. This
//     client sends a sentinel packet after every command and keeps reading until
//     the sentinel comes back, so "players" on a busy server and "showoptions"
//     return complete output instead of being silently truncated.
//   - An authentication failure is signalled by a response packet whose request
//     id is -1, not by an error. Missing that check turns a wrong password into
//     a confusing timeout.
//
// The connection is kept open between commands and re-established on failure.
package rcon

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Packet types from the Source RCON specification.
const (
	typeResponse = 0 // SERVERDATA_RESPONSE_VALUE
	typeExec     = 2 // SERVERDATA_EXECCOMMAND
	typeAuth     = 3 // SERVERDATA_AUTH
	typeAuthResp = 2 // SERVERDATA_AUTH_RESPONSE (shares the wire value with EXECCOMMAND)
)

const (
	maxPacketSize = 4 * 1024 * 1024
	minPacketSize = 10
)

// ErrAuthFailed is returned when the server rejects the RCON password.
var ErrAuthFailed = errors.New("rcon: authentication failed (check the RCON password)")

// Options configures a Client.
type Options struct {
	Host        string
	Port        int
	Password    string
	DialTimeout time.Duration
	ReadTimeout time.Duration
	// DrainTimeout bounds how long we wait for additional fragments of a
	// multi-packet response once the first fragment has arrived.
	DrainTimeout time.Duration
}

func (o *Options) applyDefaults() {
	if o.DialTimeout <= 0 {
		o.DialTimeout = 5 * time.Second
	}
	if o.ReadTimeout <= 0 {
		o.ReadTimeout = 10 * time.Second
	}
	if o.DrainTimeout <= 0 {
		o.DrainTimeout = 350 * time.Millisecond
	}
	if o.Port == 0 {
		o.Port = 27015
	}
	if o.Host == "" {
		o.Host = "127.0.0.1"
	}
}

// Client is a reusable RCON connection. It is safe for concurrent use; commands
// are serialised because the protocol has no multiplexing.
type Client struct {
	opts Options

	mu     sync.Mutex
	conn   net.Conn
	reader *bufio.Reader
	nextID int32
}

// New returns a client. No connection is made until the first command.
func New(opts Options) *Client {
	opts.applyDefaults()
	return &Client{opts: opts, nextID: 1}
}

// Addr returns the host:port this client talks to.
func (c *Client) Addr() string {
	return net.JoinHostPort(c.opts.Host, strconv.Itoa(c.opts.Port))
}

// Close drops the underlying connection.
func (c *Client) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.dropLocked()
}

func (c *Client) dropLocked() {
	if c.conn != nil {
		_ = c.conn.Close()
		c.conn = nil
		c.reader = nil
	}
}

// Exec runs a command and returns the server's complete response.
//
// A dead pooled connection is transparently replaced and the command retried
// once. Authentication failures are not retried.
func (c *Client) Exec(cmd string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	resp, err := c.execLocked(cmd)
	if err == nil {
		return resp, nil
	}
	if errors.Is(err, ErrAuthFailed) {
		return "", err
	}
	// The pooled connection was probably closed by the server between commands.
	c.dropLocked()
	return c.execLocked(cmd)
}

func (c *Client) execLocked(cmd string) (string, error) {
	if err := c.connectLocked(); err != nil {
		return "", err
	}

	id := c.nextID
	sentinel := id + 1
	c.nextID += 2
	if c.nextID > 1<<20 {
		c.nextID = 1
	}

	_ = c.conn.SetDeadline(time.Now().Add(c.opts.ReadTimeout))
	if err := writePacket(c.conn, id, typeExec, cmd); err != nil {
		return "", fmt.Errorf("rcon: send command: %w", err)
	}
	// An empty RESPONSE_VALUE packet is echoed back after the real response.
	// Seeing it echoed tells us the multi-packet reply is complete.
	if err := writePacket(c.conn, sentinel, typeResponse, ""); err != nil {
		return "", fmt.Errorf("rcon: send sentinel: %w", err)
	}

	var body strings.Builder
	gotAny := false
	for {
		if gotAny {
			// Once we have at least one fragment, only wait briefly for more.
			// Servers that ignore the sentinel simply go quiet here.
			_ = c.conn.SetReadDeadline(time.Now().Add(c.opts.DrainTimeout))
		} else {
			_ = c.conn.SetReadDeadline(time.Now().Add(c.opts.ReadTimeout))
		}
		pid, _, chunk, err := readPacket(c.reader)
		if err != nil {
			// A response already in hand beats an error: servers legitimately
			// go quiet after the last fragment, and some hang up straight
			// after answering. Either way, return what we received.
			if gotAny && (isTimeout(err) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF)) {
				break
			}
			return "", fmt.Errorf("rcon: read response: %w", err)
		}
		if pid == sentinel {
			break
		}
		if pid == -1 {
			return "", ErrAuthFailed
		}
		body.WriteString(chunk)
		gotAny = true
	}
	_ = c.conn.SetDeadline(time.Time{})
	return strings.TrimRight(body.String(), "\x00"), nil
}

func (c *Client) connectLocked() error {
	if c.conn != nil {
		return nil
	}
	conn, err := net.DialTimeout("tcp", c.Addr(), c.opts.DialTimeout)
	if err != nil {
		return fmt.Errorf("rcon: connect %s: %w", c.Addr(), err)
	}
	if tcp, ok := conn.(*net.TCPConn); ok {
		_ = tcp.SetKeepAlive(true)
		_ = tcp.SetKeepAlivePeriod(30 * time.Second)
	}
	reader := bufio.NewReaderSize(conn, 8192)

	_ = conn.SetDeadline(time.Now().Add(c.opts.ReadTimeout))
	authID := c.nextID
	c.nextID += 2
	if err := writePacket(conn, authID, typeAuth, c.opts.Password); err != nil {
		_ = conn.Close()
		return fmt.Errorf("rcon: send auth: %w", err)
	}
	// Some servers emit an empty RESPONSE_VALUE before the auth result.
	for attempts := 0; attempts < 3; attempts++ {
		id, typ, _, err := readPacket(reader)
		if err != nil {
			_ = conn.Close()
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				// PZ closes the socket outright on a bad password.
				return ErrAuthFailed
			}
			return fmt.Errorf("rcon: read auth response: %w", err)
		}
		if id == -1 {
			_ = conn.Close()
			return ErrAuthFailed
		}
		if typ == typeAuthResp {
			_ = conn.SetDeadline(time.Time{})
			c.conn = conn
			c.reader = reader
			return nil
		}
	}
	_ = conn.Close()
	return errors.New("rcon: server did not return an authentication result")
}

func isTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

func writePacket(w io.Writer, id, typ int32, body string) error {
	size := int32(10 + len(body))
	buf := bytes.NewBuffer(make([]byte, 0, size+4))
	_ = binary.Write(buf, binary.LittleEndian, size)
	_ = binary.Write(buf, binary.LittleEndian, id)
	_ = binary.Write(buf, binary.LittleEndian, typ)
	buf.WriteString(body)
	buf.WriteByte(0)
	buf.WriteByte(0)
	_, err := w.Write(buf.Bytes())
	return err
}

func readPacket(r io.Reader) (id, typ int32, body string, err error) {
	var size int32
	if err = binary.Read(r, binary.LittleEndian, &size); err != nil {
		return 0, 0, "", err
	}
	if size < minPacketSize || size > maxPacketSize {
		return 0, 0, "", fmt.Errorf("rcon: implausible packet size %d", size)
	}
	buf := make([]byte, size)
	if _, err = io.ReadFull(r, buf); err != nil {
		return 0, 0, "", err
	}
	id = int32(binary.LittleEndian.Uint32(buf[0:4]))
	typ = int32(binary.LittleEndian.Uint32(buf[4:8]))
	body = string(bytes.TrimRight(buf[8:], "\x00"))
	return id, typ, body, nil
}

// --- response parsing -------------------------------------------------------

// Player is one connected player as reported by the "players" command.
type Player struct {
	Name string `json:"name"`
}

var playerHeader = []string{"players connected", "players connected:", "connected players"}

// ParsePlayers turns the output of "players" into names.
//
// Project Zomboid prints a header line followed by one player per line prefixed
// with a hyphen and no space ("-Rick"). Trimming only "- " leaves the hyphen
// attached to the name, which then breaks every kick and ban that follows, so
// the prefix set here is deliberately generous.
func ParsePlayers(raw string) []Player {
	var out []Player
	for _, line := range strings.Split(strings.ReplaceAll(raw, "\r\n", "\n"), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		lower := strings.ToLower(line)
		isHeader := false
		for _, h := range playerHeader {
			if strings.HasPrefix(lower, h) {
				isHeader = true
				break
			}
		}
		if isHeader || strings.Contains(lower, "no players") {
			continue
		}
		name := strings.TrimSpace(strings.TrimLeft(line, "-*• \t"))
		if name == "" {
			continue
		}
		// Defensive: some builds append connection details after a tab.
		if idx := strings.IndexAny(name, "\t"); idx > 0 {
			name = strings.TrimSpace(name[:idx])
		}
		out = append(out, Player{Name: name})
	}
	return out
}

// Quote escapes a value for inclusion in an RCON command argument.
func Quote(s string) string {
	s = strings.NewReplacer("\\", "\\\\", `"`, `\"`, "\n", " ", "\r", " ").Replace(s)
	return `"` + s + `"`
}
