// Package arcane controls containers through Arcane's REST API.
//
// PZAdmin has no Docker socket. Every container operation goes to Arcane
// (pinned to 2.13.1) with an API key whose role is limited to:
//
//	containers:list   status, and resolving a container name to its ID
//	containers:read   inspect a single container
//	containers:start  start a stopped server
//	containers:stop   hard stop that stays down
//	containers:logs   optional: the container log view
//	containers:delete optional: deleting a server from PZAdmin
//
// Restarts do not come through here at all. They are RCON save and quit, and
// the container's restart policy brings it back, so they keep working while
// Arcane is down.
//
// Endpoint paths and the auth header were taken from Arcane's exported
// OpenAPI spec, not guessed. Every operation that names a container is checked
// against an allowlist supplied by the caller, so a stolen session cannot
// reach containers that are not configured Project Zomboid servers, whatever
// the API key itself would allow.
package arcane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Errors a caller can branch on.
var (
	// ErrNotConfigured means no URL, environment or key was supplied.
	ErrNotConfigured = errors.New("arcane: not configured")
	// ErrNotAllowed means the container is not a configured server.
	ErrNotAllowed = errors.New("arcane: container is not managed by PZAdmin")
	// ErrUnavailable means Arcane could not be reached at all.
	ErrUnavailable = errors.New("arcane: unreachable")
	// ErrUnauthorized means Arcane rejected the API key itself.
	ErrUnauthorized = errors.New("arcane: API key rejected")
	// ErrForbidden means the key is valid but its role lacks a permission.
	ErrForbidden = errors.New("arcane: permission denied")
	// ErrNotFound means no container with that name exists in the environment.
	ErrNotFound = errors.New("arcane: no such container")
)

// AllowFunc reports whether PZAdmin may operate on a container.
type AllowFunc func(name string) bool

// ProjectAllowFunc reports whether PZAdmin may deploy a project.
type ProjectAllowFunc func(p Project) bool

// Options configures a client.
type Options struct {
	// URL is Arcane's base address, with or without the trailing /api.
	URL string
	// EnvironmentID selects the Docker environment. It is a setting rather
	// than a constant because environments move between hosts.
	EnvironmentID string
	// APIKey is sent as X-API-Key.
	APIKey string
	Allow  AllowFunc
	// AllowProject guards project deployment the way Allow guards containers.
	AllowProject ProjectAllowFunc
	// HTTP overrides the transport, for tests.
	HTTP *http.Client
}

// Client is a minimal Arcane container client.
type Client struct {
	base         string
	env          string
	key          string
	allow        AllowFunc
	allowProject ProjectAllowFunc
	http         *http.Client

	mu       sync.Mutex
	ids      map[string]cachedID
	lastPing time.Time
	healthy  bool
	lastErr  error
}

type cachedID struct {
	id string
	at time.Time
}

// idTTL bounds how long a name-to-ID mapping is trusted. A recreated
// container keeps its name and gets a new ID, and a stale ID fails loudly
// with 404, which triggers a fresh lookup anyway.
const idTTL = 5 * time.Minute

// Container is the subset of container metadata the UI needs.
type Container struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Image   string `json:"image"`
	State   string `json:"state"`
	Status  string `json:"status"`
	Managed bool   `json:"managed"`
}

// Details describes a single container's runtime state.
type Details struct {
	Name       string    `json:"name"`
	State      string    `json:"state"`
	Running    bool      `json:"running"`
	StartedAt  time.Time `json:"startedAt"`
	FinishedAt time.Time `json:"finishedAt"`
	ExitCode   int       `json:"exitCode"`
	Health     string    `json:"health,omitempty"`
	Image      string    `json:"image"`
	// Mounts are the container's actual mounts, which are what a start uses.
	// They only change on recreate, so they can differ from the compose file.
	Mounts []Mount `json:"mounts,omitempty"`
}

// Mount is one of a container's actual mounts.
type Mount struct {
	Type        string `json:"type"`
	Source      string `json:"source"`
	Destination string `json:"destination"`
}

// New returns a client. A client missing any of URL, environment or key is
// still usable; every call returns ErrNotConfigured.
func New(opts Options) *Client {
	base := strings.TrimRight(strings.TrimSpace(opts.URL), "/")
	if base != "" && !strings.HasSuffix(base, "/api") {
		base += "/api"
	}
	allow := opts.Allow
	if allow == nil {
		allow = func(string) bool { return false }
	}
	allowProject := opts.AllowProject
	if allowProject == nil {
		allowProject = func(Project) bool { return false }
	}
	hc := opts.HTTP
	if hc == nil {
		// No client-wide timeout: log downloads can legitimately take a while.
		// Each call sets its own deadline through its context.
		hc = &http.Client{}
	}
	return &Client{
		base:         base,
		env:          strings.TrimSpace(opts.EnvironmentID),
		key:          strings.TrimSpace(opts.APIKey),
		allow:        allow,
		allowProject: allowProject,
		http:         hc,
		ids:          map[string]cachedID{},
	}
}

