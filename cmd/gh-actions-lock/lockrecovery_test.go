package main

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	parserlock "github.com/github/actions-lockfile/go/pkg/lockfile"
	"github.com/github/gh-actions-lock/internal/ghapi/httpmock"
	"github.com/github/gh-actions-lock/internal/ui"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeConfirmer struct {
	result    bool
	err       error
	called    bool
	gotPrompt string
}

func (f *fakeConfirmer) Confirm(prompt string, _ bool) (bool, error) {
	f.called = true
	f.gotPrompt = prompt
	return f.result, f.err
}

func writeScratchLock(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "actions.lock")
	if err := os.WriteFile(p, []byte("corrupt"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func discardUI() *ui.UI { return ui.NewWithWriter(io.Discard) }

func fileExists(t *testing.T, p string) bool {
	t.Helper()
	_, err := os.Stat(p)
	return err == nil
}

func TestNewLockRecovery(t *testing.T) {
	parseErr := errors.New("missing required action field \"owner_id\"")

	t.Run("read-only/relock command refuses to delete and fails", func(t *testing.T) {
		lock := writeScratchLock(t)
		fc := &fakeConfirmer{result: true}
		rec := newLockRecovery(false, discardUI(), func() (confirmer, bool) { return fc, true }, false)

		recovered, err := rec(lock, parseErr)
		if recovered || err == nil {
			t.Fatalf("want (false, error), got (%v, %v)", recovered, err)
		}
		if fc.called {
			t.Error("confirmer must not be consulted when delete is disallowed")
		}
		if !fileExists(t, lock) {
			t.Error("lockfile must be left in place")
		}
	})

	t.Run("non-interactive flag fails without prompting", func(t *testing.T) {
		lock := writeScratchLock(t)
		fc := &fakeConfirmer{result: true}
		rec := newLockRecovery(true, discardUI(), func() (confirmer, bool) { return fc, true }, true)

		recovered, err := rec(lock, parseErr)
		if recovered || err == nil {
			t.Fatalf("want (false, error), got (%v, %v)", recovered, err)
		}
		if fc.called {
			t.Error("confirmer must not be consulted under --no-interactive")
		}
		if !fileExists(t, lock) {
			t.Error("lockfile must be left in place")
		}
	})

	t.Run("headless session (canPrompt false) fails", func(t *testing.T) {
		lock := writeScratchLock(t)
		rec := newLockRecovery(false, discardUI(), func() (confirmer, bool) { return nil, false }, true)

		recovered, err := rec(lock, parseErr)
		if recovered || err == nil {
			t.Fatalf("want (false, error), got (%v, %v)", recovered, err)
		}
		if !fileExists(t, lock) {
			t.Error("lockfile must be left in place")
		}
	})

	t.Run("interactive confirm yes deletes and recovers", func(t *testing.T) {
		lock := writeScratchLock(t)
		fc := &fakeConfirmer{result: true}
		rec := newLockRecovery(false, discardUI(), func() (confirmer, bool) { return fc, true }, true)

		recovered, err := rec(lock, parseErr)
		if !recovered || err != nil {
			t.Fatalf("want (true, nil), got (%v, %v)", recovered, err)
		}
		if !fc.called {
			t.Error("confirmer should have been consulted")
		}
		if !strings.Contains(fc.gotPrompt, "has invalid contents") || !strings.Contains(fc.gotPrompt, parseErr.Error()) {
			t.Errorf("prompt must distinguish invalid contents from I/O failure and preserve the cause: %s", fc.gotPrompt)
		}
		if fileExists(t, lock) {
			t.Error("lockfile should have been deleted")
		}
	})

	t.Run("interactive confirm no fails and keeps file", func(t *testing.T) {
		lock := writeScratchLock(t)
		fc := &fakeConfirmer{result: false}
		rec := newLockRecovery(false, discardUI(), func() (confirmer, bool) { return fc, true }, true)

		recovered, err := rec(lock, parseErr)
		if recovered || err == nil {
			t.Fatalf("want (false, error), got (%v, %v)", recovered, err)
		}
		if !fileExists(t, lock) {
			t.Error("lockfile must be left in place when the user declines")
		}
	})

	t.Run("confirm error propagates and keeps file", func(t *testing.T) {
		lock := writeScratchLock(t)
		boom := errors.New("prompt failed")
		fc := &fakeConfirmer{err: boom}
		rec := newLockRecovery(false, discardUI(), func() (confirmer, bool) { return fc, true }, true)

		recovered, err := rec(lock, parseErr)
		if recovered || !errors.Is(err, boom) {
			t.Fatalf("want (false, boom), got (%v, %v)", recovered, err)
		}
		if !fileExists(t, lock) {
			t.Error("lockfile must be left in place on prompt error")
		}
	})
}

// A v0.0.1 SHA-only entry can't be migrated, and relocking reads the same
// lockfile, so every mode must point at regenerating it, never relocking.
func TestLegacySHAOnlyLockfileGuidance(t *testing.T) {
	const sha = "8e8c483db84b4bee98b60c0593521ed34d9990e8"
	for _, args := range [][]string{nil, {"--relock"}, {"--no-fix"}, {"--verify"}} {
		t.Run(strings.Join(append([]string{"default"}, args...), " "), func(t *testing.T) {
			wf := writeTempWorkflow(t, "on: push\njobs:\n  a:\n    runs-on: ubuntu-latest\n    steps:\n      - uses: actions/checkout@"+sha+"\n")
			pin := "actions/checkout@" + sha + ":sha1-" + sha
			lock := "version: 'v0.0.1'\ndependencies:\n  '" + pin + "':\n    commit: 'sha1-" + sha + "'\n    owner_id: 1\n    repo_id: 1\nworkflows:\n  '" + wf + "':\n    - '" + pin + "'\n"
			require.NoError(t, os.WriteFile(parserlock.Path, []byte(lock), 0o600))

			_, _, err := runCommandWithHTTP(t, &httpmock.Registry{}, append(append([]string{"--no-interactive"}, args...), wf)...)

			require.ErrorIs(t, err, parserlock.ErrRefNeedsRelock)
			assert.Contains(t, err.Error(), "delete it and run `gh actions-lock` to regenerate it")
			assert.NotContains(t, err.Error(), "re-lock")
			assert.NotContains(t, err.Error(), "rebuild it")
		})
	}
}
