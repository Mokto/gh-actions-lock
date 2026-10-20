package pin

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/github/gh-actions-lock/internal/dep"
	"github.com/github/gh-actions-lock/internal/lockfile"
	"github.com/github/gh-actions-lock/internal/workflowfile"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestIsPinOrVerified(t *testing.T) {
	tests := []struct {
		name string
		r    Resolution
		want bool
	}{
		{"pinned", Pinned, true},
		{"verified", Verified, true},
		{"investigate", Investigate, false},
		{"skipped", Skipped, false},
		{"unresolved", Unresolved, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, isPinOrVerified(tt.r))
		})
	}
}

func TestGroupPinnedByWorkflow(t *testing.T) {
	rec := &Record{
		Entries: []Entry{
			{NWO: "actions/checkout", Ref: "v4", SHA: "aaa", Resolution: Pinned, OnBranch: "main", Tag: "v4.1.0", Workflows: []string{"ci.yml", "release.yml"}},
			{NWO: "actions/setup-go", Ref: "v5", SHA: "bbb", Resolution: Verified, Workflows: []string{"ci.yml"}},
			{NWO: "bad/action", Ref: "v1", SHA: "ccc", Resolution: Investigate, Workflows: []string{"ci.yml"}},
			{NWO: "actions/cache", Ref: "v3", SHA: "ddd", Resolution: Skipped, Workflows: []string{"release.yml"}},
		},
	}

	got := groupPinnedByWorkflow(rec)

	// Only pinned and verified entries should appear
	require.Contains(t, got, "ci.yml")
	require.Contains(t, got, "release.yml")

	ci := got["ci.yml"]
	require.Len(t, ci, 2, "ci.yml should have checkout and setup-go")
	assert.Equal(t, "actions/checkout", ci[0].NWO)
	assert.Equal(t, "aaa", ci[0].SHA)
	assert.Equal(t, "main", ci[0].Branch)
	assert.Equal(t, "v4.1.0", ci[0].Tag)
	assert.Equal(t, "actions/setup-go", ci[1].NWO)

	release := got["release.yml"]
	require.Len(t, release, 1, "release.yml should have only checkout")
	assert.Equal(t, "actions/checkout", release[0].NWO)
}

func TestGroupPinnedByWorkflow_empty(t *testing.T) {
	rec := &Record{}
	got := groupPinnedByWorkflow(rec)
	assert.Empty(t, got)
}

func TestBuildParentMap(t *testing.T) {
	rec := &Record{
		Entries: []Entry{
			{NWO: "a/b", Ref: "v1", Resolution: Pinned, Workflows: []string{"ci.yml", "release.yml"}, RequiredBy: []string{"parent/action@v2"}},
			{NWO: "c/d", Ref: "v2", Resolution: Verified, Workflows: []string{"ci.yml"}},
			{NWO: "e/f", Ref: "v3", Resolution: Investigate, Workflows: []string{"ci.yml"}, RequiredBy: []string{"x/y@v1"}},
		},
	}

	pm := buildParentMap(rec, "ci.yml")
	assert.Equal(t, []string{"parent/action@v2"}, pm["a/b@v1"])
	assert.NotContains(t, pm, "c/d@v2", "entries without RequiredBy should not appear")
	assert.NotContains(t, pm, "e/f@v3", "investigate entries should be excluded")
}

func TestBuildParentMap_filters_workflow(t *testing.T) {
	rec := &Record{
		Entries: []Entry{
			{NWO: "a/b", Ref: "v1", Resolution: Pinned, Workflows: []string{"release.yml"}, RequiredBy: []string{"p"}},
		},
	}
	pm := buildParentMap(rec, "ci.yml")
	assert.Empty(t, pm, "should not include entries from other workflows")
}

func TestBuildDirectKeys(t *testing.T) {
	rec := &Record{
		Entries: []Entry{
			{NWO: "a/b", Ref: "v1", Resolution: Pinned, Direct: true, Workflows: []string{"ci.yml"}},
			{NWO: "c/d", Ref: "v2", Resolution: Verified, Direct: false, Workflows: []string{"ci.yml"}},
			{NWO: "e/f", Ref: "v3", Resolution: Pinned, Direct: true, Workflows: []string{"release.yml"}},
			{NWO: "g/h", Ref: "v4", Resolution: Investigate, Direct: true, Workflows: []string{"ci.yml"}},
		},
	}

	keys := buildDirectKeys(rec, "ci.yml")
	assert.True(t, keys["a/b@v1"])
	assert.NotContains(t, keys, "c/d@v2", "non-direct should be excluded")
	assert.NotContains(t, keys, "e/f@v3", "wrong workflow should be excluded")
	assert.NotContains(t, keys, "g/h@v4", "investigate should be excluded")
}

