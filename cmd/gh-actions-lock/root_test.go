package main

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	parserlock "github.com/github/actions-lockfile/go/pkg/lockfile"
	"github.com/github/gh-actions-lock/cmd/gh-actions-lock/format"
	"github.com/github/gh-actions-lock/internal/ghapi/httpmock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/github/gh-actions-lock/internal/pinpool"
	"github.com/github/gh-actions-lock/internal/resolve"
)

func TestNewRootCmdSuppressesCobraUsageForHandledErrors(t *testing.T) {
	cmd := newRootCmd(nil)
	assert.True(t, cmd.SilenceUsage)
	assert.True(t, cmd.SilenceErrors)
}

func TestNewRootCmd_Version(t *testing.T) {
	var stdout, stderr bytes.Buffer
	cmd := newRootCmd(func(string, *pinpool.Pool) (*resolve.Resolver, error) {
		t.Fatalf("unexpected resolver call: --version must skip resolution")
		return nil, nil
	})
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"--version"})

	require.NoError(t, cmd.Execute())
	assert.Empty(t, stderr.String())

	// We check the output shape ("actions-lock version <value>\n") instead of
	// comparing with cliVersion(). cliVersion() reads build info that can change
	// across build setups, and calling it here would only repeat the code.
	const prefix = "actions-lock version "
	out := stdout.String()
	require.True(t, strings.HasPrefix(out, prefix) && strings.HasSuffix(out, "\n"), "output %q", out)
	version := strings.TrimSuffix(strings.TrimPrefix(out, prefix), "\n")
	assert.NotEmpty(t, version, "output %q", out)
}

func TestCheckCommand_HelpExplainsHostAndAuthOverrides(t *testing.T) {
	cmd := newRootCmd(nil)
	var out strings.Builder
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"--help"})
	require.NoError(t, cmd.Execute())
	for _, text := range []string{
		"Host selection: --hostname, then GH_HOST",
		"GH_REPO or a remote on a host known to gh",
		"GH_TOKEN takes precedence over GITHUB_TOKEN",
		"GitHub Enterprise Server (GHES) is not supported.",
		"--hostname selects the host; it does not override token variables.",
		"env -u GH_TOKEN -u GITHUB_TOKEN gh auth status --hostname TENANT.ghe.com",
	} {
		assert.Contains(t, out.String(), text)
	}
}

// workflowUsing returns a one-job workflow whose steps use each ref in order.
func workflowUsing(uses ...string) string {
	var b strings.Builder
	b.WriteString("name: ci\non: push\njobs:\n  test:\n    runs-on: ubuntu-latest\n    steps:\n")
	for _, u := range uses {
		b.WriteString("      - uses: " + u + "\n")
	}
	return b.String()
}

// checkResult is the `--json=valid,findings` payload.
type checkResult struct {
	Valid    bool             `json:"valid"`
	Findings []format.Finding `json:"findings"`
}

// categories returns the set of finding categories.
func (c checkResult) categories() map[string]bool {
	out := map[string]bool{}
	for _, f := range c.Findings {
		out[f.Category] = true
	}
	return out
}

// runCheckJSON runs the command with --json=valid,findings and decodes the
// payload; err is the command's error.
func runCheckJSON(t *testing.T, rt http.RoundTripper, args ...string) (checkResult, error) {
	t.Helper()
	stdout, _, err := runCommandWithHTTP(t, rt, append(args, "--json=valid,findings")...)
	var c checkResult
	require.NoError(t, json.Unmarshal([]byte(stdout), &c), stdout)
	return c, err
}

// parityFallback answers parity queries the test did not stub as intact,
// since fresh pins fail closed on an inconclusive check.
type parityFallback struct{ rt http.RoundTripper }

func (p parityFallback) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Body == nil || !httpmock.ParityQuery(req) {
		return p.rt.RoundTrip(req)
	}
	body, err := io.ReadAll(req.Body)
	if err != nil {
		return nil, err
	}
	req.Body = io.NopCloser(bytes.NewReader(body))
	if resp, err := p.rt.RoundTrip(req); err == nil {
		return resp, nil
	}
	req.Body = io.NopCloser(bytes.NewReader(body))
	return httpmock.ParityIntact(req)
}

