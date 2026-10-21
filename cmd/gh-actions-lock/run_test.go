package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