func TestCommitDependencyFreeWorkflow(t *testing.T) {
	tests := []struct {
		name                   string
		skipNewWorkflowEntries bool
		wantEntry              bool
	}{
		{"records empty entry", false, true},
		{"skips onboarding", true, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			workflowPath := filepath.Join(".github", "workflows", "ci.yml")
			require.NoError(t, os.MkdirAll(filepath.Join(dir, filepath.Dir(workflowPath)), 0o755))
			require.NoError(t, os.WriteFile(filepath.Join(dir, workflowPath), []byte("on: push\n"), 0o644))
			t.Chdir(dir)

			store, err := lockfile.LoadState(dir, fakeMeta{})
			require.NoError(t, err)
			rec := &Record{Workflows: []WorkflowPlan{{Path: workflowPath}}}

			require.NoError(t, Commit(context.Background(), rec, store, &CommitOptions{SkipNewWorkflowEntries: tt.skipNewWorkflowEntries}))
			assert.Equal(t, tt.wantEntry, store.HasWorkflow(workflowfile.KeyFromPath(workflowPath)))
		})
	}
}

func TestCommitPartialResolution(t *testing.T) {
	dir := t.TempDir()
	workflowPath := filepath.Join(".github", "workflows", "ci.yml")
	require.NoError(t, os.MkdirAll(filepath.Join(dir, filepath.Dir(workflowPath)), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, workflowPath), []byte("on: push\n"), 0o644))
	t.Chdir(dir)

	store, err := lockfile.LoadState(dir, fakeMeta{})
	require.NoError(t, err)
	rec := &Record{
		Entries: []Entry{{
			NWO: "actions/checkout", Ref: "v4", SHA: strings.Repeat("a", 40),
			Resolution: Pinned, Workflows: []string{workflowPath}, Direct: true,
		}},
		Workflows: []WorkflowPlan{{Path: workflowPath}},
	}

	require.NoError(t, Commit(context.Background(), rec, store, nil))
	deps, err := store.Get(workflowfile.KeyFromPath(workflowPath))
	require.NoError(t, err)
	assert.Len(t, deps, 1)
}

func TestCommitSkipNewWorkflowEntriesPreventsRewrites(t *testing.T) {
	dir := t.TempDir()
	workflowPath := filepath.Join(".github", "workflows", "ci.yml")
	actionPath := filepath.Join(".github", "actions", "local", "action.yml")
	oldUses := "actions/checkout@" + strings.Repeat("a", 40)
	newUses := "actions/checkout@v4.2.0"
	workflowContent := []byte("on: push\njobs:\n  test:\n    runs-on: ubuntu-latest\n    steps:\n      - uses: " + oldUses + "\n")
	actionContent := []byte("name: local\nruns:\n  using: composite\n  steps:\n    - uses: " + oldUses + "\n")
	for path, content := range map[string][]byte{workflowPath: workflowContent, actionPath: actionContent} {
		require.NoError(t, os.MkdirAll(filepath.Join(dir, filepath.Dir(path)), 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(dir, path), content, 0o644))
	}
	t.Chdir(dir)

	store, err := lockfile.LoadState(dir, fakeMeta{})
	require.NoError(t, err)
	rec := &Record{Workflows: []WorkflowPlan{{
		Path:            workflowPath,
		Rewrites:        map[string]string{oldUses: newUses},
		SelfActionFiles: []string{actionPath},
		// A skipped workflow's required rewrites must not fail the run.
		RequiredRewrites: map[string]string{"gone/action@v1": "new/action@v1"},
	}}}

	require.NoError(t, Commit(context.Background(), rec, store, &CommitOptions{SkipNewWorkflowEntries: true}))
	workflowAfter, err := os.ReadFile(workflowPath)
	require.NoError(t, err)
	actionAfter, err := os.ReadFile(actionPath)
	require.NoError(t, err)
	assert.Equal(t, workflowContent, workflowAfter)
	assert.Equal(t, actionContent, actionAfter)
	assert.False(t, store.HasWorkflow(workflowfile.KeyFromPath(workflowPath)))
}

