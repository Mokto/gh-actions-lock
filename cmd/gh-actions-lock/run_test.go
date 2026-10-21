package main

import (
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	parserlock "github.com/github/actions-lockfile/go/pkg/lockfile"
	"github.com/github/gh-actions-lock/cmd/gh-actions-lock/format"
	"github.com/github/gh-actions-lock/internal/ghapi/httpmock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRun_CooldownWarningsOnlyOnFixRuns(t *testing.T) {
	for _, mode := range []struct {
		name     string
		args     []string
		readOnly bool
	}{
		{"resolve", nil, false},
		{"relock", []string{"--relock"}, false},
		{"accept-moved", []string{"--accept-moved"}, false},
		{"no-fix", []string{"--no-fix"}, true},
		{"verify", []string{"--verify"}, true},
		{"verify-local", []string{"--verify-local"}, true},
		{"no-fix-relock", []string{"--no-fix", "--relock"}, true},
		{"verify-relock", []string{"--verify", "--relock"}, true},
		{"no-fix-accept-moved", []string{"--no-fix", "--accept-moved"}, true},
	} {
		for _, jsonOut := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/json=%v", mode.name, jsonOut), func(t *testing.T) {
				const sha = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
				pins := []string{}
				if mode.readOnly {
					pins = append(pins, "example/action@main=sha1-"+sha)
				}
				path := writeTempWorkflow(t, `
name: ci
on: push
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: example/action@main
`, pins...)
				t.Setenv("GH_ACTIONS_LOCK_CONFIG", filepath.Join(t.TempDir(), "none.yml"))
				require.NoError(t, os.WriteFile(filepath.Join(".github", "dependabot.yml"), []byte(`
version: 2
updates:
  - package-ecosystem: github-actions
    directory: /
    cooldown:
      default-days: 7
      exclude: ["example/*"]
`), 0o644))
				before, err := os.ReadFile(path)
				require.NoError(t, err)
				lockBefore := ""
				if mode.readOnly {
					lockBefore = readTempLockfilePins(t)
				}

				reg := &httpmock.Registry{}
				registerLockedManifests(reg, nil)
				if mode.name != "verify-local" {
					reg.Register(httpmock.GraphQLForRepo("example", "action"), httpmock.JSONResponse(map[string]any{
						"data": map[string]any{"a0": testRepoResponse("example/action", sha, nodeActionYAML)},
					}))
				}
				if !mode.readOnly {
					reg.Register(httpmock.REST("GET", "repos/example/action$"), httpmock.JSONResponse(map[string]any{
						"id": 2, "owner": map[string]any{"id": 1}, "default_branch": "main",
					}))
				}
				args := append([]string{"--no-interactive"}, mode.args...)
				if jsonOut {
					args = append(args, "--json=valid,findings")
				}
				stdout, stderr, err := runCommandWithHTTP(t, reg, append(args, path)...)
				require.NoError(t, err, "stdout:\n%s\nstderr:\n%s", stdout, stderr)
				const warning = "Dependabot cooldown include/exclude filters are not supported and were ignored"
				if jsonOut {
					var payload struct {
						Valid    bool             `json:"valid"`
						Findings []format.Finding `json:"findings"`
					}
					require.NoError(t, json.Unmarshal([]byte(stdout), &payload))
					if mode.readOnly {
						assert.True(t, payload.Valid)
						assert.Empty(t, payload.Findings)
					} else {
						require.Len(t, payload.Findings, 2)
						assert.Equal(t, "cooldown-config-ignored", payload.Findings[0].Category)
						assert.Equal(t, "warning", payload.Findings[0].Severity)
						assert.Equal(t, warning, payload.Findings[0].Detail)
						assert.Equal(t, "not-pinned", payload.Findings[1].Category)
					}
				} else if mode.readOnly {
					assert.NotContains(t, stdout+stderr, warning)
				} else {
					assert.Contains(t, stdout+stderr, warning)
				}
				if mode.readOnly {
					after, err := os.ReadFile(path)
					require.NoError(t, err)
					assert.Equal(t, before, after)
					assert.Equal(t, lockBefore, readTempLockfilePins(t))
				} else {
					assert.Contains(t, readTempLockfilePins(t), sha)
				}
				reg.Verify(t)
			})
		}
	}
}

