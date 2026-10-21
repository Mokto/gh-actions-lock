package ghapi

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/github/gh-actions-lock/internal/ghapi/httpmock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	paritySHA   = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	parityMoved = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

func TestCheckPins_GraphQLParsesEachState(t *testing.T) {
	reg := &httpmock.Registry{}
	defer reg.Verify(t)

	var gotQuery string
	var gotVars map[string]any
	reg.Register(httpmock.GraphQL(`commit: object\(oid`), httpmock.GraphQLQuery(`{
		"data": {
			"a0": {"nameWithOwner": "actions/checkout", "databaseId": 2, "owner": {"databaseId": 1},
			       "commit": {"oid": "`+paritySHA+`"}, "tag": {"oid": "`+paritySHA+`"}, "tagRef": {}},
			"a1": {"nameWithOwner": "typesafegithub/github-actions-typing", "databaseId": 3, "owner": {"databaseId": 4},
			       "commit": {"oid": "`+paritySHA+`"}},
			"a2": {"nameWithOwner": "octo/gone", "databaseId": 5, "owner": {"databaseId": 6}, "commit": null},
			"a3": {"nameWithOwner": "octo/retag", "databaseId": 7, "owner": {"databaseId": 8},
			       "commit": {"oid": "`+paritySHA+`"}, "tag": {"oid": "`+parityMoved+`"}, "tagRef": {}},
			"a4": null
		},
		"errors": [
			{"type": "NOT_FOUND", "path": ["a2", "commit"], "message": "Could not resolve to a commit"},
			{"type": "NOT_FOUND", "path": ["a4"], "message": "Could not resolve to a Repository"}
		]
	}`, func(q string, v map[string]any) { gotQuery, gotVars = q, v }))

	states := newTestClient(t, reg).CheckPins(context.Background(), []PinCheck{
		{Owner: "actions", Repo: "checkout", SHA: paritySHA, Head: "v4.2.1"},
		{Owner: "krzema12", Repo: "github-actions-typing", SHA: paritySHA},
		{Owner: "octo", Repo: "gone", SHA: paritySHA},
		{Owner: "octo", Repo: "retag", SHA: paritySHA, Head: "v1.0.0"},
		{Owner: "octo", Repo: "deleted", SHA: paritySHA},
	})
	require.Len(t, states, 5)
	assert.Len(t, reg.Requests, 1, "one batched request for every pin")
	assert.NotContains(t, gotQuery, "action.yml", "parity must not fetch action blobs")
	assert.Equal(t, "refs/tags/v4.2.1^{commit}", gotVars["tag0"])
	assert.Equal(t, "refs/heads/v4.2.1^{commit}", gotVars["head0"])
	assert.NotContains(t, gotVars, "tag1", "a pin with no ref looks none up")

	assert.Equal(t, PinState{NameWithOwner: "actions/checkout", OwnerID: 1, RepoID: 2, CommitFound: true, HeadOID: paritySHA}, states[0])
	assert.Equal(t, "typesafegithub/github-actions-typing", states[1].NameWithOwner)
	assert.True(t, states[1].CommitFound)
	assert.NoError(t, states[2].Err)
	assert.False(t, states[2].CommitFound)
	assert.True(t, states[3].CommitFound)
	assert.Equal(t, parityMoved, states[3].HeadOID)
	assert.NoError(t, states[4].Err)
	assert.True(t, states[4].RepoMissing, "a NOT_FOUND repository is a verdict the runner shares")
	assert.False(t, states[4].ViaFallback)
}

func TestCheckPins_GraphQLBatchesFifty(t *testing.T) {
	reg := &httpmock.Registry{}
	defer reg.Verify(t)
	ok := func(req *http.Request) (*http.Response, error) {
		data := map[string]any{}
		for i := range pinCheckBatchSize {
			data[fmt.Sprintf("a%d", i)] = map[string]any{"nameWithOwner": "o/r", "commit": map[string]any{"oid": paritySHA}}
		}
		return httpmock.JSONResponse(map[string]any{"data": data})(req)
	}
	reg.Register(httpmock.GraphQL(`commit: object\(oid`), ok)
	reg.Register(httpmock.GraphQL(`commit: object\(oid`), ok)

	pins := make([]PinCheck, pinCheckBatchSize+1)
	for i := range pins {
		pins[i] = PinCheck{Owner: "o", Repo: fmt.Sprintf("r%d", i), SHA: paritySHA}
	}
	states := newTestClient(t, reg).CheckPins(context.Background(), pins)
	require.Len(t, states, len(pins))
	assert.Len(t, reg.Requests, 2)
	for _, s := range states {
		assert.True(t, s.CommitFound)
	}
}

