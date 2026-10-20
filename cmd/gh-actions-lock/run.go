// Command gh-actions-lock scans workflows and pins GitHub Actions to
// immutable commit SHAs.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"runtime/debug"
	"slices"
	"sort"
	"strings"
	"sync"

	"github.com/cli/go-gh/v2/pkg/repository"
	parserlock "github.com/github/actions-lockfile/go/pkg/lockfile"
	"github.com/github/gh-actions-lock/cmd/gh-actions-lock/format"
	"github.com/github/gh-actions-lock/internal/dep"
	"github.com/github/gh-actions-lock/internal/lockfile"
	"github.com/github/gh-actions-lock/internal/pin"
	"github.com/github/gh-actions-lock/internal/pinpool"
	"github.com/github/gh-actions-lock/internal/pipeline"
	"github.com/github/gh-actions-lock/internal/pipeline/checks"
	"github.com/github/gh-actions-lock/internal/profile"
	"github.com/github/gh-actions-lock/internal/resolve"
	"github.com/github/gh-actions-lock/internal/tag"
	"github.com/github/gh-actions-lock/internal/ui"
	"github.com/github/gh-actions-lock/internal/workflowfile"
	"github.com/spf13/cobra"
)

type checkOptions struct {
	workflowPaths []string
	jsonFields    string
	hostname      string
	// profileDir, when non-empty, enables profiling: execution trace,
	// CPU profile, and HTTP request log are written to files in this dir.
	profileDir string
	// noFix makes the run read-only: report findings and exit without
	// rewriting workflows or updating the lockfile. Orthogonal to the
	// renderer choice (--json).
	noFix bool
	// noNarrow disables tag narrowing: mutable version refs like "v4"
	// are kept as-is in the lock comment instead of being resolved to
	// the full patch tag (e.g. "v4.2.1").
	noNarrow bool
	// allowRunners and allowAllRunners are deprecated no-ops retained for
	// backward compatibility. The hosted/self-hosted runner restriction has
	// been removed; all runner labels are now accepted. These flags will be
	// removed in the next minor release.
	allowRunners    []string
	allowAllRunners bool
	// acceptMoved re-resolves deps flagged as unreachable-pin or
	// ref-moved: prunes the stale lockfile entry and re-pins to the
	// current live SHA.
	acceptMoved bool
	// relock re-resolves deps whose branch or partial-version ref has
	// legitimately advanced (ref-moved) to the current upstream SHA.
	// Unlike acceptMoved it does NOT accept unreachable-pin findings
	// (possible tampering), which stay hard errors.
	relock bool
	// verify is the frozen mode: the default checks with --no-fix and a
	// non-zero exit on any blocking finding.
	verify bool
	// verifyLocal performs a zero-network static check: every action ref
	// in scanned workflows must have a corresponding lockfile entry.
	// No auth, no resolution, no reachability — ideal for pre-commit hooks.
	verifyLocal bool
	// noMigrateLocalActions is a compatibility no-op: local paths are never migrated.
	noMigrateLocalActions bool
}

// bindCheckFlags registers the run flags on the root command.
func bindCheckFlags(cmd *cobra.Command, opts *checkOptions) {
	cmd.Flags().StringVar(&opts.jsonFields, "json", "", "Output JSON with the specified `fields` (valid,findings,workflows,dependencies,pins)")
	cmd.Flags().Lookup("json").NoOptDefVal = "valid,findings,workflows"
	cmd.Flags().StringVar(&opts.hostname, "hostname", "", "GitHub hostname to query (defaults to GH_HOST, current repo host, or github.com)")
	cmd.Flags().BoolVar(&opts.noFix, "no-fix", false, "Read-only: report findings without modifying workflows or the lockfile")
	cmd.Flags().BoolVar(&opts.noNarrow, "no-narrow", false,
		"Keep mutable version refs (e.g. v4) instead of narrowing to full semver (e.g. v4.2.1).\n"+
			"Only refs that parse as semantic versions are narrowed (v4, v4.2); branch names\n"+
			"like 'main' or non-version tags are never narrowed because a full semver tag is\n"+
			"far more likely to resolve to exactly one commit for its entire lifetime.")
	cmd.Flags().StringSliceVar(&opts.allowRunners, "allow-runners", nil, "Deprecated no-op: runner restrictions have been removed")
	cmd.Flags().BoolVarP(&opts.allowAllRunners, "allow-all-runners", "A", false, "Deprecated no-op: runner restrictions have been removed")
	cmd.Flags().BoolVar(&opts.acceptMoved, "accept-moved", false, "Re-resolve deps flagged as ref-moved or unreachable-pin to their current live SHA")
	cmd.Flags().BoolVar(&opts.relock, "relock", false, "Re-resolve the complete dependency graph from current refs; leaves unreachable pins as errors")
	cmd.Flags().BoolVar(&opts.verify, "verify", false, "Frozen check: verify locked commits and their dependency manifests without writing; fails on incomplete coverage or rejected pins")
	cmd.Flags().BoolVar(&opts.verifyLocal, "verify-local", false,
		"Offline coverage check: verify direct refs and recorded transitive edges have dependency entries.\n"+
			"No network calls, no authentication, no file writes — ideal for pre-commit hooks.\n"+
			"It does not check that the entries are trustworthy; use --verify in CI for that.")
	cmd.Flags().StringVar(&opts.profileDir, "profile", "", "Enable profiling: write trace, CPU profile, and HTTP log to `dir`")
	cmd.Flags().BoolVar(&opts.noMigrateLocalActions, "no-migrate-local-actions", false,
		"Compatibility no-op: local ./ actions are always left unchanged and never followed")
}

