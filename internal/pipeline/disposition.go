package pipeline

import (
	"fmt"

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
	// away marks a recorded entry planning provably replaces or removes
	// (see plannedAway in parity.go), so what the run writes for it is not
	// the recorded commit.
	away bool
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
// prunes the workflow's use of it, and even then a composite's recorded
// edge can keep it, so --relock excuses a recorded entry's findings only
// when planning provably replaces or removes it. Even then only what re-resolving can't fix matters:
// repository identity, a full-SHA key's commit, which no relock moves, and
// a ref that is not a tag or branch, which resolution would still accept by
// rev-parse rules. Any other entry is judged as on a plain run: that
// includes a composite's child, which planning keeps from the recorded
// graph, and a pin whose move couldn't be classified.
//
// --accept-moved also accepts a pin whose recorded ref no longer vouches
// for it, when re-resolving the workflow's use repairs it: the finding
// becomes a warning that itself prunes the entry, and planning locks a
// mutable ref at its current commit, or records a tag or branch that
// contains a full-SHA key's commit, or fails. An entry a composite holds (see heldBy) gets no override, and
// its advice names the composite.
// parityVerdict is what disposeParity decides for a finding.
type parityVerdict int

const (
	// verdictKeep reports the finding as it stands.
	verdictKeep parityVerdict = iota
	// verdictExcuse drops the finding: the run should replace or remove its
	// recorded entry.
	verdictExcuse
	// verdictAccept reports the finding as an accepted warning: the run
	// should repair its recorded entry.
	verdictAccept
)

// disposeParity returns the verdict for f, and the finding as it would block
// were its entry left as recorded; for verdictKeep that is f itself.
func disposeParity(m parityMode, f *checks.Finding, s paritySubject) (parityVerdict, checks.Finding) {
	if s.fresh {
		if s.inconclusive {
			f.Severity = checks.SeverityError
			f.Remediation = "retry; nothing is written until the repository identity is confirmed"
		}
		return verdictKeep, *f
	}
	fullSHA := parserlock.IsFullSha(s.pin.Ref)
	c := f.Category
	if c == checks.UnreachablePin && !f.RefMissing && f.Dependency != nil {
		f.Remediation = unreachableRemediation(s.heldBy, f.Dependency.NWO)
	}
	// pin.Audit holds an accepted or excused finding to the write the run
	// plans, so a mutable entry is deferred to it whoever holds it. A
	// composite's full-SHA entry stays blocking: its repair re-keys the
	// workflow's use and leaves the composite's entry as recorded.
	if m.acceptMoved && c == checks.UnreachablePin && !f.RefMissing && (!fullSHA || s.heldBy == "") {
		blocking := *f
		f.Severity = checks.SeverityWarning
		f.Remediation = "accepted: --accept-moved records a tag or branch that contains this commit"
		if !fullSHA {
			f.Remediation = fmt.Sprintf("accepted: --accept-moved locks %s at its current commit", s.pin.Ref)
		}
		return verdictAccept, blocking
	}
	excused := m.relock && (s.away || !fullSHA)
	// A history check fails closed: the commit is otherwise unproven.
	if s.inconclusive && s.head != "" {
		f.Severity = checks.SeverityError
		f.Remediation = "retry; a pin is accepted only once its commit is traced to its tag or branch"
		if !excused || fullSHA {
			return verdictKeep, *f
		}
	}
	if excused && !(c == checks.RepoRenamed || c == checks.RepoReplaced || c == checks.RepoUnavailable ||
		c == checks.UnreachablePin && (fullSHA || f.RefMissing)) {
		return verdictExcuse, *f
	}
	return verdictKeep, *f
}