func TestCheckPins_GraphQLSplitsOnBatchFailure(t *testing.T) {
	reg := &httpmock.Registry{}
	defer reg.Verify(t)
	reg.Register(httpmock.GraphQL(`a1:`), httpmock.StatusResponse(http.StatusBadGateway))
	for range 2 {
		reg.Register(httpmock.GraphQL(`commit: object\(oid`), httpmock.JSONResponse(map[string]any{
			"data": map[string]any{"a0": map[string]any{"nameWithOwner": "o/r", "commit": map[string]any{"oid": paritySHA}}},
		}))
	}

	states := newTestClient(t, reg).CheckPins(context.Background(), []PinCheck{
		{Owner: "o", Repo: "r", SHA: paritySHA},
		{Owner: "o", Repo: "s", SHA: paritySHA},
	})
	assert.Len(t, reg.Requests, 3)
	for _, s := range states {
		assert.NoError(t, s.Err)
		assert.True(t, s.CommitFound)
	}
}

// TestCheckPins_ProvesEachCommitOncePerRun: a check with no ref proves a
// repository and an immutable commit, so the run asks once whoever asks and
// however they spell it, and every caller gets the same answer, a failed one
// included. A check of a ref is asked each time: refs move.
func TestCheckPins_ProvesEachCommitOncePerRun(t *testing.T) {
	upper := strings.ToUpper(paritySHA)
	for _, tt := range []struct {
		name   string
		status int
	}{{"proven", http.StatusOK}, {"failed", http.StatusBadGateway}} {
		t.Run(tt.name, func(t *testing.T) {
			reg := &httpmock.Registry{}
			defer reg.Verify(t)
			if tt.status == http.StatusOK {
				reg.Register(httpmock.GraphQL(`commit: object\(oid`), httpmock.JSONResponse(map[string]any{
					"data": map[string]any{"a0": map[string]any{"nameWithOwner": "o/r", "databaseId": 7, "commit": map[string]any{"oid": paritySHA}}},
				}))
			} else {
				reg.RegisterReusable(httpmock.GraphQL(`commit: object\(oid`), httpmock.StatusResponse(tt.status))
			}
			c := newTestClient(t, reg)

			first := c.CheckPins(context.Background(), []PinCheck{{Owner: "o", Repo: "r", SHA: paritySHA}, {Owner: "O", Repo: "R", SHA: upper}})
			again := c.CheckPins(context.Background(), []PinCheck{{Owner: "o", Repo: "R", SHA: upper}})
			graphql := 0
			for _, r := range reg.Requests {
				if strings.Contains(r.URL.Path, "graphql") {
					graphql++
				}
			}
			assert.Equal(t, 1, graphql, "one check of the commit per run")
			require.Len(t, first, 2)
			require.Len(t, again, 1)
			assert.Equal(t, first[0], first[1])
			assert.Equal(t, first[0], again[0])
			if tt.status == http.StatusOK {
				assert.True(t, first[0].CommitFound)
				assert.EqualValues(t, 7, first[0].RepoID)
			} else {
				assert.Error(t, again[0].Err, "a failed check stays failed")
			}
		})
	}

	t.Run("a ref is checked each time", func(t *testing.T) {
		reg := &httpmock.Registry{}
		defer reg.Verify(t)
		for range 2 {
			reg.Register(httpmock.GraphQL(`commit: object\(oid`), httpmock.JSONResponse(map[string]any{
				"data": map[string]any{"a0": map[string]any{"nameWithOwner": "o/r", "commit": map[string]any{"oid": paritySHA}, "tag": map[string]any{"oid": paritySHA}, "tagRef": map[string]any{}}},
			}))
		}
		c := newTestClient(t, reg)
		for range 2 {
			c.CheckPins(context.Background(), []PinCheck{{Owner: "o", Repo: "r", SHA: paritySHA, Head: "v1"}})
		}
		assert.Len(t, reg.Requests, 2)
	})
}

