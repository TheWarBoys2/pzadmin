package server

import (
	"testing"
	"time"
)

func TestExpectedOutage(t *testing.T) {
	justStarted := At(time.Now().Add(-2 * time.Minute))
	longAgo := At(time.Now().Add(-startupGrace - time.Minute))
	cases := []struct {
		name            string
		kind            string
		st              Status
		quiet, starting bool
	}{
		{"exited container", "refused", Status{ContainerState: "exited"}, true, false},
		{"never created", "refused", Status{ContainerState: "not created"}, true, false},
		{"stopped by PZAdmin", "timeout", Status{Stopped: true, ContainerState: "running"}, true, false},
		{"booting", "refused", Status{ContainerState: "running", StartedAt: justStarted}, true, true},
		{"up but RCON silent", "refused", Status{ContainerState: "running", StartedAt: longAgo}, false, false},
		{"no container configured", "refused", Status{}, false, false},
		{"Arcane unreachable", "refused", Status{ContainerState: "unavailable"}, false, false},
		{"wrong password while stopped", "auth", Status{ContainerState: "exited"}, false, false},
		{"wrong password while booting", "auth", Status{ContainerState: "running", StartedAt: justStarted}, false, false},
	}
	for _, c := range cases {
		quiet, starting := expectedOutage(c.kind, c.st)
		if quiet != c.quiet || starting != c.starting {
			t.Errorf("%s: got quiet=%v starting=%v, want quiet=%v starting=%v",
				c.name, quiet, starting, c.quiet, c.starting)
		}
	}
}
