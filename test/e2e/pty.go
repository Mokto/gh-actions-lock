//go:build !windows

package e2e

import (
	"bytes"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/creack/pty"
	"github.com/github/gh-actions-lock/test/scenarios"
	"github.com/stretchr/testify/require"
)

// runPTY plays the terminal: it answers cursor-position queries and types
// each response once its prompt has rendered and output has settled.
func runPTY(t *testing.T, cmd *exec.Cmd, prompts []scenarios.Prompt) result {
	f, err := pty.Start(cmd)
	require.NoError(t, err)
	defer f.Close()

	chunks := make(chan []byte)
	go func() {
		defer close(chunks)
		buf := make([]byte, 4096)
		for {
			n, err := f.Read(buf)
			if n > 0 {
				chunks <- bytes.Clone(buf[:n])
			}
			if err != nil {
				return
			}
		}
	}()

	var out strings.Builder
	var settled <-chan time.Time
	answered := 0
	for chunks != nil {
		select {
		case c, ok := <-chunks:
			if !ok {
				chunks = nil
				break
			}
			out.Write(c)
			if bytes.Contains(c, []byte("\x1b[6n")) {
				_, _ = f.WriteString("\x1b[1;1R")
			}
			if len(prompts) > 0 && strings.Contains(ansiEscape.ReplaceAllString(out.String()[answered:], ""), prompts[0].Prompt) {
				settled = time.After(300 * time.Millisecond)
			}
		case <-settled:
			_, _ = f.WriteString(prompts[0].Response + "\r")
			prompts, settled, answered = prompts[1:], nil, out.Len()
		}
	}
	all := out.String()
	return result{exit: exitCode(t, cmd.Wait()), stdout: all, output: all}
}
