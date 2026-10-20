package checks

import "strings"

// PrunesDependency reports whether planning drops f's dependency from the
// recorded inventory and re-resolves it, rather than writing the recorded
// entry back verbatim. Planning re-resolves only what this marks; parity
// consults it so a finding is excused only for an entry that is rewritten.
//
// acceptMoved also prunes ref-moved and unreachable-pin deps; relock prunes
// ref-moved deps only, so a benign branch/version advance can be re-pinned
// without accepting a possibly-tampered unreachable pin. A same-ID redirect
// is pruned so it re-resolves under the canonical name.
func PrunesDependency(f Finding, acceptMoved, relock bool) bool {
	if f.Dependency == nil {
		return false
	}
	switch {
	case f.Category == Stale,
		f.Category == UnreachablePin && acceptMoved,
		f.Category == RefMoved && (acceptMoved || relock),
		f.Category == RepoRenamed && f.Severity == SeverityWarning:
		return true
	}
	return false
}

// PruneKey is the identity planning prunes a recorded entry by.
func PruneKey(nwo, ref, sha string) string {
	return strings.ToLower(nwo + "@" + ref + ":" + sha)
}