func TestValidateRequiredRewritesRecordsFiles(t *testing.T) {
	dir := t.TempDir()
	workflowPath := filepath.Join(".github", "workflows", "ci.yml")
	actionPath := filepath.Join("actions", "a", "action.yml")
	files := map[string]string{
		workflowPath: "on: push\njobs:\n  test:\n    runs-on: ubuntu-latest\n    steps:\n      - uses: $/actions/a\n",
		actionPath:   "name: a\nruns:\n  using: composite\n  steps:\n    - uses: old/action@v1\n",
	}
	for path, content := range files {
		require.NoError(t, os.MkdirAll(filepath.Join(dir, filepath.Dir(path)), 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(dir, path), []byte(content), 0o644))
	}
	t.Chdir(dir)

	plans := []WorkflowPlan{{
		Path:             workflowPath,
		SelfActionFiles:  []string{actionPath},
		RequiredRewrites: map[string]string{"old/action@v1": "new/action@v1"},
	}}
	require.NoError(t, validateRequiredRewrites(plans))
	assert.Equal(t, []string{actionPath}, plans[0].RewrittenIn["old/action@v1"])
}

func TestCommitRemovesDependenciesDroppedFromWorkflow(t *testing.T) {
	dir := t.TempDir()
	workflowPath := filepath.Join(".github", "workflows", "ci.yml")
	require.NoError(t, os.MkdirAll(filepath.Join(dir, filepath.Dir(workflowPath)), 0o755))
	workflow := []byte(`on: push
jobs:
  lint:
    uses: owner/reusable/.github/workflows/lint.yml@main
`)
	require.NoError(t, os.WriteFile(filepath.Join(dir, workflowPath), workflow, 0o644))
	t.Chdir(dir)

	store, err := lockfile.LoadState(dir, fakeMeta{})
	require.NoError(t, err)
	oldParent := dep.Dependency{NWO: "owner/action", Ref: "main", SHA: strings.Repeat("1", 40), HashAlgo: "sha1"}
	oldChild := dep.Dependency{NWO: "actions/setup-go", Ref: "v5", SHA: strings.Repeat("2", 40), HashAlgo: "sha1"}
	keep := dep.Dependency{NWO: "actions/checkout", Ref: "v7", SHA: strings.Repeat("3", 40), HashAlgo: "sha1"}
	require.NoError(t, store.Set(context.Background(), workflowfile.KeyFromPath(workflowPath),
		[]dep.Dependency{oldParent, oldChild, keep},
		map[string][]string{oldChild.Key(): {oldParent.Key()}},
		map[string]bool{oldParent.Key(): true, keep.Key(): true}))
	require.NoError(t, store.Save())

	rec := &Record{
		Entries: []Entry{{
			NWO:        keep.NWO,
			Ref:        keep.Ref,
			SHA:        keep.SHA,
			Resolution: Verified,
			Direct:     true,
			Workflows:  []string{workflowPath},
		}},
		Workflows: []WorkflowPlan{{Path: workflowPath}},
	}

	require.NoError(t, Commit(context.Background(), rec, store, nil))

	gotWorkflow, err := os.ReadFile(filepath.Join(dir, workflowPath))
	require.NoError(t, err)
	assert.Equal(t, workflow, gotWorkflow)

	got, err := os.ReadFile(filepath.Join(dir, ".github", "workflows", "actions.lock"))
	require.NoError(t, err)
	assert.Contains(t, string(got), "actions/checkout@v7")
	assert.NotContains(t, string(got), "owner/action@main")
	assert.NotContains(t, string(got), "actions/setup-go@v5")
}

