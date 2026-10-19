package main

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/github/gh-actions-lock/internal/ghapi/httpmock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestNoOnboard_BareSHAOnTrackedAction: under --no-onboard a bare SHA on an
// action the workflow already locks is a ref change, not onboarding. A SHA
// on upstream history is pinned in place of the old entry; an impostor SHA
// blocks with an error and nothing is written. Neither may drop the old pin
// and pass, which is what Dependabot SHA bumps hit.
func TestNoOnboard_BareSHAOnTrackedAction(t *testing.T) {
	const (
		oldSHA   = "eef61447b9ff4aafe5dcd4e0bbf5d482be7e7871"
		newSHA   = "11bd71901bbe5b1630ceea73d27597364c9af683"
		impostor = "11d0dfc58a242e392d9695aa26812e1d1eb8e439"
	)
	tests := []struct {
		name    string
		locked  string
		uses    string
		blocked bool
	}{
		{"sha to sha", "actions/checkout@" + oldSHA + "=sha1-" + oldSHA + "=main", newSHA, false},
		{"tag to sha", "actions/checkout@v4.2.1=sha1-" + oldSHA, newSHA, false},
		{"impostor sha", "actions/checkout@" + oldSHA + "=sha1-" + oldSHA + "=main", impostor, true},
	}
	for _, tt := range tests {
		for _, proxy := range []bool{false, true} {
			t.Run(tt.name+"/"+map[bool]string{false: "graphql", true: "dependabot proxy"}[proxy], func(t *testing.T) {
				reg := trackedSHAStubs(&httpmock.Registry{}, tt.uses, newSHA)
				if proxy {
					t.Setenv("GH_ACTIONS_LOCK_DEPENDABOT_PROXY", "1")
					useDefaultTransport(t, reg)
				}
				path := writeTempWorkflow(t, `
name: ci
on: push
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@`+tt.uses+`
`, tt.locked)
				lockBefore := readTempLockfilePins(t)
				wfBefore, err := os.ReadFile(path)
				require.NoError(t, err)

				stdout, stderr, runErr := runCommandWithHTTP(t, reg,
					"--no-onboard", "--no-narrow", "--no-interactive", "--json=findings", path)
				var out struct {
					Findings []struct {
						Category string `json:"category"`
						Severity string `json:"severity"`
					} `json:"findings"`
				}
				require.NoError(t, json.Unmarshal([]byte(stdout), &out), stdout+stderr)
				var cats []string
				for _, f := range out.Findings {
					cats = append(cats, f.Category+"/"+f.Severity)
				}
				assert.NotContains(t, cats, "onboarding-required/info", "a tracked action is not onboarding")

				if proxy {
					for _, req := range reg.Requests {
						assert.NotContains(t, req.URL.Path, "graphql", "the proxy path is REST-only")
					}
				}

				wfAfter, err := os.ReadFile(path)
				require.NoError(t, err)
				assert.Equal(t, string(wfBefore), string(wfAfter))
				lock := readTempLockfilePins(t)
				if tt.blocked {
					require.ErrorIs(t, runErr, errSilent, stderr)
					assert.Contains(t, cats, "unreachable-pin/error")
					assert.Equal(t, lockBefore, lock, "nothing is written")
					return
				}
				require.NoError(t, runErr, stderr)
				assert.Contains(t, lock, "'actions/checkout@"+tt.uses+"':")
				assert.Contains(t, lock, "sha1-"+tt.uses)
				assert.NotContains(t, lock, oldSHA)
			})
		}
	}
}

// TestNoOnboard_ProxyTransientResolveAborts: behind the Dependabot proxy a
// tracked SHA bump that can't be resolved or history-checked for a transient
// reason is a retry (exit 2 naming the failure), not a not-pinned user error.
// The bump targets a commit behind the branch head so the history check has
// to call Compare.
func TestNoOnboard_ProxyTransientResolveAborts(t *testing.T) {
	const (
		oldSHA = "eef61447b9ff4aafe5dcd4e0bbf5d482be7e7871"
		newSHA = "d632683dd7b4114ad314bca15554477dd762a938"
		head   = "11bd71901bbe5b1630ceea73d27597364c9af683"
	)
	failures := []struct {
		name string
		resp httpmock.Responder
		want string
	}{
		{"rate-limited 403", httpmock.StatusResponseWithHeaders(http.StatusForbidden, map[string]string{"X-RateLimit-Remaining": "0"}), "HTTP 403"},
		{"secondary rate-limited 403", httpmock.StatusResponseWithHeaders(http.StatusForbidden, map[string]string{"Retry-After": "60"}), "HTTP 403"},
		{"429", httpmock.StatusResponse(http.StatusTooManyRequests), "HTTP 429"},
		{"503", httpmock.StatusResponse(http.StatusServiceUnavailable), "HTTP 503"},
		{"connection refused", func(*http.Request) (*http.Response, error) { return nil, errors.New("connection refused") }, "connection refused"},
	}
	endpoints := map[string]string{
		"repo":    `repos/actions/checkout$`,
		"resolve": `repos/actions/checkout/commits/`,
		"compare": `repos/actions/checkout/compare/`,
	}
	for _, fail := range failures {
		for name, endpoint := range endpoints {
			t.Run(fail.name+"/"+name, func(t *testing.T) {
				t.Setenv("GH_ACTIONS_LOCK_DEPENDABOT_PROXY", "1")
				reg := &httpmock.Registry{}
				for range 50 {
					reg.Register(httpmock.REST("GET", endpoint), fail.resp)
				}
				trackedSHAStubs(reg, newSHA, head)
				useDefaultTransport(t, reg)
				path := writeTempWorkflow(t, `
name: ci
on: push
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@`+newSHA+`
`, "actions/checkout@"+oldSHA+"=sha1-"+oldSHA+"=main")
				lockBefore := readTempLockfilePins(t)

				_, stderr, err := runCommandWithHTTP(t, reg,
					"--no-onboard", "--no-narrow", "--no-interactive", "--json=findings", path)
				require.Error(t, err, stderr)
				assert.NotErrorIs(t, err, errSilent, "a transient failure is a retry, not a user error")
				assert.Contains(t, err.Error(), fail.want)
				assert.Equal(t, lockBefore, readTempLockfilePins(t), "nothing is written")
				hit := false
				for _, req := range reg.Requests {
					hit = hit || regexp.MustCompile(endpoint).MatchString(req.URL.Path)
				}
				assert.True(t, hit, "the failing endpoint was called")
			})
		}
	}
}

