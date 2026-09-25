package main

import (
	"os/exec"
	"testing"
)

// TestFrontend runs the browser-side suite in web/testkit, which covers the
// modal stack and the guarantee that game data is never treated as markup.
//
// It needs node, which the release image's builder does not have, so it skips
// rather than fails when node is absent. Run it locally before shipping a
// frontend change.
func TestFrontend(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed; skipping the frontend suite")
	}
	out, err := exec.Command(node, "web/testkit/run.js").CombinedOutput()
	t.Log("\n" + string(out))
	if err != nil {
		t.Fatalf("frontend tests failed: %v", err)
	}
}
