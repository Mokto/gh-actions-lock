package pipeline

import (
	"fmt"
	"maps"
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

// refuseHostConflicts blocks the run when the lockfile records a repository
// on more than one host. A repository lives on one host, so its pins can't
// be resolved or verified without guessing which, and every workflow's
// write would keep the conflicting entries: each workflow that reaches it
// gets a blocking finding, every other workflow too when none does, and no
// workflow is written.
func refuseHostConflicts(parsed []checks.ParsedWorkflow, store *lockfile.State, report *checks.Report) {
	conflicts := store.HostConflicts()
	if len(conflicts) == 0 {
		return
	}
	finding := func(path, repo string) checks.Finding {
		return checks.Finding{
			WorkflowPath: path,
			Category:     checks.NotPinned,
			Severity:     checks.SeverityError,
			Confidence:   checks.ConfidenceHigh,
			Detail:       fmt.Sprintf("%s is recorded on %s; a repository lives on one host, so its pins can't be resolved or verified", repo, strings.Join(conflicts[repo], " and ")),
			Remediation:  "keep only the entries for the host the repository lives on in actions.lock, then run `gh actions-lock`",
		}
	}
	reachedAny := false
	for i, pw := range parsed {
		var reached []string
		for _, ref := range pw.Refs {
			reached = append(reached, ref.Owner+"/"+ref.Repo)
		}
		for _, lp := range store.Closure(workflowfile.KeyFromPath(pw.Path), func(parserlock.Pin, parserlock.Action) bool { return true }) {
			reached = append(reached, lp.Pin.Owner+"/"+lp.Pin.Repo)
		}
		reported := map[string]bool{}
		wr := &report.Workflows[i]
		for _, nwo := range reached {
			repo := strings.ToLower(nwo)
			if conflicts[repo] == nil || reported[repo] {
				continue
			}
			reported[repo] = true
			reachedAny = true
			wr.Findings = append(dropValid(wr.Findings), finding(pw.Path, repo))
		}
		wr.SkipCommit = true
	}
	if reachedAny {
		return
	}
	for i, pw := range parsed {
		wr := &report.Workflows[i]
		for _, repo := range slices.Sorted(maps.Keys(conflicts)) {
			wr.Findings = append(dropValid(wr.Findings), finding(pw.Path, repo))
		}
	}
}
