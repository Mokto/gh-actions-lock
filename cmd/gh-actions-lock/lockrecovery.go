package main

import (
	"fmt"
	"path/filepath"

	"github.com/spf13/cobra"
)

// noInteractiveFlag reports the value of the persistent --no-interactive flag,
// defaulting to false when it is not registered.
func noInteractiveFlag(cmd *cobra.Command) bool {
	v, _ := cmd.Flags().GetBool("no-interactive")
	return v
}

// lockRecovery decides what to do when the on-disk lockfile's contents can't
// be used. It returns (true, nil) when loading should be retried, or a
// non-nil error to abort the run (exit 2).
type lockRecovery func(lockPath string, parseErr error) (recovered bool, err error)

// newLockRecovery builds the recovery policy: always fail, naming the
// problem. A bad entry usually means a hand edit, and deleting the lockfile
// or fixing it automatically would lose the pins it records.
func newLockRecovery() lockRecovery {
	return func(lockPath string, parseErr error) (bool, error) {
		return false, fmt.Errorf("%w; if you edited %s by hand, restore it from version control", parseErr, filepath.Base(lockPath))
	}
}
