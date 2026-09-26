// Package provision writes a new server's stack folder: compose file, .env
// and a seeded Server/ folder, plus its data and config folders.
//
// The shape it writes is exactly what package stacks reads back, and nothing
// is considered done until stacks.Inspect says the result is ready to start.
// Every rule that protects a running server applies to a new one too:
// absolute bind sources only, a pinned image, restart: unless-stopped, a
// stop grace period long enough to save, and no bind source that is missing
// or empty when Docker sees it.
//
// PZAdmin cannot create the container: that is `docker compose up -d` in the
// stack folder, which the result spells out.
package provision

import (
	"crypto/rand"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/TheWarBoys2/pzadmin/internal/compose"
	"github.com/TheWarBoys2/pzadmin/internal/pz"
	"github.com/TheWarBoys2/pzadmin/internal/stacks"
)

// Marker is written into every folder PZAdmin creates, so the empty-source
// check can tell a folder provisioned on purpose from one Docker created
// behind a typo.
const Marker = ".pzadmin"

// Port bases follow Project Zomboid's defaults.
const (
	BaseGamePort = 16261
	BaseRCONPort = 27015
)

// StopGrace is written into every generated compose file.
const StopGrace = "120s"

// Starting points.
const (
	// StartDefaults begins from a clean server's own first-boot files.
	StartDefaults = "defaults"
	// StartClone begins from another server's files, minus its identity.
	StartClone = "clone"
)

// Request describes a new server: everything the wizard collected.
type Request struct {
	// Name is the stack folder and the container name.
	Name string `json:"name"`
	// ServerName is SERVER_NAME, the ini and lua file prefix.
	ServerName string `json:"serverName"`
	// DataDir and ConfigDir default to <dataRoot>/<short>/projectzomboid/…
	DataDir   string `json:"dataDir,omitempty"`
	ConfigDir string `json:"configDir,omitempty"`

	// Ports default to the next free ones across all stacks.
	GamePort int `json:"gamePort,omitempty"`
	RCONPort int `json:"rconPort,omitempty"`

	MaxPlayers    int   `json:"maxPlayers,omitempty"`
	MemoryGB      int   `json:"memoryGb,omitempty"`
	UpdateOnStart *bool `json:"updateOnStart,omitempty"`

	// The in-game admin account, created on first boot. Chosen by the
	// operator, because it is typed into the game, not into PZAdmin.
	AdminUsername string `json:"adminUsername,omitempty"`
	AdminPassword string `json:"adminPassword,omitempty"`

	// Start is StartDefaults or StartClone; FromDir and FromName name the
	// source server for a clone. The browser names it by server ID and the
	// caller fills these in.
	Start    string `json:"start"`
	FromDir  string `json:"-"`
	FromName string `json:"-"`

	// INI and Sandbox are the settings changed in the wizard, by key.
	INI     map[string]string `json:"ini,omitempty"`
	Sandbox map[string]string `json:"sandbox,omitempty"`
}

// Starting returns the files a request begins from.
func (r Request) Starting() (Starting, error) {
	switch r.Start {
	case StartDefaults, "":
		return DefaultStart()
	case StartClone:
		if r.FromDir == "" || r.FromName == "" {
			return Starting{}, errors.New("pick a server to copy")
		}
		return CloneStart(r.FromDir, r.FromName)
	}
	return Starting{}, errors.New(`start must be "defaults" or "clone"`)
}

// Env is the host context a plan is made in.
type Env struct {
	StacksRoot string
	DataRoot   string
	// Image is the pinned image reference every new stack uses.
	Image string
	// Existing are the stacks already present, for port allocation.
	Existing []stacks.Stack
	Visible  stacks.Visible
	PUID     int
	PGID     int
}