func TestCommitRekeysAnnotatedTagObjectWithUses(t *testing.T) {
	const tagObjectSHA = "1111111111111111111111111111111111111111"
	const commitSHA = "2222222222222222222222222222222222222222"
	workflowPath := filepath.Join(".github", "workflows", "ci.yml")
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, filepath.Dir(workflowPath)), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, workflowPath), []byte(`on: push
jobs:
  build:
    runs-on: ubuntu-latest
    steps:
      - uses: owner/action@`+tagObjectSHA+`
`), 0o644))
	t.Chdir(dir)

	store, err := lockfile.LoadState(dir, fakeMeta{})
	require.NoError(t, err)
	parent := dep.Dependency{
		NWO: "owner/action", Ref: tagObjectSHA, SHA: tagObjectSHA,
		HashAlgo: "sha1", Tag: "v3.2.0",
	}
	child := dep.Dependency{
		NWO: "actions/checkout", Ref: "v4", SHA: strings.Repeat("3", 40),
		HashAlgo: "sha1",
	}
	require.NoError(t, store.Set(context.Background(), workflowfile.KeyFromPath(workflowPath),
		[]dep.Dependency{parent, child},
		map[string][]string{child.Key(): {parent.Key()}},
		map[string]bool{parent.Key(): true}))
	require.NoError(t, store.Save())

	rec := &Record{
		Entries: []Entry{{
			NWO:          parent.NWO,
			Ref:          "v3.2.0",
			SHA:          commitSHA,
			Resolution:   Verified,
			AutoFixedRef: tagObjectSHA,
			Direct:       true,
			Workflows:    []string{workflowPath},
		}},
		Workflows: []WorkflowPlan{{
			Path: workflowPath,
			Rewrites: map[string]string{
				parent.NWO + "@" + tagObjectSHA: parent.NWO + "@v3.2.0",
			},
		}},
	}
	require.NoError(t, Commit(context.Background(), rec, store, nil))

	got, err := os.ReadFile(filepath.Join(dir, ".github", "workflows", "actions.lock"))
	require.NoError(t, err)
	assert.Contains(t, string(got), "'owner/action@v3.2.0'")
	assert.Contains(t, string(got), "uses:\n            - 'actions/checkout@v4'")
	assert.NotContains(t, string(got), "'owner/action@"+tagObjectSHA+"'")
}

// TestCommitRejectedEntryRefWritesNothing: a planned entry the lockfile
// refuses must fail before any workflow is rewritten.
func TestCommitRejectedEntryRefWritesNothing(t *testing.T) {
	dir := t.TempDir()
	workflowPath := filepath.Join(".github", "workflows", "ci.yml")
	sha := strings.Repeat("a", 40)
	content := []byte("on: push\njobs:\n  test:\n    runs-on: ubuntu-latest\n    steps:\n      - uses: actions/checkout@" + sha + "\n")
	require.NoError(t, os.MkdirAll(filepath.Join(dir, filepath.Dir(workflowPath)), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, workflowPath), content, 0o644))
	t.Chdir(dir)

	store, err := lockfile.LoadState(dir, fakeMeta{})
	require.NoError(t, err)
	rec := &Record{
		Entries: []Entry{{
			NWO: "actions/checkout", Ref: "release-g1234", SHA: sha, Tag: "release-g1234",
			Resolution: Pinned, Workflows: []string{workflowPath}, Direct: true,
		}},
		Workflows: []WorkflowPlan{{
			Path:     workflowPath,
			Rewrites: map[string]string{"actions/checkout@" + sha: "actions/checkout@release-g1234"},
		}},
	}

	err = Commit(context.Background(), rec, store, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "release-g1234")
	after, err := os.ReadFile(filepath.Join(dir, workflowPath))
	require.NoError(t, err)
	assert.Equal(t, string(content), string(after), "the workflow is not rewritten")
	assert.NoFileExists(t, filepath.Join(dir, ".github", "workflows", "actions.lock"))
}

// TestUpdateLockfileRepairsOnlyForWriters: an entry repair applies only when
// a workflow it was planned for is written, so a preview without that
// workflow (Audit dropping a writer) leaves the entry as recorded.
func TestUpdateLockfileRepairsOnlyForWriters(t *testing.T) {
	sha := strings.Repeat("a", 40)
	key := "actions/cache@" + sha
	for _, written := range []bool{true, false} {
		dir := t.TempDir()
		require.NoError(t, os.MkdirAll(filepath.Join(dir, ".github", "workflows"), 0o755))
		lock := "version: 'v0.0.3'\nworkflows:\n    '.github/workflows/a.yml':\n        - 'actions/checkout@v4'\n" +
			"dependencies:\n    '" + key + "':\n        ref: '" + sha + "'\n        commit: 'sha1-" + sha + "'\n        owner_id: 1\n        repo_id: 1\n" +
			"    'actions/checkout@v4':\n        ref: 'v4'\n        commit: 'sha1-" + sha + "'\n        owner_id: 1\n        repo_id: 1\n        uses:\n            - '" + key + "'\n"
		require.NoError(t, os.WriteFile(filepath.Join(dir, ".github", "workflows", "actions.lock"), []byte(lock), 0o600))
		store, err := lockfile.LoadState(dir, fakeMeta{})
		require.NoError(t, err)
		wf := filepath.Join(".github", "workflows", "a.yml")
		rec := &Record{EntryRepairs: []EntryRepair{{Key: key, Ref: "v4.2.0", Workflows: []string{wf}}}}
		var writers []WorkflowPlan
		if written {
			writers = []WorkflowPlan{{Path: filepath.Join(".github", "workflows", "b.yml")}, {Path: wf}}
		}
		preview := store.Preview()
		require.NoError(t, updateLockfile(context.Background(), rec, writers, preview))
		a, ok := preview.Entry(key)
		require.True(t, ok)
		want := sha
		if written {
			want = "v4.2.0"
		}
		assert.Equal(t, want, a.Ref, "written=%v", written)
	}
}

