package compose

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Mount is one bind mount or volume on a service.
type Mount struct {
	// Source is the host side. For a bind mount it is an absolute path once
	// resolved; for a named volume it is the volume's name.
	Source string `json:"source"`
	Target string `json:"target"`
	Mode   string `json:"mode,omitempty"`
	// Named is true for a Docker volume rather than a path on the host.
	Named bool   `json:"named"`
	Raw   string `json:"raw"`
}

// Port is one published port.
type Port struct {
	HostIP    string `json:"hostIp,omitempty"`
	Host      string `json:"host"`
	Container string `json:"container"`
	Proto     string `json:"proto,omitempty"`
	Raw       string `json:"raw"`
}

// Service is one entry under `services:`.
type Service struct {
	// Key is the name in the compose file, which is not the container name.
	Key           string            `json:"key"`
	ContainerName string            `json:"containerName,omitempty"`
	Image         string            `json:"image,omitempty"`
	Restart       string            `json:"restart,omitempty"`
	EnvFiles      []string          `json:"envFiles,omitempty"`
	Env           map[string]string `json:"env,omitempty"`
	Volumes       []Mount           `json:"volumes,omitempty"`
	Ports         []Port            `json:"ports,omitempty"`

	// Node is the raw parsed service, kept so a new service can be rendered
	// from an existing one without knowing anything about the image's own
	// conventions.
	Node *Node `json:"-"`
}

// Stack is one compose file and everything in it.
type Stack struct {
	Path string `json:"path"`
	Dir  string `json:"dir"`
	// Project is the compose project name, which decides container name
	// prefixes when a service does not set container_name.
	Project  string    `json:"project"`
	Services []Service `json:"services"`
	// Warnings explain anything PZAdmin read but could not fully resolve.
	// They are shown rather than logged: an unresolved variable is the
	// difference between finding a server and not.
	Warnings []string `json:"warnings,omitempty"`
}

// ComposeFileNames are the file names Docker Compose itself looks for.
var ComposeFileNames = []string{
	"compose.yaml", "compose.yml", "docker-compose.yaml", "docker-compose.yml",
}

// FindComposeFile returns the compose file in dir, using the same precedence
// Docker Compose uses.
func FindComposeFile(dir string) string {
	for _, name := range ComposeFileNames {
		p := filepath.Join(dir, name)
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p
		}
	}
	return ""
}

// maxComposeBytes bounds what will be read as a compose file. Real ones are a
// few kilobytes; anything approaching this is not a compose file.
const maxComposeBytes = 2 << 20

// LoadStack reads a compose file and the .env file beside it.
func LoadStack(path string) (*Stack, error) {
	st, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if st.Size() > maxComposeBytes {
		return nil, fmt.Errorf("%s is too large to be a compose file", filepath.Base(path))
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	dir := filepath.Dir(path)
	stack := &Stack{Path: path, Dir: dir, Project: strings.ToLower(filepath.Base(dir))}

	// Compose interpolates the file using the project's .env, and only that.
	// PZAdmin's own environment is deliberately not consulted: it runs in a
	// different container from the one the daemon will start, so its variables
	// have nothing to do with what the stack will see.
	projectEnv, err := ReadEnvFile(filepath.Join(dir, ".env"))
	if err != nil {
		return nil, err
	}
	vars := projectEnv.Map()

	doc, err := Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", filepath.Base(path), err)
	}
	if name := doc.Get("name").Str(); name != "" {
		resolved, _ := interpolate(name, vars)
		stack.Project = resolved
	}

	services := doc.Get("services")
	if services == nil || services.Kind != KindMapping {
		return stack, nil
	}

	var missingVars []string
	expand := func(s string) string {
		out, missing := interpolate(s, vars)
		missingVars = append(missingVars, missing...)
		return out
	}

	for _, key := range services.Keys {
		node := services.Get(key)
		if node == nil || node.Kind != KindMapping {
			continue
		}
		svc := Service{
			Key:           key,
			ContainerName: expand(node.Get("container_name").Str()),
			Image:         expand(node.Get("image").Str()),
			Restart:       expand(node.Get("restart").Str()),
			Env:           map[string]string{},
			Node:          node,
		}

		for _, ef := range node.Get("env_file").Strs() {
			svc.EnvFiles = append(svc.EnvFiles, resolvePath(dir, expand(ef)))
		}
		// env_file entries can also be mappings with a path: key.
		if n := node.Get("env_file"); n != nil && n.Kind == KindSequence {
			for _, item := range n.Items {
				if item.Kind == KindMapping {
					if p := item.Get("path").Str(); p != "" {
						svc.EnvFiles = append(svc.EnvFiles, resolvePath(dir, expand(p)))
					}
				}
			}
		}
		for _, envPath := range svc.EnvFiles {
			file, err := ReadEnvFile(envPath)
			if err != nil {
				stack.Warnings = append(stack.Warnings,
					fmt.Sprintf("service %s: cannot read %s: %v", key, filepath.Base(envPath), err))
				continue
			}
			for k, v := range file.Map() {
				svc.Env[k] = v
			}
		}
		// Inline environment overrides env_file, as it does in Compose.
		for k, v := range readEnvironment(node.Get("environment")) {
			svc.Env[k] = expand(v)
		}

		svc.Volumes = readVolumes(node.Get("volumes"), dir, expand)
		svc.Ports = readPorts(node.Get("ports"), expand)

		if svc.ContainerName == "" {
			// Compose's default naming. Getting this right matters: PZAdmin
			// controls containers by name, and guessing wrong here would mean
			// the buttons act on nothing.
			svc.ContainerName = stack.Project + "-" + key + "-1"
		}
		stack.Services = append(stack.Services, svc)
	}

	for _, name := range dedupeStrings(sortedCopy(missingVars)) {
		stack.Warnings = append(stack.Warnings, fmt.Sprintf(
			"%s uses ${%s}, which is not set in the .env beside it. Paths and ports that "+
				"depend on it could not be resolved.", filepath.Base(path), name))
	}
	return stack, nil
}

