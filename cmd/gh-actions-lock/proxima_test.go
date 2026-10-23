package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"

	parserlock "github.com/github/actions-lockfile/go/pkg/lockfile"
	"github.com/github/gh-actions-lock/internal/ghapi/httpmock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

const tenantSHA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
const publicSHA = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

func tenantFixture(t *testing.T, publicStatus int) http.RoundTripper {
	t.Helper()
	return roundTripFunc(func(req *http.Request) (*http.Response, error) {
		tenant := req.URL.Host == "api.tenant.ghe.com"
		if tenant {
			assert.NotEmpty(t, req.Header.Get("Authorization"))
		} else {
			assert.Equal(t, "api.github.com", req.URL.Host)
			assert.Empty(t, req.Header.Get("Authorization"))
			assert.Empty(t, req.Header.Get("Cookie"))
		}
		path := req.URL.Path
		switch {
		case tenant && path == "/repos/tenant/internal":
			return httpmock.JSONResponse(map[string]any{
				"visibility": "internal", "id": 111, "owner": map[string]any{"id": 11},
			})(req)
		case path == "/repos/actions/public":
			if tenant {
				return httpmock.StatusResponse(http.StatusNotFound)(req)
			}
			if publicStatus != 200 {
				return httpmock.StatusResponse(publicStatus)(req)
			}
			return httpmock.JSONResponse(map[string]any{
				"visibility": "public", "id": 222, "owner": map[string]any{"id": 22},
			})(req)
		case tenant && path == "/graphql":
			var body struct {
				Variables map[string]string `json:"variables"`
			}
			if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
				return nil, err
			}
			assert.Equal(t, "internal", body.Variables["name0"])
			composite := "runs:\n  using: composite\n  steps:\n    - uses: actions/public@v2\n"
			return httpmock.JSONResponse(map[string]any{"data": map[string]any{
				"a0": testRepoResponse("tenant/internal", tenantSHA, composite),
			}})(req)
		case !tenant && (path == "/repos/actions/public/commits/v2" || path == "/repos/actions/public/commits/"+publicSHA):
			return httpmock.JSONResponse(map[string]any{"sha": publicSHA})(req)
		case !tenant && path == "/repos/actions/public/git/ref/tags/v2":
			return httpmock.JSONResponse(httpmock.GitRefResponse("refs/tags/v2", publicSHA))(req)
		case !tenant && path == "/repos/actions/public/contents/action.yml":
			assert.Equal(t, publicSHA, req.URL.Query().Get("ref"))
			return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(nodeActionYAML)), Request: req}, nil
		case strings.HasSuffix(path, "/releases"):
			return httpmock.JSONResponse([]any{})(req)
		}
		return nil, fmt.Errorf("unexpected request: %s %s", req.Method, req.URL)
	})
}

func TestProximaGenerationPreservesHosts(t *testing.T) {
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
	args := []string{"--hostname", "tenant.ghe.com", "--no-interactive", "--no-narrow", "--no-migrate-local-actions", "--json", path}
	_, _, err := runCommandWithHTTP(t, tenantFixture(t, 200), args...)
	require.NoError(t, err)
	raw := readTempLockfilePins(t)
	file, err := parserlock.Parse([]byte(raw))
	require.NoError(t, err)
	assert.Equal(t, "v0.0.3", file.Version)
	require.Len(t, file.Dependencies, 2)
	local := file.Dependencies["tenant/internal@v1"]
	assert.Empty(t, local.Hostname)
	assert.Equal(t, "sha1-"+tenantSHA, local.Commit)
	assert.EqualValues(t, 11, local.OwnerID)
	assert.EqualValues(t, 111, local.RepoID)
	assert.Equal(t, []string{"actions/public@v2"}, local.Uses)
	public := file.Dependencies["actions/public@v2"]
	assert.Equal(t, "github.com", public.Hostname)
	assert.Equal(t, "sha1-"+publicSHA, public.Commit)
	assert.EqualValues(t, 22, public.OwnerID)
	assert.EqualValues(t, 222, public.RepoID)

	_, _, err = runCommandWithHTTP(t, tenantFixture(t, 200), args...)
	require.NoError(t, err)
	assert.Equal(t, raw, readTempLockfilePins(t), "repeat generation must retain hosts and IDs")

	base := tenantFixture(t, 200)
	pinnedTransport := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		assert.False(t, req.URL.Host == "api.tenant.ghe.com" && req.URL.Path == "/repos/actions/public",
			"recorded dotcom pins must bypass tenant namesakes")
		return base.RoundTrip(req)
	})
	_, _, err = runCommandWithHTTP(t, pinnedTransport,
		"--hostname", "tenant.ghe.com", "--verify", "--json", path)
	require.NoError(t, err)
	assert.Equal(t, raw, readTempLockfilePins(t))

	// Convert the previous preview's explicit home host without moving its pin.
	explicit := strings.Replace(raw, "'tenant/internal@v1':\n", "'tenant/internal@v1':\n        hostname: 'tenant.ghe.com'\n", 1)
	require.NotEqual(t, raw, explicit)
	require.NoError(t, os.WriteFile(parserlock.Path, []byte(explicit), 0o600))
	_, _, err = runCommandWithHTTP(t, tenantFixture(t, 200), args...)
	require.NoError(t, err)
	assert.Equal(t, raw, readTempLockfilePins(t))
}