// Configured reports whether URL, environment and key are all set.
func (c *Client) Configured() bool {
	return c.base != "" && c.env != "" && c.key != ""
}

// EnvironmentID returns the configured environment.
func (c *Client) EnvironmentID() string { return c.env }

// Available reports whether Arcane answered an authenticated request
// recently, refreshing at most every 15 seconds.
//
// /app-version needs no authentication, so it would say "up" with a wrong
// key. Listing one container proves the URL, the key, the environment and the
// containers:list permission in one request.
func (c *Client) Available() (bool, error) {
	if !c.Configured() {
		return false, ErrNotConfigured
	}
	c.mu.Lock()
	fresh := time.Since(c.lastPing) < 15*time.Second
	healthy, lastErr := c.healthy, c.lastErr
	c.mu.Unlock()
	if fresh {
		return healthy, lastErr
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var page containerPage
	err := c.getJSON(ctx, c.envPath("/containers")+"?limit=1", &page)

	c.mu.Lock()
	c.lastPing = time.Now()
	c.healthy = err == nil
	c.lastErr = err
	c.mu.Unlock()
	return err == nil, err
}

// List returns every container in the environment, flagging the ones PZAdmin
// may control.
func (c *Client) List(ctx context.Context) ([]Container, error) {
	summaries, err := c.listAll(ctx, "")
	if err != nil {
		return nil, err
	}
	out := make([]Container, 0, len(summaries))
	for _, s := range summaries {
		name := s.name()
		id := s.ID
		if len(id) > 12 {
			id = id[:12]
		}
		out = append(out, Container{
			ID: id, Name: name, Image: s.Image, State: s.State, Status: s.Status,
			Managed: c.allow(name),
		})
	}
	return out, nil
}

// Inspect returns runtime details for a managed container.
func (c *Client) Inspect(ctx context.Context, name string) (Details, error) {
	if !c.allow(name) {
		return Details{}, ErrNotAllowed
	}
	var raw struct {
		Data struct {
			Name   string  `json:"name"`
			Image  string  `json:"image"`
			Mounts []Mount `json:"mounts"`
			State  struct {
				Status     string `json:"status"`
				Running    bool   `json:"running"`
				StartedAt  string `json:"startedAt"`
				FinishedAt string `json:"finishedAt"`
				ExitCode   int    `json:"exitCode"`
				Health     *struct {
					Status string `json:"status"`
				} `json:"health"`
			} `json:"state"`
		} `json:"data"`
	}
	err := c.withID(ctx, name, func(id string) error {
		return c.getJSON(ctx, c.envPath("/containers/"+url.PathEscape(id)), &raw)
	})
	if err != nil {
		return Details{}, err
	}
	d := Details{
		Name:     strings.TrimPrefix(raw.Data.Name, "/"),
		State:    raw.Data.State.Status,
		Running:  raw.Data.State.Running,
		ExitCode: raw.Data.State.ExitCode,
		Image:    raw.Data.Image,
		Mounts:   raw.Data.Mounts,
	}
	if d.Name == "" {
		d.Name = name
	}
	if raw.Data.State.Health != nil {
		d.Health = raw.Data.State.Health.Status
	}
	if t, err := time.Parse(time.RFC3339Nano, raw.Data.State.StartedAt); err == nil && t.Year() > 1 {
		d.StartedAt = t
	}
	if t, err := time.Parse(time.RFC3339Nano, raw.Data.State.FinishedAt); err == nil && t.Year() > 1 {
		d.FinishedAt = t
	}
	return d, nil
}

// Start starts a managed container.
func (c *Client) Start(ctx context.Context, name string) error {
	return c.lifecycle(ctx, name, "start")
}

// Stop stops a managed container. Arcane's stop takes no timeout, so Docker
// uses the container's own stop timeout: set stop_grace_period in the server's
// compose file, long enough for Project Zomboid to write its world.
//
// A container stopped this way stays down: restart: unless-stopped does not
// bring back a container that was stopped deliberately.
func (c *Client) Stop(ctx context.Context, name string) error {
	return c.lifecycle(ctx, name, "stop")
}

// Remove deletes a managed container that is not running. Volumes are never
// removed and a running container is never forced: the caller stops it first,
// so the game has had its grace period to save.
//
// A container that does not exist is not an error: there is nothing to
// remove, which is what the caller wanted.
func (c *Client) Remove(ctx context.Context, name string) error {
	if !c.allow(name) {
		return ErrNotAllowed
	}
	err := c.withID(ctx, name, func(id string) error {
		_, err := c.do(ctx, http.MethodDelete, c.envPath("/containers/"+url.PathEscape(id))+"?force=false&volumes=false", 0)
		return err
	})
	if errors.Is(err, ErrNotFound) {
		err = nil
	}
	c.mu.Lock()
	delete(c.ids, name)
	c.mu.Unlock()
	return err
}

func (c *Client) lifecycle(ctx context.Context, name, action string) error {
	if !c.allow(name) {
		return ErrNotAllowed
	}
	return c.withID(ctx, name, func(id string) error {
		_, err := c.do(ctx, http.MethodPost, c.envPath("/containers/"+url.PathEscape(id)+"/"+action), 0)
		return err
	})
}

// Logs returns the last n lines of a managed container's log.
//
// Arcane only offers a full download with no tail parameter, so the whole log
// is fetched and trimmed here. Reading stops at logReadCap bytes so a runaway
// log cannot pin memory; past that point the tail shown is of the first
// logReadCap bytes, and the output says so.
func (c *Client) Logs(ctx context.Context, name string, lines int) (string, error) {
	if !c.allow(name) {
		return "", ErrNotAllowed
	}
	if lines <= 0 || lines > 2000 {
		lines = 300
	}
	var out string
	err := c.withID(ctx, name, func(id string) error {
		resp, err := c.send(ctx, http.MethodGet, c.envPath("/containers/"+url.PathEscape(id)+"/logs/download"))
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		data, err := io.ReadAll(io.LimitReader(resp.Body, logReadCap+1))
		if err != nil && len(data) == 0 {
			return fmt.Errorf("%w: reading the log: %v", ErrUnavailable, err)
		}
		truncated := len(data) > logReadCap
		if truncated {
			data = data[:logReadCap]
		}
		out = tailLines(data, lines)
		if truncated {
			out = "[PZAdmin: the log is larger than 64 MB; these are the last lines of its first 64 MB]\n" + out
		}
		return nil
	})
	return out, err
}

const logReadCap = 64 << 20

// --- projects ----------------------------------------------------------------

// Project is a Compose project Arcane knows about. Arcane scans its projects
// directory, nested folders included, so a stack folder PZAdmin has just
// written is a project before it has ever run.
type Project struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	DirName      string `json:"dirName"`
	Path         string `json:"path"`
	RelativePath string `json:"relativePath"`
	Status       string `json:"status"`
	ServiceCount int    `json:"serviceCount"`
	RunningCount int    `json:"runningCount"`
}