// validateOutputFlags rejects incoherent structured-output flag combinations.
// Wired as PreRunE so the error surfaces at the command layer before any work runs.
func (opts *checkOptions) validateOutputFlags() error {
	if err := format.ValidateJSONFields(opts.jsonFields); err != nil {
		return err
	}
	if opts.verify && opts.verifyLocal {
		return fmt.Errorf("--verify and --verify-local are mutually exclusive")
	}
	if opts.verifyLocal && (opts.acceptMoved || opts.relock) {
		return fmt.Errorf("--verify-local cannot be combined with --accept-moved or --relock because it runs offline")
	}
	return nil
}

func runCheck(cmd *cobra.Command, opts *checkOptions, newResolver resolverFunc) (runErr error) {
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	out := cmd.OutOrStdout()
	errOut := cmd.ErrOrStderr()
	console := ui.NewWithWriter(errOut)
	defer console.StopProgress()

	// Expand verify mode flags (see verify.go for both modes).
	applyVerifyFlags(opts)

	// --verify-local: offline static coverage check. Skip all network setup.
	if opts.verifyLocal {
		return runVerifyLocal(opts, out, console)
	}

	// Every exit path below writes exactly one run log, judged by runErr, so
	// the log can never call a failed run valid.
	rl := &runLog{}
	defer func() {
		path := rl.save(runErr)
		if runErr == nil || opts.jsonFields != "" {
			return
		}
		// A tool failure prints here, before the log path, rather than
		// after it in execute.
		if !errors.Is(runErr, errSilent) && !errors.Is(runErr, context.Canceled) {
			console.StopProgress()
			console.TermError("%s", runErr)
			runErr = printedError{runErr}
		}
		if path != "" {
			console.TermBlank()
			console.TermDetail("Run log: %s", path)
		}
	}()

	// Profiling: when --profile is set, start trace + CPU profile + HTTP log.
	var prof *profile.Session
	if opts.profileDir != "" {
		if err := os.MkdirAll(opts.profileDir, 0o755); err != nil {
			return fmt.Errorf("creating profile dir: %w", err)
		}
		var err error
		prof, err = profile.Start(profile.Options{
			TracePath:      filepath.Join(opts.profileDir, "trace.out"),
			CPUProfilePath: filepath.Join(opts.profileDir, "cpu.prof"),
			HTTPLog:        true,
			Output:         errOut,
		})
		if err != nil {
			return fmt.Errorf("starting profiler: %w", err)
		}
		defer prof.Stop()
	}

	// Shared worker pool for all concurrent phases.
	pool := pinpool.New(0, console) // 0 → DefaultWorkers

	// If profiling, use a profiled resolver that logs HTTP calls.
	if prof != nil && newResolver == nil {
		newResolver = func(hostname string, pool *pinpool.Pool) (*resolve.Resolver, error) {
			return resolve.New(hostname, pool, resolve.WithProfile(prof))
		}
	}

	endSetup := prof.Phase("setup (discover + lockfile)")
	// A full-directory scan (no explicit workflow-path args) is the only
	// invocation with the authority to declare a workflow deleted. Capture
	// this before newRun overwrites opts.workflowPaths with the discovered
	// set.
	fullScan := len(opts.workflowPaths) == 0
	recoverLock := newLockRecovery()
	paths, r, store, err := newRun(opts.workflowPaths, opts.hostname, pool, newResolver, recoverLock)
	if err != nil {
		return err
	}
	rl.resolver, rl.store = r, store
	recordedWorkflows := maps.Clone(store.File().Workflows)
	var staleWorkflows []string
	if fullScan {
		keepWorkflows := make(map[string]bool, len(paths))
		for _, p := range paths {
			keepWorkflows[workflowfile.KeyFromPath(p)] = true
		}
		for _, k := range store.WorkflowKeys() {
			if !keepWorkflows[k] {
				staleWorkflows = append(staleWorkflows, k)
			}
		}
		sort.Strings(staleWorkflows)
		if !opts.noFix && len(staleWorkflows) > 0 {
			var refs []parserlock.ActionRef
			for _, pw := range pipeline.ParseAll(paths, nil) {
				if pw.LoadErr != nil {
					return fmt.Errorf("scanning %s: %w", pw.Path, pw.LoadErr)
				}
				scanErrors := append(pw.SelfRepositoryRefErrs, pw.SelfRepositoryResolutionErrs...)
				if len(scanErrors) > 0 {
					return fmt.Errorf("scanning %s: %s", pw.Path, strings.Join(scanErrors, "; "))
				}
				refs = append(refs, pw.Refs...)
			}
			// Drop obsolete pins before binding hosts or verifying identities.
			// This is in-memory only; a failed run leaves the file untouched.
			store.PruneWorkflows(keepWorkflows, refs...)
		}
	}
	if err := store.SetHostname(r.Hostname()); err != nil {
		return err
	}
	recorded := store.AllDeps()
	// A repository recorded on two hosts is bound to neither, and no host is
	// asked about it: diagnosis reports it as a blocking finding.
	conflicts := store.HostConflicts()
	for repo, hosts := range conflicts {
		owner, name, _ := strings.Cut(repo, "/")
		r.BlockRepo(owner, name, "recorded on "+strings.Join(hosts, " and ")+"; not resolved on either")
	}
	var hosted []dep.Dependency
	for _, d := range recorded {
		owner, repo := d.OwnerRepo()
		if conflicts[strings.ToLower(owner+"/"+repo)] == nil {
			hosted = append(hosted, d)
		}
	}
	if err := r.SeedHosts(hosted); err != nil {
		return err
	}
	scope := make([]string, len(paths))
	for i, p := range paths {
		scope[i] = workflowfile.KeyFromPath(p)
	}
	if err := store.VerifyHosts(ctx, scope); err != nil {
		return err
	}
	seedable := slices.DeleteFunc(recorded, func(d dep.Dependency) bool { return !lockfile.Seedable(d) })
	r.SeedBranchHints(seedable)
	// Locked refs are sticky: resolve them from the lockfile. --accept-moved
	// and --relock must see what moved, so they resolve everything live.
	relock := opts.acceptMoved || opts.relock
	if !relock {
		r.SeedFromLockfile(seedable)
	}
	endSetup()

	opts.workflowPaths = paths

	// Detailed narration is suppressed from the terminal during the run so the
	// output stays limited to phase labels, prompts, and the final summary.
	// A structured, action-centric provenance record (what was resolved and
	// how) is written at the end instead. JSON mode keeps the narration log
	// attached so machine-readable events can flow on stderr.
	if opts.jsonFields == "" {
		console.SetLog(io.Discard)
	}

	// Pipeline: scan → resolve → reachability → diagnose → enrich.
	total := len(opts.workflowPaths)
	showSpinner := opts.jsonFields == "" && !console.Headless()
	showHeadlessProgress := console.Headless()

	if showHeadlessProgress {
		console.StartProgress(fmt.Sprintf("Scanning %d %s", total, ui.Pluralize(total, "workflow", "workflows")))
	}

	// Build a Lister for pin narrowing, reusing the resolver's API client; cooldown is resolved dependabot > config file > none.
	var tagger *tag.Lister
	var cooldownCfg tag.CooldownConfig
	var cooldownWarnings []string
	if gc := r.GHClient(); gc != nil {
		cooldownCfg, _, cooldownWarnings = ResolveCooldown(".")
		tagger = tag.NewLister(gc, cooldownCfg)
	}

	runOpts := pipeline.RunOptions{
		WorkflowPaths:     opts.workflowPaths,
		Resolver:          r,
		Store:             store,
		RecordedWorkflows: recordedWorkflows,
		Pool:              pool,
		Relock:            relock,
		ReadOnly:          opts.noFix,
		// Accepting a moved pin relies on planning to re-record it, which a
		// read-only run never does.
		AcceptMoved:       opts.acceptMoved && !opts.noFix,
		Verify:            opts.verify,
		CollapseSpellings: opts.relock && fullScan && !opts.noFix,
		Profile:           prof,
	}
	// Defer spinner start until actual network work begins. The fast path
	// (everything trusted from the lockfile) returns before resolve fires,
	// so the spinner never appears and there's no flicker.
	if showSpinner {
		var once sync.Once
		runOpts.OnResolveProgress = func(done, total int) {
			once.Do(func() {
				console.StartProgress("Resolving actions")
			})
		}
	}

	endRun := prof.Phase("pipeline.Run (scan+resolve+reachability+diagnose)")
	result, err := pipeline.Run(ctx, runOpts)
	endRun()
	if err != nil {
		console.StopProgress()
		return err
	}
	if showSpinner {
		console.ClearWorkerStatuses()
		console.UpdateProgress("")
	}

	report := result.Report
	valid := result.Valid
	// A read-only run writes nothing, so a finding deferred on a write stands.
	if opts.noFix {
		report.RestoreDeferred()
		valid = report.IsValid()
	}

	// Read-only modes never touch the lockfile, so surface stale entries as
	// non-blocking info findings instead of pruning them. Fix mode reports
	// the prune only after the updated lockfile is saved.
	if opts.noFix && len(staleWorkflows) > 0 {
		appendStaleWorkflowFindings(report, staleWorkflows, false)
	}
	if opts.noFix {
		appendOwnSHAEntryFindings(report, store)
		valid = report.IsValid()
	}

	// --no-onboard: refuse to onboard new workflows or actions. Rewrite the
	// relevant not-pinned findings to onboarding-required and drop their refs
	// so Plan/Commit never pins them; already-tracked refs that were bumped
	// (ref-changed) are left to re-pin as usual.
	onboardingRefused := 0
	var refusedLabels []string
	if noOnboardFlag(cmd) {
		refusedLabels = gateNoOnboard(report)
		onboardingRefused = len(refusedLabels)
		if onboardingRefused > 0 {
			valid = report.IsValid()
		}
	}

	// Surface Dependabot cooldown keys we don't honor before the diagnosis so
	// they reach both read-only (--no-fix/--verify) and fix runs. Non-blocking.
	appendCooldownConfigFindings(report, cooldownWarnings)

	rl.report, rl.valid = report, valid

	// Render the read-only diagnosis. --json selects the renderer; it does
	// not decide whether fixes are applied. Terminal output is shown up front
	// (the human narrative). JSON is emitted later, after any fixes land, so
	// stdout never carries a diagnosis for a run that then failed to commit.
	if opts.jsonFields == "" {
		// Pause the spinner so PresentResults lines don't collide with it.
		if showSpinner {
			console.PauseProgress()
		}
		format.PresentResults(console, report, valid, !opts.noFix)
		if showSpinner {
			console.ResumeProgress()
		}
	}

	// --no-fix: read-only. Stop after the diagnosis without touching disk.
	// Strict gate — any blocking finding is a non-zero exit.
	if opts.noFix {
		console.StopProgress()
		if opts.jsonFields != "" {
			if err := format.WriteJSON(out, report, nil, valid, opts.jsonFields, cliVersion(), store.File().Version, r.Hostname()); err != nil {
				return err
			}
		}
		// Surface SSO guidance even in read-only mode — it's the actionable
		// fix for SAML-gated repos and shouldn't require a --fix run to see.
		if opts.jsonFields == "" && reportHasSSO(report) {
			console.TermBlank()
			for _, hint := range ssoFixHints() {
				console.TermDetail("%s", hint)
			}
		}
		if !valid {
			if opts.jsonFields == "" {
				hasFixable := format.PresentReadOnlyFailures(console, report)
				if hasFixable {
					flag := "--no-fix"
					if opts.verify {
						flag = "--verify"
					}
					console.TermDetail("Re-run without %s to apply fixes.", flag)
				}
			}
			return errSilent
		}
		if opts.jsonFields == "" {
			n := len(report.Workflows)
			console.TermSuccess("All %d %s valid", n, ui.Pluralize(n, "workflow", "workflows"))
		}
		return nil
	}

	// Plan: build the complete pin record (resolution, reachability,
	// narrowing, reverse lookup — all computation, no disk writes).
	var repoOwner, repoName string
	if currentRepo, err := repository.Current(); err == nil {
		repoOwner = currentRepo.Owner
		repoName = currentRepo.Name
	}

	endPlan := prof.Phase("pin.Plan (narrowing+reverse)")
	if showSpinner {
		console.ClearWorkerStatuses()
	}
	record, planErr := pin.Plan(ctx, report, pin.PlanOptions{
		Resolver:    r,
		Tagger:      tagger,
		Store:       store,
		Pool:        pool,
		RepoOwner:   repoOwner,
		RepoName:    repoName,
		NoNarrow:    opts.noNarrow,
		AcceptMoved: opts.acceptMoved,
		Relock:      opts.relock,
		PartialScan: !fullScan,
	})
	endPlan()
	rl.record = record
	copts := &pin.CommitOptions{SkipNewWorkflowEntries: noOnboardFlag(cmd)}
	if planErr == nil {
		planErr = pin.PlanEntryRepairs(ctx, record, store, r, report, copts)
	}
	if planErr == nil && len(record.Unresolved()) > 0 {
		planErr = fmt.Errorf("cannot write an incomplete lockfile: %d unresolved dependencies", len(record.Unresolved()))
	}
	if planErr != nil {
		console.StopProgress()
		// Nothing is written, so every deferred entry stays as recorded.
		report.RestoreDeferred()
		unresolved := replaceOffHistoryFindings(report, record.Unresolved())
		if opts.jsonFields != "" {
			if err := format.WriteJSON(out, report, record, false, opts.jsonFields, cliVersion(), store.File().Version, r.Hostname()); err != nil {
				return err
			}
		}
		if len(record.Unresolved()) > 0 {
			if opts.jsonFields == "" {
				// Restored deferred findings explain the failure too, even
				// when every unresolved entry is reported below.
				if len(unresolved) < len(record.Unresolved()) || !report.IsValid() {
					format.PresentReadOnlyFailures(console, report)
				}
				if len(unresolved) > 0 {
					renderUnresolvedWarnings(console, unresolved)
				}
				console.TermBlank()
				console.TermError("Nothing was written: fix the errors above and re-run")
				if fullScan {
					console.TermDetail("To lock only some workflows, pass their paths: gh actions-lock <workflow>...")
				}
			}
			return errSilent
		}
		return fmt.Errorf("planning pins: %w", planErr)
	}

	// Parity excused or accepted some findings on the strength of this
	// write; any it doesn't bear out block it.
	if err := pin.Audit(ctx, record, store, report, copts, result.InspectRetained); err != nil {
		console.StopProgress()
		return fmt.Errorf("checking the planned write: %w", err)
	}

	// All or nothing: a run with errors it cannot fix writes nothing.
	if runHasBlockingErrors(report, record, opts.acceptMoved) {
		console.StopProgress()
		report.RestoreDeferred()
		if opts.jsonFields != "" {
			if err := format.WriteJSON(out, report, record, false, opts.jsonFields, cliVersion(), store.File().Version, r.Hostname()); err != nil {
				return err
			}
			return errSilent
		}
		if investigated := record.Investigated(); len(investigated) > 0 {
			renderInvestigationAlerts(console, investigated, r)
		}
		format.PresentReadOnlyFailures(console, withoutPendingPins(report))
		console.TermBlank()
		console.TermError("Nothing was written: fix the errors above and re-run")
		return errSilent
	}

	// Commit: write all changes to disk atomically (fast local I/O, no
	// spinner label — it finishes before the user could read one).
	endCommit := prof.Phase("pin.Commit (disk writes)")
	if err := pin.Commit(ctx, record, store, copts); err != nil {
		console.StopProgress()
		return fmt.Errorf("committing pins: %w", err)
	}
	endCommit()

	console.StopProgress()

	appendUnselectedOwnSHAWarnings(report, store)

	// Record the prune in the report so --json consumers see what was
	// dropped. The terminal surface reports it via the pin summary instead.
	if len(staleWorkflows) > 0 {
		appendStaleWorkflowFindings(report, staleWorkflows, true)
	}

	// Inject info-severity findings for non-semver refs so they appear
	// in --json output. Suppressed when --no-narrow is set (user chose
	// this deliberately).
	if !opts.noNarrow {
		injectVersionRefFindings(report, record)
	}

	// Nudge on freshly pinned tags for actions with no effective cooldown.
	// Non-blocking; per-entry so a global "configured" flag can't mask an
	// action whose effective cooldown is 0.
	if tagger != nil {
		injectFreshTagFindings(ctx, report, record, tagger, cooldownCfg)
	}

	// JSON mode emits the (pre-fix) diagnosis now — after the commit
	// succeeded — so machine consumers never see findings for a run that
	// then failed to write.
	if opts.jsonFields != "" {
		return format.WriteJSON(out, report, record, valid, opts.jsonFields, cliVersion(), store.File().Version, r.Hostname())
	}

	// Terminal summary.
	return renderPinSummary(ctx, console, record, report, r, refusedLabels, opts.noNarrow, opts.acceptMoved, store.OriginalVersion(), staleWorkflows)
}

