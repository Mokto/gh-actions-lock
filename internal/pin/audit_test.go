package pin

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/github/gh-actions-lock/internal/lockfile"
	"github.com/github/gh-actions-lock/internal/pipeline/checks"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, written(preview, verified, tt.d))
		})
	}
	assert.False(t, written(preview, nil, checks.Deferred{Key: "o/a@v1", Ref: "v1", Commit: a}), "a commit this run didn't verify proves nothing")
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
	writers := []WorkflowPlan{{Rewrites: map[string]string{"o/child@v4": "o/child@v4.2.1"}}}

	got := carryVerified(preview, verified, writers)
	assert.True(t, got[checks.VerifiedKey("o/child@v4.2.1", b)], "a narrowed use carries the proof for the commit it's written at")
	assert.False(t, got[checks.VerifiedKey("o/child@v4.2.1", a)], "another commit verified under the old key carries nothing")
	assert.True(t, got[checks.VerifiedKey("o/child@v4", a)])
	assert.False(t, got[checks.VerifiedKey("o/child@v4.2.0", b)], "a sibling ref at the same commit gains nothing")
	assert.False(t, verified[checks.VerifiedKey("o/child@v4.2.1", b)], "the report's own evidence is left alone")
}

// TestAuditRechecksWithoutBlockedWriters: a.yml's deferred finding holds only
// because b.yml's write moves the shared entry to a verified commit. b.yml's
// own deferred finding holds on nothing, which blocks b.yml, so a recheck
// without b.yml's write must restore a.yml's finding too.
func TestAuditRechecksWithoutBlockedWriters(t *testing.T) {
	a, b := strings.Repeat("a", 40), strings.Repeat("b", 40)
	wfA, wfB := ".github/workflows/a.yml", ".github/workflows/b.yml"
	path := filepath.Join(t.TempDir(), "actions.lock")
	body := "version: v0.0.3\nworkflows:\n  " + wfA + ":\n    - o/shared@v1\n  " + wfB + ":\n    - o/shared@v1\n    - o/other@v1\ndependencies:\n" +
		"  o/shared@v1:\n    ref: v1\n    commit: sha1-" + a + "\n    owner_id: 1\n    repo_id: 2\n" +
		"  o/other@v1:\n    ref: v1\n    commit: sha1-" + a + "\n    owner_id: 1\n    repo_id: 3\n"
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
	store, err := lockfile.LoadStateAt(path, nil)
	require.NoError(t, err)

	deferred := func(wf, key string) checks.WorkflowReport {
		f := checks.Finding{WorkflowPath: wf, Category: checks.UnreachablePin, Severity: checks.SeverityError, Confidence: checks.ConfidenceHigh, Detail: key}
		return checks.WorkflowReport{Path: wf, Deferred: []checks.Deferred{{Key: key, Ref: "v1", Commit: a, Finding: f}}}
	}
	report := &checks.Report{
		Workflows: []checks.WorkflowReport{deferred(wfA, "o/shared@v1"), deferred(wfB, "o/other@v1")},
		Verified:  map[string]bool{checks.VerifiedKey("o/shared@v1", b): true},
	}
	entry := func(nwo, sha, wf string, res Resolution) Entry {
		return Entry{NWO: nwo, Ref: "v1", Tag: "v1", SHA: sha, Resolution: res, Workflows: []string{wf}, Direct: true, FromLock: res == Verified}
	}
	rec := &Record{
		Entries: []Entry{
			entry("o/shared", a, wfA, Verified),
			entry("o/shared", b, wfB, Pinned),
			entry("o/other", a, wfB, Verified),
		},
		Workflows: []WorkflowPlan{{Path: wfA}, {Path: wfB}},
	}

	require.NoError(t, Audit(context.Background(), rec, store, report, nil))
	for _, wr := range report.Workflows {
		assert.True(t, wr.Deferred[0].Restored, "%s keeps its finding", wr.Path)
		assert.True(t, wr.SkipCommit, "%s is not written", wr.Path)
	}
}
