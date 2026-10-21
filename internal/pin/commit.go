package pin

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"runtime"
	"slices"

	"github.com/github/gh-actions-lock/internal/dep"
	"github.com/github/gh-actions-lock/internal/lockfile"
	"github.com/github/gh-actions-lock/internal/workflowfile"
	"golang.org/x/sync/errgroup"
)

// CommitOptions configures the Commit pass.
type CommitOptions struct {
	// OnProgress is called at each phase boundary. Nil means no progress.
	OnProgress func(phase string)
	// SkipNewWorkflowEntries forces the Commit phase to skip workflows with no existing lockfile entry.
	SkipNewWorkflowEntries bool
}

// Commit writes a planned Record to disk: rewrites workflow files and
// updates the lockfile store. All-or-nothing semantics — if any write
// fails, previously written files are not rolled back (best-effort),
// but the error is returned immediately.
func Commit(ctx context.Context, rec *Record, store *lockfile.State, copts *CommitOptions) error {
	progress := func(string) {}
	if copts != nil && copts.OnProgress != nil {
		progress = copts.OnProgress
	}
	rec.Workflows = committedWorkflows(rec, store, copts)
	if err := validateRequiredRewrites(rec.Workflows); err != nil {
		return err
	}

	// Phase 1: Update lockfile entries in memory. It runs before any file is
	// rewritten, so an entry or graph the lockfile refuses leaves every file
	// as it was.
	if len(rec.Workflows) > 0 {
		progress("Updating lockfile")
	}
	if err := updateLockfile(ctx, rec, rec.Workflows, store); err != nil {
		return err
	}
	committed := make([]string, len(rec.Workflows))
	for i, wp := range rec.Workflows {
		committed[i] = workflowfile.KeyFromPath(wp.Path)
	}
	if err := store.Validate(committed...); err != nil {
		return err
	}

	// Phase 2: Rewrite workflow files (uses: line changes).
	if len(rec.Workflows) > 0 {
		progress("Rewriting workflows")
	}

	// Rewrite workflow files in parallel. Uses errgroup rather than pinpool
	// because this is fast local I/O — no progress reporting or API rate
	// limiting needed. The pool is for network-bound work.
	g, _ := errgroup.WithContext(ctx)
	g.SetLimit(runtime.GOMAXPROCS(0))
	for _, wp := range rec.Workflows {
		g.Go(func() error {
			if err := rewriteWorkflow(wp); err != nil {
				return fmt.Errorf("rewriting %s: %w", wp.Path, err)
			}
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return err
	}

	// Action files can be shared by several workflows, so merge their
	// rewrites and write each file once, serially.
	if err := rewriteSelfActionFiles(rec.Workflows); err != nil {
		return err
	}

	// Phase 3: Persist lockfile to disk.
	if err := store.Save(); err != nil {
		return fmt.Errorf("saving lockfile: %w", err)
	}

	return nil
}

// committedWorkflows are the planned workflows Commit writes.
func committedWorkflows(rec *Record, store *lockfile.State, copts *CommitOptions) []WorkflowPlan {
	if copts == nil || !copts.SkipNewWorkflowEntries {
		return rec.Workflows
	}
	var workflows []WorkflowPlan
	for _, wp := range rec.Workflows {
		if store.HasWorkflow(workflowfile.KeyFromPath(wp.Path)) {
			workflows = append(workflows, wp)
		}
	}
	return workflows
}

// updateLockfile applies rec's entry repairs for the workflows it writes,
// clears the recorded graph those of them that refresh replace, then sets
// each workflow's entries in store. Commit and every preview (Audit,
// PlanEntryRepairs) go through it, so a preview shows what Commit writes.
func updateLockfile(ctx context.Context, rec *Record, workflows []WorkflowPlan, store *lockfile.State) error {
	written := make(map[string]bool, len(workflows))
	for _, wp := range workflows {
		written[wp.Path] = true
	}
	pinnedByWorkflow := groupPinnedByWorkflow(rec)
	// A refreshed workflow's graph replaces its recorded one rather than
	// merging into it, so edges it alone owned are cleared first.
	var refreshed []string
	var refreshDeps []dep.Dependency
	refreshParents := make(dep.ParentMap)
	for _, wp := range workflows {
		if wp.Refresh {
			refreshed = append(refreshed, workflowfile.KeyFromPath(wp.Path))
			refreshDeps = append(refreshDeps, pinnedByWorkflow[wp.Path]...)
			for child, parents := range buildParentMap(rec, wp.Path) {
				refreshParents[child] = append(refreshParents[child], parents...)
			}
		}
	}
	if len(refreshed) > 0 {
		if err := store.PrepareRefresh(refreshed, refreshDeps, refreshParents); err != nil {
			return fmt.Errorf("refreshing lockfile graph: %w", err)
		}
	}
	for _, r := range rec.EntryRepairs {
		if !slices.ContainsFunc(r.Workflows, func(p string) bool { return written[p] }) {
			continue
		}
		if err := store.SetEntryRef(r.Key, r.Ref); err != nil {
			return fmt.Errorf("updating lockfile: %w", err)
		}
	}
	for _, wp := range workflows {
		wfPath := wp.Path
		wfKey := workflowfile.KeyFromPath(wfPath)
		deps := pinnedByWorkflow[wfPath]
		parentMap := buildParentMap(rec, wfPath)
		directKeys := buildDirectKeys(rec, wfPath)
		deps = retainUnresolvablePins(rec, store, wfPath, deps, directKeys)
		if err := store.Set(ctx, wfKey, deps, parentMap, directKeys); err != nil {
			return fmt.Errorf("updating lockfile for %s: %w", wfPath, err)
		}
	}
	return nil
}

func rewriteWorkflow(wp WorkflowPlan) error {
	wf, err := workflowfile.Load(wp.Path)
	if err != nil {
		return err
	}

	content := wf.Content
	if len(wp.Rewrites) > 0 {
		content, _, err = wf.RewriteActionRefs(fileRewrites(wp.Rewrites))
		if err != nil {
			return fmt.Errorf("applying rewrites: %w", err)
		}
	}

	if bytes.Equal(content, wf.Content) {
		return nil
	}
	return os.WriteFile(wp.Path, content, 0o644)
}

// rewriteSelfActionFiles applies each workflow's rewrites to the in-repo
// action files it reaches via `$/…`.
func rewriteSelfActionFiles(plans []WorkflowPlan) error {
	merged := make(map[string]map[string]string)
	for _, wp := range plans {
		if len(wp.Rewrites) == 0 {
			continue
		}
		for _, path := range wp.SelfActionFiles {
			if merged[path] == nil {
				merged[path] = make(map[string]string)
			}
			for oldUses, newUses := range wp.Rewrites {
				merged[path][string(oldUses)] = newUses
			}
		}
	}
	for path, rewrites := range merged {
		wf, err := workflowfile.Load(path)
		if err != nil {
			return fmt.Errorf("loading %s: %w", path, err)
		}
		content, changed, err := wf.RewriteActionRefs(rewrites)
		if err != nil {
			return fmt.Errorf("rewriting %s: %w", path, err)
		}
		if changed == 0 || bytes.Equal(content, wf.Content) {
			continue
		}
		if err := os.WriteFile(path, content, 0o644); err != nil {
			return fmt.Errorf("writing %s: %w", path, err)
		}
	}
	return nil
}

func isPinOrVerified(r Resolution) bool {
	return r == Pinned || r == Verified
}

func groupPinnedByWorkflow(rec *Record) map[string][]dep.Dependency {
	result := make(map[string][]dep.Dependency)
	for _, e := range rec.Entries {
		if !isPinOrVerified(e.Resolution) {
			continue
		}
		for _, wf := range e.Workflows {
			result[wf] = append(result[wf], dep.Dependency{
				Hostname:    e.Hostname,
				NWO:         e.NWO,
				Ref:         e.Ref,
				SHA:         e.SHA,
				OriginalRef: e.AutoFixedRef,
				RenamedFrom: e.RenamedFrom,
				Branch:      e.OnBranch,
				Tag:         e.Tag,
			})
		}
	}
	return result
}

// retainUnresolvablePins re-adds the workflow's existing on-disk pins for any
// entry that cannot be resolved this run (flagged for investigation or transiently
// unresolvable, e.g. 403/SSO). Without this a co-located re-pin silently
// drops the existing pin. Planned refs are spelled as the workflow spells
// them, so they reach recorded pins by reference identity.
func retainUnresolvablePins(rec *Record, store *lockfile.State, wfPath string, deps []dep.Dependency, directKeys map[dep.Key]bool) []dep.Dependency {
	retain := make(map[dep.Key]bool)
	for _, e := range rec.Entries {
		if e.Resolution != Unresolved && e.Resolution != Investigate {
			continue
		}
		for _, wf := range e.Workflows {
			if wf == wfPath {
				retain[dep.RefKey(e.NWO, e.Ref)] = true
			}
		}
	}
	if len(retain) == 0 {
		return deps
	}
	existing, err := store.Get(workflowfile.KeyFromPath(wfPath))
	if err != nil {
		return deps
	}
	have := make(map[dep.Key]bool, len(deps))
	for _, d := range deps {
		have[d.ID()] = true
	}
	for _, d := range existing {
		k := d.ID()
		if retain[k] && !have[k] {
			deps = append(deps, d)
			directKeys[k] = true
			have[k] = true
		}
	}
	return deps
}

func buildParentMap(rec *Record, wfPath string) dep.ParentMap {
	pm := make(dep.ParentMap)
	for _, e := range rec.Entries {
		if !isPinOrVerified(e.Resolution) {
			continue
		}
		for _, wf := range e.Workflows {
			if wf != wfPath {
				continue
			}
			if len(e.RequiredBy) > 0 {
				pm[dep.RefKey(e.NWO, e.Ref)] = e.RequiredBy
			}
		}
	}
	return pm
}

func buildDirectKeys(rec *Record, wfPath string) map[dep.Key]bool {
	keys := make(map[dep.Key]bool)
	for _, e := range rec.Entries {
		if !isPinOrVerified(e.Resolution) || !e.Direct {
			continue
		}
		for _, wf := range e.Workflows {
			if wf == wfPath {
				keys[dep.RefKey(e.NWO, e.Ref)] = true
			}
		}
	}
	return keys
}

// fileRewrites is rewrites as workflowfile takes them.
func fileRewrites(rewrites map[dep.Spelling]string) map[string]string {
	out := make(map[string]string, len(rewrites))
	for from, to := range rewrites {
		out[string(from)] = to
	}
	return out
}

func validateRequiredRewrites(plans []WorkflowPlan) error {
	for i := range plans {
		wp := &plans[i]
		if len(wp.RequiredRewrites) == 0 {
			continue
		}
		found := make(map[string]int)
		wp.RewrittenIn = make(map[dep.Spelling][]string)
		for _, path := range append([]string{wp.Path}, wp.SelfActionFiles...) {
			wf, err := workflowfile.Load(path)
			if err != nil {
				return fmt.Errorf("validating required rewrites in %s: %w", path, err)
			}
			matches, err := wf.ValidateRequiredActionRefRewrites(fileRewrites(wp.RequiredRewrites))
			if err != nil {
				return fmt.Errorf("validating required rewrites in %s: %w", path, err)
			}
			for oldUse, n := range matches {
				found[oldUse] += n
				if n > 0 {
					wp.RewrittenIn[dep.Spelling(oldUse)] = append(wp.RewrittenIn[dep.Spelling(oldUse)], path)
				}
			}
		}
		for oldUse := range wp.RequiredRewrites {
			if found[string(oldUse)] == 0 {
				return fmt.Errorf("required action rewrite for %s was not found in writable workflow sources", oldUse)
			}
		}
	}
	return nil
}
