package lockfile

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/github/gh-actions-lock/internal/dep"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// noNetwork fails the test if a preview asks for repo IDs.
type noNetwork struct{ t *testing.T }

func (n noNetwork) RepoIDs(context.Context, string, string, string) (int64, int64, error) {
	n.t.Fatal("a preview must not look up repo IDs")
	return 0, 0, nil
}

func TestPreview(t *testing.T) {
	a, b := strings.Repeat("a", 40), strings.Repeat("b", 40)
	path := filepath.Join(t.TempDir(), "actions.lock")
	body := "version: v0.0.3\nworkflows:\n  .github/workflows/ci.yml:\n    - o/root@v1\ndependencies:\n" +
		"  o/root@v1:\n    ref: v1\n    commit: sha1-" + a + "\n    owner_id: 1\n    repo_id: 2\n    uses:\n      - o/child@v2\n" +
		"  o/child@v2:\n    ref: v2\n    commit: sha1-" + a + "\n    owner_id: 1\n    repo_id: 3\n"
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
	store, err := LoadStateAt(path, noNetwork{t})
	require.NoError(t, err)
	before := store.File()
	beforeUses := append([]string(nil), before.Dependencies["o/root@v1"].Uses...)

	preview := store.Preview()
	// o/new has no cached IDs: the preview fills a placeholder offline.
	newDep := dep.Dependency{NWO: "o/new", Ref: "v1", SHA: b}
	rootDep := dep.Dependency{NWO: "o/root", Ref: "v1", SHA: b}
	require.NoError(t, preview.Set(context.Background(), ".github/workflows/ci.yml",
		[]dep.Dependency{rootDep, newDep}, nil, map[string]bool{rootDep.Key(): true, newDep.Key(): true}))
	preview.GC()

	got, ok := preview.Entry("o/root@v1")
	require.True(t, ok)
	assert.Equal(t, "sha1-"+b, got.Commit)
	assert.Equal(t, []string{"o/child@v2"}, got.Uses, "writing a workflow keeps recorded edges")
	_, ok = preview.Entry("o/child@v2")
	assert.True(t, ok, "a recorded edge keeps its child through GC")
	_, ok = preview.Entry("o/new@v1")
	assert.True(t, ok)

	after := store.File()
	assert.Equal(t, "sha1-"+a, after.Dependencies["o/root@v1"].Commit, "the store is untouched")
	assert.Equal(t, beforeUses, after.Dependencies["o/root@v1"].Uses)
	assert.NotContains(t, after.Dependencies, "o/new@v1")
	assert.ErrorIs(t, preview.Save(), errPreview)
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, body, string(raw), "nothing reaches disk")
}