type projectPage struct {
	Data       []Project `json:"data"`
	Pagination struct {
		TotalItems int `json:"totalItems"`
	} `json:"pagination"`
}

// Projects lists every project in the environment.
func (c *Client) Projects(ctx context.Context) ([]Project, error) {
	var all []Project
	for page := 0; page < maxPages; page++ {
		q := url.Values{}
		q.Set("start", strconv.Itoa(page*pageSize))
		q.Set("limit", strconv.Itoa(pageSize))
		var p projectPage
		if err := c.getJSON(ctx, c.envPath("/projects")+"?"+q.Encode(), &p); err != nil {
			return nil, err
		}
		all = append(all, p.Data...)
		if len(p.Data) < pageSize || (p.Pagination.TotalItems > 0 && len(all) >= p.Pagination.TotalItems) {
			break
		}
	}
	return all, nil
}

// deployOutputCap bounds how much of Arcane's streamed Compose output is kept.
const deployOutputCap = 1 << 20

// Deploy runs `docker compose up -d` on a project through Arcane: it creates
// the container if there is none, and recreates it if the compose file or its
// environment changed. Images are pulled only when missing, and volumes are
// never recreated. It returns the tail of Compose's output.
//
// Arcane streams the output and gives no structured result, so success is
// the HTTP status and the caller should confirm the container afterwards.
func (c *Client) Deploy(ctx context.Context, p Project) (string, error) {
	if !c.allowProject(p) {
		return "", ErrNotAllowed
	}
	body := []byte(`{"pullPolicy":"missing","forceRecreate":false,"recreateVolumes":false,"removeOrphans":false}`)
	resp, err := c.sendBody(ctx, http.MethodPost, c.envPath("/projects/"+url.PathEscape(p.ID)+"/up"), body)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, deployOutputCap))
	_, _ = io.Copy(io.Discard, resp.Body)
	out := tailLines(data, 60)
	if err != nil && ctx.Err() != nil {
		return out, fmt.Errorf("%w: the deploy was still running when PZAdmin stopped waiting: %v", ErrUnavailable, ctx.Err())
	}
	c.mu.Lock()
	delete(c.ids, p.Name)
	c.mu.Unlock()
	return out, nil
}

