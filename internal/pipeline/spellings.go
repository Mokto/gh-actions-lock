package pipeline

import (
	"fmt"
	"slices"
	"strings"

	parserlock "github.com/github/actions-lockfile/go/pkg/lockfile"
	"github.com/github/gh-actions-lock/internal/dep"
	"github.com/github/gh-actions-lock/internal/lockfile"
	"github.com/github/gh-actions-lock/internal/pipeline/checks"
	"github.com/github/gh-actions-lock/internal/workflowfile"
)

// refuseAmbiguousSpellings keeps a write mode from writing a workflow that
// reaches a full SHA recorded under several spellings. One commit, but each
// entry records its own ref, edges and host, and no rule picks one, even
// when the workflow spells one of them exactly. Verification checks every
// entry instead (see lockfile.InspectClosure), and --relock of every
// workflow rewrites the class under one spelling (see pin.Plan).
func refuseAmbiguousSpellings(parsed []checks.ParsedWorkflow, store *lockfile.State, report *checks.Report) {
	file := store.File()
	refs := lockfile.NewRefIndex(file)
	for i, pw := range parsed {
		if isBlocked(pw) {
			continue
		}
		reached := make([]string, 0, len(pw.Refs))
		for _, ref := range pw.Refs {
			reached = append(reached, ref.Owner+"/"+ref.Repo+"@"+ref.Ref)
		}
		closure := store.Closure(workflowfile.KeyFromPath(pw.Path), func(parserlock.Pin, parserlock.Action) bool { return true })
		for _, lp := range closure {
			reached = append(reached, lp.Key)
			reached = append(reached, lp.Action.Uses...)
		}
		reported := map[string]bool{}
		wr := &report.Workflows[i]
		for _, r := range reached {
			nwo, ref, _ := strings.Cut(r, "@")
			candidates := refs.Lookup(nwo, ref).Candidates
			if len(candidates) < 2 || reported[dep.RefKey(nwo, ref)] {
				continue
			}
			reported[dep.RefKey(nwo, ref)] = true
			wr.Findings = append(dropValid(wr.Findings), checks.Finding{
				WorkflowPath: pw.Path,
				Category:     checks.NotPinned,
				Severity:     checks.SeverityError,
				Confidence:   checks.ConfidenceHigh,
				Dependency:   &dep.Dependency{NWO: nwo, Ref: ref},
				Detail:       fmt.Sprintf("commit %s of %s is recorded under several spellings (%s) whose refs, edges or hosts can differ; nothing is written while it is ambiguous", parserlock.ShortSHA(ref), nwo, strings.Join(slices.Sorted(slices.Values(candidates)), ", ")),
				Remediation:  "spell the commit one way in every workflow and composite, and keep only that entry in actions.lock",
			})
			wr.SkipCommit = true
		}
	}
}
