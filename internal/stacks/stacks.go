// Package stacks finds Project Zomboid servers by scanning a folder of
// Compose stacks.
//
// The layout is one server per subfolder of the stacks root:
//
//	<root>/<name>/docker-compose.yml   one service, the game server
//	<root>/<name>/.env                 SERVER_NAME, RCON_PORT, RCON_PASSWORD, ...
//	<root>/<name>/Server/              <SERVER_NAME>.ini, _SandboxVars.lua, ...
//
// with the bulk data elsewhere, bind mounted by absolute path:
//
//	/srv/zomboid/<name>/projectzomboid/data    -> /project-zomboid
//	/srv/zomboid/<name>/projectzomboid/config  -> /project-zomboid-config
//	<root>/<name>/Server                       -> /project-zomboid-config/Server
//
// PZAdmin mounts its managed folders at the same path inside its own
// container as on the host, so every path in a compose file can be opened
// as written. Nothing is typed in per server: container name, ports, RCON
// details and folders are all read from these files.
package stacks

import (
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/TheWarBoys2/pzadmin/internal/compose"
	"github.com/TheWarBoys2/pzadmin/internal/pz"
)

// Container paths used by indifferentbroccoli/projectzomboid-server-docker.
const (
	TargetData   = "/project-zomboid"
	TargetConfig = "/project-zomboid-config"
	TargetServer = "/project-zomboid-config/Server"
)

// Image defaults, from the image's own README. Used only when .env is silent.
const (
	DefaultServerName = "pzserver"
	DefaultRCONPort   = 27015
	DefaultGamePort   = 16261
	DefaultUDPPort    = 16262
)

// maxStacks bounds the scan. A folder with more subfolders than this is not
// a folder of game servers.
const maxStacks = 128

// Mount role names.
const (
	RoleData   = "data"
	RoleConfig = "config"
	RoleServer = "server"
	RoleOther  = "other"
)

// Mount is one bind mount and the result of checking its source.
type Mount struct {
	Source string `json:"source"`
	Target string `json:"target"`
	Role   string `json:"role"`
	Named  bool   `json:"named,omitempty"`
	// OK is true when the source exists and is not an empty directory.
	OK bool `json:"ok"`
	// Problem explains why OK is false.
	Problem string `json:"problem,omitempty"`
}

// Stack is one discovered server.
type Stack struct {
	// Name is the subfolder name. It is the server's stable identity.
	Name        string `json:"name"`
	Dir         string `json:"dir"`
	ComposeFile string `json:"composeFile"`

	Service   string `json:"service"`
	Container string `json:"container"`
	Image     string `json:"image"`
	Restart   string `json:"restart"`
	// ImagePinned is true for a digest or an explicit tag other than latest.
	ImagePinned bool `json:"imagePinned"`
	// StopGrace is stop_grace_period as written; empty means Docker's 10s.
	StopGrace string `json:"stopGrace,omitempty"`

	// ServerName is SERVER_NAME: the ini and sandbox file prefix.
	ServerName string `json:"serverName"`

	// RCONPort is the port published on the host; RCONContainerPort is the
	// one the game listens on inside the container.
	RCONPort          int    `json:"rconPort"`
	RCONContainerPort int    `json:"rconContainerPort"`
	RCONPassword      string `json:"-"`
	HasRCONPassword   bool   `json:"hasRconPassword"`
	GamePort          int    `json:"gamePort,omitempty"`
	UDPPort           int    `json:"udpPort,omitempty"`

	DataDir   string `json:"dataDir,omitempty"`
	ConfigDir string `json:"configDir,omitempty"`
	// ServerDir holds the ini and lua files. It is the Server/ bind mount's
	// source, or <ConfigDir>/Server when there is no separate mount.
	ServerDir string `json:"serverDir,omitempty"`

	// Installed is true once the image has downloaded the game into DataDir.
	Installed bool `json:"installed"`
	// Configured is true once <ServerName>.ini exists in ServerDir.
	Configured bool `json:"configured"`

	Mounts []Mount `json:"mounts"`
	// Problems stop PZAdmin from starting the container.
	Problems []string `json:"problems,omitempty"`
	// Warnings are shown but do not block anything.
	Warnings []string `json:"warnings,omitempty"`
}

// Ready reports whether the stack is safe to start.
func (s Stack) Ready() bool { return len(s.Problems) == 0 }

// Result is one scan of the stacks root.
type Result struct {
	Root     string   `json:"root"`
	Stacks   []Stack  `json:"stacks"`
	Warnings []string `json:"warnings,omitempty"`
}

