package main

import (
	"os"
	"os/exec"
	"testing"
)

// TestFrontend runs the browser-side suite in web/testkit, which covers the
// modal stack and the guarantee that game data is never treated as markup.
//
// It needs node, which the release image's builder does not have, so it skips
// rather than fails when node is absent. CI sets CI=true, and there a missing
// node is a failure, so the suite can never be skipped without anyone noticing.
func TestFrontend(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		if os.Getenv("CI") == "true" {
			t.Fatal("node is not installed, and CI must run the frontend suite")
		}
		t.Skip("node is not installed; skipping the frontend suite")
	}
	out, err := exec.Command(node, "web/testkit/run.js").CombinedOutput()
	t.Log("\n" + string(out))
	if err != nil {
		t.Fatalf("frontend tests failed: %v", err)
	}
}
