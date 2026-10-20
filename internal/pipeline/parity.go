package pipeline

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"strings"

	parserlock "github.com/github/actions-lockfile/go/pkg/lockfile"
	"github.com/github/gh-actions-lock/internal/dep"
	"github.com/github/gh-actions-lock/internal/ghapi"
	"github.com/github/gh-actions-lock/internal/lockfile"
	"github.com/github/gh-actions-lock/internal/pipeline/checks"
	"github.com/github/gh-actions-lock/internal/resolve"
	"github.com/github/gh-actions-lock/internal/workflowfile"
)

// checkParity re-checks every locked pin in each workflow's recorded closure
// the way the runner does at job start: same repository identity, commit
// still present, and the lock entry's ref (a tag, else a branch) still
// vouching for it: the commit must be in the ref's history, since a
// fork-network commit resolves under the upstream name, so existence alone
// proves nothing (#174). A ref that has moved ahead is fine: the lock is
// sticky until --relock. Tags can move too, so with verify an exact-version
// tag that moved ahead only warns. A ref that is gone, or a history check
// that can't finish, blocks.
//
// A pinned commit fixes the dependencies it declares, so the flattened
// closure in the lockfile is checked in one batch with no recursion.
// Pins resolved in this run (refs absent from recordedKeys, and their
// transitive closure from this run's resolve) join the same batch. Any
// blocking finding keeps its workflow out of the write.
//
// Identity follows the runner: only the repo ID counts. A rename or
// transfer redirect to the same repo ID is a warning, also inside a
// remote composite; a different ID or a name that doesn't resolve blocks. Fresh pins fail closed: an
// inconclusive check keeps them out of the write.
//
// Under the Dependabot proxy any inconclusive check is an error: the run
// aborts so Dependabot retries rather than shipping an unverified lock.
func checkParity(ctx context.Context, r *resolve.Resolver, parsed []checks.ParsedWorkflow, store *lockfile.State, report *checks.Report, recordedKeys map[string]bool, resolved []dep.Dependency, parents map[string][]string, mode parityMode) error {
	gh := r.GHClient()
	if gh == nil || store == nil {
		return nil
	}
	reportIdx := make(map[string]int, len(report.Workflows))
	for i, wr := range report.Workflows {
		reportIdx[wr.Path] = i
	}
	var pins []ghapi.PinCheck
	pinIdx := map[string]int{}
	owners := map[int][]parityOwner{}
	owned := map[[2]int]int{}
	add := func(o parityOwner) {
		if o.fresh && o.lp.Action.RepoID == 0 {
			o.lp.Action.RepoID = store.RecordedRepoID(o.lp.Pin.Owner, o.lp.Pin.Repo)
			if o.lp.Action.RepoID == 0 {
				o.lp.Action.RepoID = store.RecordedRepoID(o.ref.Owner, o.ref.Repo)
			}
		}
		// Reference identity: owner/repo and a full SHA fold, other refs
		// are case-sensitive, as the Actions backend keys them.
		// A recorded and a fresh owner of one pin can have different targets
		// (the recorded ref, and what this run will write), so the target is
		// part of the key. The lockfile parser has already rejected an entry
		// ref the Actions backend resolver would not accept.
		// An entry that records its own SHA has no ref to look up: only the
		// repository and commit are checked here, and the entry is reported
		// or re-recorded elsewhere (see lockfile.RecordsOwnSHA).
		head := o.lp.Action.Ref
		if lockfile.RecordsOwnSHA(o.lp.Pin, head) {
			head = ""
		}
		key := dep.RefKey(o.lp.Pin.Owner+"/"+o.lp.Pin.Repo, o.lp.Pin.Ref) + ":" + strings.ToLower(lockedSHA(o.lp.Action.Commit)) + ":" + head
		i, seen := pinIdx[key]
		if !seen {
			i = len(pins)
			pinIdx[key] = i
			pins = append(pins, ghapi.PinCheck{Owner: o.lp.Pin.Owner, Repo: o.lp.Pin.Repo, SHA: lockedSHA(o.lp.Action.Commit), Head: head})
		}
		if j, dup := owned[[2]int{o.report, i}]; dup {
			owners[i][j].fresh = owners[i][j].fresh || o.fresh
			return
		}
		owned[[2]int{o.report, i}] = len(owners[i])
		owners[i] = append(owners[i], o)
	}
	fresh := freshClosure(resolved, parents)
	for _, pw := range parsed {
		ri, ok := reportIdx[pw.Path]
		if !ok || isBlocked(pw) {
			continue
		}
		recorded, _ := pw.PartitionRefs()
		// rootRef is the workflow ref that still uses a recorded root, by
		// the same rule as the stale check (lockfile.RootUse): the use
		// naming its key, else one naming its commit.
		use := lockfile.RootUse(recorded)
		rootRef := func(p parserlock.Pin, a parserlock.Action) (parserlock.ActionRef, bool) {
			return use(p, a.Commit)
		}
		live := func(p parserlock.Pin, a parserlock.Action) bool {
			_, ok := rootRef(p, a)
			return ok
		}
		// Planning writes a recorded entry back verbatim unless one of
		// these findings prunes it.
		effects := map[string]checks.PruneEffect{}
		for _, f := range report.Workflows[ri].Findings {
			if checks.PrunesDependency(f, mode.acceptMoved, mode.relock) {
				k := checks.PruneKey(f.Dependency.NWO, f.Dependency.Ref, f.Dependency.SHA)
				// A dropped use leaves the key behind: it wins.
				if e := checks.PruneEffectOf(f, mode.acceptMoved, mode.relock); effects[k] != checks.Dropped {
					effects[k] = e
				}
			}
		}
		closure := store.Closure(workflowfile.KeyFromPath(pw.Path), live)
		edges := compositeEdges(closure)
		for _, lp := range closure {
			ref, _ := rootRef(lp.Pin, lp.Action)
			if lp.Parent != "" {
				ref = parserlock.ActionRef{Owner: lp.Pin.Owner, Repo: lp.Pin.Repo, Ref: lp.Pin.Ref}
			}
			k := lp.Pin.String()
			add(parityOwner{report: ri, pw: pw, lp: lp, ref: ref,
				away:   plannedAway(lp, edges[k], effects[checks.PruneKey(lp.Pin.NWO, lp.Pin.Ref, lockedSHA(lp.Action.Commit))]),
				heldBy: heldBy(lp, edges[k]),
			})
		}
		if pw.Resolved {
			continue
		}
		for _, o := range fresh(pw, recordedKeys) {
			o.report = ri
			add(o)
		}
	}
	if len(pins) == 0 {
		return nil
	}

	states := gh.CheckPins(ctx, pins)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	// tagMoved marks an exact-version ref that moved ahead of its commit.
	// The Actions backend accepts it, so --verify only warns.
	tagMoved := make([]bool, len(states))
	for i := range states {
		st, pc := &states[i], pins[i]
		if pc.Head == "" || st.Err != nil || !st.CommitFound || st.HeadOID == "" || strings.EqualFold(st.HeadOID, pc.SHA) {
			continue
		}
		switch ancestry, detail, cause := r.CheckAncestryCause(ctx, pc.Owner, pc.Repo, pc.SHA, st.HeadOID); ancestry {
		case resolve.AncestryNotAncestor:
			st.CommitFound = false
		case resolve.AncestryUnknown:
			brief := fmt.Sprintf("could not check %s history", pc.Head)
			if cause != nil {
				brief += ": " + ghapi.Brief(cause)
			}
			st.Err = &ghapi.BriefError{Msg: brief, Err: fmt.Errorf("checking %s history: %s", pc.Head, detail)}
		default:
			sv, ok := parserlock.ParseSemVer(pc.Head)
			tagMoved[i] = ok && sv.IsFull()
		}
	}
	if ghapi.DependabotProxy() {
		for i, st := range states {
			if st.Err != nil {
				msg := fmt.Sprintf("could not verify locked %s/%s@%s", pins[i].Owner, pins[i].Repo, parserlock.ShortSHA(pins[i].SHA))
				return &ghapi.BriefError{Msg: msg + ": " + briefCause(st.Err), Err: fmt.Errorf("%s: %s", msg, verbose(st.Err))}
			}
		}
	}
	verified := map[string]bool{}
	defer func() {
		if report.Verified == nil {
			report.Verified = map[string]bool{}
		}
		for k := range verified {
			report.Verified[k] = true
		}
	}()
	for i, st := range states {
		for _, o := range owners[i] {
			f, ok := parityFinding(o.pw, o.ref, o.lp, pins[i], st)
			if o.fresh {
				f.FreshRef = true
			} else {
				f.RecordedRef = o.lp.Action.Ref
			}
			if !ok && mode.verify && tagMoved[i] {
				f, ok = movedTagFinding(o.pw, o.ref, o.lp, pins[i], st), true
			}
			wr := &report.Workflows[o.report]
			if !ok {
				if o.fresh {
					verified[checks.VerifiedKey(lockfile.EntryKey(o.lp.Pin), pins[i].SHA)] = true
				}
				continue
			}
			verdict, blocking := disposeParity(mode, &f, paritySubject{pin: o.lp.Pin, fresh: o.fresh, ownSHA: !o.fresh && lockfile.RecordsOwnSHA(o.lp.Pin, o.lp.Action.Ref), away: o.away, heldBy: o.heldBy, head: pins[i].Head, inconclusive: st.Err != nil})
			if verdict != verdictKeep {
				wr.Deferred = append(wr.Deferred, checks.Deferred{
					Key: lockfile.EntryKey(o.lp.Pin), Ref: o.lp.Action.Ref, Commit: pins[i].SHA, Finding: blocking,
				})
			}
			if verdict == verdictExcuse {
				continue
			}
			if o.fresh && f.Severity != checks.SeverityError {
				verified[checks.VerifiedKey(lockfile.EntryKey(o.lp.Pin), pins[i].SHA)] = true
			}
			// Dependabot reads back only actions.lock, so a rewritten `uses:`
			// would never ship; keep the pin under the name the workflow uses.
			if f.Category == checks.RepoRenamed && f.Severity == checks.SeverityWarning && o.lp.Parent == "" && !ghapi.DependabotProxy() {
				r.Redirect(o.lp.Pin.Owner, o.lp.Pin.Repo, o.ref, st.NameWithOwner)
			}
			if f.Severity == checks.SeverityError {
				wr.Findings = dropValid(wr.Findings)
				wr.SkipCommit = true
			}
			wr.Findings = append(wr.Findings, f)
		}
	}
	return nil
}

