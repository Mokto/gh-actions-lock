package httpmock

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGraphQLAndRegistry(t *testing.T) {
	reg := &Registry{}
	reg.Register(
		GraphQL(`query Example\b`),
		JSONResponse(map[string]any{"data": map[string]any{"ok": true}}),
	)

	req, err := http.NewRequest(http.MethodPost, "https://api.github.com/graphql", io.NopCloser(strings.NewReader(`{"query":"query Example { viewer { login } }"}`)))
	if err != nil {
		t.Fatalf("creating request: %v", err)
	}

	resp, err := reg.RoundTrip(req)
	if err != nil {
		t.Fatalf("roundtrip failed: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected status 200, got %d", resp.StatusCode)
	}

	reg.Verify(t)
}

func TestGraphQLQueryResponder(t *testing.T) {
	reg := &Registry{}
	called := false
	reg.Register(
		GraphQL(`query Example\b`),
		GraphQLQuery(`{"data":{"viewer":{"login":"mona"}}}`, func(query string, variables map[string]any) {
			called = true
			if query == "" {
				t.Fatal("expected query to be passed to callback")
			}
		}),
	)

	req, err := http.NewRequest(http.MethodPost, "https://api.github.com/graphql", io.NopCloser(strings.NewReader(`{"query":"query Example { viewer { login } }","variables":{"x":1}}`)))
	if err != nil {
		t.Fatalf("creating request: %v", err)
	}

	_, err = reg.RoundTrip(req)
	if err != nil {
		t.Fatalf("roundtrip failed: %v", err)
	}
	if !called {
		t.Fatal("expected GraphQLQuery callback to run")
	}
	reg.Verify(t)
}

func TestServeHTTP(t *testing.T) {
	reg := &Registry{}
	reg.RegisterReusable(REST(http.MethodGet, `/repos/o/r$`), JSONResponse(map[string]any{"id": 1}))
	srv := httptest.NewServer(reg)
	defer srv.Close()

	for range 2 {
		resp, err := http.Get(srv.URL + "/api/v3/repos/o/r")
		require.NoError(t, err)
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		assert.Equal(t, http.StatusOK, resp.StatusCode)
		assert.JSONEq(t, `{"id":1}`, string(body))
		assert.Equal(t, "application/json", resp.Header.Get("Content-Type"))
	}

	resp, err := http.Get(srv.URL + "/missing")
	require.NoError(t, err)
	resp.Body.Close()
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
	reg.Verify(t)
}