// --- internals ---------------------------------------------------------------

type containerSummary struct {
	ID     string   `json:"id"`
	Names  []string `json:"names"`
	Image  string   `json:"image"`
	State  string   `json:"state"`
	Status string   `json:"status"`
}

func (s containerSummary) name() string {
	if len(s.Names) == 0 {
		return ""
	}
	return strings.TrimPrefix(s.Names[0], "/")
}

func (s containerSummary) hasName(name string) bool {
	for _, n := range s.Names {
		if strings.TrimPrefix(n, "/") == name {
			return true
		}
	}
	return false
}

type containerPage struct {
	Success    bool               `json:"success"`
	Data       []containerSummary `json:"data"`
	Pagination struct {
		TotalItems int `json:"totalItems"`
		TotalPages int `json:"totalPages"`
	} `json:"pagination"`
}

// pageSize is what each list request asks for. Arcane defaults to 20.
const pageSize = 100

// maxPages stops a misbehaving pager from looping forever.
const maxPages = 50

func (c *Client) listAll(ctx context.Context, search string) ([]containerSummary, error) {
	var all []containerSummary
	for page := 0; page < maxPages; page++ {
		q := url.Values{}
		q.Set("start", strconv.Itoa(page*pageSize))
		q.Set("limit", strconv.Itoa(pageSize))
		// Hidden and internal containers are still containers; a game server
		// that someone hid in Arcane's UI must not become uncontrollable.
		q.Set("includeHidden", "true")
		q.Set("includeInternal", "true")
		if search != "" {
			q.Set("search", search)
		}
		var p containerPage
		if err := c.getJSON(ctx, c.envPath("/containers")+"?"+q.Encode(), &p); err != nil {
			return nil, err
		}
		all = append(all, p.Data...)
		if len(p.Data) < pageSize || (p.Pagination.TotalItems > 0 && len(all) >= p.Pagination.TotalItems) {
			break
		}
	}
	return all, nil
}

// resolve finds the container ID for an exact name. Names are never passed
// as IDs: whether Arcane forwards a name to Docker unchanged is not part of
// its documented contract, and an exact match on the list is.
func (c *Client) resolve(ctx context.Context, name string, fresh bool) (string, error) {
	if !fresh {
		c.mu.Lock()
		hit, ok := c.ids[name]
		c.mu.Unlock()
		if ok && time.Since(hit.at) < idTTL {
			return hit.id, nil
		}
	}
	list, err := c.listAll(ctx, name)
	if err != nil {
		return "", err
	}
	for _, s := range list {
		if s.hasName(name) {
			c.mu.Lock()
			c.ids[name] = cachedID{id: s.ID, at: time.Now()}
			c.mu.Unlock()
			return s.ID, nil
		}
	}
	return "", fmt.Errorf("%w: %s in environment %s", ErrNotFound, name, c.env)
}

// withID runs fn with the container's ID, retrying once with a fresh lookup
// if the cached ID has gone (the container was recreated).
func (c *Client) withID(ctx context.Context, name string, fn func(id string) error) error {
	if !c.Configured() {
		return ErrNotConfigured
	}
	id, err := c.resolve(ctx, name, false)
	if err != nil {
		return err
	}
	err = fn(id)
	if errors.Is(err, ErrNotFound) {
		c.mu.Lock()
		delete(c.ids, name)
		c.mu.Unlock()
		if id, err = c.resolve(ctx, name, true); err != nil {
			return err
		}
		return fn(id)
	}
	return err
}

func (c *Client) envPath(p string) string {
	return "/environments/" + url.PathEscape(c.env) + p
}

func (c *Client) getJSON(ctx context.Context, path string, out any) error {
	body, err := c.do(ctx, http.MethodGet, path, 8<<20)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("arcane: unexpected response from %s: %w", path, err)
	}
	return nil
}

// do sends a request and returns up to limit bytes of a successful body.
func (c *Client) do(ctx context.Context, method, path string, limit int64) ([]byte, error) {
	resp, err := c.send(ctx, method, path)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if limit <= 0 {
		// Drain so the connection can be reused; the body is not wanted.
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		return nil, nil
	}
	return io.ReadAll(io.LimitReader(resp.Body, limit))
}