type parityOwner struct {
	report int
	pw     checks.ParsedWorkflow
	lp     lockfile.LockedPin
	ref    parserlock.ActionRef
	fresh  bool
	// away marks a recorded entry planning replaces or removes: see
	// plannedAway.
	away bool
	// heldBy is the composite that keeps this entry as recorded when
	// --accept-moved re-resolves the workflow's use: see heldBy.
	heldBy string
}

// compositeEdges maps each pin a composite in the recorded closure lists in
// its `uses:` to those composites. Keys are Pin.String.
func compositeEdges(closure []lockfile.LockedPin) map[string][]lockfile.LockedPin {
	out := map[string][]lockfile.LockedPin{}
	for _, lp := range closure {
		for _, child := range lp.Action.Uses {
			if p, ok := parserlock.ParsePin(child); ok {
				out[p.String()] = append(out[p.String()], lp)
			}
		}
	}
	return out
}

// plannedAway reports whether planning provably replaces or removes a
// recorded entry, given what a finding makes it do with the workflow's use
// (checks.PruneEffect). Planning prunes only the workflow's own pins, and
// writing a workflow keeps every recorded edge, so an entry a composite
// lists survives unless the workflow's use overwrites that very key: a
// mutable ref re-resolved in place. A full-SHA use may be re-keyed by
// narrowing, and a dropped use leaves the key behind. composites are the
// recorded composites that list lp's key.
func plannedAway(lp lockfile.LockedPin, composites []lockfile.LockedPin, effect checks.PruneEffect) bool {
	if lp.Parent != "" {
		return false
	}
	switch effect {
	case checks.Reresolved:
		return len(composites) == 0 || !parserlock.IsFullSha(lp.Pin.Ref)
	case checks.Dropped:
		return len(composites) == 0
	}
	return false
}

