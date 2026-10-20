package pin

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"

	parserlock "github.com/github/actions-lockfile/go/pkg/lockfile"
	"github.com/github/gh-actions-lock/internal/lockfile"
	"github.com/github/gh-actions-lock/internal/pipeline/checks"
	"github.com/github/gh-actions-lock/internal/resolve"
	"github.com/github/gh-actions-lock/internal/workflowfile"
)

// EntryRepair re-records the ref of an existing lockfile entry.
type EntryRepair struct {
	Key string
	Ref string
}

// PlanEntryRepairs finds the tag at the commit, else a branch containing
// it, for each entry recording its own SHA as its ref that the planned write
// leaves reachable from a workflow it updates. It previews that write with
// the code Commit uses (see Audit), so whatever keeps such an entry, a
// workflow's own pin, a composite the run reuses from the lock, or a root it
// replaces, is followed. An entry only unwritten workflows reach is left as
// recorded, and one nothing reaches is dropped by Save. A commit no tag or
// branch contains is Unresolved, so nothing is written.
func PlanEntryRepairs(ctx context.Context, rec *Record, store *lockfile.State, resolver *resolve.Resolver, report *checks.Report, copts *CommitOptions) error {
	if store == nil || resolver == nil {
		return nil
	}
	// Preview without repairs: updateLockfile applies rec's.
	rec.EntryRepairs = nil
	ws := writers(rec, store, report, copts)
	preview := store.Preview()
	if err := updateLockfile(ctx, rec, ws, preview); err != nil {
		return err
	}
	preview.GC()
	unresolved := map[string]bool{}
	for _, e := range rec.Unresolved() {
		unresolved[strings.ToLower(e.NWO)+"@"+e.Ref] = true
	}
	all := func(parserlock.Pin, parserlock.Action) bool { return true }
	workflows := map[string][]string{}
	parents := map[string][]string{}
	for _, wp := range ws {
		for _, lp := range preview.Closure(workflowfile.KeyFromPath(wp.Path), all) {
			key := lp.Pin.String()
			if !lockfile.RecordsOwnSHA(lp.Pin, lp.Action.Ref) || unresolved[key] {
				continue
			}
			if !slices.Contains(workflows[key], wp.Path) {
				workflows[key] = append(workflows[key], wp.Path)
			}
			if lp.Parent != "" && !slices.Contains(parents[key], lp.Parent) {
				parents[key] = append(parents[key], lp.Parent)
			}
		}
	}
	for _, key := range slices.Sorted(maps.Keys(workflows)) {
		pin, _ := parserlock.ParsePin(key)
		tag, branch, err := resolver.DiscoverContaining(ctx, pin.Owner, pin.Repo, pin.Ref, pin.Ref)
		if err != nil {
			return fmt.Errorf("%s: %w", key, err)
		}
		ref := parserlock.BestRef(tag, branch)
		if ref == "" {
			rec.Entries = append(rec.Entries, Entry{
				NWO:        pin.NWO,
				Ref:        pin.Ref,
				SHA:        pin.Ref,
				Resolution: Unresolved,
				Issue:      "reverse-lookup",
				Reason:     "this commit has no exact tag and is not on any branch. Update the action that uses it to a version pinning a current upstream tag or branch, then rerun",
				Workflows:  workflows[key],
				RequiredBy: slices.Sorted(slices.Values(parents[key])),
			})
			continue
		}
		rec.EntryRepairs = append(rec.EntryRepairs, EntryRepair{Key: key, Ref: ref})
	}
	return nil
}
