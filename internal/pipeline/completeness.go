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
	for i, pw := range parsed {
		if isBlocked(pw) || report.Workflows[i].SkipCommit || !report.Workflows[i].IsValid() {
			continue
		}
		recorded, _ := pw.PartitionRefs()
		// A full-SHA root is checked under its own entry when one exists;
		// otherwise under the entry whose commit it pins. Either way it
		// takes that entry's spelling, so the walk finds the entry it names.
		for j, ref := range recorded {
			if !parserlock.IsFullSha(ref.Ref) {
				continue
			}
			if i := slices.IndexFunc(pw.ExistingDeps, func(d dep.Dependency) bool {
				return dep.RefKey(d.NWO, d.Ref) == dep.RefKey(ref.NWO(), ref.Ref)
			}); i >= 0 {
				recorded[j].Ref = pw.ExistingDeps[i].Ref
				continue
			}
			for _, d := range pw.ExistingDeps {
				if strings.EqualFold(d.NWO, ref.NWO()) && strings.EqualFold(d.SHA, ref.Ref) {
					recorded[j].Ref = d.Ref
					break
				}
			}
		}
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
		for _, child := range slices.Sorted(maps.Keys(parents)) {
			pp := parents[child]
			for _, parent := range pp {
				action, ok := file.Dependencies[dep.FoldKey(parent)]
				if !ok {
					continue
				}
				found := false
				for _, use := range action.Uses {
					if dep.FoldKey(use) == dep.FoldKey(child) {
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