// heldBy is the composite (NWO@ref) that keeps a recorded entry as recorded
// even when --accept-moved re-resolves the workflow's use of it, or "" when
// that repairs it. Repair advice and the override both ask this.
// --accept-moved rebuilds the workflow's graph, re-resolving a mutable ref
// wherever it sits, so only a full SHA a composite pins is held.
func heldBy(lp lockfile.LockedPin, composites []lockfile.LockedPin) string {
	switch {
	case !parserlock.IsFullSha(lp.Pin.Ref), plannedAway(lp, composites, checks.Reresolved):
		return ""
	case lp.Parent != "":
		return lockParent(lp)
	}
	return composites[0].Pin.NWO + "@" + composites[0].Pin.Ref
}

// freshClosure returns a function yielding the closure of a workflow's
// unrecorded refs from this run's single resolve, so no further requests
// are made per workflow. A workflow's refs reach what the run resolved by
// reference identity: the run records each full SHA under one spelling.
func freshClosure(resolved []dep.Dependency, parents map[string][]string) func(checks.ParsedWorkflow, map[string]bool) []parityOwner {
	children := map[string][]dep.Dependency{}
	byKey := map[string][]dep.Dependency{}
	for _, d := range resolved {
		if d.SHA == "" {
			continue
		}
		keys := []string{d.Key()}
		for _, ref := range d.OriginalRefs {
			keys = append(keys, ref.NWO()+"@"+ref.Ref)
		}
		for _, k := range keys {
			byKey[dep.RefKeyOf(k)] = append(byKey[dep.RefKeyOf(k)], d)
			for _, p := range parents[k] {
				children[dep.RefKeyOf(p)] = append(children[dep.RefKeyOf(p)], d)
			}
		}
	}
	return func(pw checks.ParsedWorkflow, recordedKeys map[string]bool) []parityOwner {
		var out []parityOwner
		seen := map[string]bool{}
		var walk func(d dep.Dependency, ref parserlock.ActionRef, parent string)
		walk = func(d dep.Dependency, ref parserlock.ActionRef, parent string) {
			id := dep.RefKeyOf(d.Key()) + "/" + d.Path
			if seen[id] {
				return
			}
			seen[id] = true
			owner, repo := d.OwnerRepo()
			o := parityOwner{pw: pw, fresh: true, ref: ref, lp: lockfile.LockedPin{
				Pin:    parserlock.Pin{NWO: d.NWO, Owner: owner, Repo: repo, Ref: d.Ref},
				Action: parserlock.Action{Ref: freshEntryRef(d), Commit: d.SHA},
				Parent: parent,
			}}
			out = append(out, o)
			for _, c := range children[dep.RefKeyOf(d.Key())] {
				co, cr := c.OwnerRepo()
				walk(c, parserlock.ActionRef{Owner: co, Repo: cr, Path: c.Path, Ref: c.Ref}, d.Key())
			}
		}
		for _, ref := range pw.Refs {
			key := dep.RefKey(ref.Owner+"/"+ref.Repo, ref.Ref)
			if recordedKeys[key] {
				continue
			}
			for _, d := range byKey[key] {
				walk(d, ref, "")
			}
		}
		return out
	}
}