// Visible reports whether PZAdmin can see a host path. With same-path mounts
// that means the path sits under one of the folders mounted into PZAdmin;
// anything else does not exist from PZAdmin's point of view, and must be
// reported as unverifiable rather than as missing.
type Visible func(path string) bool

// UnderAny returns a Visible that accepts paths at or under any of roots.
func UnderAny(roots ...string) Visible {
	clean := make([]string, 0, len(roots))
	for _, r := range roots {
		if r = strings.TrimSpace(r); r != "" {
			clean = append(clean, filepath.Clean(r))
		}
	}
	return func(p string) bool {
		p = filepath.Clean(p)
		for _, r := range clean {
			if p == r || strings.HasPrefix(p, r+string(filepath.Separator)) {
				return true
			}
		}
		return false
	}
}

// Scan reads every subfolder of root that holds a compose file.
func Scan(root string, visible Visible) Result {
	res := Result{Root: root}
	if strings.TrimSpace(root) == "" {
		res.Warnings = append(res.Warnings, "No stacks folder is configured (PZADMIN_STACKS_ROOT).")
		return res
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		res.Warnings = append(res.Warnings, "Cannot read the stacks folder "+root+": "+err.Error())
		return res
	}
	if compose.FindComposeFile(root) != "" {
		res.Warnings = append(res.Warnings, "There is a compose file directly in "+root+
			". It is ignored: each server is expected in its own subfolder.")
	}
	count := 0
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		dir := filepath.Join(root, e.Name())
		file := compose.FindComposeFile(dir)
		if file == "" {
			continue
		}
		if count >= maxStacks {
			res.Warnings = append(res.Warnings, "Stopped after "+strconv.Itoa(maxStacks)+" stacks.")
			break
		}
		count++
		res.Stacks = append(res.Stacks, Inspect(dir, file, visible))
	}
	sort.Slice(res.Stacks, func(i, j int) bool { return res.Stacks[i].Name < res.Stacks[j].Name })
	return res
}

// Inspect reads one stack folder.
func Inspect(dir, file string, visible Visible) Stack {
	s := Stack{Name: filepath.Base(dir), Dir: dir, ComposeFile: file}
	if visible == nil {
		visible = func(string) bool { return true }
	}

	stack, err := compose.LoadStack(file)
	if err != nil {
		// Reported, never half-read: acting on a misread file would mean
		// acting on the wrong server.
		s.Problems = append(s.Problems, "Cannot read "+filepath.Base(file)+": "+err.Error())
		return s
	}
	s.Warnings = append(s.Warnings, stack.Warnings...)

	svc, ok := pickService(stack, &s)
	if !ok {
		return s
	}
	s.Service = svc.Key
	s.Container = svc.ContainerName
	s.Image = svc.Image
	s.Restart = svc.Restart
	s.ImagePinned = ImagePinned(s.Image)
	if !s.ImagePinned {
		s.Warnings = append(s.Warnings, "The image "+quoteOr(s.Image, "(none)")+" is not pinned, so a pull "+
			"or recreate can change it silently. Pin it to a digest (image@sha256:...) or a version tag.")
	}
	if svc.Node != nil {
		s.StopGrace = strings.TrimSpace(svc.Node.Get("stop_grace_period").Str())
	}
	if grace, ok := parseGrace(s.StopGrace); !ok || grace < MinStopGrace {
		s.Warnings = append(s.Warnings, "stop_grace_period is "+quoteOr(s.StopGrace, "not set (Docker's 10s)")+
			". A hard stop gives the game that long to save before it is killed; set it to at least "+
			strconv.Itoa(int(MinStopGrace.Seconds()))+"s.")
	}

	if _, err := os.Stat(filepath.Join(dir, ".env")); err == nil && len(svc.EnvFiles) == 0 {
		s.Warnings = append(s.Warnings, "There is a .env here but the service has no env_file entry, "+
			"so the container never sees it. Add `env_file: .env`.")
	}
	if s.Restart != "unless-stopped" && s.Restart != "always" {
		s.Warnings = append(s.Warnings, "restart is "+quoteOr(s.Restart, "not set")+
			". Restarts work by asking the game to quit and letting Docker bring it back, "+
			"which needs restart: unless-stopped.")
	}

	env := svc.Env
	s.ServerName = firstNonEmpty(env["SERVER_NAME"], DefaultServerName)
	if env["SERVER_NAME"] == "" {
		s.Warnings = append(s.Warnings, "SERVER_NAME is not set; the image default "+DefaultServerName+" is assumed.")
	}

	readMounts(svc, &s, visible)
	readINIAndPorts(svc, &s)

	if s.DataDir != "" {
		_, err := os.Stat(filepath.Join(s.DataDir, "start-server.sh"))
		s.Installed = err == nil
	}
	return s
}