func TestRun_ParitySSOGuidance(t *testing.T) {
	const message = "Resource protected by organization SAML enforcement. You must grant your Personal Access token access to this organization."
	for _, mode := range []struct {
		name string
		args []string
	}{
		{"fix", nil},
		{"no-fix", []string{"--no-fix"}},
		{"verify", []string{"--verify"}},
	} {
		t.Run(mode.name, func(t *testing.T) {
			t.Setenv("CODESPACES", "")
			reg := &httpmock.Registry{}
			reg.Register(httpmock.GraphQL(`commit: object\(oid`), func(req *http.Request) (*http.Response, error) {
				resp, err := httpmock.JSONResponse(map[string]string{"message": message})(req)
				if err != nil {
					return nil, err
				}
				resp.StatusCode = http.StatusForbidden
				return resp, nil
			})
			reg.Register(httpmock.REST("GET", `^/repos/krzema12/github-actions-typing$`), httpmock.StatusResponse(http.StatusNotFound))
			path := writeTempWorkflow(t, transferWorkflow, "krzema12/github-actions-typing@v2.2.2=sha1-"+parityTestSHA)
			lockBefore := readTempLockfilePins(t)

			_, stderr, err := runCommandWithHTTP(t, reg, append(mode.args, "--no-interactive", path)...)
			require.ErrorIs(t, err, errSilent)
			assert.Contains(t, stderr, "GitHub returned 403 Forbidden: "+message)
			assert.NotContains(t, stderr, "https://api.github.com/")
			assert.Equal(t, 1, strings.Count(stderr, message))
			if mode.name != "fix" {
				assert.Equal(t, 1, strings.Count(stderr, "gh auth refresh -h github.com"))
			}
			assert.Equal(t, lockBefore, readTempLockfilePins(t))
			reg.Verify(t)
		})
	}
}

func TestRun_ParitySSORateLimitedGuidance(t *testing.T) {
	const message = "Resource protected by organization SAML enforcement."
	for _, mode := range []struct {
		name string
		args []string
	}{
		{"fix", nil},
		{"no-fix", []string{"--no-fix"}},
		{"verify", []string{"--verify"}},
	} {
		t.Run(mode.name, func(t *testing.T) {
			t.Setenv("CODESPACES", "true")
			reg := &httpmock.Registry{}
			reg.Register(httpmock.GraphQL(`commit: object\(oid`), httpmock.JSONResponse(map[string]any{
				"data": map[string]any{"a0": map[string]any{
					"nameWithOwner": "krzema12/github-actions-typing",
					"databaseId":    1,
					"commit":        map[string]any{"oid": parityTestSHA},
					"tag":           map[string]any{"oid": parityTestMoved},
					"tagRef":        map[string]any{"oid": parityTestMoved},
				}},
			}))
			reg.RegisterReusable(httpmock.REST("HEAD", `^/orgs/krzema12$`), httpmock.StatusResponse(http.StatusOK))
			reg.Register(httpmock.REST("GET", `^/repos/krzema12/github-actions-typing/compare/`), func(req *http.Request) (*http.Response, error) {
				resp, err := httpmock.JSONResponse(map[string]string{"message": message})(req)
				if err != nil {
					return nil, err
				}
				resp.StatusCode = http.StatusForbidden
				return resp, nil
			})
			reg.Register(httpmock.REST("GET", `^/repos/krzema12/github-actions-typing/compare/`), httpmock.StatusResponse(http.StatusTooManyRequests))
			useDefaultTransport(t, reg)
			path := writeTempWorkflow(t, transferWorkflow, "krzema12/github-actions-typing@v2.2.2=sha1-"+parityTestSHA)
			lockBefore := readTempLockfilePins(t)

			_, stderr, err := runCommandWithHTTP(t, reg, append(mode.args, "--no-interactive", path)...)
			require.ErrorIs(t, err, errSilent)
			assert.Contains(t, stderr, "SSO authorization required")
			assert.Contains(t, stderr, "Authorize it at https://github.com/orgs/krzema12/sso and retry")
			assert.Contains(t, stderr, "anonymous fallback for public repositories was rate-limited")
			assert.NotContains(t, stderr, "https://api.github.com/")
			assert.Equal(t, 1, strings.Count(stderr, "Authorize it at"))
			if mode.name != "fix" {
				assert.Equal(t, 1, strings.Count(stderr, "Your codespace's GITHUB_TOKEN cannot be SSO-authorized."))
			}
			assert.Equal(t, lockBefore, readTempLockfilePins(t))
			reg.Verify(t)
		})
	}
}

func composite(uses ...string) string {
	var b strings.Builder
	b.WriteString("runs:\n  using: composite\n  steps:\n")
	for _, u := range uses {
		b.WriteString("    - uses: " + u + "\n")
	}
	return b.String()
}

