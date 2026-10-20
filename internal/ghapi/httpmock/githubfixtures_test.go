package httpmock

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func graphQLRequest(t *testing.T, body string) *http.Request {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, "https://api.github.com/graphql", strings.NewReader(body))
	require.NoError(t, err)
	return req
}

func TestStubRepoAnswersEveryRefAtOID(t *testing.T) {
	reg := &Registry{}
	reg.StubRepo("octo/action", "abc")

	resp, err := reg.RoundTrip(graphQLRequest(t, `{"query":"q","variables":{"owner0":"octo","name0":"action"}}`))
	require.NoError(t, err)
	var got struct {
		Data map[string]map[string]any `json:"data"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&got))
	repo := got.Data["a0"]
	assert.Equal(t, "octo/action", repo["nameWithOwner"])
	for _, k := range []string{"commit", "tag", "head"} {
		assert.Equal(t, map[string]any{"oid": "abc"}, repo[k], k)
	}
	reg.Verify(t)
}

func TestParityIntactEchoesEveryPin(t *testing.T) {
	req := graphQLRequest(t, `{"query":"commit: object(oid: $oid0)","variables":{"owner0":"o","name0":"r","oid0":"s0","owner1":"p","name1":"q","oid1":"s1"}}`)
	require.True(t, ParityQuery(req))

	resp, err := ParityIntact(req)
	require.NoError(t, err)
	var got struct {
		Data map[string]map[string]any `json:"data"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&got))
	require.Len(t, got.Data, 2)
	assert.Equal(t, "p/q", got.Data["a1"]["nameWithOwner"])
	assert.Equal(t, map[string]any{"oid": "s1"}, got.Data["a1"]["tag"])
}

func TestCountingTransport(t *testing.T) {
	ct := &CountingTransport{Next: RoundTripFunc(StatusResponse(http.StatusNoContent))}
	for _, path := range []string{"/graphql", "/repos/o/r"} {
		req, err := http.NewRequest(http.MethodPost, "https://api.github.com"+path, strings.NewReader(`{}`))
		require.NoError(t, err)
		resp, err := ct.RoundTrip(req)
		require.NoError(t, err)
		assert.Equal(t, http.StatusNoContent, resp.StatusCode)
	}
	assert.Equal(t, 2, ct.Count())
	assert.Equal(t, []string{"/graphql", "/repos/o/r"}, ct.Paths())
	assert.Equal(t, 1, ct.CountMatching(GraphQL("")))
}