func pickService(stack *compose.Stack, s *Stack) (compose.Service, bool) {
	switch len(stack.Services) {
	case 0:
		s.Problems = append(s.Problems, "The compose file has no services.")
		return compose.Service{}, false
	case 1:
		return stack.Services[0], true
	}
	var games []compose.Service
	for _, svc := range stack.Services {
		for _, m := range svc.Volumes {
			if cleanTarget(m.Target) == TargetConfig {
				games = append(games, svc)
				break
			}
		}
	}
	if len(games) == 1 {
		s.Warnings = append(s.Warnings, "The stack has several services; "+games[0].Key+
			" was taken as the game server.")
		return games[0], true
	}
	s.Problems = append(s.Problems, "The stack has "+strconv.Itoa(len(stack.Services))+
		" services. PZAdmin expects exactly one game server per stack folder.")
	return compose.Service{}, false
}

func readMounts(svc compose.Service, s *Stack, visible Visible) {
	for _, m := range svc.Volumes {
		target := cleanTarget(m.Target)
		out := Mount{Source: m.Source, Target: target, Named: m.Named, Role: RoleOther}
		switch target {
		case TargetData:
			out.Role = RoleData
		case TargetConfig:
			out.Role = RoleConfig
		case TargetServer:
			out.Role = RoleServer
		}
		checkMount(&out, rawSource(m), visible)
		if !out.OK {
			s.Problems = append(s.Problems, out.Target+": "+out.Problem)
		}
		if !out.Named {
			switch out.Role {
			case RoleData:
				s.DataDir = out.Source
			case RoleConfig:
				s.ConfigDir = out.Source
			case RoleServer:
				s.ServerDir = out.Source
			}
		}
		s.Mounts = append(s.Mounts, out)
	}
	if s.DataDir == "" {
		s.Problems = append(s.Problems, "Nothing is bind mounted at "+TargetData+
			"; PZAdmin cannot find the game files.")
	}
	if s.ConfigDir == "" {
		s.Problems = append(s.Problems, "Nothing is bind mounted at "+TargetConfig+
			"; PZAdmin cannot find saves or logs.")
	}
	if s.ServerDir == "" && s.ConfigDir != "" {
		s.ServerDir = filepath.Join(s.ConfigDir, "Server")
		s.Warnings = append(s.Warnings, "Server/ is not mounted separately, so its files live in "+
			s.ServerDir+" rather than in the stack folder.")
	}
}

// MinStopGrace is the shortest stop_grace_period that is not warned about.
// The image's SIGTERM handler saves over RCON, and a large world takes a
// while to write.
const MinStopGrace = 60 * time.Second

// parseGrace reads a Compose duration ("120s", "2m", "1m30s").
func parseGrace(v string) (time.Duration, bool) {
	if v == "" {
		return 10 * time.Second, true
	}
	d, err := time.ParseDuration(v)
	return d, err == nil
}

// ImagePinned reports whether an image reference names one exact image: a
// digest, or an explicit tag other than latest. Tags can still be re-pushed,
// but an untagged or latest reference changes on every pull by design.
func ImagePinned(ref string) bool {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return false
	}
	if strings.Contains(ref, "@sha256:") {
		return true
	}
	last := ref[strings.LastIndex(ref, "/")+1:]
	i := strings.LastIndex(last, ":")
	if i < 0 {
		return false
	}
	tag := last[i+1:]
	return tag != "" && tag != "latest"
}

// CheckSource applies the bind-source rule to one path: it must be absolute,
// visible to PZAdmin, exist, and not be an empty directory. It returns "" when
// the source is fine.
func CheckSource(source string, visible Visible) string {
	m := Mount{Source: source}
	checkMount(&m, source, visible)
	return m.Problem
}

// checkMount decides whether a bind mount source is safe to hand to Docker.
//
// Docker creates a missing bind source as an empty directory and mounts it
// without complaint, so a wrong path boots a blank world. The rule is
// therefore strict: the source must exist and must not be an empty
// directory. Folders PZAdmin provisions carry a marker file, so a brand-new
// server passes and a typo does not.
func checkMount(m *Mount, raw string, visible Visible) {
	if m.Named {
		m.OK = true
		m.Problem = ""
		return
	}
	switch {
	case m.Source == "" || strings.Contains(m.Source, "${"):
		m.Problem = "the source path did not resolve; a variable it uses is not set in .env"
		return
	case raw != "" && !filepath.IsAbs(raw):
		// Relative sources work, but they are how a compose file quietly
		// starts pointing somewhere else after being moved.
		m.Problem = "the source " + raw + " is relative; use an absolute path"
		return
	case !visible(m.Source):
		m.Problem = m.Source + " is outside the folders mounted into PZAdmin, so it cannot be checked"
		return
	}
	st, err := os.Stat(m.Source)
	if err != nil {
		m.Problem = m.Source + " does not exist. Docker would create it empty and the server would start with nothing in it"
		return
	}
	if st.IsDir() {
		f, err := os.Open(m.Source)
		if err != nil {
			m.Problem = "cannot read " + m.Source + ": " + err.Error()
			return
		}
		names, _ := f.Readdirnames(1)
		f.Close()
		if len(names) == 0 {
			m.Problem = m.Source + " is empty. That is what a mistyped path looks like after Docker has created it"
			return
		}
	}
	m.OK = true
}