// writeManifestRepo writes one workflow per name using its refs, and a
// lockfile recording each entry with the given `uses:`. Entry commits are
// distinct; each workflow records the entries its refs name.
func writeManifestRepo(t *testing.T, workflows map[string][]string, entries map[string][]string) {
	t.Helper()
	dir := t.TempDir()
	wfDir := filepath.Join(dir, ".github", "workflows")
	require.NoError(t, os.MkdirAll(wfDir, 0o755))
	var lock strings.Builder
	lock.WriteString("version: '" + parserlock.Version + "'\nworkflows:\n")
	for _, name := range slices.Sorted(maps.Keys(workflows)) {
		body := "on: push\njobs:\n  test:\n    runs-on: ubuntu-latest\n    steps:\n"
		lock.WriteString("    '.github/workflows/" + name + "':\n")
		var keys []string
		for _, ref := range workflows[name] {
			body += "      - uses: " + ref + "\n"
			r := parserlock.ParseActionRef(ref)
			if k := r.NWO() + "@" + r.Ref; !slices.Contains(keys, k) {
				keys = append(keys, k)
				lock.WriteString("        - '" + k + "'\n")
			}
		}
		require.NoError(t, os.WriteFile(filepath.Join(wfDir, name), []byte(body), 0o600))
	}
	lock.WriteString("dependencies:\n")
	for i, key := range slices.Sorted(maps.Keys(entries)) {
		_, ref, _ := strings.Cut(key, "@")
		if parserlock.IsFullSha(ref) {
			ref = "v0"
		}
		fmt.Fprintf(&lock, "    '%s':\n        ref: '%s'\n        commit: 'sha1-%s'\n        owner_id: 1\n        repo_id: %d\n", key, ref, entrySHA(i, key), i+1)
		if len(entries[key]) > 0 {
			lock.WriteString("        uses:\n")
			for _, u := range entries[key] {
				lock.WriteString("            - '" + u + "'\n")
			}
		}
	}
	require.NoError(t, os.WriteFile(filepath.Join(wfDir, "actions.lock"), []byte(lock.String()), 0o600))
	t.Chdir(dir)
}

// entrySHA is entry i's commit; a full-SHA key keeps its own.
func entrySHA(i int, key string) string {
	if _, ref, _ := strings.Cut(key, "@"); parserlock.IsFullSha(ref) {
		return strings.ToLower(ref)
	}
	return fmt.Sprintf("%040x", i+1)
}

const childSHA = "ABCDEF0123456789ABCDEF0123456789ABCDEF01"

func TestRun_LockedManifests(t *testing.T) {
	for name, tc := range map[string]struct {
		workflow  []string
		entries   map[string][]string
		manifests map[string]string
		// blocked is the detail of the blocking finding; "" means valid.
		blocked string
	}{
		"recorded edges": {
			workflow:  []string{"acme/comp@v1"},
			entries:   map[string][]string{"acme/comp@v1": {"acme/child@v2"}, "acme/child@v2": nil},
			manifests: map[string]string{"acme/comp": composite("acme/child@v2", "./local", "docker://alpine")},
		},
		"erased edge": {
			workflow:  []string{"acme/comp@v1"},
			entries:   map[string][]string{"acme/comp@v1": nil, "acme/child@v2": nil},
			manifests: map[string]string{"acme/comp": composite("acme/child@v2")},
			blocked:   "acme/comp@v1 at locked commit 000000000000 uses acme/child@v2, but its lockfile entry acme/comp@v1 does not record it",
		},
		"erased nested edge": {
			workflow: []string{"acme/comp@v1"},
			entries:  map[string][]string{"acme/comp@v1": {"acme/mid@v1"}, "acme/mid@v1": nil, "acme/leaf@v1": nil},
			manifests: map[string]string{
				"acme/comp": composite("acme/mid@v1"),
				"acme/mid":  composite("acme/leaf@v1"),
			},
			blocked: "acme/mid@v1 at locked commit 000000000000 uses acme/leaf@v1",
		},
		"sub-action reached from a sibling": {
			workflow: []string{"acme/tools/a@v1"},
			entries:  map[string][]string{"acme/tools@v1": {"acme/x@v1"}, "acme/x@v1": nil},
			manifests: map[string]string{
				"acme/tools/a": composite("$/b", "acme/x@v1"),
				"acme/tools/b": composite("acme/y@v1"),
			},
			blocked: "acme/tools/b@v1 at locked commit 000000000000 uses acme/y@v1",
		},
		"full SHA in another case": {
			workflow:  []string{"acme/comp@v1"},
			entries:   map[string][]string{"acme/comp@v1": {"acme/child@" + strings.ToLower(childSHA)}, "acme/child@" + strings.ToLower(childSHA): nil},
			manifests: map[string]string{"acme/comp": composite("acme/child@" + childSHA)},
		},
		"missing manifest": {
			workflow:  []string{"acme/comp/gone@v1"},
			entries:   map[string][]string{"acme/comp@v1": nil},
			manifests: map[string]string{"acme/comp/gone": ""},
			blocked:   "acme/comp/gone@v1 has no action.yml or action.yaml at locked commit 000000000000",
		},
	} {
		for _, mode := range [][]string{{"--verify"}, {"--no-fix"}, {}} {
			t.Run(name+"/"+strings.Join(append([]string{"default"}, mode...)[min(len(mode), 1):], ""), func(t *testing.T) {
				writeManifestRepo(t, map[string][]string{"ci.yml": tc.workflow}, tc.entries)
				before, err := os.ReadFile(".github/workflows/actions.lock")
				require.NoError(t, err)
				reg := &httpmock.Registry{}
				registerLockedManifests(reg, tc.manifests)

				p, err := runParity(t, reg, mode...)
				if tc.blocked == "" {
					require.NoError(t, err)
					assert.True(t, p.Valid, "%+v", p.Findings)
					return
				}
				require.ErrorIs(t, err, errSilent)
				assert.False(t, p.Valid)
				var details []string
				for _, f := range p.Findings {
					if f.Severity == "error" {
						details = append(details, f.Detail)
					}
				}
				require.Len(t, details, 1, "%+v", p.Findings)
				assert.Contains(t, details[0], tc.blocked)
				after, err := os.ReadFile(".github/workflows/actions.lock")
				require.NoError(t, err)
				assert.Equal(t, string(before), string(after), "a blocked workflow is not written")
			})
		}
	}
}

