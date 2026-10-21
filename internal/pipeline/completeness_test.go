package pipeline

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/github/gh-actions-lock/internal/lockfile"
	"github.com/github/gh-actions-lock/internal/pipeline/checks"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLockedManifestInspectionsFollowPreviewInputs(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	const workflow = "ci.yml"
	require.NoError(t, os.WriteFile(workflow, []byte(`
on: push
jobs:
  ci:
    runs-on: ubuntu-latest
    steps:
      - uses: o/a@v1
`), 0o600))
	body := "version: v0.0.3\nworkflows:\n  ci.yml:\n    - o/a@v1\ndependencies:\n" +
		"  o/a@v1:\n    ref: v1\n    commit: sha1-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\n    owner_id: 1\n    repo_id: 2\n"
	path := filepath.Join(dir, "actions.lock")
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
	store, err := lockfile.LoadStateAt(path, nil)
	require.NoError(t, err)
	parsed := ParseAll([]string{workflow}, store)
	report := &checks.Report{Workflows: []checks.WorkflowReport{{Path: workflow}}}
	opts := RunOptions{Store: store, Relock: true}
	inspected := map[int]map[string]bool{}
	replacing := true
	proof := func(d checks.Deferred) bool {
		assert.Equal(t, "o/a@v1", d.Key)
		assert.True(t, d.Finding.IncompleteLockfile)
		return replacing
	}

	inspections, owners := lockedManifestInspections(opts, parsed, report, nil, proof, inspected)
	require.Len(t, inspections, 1)
	assert.Empty(t, inspections[0].keys, "actual replacement requires no retained-manifest read")
	assert.Empty(t, owners)

	replacing = false
	inspections, owners = lockedManifestInspections(opts, parsed, report, nil, proof, inspected)
	require.Len(t, inspections, 1, "a writer dropped by Audit makes its inputs retained again")
	assert.True(t, inspections[0].keys["o/a@v1"])
	assert.Len(t, owners, 1)

	inspections, owners = lockedManifestInspections(opts, parsed, report, nil, proof, inspected)
	assert.Empty(t, inspections, "unchanged retained inputs do not repeat immutable traversal")
	assert.Empty(t, owners)
}
