package resolve

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"

	parserlock "github.com/github/actions-lockfile/go/pkg/lockfile"
	"github.com/github/gh-actions-lock/internal/dep"
	"github.com/github/gh-actions-lock/internal/ghapi"
	"github.com/github/gh-actions-lock/internal/pinpool"
	"github.com/github/gh-actions-lock/internal/uses"
)

// CompositeLocalPathError is returned when a remote composite action uses a
// local path (./…) reference in its steps. We cannot resolve transitive
// dependencies behind such references, so the workflow must be blocked.
type CompositeLocalPathError struct {
	// Parent is the NWO@Ref of the composite action containing the local path.
	Parent string
	// LocalPath is the ./… value found in the composite's steps.
	LocalPath string
}

func (e *CompositeLocalPathError) Error() string {
	return fmt.Sprintf("composite action %s uses local path %q which cannot be resolved for pinning", e.Parent, e.LocalPath)
}

// IsCompositeLocalPath reports whether err (or any error in its chain)
// is a CompositeLocalPathError.
func IsCompositeLocalPath(err error) bool {
	var target *CompositeLocalPathError
	return errors.As(err, &target)
}

// InvalidSelfRepositoryRefError is returned when a fetched composite carries
// the invalid `$/…@ref` form.
type InvalidSelfRepositoryRefError struct {
	Parent string
	Ref    string
}

func (e *InvalidSelfRepositoryRefError) Error() string {
	return fmt.Sprintf("composite action %s uses self repository reference %q with a forbidden @ref", e.Parent, e.Ref)
}

// IsInvalidSelfRepositoryRef reports whether err contains an invalid nested
// self repository reference.
func IsInvalidSelfRepositoryRef(err error) bool {
	var target *InvalidSelfRepositoryRefError
	return errors.As(err, &target)
}

// MalformedNestedRefError is returned when a fetched composite's `uses:`
// names a remote action the parser can't accept. It is never skipped: the
// composite's dependencies would be locked without it.
type MalformedNestedRefError struct {
	Parent string
	Ref    string
}

func (e *MalformedNestedRefError) Error() string {
	return fmt.Sprintf("composite action %s: %s", e.Parent, uses.Classify(e.Ref, uses.Step).Err())
}

// selfRepositoryPrefix marks a `$/…` self repository action inside a composite's
// nested uses. Kept local to avoid importing the workflowfile package into the
// resolver; the sibling detection here is a plain prefix check.
const selfRepositoryPrefix = "$/"

type resolutionRequest struct {
	ref      parserlock.ActionRef
	fetchRef string
}

// LatestRef returns the highest stable tag for an action repository.
func (r *Resolver) LatestRef(ctx context.Context, owner, repo string) (string, error) {
	key := ghapi.ForRepo(owner, repo)
	if ref, ok := r.latestRefCache.Get(key); ok {
		return ref, nil
	}

	entries, err := r.gh.ListTags(ctx, owner, repo)
	if err != nil {
		return "", err
	}

	tags := make([]string, 0, len(entries))
	for _, e := range entries {
		tags = append(tags, e.Name)
	}

	best := selectLatestTag(tags)
	if best == "" {
		return "", fmt.Errorf("%s: no tags available to upgrade", key)
	}

	r.latestRefCache.Put(key, best)
	return best, nil
}

func cacheKey(ref parserlock.ActionRef) ghapi.ActionRef {
	return ghapi.ForActionRef(ref.Owner, ref.Repo, ref.Path, ref.Ref)
}

// batchActionFileSize caps how many action refs are folded into a single
// batched ResolveActionFiles GraphQL round-trip. The action-file query is
// heavy per alias (repository + object + two blob fetches), so this stays well
// under reachability's branch batch size. ResolveActionFiles splits adaptively
// if a chunk is still rejected for cost, so this is an upper bound, not a
// fixed request shape.
const batchActionFileSize = 20

