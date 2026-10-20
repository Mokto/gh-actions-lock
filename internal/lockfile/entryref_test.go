package lockfile

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	parserlock "github.com/github/actions-lockfile/go/pkg/lockfile"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const entryRefSHA = "9ddf35b71a482be7d8922b28e8d00df16b77e315"

func TestValidEntryRef(t *testing.T) {
	tests := []struct {
		name string
		key  string
		ref  string
		want bool
	}{
		{"tag key records its tag", "v4.2.1", "v4.2.1", true},
		{"branch key records its branch", "main", "main", true},
		{"full-SHA key records a tag", entryRefSHA, "v6.1.0", true},
		{"full-SHA key records a branch", entryRefSHA, "main", true},
		{"empty ref", entryRefSHA, "", false},
		{"full-SHA key records the SHA", entryRefSHA, entryRefSHA, false},
		{"non-SHA key records a SHA", "v4", entryRefSHA, false},
		{"non-SHA key records another ref", "v4", "main", false},
		{"ref case differs from key", "Main", "main", false},
		{"qualified tag", entryRefSHA, "refs/tags/v6.1.0", false},
		{"qualified branch", entryRefSHA, "refs/heads/main", false},
		{"pull request ref", entryRefSHA, "refs/pull/1/head", false},
		{"qualified ref on a matching key", "refs/heads/main", "refs/heads/main", false},
		// Revision syntax would be evaluated, not looked up literally.
		{"parent of a branch", entryRefSHA, "main~1", false},
		{"parent of a tag", entryRefSHA, "v1^", false},
		{"reflog entry", entryRefSHA, "v1@{1}", false},
		{"range", entryRefSHA, "a..b", false},
		{"path in a ref", entryRefSHA, "refs:x", false},
		{"revision syntax on a matching key", "main~1", "main~1", false},
		{"describe name", entryRefSHA, "v6.1.0-1-g373d742f768e", false},
		{"describe name on a matching key", "v1-g1234", "v1-g1234", false},
		{"three hex digits is a plain name", entryRefSHA, "v1-gabc", true},
		{"uppercase G is a plain name", entryRefSHA, "v1-G1234", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pin := parserlock.Pin{Owner: "actions", Repo: "checkout", Ref: tt.key}
			assert.Equal(t, tt.want, ValidEntryRef(pin, tt.ref))
		})
	}
}

// TestLoadState_InvalidEntryRefIsCorrupt: an entry whose ref the Actions
// backend resolver would reject is a parse error, never a finding a fix run
// could launder.
func TestLoadState_InvalidEntryRefIsCorrupt(t *testing.T) {
	tests := []struct {
		name string
		key  string
		ref  string
	}{
		{"full-SHA key records another SHA", entryRefSHA, "cccccccccccccccccccccccccccccccccccccccc"},
		{"qualified tag", entryRefSHA, "refs/tags/v6.1.0"},
		{"qualified pull request ref", entryRefSHA, "refs/pull/1/head"},
		{"tag key records a branch", "v4", "main"},
		{"revision expression", entryRefSHA, "main~1"},
		{"describe name", entryRefSHA, "v6.1.0-1-g373d742f768e"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := writeEntryRefLock(t, tt.key, tt.ref)
			_, err := LoadState(dir, fakeMetadataResolver{})
			require.ErrorIs(t, err, ErrCorruptLockfile)
			assert.Contains(t, err.Error(), "actions/checkout@"+tt.key)
		})
	}
}

func TestLoadState_ValidEntryRefLoads(t *testing.T) {
	for _, tt := range []struct{ key, ref string }{
		{"v4", "v4"},
		{entryRefSHA, "v6.1.0"},
		// Well-formed; parity rejects it as not a tag or branch.
		{entryRefSHA, "pull/1/head"},
	} {
		dir := writeEntryRefLock(t, tt.key, tt.ref)
		_, err := LoadState(dir, fakeMetadataResolver{})
		require.NoError(t, err, "%s@%s", tt.key, tt.ref)
	}
}

// TestLoadState_OwnSHARefIsUnrecorded: a workflow's full-SHA pin recorded
// with that SHA as its ref loads, but as unrecorded, so a run re-resolves it
// and records its tag or branch instead of failing the whole lockfile.
func TestLoadState_OwnSHARefIsUnrecorded(t *testing.T) {
	dir := writeEntryRefLock(t, entryRefSHA, entryRefSHA)
	s, err := LoadState(dir, fakeMetadataResolver{})
	require.NoError(t, err)

	key := "actions/checkout@" + entryRefSHA
	assert.Equal(t, entryRefSHA, s.File().Dependencies[key].Ref, "the entry is kept as written")
	deps, err := s.Get(".github/workflows/ci.yml")
	require.NoError(t, err)
	assert.Empty(t, deps)
	assert.Empty(t, s.AllDeps(), "never seeds the resolver")
	pin, _ := parserlock.ParsePin(key)
	assert.True(t, RecordsOwnSHA(pin, entryRefSHA))
}

