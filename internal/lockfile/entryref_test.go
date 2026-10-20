package lockfile

import (
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
		{"full-SHA key records the SHA", entryRefSHA, entryRefSHA},
		{"qualified tag", entryRefSHA, "refs/tags/v6.1.0"},
		{"qualified pull request ref", entryRefSHA, "refs/pull/1/head"},
		{"tag key records a branch", "v4", "main"},
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