// send performs a request and turns any non-2xx answer into a typed error.
// On success the caller owns the body.
func (c *Client) send(ctx context.Context, method, path string) (*http.Response, error) {
	return c.sendBody(ctx, method, path, nil)
}

func (c *Client) sendBody(ctx context.Context, method, path string, body []byte) (*http.Response, error) {
	if !c.Configured() {
		return nil, ErrNotConfigured
	}
	var reader io.Reader
	if body != nil {
		reader = strings.NewReader(string(body))
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, reader)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("X-API-Key", c.key)
	req.Header.Set("Accept", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("%w: %v", ErrUnavailable, ctx.Err())
		}
		var dns *net.DNSError
		if errors.As(err, &dns) && dns.IsNotFound {
			return nil, fmt.Errorf("%w: PZAdmin's container cannot look up %q. Containers do not see "+
				"names from the host's hosts file or a LAN-only DNS server. If Arcane runs on this "+
				"machine, set PZADMIN_ARCANE_URL to http://host.docker.internal:<Arcane's port>; "+
				"otherwise use its IP address: %w", ErrUnavailable, dns.Name, err)
		}
		return nil, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return resp, nil
	}
	defer resp.Body.Close()
	errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	return nil, statusError(resp.StatusCode, method, path, errBody)
}

// problem is Arcane's error body (RFC 7807).
type problem struct {
	Title  string `json:"title"`
	Detail string `json:"detail"`
	Status int    `json:"status"`
}

func statusError(code int, method, path string, body []byte) error {
	var p problem
	_ = json.Unmarshal(body, &p)
	msg := strings.TrimSpace(p.Detail)
	if msg == "" {
		msg = strings.TrimSpace(p.Title)
	}
	if msg == "" {
		msg = strings.TrimSpace(string(body))
	}
	if len(msg) > 300 {
		msg = msg[:300]
	}
	switch code {
	case http.StatusUnauthorized:
		return fmt.Errorf("%w: Arcane did not accept the API key (%s). Check "+
			"PZADMIN_ARCANE_API_KEY; a key that was deleted, expired or copied "+
			"from the masked list gives this", ErrUnauthorized, msg)
	case http.StatusForbidden:
		perm := PermissionFor(path)
		if method == http.MethodDelete && strings.Contains(path, "/containers/") {
			perm = "containers:delete"
		}
		return fmt.Errorf("%w: the API key's role needs %s (%s)", ErrForbidden, perm, msg)
	case http.StatusNotFound:
		return fmt.Errorf("%w: %s (%s)", ErrNotFound, path, msg)
	}
	return fmt.Errorf("arcane: %s returned %d: %s", path, code, msg)
}

// PermissionFor names the Arcane permission an endpoint needs, so a 403 can
// say exactly what to add to the role.
func PermissionFor(path string) string {
	p := path
	if i := strings.IndexByte(p, '?'); i >= 0 {
		p = p[:i]
	}
	switch {
	case strings.HasSuffix(p, "/up"):
		return "projects:deploy"
	case strings.HasSuffix(p, "/projects"):
		return "projects:list"
	case strings.Contains(p, "/projects/"):
		return "projects:read"
	case strings.HasSuffix(p, "/start"):
		return "containers:start"
	case strings.HasSuffix(p, "/stop"):
		return "containers:stop"
	case strings.HasSuffix(p, "/logs/download"):
		return "containers:logs"
	case strings.HasSuffix(p, "/containers"):
		return "containers:list"
	case strings.Contains(p, "/containers/"):
		return "containers:read"
	}
	return "a permission for " + p
}

// tailLines keeps the last n lines of data, stripping Docker's stream framing if
// the log arrives multiplexed.
func tailLines(data []byte, n int) string {
	text := strings.TrimRight(demultiplex(data), "\n")
	if text == "" {
		return ""
	}
	all := strings.Split(text, "\n")
	if len(all) > n {
		all = all[len(all)-n:]
	}
	return strings.Join(all, "\n")
}

// demultiplex strips Docker's 8-byte stream framing. Containers without a TTY
// produce framed logs; whether Arcane strips the framing on download is not
// documented, so both forms are accepted.
func demultiplex(b []byte) string {
	var out strings.Builder
	for len(b) >= 8 {
		if b[0] > 2 || b[1] != 0 || b[2] != 0 || b[3] != 0 {
			out.Write(b)
			return out.String()
		}
		size := int(b[4])<<24 | int(b[5])<<16 | int(b[6])<<8 | int(b[7])
		b = b[8:]
		if size > len(b) {
			size = len(b)
		}
		out.Write(b[:size])
		b = b[size:]
	}
	out.Write(b)
	return out.String()
}
