package pin

import (
	"context"
	"fmt"
	"strings"

	parserlock "github.com/github/actions-lockfile/go/pkg/lockfile"
	"github.com/github/gh-actions-lock/internal/lockfile"
	"github.com/github/gh-actions-lock/internal/pipeline/checks"
)

// Audit holds each finding parity deferred (checks.Deferred) to the write
// rec plans. It applies that write to a preview of store with the code
// Commit and Save use, and a deferred finding holds only if its entry then
// is gone, or records a commit this run verified, or, for a full-SHA key,
// a new ref for the same commit. Otherwise the finding goes back on its
// workflow, blocking it. Commit writes all or nothing, so one such finding
// keeps the whole write off disk.
func Audit(ctx context.Context, rec *Record, store *lockfile.State, report *checks.Report, copts *CommitOptions) error {
	deferred := false
	for _, wr := range report.Workflows {
		deferred = deferred || len(wr.Deferred) > 0
	}
	if !deferred {
		return nil
	}
	preview := store.Preview()
	if err := updateLockfile(ctx, rec, committedWorkflows(rec, store, copts), preview); err != nil {
		return err
	}
	preview.GC()
	for i := range report.Workflows {
		wr := &report.Workflows[i]
		for _, d := range wr.Deferred {
			if written(preview, report.Verified, d) {
				continue
			}
			reinstate(wr, d.Finding)
		}
	}
	return nil
}

// written reports whether the previewed write replaces or removes d's entry.
func written(preview *lockfile.State, verified map[string]bool, d checks.Deferred) bool {
	a, ok := preview.Entry(d.Key)
	if !ok {
		return true
	}
	_, sha, _ := strings.Cut(a.Commit, "-")
	if !strings.EqualFold(sha, d.Commit) {
		return verified[checks.VerifiedKey(d.Key, sha)]
	}
	// Planning re-records a full-SHA key's ref only once that ref names the
	// commit (RefNamesCommit), so a new ref is a repair.
	pin, ok := parserlock.ParsePin(d.Key)
	return ok && parserlock.IsFullSha(pin.Ref) && a.Ref != d.Ref
}

// reinstate puts f back on wr, replacing the warning that accepted it.
func reinstate(wr *checks.WorkflowReport, f checks.Finding) {
	kept := wr.Findings[:0]
	for _, g := range wr.Findings {
		if g.Category == f.Category && g.Severity != checks.SeverityError && sameDependency(g, f) {
			continue
		}
		kept = append(kept, g)
	}
	wr.Findings = kept
	if f.Severity == checks.SeverityError {
		wr.SkipCommit = true
		valid := wr.Findings[:0]
		for _, g := range wr.Findings {
			if g.Category != checks.Valid {
				valid = append(valid, g)
			}
		}
		wr.Findings = valid
	}
	wr.Findings = append(wr.Findings, f)
}

func sameDependency(a, b checks.Finding) bool {
	if a.Dependency == nil || b.Dependency == nil {
		return false
	}
	return fmt.Sprint(a.Dependency.Key(), a.Dependency.SHA) == fmt.Sprint(b.Dependency.Key(), b.Dependency.SHA)
}