// injectVersionRefFindings appends info-severity findings for entries pinned
// with a non-full-semver ref (v4, v3.1, main, etc.). These surface in --json
// output so machine consumers can detect imprecise refs.
func injectVersionRefFindings(report *checks.Report, record *pin.Record) {
	// Index which workflows each non-semver dep appears in.
	type depInfo struct {
		nwo string
		ref string
		wfs map[string]bool
	}
	seen := map[string]*depInfo{} // NWO@Ref → info
	for _, e := range record.Entries {
		if e.Resolution != pin.Pinned && e.Resolution != pin.Verified {
			continue
		}
		// Transitive deps come from a composite's action.yml; the user
		// cannot change their ref, so the nudge is not actionable.
		if !e.Direct {
			continue
		}
		sv, ok := parserlock.ParseSemVer(e.Ref)
		if ok && sv.IsFull() {
			continue
		}
		// Only flag partial semver refs (v4, v3.1) — not branch names
		// like canary, main, nightly which are intentional.
		if !ok {
			continue
		}
		key := e.NWO + "@" + e.Ref
		di, exists := seen[key]
		if !exists {
			di = &depInfo{nwo: e.NWO, ref: e.Ref, wfs: map[string]bool{}}
			seen[key] = di
		}
		for _, wf := range e.Workflows {
			di.wfs[wf] = true
		}
	}
	if len(seen) == 0 {
		return
	}

	// Append a finding to each affected workflow report.
	for i := range report.Workflows {
		wr := &report.Workflows[i]
		for _, di := range seen {
			if !di.wfs[wr.Path] {
				continue
			}
			wr.Findings = append(wr.Findings, checks.Finding{
				WorkflowPath: wr.Path,
				Category:     checks.VersionRef,
				Severity:     checks.SeverityInfo,
				Confidence:   checks.ConfidenceHigh,
				Detail: fmt.Sprintf(
					"%s@%s: prefer a full semver ref (e.g. v4.2.1) — each patch tag resolves to exactly one commit",
					di.nwo, di.ref,
				),
			})
		}
	}
}

// appendStaleWorkflowFindings records a repo-level info finding for each
// workflow key whose file no longer exists on disk. pruned controls the
// wording: true after the entry was dropped, false when it was only reported
// (read-only run).
func appendStaleWorkflowFindings(report *checks.Report, workflows []string, pruned bool) {
	for _, wf := range workflows {
		detail := fmt.Sprintf("lockfile records deleted workflow %s — run `gh actions-lock` to prune it", wf)
		if pruned {
			detail = fmt.Sprintf("pruned stale lockfile entry for deleted workflow %s", wf)
		}
		report.RepoFindings = append(report.RepoFindings, checks.Finding{
			WorkflowPath: wf,
			Category:     checks.StaleWorkflow,
			Severity:     checks.SeverityInfo,
			Confidence:   checks.ConfidenceHigh,
			Detail:       detail,
		})
	}
}

// cliVersion returns the gh-actions-lock extension version embedded by the Go
// build system. Returns "(devel)" for local `go build` and a real version
// like "v0.1.2" when installed via `gh extension install`.
func cliVersion() string {
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" {
		return info.Main.Version
	}
	return "unknown"
}
