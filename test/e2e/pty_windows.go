package e2e

import (
	"os/exec"
	"testing"

	"github.com/github/gh-actions-lock/test/scenarios"
)

// runPTY is never reached on Windows: Run skips interactive scenarios there.
func runPTY(t *testing.T, _ *exec.Cmd, _ []scenarios.Prompt) result {
	t.Skip("interactive scenarios need a PTY, which Windows lacks")
	return result{}
}
