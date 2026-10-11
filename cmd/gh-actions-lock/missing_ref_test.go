package main

import (
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/github/gh-actions-lock/internal/ghapi/httpmock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestMissingRefNamesTheCause proves a ref that does not exist fails with an
// error finding that names it, rather than a fixable ref-changed plus a
// reachability warning.
func TestMissingRefNamesTheCause(t *testing.T) {
	for _, proxy := range []bool{false, true} {
		t.Run(map[bool]string{false: "graphql", true: "dependabot proxy"}[proxy], func(t *testing.T) {
			reg := &httpmock.Registry{}
			// Each resolve pass (plan, then diagnosis) asks again.
			for range 3 {
				registerMissingRef(reg, proxy)
			}
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
      - uses: actions/checkout@v4.2.2
`, "actions/checkout@v4.2.2")
			wf, err := os.ReadFile(path)
			require.NoError(t, err)
			wf = []byte(strings.Replace(string(wf), "@v4.2.2", "@v999.0.0", 1))
			require.NoError(t, os.WriteFile(path, wf, 0o600))
			lockBefore := readTempLockfilePins(t)

			stdout, _, err := runCommandWithHTTP(t, reg,
				"--no-onboard", "--no-narrow", "--no-interactive", "--json=findings", path)
			require.ErrorIs(t, err, errSilent)
			var out struct {
				Findings []struct {
					Category    string `json:"category"`
					Severity    string `json:"severity"`
					Detail      string `json:"detail"`
					Remediation string `json:"remediation"`
				} `json:"findings"`
			}
			require.NoError(t, json.Unmarshal([]byte(stdout), &out), stdout)
			found := false
			for _, f := range out.Findings {
				if f.Category == "not-pinned" && f.Severity == "error" {
					found = true
					assert.Equal(t, "ref v999.0.0 does not exist in actions/checkout", f.Detail)
					assert.Equal(t, "point `uses:` at a tag or branch that exists", f.Remediation)
				}
			}
			assert.True(t, found, stdout)
			assert.Equal(t, lockBefore, readTempLockfilePins(t))
			after, err := os.ReadFile(path)
			require.NoError(t, err)
			assert.Equal(t, string(wf), string(after))
		})
	}
}

func registerMissingRef(reg *httpmock.Registry, proxy bool) {
	if !proxy {
		reg.Register(httpmock.GraphQL(`repository\(owner:`), httpmock.JSONResponse(map[string]any{
			"data": map[string]any{"a0": map[string]any{"nameWithOwner": "actions/checkout", "object": nil}},
		}))
		return
	}
	reg.Register(httpmock.REST("GET", `^/repos/actions/checkout$`), httpmock.JSONResponse(map[string]any{
		"full_name": "actions/checkout", "id": 1, "owner": map[string]any{"id": 1},
	}))
	reg.Register(httpmock.REST("GET", `^/repos/actions/checkout/commits/v999\.0\.0$`),
		httpmock.StatusResponse(http.StatusUnprocessableEntity))
}
