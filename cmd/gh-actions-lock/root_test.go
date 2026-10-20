package main

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	parserlock "github.com/github/actions-lockfile/go/pkg/lockfile"
	"github.com/github/gh-actions-lock/internal/ghapi/httpmock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/github/gh-actions-lock/internal/pinpool"
	"github.com/github/gh-actions-lock/internal/resolve"
)

func TestNewRootCmdSuppressesCobraUsageForHandledErrors(t *testing.T) {
	cmd := newRootCmd(nil)
	if !cmd.SilenceUsage || !cmd.SilenceErrors {
		t.Fatalf("expected root command to suppress Cobra usage/errors for handled failures")
	}
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

	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if stderr.Len() != 0 {
		t.Fatalf("expected empty stderr, got %q", stderr.String())
	}

	// We check the output shape ("actions-lock version <value>\n") instead of
	// comparing with cliVersion(). cliVersion() reads build info that can change
	// across build setups, and calling it here would only repeat the code.
	const prefix = "actions-lock version "
	out := stdout.String()
	if !strings.HasPrefix(out, prefix) || !strings.HasSuffix(out, "\n") {
		t.Fatalf("unexpected output format: %q", out)
	}
	version := strings.TrimSuffix(strings.TrimPrefix(out, prefix), "\n")
	if version == "" {
		t.Fatalf("expected non-empty version in stdout: %q", out)
	}
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

const nodeActionYAML = "name: Test Action\nruns:\n  using: node20\n"

func testRepoResponse(nameWithOwner, oid, actionYAML string) map[string]any {
	return map[string]any{
		"nameWithOwner": nameWithOwner,
		// commit, tag and head answer the parity query from the same fixture.
		"commit":  map[string]any{"oid": oid},
		"tag":     map[string]any{"oid": oid},
		"tagRef":  map[string]any{"oid": oid},
		"head":    map[string]any{"oid": oid},
		"headRef": map[string]any{"oid": oid},
		"object": map[string]any{
			"oid": oid,
			"file": map[string]any{
				"object": map[string]any{
					"text": actionYAML,
				},
			},
			"fileYaml": nil,
		},
	}
}

// registerParity stubs one parity query that reports every pin intact.
func registerParity(reg *httpmock.Registry) {
	reg.Register(httpmock.GraphQL(`commit: object\(oid`), parityOK)
}

// parityOK answers a parity query as if every locked pin still matches.
func parityOK(req *http.Request) (*http.Response, error) {
	var body struct {
		Variables map[string]any `json:"variables"`
	}
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
		return nil, err
	}
	data := map[string]any{}
	for i := 0; ; i++ {
		owner, ok := body.Variables[fmt.Sprintf("owner%d", i)].(string)
		if !ok {
			break
		}
		oid := map[string]any{"oid": body.Variables[fmt.Sprintf("oid%d", i)]}
		data[fmt.Sprintf("a%d", i)] = map[string]any{
			"nameWithOwner": owner + "/" + body.Variables[fmt.Sprintf("name%d", i)].(string),
			"commit":        oid,
			"tag":           oid,
			"tagRef":        oid,
			"head":          oid,
			"headRef":       oid,
		}
	}
	return httpmock.JSONResponse(map[string]any{"data": data})(req)
}

// parityFallback answers parity queries the test did not stub as intact,
// since fresh pins fail closed on an inconclusive check.
type parityFallback struct{ rt http.RoundTripper }

func (p parityFallback) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Body == nil || !httpmock.GraphQL(`commit: object\(oid`)(req) {
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
	return parityOK(req)
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
	if strings.Contains(body, "\ndependencies:") {
		t.Fatalf("writeTempWorkflow: workflow body contains embedded `dependencies:` block; pass pins as the variadic argument instead")
	}

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
		if !ok {
			t.Fatalf("writeTempLockfile: invalid pin %q", key)
		}
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
