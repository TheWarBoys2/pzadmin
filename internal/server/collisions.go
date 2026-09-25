package server

import (
	"fmt"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/TheWarBoys2/pzadmin/internal/config"
	"github.com/TheWarBoys2/pzadmin/internal/pz"
)

// Two servers configured with the same RCON address are the same game server
// as far as RCON is concerned. PZAdmin then probes it twice, gets the same
// player list back, and reports everyone as online on both — which reads as a
// bug in PZAdmin rather than as a configuration collision.
//
// The same applies to two servers pointed at one directory, or at one Docker
// container: the first is harmless until you edit a config, the second means a
// restart hits the wrong server.

// fleetWarnings compares every configured server against every other. It reads
// nothing from disk, so it is cheap enough to run on every status push.
func fleetWarnings(cfg config.Config) map[string][]string {
	out := map[string][]string{}
	add := func(id, msg string) { out[id] = append(out[id], msg) }

	byAddress := map[string][]config.Server{}
	byPath := map[string][]config.Server{}
	byContainer := map[string][]config.Server{}

	for _, s := range cfg.Servers {
		if !s.Enabled {
			continue
		}
		address := strings.ToLower(s.Host) + ":" + strconv.Itoa(s.RCONPort)
		byAddress[address] = append(byAddress[address], s)
		if s.PZPath != "" {
			byPath[filepath.Clean(s.PZPath)] = append(byPath[filepath.Clean(s.PZPath)], s)
		}
		if s.DockerContainer != "" {
			key := strings.ToLower(s.DockerContainer)
			byContainer[key] = append(byContainer[key], s)
		}
	}

	for address, group := range byAddress {
		if len(group) < 2 {
			continue
		}
		for _, s := range group {
			add(s.ID, fmt.Sprintf(
				"This server and %s are both set to RCON %s, so PZAdmin is talking to one "+
					"game server twice. Whoever is playing will appear on both. Give each server "+
					"its own RCONPort in its .ini and update it here.",
				othersNamed(group, s.ID), address))
		}
	}

	for path, group := range byPath {
		if len(group) < 2 {
			continue
		}
		for _, s := range group {
			add(s.ID, fmt.Sprintf(
				"This server and %s both point at %s. Editing config, mods or backups for one "+
					"will change the other.", othersNamed(group, s.ID), path))
		}
	}

	for _, group := range byContainer {
		if len(group) < 2 {
			continue
		}
		for _, s := range group {
			add(s.ID, fmt.Sprintf(
				"This server and %s are both set to the container %s, so restarting one restarts "+
					"the other.", othersNamed(group, s.ID), s.DockerContainer))
		}
	}
	return out
}

func othersNamed(group []config.Server, exclude string) string {
	var names []string
	for _, s := range group {
		if s.ID != exclude {
			names = append(names, s.Name)
		}
	}
	switch len(names) {
	case 0:
		return "another server"
	case 1:
		return names[0]
	default:
		return strings.Join(names[:len(names)-1], ", ") + " and " + names[len(names)-1]
	}
}

// rconMismatch compares what PZAdmin is configured to dial against what the
// server's own .ini says it listens on. A mismatch means PZAdmin is managing a
// different server than the files it is showing you.
func rconMismatch(s config.Server, layout pz.Layout) string {
	if layout.ConfigDir == "" {
		return ""
	}
	files, err := pz.ListConfigFiles(layout.ConfigDir)
	if err != nil {
		return ""
	}
	for _, f := range files {
		if f.Kind != "ini" {
			continue
		}
		ini, err := pz.LoadINI(filepath.Join(layout.ConfigDir, f.Name))
		if err != nil {
			continue
		}
		raw, ok := ini.Get("RCONPort")
		if !ok {
			continue
		}
		port, err := strconv.Atoi(strings.TrimSpace(raw))
		if err != nil || port == 0 {
			continue
		}
		if port != s.RCONPort {
			return fmt.Sprintf(
				"PZAdmin connects on port %d, but this server's %s says RCONPort=%d. "+
					"Either PZAdmin is managing a different server than the files shown here, "+
					"or the port here is wrong.", s.RCONPort, f.Name, port)
		}
		return ""
	}
	return ""
}