// ResolveAllRecursive resolves action refs and recursively discovers transitive
// dependencies from composite actions by reading their action.yml via GraphQL.
// The returned ParentMap (child dep key → parent dep keys) is owned by the
// caller and safe to mutate or hold across concurrent resolver calls.
func (r *Resolver) ResolveAllRecursive(ctx context.Context, refs []parserlock.ActionRef) ([]dep.Dependency, dep.ParentMap, error) {
	g, err := r.resolveRecursive(ctx, refs, nil)
	deps, parents := g.Flatten()
	return deps, parents, err
}

// UnlockedRefError identifies a dependency discovered in a locked manifest
// without a recorded commit. Verification must not resolve its mutable ref.
type UnlockedRefError struct {
	Ref parserlock.ActionRef
}

func (e *UnlockedRefError) Error() string {
	return fmt.Sprintf("no locked commit for %s@%s", e.Ref.FullName(), e.Ref.Ref)
}

// ResolveLockedRecursive discovers dependencies using only recorded commits.
// Its content cache is separate from live-ref resolution and lockfile seeds.
func (r *Resolver) ResolveLockedRecursive(ctx context.Context, refs []parserlock.ActionRef, deps []dep.Dependency) ([]dep.Dependency, dep.ParentMap, error) {
	locked := make(map[ghapi.NWORef]string, len(deps))
	for _, d := range deps {
		owner, repo := d.OwnerRepo()
		if d.SHA != "" {
			locked[ghapi.ForNWORef(owner, repo, d.Ref)] = d.SHA
		}
	}
	g, err := r.resolveRecursive(ctx, refs, locked)
	deps, parents := g.Flatten()
	return deps, parents, err
}

