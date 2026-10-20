package pipeline

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	parserlock "github.com/github/actions-lockfile/go/pkg/lockfile"
	"github.com/github/gh-actions-lock/internal/dep"
	"github.com/github/gh-actions-lock/internal/ghapi"
	"github.com/github/gh-actions-lock/internal/lockfile"
	"github.com/github/gh-actions-lock/internal/pipeline/checks"
	"github.com/github/gh-actions-lock/internal/resolve"
)

func checkLockedManifests(ctx context.Context, r *resolve.Resolver, parsed []checks.ParsedWorkflow, store *lockfile.State, report *checks.Report) error {
	file := store.File()
	deps := store.AllDeps()
	refs := lockfile.NewRefIndex(file)
	for i, pw := range parsed {
		if isBlocked(pw) || report.Workflows[i].SkipCommit || !report.Workflows[i].IsValid() {
			continue
		}
		recorded, _ := pw.PartitionRefs()
		recorded = lockedRoots(recorded, pw.ExistingDeps)
		_, parents, err := r.ResolveLockedRecursive(ctx, recorded, deps)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		// Behind the proxy a transient read is a retry, matching resolution
		// and parity, not a finding.
		if err != nil && ghapi.DependabotProxy() && ghapi.IsTransient(err) {
			return fmt.Errorf("could not verify locked dependency manifests: %w", err)
		}
		wr := &report.Workflows[i]
		// The recorded edges must include every edge discovered at the locked
		// commits, even when a child exists elsewhere in the same lockfile.
		// Discovery names entries as manifests spell them; match them to the
		// recorded keys with dep.RefKey's rule, so a full SHA in another case
		// neither fails a complete graph nor skips its entry's descendants.
		// A spelling that reaches several entries and none exactly checks
		// each of them: their edges can differ.
		for _, child := range slices.Sorted(maps.Keys(parents)) {
			for _, parent := range lockedParents(refs, parents[child]) {
				action := file.Dependencies[parent]
				found := false
				for _, use := range action.Uses {
					if dep.RefKeyOf(use) == dep.RefKeyOf(child) {
						found = true
						break
					}
				}
				if found {
					continue
				}
				ref := parserlock.ParseActionRef(child)
				f := checks.Finding{
					WorkflowPath:       pw.Path,
					Category:           checks.NotPinned,
					Severity:           checks.SeverityError,
					Confidence:         checks.ConfidenceHigh,
					ParentNWO:          parent,
					IncompleteLockfile: true,
					TrackedAction:      true,
					Detail:             fmt.Sprintf("locked manifest for %s uses %s but the lockfile omits that edge", parent, child),
					Remediation:        "restore the graph from version control, or run `gh actions-lock --relock`",
				}
				if ref != nil {
					f.Dependency = &dep.Dependency{NWO: ref.NWO(), Ref: ref.Ref}
				}
				wr.Findings = append(dropValid(wr.Findings), f)
				wr.SkipCommit = true
			}
		}
		if cycle := (*resolve.UsesCycleError)(nil); errors.As(err, &cycle) {
			wr.Findings = append(dropValid(wr.Findings), usesCycleFinding(pw.Path, cycle))
			wr.SkipCommit = true
		}
		if err != nil && !(wr.SkipCommit && onlyExplained(err)) {
			wr.Findings = append(dropValid(wr.Findings), checks.Finding{
				WorkflowPath: pw.Path,
				Category:     checks.ReachabilityUnknown,
				Severity:     checks.SeverityError,
				Confidence:   checks.ConfidenceLow,
				Detail:       fmt.Sprintf("could not verify locked dependency manifests: %s", err),
				Remediation:  "retry; complete dependency coverage could not be established",
			})
			wr.SkipCommit = true
		}
		// Direct missing entries are diagnosed structurally. Unlocked children
		// are named by the missing-edge findings above.
	}
	return nil
}

// lockedRoots names each full-SHA root by the entries it reaches: its own
// spelling when recorded, else every entry with its identity, so each
// candidate's graph is walked. A full SHA with no such entry takes the first
// entry whose commit it pins.
func lockedRoots(roots []parserlock.ActionRef, existing []dep.Dependency) []parserlock.ActionRef {
	var out []parserlock.ActionRef
	for _, ref := range roots {
		if !parserlock.IsFullSha(ref.Ref) {
			out = append(out, ref)
			continue
		}
		var spellings []string
		for _, d := range existing {
			if dep.RefKey(d.NWO, d.Ref) != dep.RefKey(ref.NWO(), ref.Ref) {
				continue
			}
			if d.Ref == ref.Ref {
				spellings = []string{d.Ref}
				break
			}
			if !slices.Contains(spellings, d.Ref) {
				spellings = append(spellings, d.Ref)
			}
		}
		if len(spellings) == 0 {
			for _, d := range existing {
				if strings.EqualFold(d.NWO, ref.NWO()) && strings.EqualFold(d.SHA, ref.Ref) {
					spellings = []string{d.Ref}
					break
				}
			}
		}
		if len(spellings) == 0 {
			out = append(out, ref)
		}
		slices.Sort(spellings)
		for _, s := range spellings {
			named := ref
			named.Ref = s
			out = append(out, named)
		}
	}
	return out
}

// lockedParents maps discovered parents to the recorded entries whose edges
// must cover the child: each parent's exact entry, else every entry with its
// identity.
func lockedParents(refs lockfile.RefIndex, parents []string) []string {
	var out []string
	for _, parent := range parents {
		nwo, ref, _ := strings.Cut(parent, "@")
		for _, k := range refs.Lookup(nwo, ref).Entries() {
			if !slices.Contains(out, k) {
				out = append(out, k)
			}
		}
	}
	return out
}

// onlyExplained reports whether every error in err already has a finding:
// an unlocked child is named by its missing edge, a cycle by its own.
func onlyExplained(err error) bool {
	if err == nil {
		return true
	}
	switch err.(type) {
	case *resolve.UnlockedRefError, *resolve.UsesCycleError:
		return true
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		for _, inner := range joined.Unwrap() {
			if !onlyExplained(inner) {
				return false
			}
		}
		return true
	}
	if inner := errors.Unwrap(err); inner != nil {
		return onlyExplained(inner)
	}
	return false
}

// usesCycleFinding blocks a workflow whose composite actions reach
// themselves: the action can't run, and the lockfile can't record it.
func usesCycleFinding(path string, cycle *resolve.UsesCycleError) checks.Finding {
	return checks.Finding{
		WorkflowPath: path,
		Category:     checks.NotPinned,
		Severity:     checks.SeverityError,
		Confidence:   checks.ConfidenceHigh,
		Detail:       cycle.Error(),
		Remediation:  "stop using that version; an action that uses itself can't run",
	}
}