func readINIAndPorts(svc compose.Service, s *Stack) {
	env := svc.Env
	envRCONPort := atoiPort(env["RCON_PORT"])
	envPassword := env["RCON_PASSWORD"]

	// The image rewrites RCONPort and RCONPassword into the ini from its
	// environment on every boot. So the ini says what the running container
	// uses, and .env says what the next recreate will use. Connecting needs
	// the former.
	s.RCONContainerPort = firstPositive(envRCONPort, DefaultRCONPort)
	s.RCONPassword = envPassword
	if s.ServerDir != "" {
		iniPath := filepath.Join(s.ServerDir, s.ServerName+".ini")
		if ini, err := pz.LoadINI(iniPath); err == nil {
			s.Configured = true
			if p := ini.GetInt("RCONPort", 0); p > 0 {
				if envRCONPort > 0 && p != envRCONPort {
					s.Warnings = append(s.Warnings, "RCON_PORT in .env ("+strconv.Itoa(envRCONPort)+
						") differs from the ini ("+strconv.Itoa(p)+"). The container is still using the "+
						"old value; recreate it to apply .env.")
				}
				s.RCONContainerPort = p
			}
			if v, ok := ini.Get("RCONPassword"); ok && v != "" {
				if envPassword != "" && v != envPassword {
					s.Warnings = append(s.Warnings, "RCON_PASSWORD in .env differs from the ini. "+
						"The container is still using the old one; recreate it to apply .env.")
				}
				s.RCONPassword = v
			}
		}
	}
	s.HasRCONPassword = s.RCONPassword != ""
	if !s.HasRCONPassword {
		s.Problems = append(s.Problems, "No RCON password is set (RCON_PASSWORD in .env). "+
			"PZAdmin cannot reach the server without one.")
	}

	s.RCONPort = published(svc.Ports, s.RCONContainerPort, "tcp")
	if s.RCONPort == 0 {
		s.Problems = append(s.Problems, "RCON port "+strconv.Itoa(s.RCONContainerPort)+
			"/tcp is not published, so PZAdmin cannot reach it. Add it under ports.")
	}
	s.GamePort = published(svc.Ports, firstPositive(atoiPort(env["DEFAULT_PORT"]), DefaultGamePort), "udp")
	s.UDPPort = published(svc.Ports, firstPositive(atoiPort(env["UDP_PORT"]), DefaultUDPPort), "udp")
}

// published returns the host port mapped to a container port and protocol.
// A mapping without a protocol is TCP, as in Compose.
func published(ports []compose.Port, container int, proto string) int {
	if container <= 0 {
		return 0
	}
	for _, p := range ports {
		pp := strings.ToLower(p.Proto)
		if pp == "" {
			pp = "tcp"
		}
		if pp != proto || atoiPort(p.Container) != container {
			continue
		}
		if h := atoiPort(p.Host); h > 0 {
			return h
		}
		// "27015/tcp" with no host side publishes on a random port, which
		// cannot be relied on.
	}
	return 0
}

// rawSource recovers the source as written, before relative resolution.
func rawSource(m compose.Mount) string {
	if i := strings.Index(m.Raw, ":"+m.Target); i >= 0 {
		return m.Raw[:i]
	}
	return m.Source
}

func cleanTarget(t string) string {
	t = strings.TrimSpace(t)
	if t == "" {
		return t
	}
	return filepath.Clean(t)
}

func atoiPort(v string) int {
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil || n <= 0 || n > 65535 {
		return 0
	}
	return n
}

func firstPositive(v ...int) int {
	for _, n := range v {
		if n > 0 {
			return n
		}
	}
	return 0
}

func firstNonEmpty(v ...string) string {
	for _, s := range v {
		if s = strings.TrimSpace(s); s != "" {
			return s
		}
	}
	return ""
}

func quoteOr(v, def string) string {
	if v == "" {
		return def
	}
	return strconv.Quote(v)
}
