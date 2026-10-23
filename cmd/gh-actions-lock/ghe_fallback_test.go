package main

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestGHEAnonymousFallbackRateLimit pins what Dependabot parses when the
// anonymous github.com fallback is rate-limited: exit 1 with a retryable
// reachability-unknown error. Only the remediation text names the fix.
func TestGHEAnonymousFallbackRateLimit(t *testing.T) {
	path := writeTempWorkflow(t, `
name: ci
on: push
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: tenant/internal@v1
      - uses: actions/public@v2
`)
	_, _, err := runCommandWithHTTP(t, tenantFixture(t, 200),
		"--hostname", "tenant.ghe.com", "--no-interactive", "--no-narrow", "--no-migrate-local-actions", "--json", path)
	require.NoError(t, err)

	base := tenantFixture(t, 200)
	limited := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Host == "api.github.com" && strings.Contains(req.URL.Path, "/git/") {
			return &http.Response{StatusCode: http.StatusForbidden, Header: http.Header{"X-Ratelimit-Remaining": {"0"}},
				Body: io.NopCloser(strings.NewReader(`{"message":"API rate limit exceeded"}`)), Request: req}, nil
		}
		return base.RoundTrip(req)
	})
	stdout, stderr, err := runCommandWithHTTP(t, limited, "--hostname", "tenant.ghe.com", "--verify", "--json=valid,findings", path)
	require.ErrorIs(t, err, errSilent, stderr)
	var report struct {
		Valid    bool             `json:"valid"`
		Findings []map[string]any `json:"findings"`
	}
	require.NoError(t, json.Unmarshal([]byte(stdout), &report))
	assert.False(t, report.Valid)
	require.Len(t, report.Findings, 1)
	f := report.Findings[0]
	assert.Equal(t, "reachability-unknown", f["category"])
	assert.Equal(t, "error", f["severity"])
	assert.Equal(t, "actions/public@v2", f["dependency"])
	assert.Equal(t, true, f["retryable"])
	assert.Contains(t, f["remediation"], "gh auth login --hostname github.com")
}
