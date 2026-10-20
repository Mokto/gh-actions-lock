package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestNewLockRecovery: a bad lockfile always fails, names the problem, points
// at version control for a hand edit and at a fresh lock otherwise, since
// a committed bad file can't be restored, and is never deleted.
func TestNewLockRecovery(t *testing.T) {
	p := filepath.Join(t.TempDir(), "actions.lock")
	require.NoError(t, os.WriteFile(p, []byte("corrupt"), 0o600))
	parseErr := errors.New(".github/workflows/actions.lock: line 7: entry for actions/checkout@v4 records ref \"main\", which doesn't match its key's ref \"v4\"")

	recovered, err := newLockRecovery()(p, parseErr)
	require.Error(t, err)
	assert.False(t, recovered)
	assert.ErrorIs(t, err, parseErr)
	assert.Equal(t, parseErr.Error()+"; if you edited actions.lock by hand, restore it from version control; otherwise move it aside and run `gh actions-lock` to lock your workflows again", err.Error())
	assert.NotContains(t, err.Error(), "delete")
	assert.FileExists(t, p)
}
