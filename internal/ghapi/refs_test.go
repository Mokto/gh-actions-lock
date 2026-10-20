package ghapi

import (
	"context"
	"net/http"
	"testing"

	"github.com/github/gh-actions-lock/internal/ghapi/httpmock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestGitRefAsksOncePerRef: repair and reverse lookup both look up a
// recorded ref by exact name, so one run must not ask twice. Each stub
// below answers once.
func TestGitRefAsksOncePerRef(t *testing.T) {
	reg := &httpmock.Registry{}
	defer reg.Verify(t)
	reg.Register(httpmock.REST("GET", `^/repos/o/r/git/ref/tags/v1$`), httpmock.JSONResponse(httpmock.GitRefResponse("refs/tags/v1", paritySHA)))
	reg.Register(httpmock.REST("GET", `^/repos/o/r/git/ref/tags/gone$`), httpmock.StatusResponse(http.StatusNotFound))
	reg.Register(httpmock.REST("GET", `^/repos/o/r/git/ref/tags/flaky$`), httpmock.StatusResponse(http.StatusBadGateway))
	reg.Register(httpmock.REST("GET", `^/repos/o/r/git/ref/tags/flaky$`), httpmock.JSONResponse(httpmock.GitRefResponse("refs/tags/flaky", paritySHA)))

	c := newTestClient(t, reg)
	ctx := context.Background()
	for _, owner := range []string{"o", "O"} {
		got, err := c.TagExists(ctx, owner, "r", "v1")
		require.NoError(t, err)
		assert.True(t, got, "found")
		got, err = c.TagExists(ctx, "o", "r", "gone")
		require.NoError(t, err)
		assert.False(t, got, "a missing ref is cached too")
	}
	_, err := c.TagExists(ctx, "o", "r", "flaky")
	require.Error(t, err)
	got, err := c.TagExists(ctx, "o", "r", "flaky")
	require.NoError(t, err, "a failed lookup is not cached")
	assert.True(t, got)
}