// TestRun_LockedManifestsOneBatchPerLevel: reads are batched across
// workflows, one request per composite nesting level.
func TestRun_LockedManifestsOneBatchPerLevel(t *testing.T) {
	writeManifestRepo(t, map[string][]string{"a.yml": {"acme/a@v1"}, "b.yml": {"acme/b@v1"}},
		map[string][]string{"acme/a@v1": {"acme/leaf@v1"}, "acme/b@v1": {"acme/leaf@v1"}, "acme/leaf@v1": nil})
	reg := &httpmock.Registry{}
	reads := registerLockedManifests(reg, map[string]string{"acme/a": composite("acme/leaf@v1"), "acme/b": composite("acme/leaf@v1")})

	p, err := runParity(t, reg, "--verify")
	require.NoError(t, err)
	assert.True(t, p.Valid, "%+v", p.Findings)
	assert.EqualValues(t, 2, reads.Load(), "the roots, then their shared child")
}

// TestRun_LockedManifestsUnderRelock: a read-only relock leaves the lockfile
// as recorded, so it is checked; a writable one re-resolves from live
// manifests and reads none at locked commits.
func TestRun_LockedManifestsUnderRelock(t *testing.T) {
	manifests := map[string]string{"acme/comp": composite("acme/child@v2")}
	for _, tc := range []struct {
		args  []string
		reads int64
	}{
		{[]string{"--verify", "--relock"}, 1},
		{[]string{"--no-fix", "--accept-moved"}, 1},
		{[]string{"--relock"}, 0},
	} {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			writeManifestRepo(t, map[string][]string{"ci.yml": {"acme/comp@v1"}},
				map[string][]string{"acme/comp@v1": nil, "acme/child@v2": nil})
			reg := &httpmock.Registry{}
			reads := registerLockedManifests(reg, manifests)
			registerLive(reg, manifests, map[string]string{"acme/comp": entrySHA(1, ""), "acme/child": entrySHA(0, "")})

			p, err := runParity(t, reg, append(tc.args, "--no-narrow")...)
			assert.Equal(t, tc.reads, reads.Load())
			if tc.reads == 0 {
				require.NoError(t, err, "%+v", p.Findings)
				return
			}
			require.ErrorIs(t, err, errSilent)
			assert.Contains(t, fmt.Sprint(p.Findings), "does not record it")
		})
	}
}

// registerLive answers live resolution of each owner/repo at commits.
func registerLive(reg *httpmock.Registry, manifests, commits map[string]string) {
	reg.RegisterReusable(httpmock.GraphQL(`file: file\(path`), actionFiles(manifests, commits))
	reg.RegisterReusable(httpmock.REST("GET", `/(tags|releases)$`), httpmock.JSONResponse([]any{}))
}

