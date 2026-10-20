package pin

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/github/gh-actions-lock/internal/lockfile"
	"github.com/github/gh-actions-lock/internal/resolve"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestProveIdentity_NoClientFailsClosed: without an API client no
// candidate's identity can be proven, so every candidate is refused.
func TestProveIdentity_NoClientFailsClosed(t *testing.T) {
	sha := strings.Repeat("a", 40)
	key := "actions/cache@" + sha
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, ".github", "workflows"), 0o755))
	lock := "version: 'v0.0.3'\nworkflows: {}\ndependencies:\n    '" + key + "':\n        ref: '" + sha + "'\n        commit: 'sha1-" + sha + "'\n        owner_id: 1\n        repo_id: 1\n"
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".github", "workflows", "actions.lock"), []byte(lock), 0o600))
	store, err := lockfile.LoadState(dir, fakeMeta{})
	require.NoError(t, err)

	failed := proveIdentity(context.Background(), &resolve.Resolver{}, store.Preview(), []string{key})
	assert.Contains(t, failed, key)
	assert.Empty(t, proveIdentity(context.Background(), &resolve.Resolver{}, store.Preview(), nil), "no candidates, nothing to prove")
}
