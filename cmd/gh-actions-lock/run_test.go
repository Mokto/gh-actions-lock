package main

import (
	"net/http"
	"strings"
	"testing"

	"github.com/github/gh-actions-lock/internal/ghapi/httpmock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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
