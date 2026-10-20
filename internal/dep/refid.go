package dep

import (
	"strings"

	parserlock "github.com/github/actions-lockfile/go/pkg/lockfile"
)

// Reference identity (E2) is the only relation for comparing references that
// come from different sources: workflow text, manifest text, recorded
// lockfile keys, resolution output. owner/repo are case-insensitive; a full
// SHA names a commit, which git compares case-insensitively; a tag or branch
// name is exact.
//
// Lockfile keys keep the spelling they were written in, so a recorded entry
// is reached from another source's spelling through lockfile.RefIndex, which
// returns the stored key, never by building the key here.

// RefKey is the E2 identity of owner/repo@ref.
func RefKey(nwo, ref string) string {
	if parserlock.IsFullSha(ref) {
		ref = strings.ToLower(ref)
	}
	return strings.ToLower(nwo) + "@" + ref
}

// RefKeyOf is RefKey for an owner/repo@ref string. Refs can contain "@";
// owner/repo can't.
func RefKeyOf(key string) string {
	nwo, ref, _ := strings.Cut(key, "@")
	return RefKey(nwo, ref)
}
