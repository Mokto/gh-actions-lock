package ghapi

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/cli/go-gh/v2/pkg/api"
	"github.com/cli/go-gh/v2/pkg/config"
	"github.com/github/gh-actions-lock/internal/ghapi/httpmock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stubGhConfig replaces go-gh's cached config read with hosts.
func stubGhConfig(t *testing.T, hosts string) {
	t.Helper()
	orig := config.Read
	t.Cleanup(func() { config.Read = orig })
	config.Read = func(*config.Config) (*config.Config, error) { return config.ReadFromString(hosts), nil }
}

// fakeGh points GH_PATH at a script that records its arguments in the
// returned file and prints out, or fails when out is empty.
func fakeGh(t *testing.T, out string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell script stub")
	}
	dir := t.TempDir()
	calls := filepath.Join(dir, "calls")
	script := "#!/bin/sh\necho \"$@\" >> " + calls + "\n"
	if out == "" {
		script += "exit 1\n"
	} else {
		script += "echo " + out + "\n"
	}
	gh := filepath.Join(dir, "gh")
	require.NoError(t, os.WriteFile(gh, []byte(script), 0o755))
	t.Setenv("GH_PATH", gh)
	return calls
}

func readCalls(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return ""
	}
	require.NoError(t, err)
	return string(b)
}

func TestStoredDotcomToken(t *testing.T) {
	for _, tt := range []struct {
		name      string
		hosts     string
		keyring   string
		want      string
		wantCalls string
	}{
		{
			name:  "config token",
			hosts: "hosts:\n  github.com:\n    oauth_token: from-config\n",
			want:  "from-config",
		},
		{
			name:      "keyring token",
			hosts:     "hosts:\n  tenant.ghe.com:\n    oauth_token: tenant-config\n",
			keyring:   "from-keyring",
			want:      "from-keyring",
			wantCalls: "auth token --secure-storage --hostname github.com\n",
		},
		{
			name:      "no github.com login",
			hosts:     "hosts:\n  tenant.ghe.com:\n    oauth_token: tenant-config\n",
			wantCalls: "auth token --secure-storage --hostname github.com\n",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("GH_TOKEN", "env-gh-token")
			t.Setenv("GITHUB_TOKEN", "env-github-token")
			t.Setenv("GH_ENTERPRISE_TOKEN", "env-enterprise-token")
			stubGhConfig(t, tt.hosts)
			calls := fakeGh(t, tt.keyring)
			assert.Equal(t, tt.want, storedDotcomToken())
			assert.Equal(t, tt.wantCalls, readCalls(t, calls))
		})
	}
}

func withDotcomToken(token string) ClientOption {
	return func(c *clientConfig) { c.dotcomToken = func() string { return token } }
}

func TestPublicFallbackCredential(t *testing.T) {
	for _, tt := range []struct {
		name     string
		stored   string
		wantAuth string
	}{
		{name: "stored github.com login authenticates fallback", stored: "dotcom-secret", wantAuth: "token dotcom-secret"},
		{name: "no stored login stays anonymous"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			reg := &httpmock.Registry{}
			defer reg.Verify(t)
			reg.Register(hostRequest("api.tenant.ghe.com", `repos/o/r$`), httpmock.StatusResponse(404))
			reg.Register(hostRequest("api.github.com", `repos/o/r$`),
				httpmock.JSONResponse(map[string]any{"visibility": "public", "id": 2, "owner": map[string]any{"id": 1}}))
			c, err := New("tenant.ghe.com", WithClientTransport(reg), withDotcomToken(tt.stored), func(cfg *clientConfig) {
				cfg.authToken = "tenant-only-secret"
			})
			require.NoError(t, err)
			selected, err := c.ForRepo(context.Background(), "o", "r")
			require.NoError(t, err)
			assert.Equal(t, "github.com", selected.Hostname)
			for _, req := range reg.Requests {
				auth := req.Header.Get("Authorization")
				if req.URL.Host == "api.github.com" {
					assert.Equal(t, tt.wantAuth, auth)
					assert.NotContains(t, auth, "tenant-only-secret")
					assert.Empty(t, req.Header.Get("Cookie"))
				} else {
					assert.Contains(t, auth, "tenant-only-secret")
				}
			}
		})
	}
}

func TestPublicFallbackTokenOnlyForTenants(t *testing.T) {
	for _, host := range []string{"github.com", "github.example.com"} {
		_, err := New(host, WithClientTransport(&httpmock.Registry{}), func(c *clientConfig) {
			c.dotcomToken = func() string { t.Errorf("%s looked up a github.com credential", host); return "" }
		})
		require.NoError(t, err)
	}
}

func publicLimited(mode string) error {
	h := http.Header{"X-Ratelimit-Remaining": {"0"}}
	if mode != "" {
		h.Set(publicAuthHeader, mode)
	}
	return &api.HTTPError{StatusCode: http.StatusForbidden, Headers: h}
}

func TestAnonymousFallbackLimited(t *testing.T) {
	for _, tt := range []struct {
		name       string
		err        error
		want       bool
		wantReason string
	}{
		{"anonymous fallback", publicLimited(publicAnonymous), true, "github.com rate limit for anonymous public lookups exhausted"},
		{"anonymous fallback, wrapped", &UnknownOutcomeError{Cause: publicLimited(publicAnonymous)}, true, "github.com rate limit for anonymous public lookups exhausted"},
		{"stored login", publicLimited(publicStored), false, "GitHub API rate limit exhausted"},
		{"tenant or proxied", publicLimited(""), false, "GitHub API rate limit exhausted"},
		{"anonymous but not limited", &api.HTTPError{StatusCode: 500, Headers: http.Header{publicAuthHeader: {publicAnonymous}}}, false, "GitHub API returned 500 Internal Server Error"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, AnonymousFallbackLimited(tt.err))
			reason, limited := Outage(tt.err)
			assert.Equal(t, tt.wantReason, reason)
			assert.Equal(t, tt.want || strings.Contains(reason, "rate limit"), limited)
			if tt.want {
				assert.Contains(t, Brief(tt.err), "gh auth login --hostname github.com")
			} else {
				assert.NotContains(t, Brief(tt.err), "gh auth login")
			}
		})
	}
}

func TestStoredDotcomCredentialRejected(t *testing.T) {
	t.Setenv("GH_TOKEN", "tenant-token")
	u, _ := http.NewRequest(http.MethodGet, "https://api.github.com/repos/o/r", nil)
	err := &api.HTTPError{StatusCode: http.StatusUnauthorized, RequestURL: u.URL, Headers: http.Header{publicAuthHeader: {publicStored}}}
	got := ConfigError(err)
	require.Error(t, got)
	assert.Equal(t, "GitHub rejected the stored github.com credential used for public lookups (401); run `gh auth login --hostname github.com`", got.Error())
}
