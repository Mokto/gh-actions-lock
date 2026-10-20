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
	"maps"
	"os"
	"path/filepath"
	"slices"

	parserlock "github.com/github/actions-lockfile/go/pkg/lockfile"
	"github.com/github/gh-actions-lock/cmd/gh-actions-lock/format"
	"github.com/github/gh-actions-lock/internal/dep"
	"github.com/github/gh-actions-lock/internal/lockfile"
	"github.com/github/gh-actions-lock/internal/pipeline"
	"github.com/github/gh-actions-lock/internal/pipeline/checks"
	"github.com/github/gh-actions-lock/internal/ui"
	"github.com/github/gh-actions-lock/internal/workflowfile"
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
	if len(opts.workflowPaths) > 0 {
		appendUnscannedOwnSHAFindings(report, store, paths)
	}
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

// appendUnscannedOwnSHAFindings reports, for a path-scoped read-only run,
// each pin of a workflow it didn't scan whose entry records its own SHA as
// its ref. The Actions backend rejects the pin, and only a full-scan fix run
// may rewrite that workflow, so the run fails rather than passing.
func appendUnscannedOwnSHAFindings(report *checks.Report, store *lockfile.State, scanned []string) {
	skip := map[string]bool{}
	for _, p := range scanned {
		skip[workflowfile.KeyFromPath(p)] = true
	}
	entries := store.OwnSHAEntries()
	for _, wf := range slices.Sorted(maps.Keys(entries)) {
		if skip[wf] {
			continue
		}
		for _, key := range entries[wf] {
			pin, _ := parserlock.ParsePin(key)
			report.RepoFindings = append(report.RepoFindings, checks.Finding{
				WorkflowPath: wf,
				Category:     checks.NotPinned,
				Severity:     checks.SeverityError,
				Confidence:   checks.ConfidenceHigh,
				Dependency:   &dep.Dependency{NWO: pin.NWO, Ref: pin.Ref, SHA: pin.Ref},
				Detail:       fmt.Sprintf("lock entry for %s in %s records ref %s, a commit SHA rather than the tag or branch it was taken from; the Actions backend rejects this pin", key, wf, parserlock.ShortSHA(pin.Ref)),
				Remediation:  "run `gh actions-lock` without workflow paths to record the tag or branch this commit came from",
			})
		}
	}
}