func (r *Resolver) resolveRecursive(ctx context.Context, refs []parserlock.ActionRef, locked map[ghapi.NWORef]string) (*Graph, error) {
	seen := make(map[ghapi.ActionRef]bool)
	g := newGraph(refs)

	pending := make([]resolutionRequest, len(refs))
	for i, ref := range refs {
		pending[i] = resolutionRequest{ref: ref}
	}
	depth := 0

	// Rolling counters spanning every BFS depth. Total grows as transitive
	// refs are discovered; done grows as workers complete. The caller renders
	// a single non-jumping bar over the union of work — "transitive" is not a
	// distinct top-level phase, just deeper edges in the same graph.
	var resolveDone atomic.Int64
	var resolveTotal atomic.Int64
	// Fire an initial 0/N callback so the UI shows the bar immediately at the
	// known size of the first wave (refs the caller passed in).
	if r.OnResolveProgress != nil {
		// Compute first-wave uncached size up-front for an accurate initial
		// total. Dedup by cacheKey to mirror the seen{} dedup the BFS loop
		// applies before resolving, so duplicate input refs don't inflate the
		// total and leave the bar stranded short of [N/N].
		firstWave := 0
		seenFirst := make(map[ghapi.ActionRef]bool, len(refs))
		for _, ref := range refs {
			key := cacheKey(ref)
			if seenFirst[key] {
				continue
			}
			seenFirst[key] = true
			if _, ok := r.cache.Get(key); !ok {
				firstWave++
			}
		}
		resolveTotal.Store(int64(firstWave))
		if firstWave > 0 {
			r.FireResolveProgress(0, firstWave)
		}
	}

	var resolveErr error

	for len(pending) > 0 {
		if depth >= r.MaxRecursionDepth {
			return g, fmt.Errorf("composite action recursion exceeded max depth %d", r.MaxRecursionDepth)
		}

		var toResolve []resolutionRequest
		for _, request := range pending {
			key := cacheKey(request.ref)
			if !seen[key] {
				seen[key] = true
				if locked != nil && request.fetchRef == "" {
					request.fetchRef = locked[ghapi.ForNWORef(request.ref.Owner, request.ref.Repo, request.ref.Ref)]
					if request.fetchRef == "" {
						resolveErr = errors.Join(resolveErr, &UnlockedRefError{Ref: request.ref})
						continue
					}
				}
				toResolve = append(toResolve, request)
			}
		}

		if len(toResolve) == 0 {
			break
		}

		deps, actionYMLs, requested, err := r.resolveWithActionYMLParallel(ctx, toResolve, depth, &resolveDone, &resolveTotal, locked != nil)
		if depth > 0 || ghapi.DependabotProxy() || locked != nil {
			// A remote composite's `uses:` can't be rewritten here, and the
			// runner looks it up as written, so keep the name it uses. Nor
			// can Dependabot's, which reads back only actions.lock.
			for i := range deps {
				if len(deps[i].OriginalRefs) > 0 {
					deps[i].NWO = deps[i].OriginalRefs[0].NWO()
					deps[i].OriginalRefs = nil
				}
			}
		}
		// Keep partial results: per-ref failures are surfaced via err, but
		// successful resolutions in `deps` should not be discarded — downstream
		// renderers degrade gracefully per-ref instead of marking everything
		// unresolved. Continue BFS so transitive deps of successful refs are
		// still discovered.
		for i := range deps {
			g.setResolved(requested[i], deps[i])
		}
		if err != nil {
			resolveErr = errors.Join(resolveErr, err)
		}

		var nextPending []resolutionRequest
		for i := range deps {
			yml := actionYMLs[i]
			if yml == "" {
				if locked != nil && !isWorkflowCallee(deps[i].Path) {
					resolveErr = errors.Join(resolveErr, fmt.Errorf("action manifest is missing at locked commit for %s", deps[i].Key()))
				}
				continue
			}

			meta, parseErr := parserlock.ParseActionMeta(yml)
			if parseErr != nil {
				resolveErr = errors.Join(resolveErr, fmt.Errorf("reading manifest for %s: %w", deps[i].Key(), parseErr))
			}
			if parseErr != nil || meta.Execution != parserlock.ExecComposite {
				continue
			}

			parentKey := deps[i].Key()
			for _, rawUse := range meta.NestedUses {
				use := strings.TrimSpace(rawUse)
				if strings.HasPrefix(use, "./") {
					resolveErr = errors.Join(resolveErr, &CompositeLocalPathError{
						Parent:    parentKey,
						LocalPath: use,
					})
					continue
				}
				var request resolutionRequest
				if strings.HasPrefix(use, selfRepositoryPrefix) {
					if strings.Contains(use, "@") {
						resolveErr = errors.Join(resolveErr, &InvalidSelfRepositoryRefError{
							Parent: parentKey,
							Ref:    use,
						})
						continue
					}
					// `$/…` inside a composite is a same-tarball self repository reference:
					// it resolves within THIS composite's own repo at its own
					// resolved SHA (the parent repo, not the workflow-run repo).
					// Retain the logical ref for lockfile identity while fetching
					// the sibling action file from the immutable parent commit.
					owner, repo := deps[i].OwnerRepo()
					request = resolutionRequest{
						ref: parserlock.ActionRef{
							Owner: owner,
							Repo:  repo,
							Path:  strings.TrimPrefix(use, selfRepositoryPrefix),
							Ref:   deps[i].Ref,
						},
						fetchRef: deps[i].SHA,
					}
				} else {
					c := uses.Classify(use, uses.Step)
					if c.Kind == uses.Invalid {
						resolveErr = errors.Join(resolveErr, &MalformedNestedRefError{Parent: parentKey, Ref: use})
					}
					if c.Kind != uses.Action {
						continue
					}
					request = resolutionRequest{ref: c.Action}
				}
				actionRef := request.ref
				if actionRef.Owner == "" {
					continue
				}
				// Edges are path-aware: a sub-action of the same repo+ref is
				// its own node, so its own dependencies and any cycle through
				// it stay visible. The seen{} set makes this enqueue loop-safe.
				g.addEdge(requested[i], actionRef)
				nextPending = append(nextPending, request)
			}
		}

		pending = nextPending
		depth++
	}

	if err := g.conflictingCommits(); err != nil {
		resolveErr = errors.Join(resolveErr, err)
	}
	if c := g.cycle(); c != nil {
		resolveErr = errors.Join(resolveErr, c)
	}
	return g, resolveErr
}

