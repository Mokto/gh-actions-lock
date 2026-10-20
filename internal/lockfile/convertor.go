// Package lockfile manages CLI lockfile state: loading, saving, and
// converting the on-disk format.
package lockfile

import (
	"errors"
	"fmt"
	"strings"

	parserlock "github.com/github/actions-lockfile/go/pkg/lockfile"
	"github.com/github/gh-actions-lock/internal/dep"
	"github.com/github/gh-actions-lock/internal/ghapi"
)

// depToPin converts a dep.Dependency into a parserlock.Pin without any
// case-normalization — callers should rely on Pin.String / Pin.Canonical for
// the canonical form. Sub-action path is dropped: the lockfile pin grammar
// identifies a downloaded tarball at repo+sha granularity, and distinct
// subpaths in the same repo+ref collapse to one entry.
func depToPin(d dep.Dependency) (parserlock.Pin, error) {
	owner, repo := d.OwnerRepo()
	if owner == "" || repo == "" {
		return parserlock.Pin{}, fmt.Errorf("invalid NWO %q", d.NWO)
	}
	if d.Ref == "" {
		return parserlock.Pin{}, fmt.Errorf("missing ref for %s", d.NWO)
	}
	if d.SHA == "" {
		return parserlock.Pin{}, fmt.Errorf("missing SHA for %s@%s", d.NWO, d.Ref)
	}
	ref, err := parserlock.ParsePinRef(d.Ref)
	if err != nil {
		return parserlock.Pin{}, fmt.Errorf("%s: %w", d.NWO, err)
	}
	return parserlock.Pin{
		Owner: owner,
		Repo:  repo,
		Ref:   ref,
	}, nil
}

// validEntryRef returns the tag or branch an entry records for pin: a name
// the Actions backend accepts, never a commit SHA or "refs/" name, and equal
// to the key's ref unless the key is a commit.
func validEntryRef(pin parserlock.Pin, ref string) (parserlock.RefName, error) {
	if ref == "" {
		return parserlock.RefName{}, errors.New("missing ref")
	}
	n, err := parserlock.ValidateEntryRef(pin.Ref, ref)
	if err != nil {
		return parserlock.RefName{}, err
	}
	if !ghapi.AcceptedRefName(ref) {
		return parserlock.RefName{}, fmt.Errorf("ref %q is not a tag or branch name the Actions backend accepts", ref)
	}
	return n, nil
}

// pinToDep converts a parser Pin back into the internal dep.Dependency type.
// The lockfile-serialized Pin carries no Path, so the resulting Dependency
// has Path="" — sub-action paths are reconstructed at resolve time from
// workflow uses: strings.
func pinToDep(p parserlock.Pin) dep.Dependency {
	return dep.Dependency{
		NWO: p.NWO,
		Ref: p.Ref.String(),
	}
}

// RefIndexKey is parserlock.IndexKey for a dependency's string ref. Text that
// isn't a valid pin ref keeps its spelling, so it never matches a parsed pin.
func RefIndexKey(owner, repo, ref string) string {
	if r, err := parserlock.ParsePinRef(ref); err == nil {
		return parserlock.IndexKey(owner, repo, r)
	}
	return strings.ToLower(owner+"/"+repo) + "@" + ref
}
