package checks

import (
	"context"
	"strings"

	parserlock "github.com/github/actions-lockfile/go/pkg/lockfile"
	"github.com/github/gh-actions-lock/internal/dep"
	"github.com/github/gh-actions-lock/internal/lockfile"
	"github.com/github/gh-actions-lock/internal/workflowfile"
)

// RunChecks evaluates all enabled validators against the given parsed
// workflow and returns findings in catalog order. The lockfile snapshot
// scopes the structural checks; the resolver enables the resolver-bound
// checks (misleading-sha, ref-moved, forgery). When r is nil,
// resolver-bound checks are skipped silently.
//
// Returned findings have their primitive fields populated, plus
// ActionRef (for direct uses) and Dependency (for ref-tied entries).
// DocURL and ParentNWO are attached by the caller (diagnoseOneParsed)
// because they need lookup tables runChecks doesn't carry.
func RunChecks(ctx context.Context, pw ParsedWorkflow, lf parserlock.File, r CheckResolver) []Finding {
	wfEntry, tracked := lf.LookupWorkflow(workflowfile.KeyFromPath(pw.Path))
	refs := lockfile.NewRefIndex(lf)
	depPins, depIndex := parseWorkflowDeps(wfEntry, lf.Dependencies, refs)

	var out []Finding
	out = append(out, checkUntracked(pw, tracked)...)
	out = append(out, checkNotPinned(pw, depPins, depIndex)...)
	out = append(out, checkSHARecordedRef(pw, wfEntry, lf.Dependencies)...)
	out = append(out, checkRecordedClosure(pw, lf)...)
	out = append(out, checkShaAsRef(pw, depIndex)...)
	out = append(out, checkRefChanged(pw, depPins)...)
	out = append(out, checkStale(pw, depPins)...)

	if r != nil {
		out = append(out, checkMisleadingSha(ctx, pw, r)...)
		refMoved := checkRefMovedAndForgery(ctx, pw, depIndex, r)
		out = append(out, refMoved...)
	}
	return out
}

// lockedPin pairs a parsed Pin with the commit hash from the lockfile's
// Action.Commit field. Pin keys no longer embed the hash (v0.0.2 schema),
// so the SHA must be retrieved from the Action metadata.
type lockedPin struct {
	// Key is the pin's stored key in the lockfile.
	Key string
	parserlock.Pin
	Commit string // full "algo-hex" from Action.Commit
}

// SHA returns just the hex portion of the commit (strips "algo-" prefix).
func (lp lockedPin) SHA() string {
	if idx := strings.Index(lp.Commit, "-"); idx >= 0 {
		return lp.Commit[idx+1:]
	}
	return lp.Commit
}

// parseWorkflowDeps decodes a workflow's recorded roots into Pins plus an
// index keyed by reference identity (dep.RefKey), carrying each root's
// commit. A root keeps its stored key as Key; its Pin is the identity, a
// full SHA lowercased, and a root spelling a full SHA in another case than
// its only entry takes that entry's commit. Unparseable roots are dropped.
func parseWorkflowDeps(rawDeps []string, deps map[string]parserlock.Action, refs lockfile.RefIndex) ([]lockedPin, map[string]lockedPin) {
	pins := make([]lockedPin, 0, len(rawDeps))
	idx := make(map[string]lockedPin, len(rawDeps))
	for _, raw := range rawDeps {
		pin, ok := parserlock.ParsePin(raw)
		if !ok {
			continue
		}
		lp := lockedPin{Key: raw, Pin: pin}
		if parserlock.IsFullSha(pin.Ref) {
			lp.Pin.Ref = strings.ToLower(pin.Ref)
		}
		if action, found := deps[raw]; found {
			lp.Commit = action.Commit
		} else if c := refs.Lookup(pin.NWO, pin.Ref).Candidates; len(c) == 1 {
			lp.Commit = deps[c[0]].Commit
		}
		pins = append(pins, lp)
		idx[dep.RefKey(pin.NWO, pin.Ref)] = lp
	}
	return pins, idx
}

// newRefFinding builds a Finding with the common header fields populated
// from a uses: ref. Category/Severity can be empty when the caller fills
// them in based on a downstream branch (e.g. ref-moved vs forgery).
// Confidence is required at construction — see Finding.Confidence.
func newRefFinding(pw ParsedWorkflow, ref parserlock.ActionRef, cat Category, sev Severity, conf Confidence) Finding {
	refCopy := ref
	return Finding{
		WorkflowPath: pw.Path,
		Category:     cat,
		Severity:     sev,
		Confidence:   conf,
		ActionRef:    &refCopy,
	}
}

// synthDep builds a dep.Dependency from an ActionRef + locked SHA.
// Used by checks that surface lock-state but don't have a real
// Dependency pointer from the store.
func synthDep(ref parserlock.ActionRef, sha string) *dep.Dependency {
	return &dep.Dependency{
		NWO:  ref.Owner + "/" + ref.Repo,
		Path: ref.Path,
		Ref:  ref.Ref,
		SHA:  sha,
	}
}

// nwoLower returns lowercased "owner/repo". Trivial but used in several
// validators; centralized so the casing rule lives in one place.
func nwoLower(owner, repo string) string {
	return strings.ToLower(owner) + "/" + strings.ToLower(repo)
}

// formatUseName renders "owner/repo" or "owner/repo/path" for human
// messages. Index/lookup keys are always at owner/repo granularity.
func formatUseName(owner, repo, path string) string {
	s := nwoLower(owner, repo)
	if path != "" {
		s += "/" + path
	}
	return s
}

// rootKey is ref's identity, the key of the workflow-local root index
// (see parseWorkflowDeps).
func rootKey(ref parserlock.ActionRef) string {
	return dep.RefKey(ref.Owner+"/"+ref.Repo, ref.Ref)
}
