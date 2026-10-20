package pipeline

import (
	parserlock "github.com/github/actions-lockfile/go/pkg/lockfile"
	"github.com/github/gh-actions-lock/internal/pipeline/checks"
)

// parityMode is the run's flags as they bear on parity findings.
type parityMode struct {
	verify bool
	// relock re-resolves every ref.
	relock bool
	// acceptMoved implies relock.
	acceptMoved bool
}

// paritySubject is the lock entry a parity finding is about.
type paritySubject struct {
	pin parserlock.Pin
	// fresh marks a pin resolved in this run: it is what the run would write.
	fresh bool
	// pruned marks a recorded entry that the workflow's findings already
	// make planning drop and re-resolve (checks.PrunesDependency), so what
	// the run writes for it is not the recorded commit.
	pruned bool
	// heldBy is the composite that keeps the entry as recorded even when
	// --accept-moved re-resolves the workflow's use (see heldBy in
	// parity.go); "" when that repairs it.
	heldBy string
	// head is the ref the commit was traced to; "" for an entry without one.
	head string
	// inconclusive marks a check that couldn't finish.
	inconclusive bool
}

// disposeParity decides whether a parity finding reaches the report, and
// sets its severity and remediation for the run's mode. It is the one place
// that weighs a finding against the mode, so every mode agrees on what a
// pin's state means.
//
// Planning writes every recorded entry back verbatim unless a finding
// prunes it, so --relock excuses a recorded entry's findings only when the
// entry is pruned. Even then only what re-resolving can't fix matters:
// repository identity, a full-SHA key's commit, which no relock moves, and
// a ref that is not a tag or branch, which resolution would still accept by
// rev-parse rules. Any other entry is judged as on a plain run: that
// includes a composite's child, which planning keeps from the recorded
// graph, and a pin whose move couldn't be classified.
//
// --accept-moved also accepts a direct full-SHA key whose recorded ref no
// longer vouches for it: the finding becomes a warning that itself prunes
// the entry, and planning records a tag or branch that contains the commit,
// or fails. An entry a composite holds (see heldBy) gets no override, and
// its advice names the composite.
func disposeParity(m parityMode, f *checks.Finding, s paritySubject) bool {
	if s.fresh {
		if s.inconclusive {
			f.Severity = checks.SeverityError
			f.Remediation = "retry; nothing is written until the repository identity is confirmed"
		}
		return true
	}
	fullSHA := parserlock.IsFullSha(s.pin.Ref)
	c := f.Category
	if c == checks.UnreachablePin && !f.RefMissing && f.Dependency != nil {
		f.Remediation = unreachableRemediation(s.heldBy, f.Dependency.NWO)
	}
	if m.acceptMoved && c == checks.UnreachablePin && fullSHA && !f.RefMissing && s.heldBy == "" {
		f.Severity = checks.SeverityWarning
		f.Remediation = "accepted: --accept-moved records a tag or branch that contains this commit"
		return true
	}
	excused := m.relock && s.pruned
	// A history check fails closed: the commit is otherwise unproven.
	if s.inconclusive && s.head != "" && (!excused || fullSHA) {
		f.Severity = checks.SeverityError
		f.Remediation = "retry; a pin is accepted only once its commit is traced to its tag or branch"
		return true
	}
	if !excused {
		return true
	}
	return c == checks.RepoRenamed || c == checks.RepoReplaced || c == checks.RepoUnavailable ||
		c == checks.UnreachablePin && (fullSHA || f.RefMissing)
}
