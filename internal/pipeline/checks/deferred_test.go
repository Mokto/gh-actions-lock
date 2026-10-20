package checks

import (
	"testing"

	"github.com/github/gh-actions-lock/internal/dep"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRestoreDeferred(t *testing.T) {
	d := &dep.Dependency{NWO: "o/a", Ref: "v1", SHA: "aaaa"}
	blocking := Finding{Category: UnreachablePin, Severity: SeverityError, Dependency: d}
	r := Report{Workflows: []WorkflowReport{{
		Findings: []Finding{
			{Category: UnreachablePin, Severity: SeverityWarning, Dependency: d, Remediation: "accepted"},
			{Category: Valid, Severity: SeverityOK},
		},
		Deferred: []Deferred{{Key: "o/a@v1", Ref: "v1", Commit: "aaaa", Finding: blocking}},
	}}}

	r.RestoreDeferred()
	r.RestoreDeferred()
	wr := r.Workflows[0]
	require.Len(t, wr.Findings, 1, "the accepting warning and the valid marker go, and the finding comes back once")
	assert.Equal(t, SeverityError, wr.Findings[0].Severity)
	assert.True(t, wr.SkipCommit)
	assert.True(t, wr.Deferred[0].Restored)
}
