package main

// Verification modes for gh actions-lock:
//
//   --verify       Frozen check: the default checks, including runner parity
//                  for every locked pin, without writing. Requires auth.
//
//   --verify-local Offline static coverage check. Every action ref must have
//                  a lockfile entry. No network, no auth, no writes — ideal for
//                  pre-commit. Same-repo `./…` refs are reported, not migrated.

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	parserlock "github.com/github/actions-lockfile/go/pkg/lockfile"
	"github.com/github/gh-actions-lock/cmd/gh-actions-lock/format"
	"github.com/github/gh-actions-lock/internal/lockfile"
	"github.com/github/gh-actions-lock/internal/pipeline"
	"github.com/github/gh-actions-lock/internal/ui"
)

// applyVerifyFlags expands --verify into its constituent flags. Called at the
// top of runCheck before any work begins.
func applyVerifyFlags(opts *checkOptions) {
	if opts.verify {
		opts.noFix = true
	}
}

// runVerifyLocal performs the offline lockfile coverage check. It parses all
// workflows and verifies that every action ref has a corresponding lockfile
// entry — no network, no auth, never writes to disk.
func runVerifyLocal(opts *checkOptions, out io.Writer, console *ui.UI) error {
	workflowsDir := os.Getenv("GH_ACTIONS_LOCK_WORKFLOWS_DIR")
	paths, err := discoverWorkflowPaths(opts.workflowPaths, workflowsDir, false)
	if err != nil {
		return err
	}

	// Load the lockfile without a resolver (no network).
	lockPath := filepath.Join(".", parserlock.Path)
	if workflowsDir != "" {
		lockPath = filepath.Join(workflowsDir, "actions.lock")
	}
	store, err := lockfile.LoadStateAt(lockPath, nil)
	if errors.Is(err, lockfile.ErrCorruptLockfile) {
		// Word it as the other modes do.
		_, err = newLockRecovery()(lockPath, err)
		return err
	}
	if err != nil {
		return fmt.Errorf("opening lockfile: %w", err)
	}
	hostname := resolveHostname(opts.hostname)
	if err := store.SetHostname(hostname); err != nil {
		return err
	}

	parsed := pipeline.ParseAll(paths, store)
	report := pipeline.VerifyLocalCoverage(parsed, store)
	valid := report.IsValid()

	if opts.jsonFields != "" {
		if err := format.WriteJSON(out, report, nil, valid, opts.jsonFields, cliVersion(), store.File().Version, hostname); err != nil {
			return err
		}
	} else {
		format.PresentResults(console, report, valid, false)
	}

	if !valid {
		if opts.jsonFields == "" {
			console.TermDetail("Run without --verify-local to resolve and pin missing actions and migrate `./…` refs to `$/…`.")
		}
		return errSilent
	}

	total := len(paths)
	if opts.jsonFields == "" {
		console.TermSuccess("All %d %s have complete lockfile coverage.",
			total, ui.Pluralize(total, "workflow", "workflows"))
	}
	return nil
}