// Plan is everything Apply will do. It is shown before anything is written.
type Plan struct {
	Name       string   `json:"name"`
	ServerName string   `json:"serverName"`
	StackDir   string   `json:"stackDir"`
	DataDir    string   `json:"dataDir"`
	ConfigDir  string   `json:"configDir"`
	ServerDir  string   `json:"serverDir"`
	Image      string   `json:"image"`
	GamePort   int      `json:"gamePort"`
	UDPPort    int      `json:"udpPort"`
	RCONPort   int      `json:"rconPort"`
	Compose    string   `json:"compose"`
	EnvFile    string   `json:"env"`
	Files      []string `json:"files"`
	Creates    []string `json:"creates"`
	Reuses     []string `json:"reuses,omitempty"`
	Warnings   []string `json:"warnings,omitempty"`
	Command    string   `json:"command"`
	// StartedFrom says which files the server began from.
	StartedFrom   string `json:"startedFrom"`
	AdminUsername string `json:"adminUsername"`
	// ServerFiles are the exact contents of Server/, by file name.
	ServerFiles map[string]string `json:"serverFiles"`
	// Changed counts the settings the wizard changed.
	ChangedINI     int `json:"changedIni"`
	ChangedSandbox int `json:"changedSandbox"`

	rconPassword  string
	adminPassword string
	stacksRoot    string
	dataRoot      string
	visible       stacks.Visible
}

var (
	nameRe       = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,39}$`)
	serverNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,39}$`)
	// safePath excludes everything that would need quoting in a compose
	// short-syntax volume or could split it: spaces, colons, quotes, $.
	safePath = regexp.MustCompile(`^/[A-Za-z0-9._/-]+$`)
)