func TestCheckPins_RESTOnly(t *testing.T) {
	t.Setenv("GH_ACTIONS_LOCK_DEPENDABOT_PROXY", "1")
	reg := &httpmock.Registry{}
	defer reg.Verify(t)

	reg.Register(httpmock.REST("GET", `^/repos/krzema12/github-actions-typing$`), httpmock.JSONResponse(map[string]any{
		"full_name": "typesafegithub/github-actions-typing", "id": 3, "owner": map[string]any{"id": 4},
	}))
	reg.Register(httpmock.REST("GET", `^/repos/krzema12/github-actions-typing/commits/`+paritySHA+`$`), httpmock.JSONResponse(map[string]any{"sha": paritySHA}))
	reg.Register(httpmock.REST("GET", `^/repos/octo/retag$`), httpmock.JSONResponse(map[string]any{"full_name": "octo/retag"}))
	reg.Register(httpmock.REST("GET", `^/repos/octo/retag/git/ref/tags/v1\.0\.0$`), httpmock.JSONResponse(httpmock.GitRefResponse("refs/tags/v1.0.0", parityMoved)))
	reg.Register(httpmock.REST("GET", `^/repos/octo/retag/commits/`+paritySHA+`$`), httpmock.JSONResponse(map[string]any{"sha": paritySHA}))
	reg.Register(httpmock.REST("GET", `^/repos/octo/retag/git/ref/tags/v2\.0\.0$`), httpmock.JSONResponse(httpmock.GitRefResponse("refs/tags/v2.0.0", paritySHA)))
	reg.Register(httpmock.REST("GET", `^/repos/octo/retag/commits/`+parityMoved+`$`), httpmock.StatusResponse(http.StatusUnprocessableEntity))
	reg.Register(httpmock.REST("GET", `^/repos/octo/deleted$`), httpmock.StatusResponse(http.StatusNotFound))

	c := newTestClient(t, reg)
	c.anonHTTP = &http.Client{Transport: reg}
	states := c.CheckPins(context.Background(), []PinCheck{
		{Owner: "krzema12", Repo: "github-actions-typing", SHA: paritySHA},
		{Owner: "octo", Repo: "retag", SHA: paritySHA, Head: "v1.0.0"},
		{Owner: "octo", Repo: "retag", SHA: paritySHA, Head: "v2.0.0"},
		{Owner: "octo", Repo: "retag", SHA: parityMoved},
		{Owner: "octo", Repo: "deleted", SHA: paritySHA},
	})
	for _, s := range states {
		require.NoError(t, s.Err)
	}
	assert.Equal(t, PinState{NameWithOwner: "typesafegithub/github-actions-typing", OwnerID: 4, RepoID: 3, CommitFound: true}, states[0])
	assert.Equal(t, PinState{NameWithOwner: "octo/retag", CommitFound: true, HeadOID: parityMoved}, states[1], "a moved tag is not a missing commit")
	assert.Equal(t, PinState{NameWithOwner: "octo/retag", CommitFound: true, HeadOID: paritySHA}, states[2])
	assert.False(t, states[3].CommitFound)
	assert.Equal(t, PinState{RepoMissing: true, ViaFallback: true}, states[4])
	for _, req := range reg.Requests {
		assert.False(t, strings.HasSuffix(req.URL.Path, "/graphql"), "REST-only must not call GraphQL")
	}
}

func TestPinCheckRefs(t *testing.T) {
	tests := []struct {
		name string
		pc   PinCheck
		want []string
	}{
		{name: "no ref", pc: PinCheck{}},
		{name: "head tries tag then branch", pc: PinCheck{Head: "main"}, want: []string{"refs/tags/main", "refs/heads/main"}},
		{name: "a full ref name is never used as-is", pc: PinCheck{Head: "refs/tags/v1"}, want: []string{"refs/tags/refs/tags/v1", "refs/heads/refs/tags/v1"}},
		{name: "a pull request ref is not a tag or branch", pc: PinCheck{Head: "pull/1/head"}, want: []string{"refs/tags/pull/1/head", "refs/heads/pull/1/head"}},
		{name: "revision syntax is never looked up", pc: PinCheck{Head: "main~1"}},
		{name: "reflog syntax is never looked up", pc: PinCheck{Head: "v1@{1}"}},
		{name: "a describe name is never looked up", pc: PinCheck{Head: "v6.1.0-1-g373d742f768e"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, tt.pc.refs())
		})
	}
}

