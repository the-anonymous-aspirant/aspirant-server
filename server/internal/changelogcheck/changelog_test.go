// Package changelogcheck runs the repository's changelog checks under
// `go test ./...` from server/, the command aspirant-server's verify runs
// (system_3 #10799-A1). It lives under server/ so that command reaches it.
// Before this, nothing ran `scripts/changelog.sh check` when a PR was made, and nine
// malformed fragments landed in a row (2026-11-12). Each one stopped
// `changelog.sh render` until it was repaired.
package changelogcheck

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// repoRoot walks up from the test's directory to the one holding go.mod.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("no go.mod above the test directory")
		}
		dir = parent
	}
}

// runSh runs a repo shell script with sh from the repo root and fails the
// test with its output when it exits non-zero. A missing sh fails too: a
// verify that cannot run the check must not read green.
func runSh(t *testing.T, script string, args ...string) {
	t.Helper()
	root := repoRoot(t)
	cmd := exec.Command("sh", append([]string{script}, args...)...)
	cmd.Dir = root
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("sh %s %v failed: %v\n%s", script, args, err, out)
	}
}

// Every fragment in docs/changelog.d is well formed: its first line is a
// `- ` bullet and every other non-empty line is a bullet or an indented
// continuation. A malformed fragment turns this test, and the verify, red.
func TestChangelogFragmentsAreWellFormed(t *testing.T) {
	runSh(t, "scripts/changelog.sh", "check")
}

// The changelog script's own unit suite, which also runs the repo check.
func TestChangelogScriptUnitSuite(t *testing.T) {
	runSh(t, "tests/changelog_unit.sh")
}