// resolveWithActionYMLParallel resolves refs one-per-worker via the shared pool
// so the UI can show a stable [done/total] counter and per-worker status rows.
// Cached refs short-circuit without a worker.
//
// resolveDone and resolveTotal are rolling counters owned by ResolveAllRecursive
// that span every BFS depth, so a single non-jumping progress bar can cover
// direct + transitive resolution as one phase.
// The returned requests align with the returned deps.
func (r *Resolver) resolveWithActionYMLParallel(ctx context.Context, refs []resolutionRequest, depth int, resolveDone, resolveTotal *atomic.Int64, locked bool) ([]dep.Dependency, []string, []parserlock.ActionRef, error) {
	type resolveResult struct {
		dep dep.Dependency
		yml string
		ok  bool
	}
	results := make([]resolveResult, len(refs))

	var uncachedIdx []int
	for i, request := range refs {
		cache := &r.cache
		key := cacheKey(request.ref)
		if locked {
			cache = &r.lockedContentCache
			key = ghapi.ForActionRef(request.ref.Owner, request.ref.Repo, request.ref.Path, request.fetchRef)
		}
		if entry, ok := cache.Get(key); ok {
			entry.dep.Ref = request.ref.Ref
			results[i] = resolveResult{dep: entry.dep, yml: entry.actionYML, ok: true}
		} else {
			uncachedIdx = append(uncachedIdx, i)
		}
	}

	flatten := func() ([]dep.Dependency, []string, []parserlock.ActionRef) {
		var deps []dep.Dependency
		var ymls []string
		var requested []parserlock.ActionRef
		for i, res := range results {
			if !res.ok {
				continue
			}
			deps = append(deps, res.dep)
			ymls = append(ymls, res.yml)
			requested = append(requested, refs[i].ref)
		}
		return deps, ymls, requested
	}

	total := len(uncachedIdx)
	if total == 0 {
		deps, ymls, requested := flatten()
		return deps, ymls, requested, nil
	}

	// Grow the rolling resolve total by the new uncached refs at this depth.
	// First-wave size was preseeded by ResolveAllRecursive, so only count
	// deeper depths here to avoid double-counting.
	if depth > 0 {
		newTotal := resolveTotal.Add(int64(total))
		r.FireResolveProgress(int(resolveDone.Load()), int(newTotal))
	}

	// Group the wave's uncached refs into batches, each resolved in one
	// ResolveActionFiles GraphQL round-trip instead of one request per ref.
	// One pool job == one batch; the per-ref progress counter still ticks once
	// per ref so the bar stays ref-denominated. The pool runs with an empty
	// label so its batch-denominated counter doesn't fight FireResolveProgress.
	type actionBatch struct {
		idxs []int                     // global indices into refs/results
		reqs []ghapi.ActionFileRequest // aligned 1:1 with idxs
	}
	var batches []actionBatch
	for start := 0; start < len(uncachedIdx); start += batchActionFileSize {
		end := start + batchActionFileSize
		if end > len(uncachedIdx) {
			end = len(uncachedIdx)
		}
		span := uncachedIdx[start:end]
		b := actionBatch{
			idxs: make([]int, len(span)),
			reqs: make([]ghapi.ActionFileRequest, len(span)),
		}
		for j, idx := range span {
			request := refs[idx]
			fetchRef := request.fetchRef
			if fetchRef == "" {
				fetchRef = request.ref.Ref
			}
			b.idxs[j] = idx
			b.reqs[j] = ghapi.ActionFileRequest{
				Owner: request.ref.Owner, Repo: request.ref.Repo,
				Path: request.ref.Path, Ref: fetchRef,
			}
		}
		batches = append(batches, b)
	}

	// The pool keeps only the first failing batch; collect every batch's
	// errors so a 404 in one batch can't hide a 5xx in another.
	var (
		batchMu   sync.Mutex
		batchErrs []error
	)
	poolErr := pinpool.RunTyped(r.Pool, ctx, "",
		batches,
		func(b actionBatch) string {
			head := refs[b.idxs[0]].ref
			label := "resolving " + head.NWO() + "@" + head.Ref
			if len(b.idxs) > 1 {
				label = fmt.Sprintf("%s (+%d more)", label, len(b.idxs)-1)
			}
			return label
		},
		func(ctx context.Context, _ int, b actionBatch) error {
			res := r.gh.ResolveActionFiles(ctx, b.reqs)
			var errs []error
			for j, idx := range b.idxs {
				ref := refs[idx].ref
				if j < len(res) && res[j].Err == nil && res[j].ActionYML == "" && !isWorkflowCallee(res[j].Path) {
					errs = append(errs, fmt.Errorf("%s@%s: action manifest is missing at %s", ref.NWO(), ref.Ref, res[j].CommitOID))
				} else if j < len(res) && res[j].Err == nil {
					d := dep.Dependency{
						Hostname: res[j].Hostname,
						NWO:      res[j].Owner + "/" + res[j].Repo,
						Path:     res[j].Path,
						Ref:      ref.Ref,
						SHA:      res[j].CommitOID,
					}
					if res[j].OriginalNWO != "" {
						d.OriginalRefs = []parserlock.ActionRef{ref}
					}
					entry := resolvedEntry{dep: d, actionYML: res[j].ActionYML}
					if locked {
						r.lockedContentCache.Put(ghapi.ForActionRef(ref.Owner, ref.Repo, ref.Path, refs[idx].fetchRef), entry)
					} else {
						r.cache.Put(cacheKey(ref), entry)
					}
					results[idx] = resolveResult{dep: d, yml: res[j].ActionYML, ok: true}
				} else if j < len(res) && res[j].Err != nil {
					errs = append(errs, fmt.Errorf("%s@%s: %w", ref.NWO(), ref.Ref, res[j].Err))
				}
				done := resolveDone.Add(1)
				r.FireResolveProgress(int(done), int(resolveTotal.Load()))
			}
			err := errors.Join(errs...)
			if err != nil {
				batchMu.Lock()
				batchErrs = append(batchErrs, err)
				batchMu.Unlock()
			}
			return err
		},
	)
	if len(batchErrs) > 0 {
		poolErr = errors.Join(batchErrs...)
	}

	deps, ymls, requested := flatten()
	return deps, ymls, requested, poolErr
}