// writeTempWorkflow writes a workflow YAML body to a scratch repo at
// .github/workflows/workflow.yml and (if pins are provided) materializes the
// detached lockfile at .github/workflows/actions.lock. The fixture body must
// NOT carry an embedded `dependencies:` block — pin metadata lives in the
// detached lockfile per the per-repo dependency-pinning architecture.
// Returns the relative workflow path; the test's working directory is
// chdir'd to the scratch repo.
func writeTempWorkflow(t *testing.T, body string, pins ...string) string {
	t.Helper()

	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, ".github", "workflows"), 0o755))

	body = strings.TrimSpace(body) + "\n"
	require.NotContains(t, body, "\ndependencies:", "pass pins as arguments, not an embedded dependencies block")

	wfRel := filepath.Join(".github", "workflows", "workflow.yml")
	wfPath := filepath.Join(dir, wfRel)
	require.NoError(t, os.WriteFile(wfPath, []byte(body), 0o600))

	if len(pins) > 0 {
		writeTempLockfile(t, dir, "workflow.yml", pins)
	}

	t.Chdir(dir)
	return filepath.ToSlash(wfRel)
}

// writeTempLockfile writes a v0.0.1 actions.lock fixture covering the given
// workflow file. Owner/repo IDs are stubbed; the read path doesn't validate
// them. All user-supplied scalars are single-quoted to mirror the
// production emitter (see internal/lockfile/store.go::marshalDeterministic).
// writeTempLockfile writes a minimal lockfile for the given pins. Each
// pinString has the form "owner/repo@ref" (the v0.0.2 pin key format).
// A synthetic commit hash is generated from the pin key for each entry.
// The ref field in each action body matches the pin key's ref. Overrides
// can pass "owner/repo@ref=sha1-hex" to supply an explicit commit, and
// "owner/repo@ref=sha1-hex=ref" to record a different ref (a full-SHA key
// must record the tag or branch its commit came from).
func writeTempLockfile(t *testing.T, repoDir, wfName string, pinStrings []string) {
	t.Helper()
	var sb strings.Builder
	sb.WriteString("version: '" + parserlock.Version + "'\ndependencies:\n")
	for _, raw := range pinStrings {
		key, commit, _ := strings.Cut(raw, "=")
		commit, ref, _ := strings.Cut(commit, "=")
		pin, ok := parserlock.ParsePin(key)
		require.True(t, ok, "invalid pin %q", key)
		ref = cmp.Or(ref, pin.Ref)
		if commit == "" {
			// Generate deterministic fake commit from pin key.
			commit = "sha1-" + strings.Repeat(string("abcdef0123456789"[len(key)%16]), 40)
		}
		sb.WriteString("  '" + key + "':\n    ref: '" + ref + "'\n    commit: '" + commit + "'\n    owner_id: 1\n    repo_id: 1\n")
	}
	sb.WriteString("workflows:\n  '.github/workflows/" + wfName + "':\n")
	for _, raw := range pinStrings {
		key := raw
		if idx := strings.Index(raw, "="); idx >= 0 {
			key = raw[:idx]
		}
		sb.WriteString("    - '" + key + "'\n")
	}
	p := filepath.Join(repoDir, ".github", "workflows", "actions.lock")
	require.NoError(t, os.WriteFile(p, []byte(sb.String()), 0o600))
}

// readTempLockfilePins returns the raw pin strings from the actions.lock file
// in the current working directory. Useful for assertions in write/upgrade
// tests that previously inspected the workflow YAML directly.
func readTempLockfilePins(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(".github", "workflows", "actions.lock"))
	require.NoError(t, err)
	return string(b)
}

func runCommandWithHTTP(t *testing.T, rt http.RoundTripper, args ...string) (string, string, error) {
	t.Helper()
	return runCommandWithHTTPContext(t, context.Background(), rt, args...)
}

func runCommandWithHTTPContext(t *testing.T, ctx context.Context, rt http.RoundTripper, args ...string) (string, string, error) {
	t.Helper()

	stdoutR, stdoutW, err := os.Pipe()
	require.NoError(t, err)
	stderrR, stderrW, err := os.Pipe()
	require.NoError(t, err)

	// Drain while the command runs: a pipe buffers only a few KB on Windows,
	// so a large JSON payload would otherwise block the writer forever.
	drain := func(r io.Reader) <-chan []byte {
		ch := make(chan []byte, 1)
		go func() {
			b, _ := io.ReadAll(r)
			ch <- b
		}()
		return ch
	}
	stdoutCh, stderrCh := drain(stdoutR), drain(stderrR)

	newResolver := func(hostname string, pool *pinpool.Pool) (*resolve.Resolver, error) {
		return resolve.New(hostname, pool, resolve.WithTransport(parityFallback{rt}))
	}

	cmd := newRootCmd(newResolver)
	cmd.SetOut(stdoutW)
	cmd.SetErr(stderrW)
	cmd.SetArgs(args)
	runErr := cmd.ExecuteContext(ctx)

	_ = stdoutW.Close()
	_ = stderrW.Close()

	return string(<-stdoutCh), string(<-stderrCh), runErr
}

