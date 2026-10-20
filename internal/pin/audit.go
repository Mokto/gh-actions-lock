package pin

import (
	"context"
	"strings"

	parserlock "github.com/github/actions-lockfile/go/pkg/lockfile"
	"github.com/github/gh-actions-lock/internal/lockfile"
	"github.com/github/gh-actions-lock/internal/pipeline/checks"
)

// Audit holds each finding parity deferred (checks.Deferred) to the write
// rec plans. It applies the write of every workflow still due to be
// written to a preview of store, with the code Commit and Save use, and a
// deferred finding holds only if its entry then is gone, or records a
// commit this run verified, or, for a full-SHA key, a new ref for the same
// commit. Otherwise the finding goes back on its workflow, which is then no
// longer written, so Audit runs again without that workflow's write: a
// finding that held only because of it goes back too. It stops once a pass
// blocks no further workflow.
func Audit(ctx context.Context, rec *Record, store *lockfile.State, report *checks.Report, copts *CommitOptions) error {
	deferred := false
	for _, wr := range report.Workflows {
		deferred = deferred || len(wr.Deferred) > 0
	}
	if !deferred {
		return nil
	}
	byPath := make(map[string]*checks.WorkflowReport, len(report.Workflows))
	for i := range report.Workflows {
		byPath[report.Workflows[i].Path] = &report.Workflows[i]
	}
	for {
		var writers []WorkflowPlan
		for _, wp := range committedWorkflows(rec, store, copts) {
			if wr, ok := byPath[wp.Path]; !ok || !wr.SkipCommit && !wr.BlockingResolverError {
				writers = append(writers, wp)
			}
		}
		preview := store.Preview()
		if err := updateLockfile(ctx, rec, writers, preview); err != nil {
			return err
		}
		preview.GC()
		verified := carryVerified(report.Verified, writers)
		blocked := false
		for i := range report.Workflows {
			wr := &report.Workflows[i]
			for j, d := range wr.Deferred {
				if d.Restored || written(preview, verified, d) {
					continue
				}
				adviseHolder(preview, &wr.Deferred[j])
				was := wr.SkipCommit
				wr.Restore(j)
				blocked = blocked || wr.SkipCommit && !was
			}
		}
		if !blocked {
			return nil
		}
	}
}

// carryVerified extends verified through the writers' planned `uses:`
// rewrites. Narrowing, reverse lookup and re-recording each rewrite a use
// only to a ref they checked names the same commit, so a commit verified
// under the old key is verified under the new one. A sibling ref merely at
// the same commit gains nothing.
func carryVerified(verified map[string]bool, writers []WorkflowPlan) map[string]bool {
	out := make(map[string]bool, len(verified))
	for k := range verified {
		out[k] = true
	}
	for _, wp := range writers {
		for oldUses, newUses := range wp.Rewrites {
			oldKey, ok := usesKey(oldUses)
			newKey, ok2 := usesKey(newUses)
			if !ok || !ok2 || oldKey == newKey {
				continue
			}
			for k := range verified {
				if sha, ok := strings.CutPrefix(k, oldKey+":"); ok {
					out[checks.VerifiedKey(newKey, sha)] = true
				}
			}
		}
	}
	return out
}

// usesKey is the on-disk key of a `uses:` value.
func usesKey(uses string) (string, bool) {
	ref := parserlock.ParseActionRef(uses)
	if ref == nil {
		return "", false
	}
	p, ok := parserlock.ParsePin(ref.Owner + "/" + ref.Repo + "@" + ref.Ref)
	if !ok {
		return "", false
	}
	return lockfile.EntryKey(p), true
}

// adviseHolder points an unreachable pin's advice at the entry that kept
// it: the write the override planned left it to that entry's recorded
// edge, so rerunning the override can't help.
func adviseHolder(preview *lockfile.State, d *checks.Deferred) {
	f := &d.Finding
	if f.Category != checks.UnreachablePin || f.RefMissing || f.Dependency == nil {
		return
	}
	holders := preview.UsedBy(d.Key)
	if len(holders) == 0 {
		return
	}
	if p, ok := parserlock.ParsePin(holders[0]); ok {
		f.Remediation = checks.UnreachableRemediation(p.NWO+"@"+p.Ref, f.Dependency.NWO)
	}
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