// Make validates a request and works out the plan. It touches nothing.
func Make(req Request, env Env) (*Plan, error) {
	req.Name = strings.TrimSpace(req.Name)
	req.ServerName = strings.TrimSpace(req.ServerName)
	if !nameRe.MatchString(req.Name) {
		return nil, errors.New("the stack name must be lower case letters, digits, - or _, up to 40 characters")
	}
	short := strings.TrimPrefix(req.Name, "pz-")
	if req.ServerName == "" {
		req.ServerName = short
	}
	if !serverNameRe.MatchString(req.ServerName) {
		return nil, errors.New("the server name must be letters, digits, - or _, up to 40 characters")
	}
	if !stacks.ImagePinned(env.Image) {
		return nil, fmt.Errorf("the game image %q is not pinned. Set PZADMIN_GAME_IMAGE under environment: in PZAdmin's docker-compose.yml "+
			"to a digest (image@sha256:...) or a version tag", env.Image)
	}
	if env.StacksRoot == "" || env.DataRoot == "" {
		return nil, errors.New("the stacks and data folders are not configured")
	}
	if env.Visible == nil {
		env.Visible = stacks.UnderAny(env.StacksRoot, env.DataRoot)
	}

	p := &Plan{
		Name:       req.Name,
		ServerName: req.ServerName,
		StackDir:   filepath.Join(env.StacksRoot, req.Name),
		Image:      env.Image,
		stacksRoot: env.StacksRoot,
		dataRoot:   env.DataRoot,
		visible:    env.Visible,
	}
	p.ServerDir = filepath.Join(p.StackDir, "Server")
	if _, err := os.Lstat(p.StackDir); err == nil {
		return nil, fmt.Errorf("%s already exists. PZAdmin never writes into an existing stack folder", p.StackDir)
	}
	for _, st := range env.Existing {
		if st.Container == req.Name {
			return nil, fmt.Errorf("the container name %s is already used by the stack %s", req.Name, st.Name)
		}
	}

	base := filepath.Join(env.DataRoot, short, "projectzomboid")
	p.DataDir = firstNonEmpty(req.DataDir, filepath.Join(base, "data"))
	p.ConfigDir = firstNonEmpty(req.ConfigDir, filepath.Join(base, "config"))
	for _, d := range []struct{ label, path string }{{"data", p.DataDir}, {"config", p.ConfigDir}} {
		clean := filepath.Clean(d.path)
		if !safePath.MatchString(clean) {
			return nil, fmt.Errorf("the %s folder %q must be an absolute path of letters, digits and . _ - /", d.label, d.path)
		}
		if !underDir(env.DataRoot, clean) || clean == filepath.Clean(env.DataRoot) {
			return nil, fmt.Errorf("the %s folder must be inside %s", d.label, env.DataRoot)
		}
	}
	p.DataDir, p.ConfigDir = filepath.Clean(p.DataDir), filepath.Clean(p.ConfigDir)
	if p.DataDir == p.ConfigDir || underDir(p.DataDir, p.ConfigDir) || underDir(p.ConfigDir, p.DataDir) {
		return nil, errors.New("the data and config folders must be separate, and neither inside the other")
	}
	if !safePath.MatchString(p.StackDir) {
		return nil, fmt.Errorf("the stack folder %q contains characters PZAdmin will not write into a compose file", p.StackDir)
	}
	for _, st := range env.Existing {
		for _, used := range []string{st.DataDir, st.ConfigDir} {
			if used != "" && (used == p.DataDir || used == p.ConfigDir) {
				return nil, fmt.Errorf("%s is already mounted by %s. Two servers on one folder corrupt each other's world", used, st.Name)
			}
		}
	}
	for _, d := range []string{p.DataDir, p.ConfigDir} {
		switch state(d) {
		case dirMissing:
			p.Creates = append(p.Creates, d)
		case dirEmpty:
			p.Creates = append(p.Creates, d) // gets a marker
		case dirFull:
			p.Reuses = append(p.Reuses, d)
			p.Warnings = append(p.Warnings, d+" already has files in it and will be used as it is.")
		}
	}
	if state(filepath.Join(p.ConfigDir, "Server")) == dirFull {
		p.Warnings = append(p.Warnings, filepath.Join(p.ConfigDir, "Server")+" has files in it. They will be hidden "+
			"behind the stack's own Server/ folder and not used.")
	}

	if err := p.allocatePorts(req, env.Existing); err != nil {
		return nil, err
	}

	maxPlayers := req.MaxPlayers
	if maxPlayers <= 0 {
		maxPlayers = 32
	}
	memory := req.MemoryGB
	if memory <= 0 {
		memory = 8
	}
	update := true
	if req.UpdateOnStart != nil {
		update = *req.UpdateOnStart
	}
	admin := firstNonEmpty(req.AdminUsername, "admin")
	if !serverNameRe.MatchString(admin) {
		return nil, errors.New("the admin username must be letters, digits, - or _")
	}
	if err := ValidAdminPassword(req.AdminPassword); err != nil {
		return nil, err
	}
	p.AdminUsername = admin
	p.adminPassword = req.AdminPassword
	p.rconPassword = randomSecret(24)
	puid, pgid := env.PUID, env.PGID
	if puid <= 0 {
		puid = 1000
	}
	if pgid <= 0 {
		pgid = 1000
	}

	p.EnvFile = renderEnv([][2]string{
		{"PUID", strconv.Itoa(puid)},
		{"PGID", strconv.Itoa(pgid)},
		{"MEMORY_XMX_GB", strconv.Itoa(memory)},
		{"MEMORY_XMS_GB", ""},
		{"VM_ARGS", ""},
		{"RCON_PASSWORD", p.rconPassword},
		{"ADMIN_USERNAME", admin},
		{"ADMIN_PASSWORD", p.adminPassword},
		{"UPDATE_ON_START", strconv.FormatBool(update)},
		{"SERVER_BRANCH", ""},
		{"SERVER_NAME", p.ServerName},
		{"DEFAULT_PORT", strconv.Itoa(p.GamePort)},
		{"UDP_PORT", strconv.Itoa(p.UDPPort)},
		{"RCON_PORT", strconv.Itoa(p.RCONPort)},
		{"MAX_PLAYERS", strconv.Itoa(maxPlayers)},
		{"STEAM_VAC", "true"},
		{"USE_STEAM", "true"},
	})
	p.Compose = renderCompose(p)
	if err := p.buildServerFiles(req, maxPlayers); err != nil {
		return nil, err
	}
	p.Files = []string{
		filepath.Join(p.StackDir, "docker-compose.yml"),
		filepath.Join(p.StackDir, ".env"),
	}
	names := make([]string, 0, len(p.ServerFiles))
	for name := range p.ServerFiles {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		p.Files = append(p.Files, filepath.Join(p.ServerDir, name))
	}
	p.Command = "cd " + p.StackDir + " && docker compose up -d"
	return p, nil
}