// ==========================================================================
// Supply Chain Attack Reachability Tests
//
// These tests model real-world attacks where tag mutation or fork-network
// injection was used to compromise GitHub Actions. The reachability check
// should catch cases where a pinned SHA exists in the GitHub fork network
// but is NOT on the canonical repository's ref lineage.
//
// References:
//   - tj-actions/changed-files (CVE-2025-30066): tag v44 pointed to malicious commit from fork
//   - reviewdog/action-setup: tag mutation via compromised PAT
//   - xygeni/xygeni-action: C2 reverse shell backdoor via tag poisoning
//   - aquasecurity/trivy-action: scanner-to-stealer tag manipulation
// ==========================================================================

func TestRootCommand_JSONExpandsDirectoryArguments(t *testing.T) {
	reg := &httpmock.Registry{}
	defer reg.Verify(t)

	dir := t.TempDir()
	first := filepath.Join(dir, "first.yml")
	second := filepath.Join(dir, "second.yaml")
	ignored := filepath.Join(dir, "notes.txt")
	workflow := []byte(strings.TrimSpace(`
name: run only
on: push
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - run: echo ok
`) + "\n")
	require.NoError(t, os.WriteFile(first, workflow, 0o600))
	require.NoError(t, os.WriteFile(second, workflow, 0o600))
	require.NoError(t, os.WriteFile(ignored, []byte("not a workflow\n"), 0o600))

	// No lockfile, so both workflows are untracked and the run fails.
	stdout, _, err := runCommandWithHTTP(t, reg, "--no-fix", "--json=valid,workflows", dir)
	require.ErrorIs(t, err, errSilent)

	var payload struct {
		Workflows []struct {
			Path string `json:"path"`
		} `json:"workflows"`
	}
	require.NoError(t, json.Unmarshal([]byte(stdout), &payload))
	require.Len(t, payload.Workflows, 2)
	assert.Equal(t, first, payload.Workflows[0].Path)
	assert.Equal(t, second, payload.Workflows[1].Path)
}

// A partial checkout without .git, with a `$/` composite outside .github,
// relocks only when GH_ACTIONS_LOCK_REPO_ROOT names the root.
func TestRun_RepoRootEnvWithoutGit(t *testing.T) {
	tests := []struct {
		name    string
		root    string // "abs" uses the temp dir path; anything else is used verbatim
		wantErr string
	}{
		{name: "absolute root locks composite deps", root: "abs"},
		{name: "relative root equal to cwd locks composite deps", root: "."},
		{name: "env unset fails without .git", wantErr: "not in a git repository"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			write := func(rel, content string) {
				full := filepath.Join(dir, rel)
				require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o755))
				require.NoError(t, os.WriteFile(full, []byte(content), 0o600))
			}
			write("tools/act/action.yml",
				"runs:\n  using: composite\n  steps:\n    - uses: actions/setup-go@v5.0.0\n")
			write(".github/workflows/ci.yml",
				"name: ci\non: push\njobs:\n  b:\n    runs-on: ubuntu-latest\n    steps:\n      - uses: $/tools/act\n")
			t.Chdir(dir)
			t.Setenv("GH_ACTIONS_LOCK_REPO_ROOT", "")
			switch tt.root {
			case "abs":
				// On macOS t.TempDir is under /var while the cwd resolves to
				// /private/var, so this also covers symlink-equivalent paths.
				t.Setenv("GH_ACTIONS_LOCK_REPO_ROOT", dir)
			case "":
			default:
				t.Setenv("GH_ACTIONS_LOCK_REPO_ROOT", tt.root)
			}
			reg := &httpmock.Registry{}
			registerSetupGo(reg)

			_, stderr, err := runCommandWithHTTP(t, reg)

			if tt.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error()+stderr, tt.wantErr)
				return
			}
			require.NoError(t, err, stderr)
			assert.Contains(t, readTempLockfilePins(t), "'actions/setup-go@v5.0.0'")
		})
	}
}
