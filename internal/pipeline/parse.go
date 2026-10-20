package pipeline

import (
	"context"
	"errors"

	parserlock "github.com/github/actions-lockfile/go/pkg/lockfile"
	"github.com/github/gh-actions-lock/internal/dep"
	"github.com/github/gh-actions-lock/internal/lockfile"
	"github.com/github/gh-actions-lock/internal/pinpool"
	"github.com/github/gh-actions-lock/internal/pipeline/checks"
	"github.com/github/gh-actions-lock/internal/resolve"
	"github.com/github/gh-actions-lock/internal/workflowfile"
)

// Diagnose scans workflows and produces findings for each.
//
// It is a backward-compatible wrapper around ParseAll, resolver pre-warming,
// and DiagnoseParsed. Newer callers can drive those phases directly to control
// UI progress.
func Diagnose(ctx context.Context, paths []string, r *resolve.Resolver, store *lockfile.State, pool *pinpool.Pool) *checks.Report {
	parsed := ParseAll(paths, store)
	if r != nil {
		refs, _ := CollectResolvable(parsed)
		if len(refs) > 0 {
			_, _, _ = r.ResolveAllRecursive(ctx, refs)
		}
	}
	return DiagnoseParsed(ctx, parsed, r, store, pool)
}

// ParseAll loads and parses every workflow path, returning a slice in input
// order. onScan, if non-nil, fires with 1-based progress before each workflow
// is parsed so the UI can render [i/N] without leaking resolver detail.
func ParseAll(paths []string, store *lockfile.State) []checks.ParsedWorkflow {
	total := len(paths)
	out := make([]checks.ParsedWorkflow, 0, total)
	for _, path := range paths {
		pw := checks.ParsedWorkflow{Path: path}
		if store != nil {
			wfKey := workflowfile.KeyFromPath(path)
			deps, depsErr := store.Get(wfKey)
			if depsErr != nil {
				pw.DepsErr = depsErr
			} else {
				pw.ExistingDeps = deps
			}
		}
		wf, err := workflowfile.Load(path)
		if err != nil {
			pw.LoadErr = err
			out = append(out, pw)
			continue
		}
		scan := wf.ExtractActionRefs()
		selfScan := workflowfile.ScanSelfRepositoryActions(path, scan.SelfRepositoryActionRefs)
		// A reference that can't be parsed is never skipped: the workflow
		// would lock without it.
		if bad := mergeStrings(scan.MalformedRefs, selfScan.MalformedRefs); len(bad) > 0 {
			errs := make([]error, len(bad))
			for i, msg := range bad {
				errs[i] = errors.New(msg)
			}
			pw.LoadErr = errors.Join(errs...)
			out = append(out, pw)
			continue
		}

		// Refs inside `$/…` actions are rewritable too: their action file is
		// rewritten alongside the workflow, so narrowing stays in sync.
		// RewriteRefs keep the spelling the files use; Refs carry each
		// reference's identity, a full SHA lowercased, for everything that
		// compares them.
		pw.RewriteRefs = mergeActionRefs(scan.Refs, selfScan.Refs)
		pw.Refs = mergeActionRefs(normalizeRefs(pw.RewriteRefs))
		pw.SelfActionFiles = selfScan.ActionFiles
		pw.SelfActionRefs = selfScan.Refs
		pw.LocalPaths = mergeStrings(scan.LocalPaths, selfScan.LocalPaths)
		pw.SelfRepositoryRefs = mergeStrings(scan.SelfRepositoryRefs, selfScan.SelfRepositoryRefs)
		pw.SelfRepositoryRefErrs = mergeStrings(scan.SelfRepositoryRefErrs, selfScan.SelfRepositoryRefErrs)
		pw.SelfRepositoryResolutionErrs = selfScan.Errors
		pw.ParseWarnings = append(scan.Warnings, selfScan.Warnings...)
		out = append(out, pw)
	}
	return out
}

// normalizeRefs lowercases each full-SHA ref: git names a commit by its SHA
// in any case. Tag and branch names are case-sensitive and kept.
func normalizeRefs(refs []parserlock.ActionRef) []parserlock.ActionRef {
	out := make([]parserlock.ActionRef, len(refs))
	for i, ref := range refs {
		out[i] = dep.NormalizeRef(ref)
	}
	return out
}

func mergeActionRefs(groups ...[]parserlock.ActionRef) []parserlock.ActionRef {
	seen := make(map[string]bool)
	var refs []parserlock.ActionRef
	for _, group := range groups {
		for _, ref := range group {
			key := ref.FullName() + "@" + ref.Ref
			if seen[key] {
				continue
			}
			seen[key] = true
			refs = append(refs, ref)
		}
	}
	return refs
}

func mergeStrings(groups ...[]string) []string {
	seen := make(map[string]bool)
	var values []string
	for _, group := range groups {
		for _, value := range group {
			if seen[value] {
				continue
			}
			seen[value] = true
			values = append(values, value)
		}
	}
	return values
}

// CollectResolvable returns the deduplicated union of refs and existing deps
// across all parsed workflows. Use the returned slices to pre-warm the
// resolver caches once before per-workflow diagnostics.
func CollectResolvable(parsed []checks.ParsedWorkflow) ([]parserlock.ActionRef, []dep.Dependency) {
	return collectResolvable(parsed, resolve.RefSet{})
}

// CollectUnrecordedResolvable is like CollectResolvable but excludes refs
// and deps whose owner/repo@ref is in recorded. Use this when per-dep
// lockfile trust has already seeded the resolver cache for recorded deps,
// so only genuinely new refs need network resolution.
func CollectUnrecordedResolvable(parsed []checks.ParsedWorkflow, recorded resolve.RefSet) ([]parserlock.ActionRef, []dep.Dependency) {
	return collectResolvable(parsed, recorded)
}

func collectResolvable(parsed []checks.ParsedWorkflow, exclude resolve.RefSet) ([]parserlock.ActionRef, []dep.Dependency) {
	var refs []parserlock.ActionRef
	for _, pw := range parsed {
		for _, ref := range pw.Refs {
			if exclude.Has(ref) {
				continue
			}
			refs = append(refs, ref)
		}
	}
	refs = resolve.DedupRequests(refs)
	seenDep := make(map[string]bool)
	var deps []dep.Dependency
	for _, pw := range parsed {
		for _, dep := range pw.ExistingDeps {
			key := dep.Key()
			owner, repo := dep.OwnerRepo()
			if exclude.Has(parserlock.ActionRef{Owner: owner, Repo: repo, Ref: dep.Ref}) || seenDep[key] {
				continue
			}
			seenDep[key] = true
			deps = append(deps, dep)
		}
	}
	return refs, deps
}

// DiagnoseParsed runs the engine diagnostics for each pre-parsed workflow.
// Assumes the resolver caches have already been warmed (calls into the
// resolver will hit cache and stay silent). Returns a checks.Report aggregating per-
// workflow findings in input order.