// TestRun_LockedManifestsBesideAFix: a workflow whose other pins a default
// run would fix is still checked, and the damage blocks the whole write.
func TestRun_LockedManifestsBesideAFix(t *testing.T) {
	writeManifestRepo(t, map[string][]string{"ci.yml": {"acme/comp@v1"}},
		map[string][]string{"acme/comp@v1": nil, "acme/child@v2": nil})
	require.NoError(t, os.WriteFile(".github/workflows/ci.yml", []byte("on: push\njobs:\n  test:\n    runs-on: ubuntu-latest\n    steps:\n      - uses: acme/comp@v1\n      - uses: acme/new@v1\n"), 0o600))
	before, err := os.ReadFile(".github/workflows/actions.lock")
	require.NoError(t, err)
	reg := &httpmock.Registry{}
	registerLockedManifests(reg, map[string]string{"acme/comp": composite("acme/child@v2")})
	registerLive(reg, nil, map[string]string{"acme/new": entrySHA(9, "")})

	p, err := runParity(t, reg, "--no-narrow")
	require.ErrorIs(t, err, errSilent)
	assert.Contains(t, fmt.Sprint(p.Findings), "does not record it")
	after, err := os.ReadFile(".github/workflows/actions.lock")
	require.NoError(t, err)
	assert.Equal(t, string(before), string(after))
}

// TestRun_LockedManifestsSeededRoot: a workflow newly using an action
// another workflow locked takes it from the lockfile without a live read,
// so its manifest is checked before the entry is reused.
func TestRun_LockedManifestsSeededRoot(t *testing.T) {
	writeManifestRepo(t, map[string][]string{"a.yml": {"acme/comp@v1"}},
		map[string][]string{"acme/comp@v1": nil, "acme/child@v2": nil})
	require.NoError(t, os.WriteFile(".github/workflows/b.yml", []byte("on: push\njobs:\n  test:\n    runs-on: ubuntu-latest\n    steps:\n      - uses: acme/comp@v1\n"), 0o600))
	before, err := os.ReadFile(".github/workflows/actions.lock")
	require.NoError(t, err)
	reg := &httpmock.Registry{}
	registerLockedManifests(reg, map[string]string{"acme/comp": composite("acme/child@v2")})

	p, err := runParity(t, reg, ".github/workflows/b.yml")
	require.ErrorIs(t, err, errSilent)
	require.NotEmpty(t, p.Findings)
	assert.Contains(t, p.Findings[len(p.Findings)-1].Detail, "uses acme/child@v2, but its lockfile entry acme/comp@v1 does not record it")
	after, err := os.ReadFile(".github/workflows/actions.lock")
	require.NoError(t, err)
	assert.Equal(t, string(before), string(after))
}

// TestRun_LockedManifestsInconclusive: a failed read warns, as an
// inconclusive parity check does, except behind the Dependabot proxy,
// which aborts so the update is retried.
func TestRun_LockedManifestsInconclusive(t *testing.T) {
	for _, proxy := range []bool{false, true} {
		t.Run(fmt.Sprint("proxy=", proxy), func(t *testing.T) {
			writeManifestRepo(t, map[string][]string{"ci.yml": {"acme/comp@v1"}}, map[string][]string{"acme/comp@v1": nil})
			reg := &httpmock.Registry{}
			reg.RegisterReusable(lockedManifestRead, httpmock.StatusResponse(http.StatusBadGateway))
			if proxy {
				t.Setenv("GH_ACTIONS_LOCK_DEPENDABOT_PROXY", "1")
				useDefaultTransport(t, reg)
				reg.Register(httpmock.REST("GET", `^/repos/acme/comp$`), httpmock.JSONResponse(map[string]any{"full_name": "acme/comp", "id": 1, "owner": map[string]any{"id": 1}}))
				reg.Register(httpmock.REST("GET", `^/repos/acme/comp/git/ref/tags/v1$`), httpmock.JSONResponse(httpmock.GitRefResponse("refs/tags/v1", entrySHA(0, ""))))
				reg.RegisterReusable(httpmock.REST("GET", `/commits/`), httpmock.StatusResponse(http.StatusBadGateway))
			}

			stdout, stderr, err := runCommandWithHTTP(t, reg, "--verify", "--json=valid,findings")
			if proxy {
				require.Error(t, err)
				assert.NotErrorIs(t, err, errSilent)
				assert.Contains(t, err.Error()+stderr, "could not read locked manifest acme/comp@v1")
				return
			}
			require.NoError(t, err, stderr)
			var p parityPayload
			require.NoError(t, json.Unmarshal([]byte(stdout), &p), stdout)
			require.Len(t, p.Findings, 1)
			assert.Equal(t, "reachability-unknown", p.Findings[0].Category)
			assert.Equal(t, "warning", p.Findings[0].Severity)
		})
	}
}
