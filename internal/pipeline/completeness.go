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

func checkLockedManifests(ctx context.Context, opts RunOptions, parsed []checks.ParsedWorkflow, report *checks.Report, resolvedGraph *resolve.Graph, replaced checks.ReplacementProof, inspected map[int]map[string]bool, planned map[string][]string) error {
	if opts.Resolver == nil || opts.Store == nil {
		return nil
	}
	r := opts.Resolver
	file := opts.Store.File()
	deps := opts.Store.AllDeps()
	refs := lockfile.NewRefIndex(file)
	inspections, owners := lockedManifestInspections(opts, parsed, report, resolvedGraph, replaced, inspected, planned)
	var roots []parserlock.ActionRef
	for _, owner := range owners {
		roots = append(roots, owner.roots...)
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
	for i := range owners {
		owners[i].manifests, owners[i].err = graph.Manifests(owners[i].roots)
	}
	for _, inspection := range inspections {
		pw := parsed[inspection.workflow]
		wr := &report.Workflows[inspection.workflow]
		var retained []parserlock.ActionRef
		var discoveryErr error
		covered := map[string]bool{}
		unproven := map[string][]string{}
		coveredByOwner := map[string]map[string]bool{}
		requiredByOwner := map[string]map[string]bool{}
		for _, owner := range owners {
			localCoverage := coveredByOwner[owner.path]
			if localCoverage == nil {
				localCoverage = map[string]bool{}
				coveredByOwner[owner.path] = localCoverage
				requiredByOwner[owner.path] = map[string]bool{}
			}
			for _, root := range owner.roots {
				for _, key := range refs.Lookup(root.NWO(), root.Ref).Candidates {
					if inspection.keys[key] {
						retained = append(retained, root)
						covered[key] = true
						localCoverage[key] = true
					}
				}
			}
			for _, manifest := range owner.manifests {
				d := manifest.Entry.Dep
				for _, key := range refs.Lookup(d.NWO, d.Ref).Candidates {
					if inspection.keys[key] {
						retained = append(retained, manifest.Ref)
						covered[key] = true
						localCoverage[key] = true
					}
				}
			}
			relevant := false
			for key := range inspection.keys {
				if owner.keys[key] {
					relevant = true
					if owner.unproven[key] || owner.required[key] {
						requiredByOwner[owner.path][key] = true
					}
				}
			}
			if relevant {
				discoveryErr = errors.Join(discoveryErr, owner.err)
			}
		}
		for path, keys := range requiredByOwner {
			for key := range keys {
				if !coveredByOwner[path][key] {
					unproven[key] = append(unproven[key], path)
				}
			}
		}
		for key := range unproven {
			slices.Sort(unproven[key])
		}
		_, parents, err := graph.Inspect(resolve.DedupRequests(retained))
		err = errors.Join(discoveryErr, err)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		for _, key := range slices.Sorted(maps.Keys(inspection.keys)) {
			if err != nil && !onlyExplained(err) {
				continue
			}
			if !covered[key] || len(unproven[key]) > 0 {
				paths := inspection.owners[key]
				if len(unproven[key]) > 0 {
					paths = unproven[key]
				}
				appendUnprovenOwner(pw.Path, wr, key, paths)
				continue
			}
			for _, child := range file.Dependencies[key].Uses {
				if !slices.ContainsFunc(parents[dep.RefKeyOf(child)], func(parent string) bool {
					return dep.RefKeyOf(parent) == dep.RefKeyOf(key)
				}) {
					appendUnprovenOwner(pw.Path, wr, key, inspection.owners[key])
					break
				}
			}
		}
		appendLockedManifestFindings(pw.Path, wr, file, refs, parents, err)
	}
	return nil
}

type lockedInspection struct {
	workflow int
	keys     map[string]bool
	owners   map[string][]string
}

type manifestOwner struct {
	path      string
	roots     []parserlock.ActionRef
	keys      map[string]bool
	required  map[string]bool
	unproven  map[string]bool
	manifests []resolve.Manifest
	err       error
}

func lockedManifestInspections(opts RunOptions, parsed []checks.ParsedWorkflow, report *checks.Report, fresh *resolve.Graph, replaced checks.ReplacementProof, inspected map[int]map[string]bool, planned map[string][]string) ([]lockedInspection, []manifestOwner) {
	file := opts.Store.File()
	if opts.RecordedWorkflows != nil {
		file.Workflows = opts.RecordedWorkflows
	}
	refs := lockfile.NewRefIndex(file)
	var inspections []lockedInspection
	for i, pw := range parsed {
		wr := &report.Workflows[i]
		// Repairable diagnostics do not exempt retained pins from inspection.
		if isBlocked(pw) || wr.SkipCommit || wr.BlockingResolverError {
			continue
		}
		inspection := lockedInspection{workflow: i, keys: map[string]bool{}, owners: map[string][]string{}}
		add := func(ref parserlock.ActionRef) {
			for _, key := range refs.Lookup(ref.NWO(), ref.Ref).Candidates {
				recordedManifestKeys(file, refs, key, inspection.keys)
			}
		}
		recorded := lockedManifestRoots(pw)
		for _, ref := range recorded {
			add(ref)
		}
		for _, key := range planned[pw.Path] {
			if ref := parserlock.ParseActionRef(key); ref != nil {
				add(*ref)
			}
		}
		var manifestErr error
		if fresh != nil {
			var roots []parserlock.ActionRef
			for _, root := range fresh.Roots() {
				if !slices.ContainsFunc(pw.Refs, func(ref parserlock.ActionRef) bool {
					return ref.Path == root.Path && dep.RefKey(ref.NWO(), ref.Ref) == dep.RefKey(root.NWO(), root.Ref)
				}) {
					continue
				}
				// A read-only refresh must not import the live replacement
				// closure of a root the workflow already owns.
				if opts.ReadOnly && opts.Relock && slices.ContainsFunc(recorded, func(ref parserlock.ActionRef) bool {
					return ref.Path == root.Path && (dep.RefKey(ref.NWO(), ref.Ref) == dep.RefKey(root.NWO(), root.Ref) ||
						parserlock.IsFullSha(root.Ref) && strings.EqualFold(ref.NWO(), root.NWO()))
				}) {
					continue
				}
				roots = append(roots, root)
			}
			manifests, err := fresh.Manifests(roots)
			manifestErr = err
			for _, manifest := range manifests {
				add(manifest.Ref)
				d := manifest.Entry.Dep
				owner, repo := d.OwnerRepo()
				add(parserlock.ActionRef{Owner: owner, Repo: repo, Ref: d.Ref})
			}
		}
		if replaced != nil {
			for key := range inspection.keys {
				action := file.Dependencies[key]
				_, sha, _ := strings.Cut(action.Commit, "-")
				if replaced(checks.Deferred{
					Key: key, Ref: action.Ref, Commit: sha,
					Finding: checks.Finding{IncompleteLockfile: true},
				}) {
					delete(inspection.keys, key)
				}
			}
		}
		if manifestErr != nil && len(inspection.keys) > 0 {
			appendLockedManifestFindings(pw.Path, wr, file, refs, nil, manifestErr)
		}
		if previous, ok := inspected[i]; ok && maps.Equal(previous, inspection.keys) {
			continue
		}
		if inspected != nil {
			// Audit may drop writers and retry. Unchanged retained inputs
			// already have immutable coverage; only newly retained keys need
			// another traversal.
			inspected[i] = maps.Clone(inspection.keys)
		}
		inspections = append(inspections, inspection)
	}

	var owners []manifestOwner
	for _, path := range slices.Sorted(maps.Keys(file.Workflows)) {
		rootKeys := map[string]map[string]bool{}
		for _, key := range file.Workflows[path] {
			if pin, ok := parserlock.ParsePin(key); ok {
				for _, candidate := range refs.Lookup(pin.NWO, pin.Ref).Candidates {
					keys := map[string]bool{}
					recordedManifestKeys(file, refs, candidate, keys)
					rootKeys[candidate] = keys
				}
			}
		}
		relevant := map[string]bool{}
		for _, inspection := range inspections {
			for root, keys := range rootKeys {
				for key := range inspection.keys {
					if keys[key] {
						relevant[root] = true
						if !slices.Contains(inspection.owners[key], path) {
							inspection.owners[key] = append(inspection.owners[key], path)
						}
					}
				}
			}
		}
		if len(relevant) == 0 {
			continue
		}
		owner := ParseAll([]string{path}, opts.Store)[0]
		use := lockfile.RootUse(owner.Refs)
		for _, key := range slices.Sorted(maps.Keys(relevant)) {
			pin, ok := parserlock.ParsePin(key)
			if !ok {
				continue
			}
			action, ok := file.Dependencies[key]
			if !ok {
				continue
			}
			_, matches := use(pin, action.Commit)
			entryOwner := manifestOwner{
				path: path, keys: rootKeys[key], required: map[string]bool{key: true},
			}
			if !matches || owner.LoadErr != nil || owner.DepsErr != nil || isBlocked(owner) {
				entryOwner.unproven = rootKeys[key]
				owners = append(owners, entryOwner)
				continue
			}
			// Keep every declared path that names this owned root. A full
			// SHA use is inspected under its workflow-owned stored key.
			for _, ref := range owner.Refs {
				if _, ok := lockfile.RootUse([]parserlock.ActionRef{ref})(pin, action.Commit); ok {
					ref.Ref = pin.Ref
					entryOwner.roots = append(entryOwner.roots, ref)
				}
			}
			entryOwner.roots = resolve.DedupRequests(entryOwner.roots)
			owners = append(owners, entryOwner)
		}
	}
	return inspections, owners
}

func recordedManifestKeys(file parserlock.File, refs lockfile.RefIndex, key string, out map[string]bool) {
	if pin, ok := parserlock.ParsePin(key); ok {
		for _, candidate := range refs.Lookup(pin.NWO, pin.Ref).Candidates {
			if out[candidate] {
				continue
			}
			out[candidate] = true
			for _, child := range file.Dependencies[candidate].Uses {
				recordedManifestKeys(file, refs, child, out)
			}
		}
	}
}

func appendUnprovenOwner(path string, wr *checks.WorkflowReport, key string, owners []string) {
	pin, _ := parserlock.ParsePin(key)
	wr.Findings = append(dropValid(wr.Findings), checks.Finding{
		WorkflowPath:       path,
		Dependency:         &dep.Dependency{NWO: pin.NWO, Ref: pin.Ref},
		Category:           checks.NotPinned,
		Severity:           checks.SeverityError,
		Confidence:         checks.ConfidenceHigh,
		IncompleteLockfile: true,
		TrackedAction:      true,
		Detail:             fmt.Sprintf("cannot prove retained manifest paths for %s from owning workflows %s", key, strings.Join(owners, ", ")),
		Remediation:        "restore the owning workflow and graph from version control, or run `gh actions-lock --relock` over all owning workflows",
	})
	wr.SkipCommit = true
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
			ref := parserlock.ParseActionRef(child)
			missing := ref != nil && len(refs.Lookup(ref.NWO(), ref.Ref).Candidates) == 0
			if found && !missing {
				continue
			}
			if slices.ContainsFunc(wr.Findings, func(f checks.Finding) bool {
				return f.Category == checks.NotPinned && f.Severity == checks.SeverityError &&
					f.Dependency != nil && dep.RefKey(f.Dependency.NWO, f.Dependency.Ref) == dep.RefKeyOf(child) &&
					dep.RefKeyOf(f.ParentNWO) == dep.RefKeyOf(parent)
			}) {
				wr.SkipCommit = true
				continue
			}
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
			if missing && found {
				f.Detail = fmt.Sprintf("locked manifest for %s uses %s but its lockfile entry is missing", parent, child)
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