func TestProximaOmittedPinsCannotMoveToDotcomOrTenantNamesakes(t *testing.T) {
	for _, status := range []int{200, 401, 403, 404, 503} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			path := writeTempWorkflow(t, `
name: ci
on: push
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/public@v2
`, "actions/public@v2=sha1-"+publicSHA)
			raw := readTempLockfilePins(t)
			raw = strings.ReplaceAll(raw, "owner_id: 1", "owner_id: 22")
			raw = strings.ReplaceAll(raw, "repo_id: 1", "repo_id: 222")
			require.NoError(t, os.WriteFile(parserlock.Path, []byte(raw), 0o600))
			transport := roundTripFunc(func(req *http.Request) (*http.Response, error) {
				assert.Equal(t, "api.tenant.ghe.com", req.URL.Host, "omitted pins must not fall back to dotcom")
				assert.NotEmpty(t, req.Header.Get("Authorization"))
				if req.URL.Path == "/graphql" {
					return parityRepoID(req, status, 333)
				}
				assert.Equal(t, "/repos/actions/public", req.URL.Path)
				if status != 200 {
					return httpmock.StatusResponse(status)(req)
				}
				return httpmock.JSONResponse(map[string]any{
					"visibility": "internal", "id": 333, "owner": map[string]any{"id": 33},
				})(req)
			})
			stdout, _, err := runCommandWithHTTP(t, transport,
				"--hostname", "tenant.ghe.com", "--no-narrow", "--no-migrate-local-actions", "--json", path)
			switch status {
			case 200:
				require.ErrorIs(t, err, errSilent)
				assert.Contains(t, stdout, "repo-replaced")
			case 404:
				require.ErrorIs(t, err, errSilent)
				assert.Contains(t, stdout, "repo-unavailable")
			default:
				require.Error(t, err)
				assert.NotErrorIs(t, err, errSilent, "inconclusive identity must exit 2")
				assert.ErrorContains(t, err, fmt.Sprintf("HTTP %d", status))
			}
			assert.Equal(t, raw, readTempLockfilePins(t))
		})
	}
}

// parityRepoID answers a parity query as a pin whose repository now has
// repoID, or as a missing repository when status is 404.
func parityRepoID(req *http.Request, status int, repoID int) (*http.Response, error) {
	var body struct {
		Variables map[string]any `json:"variables"`
	}
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
		return nil, err
	}
	if status == http.StatusNotFound {
		return httpmock.JSONResponse(map[string]any{"data": map[string]any{"a0": nil}})(req)
	}
	oid := map[string]any{"oid": body.Variables["oid0"]}
	return httpmock.JSONResponse(map[string]any{"data": map[string]any{"a0": map[string]any{
		"nameWithOwner": fmt.Sprintf("%s/%s", body.Variables["owner0"], body.Variables["name0"]),
		"databaseId":    repoID,
		"commit":        oid,
		"tag":           oid,
		"tagRef":        oid,
	}}})(req)
}

func TestProximaIncompleteGenerationDoesNotWrite(t *testing.T) {
	for _, status := range []int{404, 403, 429, 500} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			path := writeTempWorkflow(t, `
name: ci
on: push
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: tenant/internal@v1
`)
			before, err := os.ReadFile(path)
			require.NoError(t, err)
			stdout, _, err := runCommandWithHTTP(t, tenantFixture(t, status),
				"--hostname", "tenant.ghe.com", "--no-narrow", "--no-migrate-local-actions", "--json", path)
			require.ErrorIs(t, err, errSilent)
			var result struct {
				Valid bool `json:"valid"`
			}
			require.NoError(t, json.Unmarshal([]byte(stdout), &result))
			assert.False(t, result.Valid)
			_, err = os.Stat(parserlock.Path)
			assert.True(t, os.IsNotExist(err))
			after, err := os.ReadFile(path)
			require.NoError(t, err)
			assert.Equal(t, before, after)
		})
	}
}