func (p *Plan) allocatePorts(req Request, existing []stacks.Stack) error {
	used := map[int]string{}
	for _, st := range existing {
		for _, port := range []int{st.GamePort, st.UDPPort, st.RCONPort} {
			if port > 0 {
				used[port] = st.Name
			}
		}
	}
	valid := func(n int) bool { return n >= 1024 && n <= 65534 }
	if req.GamePort != 0 {
		if !valid(req.GamePort) {
			return errors.New("the game port must be between 1024 and 65534")
		}
		p.GamePort = req.GamePort
	} else {
		for n := BaseGamePort; n < BaseGamePort+400; n += 2 {
			if used[n] == "" && used[n+1] == "" {
				p.GamePort = n
				break
			}
		}
	}
	p.UDPPort = p.GamePort + 1
	if req.RCONPort != 0 {
		if !valid(req.RCONPort) {
			return errors.New("the RCON port must be between 1024 and 65534")
		}
		p.RCONPort = req.RCONPort
	} else {
		for n := BaseRCONPort; n < BaseRCONPort+400; n++ {
			if used[n] == "" {
				p.RCONPort = n
				break
			}
		}
	}
	if p.GamePort == 0 || p.RCONPort == 0 {
		return errors.New("no free ports were found")
	}
	for _, n := range []int{p.GamePort, p.UDPPort, p.RCONPort} {
		if owner := used[n]; owner != "" {
			return fmt.Errorf("port %d is already published by %s", n, owner)
		}
	}
	if p.RCONPort == p.GamePort || p.RCONPort == p.UDPPort {
		return errors.New("the RCON port must differ from the game ports")
	}
	return nil
}

// buildServerFiles works out the exact contents of Server/: the starting
// files, the wizard's changes, and the values .env owns, which the image
// would write on boot anyway and which are written now so the files are
// right before the first boot too.
func (p *Plan) buildServerFiles(req Request, maxPlayers int) error {
	start, err := req.Starting()
	if err != nil {
		return err
	}
	p.StartedFrom = start.From
	iniText, sbText, err := applyChanges(start, req.INI, req.Sandbox)
	if err != nil {
		return err
	}
	ini, err := pz.ParseINI(iniText)
	if err != nil {
		return err
	}
	for key, value := range map[string]string{
		"DefaultPort":  strconv.Itoa(p.GamePort),
		"UDPPort":      strconv.Itoa(p.UDPPort),
		"RCONPort":     strconv.Itoa(p.RCONPort),
		"RCONPassword": p.rconPassword,
		"MaxPlayers":   strconv.Itoa(maxPlayers),
	} {
		if err := ini.Set(key, value); err != nil {
			return err
		}
	}
	p.ChangedINI, p.ChangedSandbox = len(req.INI), len(req.Sandbox)
	p.ServerFiles = map[string]string{
		p.ServerName + ".ini":             ensureNewline(ini.Render()),
		p.ServerName + "_SandboxVars.lua": ensureNewline(sbText),
	}
	// The spawn regions file names the spawn points file, so it has to be
	// renamed along with it.
	if start.SpawnRegions != "" {
		p.ServerFiles[p.ServerName+"_spawnregions.lua"] = strings.ReplaceAll(start.SpawnRegions,
			start.From+"_spawnpoints.lua", p.ServerName+"_spawnpoints.lua")
	}
	if start.SpawnPoints != "" {
		p.ServerFiles[p.ServerName+"_spawnpoints.lua"] = start.SpawnPoints
	}
	return nil
}

func ensureNewline(s string) string {
	if strings.HasSuffix(s, "\n") {
		return s
	}
	return s + "\n"
}