func TestCheckPins_RefLookup(t *testing.T) {
	t.Run("graphql prefers the tag over a same-named branch", func(t *testing.T) {
		reg := &httpmock.Registry{}
		defer reg.Verify(t)
		var gotQuery string
		var gotVars map[string]any
		reg.Register(httpmock.GraphQL(`commit: object\(oid`), httpmock.GraphQLQuery(`{"data": {
			"a0": {"nameWithOwner": "o/r", "commit": {"oid": "`+paritySHA+`"},
				"tag": {"oid": "`+paritySHA+`"}, "tagRef": {}, "head": {"oid": "`+parityMoved+`"}, "headRef": {}},
			"a1": {"nameWithOwner": "o/r", "commit": {"oid": "`+paritySHA+`"},
				"tag": null, "head": {"oid": "`+parityMoved+`"}, "headRef": {}}}}`,
			func(q string, v map[string]any) { gotQuery, gotVars = q, v }))

		states := newTestClient(t, reg).CheckPins(context.Background(), []PinCheck{
			{Owner: "o", Repo: "r", SHA: paritySHA, Head: "v1"},
			{Owner: "o", Repo: "r", SHA: paritySHA, Head: "v1.0.0"},
		})
		require.NoError(t, states[0].Err)
		require.NoError(t, states[1].Err)
		assert.Contains(t, gotQuery, "tag: object(expression: $tag0)")
		assert.Contains(t, gotQuery, "head: object(expression: $head0)")
		assert.Equal(t, "refs/tags/v1^{commit}", gotVars["tag0"])
		assert.Equal(t, "refs/heads/v1^{commit}", gotVars["head0"])
		assert.Equal(t, paritySHA, states[0].HeadOID)
		assert.Equal(t, parityMoved, states[1].HeadOID, "a ref falls back to a branch like the Actions backend resolver")
	})

	t.Run("rest falls back from tag to branch", func(t *testing.T) {
		t.Setenv("GH_ACTIONS_LOCK_DEPENDABOT_PROXY", "1")
		reg := &httpmock.Registry{}
		defer reg.Verify(t)
		reg.Register(httpmock.REST("GET", `^/repos/o/r$`), httpmock.JSONResponse(map[string]any{"full_name": "o/r"}))
		for range 2 {
			reg.Register(httpmock.REST("GET", `^/repos/o/r/commits/`+paritySHA+`$`), httpmock.JSONResponse(map[string]any{"sha": paritySHA}))
		}
		reg.Register(httpmock.REST("GET", `^/repos/o/r/git/ref/tags/main$`), httpmock.StatusResponse(http.StatusNotFound))
		reg.Register(httpmock.REST("GET", `^/repos/o/r/git/ref/heads/main$`), httpmock.JSONResponse(httpmock.GitRefResponse("refs/heads/main", parityMoved)))
		reg.Register(httpmock.REST("GET", `^/repos/o/r/git/ref/tags/pull/1/head$`), httpmock.StatusResponse(http.StatusNotFound))
		reg.Register(httpmock.REST("GET", `^/repos/o/r/git/ref/heads/pull/1/head$`), httpmock.StatusResponse(http.StatusNotFound))

		c := newTestClient(t, reg)
		c.anonHTTP = &http.Client{Transport: reg}
		states := c.CheckPins(context.Background(), []PinCheck{
			{Owner: "o", Repo: "r", SHA: paritySHA, Head: "main"},
			{Owner: "o", Repo: "r", SHA: paritySHA, Head: "pull/1/head"},
		})
		require.NoError(t, states[0].Err)
		require.NoError(t, states[1].Err)
		assert.Equal(t, parityMoved, states[0].HeadOID)
		assert.Empty(t, states[1].HeadOID, "a pull request head is not a tag or branch")
	})
}