func TestProximaLegacyPinsRequireDotcomIdentity(t *testing.T) {
	for _, matches := range []bool{true, false} {
		t.Run(fmt.Sprintf("matching IDs=%v", matches), func(t *testing.T) {
			path := writeTempWorkflow(t, `
name: ci
on: push
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/public@v2
`, "actions/public@v2=sha1-"+publicSHA)
			raw := strings.ReplaceAll(readTempLockfilePins(t), "v0.0.3", "v0.0.2")
			if matches {
				raw = strings.ReplaceAll(raw, "owner_id: 1", "owner_id: 22")
				raw = strings.ReplaceAll(raw, "repo_id: 1", "repo_id: 222")
			}
			require.NoError(t, os.WriteFile(parserlock.Path, []byte(raw), 0o600))
			stdout, _, err := runCommandWithHTTP(t, tenantFixture(t, 200),
				"--hostname", "tenant.ghe.com", "--no-narrow", "--no-migrate-local-actions", "--json", path)
			if matches {
				require.NoError(t, err)
				assert.Contains(t, readTempLockfilePins(t), "hostname: 'github.com'")
			} else {
				require.ErrorIs(t, err, errSilent)
				assert.Contains(t, stdout, "repo-replaced")
				assert.Equal(t, raw, readTempLockfilePins(t))
			}
		})
	}
}

// TestOwnSHAEntryKeepsRecordedHost: an entry recording its own SHA as its
// ref is re-resolved, but its recorded host still binds the repository
// before any lookup: a github.com pin never reaches a tenant namesake, and a
// pin to another host fails as it would for any other entry.
func TestOwnSHAEntryKeepsRecordedHost(t *testing.T) {
	writeLock := func(t *testing.T, version, hostname string) string {
		t.Helper()
		path := writeTempWorkflow(t, `
name: ci
on: push
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/public@`+publicSHA+`
`)
		lock := "version: '" + version + "'\nworkflows:\n    '.github/workflows/workflow.yml':\n        - 'actions/public@" + publicSHA + "'\n" +
			"dependencies:\n    'actions/public@" + publicSHA + "':\n        hostname: '" + hostname + "'\n        ref: '" + publicSHA + "'\n        commit: 'sha1-" + publicSHA + "'\n        owner_id: 22\n        repo_id: 222\n"
		require.NoError(t, os.WriteFile(parserlock.Path, []byte(lock), 0o600))
		return path
	}

	t.Run("github.com pin on a tenant", func(t *testing.T) {
		path := writeLock(t, "v0.0.3", "github.com")
		base := tenantFixture(t, 200)
		transport := roundTripFunc(func(req *http.Request) (*http.Response, error) {
			assert.False(t, req.URL.Host == "api.tenant.ghe.com" && strings.Contains(req.URL.Path, "actions/public"),
				"a recorded github.com pin must bypass tenant namesakes: %s", req.URL)
			if req.URL.Host == "api.tenant.ghe.com" && req.URL.Path == "/graphql" {
				return parityRepoID(req, 200, 222)
			}
			return base.RoundTrip(req)
		})
		stdout, stderr, err := runCommandWithHTTP(t, transport, "--hostname", "tenant.ghe.com", "--verify", "--json=valid,findings", path)
		require.ErrorIs(t, err, errSilent, stderr)
		assert.Contains(t, stdout, "records ref "+parserlock.ShortSHA(publicSHA))
	})

	t.Run("pin to another host", func(t *testing.T) {
		path := writeLock(t, "v0.0.3", "other.ghe.com")
		transport := roundTripFunc(func(req *http.Request) (*http.Response, error) {
			t.Errorf("unexpected request: %s", req.URL)
			return nil, fmt.Errorf("unexpected request")
		})
		_, _, err := runCommandWithHTTP(t, transport, "--verify", path)
		require.Error(t, err)
		assert.NotErrorIs(t, err, errSilent)
		assert.ErrorContains(t, err, "actions/public is pinned to other.ghe.com, not the selected host github.com")
	})
}
