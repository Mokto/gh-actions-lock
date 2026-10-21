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

func checkLockedManifests(ctx context.Context, opts RunOptions, parsed []checks.ParsedWorkflow, report *checks.Report, resolved []dep.Dependency, resolvedParents dep.ParentMap, resolvedGraph *resolve.Graph) error {
	// Only a writable refresh replaces the recorded graph instead of retaining it.
	if opts.Resolver == nil || opts.Store == nil || opts.Relock && !opts.ReadOnly {
		return nil
	}
	r := opts.Resolver
	file := opts.Store.File()
	deps := opts.Store.AllDeps()
	refs := lockfile.NewRefIndex(file)
	inspections := lockedManifestInspections(parsed, report, deps, freshClosure(resolved, resolvedParents))
	var roots []parserlock.ActionRef
	for _, inspection := range inspections {
		roots = append(roots, inspection.roots...)
	}
	graph, err := r.ResolveLockedGraph(ctx, roots, deps)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	// Behind the proxy a transient read is a retry, matching resolution
	// and parity, not a finding.
	if err != nil && ghapi.DependabotProxy() && ghapi.IsTransient(err) {
		return fmt.Errorf("could not verify locked dependency manifests: %w", err)
	}
	for _, inspection := range inspections {
		pw := parsed[inspection.workflow]
		wr := &report.Workflows[inspection.workflow]
		_, parents, err := graph.Inspect(inspection.roots)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		appendLockedManifestFindings(pw.Path, wr, file, refs, parents, err)
	}
	return nil
}

type lockedInspection struct {
	workflow int
	roots    []parserlock.ActionRef
}

func lockedManifestInspections(parsed []checks.ParsedWorkflow, report *checks.Report, deps []dep.Dependency, fresh func(checks.ParsedWorkflow, resolve.RefSet) []parityOwner) []lockedInspection {
	var inspections []lockedInspection
	for i, pw := range parsed {
		wr := &report.Workflows[i]
		// Repairable diagnostics do not exempt retained pins from inspection.
		if isBlocked(pw) || wr.SkipCommit || wr.BlockingResolverError {
			continue
		}
		// Seeds are shared across workflows, including descendants of fresh
		// roots. Inspect the reached recorded pins, not just this workflow's
		// previous lockfile ownership.
		reached := pw
		reached.Refs = slices.Clone(pw.Refs)
		for _, o := range fresh(pw, resolve.RefSet{}) {
			reached.Refs = append(reached.Refs, o.ref)
		}
		reached.ExistingDeps = deps
		recorded := lockedManifestRoots(reached)
		inspections = append(inspections, lockedInspection{workflow: i, roots: recorded})
	}
	return inspections
}

func appendLockedManifestFindings(path string, wr *checks.WorkflowReport, file parserlock.File, refs lockfile.RefIndex, parents dep.ParentMap, err error) {
	// The recorded edges must include every edge discovered at the locked
	// commits, even when a child exists elsewhere in the same lockfile.
	// Discovery names entries as manifests spell them; match them to the
	// recorded keys with dep.RefKey's rule, so a full SHA in another case
	// neither fails a complete graph nor skips its entry's descendants.
	// A spelling that reaches several entries checks each of them:
	// their edges can differ.
	for _, childID := range slices.SortedFunc(maps.Keys(parents), dep.Key.Compare) {
		child := childID.String()
		for _, parent := range lockedParents(refs, parents[childID]) {
			action := file.Dependencies[parent]
			found := false
			for _, use := range action.Uses {
				if dep.RefKeyOf(use) == childID {
					found = true
					break
				}
			}
			if found {
				continue
			}
			ref := parserlock.ParseActionRef(child)
			f := checks.Finding{
				WorkflowPath:       path,
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
		wr.Findings = append(dropValid(wr.Findings), usesCycleFinding(path, cycle))
		wr.SkipCommit = true
	}
	if err != nil && !(wr.SkipCommit && onlyExplained(err)) {
		wr.Findings = append(dropValid(wr.Findings), checks.Finding{
			WorkflowPath: path,
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

// lockedRoots names each full-SHA root by every entry with its identity,
// exact spelling or not, so each candidate's graph is walked. A full SHA with no such entry takes the first
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
			if !slices.Contains(spellings, d.Ref) {
				spellings = append(spellings, d.Ref)
			}
		}
		if len(spellings) == 0 {
			for _, d := range existing {
				if dep.RefKey(d.NWO, d.SHA) == dep.RefKey(ref.NWO(), ref.Ref) {
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
// must cover the child: every entry with the parent's identity.
func lockedParents(refs lockfile.RefIndex, parents []string) []string {
	var out []string
	for _, parent := range parents {
		nwo, ref, _ := strings.Cut(parent, "@")
		for _, k := range refs.Lookup(nwo, ref).Candidates {
			if !slices.Contains(out, k) {
				out = append(out, k)
			}
		}
	}
	return out
}

func lockedManifestRoots(pw checks.ParsedWorkflow) []parserlock.ActionRef {
	recorded, _ := pw.PartitionRefs()
	return lockedRoots(recorded, pw.ExistingDeps)
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