// ValidAdminPassword checks the in-game admin password. It lands in .env,
// where Compose interpolates $, treats # after a space as a comment and
// strips quotes, so those, spaces and backslashes are refused rather than
// silently mangled.
func ValidAdminPassword(v string) error {
	if len(v) < 4 || len(v) > 64 {
		return errors.New("the admin password must be 4 to 64 characters")
	}
	for _, r := range v {
		if r <= ' ' || r > '~' || strings.ContainsRune("\"'`$#\\", r) {
			return errors.New("the admin password cannot contain spaces, quotes, $, # or backslashes")
		}
	}
	return nil
}

// Result is what Apply did.
type Result struct {
	Plan  *Plan        `json:"plan"`
	Stack stacks.Stack `json:"stack"`
}

// Apply writes the plan. On any failure it removes what it created, and only
// what it created: a reused folder is never touched.
func Apply(p *Plan) (*Result, error) {
	var created []string
	undo := func() {
		for i := len(created) - 1; i >= 0; i-- {
			_ = os.RemoveAll(created[i])
		}
	}

	if err := os.Mkdir(p.StackDir, 0o755); err != nil {
		return nil, fmt.Errorf("create %s: %w", p.StackDir, err)
	}
	created = append(created, p.StackDir)

	for _, d := range p.Creates {
		existed := state(d) != dirMissing
		// Undo removes the highest folder this call created, not just the
		// leaf, so a failed create leaves no empty parents behind.
		top := topMissing(d, p.dataRoot)
		if err := os.MkdirAll(d, 0o755); err != nil {
			undo()
			return nil, fmt.Errorf("create %s: %w", d, err)
		}
		if !existed && top != "" {
			created = append(created, top)
		}
		if err := os.WriteFile(filepath.Join(d, Marker),
			[]byte("Created by PZAdmin for "+p.Name+". Safe to delete once the server has run.\n"), 0o644); err != nil {
			undo()
			return nil, fmt.Errorf("mark %s: %w", d, err)
		}
		if existed {
			created = append(created, filepath.Join(d, Marker))
		}
	}

	if err := os.Mkdir(p.ServerDir, 0o755); err != nil {
		undo()
		return nil, err
	}
	for name, content := range p.ServerFiles {
		if err := os.WriteFile(filepath.Join(p.ServerDir, name), []byte(content), 0o644); err != nil {
			undo()
			return nil, err
		}
	}
	if err := compose.WriteFileAtomic(filepath.Join(p.StackDir, ".env"), []byte(p.EnvFile), 0o600); err != nil {
		undo()
		return nil, err
	}
	composePath := filepath.Join(p.StackDir, "docker-compose.yml")
	if err := compose.WriteFileAtomic(composePath, []byte(p.Compose), 0o644); err != nil {
		undo()
		return nil, err
	}

	// Read back what was written exactly as discovery will. Anything short of
	// ready means the files do not say what the plan said.
	st := stacks.Inspect(p.StackDir, composePath, p.visible)
	if !st.Ready() {
		undo()
		return nil, fmt.Errorf("the written stack did not pass its own checks: %s", strings.Join(st.Problems, "; "))
	}
	if st.RCONPort != p.RCONPort || st.Container != p.Name || st.ServerName != p.ServerName {
		undo()
		return nil, errors.New("the written stack reads back differently from the plan")
	}
	return &Result{Plan: p, Stack: st}, nil
}

// Masked returns a copy safe to show before anything is written: the
// secrets in it would be regenerated on create anyway.
func (p *Plan) Masked() *Plan {
	cp := *p
	lines := strings.Split(cp.EnvFile, "\n")
	for i, l := range lines {
		for _, k := range []string{"RCON_PASSWORD=", "ADMIN_PASSWORD="} {
			if strings.HasPrefix(l, k) {
				lines[i] = k + "(hidden)"
			}
		}
	}
	cp.EnvFile = strings.Join(lines, "\n")
	cp.ServerFiles = map[string]string{}
	for name, content := range p.ServerFiles {
		if strings.HasSuffix(name, ".ini") {
			content = strings.Replace(content, "RCONPassword="+p.rconPassword, "RCONPassword=(generated on create)", 1)
		}
		cp.ServerFiles[name] = content
	}
	return &cp
}