// isWorkflowCallee reports whether path names a reusable workflow, which has
// no action manifest.
func isWorkflowCallee(path string) bool {
	return strings.HasPrefix(path, ".github/workflows/") &&
		(strings.HasSuffix(path, ".yml") || strings.HasSuffix(path, ".yaml"))
}

// selectLatestTag returns the highest semver tag from a list of tag names.
// Prefers vN (major-only) over vN.M.P when a matching major exists.
func selectLatestTag(tags []string) string {
	seen := make(map[string]struct{}, len(tags))
	bestMajor := -1
	bestMajorTag := ""
	bestVersion := [3]int{-1, -1, -1}
	bestVersionTag := ""
	bestFallback := ""

	for _, tag := range tags {
		tag = strings.TrimSpace(tag)
		if tag == "" {
			continue
		}
		if _, ok := seen[tag]; ok {
			continue
		}
		seen[tag] = struct{}{}
		if tag > bestFallback {
			bestFallback = tag
		}

		sv, ok := parserlock.ParseSemVer(tag)
		if !ok || !sv.IsStable() {
			continue
		}

		if sv.Raw == sv.MajorTag() && sv.Major > bestMajor {
			bestMajor = sv.Major
			bestMajorTag = tag
		}

		version := [3]int{sv.Major, sv.Minor, sv.Patch}
		if version[0] > bestVersion[0] ||
			(version[0] == bestVersion[0] && version[1] > bestVersion[1]) ||
			(version[0] == bestVersion[0] && version[1] == bestVersion[1] && version[2] > bestVersion[2]) {
			bestVersion = version
			bestVersionTag = tag
		}
	}

	if bestMajorTag != "" {
		return bestMajorTag
	}
	if bestVersionTag != "" {
		return bestVersionTag
	}
	return bestFallback
}