// briefCause is err for a terminal line: see ghapi.Brief.
func briefCause(err error) string {
	var be *ghapi.BriefError
	if errors.As(err, &be) {
		return be.Msg
	}
	return ghapi.Brief(err)
}

// verbose is err with any underlying detail a BriefError shortened.
func verbose(err error) string {
	var be *ghapi.BriefError
	if errors.As(err, &be) {
		return be.Verbose()
	}
	return err.Error()
}

// movedTagFinding warns that an exact-version tag moved ahead of its locked
// commit. Tags can move and the Actions backend accepts the pin, so it never
// blocks. The advice must follow the tag: --relock re-resolves a tag
// `uses:`, but leaves a full-SHA one, and only its parent changes a
// composite's pin.
func movedTagFinding(pw checks.ParsedWorkflow, ref parserlock.ActionRef, lp lockfile.LockedPin, pc ghapi.PinCheck, st ghapi.PinState) checks.Finding {
	nwo := pc.Owner + "/" + pc.Repo
	f := checks.Finding{
		WorkflowPath: pw.Path,
		ActionRef:    &ref,
		Dependency:   &dep.Dependency{NWO: nwo, Path: ref.Path, Ref: lp.Pin.Ref, SHA: pc.SHA},
		Category:     checks.RefMoved,
		Severity:     checks.SeverityWarning,
		Confidence:   checks.ConfidenceHigh,
		ObservedSHA:  st.HeadOID,
		TagMoved:     true,
		ParentNWO:    lockParent(lp),
		Detail:       fmt.Sprintf("tag %s now points at %s; locked commit %s is still in its history", pc.Head, parserlock.ShortSHA(st.HeadOID), parserlock.ShortSHA(pc.SHA)),
		Remediation:  "the Actions backend accepts this pin; run `gh actions-lock --relock` to lock the tag's current commit",
	}
	if f.ParentNWO != "" {
		f.Detail += fmt.Sprintf(" (used by %s)", f.ParentNWO)
	}
	if parserlock.IsFullSha(lp.Pin.Ref) {
		uses := nwo
		if ref.Path != "" {
			uses += "/" + ref.Path
		}
		f.Remediation = fmt.Sprintf("the Actions backend accepts this pin; to follow the tag, change `uses:` to %s@%s and run `gh actions-lock`", uses, pc.Head)
		if f.ParentNWO != "" {
			f.Remediation = fmt.Sprintf("the Actions backend accepts this pin; only upgrading %s follows the tag", f.ParentNWO)
		}
	}
	return f
}

