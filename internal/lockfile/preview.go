package lockfile

import (
	"context"
	"errors"
	"maps"
	"slices"

	parserlock "github.com/github/actions-lockfile/go/pkg/lockfile"
)

// errPreview reports a write attempted on a preview.
var errPreview = errors.New("lockfile preview is never saved")

// Preview returns an in-memory copy of s for computing what a write would
// leave on disk. It never reaches the network or the file: an owner/repo
// ID missing from s's cache gets a placeholder, since IDs decide no key,
// commit or edge, and Save refuses. Writes to the preview leave s alone.
func (s *State) Preview() *State {
	s.mu.Lock()
	defer s.mu.Unlock()
	file := s.file
	file.Dependencies = make(map[string]parserlock.Action, len(s.file.Dependencies))
	for k, a := range s.file.Dependencies {
		a.Uses = slices.Clone(a.Uses)
		file.Dependencies[k] = a
	}
	file.Workflows = make(map[string][]string, len(s.file.Workflows))
	for k, w := range s.file.Workflows {
		file.Workflows[k] = slices.Clone(w)
	}
	return &State{
		file:            file,
		originalVersion: s.originalVersion,
		meta:            previewIDs{},
		idCache:         maps.Clone(s.idCache),
		hostname:        s.hostname,
		preview:         true,
	}
}

// GC drops entries no workflow reaches, as Save does.
func (s *State) GC() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneOrphans()
}

// Entry returns the action recorded under an on-disk key.
func (s *State) Entry(key string) (parserlock.Action, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.file.Dependencies[key]
	return a, ok
}

// EntryKey is a pin's on-disk key.
func EntryKey(p parserlock.Pin) string {
	return p.Canonical().String()
}

// previewIDs stands in for the metadata resolver in a preview.
type previewIDs struct{}

func (previewIDs) RepoIDs(context.Context, string, string, string) (int64, int64, error) {
	return 1, 1, nil
}