// TestCheckPins_ExactRefName: a recorded ref counts only if a tag or branch
// exists by exactly that name.
func TestCheckPins_ExactRefName(t *testing.T) {
	t.Run("graphql ignores an expression that names no ref", func(t *testing.T) {
		reg := &httpmock.Registry{}
		defer reg.Verify(t)
		var gotQuery string
		var gotVars map[string]any
		reg.Register(httpmock.GraphQL(`commit: object\(oid`), httpmock.GraphQLQuery(`{"data": {
			"a0": {"nameWithOwner": "o/r", "commit": {"oid": "`+paritySHA+`"},
				"tag": {"oid": "`+paritySHA+`"}, "head": {"oid": "`+paritySHA+`"}, "tagRef": null, "headRef": null}}}`,
			func(q string, v map[string]any) { gotQuery, gotVars = q, v }))

		states := newTestClient(t, reg).CheckPins(context.Background(), []PinCheck{{Owner: "o", Repo: "r", SHA: paritySHA, Head: "v9"}})
		require.NoError(t, states[0].Err)
		assert.Contains(t, gotQuery, "tagRef: ref(qualifiedName: $tagRef0)")
		assert.Contains(t, gotQuery, "headRef: ref(qualifiedName: $headRef0)")
		assert.Equal(t, "refs/tags/v9", gotVars["tagRef0"])
		assert.Equal(t, "refs/heads/v9", gotVars["headRef0"])
		assert.Empty(t, states[0].HeadOID, "no such tag or branch")
	})

	t.Run("graphql never looks up a describe name", func(t *testing.T) {
		reg := &httpmock.Registry{}
		defer reg.Verify(t)
		var gotVars map[string]any
		reg.Register(httpmock.GraphQL(`commit: object\(oid`), httpmock.GraphQLQuery(`{"data": {
			"a0": {"nameWithOwner": "o/r", "commit": {"oid": "`+paritySHA+`"}}}}`,
			func(_ string, v map[string]any) { gotVars = v }))

		states := newTestClient(t, reg).CheckPins(context.Background(), []PinCheck{{Owner: "o", Repo: "r", SHA: paritySHA, Head: "v6.1.0-1-g373d742f768e"}})
		require.NoError(t, states[0].Err)
		assert.NotContains(t, gotVars, "tag0")
		assert.NotContains(t, gotVars, "tagRef0")
		assert.Empty(t, states[0].HeadOID)
	})

	t.Run("rest reads the exact ref", func(t *testing.T) {
		t.Setenv("GH_ACTIONS_LOCK_DEPENDABOT_PROXY", "1")
		reg := &httpmock.Registry{}
		defer reg.Verify(t)
		reg.Register(httpmock.REST("GET", `^/repos/o/r$`), httpmock.JSONResponse(map[string]any{"full_name": "o/r"}))
		reg.Register(httpmock.REST("GET", `^/repos/o/r/git/ref/tags/v1\.0\.0$`), httpmock.JSONResponse(httpmock.GitRefResponse("refs/tags/v1.0.0", paritySHA)))

		c := newTestClient(t, reg)
		c.anonHTTP = &http.Client{Transport: reg}
		states := c.CheckPins(context.Background(), []PinCheck{{Owner: "o", Repo: "r", SHA: paritySHA, Head: "v1.0.0"}})
		require.NoError(t, states[0].Err)
		assert.Equal(t, paritySHA, states[0].HeadOID)
		assert.True(t, states[0].CommitFound, "a ref at the commit proves it exists")
	})

	t.Run("rest ignores a name that is not an exact ref", func(t *testing.T) {
		t.Setenv("GH_ACTIONS_LOCK_DEPENDABOT_PROXY", "1")
		reg := &httpmock.Registry{}
		defer reg.Verify(t)
		reg.Register(httpmock.REST("GET", `^/repos/o/r$`), httpmock.JSONResponse(map[string]any{"full_name": "o/r"}))
		reg.Register(httpmock.REST("GET", `^/repos/o/r/commits/`+paritySHA+`$`), httpmock.JSONResponse(map[string]any{"sha": paritySHA}))
		reg.Register(httpmock.REST("GET", `^/repos/o/r/git/ref/tags/v9$`), httpmock.StatusResponse(http.StatusNotFound))
		// git/ref answers a name that only prefixes refs with an array.
		reg.Register(httpmock.REST("GET", `^/repos/o/r/git/ref/heads/v9$`),
			httpmock.JSONResponse([]any{httpmock.GitRefResponse("refs/heads/v9/x", paritySHA)}))

		c := newTestClient(t, reg)
		c.anonHTTP = &http.Client{Transport: reg}
		states := c.CheckPins(context.Background(), []PinCheck{{Owner: "o", Repo: "r", SHA: paritySHA, Head: "v9"}})
		require.NoError(t, states[0].Err)
		assert.Empty(t, states[0].HeadOID)
		for _, req := range reg.Requests {
			assert.NotContains(t, req.URL.Path, "/commits/refs", "commits/{ref} would evaluate the name")
		}
	})

	t.Run("rest never looks up a describe name", func(t *testing.T) {
		t.Setenv("GH_ACTIONS_LOCK_DEPENDABOT_PROXY", "1")
		reg := &httpmock.Registry{}
		defer reg.Verify(t)
		reg.Register(httpmock.REST("GET", `^/repos/o/r$`), httpmock.JSONResponse(map[string]any{"full_name": "o/r"}))
		reg.Register(httpmock.REST("GET", `^/repos/o/r/commits/`+paritySHA+`$`), httpmock.JSONResponse(map[string]any{"sha": paritySHA}))

		c := newTestClient(t, reg)
		c.anonHTTP = &http.Client{Transport: reg}
		states := c.CheckPins(context.Background(), []PinCheck{{Owner: "o", Repo: "r", SHA: paritySHA, Head: "v6.1.0-1-g373d742f768e"}})
		require.NoError(t, states[0].Err)
		assert.Empty(t, states[0].HeadOID)
	})

	t.Run("rest peels an annotated tag only once the ref exists", func(t *testing.T) {
		t.Setenv("GH_ACTIONS_LOCK_DEPENDABOT_PROXY", "1")
		reg := &httpmock.Registry{}
		defer reg.Verify(t)
		reg.Register(httpmock.REST("GET", `^/repos/o/r$`), httpmock.JSONResponse(map[string]any{"full_name": "o/r"}))
		reg.Register(httpmock.REST("GET", `^/repos/o/r/git/ref/tags/v1\.0\.0$`), httpmock.JSONResponse(map[string]any{
			"ref": "refs/tags/v1.0.0", "object": map[string]any{"sha": parityMoved, "type": "tag"},
		}))
		reg.Register(httpmock.REST("GET", `^/repos/o/r/commits/refs/tags/v1\.0\.0$`), httpmock.JSONResponse(map[string]any{"sha": paritySHA}))

		c := newTestClient(t, reg)
		c.anonHTTP = &http.Client{Transport: reg}
		states := c.CheckPins(context.Background(), []PinCheck{{Owner: "o", Repo: "r", SHA: paritySHA, Head: "v1.0.0"}})
		require.NoError(t, states[0].Err)
		assert.Equal(t, paritySHA, states[0].HeadOID, "the tag object peels to its commit")
		assert.True(t, states[0].CommitFound)
	})

	t.Run("rest ref lookup failure is inconclusive", func(t *testing.T) {
		t.Setenv("GH_ACTIONS_LOCK_DEPENDABOT_PROXY", "1")
		reg := &httpmock.Registry{}
		defer reg.Verify(t)
		reg.Register(httpmock.REST("GET", `^/repos/o/r$`), httpmock.JSONResponse(map[string]any{"full_name": "o/r"}))
		reg.Register(httpmock.REST("GET", `^/repos/o/r/git/ref/tags/main$`), httpmock.StatusResponse(http.StatusBadGateway))

		c := newTestClient(t, reg)
		c.anonHTTP = &http.Client{Transport: reg}
		states := c.CheckPins(context.Background(), []PinCheck{{Owner: "o", Repo: "r", SHA: paritySHA, Head: "main"}})
		require.Error(t, states[0].Err)
		assert.Empty(t, states[0].HeadOID)
	})
}