// trackedSHAStubs answers both the GraphQL and the anonymous REST paths for
// actions/checkout, with head as the only branch and tag commit. Stubs are
// one-shot, so each is registered once per pass the run makes.
func trackedSHAStubs(reg *httpmock.Registry, uses, head string) *httpmock.Registry {
	raw := func(body string) httpmock.Responder {
		return func(req *http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusOK, Request: req, Header: http.Header{},
				Body: io.NopCloser(strings.NewReader(body))}, nil
		}
	}
	status := "identical"
	if uses != head {
		status = "diverged"
	}
	compare := map[string]any{"status": status, "merge_base_commit": map[string]any{"sha": head}}
	for range 8 {
		reg.Register(httpmock.GraphQLForRepo("actions", "checkout"), httpmock.JSONResponse(map[string]any{
			"data": map[string]any{"a0": testRepoResponse("actions/checkout", uses, nodeActionYAML)},
		}))
		reg.Register(httpmock.REST("GET", `repos/actions/checkout$`), httpmock.JSONResponse(map[string]any{
			"full_name": "actions/checkout", "default_branch": "main", "id": 1, "owner": map[string]any{"id": 1},
		}))
		reg.Register(httpmock.REST("GET", `repos/actions/checkout/commits/`), httpmock.JSONResponse(map[string]any{"sha": uses}))
		reg.Register(httpmock.REST("GET", `repos/actions/checkout/contents/action\.yml`), raw(nodeActionYAML))
		reg.Register(httpmock.REST("GET", `repos/actions/checkout/git/ref/heads/main`), httpmock.JSONResponse(map[string]any{
			"ref": "refs/heads/main", "object": map[string]any{"sha": head, "type": "commit"},
		}))
		reg.Register(httpmock.REST("GET", `repos/actions/checkout/tags`), httpmock.JSONResponse(httpmock.TagListResponse("v4.2.2", head)))
		reg.Register(httpmock.REST("GET", `repos/actions/checkout/releases`), httpmock.JSONResponse([]map[string]any{}))
		reg.Register(httpmock.REST("GET", `repos/actions/checkout/branches`), httpmock.JSONResponse(httpmock.BranchListResponse("main", head)))
		reg.Register(httpmock.REST("GET", `repos/actions/checkout/compare/`), httpmock.JSONResponse(compare))
	}
	return reg
}

// TestProxyTransientNotMaskedByMissingRef: when one fresh ref is a 404 and
// another hits an outage, the run is still a retry. A 404 in one batch must
// not hide a 5xx in another.
func TestProxyTransientNotMaskedByMissingRef(t *testing.T) {
	const (
		oldSHA = "eef61447b9ff4aafe5dcd4e0bbf5d482be7e7871"
		newSHA = "d632683dd7b4114ad314bca15554477dd762a938"
		head   = "11bd71901bbe5b1630ceea73d27597364c9af683"
	)
	t.Setenv("GH_ACTIONS_LOCK_DEPENDABOT_PROXY", "1")
	reg := &httpmock.Registry{}
	for range 50 {
		reg.Register(httpmock.REST("GET", `repos/actions/checkout/commits/`), httpmock.StatusResponse(http.StatusServiceUnavailable))
		reg.Register(httpmock.REST("GET", `repos/nope/gone`), httpmock.StatusResponse(http.StatusNotFound))
	}
	trackedSHAStubs(reg, newSHA, head)
	useDefaultTransport(t, reg)
	path := writeTempWorkflow(t, `
name: ci
on: push
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: nope/gone@v1
      - uses: actions/checkout@`+newSHA+`
`, "actions/checkout@"+oldSHA+"=sha1-"+oldSHA+"=main")
	lockBefore := readTempLockfilePins(t)

	_, stderr, err := runCommandWithHTTP(t, reg,
		"--no-onboard", "--no-narrow", "--no-interactive", "--json=findings", path)
	require.Error(t, err, stderr)
	assert.NotErrorIs(t, err, errSilent, "an outage is a retry even beside a missing ref")
	assert.Contains(t, err.Error(), "HTTP 503")
	assert.Equal(t, lockBefore, readTempLockfilePins(t), "nothing is written")
}
