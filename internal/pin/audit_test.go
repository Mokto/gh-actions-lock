package pin

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/github/gh-actions-lock/internal/dep"
	"github.com/github/gh-actions-lock/internal/lockfile"
	"github.com/github/gh-actions-lock/internal/pipeline/checks"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAuditInspectsWritePreview(t *testing.T) {
	errInspect := errors.New("manifest inspection failed")
	for _, tt := range []struct {
		name       string
		block      bool
		inspectErr error
		calls      int
	}{
		{name: "inspects without parity deferrals", calls: 1},
		{name: "rebuilds preview after inspection blocks a writer", block: true, calls: 2},
		{name: "returns inspection errors", inspectErr: errInspect, calls: 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			sha := strings.Repeat("a", 40)
			path := filepath.Join(t.TempDir(), "actions.lock")
			body := "version: v0.0.3\nworkflows:\n  ci.yml:\n    - o/a@v1\ndependencies:\n" +
				"  o/a@v1:\n    ref: v1\n    commit: sha1-" + sha + "\n    owner_id: 1\n    repo_id: 2\n"
			require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
			store, err := lockfile.LoadStateAt(path, nil)
			require.NoError(t, err)
			rec := &Record{Workflows: []WorkflowPlan{{Path: "ci.yml"}}}
			report := &checks.Report{Workflows: []checks.WorkflowReport{{Path: "ci.yml"}}}
			calls := 0
			err = Audit(context.Background(), rec, store, report, nil, func(ctx context.Context, replaced checks.ReplacementProof) error {
				require.NoError(t, ctx.Err())
				calls++
				assert.Equal(t, calls == 1, replaced(checks.Deferred{
					Key: "o/a@v1", Ref: "v1", Commit: sha,
					Finding: checks.Finding{IncompleteLockfile: true},
				}), "a blocked writer cannot prove removal on the next preview")
				if tt.block {
					report.Workflows[0].SkipCommit = true
				}
				return tt.inspectErr
			})
			require.ErrorIs(t, err, tt.inspectErr)
			assert.Equal(t, tt.calls, calls)
			_, present := store.Entry("o/a@v1")
			assert.True(t, present, "the preview must not mutate the active store")
			got, err := os.ReadFile(path)
			require.NoError(t, err)
			assert.Equal(t, body, string(got), "inspection must not write files")
		})
	}
}

func TestWritten(t *testing.T) {
	a, b := strings.Repeat("a", 40), strings.Repeat("b", 40)
	path := filepath.Join(t.TempDir(), "actions.lock")
	body := "version: v0.0.3\nworkflows:\n  ci.yml:\n    - o/a@v1\n    - o/sha@" + a + "\ndependencies:\n" +
		"  o/a@v1:\n    ref: v1\n    commit: sha1-" + b + "\n    owner_id: 1\n    repo_id: 2\n" +
		"  o/sha@" + a + ":\n    ref: main\n    commit: sha1-" + a + "\n    owner_id: 1\n    repo_id: 3\n"
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
	store, err := lockfile.LoadStateAt(path, nil)
	require.NoError(t, err)
	preview := store.Preview()
	verified := map[string]bool{checks.VerifiedKey("o/a@v1", b): true}

	tests := []struct {
		name string
		d    checks.Deferred
		want bool
	}{
		{"an entry rewritten at a verified commit", checks.Deferred{Key: "o/a@v1", Ref: "v1", Commit: a}, true},
		{"an entry still at its recorded commit", checks.Deferred{Key: "o/a@v1", Ref: "v1", Commit: b}, false},
		{"an entry gone from the lock", checks.Deferred{Key: "o/gone@v1", Ref: "v1", Commit: a}, true},
		{"a full-SHA key re-recorded under a new ref", checks.Deferred{Key: "o/sha@" + a, Ref: "v1-gone", Commit: a}, true},
		{"a full-SHA key still under its recorded ref", checks.Deferred{Key: "o/sha@" + a, Ref: "main", Commit: a}, false},
		{"a new recorded ref does not rebuild manifest edges", checks.Deferred{
			Key: "o/sha@" + a, Ref: "v1-gone", Commit: a,
			Finding: checks.Finding{IncompleteLockfile: true},
		}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, written(preview, verified, tt.d))
		})
	}
	assert.False(t, written(preview, nil, checks.Deferred{Key: "o/a@v1", Ref: "v1", Commit: a}), "a commit this run didn't verify proves nothing")
	manifest := checks.Deferred{
		Key: "o/a@v1", Ref: "v1", Commit: b,
		Finding: checks.Finding{IncompleteLockfile: true},
	}
	assert.False(t, written(preview, verified, manifest), "a verified commit does not prove its graph was rebuilt")
	require.NoError(t, preview.PrepareRefresh([]string{"ci.yml"}, []dep.Dependency{{NWO: "o/a", Ref: "v1", SHA: b}}, nil))
	assert.True(t, written(preview, verified, manifest), "actual same-commit graph rebuilding is a replacement")
	assert.False(t, written(preview, nil, manifest), "rebuilding still requires a verified commit")
}