func renderCompose(p *Plan) string {
	var b strings.Builder
	b.WriteString("# Written by PZAdmin. Edit freely; PZAdmin reads this file, it does not own it.\n")
	b.WriteString("services:\n")
	fmt.Fprintf(&b, "  %s:\n", p.Name)
	fmt.Fprintf(&b, "    image: %s\n", p.Image)
	fmt.Fprintf(&b, "    container_name: %s\n", p.Name)
	b.WriteString("    restart: unless-stopped\n")
	fmt.Fprintf(&b, "    stop_grace_period: %s\n", StopGrace)
	b.WriteString("    env_file:\n      - .env\n")
	b.WriteString("    ports:\n")
	fmt.Fprintf(&b, "      - \"%d:%d/udp\"\n", p.GamePort, p.GamePort)
	fmt.Fprintf(&b, "      - \"%d:%d/udp\"\n", p.UDPPort, p.UDPPort)
	fmt.Fprintf(&b, "      - \"%d:%d/tcp\"   # RCON: PZAdmin needs this\n", p.RCONPort, p.RCONPort)
	b.WriteString("    volumes:\n")
	fmt.Fprintf(&b, "      - %s:%s\n", p.DataDir, stacks.TargetData)
	fmt.Fprintf(&b, "      - %s:%s\n", p.ConfigDir, stacks.TargetConfig)
	fmt.Fprintf(&b, "      - %s:%s\n", p.ServerDir, stacks.TargetServer)
	return b.String()
}

func renderEnv(pairs [][2]string) string {
	var b strings.Builder
	b.WriteString("# Written by PZAdmin. RCON_PORT, RCON_PASSWORD, DEFAULT_PORT, UDP_PORT and\n")
	b.WriteString("# MAX_PLAYERS are written into the ini by the image on every boot.\n")
	b.WriteString("# Changes here need a recreate: docker compose up -d\n")
	for _, kv := range pairs {
		fmt.Fprintf(&b, "%s=%s\n", kv[0], kv[1])
	}
	return b.String()
}

// topMissing returns the highest ancestor of dir, below stop, that does not
// exist yet: the first folder MkdirAll will create.
func topMissing(dir, stop string) string {
	top := ""
	for d := filepath.Clean(dir); underDir(stop, d) && d != filepath.Clean(stop); d = filepath.Dir(d) {
		if _, err := os.Lstat(d); err == nil {
			break
		}
		top = d
	}
	return top
}

type dirState int

const (
	dirMissing dirState = iota
	dirEmpty
	dirFull
)

func state(dir string) dirState {
	f, err := os.Open(dir)
	if err != nil {
		return dirMissing
	}
	defer f.Close()
	if names, _ := f.Readdirnames(1); len(names) == 0 {
		return dirEmpty
	}
	return dirFull
}

func underDir(dir, child string) bool {
	dir, child = filepath.Clean(dir), filepath.Clean(child)
	return child == dir || strings.HasPrefix(child, dir+string(filepath.Separator))
}

func firstNonEmpty(v ...string) string {
	for _, s := range v {
		if s = strings.TrimSpace(s); s != "" {
			return s
		}
	}
	return ""
}

// randomSecret returns n characters from an alphabet that needs no quoting in
// an env file or an ini.
func randomSecret(n int) string {
	const alphabet = "abcdefghijkmnopqrstuvwxyzABCDEFGHJKLMNPQRSTUVWXYZ23456789"
	out := make([]byte, n)
	for i := range out {
		k, err := rand.Int(rand.Reader, big.NewInt(int64(len(alphabet))))
		if err != nil {
			panic(err)
		}
		out[i] = alphabet[k.Int64()]
	}
	return string(out)
}
