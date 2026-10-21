package lockfile

import (
	"slices"

	parserlock "github.com/github/actions-lockfile/go/pkg/lockfile"
	"github.com/github/gh-actions-lock/internal/dep"
)

// RefIndex reaches recorded keys from a reference spelled by another source:
// a workflow, a manifest, or a resolution. It never picks one spelling over
// another; callers apply their own rule to the match (see RefMatch).
type RefIndex struct {
	// spelled holds every key the lockfile records anywhere: dependency
	// entries, workflow roots and uses edges, including dangling ones.
	spelled map[string]bool
	// entries maps dep.RefKey to the dependency entries in that class.
	entries map[dep.Key][]string
}

// RefMatch is what a lockfile records for one reference.
type RefMatch struct {
	// Exact is the stored key spelled exactly as the reference (owner/repo
	// folded), if the lockfile records that spelling anywhere; else "".
	Exact string
	// Candidates are the dependency entries with the reference's identity
	// (dep.RefKey), sorted. More than one means one full SHA is recorded
	// under several spellings; the parser makes their commits equal, but
	// their other fields can differ.
	Candidates []string
}

// NewRefIndex indexes file's recorded keys.
func NewRefIndex(file parserlock.File) RefIndex {
	x := RefIndex{spelled: map[string]bool{}, entries: map[dep.Key][]string{}}
	for key, action := range file.Dependencies {
		x.spelled[key] = true
		x.entries[dep.RefKeyOf(key)] = append(x.entries[dep.RefKeyOf(key)], key)
		for _, use := range action.Uses {
			x.spelled[use] = true
		}
	}
	for _, roots := range file.Workflows {
		for _, key := range roots {
			x.spelled[key] = true
		}
	}
	for _, keys := range x.entries {
		slices.Sort(keys)
	}
	return x
}

// Lookup returns what the lockfile records for owner/repo@ref as spelled.
func (x RefIndex) Lookup(nwo, ref string) RefMatch {
	var m RefMatch
	if owner, repo, ok := parserlock.SplitNWO(nwo); ok {
		if key := parserlock.IndexKey(owner, repo, ref); x.spelled[key] {
			m.Exact = key
		}
	}
	m.Candidates = slices.Clone(x.entries[dep.RefKey(nwo, ref)])
	return m
}
