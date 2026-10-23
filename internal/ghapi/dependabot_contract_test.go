package ghapi

// Dependabot runs this CLI with the environment its github_actions lockfile
// Env.build creates: GH_TOKEN is the home-host token, or a placeholder plus
// GH_ACTIONS_LOCK_DEPENDABOT_PROXY=1 when a proxy supplies the credential.
// Its contract: only the home-host token is ever used, and other hosts
// (the github.com public fallback) are read anonymously.

import (
	"context"
	"net/http"
	"testing"

	"github.com/github/gh-actions-lock/internal/ghapi/httpmock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// envBuildTransport injects reg the way the CLI's own transport is used,
// leaving go-gh to resolve the tenant token from GH_TOKEN as it would in a
// Dependabot run. The public fallback keeps the default credential lookup.
func envBuildTransport(reg http.RoundTripper) ClientOption {
	return func(c *clientConfig) {
		c.transport = reg
		c.logIgnore = true
	}
}

func fallbackRegistry() *httpmock.Registry {
	reg := &httpmock.Registry{}
	reg.Register(hostRequest("api.tenant.ghe.com", `repos/o/r$`), httpmock.StatusResponse(404))
	reg.Register(hostRequest("api.github.com", `repos/o/r$`), httpmock.JSONResponse(map[string]any{
		"visibility": "public", "id": 2, "owner": map[string]any{"id": 1},
	}))
	return reg
}

func TestDependabotContract(t *testing.T) {
	t.Run("proxy mode never looks up a github.com credential", func(t *testing.T) {
		t.Setenv("GH_TOKEN", "x-access-token")
		t.Setenv("GH_ACTIONS_LOCK_DEPENDABOT_PROXY", "1")
		stubGhConfig(t, "hosts:\n  github.com:\n    oauth_token: stored-dotcom\n")
		calls := fakeGh(t, "stored-dotcom")
		reg := fallbackRegistry()
		defer reg.Verify(t)

		c, err := New("tenant.ghe.com", envBuildTransport(reg))
		require.NoError(t, err)
		// The proxy client reads REST through its raw HTTP client.
		c.local.anonHTTP = &http.Client{Transport: reg}
		_, err = c.ForRepo(context.Background(), "o", "r")
		require.NoError(t, err)
		assert.Empty(t, readCalls(t, calls), "ran gh under the Dependabot proxy")
		for _, req := range reg.Requests {
			if req.URL.Host == "api.github.com" {
				assert.Empty(t, req.Header.Get("Authorization"))
			}
		}
	})

	t.Run("proxy mode keeps rate-limit messages", func(t *testing.T) {
		t.Setenv("GH_ACTIONS_LOCK_DEPENDABOT_PROXY", "1")
		reg := &httpmock.Registry{}
		reg.Register(hostRequest("api.github.com", `repos/o/r$`), func(req *http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 403, Header: http.Header{"X-Ratelimit-Remaining": {"0"}}, Body: http.NoBody, Request: req}, nil
		})
		c, err := New("tenant.ghe.com", WithClientTransport(reg))
		require.NoError(t, err)
		require.NoError(t, c.PinHost("o", "r", "github.com"))
		_, err = c.ForRepo(context.Background(), "o", "r")
		require.Error(t, err)
		assert.False(t, AnonymousFallbackLimited(err))
		reason, limited := Outage(err)
		assert.Equal(t, "GitHub API rate limit exhausted", reason)
		assert.True(t, limited)
		assert.Equal(t, "GitHub returned 403 Forbidden", Brief(err))
	})

	t.Run("proxyless home-host token never reaches github.com", func(t *testing.T) {
		t.Setenv("GH_TOKEN", "home-host-token")
		t.Setenv("GH_ACTIONS_LOCK_DEPENDABOT_PROXY", "")
		stubGhConfig(t, "")
		fakeGh(t, "")
		reg := fallbackRegistry()
		defer reg.Verify(t)

		c, err := New("tenant.ghe.com", envBuildTransport(reg))
		require.NoError(t, err)
		selected, err := c.ForRepo(context.Background(), "o", "r")
		require.NoError(t, err)
		assert.Equal(t, "github.com", selected.Hostname)
		for _, req := range reg.Requests {
			if req.URL.Host == "api.github.com" {
				assert.Empty(t, req.Header.Get("Authorization"))
			} else {
				assert.Equal(t, "token home-host-token", req.Header.Get("Authorization"))
			}
		}
	})
}
