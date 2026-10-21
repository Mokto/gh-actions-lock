package checks

import (
	"strings"

	"github.com/github/gh-actions-lock/internal/dep"
)

// Deferred is a finding on a recorded lock entry that parity excused or
// accepted because the run should replace or remove the entry. It holds
// only if the write the run plans does that; otherwise Finding, as it would
// block with the entry left as recorded, stands.
type Deferred struct {
	// Key is the entry's on-disk key (canonical NWO@ref).
	Key string
	// Ref and Commit are what the entry records.
	Ref    string
	Commit string
	// Finding is the finding as it blocks when the entry stays as recorded.
	Finding Finding
	// Restored marks a finding put back on its workflow.
	Restored bool
}

// Restore puts d's finding back on wr, replacing the warning that accepted
// it, once.
func (wr *WorkflowReport) Restore(i int) {
	d := &wr.Deferred[i]
	if d.Restored {
		return
	}
	d.Restored = true
	f := d.Finding
	kept := wr.Findings[:0]
	for _, g := range wr.Findings {
		if g.Category == f.Category && g.Severity != SeverityError && sameDependency(g, f) {
			continue
		}
		kept = append(kept, g)
	}
	wr.Findings = kept
	if f.Severity == SeverityError {
		wr.SkipCommit = true
		valid := wr.Findings[:0]
		for _, g := range wr.Findings {
			if g.Category != Valid {
				valid = append(valid, g)
			}
		}
		wr.Findings = valid
	}
	wr.Findings = append(wr.Findings, f)
}

// RestoreDeferred puts every deferred finding not yet restored back on its
// workflow. A run that writes nothing leaves each deferred entry as
// recorded, so its finding stands; call this on every such path before
// reporting.
func (r *Report) RestoreDeferred() {
	for i := range r.Workflows {
		for j := range r.Workflows[i].Deferred {
			r.Workflows[i].Restore(j)
		}
	}
}

func sameDependency(a, b Finding) bool {
	if a.Dependency == nil || b.Dependency == nil {
		return false
	}
	return a.Dependency.ID() == b.Dependency.ID() && dep.SameRef(a.Dependency.SHA, b.Dependency.SHA)
}

// VerifiedKey identifies a commit this run checked for an on-disk key.
func VerifiedKey(key, sha string) string {
	return key + ":" + strings.ToLower(sha)
}