func TestAuditInspectionFollowsEntryRepairScope(t *testing.T) {
	for _, skipNew := range []bool{false, true} {
		t.Run(fmt.Sprintf("no-onboard=%v", skipNew), func(t *testing.T) {
			sha := strings.Repeat("a", 40)
			key := "o/a@" + sha
			path := filepath.Join(t.TempDir(), "actions.lock")
			body := "version: v0.0.3\nworkflows:\n  ci.yml:\n    - " + key + "\ndependencies:\n" +
				"  " + key + ":\n    ref: old\n    commit: sha1-" + sha + "\n    owner_id: 1\n    repo_id: 2\n"
			require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
			store, err := lockfile.LoadStateAt(path, nil)
			require.NoError(t, err)
			rec := &Record{
				Workflows: []WorkflowPlan{{Path: "new.yml"}},
				EntryRepairs: []EntryRepair{{
					Key: key, Ref: "main", Workflows: []string{"new.yml"},
				}},
			}
			report := &checks.Report{Workflows: []checks.WorkflowReport{{Path: "new.yml"}}}
			var inspected bool
			err = Audit(context.Background(), rec, store, report, &CommitOptions{SkipNewWorkflowEntries: skipNew},
				func(_ context.Context, replaced checks.ReplacementProof) error {
					inspected = true
					entry := checks.Deferred{Key: key, Ref: "old", Commit: sha}
					assert.Equal(t, !skipNew, replaced(entry), "only a surviving writer applies the entry repair")
					entry.Finding.IncompleteLockfile = true
					assert.False(t, replaced(entry), "neither an omitted writer nor a ref-field repair replaces the manifest graph")
					return nil
				})
			require.NoError(t, err)
			assert.True(t, inspected, "no-onboard does not skip retained inspection")
			action, ok := store.Entry(key)
			require.True(t, ok)
			assert.Equal(t, "old", action.Ref, "only the preview applies entry repairs")
		})
	}
}

func TestAuditRestoresParityBeforeManifestInspection(t *testing.T) {
	sha := strings.Repeat("a", 40)
	path := filepath.Join(t.TempDir(), "actions.lock")
	body := "version: v0.0.3\nworkflows:\n  ci.yml:\n    - o/a@v1\ndependencies:\n" +
		"  o/a@v1:\n    ref: v1\n    commit: sha1-" + sha + "\n    owner_id: 1\n    repo_id: 2\n"
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
	store, err := lockfile.LoadStateAt(path, nil)
	require.NoError(t, err)
	rec := &Record{
		Workflows: []WorkflowPlan{{Path: "ci.yml"}},
		Entries: []Entry{{
			NWO: "o/a", Ref: "v1", SHA: sha, Resolution: Verified,
			Direct: true, Workflows: []string{"ci.yml"},
		}},
	}
	report := &checks.Report{Workflows: []checks.WorkflowReport{{
		Path: "ci.yml",
		Deferred: []checks.Deferred{{
			Key: "o/a@v1", Ref: "v1", Commit: sha,
			Finding: checks.Finding{Severity: checks.SeverityError, Category: checks.RefMoved},
		}},
	}}}
	var calls int
	err = Audit(context.Background(), rec, store, report, nil,
		func(_ context.Context, replaced checks.ReplacementProof) error {
			calls++
			assert.True(t, report.Workflows[0].Deferred[0].Restored)
			assert.True(t, report.Workflows[0].SkipCommit)
			assert.False(t, replaced(checks.Deferred{
				Key: "o/a@v1", Ref: "v1", Commit: sha,
				Finding: checks.Finding{IncompleteLockfile: true},
			}), "restoring parity removes that writer before manifest inspection")
			return nil
		})
	require.NoError(t, err)
	assert.Equal(t, 1, calls, "inspection runs only after the parity preview stabilizes")
}

func TestCarryVerified(t *testing.T) {
	a, b := strings.Repeat("a", 40), strings.Repeat("b", 40)
	path := filepath.Join(t.TempDir(), "actions.lock")
	body := "version: v0.0.3\nworkflows:\n  ci.yml:\n    - o/child@v4.2.1\ndependencies:\n" +
		"  o/child@v4.2.1:\n    ref: v4.2.1\n    commit: sha1-" + b + "\n    owner_id: 1\n    repo_id: 2\n"
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
	store, err := lockfile.LoadStateAt(path, nil)
	require.NoError(t, err)
	preview := store.Preview()
	verified := map[string]bool{
		checks.VerifiedKey("o/child@v4", a): true,
		checks.VerifiedKey("o/child@v4", b): true,
	}
	writers := []WorkflowPlan{{Rewrites: map[dep.Spelling]string{"o/child@v4": "o/child@v4.2.1"}}}

	got := carryVerified(preview, verified, writers)
	assert.True(t, got[checks.VerifiedKey("o/child@v4.2.1", b)], "a narrowed use carries the proof for the commit it's written at")
	assert.False(t, got[checks.VerifiedKey("o/child@v4.2.1", a)], "another commit verified under the old key carries nothing")
	assert.True(t, got[checks.VerifiedKey("o/child@v4", a)])
	assert.False(t, got[checks.VerifiedKey("o/child@v4.2.0", b)], "a sibling ref at the same commit gains nothing")
	assert.False(t, verified[checks.VerifiedKey("o/child@v4.2.1", b)], "the report's own evidence is left alone")
}