func sortedCopy(in []string) []string {
	out := append([]string{}, in...)
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// readEnvironment handles both spellings of the environment key: a mapping of
// names to values, and a list of NAME=value strings.
func readEnvironment(n *Node) map[string]string {
	out := map[string]string{}
	if n == nil {
		return out
	}
	switch n.Kind {
	case KindMapping:
		for _, k := range n.Keys {
			out[k] = n.Get(k).Str()
		}
	case KindSequence:
		for _, item := range n.Items {
			if item.Kind != KindScalar {
				continue
			}
			k, v, found := strings.Cut(item.Value, "=")
			if !found {
				// "NAME" with no value means "take it from the host".
				out[strings.TrimSpace(k)] = ""
				continue
			}
			out[strings.TrimSpace(k)] = v
		}
	}
	return out
}

func readVolumes(n *Node, dir string, expand func(string) string) []Mount {
	if n == nil || n.Kind != KindSequence {
		return nil
	}
	var out []Mount
	for _, item := range n.Items {
		switch item.Kind {
		case KindScalar:
			if m, ok := parseShortMount(expand(item.Value), dir); ok {
				out = append(out, m)
			}
		case KindMapping:
			m := Mount{
				Source: expand(item.Get("source").Str()),
				Target: expand(item.Get("target").Str()),
				Raw:    expand(item.Get("source").Str()) + ":" + expand(item.Get("target").Str()),
			}
			if item.Get("read_only").Str() == "true" {
				m.Mode = "ro"
			}
			if item.Get("type").Str() == "volume" {
				m.Named = true
			} else if m.Source != "" {
				m.Source = resolvePath(dir, m.Source)
			}
			if m.Target != "" {
				out = append(out, m)
			}
		}
	}
	return out
}

// parseShortMount reads "source:target[:mode]".
//
// The target is always an absolute path in a container, so the split is found
// by looking for the colon that precedes a leading slash rather than by
// counting colons: a source can itself contain them.
func parseShortMount(raw string, dir string) (Mount, bool) {
	if raw == "" {
		return Mount{}, false
	}
	parts := strings.Split(raw, ":")
	switch len(parts) {
	case 1:
		// An anonymous volume: just a container path.
		return Mount{Target: parts[0], Named: true, Raw: raw}, true
	case 2:
		m := Mount{Source: parts[0], Target: parts[1], Raw: raw}
		m.Named = !isPathish(parts[0])
		if !m.Named {
			m.Source = resolvePath(dir, m.Source)
		}
		return m, true
	default:
		m := Mount{
			Source: strings.Join(parts[:len(parts)-2], ":"),
			Target: parts[len(parts)-2],
			Mode:   parts[len(parts)-1],
			Raw:    raw,
		}
		m.Named = !isPathish(m.Source)
		if !m.Named {
			m.Source = resolvePath(dir, m.Source)
		}
		return m, true
	}
}

// isPathish distinguishes a bind mount source from a named volume. Compose
// uses the same rule: anything starting with / . or ~ is a path.
func isPathish(s string) bool {
	return strings.HasPrefix(s, "/") || strings.HasPrefix(s, ".") || strings.HasPrefix(s, "~")
}

func readPorts(n *Node, expand func(string) string) []Port {
	if n == nil || n.Kind != KindSequence {
		return nil
	}
	var out []Port
	for _, item := range n.Items {
		switch item.Kind {
		case KindScalar:
			if p, ok := parseShortPort(expand(item.Value)); ok {
				out = append(out, p)
			}
		case KindMapping:
			p := Port{
				Host:      expand(item.Get("published").Str()),
				Container: expand(item.Get("target").Str()),
				Proto:     expand(item.Get("protocol").Str()),
			}
			p.Raw = p.Host + ":" + p.Container
			if p.Container != "" {
				out = append(out, p)
			}
		}
	}
	return out
}

func parseShortPort(raw string) (Port, bool) {
	if raw == "" {
		return Port{}, false
	}
	p := Port{Raw: raw}
	body := raw
	if idx := strings.LastIndex(body, "/"); idx >= 0 {
		p.Proto = body[idx+1:]
		body = body[:idx]
	}
	parts := strings.Split(body, ":")
	switch len(parts) {
	case 1:
		p.Container = parts[0]
	case 2:
		p.Host, p.Container = parts[0], parts[1]
	default:
		p.HostIP = strings.Join(parts[:len(parts)-2], ":")
		p.Host, p.Container = parts[len(parts)-2], parts[len(parts)-1]
	}
	if p.Container == "" {
		return Port{}, false
	}
	return p, true
}

// resolvePath turns a compose-relative path into an absolute one, in the
// coordinates of the machine running Docker.
func resolvePath(dir, p string) string {
	if p == "" {
		return ""
	}
	if strings.HasPrefix(p, "${") || strings.Contains(p, "${") {
		// Left unresolved on purpose; a warning already says so.
		return p
	}
	if filepath.IsAbs(p) {
		return filepath.Clean(p)
	}
	return filepath.Clean(filepath.Join(dir, p))
}
