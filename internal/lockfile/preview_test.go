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
	rootDep := dep.Dependency{NWO: "o/root", Ref: "v1", SHA: b, FromLock: true}
	require.NoError(t, preview.Set(context.Background(), ".github/workflows/ci.yml",
		[]dep.Dependency{rootDep, newDep}, nil, map[string]bool{rootDep.Key(): true, newDep.Key(): true}))
	preview.GC()

	got, ok := preview.Entry("o/root@v1")
	require.True(t, ok)
	assert.Equal(t, "sha1-"+b, got.Commit)
	assert.Equal(t, []string{"o/child@v2"}, got.Uses, "writing a workflow keeps an unread entry's recorded edges")
	_, ok = preview.Entry("o/child@v2")
	assert.True(t, ok, "a recorded edge keeps its child through GC")
	assert.Equal(t, []string{"o/root@v1"}, preview.UsedBy("o/child@v2"))
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

type fixedIDs struct{}

func (fixedIDs) RepoIDs(context.Context, string, string, string) (int64, int64, error) {
	return 7, 8, nil
}

// TestPreviewProjectsRealSet: a write to a preview leaves the same keys,
// refs, commits, edges and workflow lists as the same write to the store;
// only the owner/repo IDs it couldn't look up differ. The preview's
// placeholder IDs never reach the store's cache.
func TestPreviewProjectsRealSet(t *testing.T) {
	a, b := strings.Repeat("a", 40), strings.Repeat("b", 40)
	path := filepath.Join(t.TempDir(), "actions.lock")
	body := "version: v0.0.3\nworkflows:\n  .github/workflows/ci.yml:\n    - o/root@v1\n  .github/workflows/other.yml:\n    - o/keep@v1\ndependencies:\n" +
		"  o/root@v1:\n    ref: v1\n    commit: sha1-" + a + "\n    owner_id: 1\n    repo_id: 2\n    uses:\n      - o/child@v2\n" +
		"  o/child@v2:\n    ref: v2\n    commit: sha1-" + a + "\n    owner_id: 1\n    repo_id: 3\n" +
		"  o/keep@v1:\n    ref: v1\n    commit: sha1-" + a + "\n    owner_id: 1\n    repo_id: 4\n" +
		"  o/orphan@v1:\n    ref: v1\n    commit: sha1-" + a + "\n    owner_id: 1\n    repo_id: 5\n"
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
	store, err := LoadStateAt(path, fixedIDs{})
	require.NoError(t, err)
	preview := store.Preview()

	deps := []dep.Dependency{{NWO: "o/root", Ref: "v1", SHA: b}, {NWO: "o/new", Ref: "v1", SHA: b}}
	direct := map[string]bool{deps[0].Key(): true, deps[1].Key(): true}
	ctx := context.Background()
	require.NoError(t, preview.Set(ctx, ".github/workflows/ci.yml", deps, nil, direct))
	preview.GC()
	assert.Zero(t, store.RecordedRepoID("o", "new"), "a preview never fills the store's ID cache")

	require.NoError(t, store.Set(ctx, ".github/workflows/ci.yml", deps, nil, direct))
	store.GC()
	project := func(s *State) (map[string][3]string, map[string][]string) {
		f := s.File()
		out := map[string][3]string{}
		for k, a := range f.Dependencies {
			out[k] = [3]string{a.Ref, a.Commit, strings.Join(a.Uses, ",")}
		}
		return out, f.Workflows
	}
	wantDeps, wantWorkflows := project(store)
	gotDeps, gotWorkflows := project(preview)
	assert.Equal(t, wantDeps, gotDeps)
	assert.Equal(t, wantWorkflows, gotWorkflows)
	assert.NotContains(t, gotDeps, "o/orphan@v1", "GC drops what no workflow reaches, as Save does")
}
