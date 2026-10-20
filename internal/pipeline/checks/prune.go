package checks

import (
	"strings"

	parserlock "github.com/github/actions-lockfile/go/pkg/lockfile"
)

// PruneEffect is what planning does with a recorded entry a finding prunes
// from the workflow's inventory. Either way the recorded entry itself stays
// in the lock while another entry's recorded `uses:` lists it: writing a
// workflow merges, never drops, recorded edges.
type PruneEffect int

const (
	// NotPruned: planning writes the entry back verbatim.
	NotPruned PruneEffect = iota
	// Reresolved: planning re-resolves the workflow's use under the same
	// key, overwriting the entry, unless narrowing re-keys a full-SHA use.
	Reresolved
	// Dropped: the workflow's use leaves the entry's key, because the
	// workflow no longer uses it or it re-resolves under a new name.
	Dropped
)

// PruneEffectOf reports what planning does with f's dependency.
//
// acceptMoved also prunes ref-moved and unreachable-pin deps; relock prunes
// ref-moved deps only, so a benign branch/version advance can be re-pinned
// without accepting a possibly-tampered unreachable pin. A same-ID redirect
// is pruned so it re-resolves under the canonical name.
func PruneEffectOf(f Finding, acceptMoved, relock bool) PruneEffect {
	if f.Dependency == nil {
		return NotPruned
	}
	switch {
	case f.Category == Stale,
		f.Category == RepoRenamed && f.Severity == SeverityWarning:
		return Dropped
	case f.Reresolve,
		f.Category == UnreachablePin && acceptMoved,
		f.Category == RefMoved && (acceptMoved || relock):
		return Reresolved
	}
	return NotPruned
}

// PrunesDependency reports whether planning drops f's dependency from the
// recorded inventory rather than writing the entry back verbatim.
func PrunesDependency(f Finding, acceptMoved, relock bool) bool {
	return PruneEffectOf(f, acceptMoved, relock) != NotPruned
}

// PruneKey is the identity planning prunes a recorded entry by: the
// lockfile's canonical pin key, which folds owner/repo but keeps a ref's
// case, since refs are case-sensitive, and the commit, folded. A full-SHA
// ref names a commit, so it folds too.
func PruneKey(nwo, ref, sha string) string {
	if parserlock.IsFullSha(ref) {
		ref = strings.ToLower(ref)
	}
	key := nwo + "@" + ref
	if p, ok := parserlock.ParsePin(key); ok {
		key = p.String()
	}
	return key + ":" + strings.ToLower(sha)
}