// TestUpdateLockfileClearsRefreshedEdges: a refreshed workflow's recorded
// edges are replaced, not merged, so a child its composite no longer uses
// leaves the graph. Every write path (Commit, Audit's and repair's
// previews) goes through updateLockfile, so they all see the same graph.
func TestUpdateLockfileClearsRefreshedEdges(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, ".github", "workflows"), 0o755))
	root, child := strings.Repeat("a", 40), strings.Repeat("b", 40)
	lock := "version: 'v0.0.3'\nworkflows:\n    '.github/workflows/a.yml':\n        - 'o/root@v1'\n" +
		"dependencies:\n    'o/root@v1':\n        ref: 'v1'\n        commit: 'sha1-" + root + "'\n        owner_id: 1\n        repo_id: 1\n        uses:\n            - 'o/child@v2'\n" +
		"    'o/child@v2':\n        ref: 'v2'\n        commit: 'sha1-" + child + "'\n        owner_id: 1\n        repo_id: 2\n"
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".github", "workflows", "actions.lock"), []byte(lock), 0o600))
	store, err := lockfile.LoadState(dir, fakeMeta{})
	require.NoError(t, err)
	wf := filepath.Join(".github", "workflows", "a.yml")
	rec := &Record{
		Entries:   []Entry{{NWO: "o/root", Ref: "v1", SHA: strings.Repeat("c", 40), Resolution: Pinned, Direct: true, Workflows: []string{wf}}},
		Workflows: []WorkflowPlan{{Path: wf, Refresh: true}},
	}

	preview := store.Preview()
	require.NoError(t, updateLockfile(context.Background(), rec, rec.Workflows, preview))
	preview.GC()
	a, ok := preview.Entry("o/root@v1")
	require.True(t, ok)
	assert.Empty(t, a.Uses, "the dropped edge is gone")
	_, ok = preview.Entry("o/child@v2")
	assert.False(t, ok, "the dropped child is gone")
}

// TestCommitUsesCycleRewritesNothing: a graph the lockfile can't record must
// fail before any workflow rewrite, not after.
func TestCommitUsesCycleRewritesNothing(t *testing.T) {
	dir := t.TempDir()
	workflowPath := filepath.Join(".github", "workflows", "ci.yml")
	oldUses := "acme/a@" + strings.Repeat("a", 40)
	workflowContent := []byte("on: push\njobs:\n  test:\n    runs-on: ubuntu-latest\n    steps:\n      - uses: " + oldUses + "\n")
	require.NoError(t, os.MkdirAll(filepath.Dir(filepath.Join(dir, workflowPath)), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, workflowPath), workflowContent, 0o644))
	t.Chdir(dir)

	store, err := lockfile.LoadState(dir, fakeMeta{})
	require.NoError(t, err)
	rec := &Record{
		Entries: []Entry{
			{NWO: "acme/a", Ref: "v1", SHA: strings.Repeat("a", 40), Resolution: Pinned, Direct: true, OnBranch: "main", Workflows: []string{workflowPath}, RequiredBy: []string{"acme/b@v1"}},
			{NWO: "acme/b", Ref: "v1", SHA: strings.Repeat("b", 40), Resolution: Pinned, OnBranch: "main", Workflows: []string{workflowPath}, RequiredBy: []string{"acme/a@v1"}},
		},
		Workflows: []WorkflowPlan{{Path: workflowPath, Rewrites: map[string]string{oldUses: "acme/a@v1"}}},
	}

	err = Commit(context.Background(), rec, store, nil)
	require.ErrorIs(t, err, lockfile.ErrUsesCycle)
	after, err := os.ReadFile(workflowPath)
	require.NoError(t, err)
	assert.Equal(t, workflowContent, after)
	assert.NoFileExists(t, filepath.Join(dir, ".github", "workflows", "actions.lock"))
}
