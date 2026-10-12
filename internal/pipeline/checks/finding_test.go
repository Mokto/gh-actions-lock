package checks

import (
	"testing"

	parserlock "github.com/github/actions-lockfile/go/pkg/lockfile"
	"github.com/stretchr/testify/assert"
)

func TestFindingIsValidFollowsSeverity(t *testing.T) {
	categories := []Category{Stale, NotPinned, RefChanged, UnreachablePin, ShaAsRef, RefMoved, ReachabilityUnknown, StaleWorkflow, Valid}
	for _, sev := range []Severity{SeverityOK, SeverityInfo, SeverityWarning, SeverityError} {
		for _, cat := range categories {
			t.Run(string(cat)+"/"+string(sev), func(t *testing.T) {
				f := Finding{Category: cat, Severity: sev, ActionRef: &parserlock.ActionRef{Owner: "o", Repo: "r", Ref: "v1"}}
				want := sev != SeverityError
				assert.Equal(t, want, f.IsValid())
				r := Report{Workflows: []WorkflowReport{{Findings: []Finding{f}}}}
				assert.Equal(t, want, r.IsValid())
			})
		}
	}
}
