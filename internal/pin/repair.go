package pin

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"

	parserlock "github.com/github/actions-lockfile/go/pkg/lockfile"
	"github.com/github/gh-actions-lock/internal/ghapi"
	"github.com/github/gh-actions-lock/internal/lockfile"
	"github.com/github/gh-actions-lock/internal/pipeline/checks"
	"github.com/github/gh-actions-lock/internal/resolve"
	"github.com/github/gh-actions-lock/internal/workflowfile"
)

// EntryRepair re-records the ref of an existing lockfile entry.
type EntryRepair struct {
	Key string
	Ref string
	// Workflows are the written workflows that reach the entry; the repair
	// applies only while one of them is written.
	Workflows []string
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
	keys := slices.Sorted(maps.Keys(workflows))
	failed := proveIdentity(ctx, resolver, preview, keys)
	for _, key := range keys {
		pin, _ := parserlock.ParsePin(key)
		if reason, ok := failed[key]; ok {
			rec.Entries = append(rec.Entries, Entry{
				NWO:        pin.NWO,
				Ref:        pin.Ref,
				SHA:        pin.Ref,
				Resolution: Unresolved,
				Issue:      "identity",
				Reason:     reason + "; nothing is written. Run `gh actions-lock --relock` to re-resolve it",
				Workflows:  workflows[key],
				RequiredBy: slices.Sorted(slices.Values(parents[key])),
			})
			continue
		}
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
		rec.EntryRepairs = append(rec.EntryRepairs, EntryRepair{Key: key, Ref: ref, Workflows: workflows[key]})
	}
	return nil
}

// proveIdentity checks, in one batch, that each repair candidate's recorded
// repository still answers to its name with the recorded repo ID and has the
// recorded commit. No other check in the run need have reached a candidate
// (a composite reused from the lock carries no manifest), so a repair is
// admitted only on this proof. It returns why each failed candidate can't be
// repaired; a check that can't finish fails closed.
func proveIdentity(ctx context.Context, resolver *resolve.Resolver, preview *lockfile.State, keys []string) map[string]string {
	gh := resolver.GHClient()
	if len(keys) == 0 || gh == nil {
		return nil
	}
	checks := make([]ghapi.PinCheck, len(keys))
	recorded := make([]parserlock.Action, len(keys))
	for i, key := range keys {
		pin, _ := parserlock.ParsePin(key)
		recorded[i], _ = preview.Entry(key)
		_, sha, _ := strings.Cut(recorded[i].Commit, "-")
		checks[i] = ghapi.PinCheck{Owner: pin.Owner, Repo: pin.Repo, SHA: sha}
	}
	failed := map[string]string{}
	for i, st := range gh.CheckPins(ctx, checks) {
		nwo := checks[i].Owner + "/" + checks[i].Repo
		switch {
		case st.Err != nil:
			failed[keys[i]] = fmt.Sprintf("could not confirm %s is the repository locked: %s", nwo, ghapi.Brief(st.Err))
		case st.RepoMissing:
			failed[keys[i]] = fmt.Sprintf("%s is missing or not visible to this token", nwo)
		case recorded[i].RepoID != 0 && st.RepoID != 0 && st.RepoID != recorded[i].RepoID:
			failed[keys[i]] = fmt.Sprintf("%s now resolves to a different repository than the one locked (repo ID %d, locked %d)", nwo, st.RepoID, recorded[i].RepoID)
		case !st.CommitFound:
			failed[keys[i]] = fmt.Sprintf("commit %s is not in %s", parserlock.ShortSHA(checks[i].SHA), nwo)
		}
	}
	return failed
}