func TestTagExists(t *testing.T) {
	for _, proxy := range []bool{false, true} {
		t.Run(map[bool]string{false: "authenticated", true: "dependabot proxy"}[proxy], func(t *testing.T) {
			if proxy {
				t.Setenv("GH_ACTIONS_LOCK_DEPENDABOT_PROXY", "1")
			}
			reg := &httpmock.Registry{}
			defer reg.Verify(t)
			reg.Register(httpmock.REST("GET", `^/repos/o/r/git/ref/tags/v1$`), httpmock.JSONResponse(httpmock.GitRefResponse("refs/tags/v1", paritySHA)))
			reg.Register(httpmock.REST("GET", `^/repos/o/r/git/ref/tags/main$`), httpmock.StatusResponse(http.StatusNotFound))
			// git/ref answers a name that only prefixes refs with an array.
			reg.Register(httpmock.REST("GET", `^/repos/o/r/git/ref/tags/v2$`),
				httpmock.JSONResponse([]any{httpmock.GitRefResponse("refs/tags/v2/x", paritySHA)}))
			reg.Register(httpmock.REST("GET", `^/repos/o/r/git/ref/tags/down$`), httpmock.StatusResponse(http.StatusBadGateway))

			c := newTestClient(t, reg)
			c.anonHTTP = &http.Client{Transport: reg}
			ctx := context.Background()
			for name, want := range map[string]bool{"v1": true, "main": false, "v2": false} {
				got, err := c.TagExists(ctx, "o", "r", name)
				require.NoError(t, err, name)
				assert.Equal(t, want, got, name)
			}
			_, err := c.TagExists(ctx, "o", "r", "down")
			assert.Error(t, err, "a failed lookup is not a missing tag")
		})
	}
}

