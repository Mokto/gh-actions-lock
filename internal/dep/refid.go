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

// Ref is a reference's identity: a full SHA in one spelling, any other ref
// exactly as written. Built only by NewRef, so two Refs are equal exactly
// when they name the same reference, and the raw spelling can't be
// compared by accident.
type Ref struct{ ref string }

// NewRef is the identity of ref.
func NewRef(ref string) Ref {
	if parserlock.IsFullSha(ref) {
		ref = strings.ToLower(ref)
	}
	return Ref{ref}
}

// String is the ref in its normalized spelling.
func (r Ref) String() string { return r.ref }

// NewCommit is the identity of a commit ID: object IDs are hex, so any
// length folds case.
func NewCommit(sha string) Ref { return Ref{strings.ToLower(sha)} }

// SameRef reports whether a and b name the same reference.
func SameRef(a, b string) bool { return NewRef(a) == NewRef(b) }

// Key is the E2 identity of owner/repo@ref: owner/repo folded, the ref a
// Ref. It's comparable and serves as a map key.
type Key struct {
	nwo string
	ref Ref
}

// RefKey is the E2 identity of owner/repo@ref.
func RefKey(nwo, ref string) Key {
	return Key{strings.ToLower(nwo), NewRef(ref)}
}

// RefKeyOf is RefKey for an owner/repo@ref string. Refs can contain "@";
// owner/repo can't.
func RefKeyOf(key string) Key {
	nwo, ref, _ := strings.Cut(key, "@")
	return RefKey(nwo, ref)
}

// String is owner/repo@ref in normalized spelling, for display and for
// composing a larger identity.
func (k Key) String() string { return k.nwo + "@" + k.ref.ref }

// Compare orders keys by their normalized spelling.
func (k Key) Compare(o Key) int { return strings.Compare(k.String(), o.String()) }

// Spelling is `uses:` text exactly as a file spells it. Workflow rewrites
// replace bytes, so they are keyed by Spelling, never by identity, and
// nothing compares two Spellings to decide what a reference is.
type Spelling string

// NormalizeRef is ref with a full SHA lowercased, the form references take
// once parsed: every comparison downstream sees one spelling per commit.
func NormalizeRef(ref parserlock.ActionRef) parserlock.ActionRef {
	if parserlock.IsFullSha(ref.Ref) {
		ref.Ref = strings.ToLower(ref.Ref)
	}
	return ref
}