// lockParent is the NWO@ref of the composite that pins lp, or "" for a
// workflow's own pin.
func lockParent(lp lockfile.LockedPin) string {
	if parent, ok := parserlock.ParsePin(lp.Parent); ok {
		return parent.NWO + "@" + parent.Ref
	}
	return ""
}

// freshEntryRef is the ref a fresh pin is checked against. A full-SHA key
// has none yet: planning picks its tag or branch by reverse lookup, which
// only accepts a tag at the commit or a branch containing it.
func freshEntryRef(d dep.Dependency) string {
	if parserlock.IsFullSha(d.Ref) {
		return ""
	}
	return d.Ref
}

func parityFinding(pw checks.ParsedWorkflow, ref parserlock.ActionRef, lp lockfile.LockedPin, pc ghapi.PinCheck, st ghapi.PinState) (checks.Finding, bool) {
	nwo := pc.Owner + "/" + pc.Repo
	f := checks.Finding{
		WorkflowPath: pw.Path,
		ActionRef:    &ref,
		Dependency:   &dep.Dependency{NWO: nwo, Path: ref.Path, Ref: lp.Pin.Ref, SHA: pc.SHA},
		Severity:     checks.SeverityError,
		Confidence:   checks.ConfidenceHigh,
	}
	f.ParentNWO = lockParent(lp)
	via := ""
	if f.ParentNWO != "" {
		via = fmt.Sprintf(" (used by %s)", f.ParentNWO)
	}
	short := parserlock.ShortSHA(pc.SHA)
	switch {
	case st.Err != nil:
		f.Category = checks.ReachabilityUnknown
		f.Severity = checks.SeverityWarning
		f.Confidence = checks.ConfidenceLow
		f.Detail = fmt.Sprintf("could not verify locked %s@%s: %s", nwo, short, verbose(st.Err))
		f.TermDetail = fmt.Sprintf("could not verify locked %s@%s: %s", nwo, short, briefCause(st.Err))
		var be *ghapi.BriefError
		if errors.As(st.Err, &be) {
			f.TermDetail = be.Msg
		}
		f.Remediation = "retry; the Actions backend will re-check this pin when the job starts"
	case st.RepoMissing:
		f.Category = checks.RepoUnavailable
		f.Detail = fmt.Sprintf("%s is missing or not visible to this token%s", nwo, via)
		moved := "if the repository moved or was deleted, update `uses:` and run `gh actions-lock`"
		access := fmt.Sprintf("if it's private or internal, use a token that can read it or authorize SSO for %s", pc.Owner)
		f.Remediation = moved + "; " + access
		if st.ViaFallback {
			f.Remediation = access + "; " + moved
		}
	case idChanged(lp.Action.RepoID, st.RepoID):
		f.Category = checks.RepoReplaced
		f.Detail = fmt.Sprintf("%s%s now resolves to a different repository than the one locked (repo ID %d, locked %d)", nwo, via, st.RepoID, lp.Action.RepoID)
		f.Remediation = "do not trust it: the locked repository was deleted and its name taken over, possibly by an attacker. Point `uses:` at a repository you have verified"
		if f.ParentNWO != "" {
			f.Remediation = fmt.Sprintf("do not trust it: the locked repository was deleted and its name taken over, possibly by an attacker. Stop using %s or upgrade it to a version that doesn't use %s", f.ParentNWO, nwo)
		}
	case !st.CommitFound:
		f.Category = checks.UnreachablePin
		f.Detail = checks.NotInHistoryDetail(pc.SHA, nwo, cmp.Or(pc.Head, ref.Ref), st.HeadOID) + via
		f.Remediation = checks.UnreachableRemediation(f.ParentNWO, nwo)
	case pc.Head != "" && st.HeadOID == "":
		f.Category = checks.UnreachablePin
		f.Detail = fmt.Sprintf("ref %s is not a tag or branch of %s; the Actions backend will reject this pin%s", pc.Head, nwo, via)
		f.RefGone = true
		// A full-SHA `uses:` names no ref to repoint; the lock entry is wrong.
		f.Remediation = checks.UnreachableRemediation(f.ParentNWO, nwo)
		if !parserlock.IsFullSha(lp.Pin.Ref) {
			f.Remediation = "point `uses:` at a tag or branch that exists, then run `gh actions-lock`"
			f.RefMissing = true
		}
		if ghapi.DescribeName(pc.Head) {
			// Refused by name, not looked up: it may well exist.
			f.Detail = fmt.Sprintf("ref %s looks like `git describe` output, which the Actions backend refuses as a tag or branch name; it will reject this pin%s", pc.Head, via)
			f.RefGone, f.RefRefused = false, true
			if f.RefMissing {
				f.Remediation = "point `uses:` at a tag or branch whose name doesn't end in -g<hex>, then run `gh actions-lock`"
			}
		}
	case st.NameWithOwner != "" && !strings.EqualFold(st.NameWithOwner, nwo):
		f.Category = checks.RepoRenamed
		f.Severity = checks.SeverityWarning
		f.Detail = fmt.Sprintf("%s was renamed or transferred to %s%s; the runner follows the redirect while the repo ID matches", nwo, st.NameWithOwner, via)
		uses := st.NameWithOwner
		if ref.Path != "" {
			uses += "/" + ref.Path
		}
		f.Remediation = fmt.Sprintf("run `gh actions-lock` to rewrite it as `uses: %s@%s`", uses, ref.Ref)
		if f.ParentNWO != "" {
			f.Remediation = fmt.Sprintf("upgrade %s to a version that uses %s", f.ParentNWO, st.NameWithOwner)
		}
	default:
		return checks.Finding{}, false
	}
	if !f.RefMissing {
		f.DocURL = DocURLFor(f.Category)
	}
	return f, true
}

func idChanged(locked, live int64) bool {
	return locked != 0 && live != 0 && locked != live
}

func lockedSHA(commit string) string {
	if i := strings.IndexByte(commit, '-'); i >= 0 {
		return commit[i+1:]
	}
	return commit
}

func isBlocked(pw checks.ParsedWorkflow) bool {
	return pw.LoadErr != nil || pw.DepsErr != nil || len(pw.LocalPaths) > 0 ||
		len(pw.SelfRepositoryRefErrs) > 0 || len(pw.SelfRepositoryResolutionErrs) > 0
}

func dropValid(ff []checks.Finding) []checks.Finding {
	out := ff[:0]
	for _, f := range ff {
		if f.Category != checks.Valid {
			out = append(out, f)
		}
	}
	return out
}