// TestCheckPins_UnpeelableTagShadowsBranch: a tag that exists but doesn't
// peel to a commit still wins over a same-named branch, as in the Actions
// backend; the ref resolves to no commit rather than the branch's.
func TestCheckPins_UnpeelableTagShadowsBranch(t *testing.T) {
	t.Run("graphql", func(t *testing.T) {
		reg := &httpmock.Registry{}
		defer reg.Verify(t)
		reg.Register(httpmock.GraphQL(`commit: object\(oid`), httpmock.GraphQLQuery(`{"data": {
			"a0": {"nameWithOwner": "o/r", "commit": {"oid": "`+paritySHA+`"},
				"tag": null, "tagRef": {}, "head": {"oid": "`+paritySHA+`"}, "headRef": {}}}}`, func(string, map[string]any) {}))
		states := newTestClient(t, reg).CheckPins(context.Background(), []PinCheck{{Owner: "o", Repo: "r", SHA: paritySHA, Head: "v1"}})
		require.NoError(t, states[0].Err)
		assert.Empty(t, states[0].HeadOID)
	})
	t.Run("rest", func(t *testing.T) {
		t.Setenv("GH_ACTIONS_LOCK_DEPENDABOT_PROXY", "1")
		reg := &httpmock.Registry{}
		defer reg.Verify(t)
		reg.Register(httpmock.REST("GET", `^/repos/o/r$`), httpmock.JSONResponse(map[string]any{"full_name": "o/r"}))
		reg.Register(httpmock.REST("GET", `^/repos/o/r/git/ref/tags/v1$`), httpmock.JSONResponse(map[string]any{
			"ref": "refs/tags/v1", "object": map[string]any{"sha": parityMoved, "type": "tag"},
		}))
		reg.Register(httpmock.REST("GET", `^/repos/o/r/commits/refs/tags/v1$`), httpmock.StatusResponse(http.StatusUnprocessableEntity))
		reg.Register(httpmock.REST("GET", `^/repos/o/r/commits/`+paritySHA+`$`), httpmock.JSONResponse(map[string]any{"sha": paritySHA}))
		c := newTestClient(t, reg)
		c.anonHTTP = &http.Client{Transport: reg}
		states := c.CheckPins(context.Background(), []PinCheck{{Owner: "o", Repo: "r", SHA: paritySHA, Head: "v1"}})
		require.NoError(t, states[0].Err)
		assert.Empty(t, states[0].HeadOID)
		for _, req := range reg.Requests {
			assert.NotContains(t, req.URL.Path, "/git/ref/heads/", "an existing tag shadows the branch")
		}
	})
}
