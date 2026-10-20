package pin

import (
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

func TestReinstate(t *testing.T) {
	d := &dep.Dependency{NWO: "o/a", Ref: "v1", SHA: "aaaa"}
	wr := checks.WorkflowReport{Findings: []checks.Finding{
		{Category: checks.UnreachablePin, Severity: checks.SeverityWarning, Dependency: d, Remediation: "accepted"},
		{Category: checks.Valid, Severity: checks.SeverityOK},
	}}
	reinstate(&wr, checks.Finding{Category: checks.UnreachablePin, Severity: checks.SeverityError, Dependency: d})
	require.Len(t, wr.Findings, 1, "the accepting warning and the valid marker go")
	assert.Equal(t, checks.SeverityError, wr.Findings[0].Severity)
	assert.True(t, wr.SkipCommit)
}