// TestLoadState_TransitiveOwnSHARefIsCorrupt: only a workflow's own pin is
// re-resolved, so an entry no workflow lists directly stays a load error.
func TestLoadState_TransitiveOwnSHARefIsCorrupt(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, ".github", "workflows"), 0o755))
	parent, child := "actions/checkout@v4", "actions/cache@"+entryRefSHA
	body := strings.Join([]string{
		"version: '" + parserlock.Version + "'",
		"dependencies:",
		"  '" + parent + "':",
		"    ref: 'v4'",
		"    commit: 'sha1-" + entryRefSHA + "'",
		"    owner_id: 1",
		"    repo_id: 1",
		"    uses:",
		"      - '" + child + "'",
		"  '" + child + "':",
		"    ref: '" + entryRefSHA + "'",
		"    commit: 'sha1-" + entryRefSHA + "'",
		"    owner_id: 1",
		"    repo_id: 2",
		"workflows:",
		"  '.github/workflows/ci.yml':",
		"    - '" + parent + "'",
		"",
	}, "\n")
	require.NoError(t, os.WriteFile(filepath.Join(dir, parserlock.Path), []byte(body), 0o600))

	_, err := LoadState(dir, fakeMetadataResolver{})
	require.ErrorIs(t, err, ErrCorruptLockfile)
	assert.Contains(t, err.Error(), child+" records ref")
}

func writeEntryRefLock(t *testing.T, key, ref string) string {
	t.Helper()
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, ".github", "workflows"), 0o755))
	pin := "actions/checkout@" + key
	body := strings.Join([]string{
		"version: '" + parserlock.Version + "'",
		"dependencies:",
		"  '" + pin + "':",
		"    ref: '" + ref + "'",
		"    commit: 'sha1-" + entryRefSHA + "'",
		"    owner_id: 1",
		"    repo_id: 1",
		"workflows:",
		"  '.github/workflows/ci.yml':",
		"    - '" + pin + "'",
		"",
	}, "\n")
	require.NoError(t, os.WriteFile(filepath.Join(dir, parserlock.Path), []byte(body), 0o600))
	return dir
}

// TestLoadState_ContentErrorNamesProblem: an invalid lockfile is reported by
// what's wrong with it, never as "unreadable", and still matches
// ErrCorruptLockfile.
func TestLoadState_ContentErrorNamesProblem(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, ".github", "workflows"), 0o755))
	sha := "9ddf35b71a482be7d8922b28e8d00df16b77e315"
	lock := "version: 'v0.0.3'\ndependencies:\n  'actions/checkout@" + sha + "':\n    ref: 'main~1'\n    commit: 'sha1-" + sha + "'\n    owner_id: 1\n    repo_id: 1\nworkflows:\n  '.github/workflows/ci.yml':\n    - 'actions/checkout@" + sha + "'\n"
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".github", "workflows", "actions.lock"), []byte(lock), 0o600))

	_, err := LoadState(dir, fakeMetadataResolver{})
	require.ErrorIs(t, err, ErrCorruptLockfile)
	assert.Equal(t, `.github/workflows/actions.lock: line 3: entry for actions/checkout@`+sha+` records ref "main~1", which isn't a tag or branch name the Actions backend accepts`, err.Error())
	assert.NotContains(t, err.Error(), "unreadable")
}

// TestLoadState_ContentErrorLineIsTheEntry: the reported line is the
// dependency entry, not the workflow list item that names the same key.
func TestLoadState_ContentErrorLineIsTheEntry(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, ".github", "workflows"), 0o755))
	key := "actions/checkout@" + entryRefSHA
	lock := "version: 'v0.0.3'\n" +
		"workflows:\n" +
		"  '.github/workflows/ci.yml':\n" +
		"    - '" + key + "'\n" +
		"dependencies:\n" +
		"  '" + key + "':\n" +
		"    ref: 'main~1'\n" +
		"    commit: 'sha1-" + entryRefSHA + "'\n" +
		"    owner_id: 1\n" +
		"    repo_id: 1\n"
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".github", "workflows", "actions.lock"), []byte(lock), 0o600))

	_, err := LoadState(dir, fakeMetadataResolver{})
	require.ErrorIs(t, err, ErrCorruptLockfile)
	assert.Contains(t, err.Error(), "actions.lock: line 6: entry for "+key+" ")
}

// TestLoadState_ContentErrorLineKeepsRefCase: refs are case-sensitive, so
// the line is the invalid entry's, not a case variant's, in either order;
// owner and repo casing doesn't matter.
func TestLoadState_ContentErrorLineKeepsRefCase(t *testing.T) {
	const bad = "v1-g1234abcd"  // refused: shaped like `git describe` output
	const good = "V1-G1234ABCD" // accepted: the describe suffix is lowercase hex
	entry := func(key, ref string) string {
		return "  '" + key + "':\n" +
			"    ref: '" + ref + "'\n" +
			"    commit: 'sha1-" + entryRefSHA + "'\n" +
			"    owner_id: 1\n" +
			"    repo_id: 1\n"
	}
	tests := []struct {
		name     string
		entries  []string
		wantLine int
		wantKey  string
	}{
		{"valid variant first", []string{entry("actions/checkout@"+good, good), entry("actions/checkout@"+bad, bad)}, 9, "actions/checkout@" + bad},
		{"invalid variant first", []string{entry("actions/checkout@"+bad, bad), entry("actions/checkout@"+good, good)}, 4, "actions/checkout@" + bad},
		{"owner and repo casing", []string{entry("actions/checkout@"+good, good), entry("Actions/Checkout@"+bad, bad)}, 9, "actions/checkout@" + bad},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			require.NoError(t, os.MkdirAll(filepath.Join(dir, ".github", "workflows"), 0o755))
			lock := "version: 'v0.0.3'\nworkflows: {}\ndependencies:\n" + strings.Join(tt.entries, "")
			require.NoError(t, os.WriteFile(filepath.Join(dir, ".github", "workflows", "actions.lock"), []byte(lock), 0o600))

			_, err := LoadState(dir, fakeMetadataResolver{})
			require.ErrorIs(t, err, ErrCorruptLockfile)
			assert.Contains(t, err.Error(), fmt.Sprintf("actions.lock: line %d: entry for %s ", tt.wantLine, tt.wantKey))
		})
	}
}
