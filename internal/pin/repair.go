package pin

import (
	"context"
	"fmt"
	"maps"
	"slices"

	parserlock "github.com/github/actions-lockfile/go/pkg/lockfile"
	"github.com/github/gh-actions-lock/internal/lockfile"
	"github.com/github/gh-actions-lock/internal/pipeline/checks"
	"github.com/github/gh-actions-lock/internal/workflowfile"
)

// EntryRepair re-records the ref of an existing lockfile entry.
type EntryRepair struct {
	Key string
	Ref string
}

// planEntryRepairs finds the tag at the commit, else a branch containing it,
// for each entry that records its own SHA as its ref and that the planned
// workflows reach only through another entry's `uses:`. A workflow's own pin
// of that shape is re-resolved by planWorkflow instead, and an entry nothing
// reaches is dropped by Save. A commit no tag or branch contains is
// Unresolved, so nothing is written.
func planEntryRepairs(ctx context.Context, rec *Record, report *checks.Report, opts PlanOptions) error {
	if opts.Store == nil || opts.Resolver == nil {
		return nil
	}
	all := func(parserlock.Pin) bool { return true }
	workflows := map[string][]string{}
	for _, wr := range report.Workflows {
		for _, lp := range opts.Store.Closure(workflowfile.KeyFromPath(wr.Path), all) {
			if lp.Parent == "" || !lockfile.RecordsOwnSHA(lp.Pin, lp.Action.Ref) {
				continue
			}
			key := lp.Pin.String()
			if !slices.Contains(workflows[key], wr.Path) {
				workflows[key] = append(workflows[key], wr.Path)
			}
		}
	}
	for _, key := range slices.Sorted(maps.Keys(workflows)) {
		pin, _ := parserlock.ParsePin(key)
		tag, branch, err := opts.Resolver.DiscoverContaining(ctx, pin.Owner, pin.Repo, pin.Ref, pin.Ref)
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
			})
			continue
		}
		rec.EntryRepairs = append(rec.EntryRepairs, EntryRepair{Key: key, Ref: ref})
	}
	return nil
}
