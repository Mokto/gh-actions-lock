package pipeline

import (
	"errors"
	"testing"

	parserlock "github.com/github/actions-lockfile/go/pkg/lockfile"
	"github.com/github/gh-actions-lock/internal/ghapi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestReadManifest(t *testing.T) {
	const sha = "ABCDEF0123456789ABCDEF0123456789ABCDEF01"
	file := parserlock.File{Dependencies: map[string]parserlock.Action{
		"Acme/Comp@v1":  {Commit: "sha1-1111111111111111111111111111111111111111", Uses: []string{"acme/child@v2", "acme/gone@v1", "acme/pinned@" + sha}},
		"acme/child@v2": {Commit: "sha1-2222222222222222222222222222222222222222"},
		"acme/pinned@abcdef0123456789abcdef0123456789abcdef01": {Commit: "sha1-abcdef0123456789abcdef0123456789abcdef01"},
	}}
	byKey := map[string]string{}
	for k := range file.Dependencies {
		p, _ := parserlock.ParsePin(k)
		byKey[pinKey(p.NWO, p.Ref)] = k
	}
	n := manifestNode{key: "Acme/Comp@v1"}
	ok := ghapi.ActionFileResult{CommitOID: "1111111111111111111111111111111111111111"}

	t.Run("a missing commit parity didn't check is unverified", func(t *testing.T) {
		assert.Error(t, readManifest(n, file, byKey, ghapi.ActionFileResult{Err: &ghapi.RefNotFoundError{Err: errors.New("gone")}}).err)
	})
	t.Run("no result is unverified", func(t *testing.T) {
		assert.Error(t, readManifest(n, file, byKey, ghapi.ActionFileResult{}).err)
	})
	t.Run("a Dockerfile is a leaf", func(t *testing.T) {
		res := ok
		res.Dockerfile = true
		rd := readManifest(n, file, byKey, res)
		assert.False(t, rd.missing)
		assert.Empty(t, rd.edges)
	})
	t.Run("edges", func(t *testing.T) {
		res := ok
		res.ActionYML = "runs:\n  using: composite\n  steps:\n" +
			"    - uses: ACME/child@v2\n" +
			"    - uses: acme/gone@v1\n" +
			"    - uses: acme/pinned/sub@" + sha + "\n" +
			"    - uses: acme/comp/other@v1\n" +
			"    - uses: $/third\n" +
			"    - uses: acme/new@v1\n"
		rd := readManifest(n, file, byKey, res)
		require.NotNil(t, rd)
		require.NoError(t, rd.err)
		want := []manifestEdge{
			{to: manifestNode{key: "acme/child@v2"}},
			{}, // recorded but dangling: the structural check reports it
			{to: manifestNode{key: "acme/pinned@abcdef0123456789abcdef0123456789abcdef01", path: "sub"}},
			{to: manifestNode{key: "Acme/Comp@v1", path: "other"}},
			{to: manifestNode{key: "Acme/Comp@v1", path: "third"}},
			{omitted: true},
		}
		require.Len(t, rd.edges, len(want))
		for i, w := range want {
			assert.Equal(t, w.to, rd.edges[i].to, rd.edges[i].use)
			assert.Equal(t, w.omitted, rd.edges[i].omitted, rd.edges[i].use)
		}
	})
}
