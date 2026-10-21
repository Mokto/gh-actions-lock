package format

import (
	"bytes"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/github/gh-actions-lock/internal/pipeline/checks"
	"github.com/stretchr/testify/assert"

	parserlock "github.com/github/actions-lockfile/go/pkg/lockfile"
	"github.com/github/gh-actions-lock/internal/dep"
	"github.com/github/gh-actions-lock/internal/ui"
)

// newTestUI returns a UI whose narration log sinks to io.Discard, mirroring
// the production setup `check.go` installs for human-terminal runs. Term*
// methods bypass the log and write to the returned buffer; narration
// helpers (Success/Warning/Detail) go to the discard sink.
func newTestUI() (*ui.UI, *bytes.Buffer) {
	var buf bytes.Buffer
	u := ui.NewPlain(&buf)
	u.SetLog(io.Discard)
	return u, &buf
}

// TestPresentResults_WarningsReachTerminal locks parity between the
// human-terminal renderer and the JSON renderer for warning-severity
// findings.
func TestPresentResults_WarningsReachTerminal(t *testing.T) {
	tests := []struct {
		name       string
		findings   []checks.Finding
		wantOutput []string
		notWanted  []string
	}{
		{
			name: "ref-moved warning is swallowed until update path exists",
			findings: []checks.Finding{{
				WorkflowPath: ".github/workflows/a.yml",
				Category:     checks.RefMoved,
				Severity:     checks.SeverityWarning,
				Confidence:   checks.ConfidenceHigh,
				Dependency: &dep.Dependency{
					NWO: "octo/action",
					Ref: "v1",
					SHA: "1111111111111111111111111111111111111111",
				},
				ObservedSHA: "2222222222222222222222222222222222222222",
				Detail:      "ref v1 now resolves to 222222222222, lockfile pins 111111111111",
			}},
			notWanted: []string{
				"moved upstream",
				"compare/111111111111...222222222222",
				"run `gh actions-lock` to update",
			},
		},
		{
			name: "sha-as-ref direct warning surfaces",
			findings: []checks.Finding{{
				WorkflowPath: ".github/workflows/a.yml",
				Category:     checks.ShaAsRef,
				Severity:     checks.SeverityWarning,
				Confidence:   checks.ConfidenceHigh,
				ActionRef:    &parserlock.ActionRef{Owner: "octo", Repo: "action", Ref: "abc123"},
				Dependency: &dep.Dependency{
					NWO: "octo/action",
					Ref: "abc123",
					SHA: "abc1230000000000000000000000000000000000",
				},
				Detail: "pinned to a bare SHA without a symbolic ref",
			}},
			wantOutput: []string{
				"pinned to a bare SHA",
			},
		},
		{
			name: "workflow-level not-pinned warning surfaces",
			findings: []checks.Finding{{
				WorkflowPath: ".github/workflows/a.yml",
				Category:     checks.NotPinned,
				Severity:     checks.SeverityWarning,
				Confidence:   checks.ConfidenceHigh,
				Detail:       "no lockfile entry",
			}},
			wantOutput: []string{
				"not yet pinned",
			},
		},
		{
			name: "reachability-unknown surfaces under the dep key",
			findings: []checks.Finding{{
				WorkflowPath: ".github/workflows/a.yml",
				Category:     checks.ReachabilityUnknown,
				Severity:     checks.SeverityWarning,
				Confidence:   checks.ConfidenceLow,
				Dependency: &dep.Dependency{
					NWO: "octo/action",
					Ref: "v1",
					SHA: "1111111111111111111111111111111111111111",
				},
				Detail:      "branch list rate limited",
				Remediation: "retry later",
			}},
			wantOutput: []string{
				"octo/action@v1",
				"branch list rate limited",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			u, buf := newTestUI()
			report := &checks.Report{
				Workflows: []checks.WorkflowReport{{
					Path:     ".github/workflows/a.yml",
					Findings: tt.findings,
				}},
			}
			PresentResults(u, report, true, false)

			got := buf.String()
			for _, want := range tt.wantOutput {
				if !strings.Contains(got, want) {
					t.Errorf("PresentResults output missing %q\nfull output:\n%s", want, got)
				}
			}
			for _, unwanted := range tt.notWanted {
				if strings.Contains(got, unwanted) {
					t.Errorf("PresentResults output unexpectedly contains %q\nfull output:\n%s", unwanted, got)
				}
			}
		})
	}
}

// TestPresentResults_NoSuccessLine verifies PresentResults no longer emits
// the "All N workflows valid" success line — renderPinSummary owns that.
func TestPresentResults_NoSuccessLine(t *testing.T) {
	u, buf := newTestUI()
	report := &checks.Report{
		Workflows: []checks.WorkflowReport{
			{Path: ".github/workflows/a.yml"},
			{Path: ".github/workflows/b.yml"},
		},
	}
	PresentResults(u, report, true, false)

	got := buf.String()
	if strings.Contains(got, "All") && strings.Contains(got, "valid") {
		t.Errorf("PresentResults should not print success line, got:\n%s", got)
	}
}

// TestPresentResults_RemediateHints locks the willRemediate-aware "↳"
// follow-up lines that PresentResults emits under each warning headline.
// Categories the remediator auto-fixes (NotPinned, SHAAsRef) flip between
// "resolving below" and a manual "run X" hint; categories it skips
// (RefMoved, inconclusive) always show the manual hint.
func TestPresentResults_RemediateHints(t *testing.T) {
	tests := []struct {
		name          string
		willRemediate bool
		findings      []checks.Finding
		wantOutput    []string
		notWanted     []string
	}{
		{
			name:          "not-pinned shows resolving-below when remediating",
			willRemediate: true,
			findings: []checks.Finding{{
				WorkflowPath: ".github/workflows/a.yml",
				Category:     checks.NotPinned,
				Severity:     checks.SeverityWarning,
				Confidence:   checks.ConfidenceHigh,
			}},
			wantOutput: []string{"not yet pinned", "↳ resolving below"},
			notWanted:  []string{"run `gh actions-lock`"},
		},
		{
			name:          "not-pinned shows manual hint when not remediating",
			willRemediate: false,
			findings: []checks.Finding{{
				WorkflowPath: ".github/workflows/a.yml",
				Category:     checks.NotPinned,
				Severity:     checks.SeverityWarning,
				Confidence:   checks.ConfidenceHigh,
			}},
			wantOutput: []string{"not yet pinned", "↳ run `gh actions-lock` to pin them"},
			notWanted:  []string{"resolving below"},
		},
		{
			name:          "sha-as-ref suppressed when remediating",
			willRemediate: true,
			findings: []checks.Finding{{
				WorkflowPath: ".github/workflows/a.yml",
				Category:     checks.ShaAsRef,
				Severity:     checks.SeverityWarning,
				Confidence:   checks.ConfidenceHigh,
				ActionRef:    &parserlock.ActionRef{Owner: "octo", Repo: "action", Ref: "abc123"},
				Dependency: &dep.Dependency{
					NWO: "octo/action",
					Ref: "abc123",
					SHA: "abc1230000000000000000000000000000000000",
				},
			}},
			notWanted: []string{"bare SHA", "resolving below"},
		},
		{
			name:          "ref-moved is swallowed (deferred to update path)",
			willRemediate: true,
			findings: []checks.Finding{{
				WorkflowPath: ".github/workflows/a.yml",
				Category:     checks.RefMoved,
				Severity:     checks.SeverityWarning,
				Confidence:   checks.ConfidenceHigh,
				Dependency: &dep.Dependency{
					NWO: "octo/action",
					Ref: "v1",
					SHA: "1111111111111111111111111111111111111111",
				},
				ObservedSHA: "2222222222222222222222222222222222222222",
				Detail:      "ref v1 now resolves to 222222222222",
			}},
			notWanted: []string{
				"moved upstream",
				"run `gh actions-lock` to update",
			},
		},
		{
			name:          "inconclusive warning surfaces ↳ remediation when set",
			willRemediate: false,
			findings: []checks.Finding{{
				WorkflowPath: ".github/workflows/a.yml",
				Category:     checks.ReachabilityUnknown,
				Severity:     checks.SeverityWarning,
				Confidence:   checks.ConfidenceLow,
				Dependency: &dep.Dependency{
					NWO: "octo/action",
					Ref: "v1",
					SHA: "1111111111111111111111111111111111111111",
				},
				Detail:      "branch list rate limited",
				Remediation: "retry later",
			}},
			wantOutput: []string{
				"branch list rate limited",
				"↳ retry later",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			u, buf := newTestUI()
			report := &checks.Report{
				Workflows: []checks.WorkflowReport{{
					Path:     ".github/workflows/a.yml",
					Findings: tt.findings,
				}},
			}
			PresentResults(u, report, true, tt.willRemediate)

			got := buf.String()
			for _, want := range tt.wantOutput {
				if !strings.Contains(got, want) {
					t.Errorf("PresentResults output missing %q\nfull output:\n%s", want, got)
				}
			}
			for _, unwanted := range tt.notWanted {
				if strings.Contains(got, unwanted) {
					t.Errorf("PresentResults output unexpectedly contains %q\nfull output:\n%s", unwanted, got)
				}
			}
		})
	}
}

func TestPresentResults_TransitiveSHAAsRefStillSuppressed(t *testing.T) {
	u, buf := newTestUI()
	report := &checks.Report{
		Workflows: []checks.WorkflowReport{{
			Path: ".github/workflows/a.yml",
			Findings: []checks.Finding{{
				WorkflowPath: ".github/workflows/a.yml",
				Category:     checks.ShaAsRef,
				Severity:     checks.SeverityWarning,
				Confidence:   checks.ConfidenceHigh,
				// Transitive: Dependency set, ActionRef nil.
				Dependency: &dep.Dependency{
					NWO: "octo/composite",
					Ref: "abc123",
					SHA: "abc1230000000000000000000000000000000000",
				},
				Detail: "transitive bare SHA",
			}},
		}},
	}
	PresentResults(u, report, true, false)

	got := buf.String()
	if strings.Contains(got, "bare SHA") {
		t.Errorf("transitive sha-as-ref should be suppressed, got:\n%s", got)
	}
}

// TestPresentResults_ParseWarningsSurface guards the per-workflow
// ParseWarnings rendering: malformed `uses:` lines must reach the
// terminal, not just JSON.
func TestPresentResults_ParseWarningsSurface(t *testing.T) {
	u, buf := newTestUI()
	report := &checks.Report{
		Workflows: []checks.WorkflowReport{{
			Path:          ".github/workflows/a.yml",
			ParseWarnings: []string{"malformed uses: at line 12"},
		}},
	}
	PresentResults(u, report, true, false)

	got := buf.String()
	if !strings.Contains(got, "malformed uses:") {
		t.Errorf("ParseWarnings missing from terminal output:\n%s", got)
	}
	if !strings.Contains(got, ".github/workflows/a.yml") {
		t.Errorf("ParseWarnings should be prefixed with workflow path:\n%s", got)
	}
}

// TestPresentResults_ExcludeCategoriesSkipsImpostor verifies that excluded
// categories are not rendered in the error findings block. This prevents
// duplication when renderInvestigationAlerts already surfaced the same
// findings (e.g. unreachable-pin).
func TestPresentResults_ExcludeCategoriesSkipsImpostor(t *testing.T) {
	var buf bytes.Buffer
	u := ui.NewPlain(&buf)
	report := &checks.Report{
		Workflows: []checks.WorkflowReport{
			{
				Path: ".github/workflows/ci.yml",
				Findings: []checks.Finding{
					{
						WorkflowPath: ".github/workflows/ci.yml",
						Category:     checks.UnreachablePin,
						Severity:     checks.SeverityError,
						Confidence:   checks.ConfidenceHigh,
						Dependency:   &dep.Dependency{NWO: "octo/action", Ref: "v1", SHA: "aaaa"},
						Detail:       "commit aaaa lockfile entry was not a prior state",
					},
					{
						WorkflowPath: ".github/workflows/ci.yml",
						Category:     checks.NotPinned,
						Severity:     checks.SeverityError,
						Confidence:   checks.ConfidenceHigh,
						Dependency:   &dep.Dependency{NWO: "actions/checkout", Ref: "v4", SHA: "bbbb"},
						Detail:       "not pinned",
					},
				},
			},
		},
	}

	PresentResults(u, report, false, false, checks.UnreachablePin)
	got := buf.String()

	if strings.Contains(got, "Pinned commit not in upstream history") {
		t.Errorf("excluded unreachable-pin should not appear:\n%s", got)
	}
	if strings.Contains(got, "lockfile entry was not a prior state") {
		t.Errorf("excluded forgery detail should not appear:\n%s", got)
	}
	// The summary line should not count the excluded category.
	if strings.Contains(got, "unreachable-pin") {
		t.Errorf("excluded category should not appear in summary:\n%s", got)
	}
}

// TestPresentResults_ExcludeKeepsOtherFindings verifies that non-excluded
// categories still render when some categories are excluded.
func TestPresentResults_ExcludeKeepsOtherFindings(t *testing.T) {
	var buf bytes.Buffer
	u := ui.NewPlain(&buf)
	report := &checks.Report{
		Workflows: []checks.WorkflowReport{
			{
				Path: ".github/workflows/ci.yml",
				Findings: []checks.Finding{
					{
						WorkflowPath: ".github/workflows/ci.yml",
						Category:     checks.UnreachablePin,
						Severity:     checks.SeverityError,
						Confidence:   checks.ConfidenceHigh,
						Dependency:   &dep.Dependency{NWO: "octo/action", Ref: "v1", SHA: "aaaa"},
						Detail:       "commit aaaa lockfile entry was not a prior state",
					},
					{
						WorkflowPath: ".github/workflows/ci.yml",
						Category:     checks.LocalAction,
						Severity:     checks.SeverityError,
						Confidence:   checks.ConfidenceHigh,
						Detail:       "local action ./my-action",
					},
				},
			},
			{
				Path: ".github/workflows/release.yml",
				Findings: []checks.Finding{{
					WorkflowPath: ".github/workflows/release.yml",
					Category:     checks.NotPinned,
					Severity:     checks.SeverityError,
					Confidence:   checks.ConfidenceHigh,
					ActionRef:    &parserlock.ActionRef{Owner: "actions", Repo: "checkout", Ref: "v4"},
					Dependency:   &dep.Dependency{NWO: "actions/checkout", Ref: "v4"},
				}},
			},
		},
	}

	PresentResults(u, report, false, false, checks.UnreachablePin, checks.NotPinned)
	got := buf.String()

	if strings.Contains(got, "Pinned commit not in upstream history") {
		t.Errorf("excluded unreachable-pin should not appear:\n%s", got)
	}
	if !strings.Contains(got, "✗ local action") {
		t.Errorf("non-excluded local-action should still appear:\n%s", got)
	}
	if !strings.Contains(got, "1 of 2 workflows failed: 1 local-action") {
		t.Errorf("excluded findings should not count as failed:\n%s", got)
	}
	if strings.Contains(got, "not-pinned") {
		t.Errorf("excluded not-pinned should not appear:\n%s", got)
	}
}

func TestPresentResults_ExcludedNotPinnedKeepsWorkflowLoadErrors(t *testing.T) {
	report := &checks.Report{
		Workflows: []checks.WorkflowReport{
			{
				Path: ".github/workflows/pinned.yml",
				Findings: []checks.Finding{{
					Category:  checks.NotPinned,
					Severity:  checks.SeverityError,
					ActionRef: &parserlock.ActionRef{Owner: "actions", Repo: "checkout", Ref: "v4"},
				}},
			},
			{
				Path: ".github/workflows/broken.yml",
				Findings: []checks.Finding{{
					Category: checks.NotPinned,
					Severity: checks.SeverityError,
					Detail:   "failed to load workflow",
				}},
			},
		},
	}
	var buf bytes.Buffer
	PresentResults(ui.NewPlain(&buf), report, false, false, checks.NotPinned)

	got := buf.String()
	if !strings.Contains(got, "1 of 2 workflows failed: 1 not-pinned") {
		t.Errorf("workflow load error should remain after remediation:\n%s", got)
	}
	if !strings.Contains(got, ".github/workflows/broken.yml") ||
		!strings.Contains(got, "failed to load workflow") {
		t.Errorf("workflow load error detail should be rendered:\n%s", got)
	}
	if strings.Contains(got, "not yet pinned") {
		t.Errorf("workflow load error should not render as a fixable warning:\n%s", got)
	}
}

func TestPresentResults_ExcludedNotPinnedDropsRemediableWorkflowErrors(t *testing.T) {
	report := &checks.Report{
		Workflows: []checks.WorkflowReport{
			{
				Path: ".github/workflows/repaired.yml",
				Findings: []checks.Finding{{
					Category:   checks.NotPinned,
					Severity:   checks.SeverityError,
					Remediable: true,
					Detail:     "failed to read dependencies",
				}},
			},
			{
				Path: ".github/workflows/blocked.yml",
				Findings: []checks.Finding{{
					Category: checks.LocalAction,
					Severity: checks.SeverityError,
					Detail:   "local action ./my-action",
				}},
			},
		},
	}
	var buf bytes.Buffer
	PresentResults(ui.NewPlain(&buf), report, false, false, checks.NotPinned)

	got := buf.String()
	if !strings.Contains(got, "1 of 2 workflows failed: 1 local-action") {
		t.Errorf("repaired dependency error should not remain after remediation:\n%s", got)
	}
	if strings.Contains(got, "not-pinned") || strings.Contains(got, "failed to read dependencies") {
		t.Errorf("repaired dependency error should be excluded:\n%s", got)
	}
}

// TestPresentResults_RepoFindingsSurface guards repo-level warnings
// (e.g. non-immutable releases) reach the terminal.
func TestPresentResults_RepoFindingsSurface(t *testing.T) {
	u, buf := newTestUI()
	report := &checks.Report{
		RepoFindings: []checks.Finding{{
			Category:   checks.NotPinned,
			Severity:   checks.SeverityWarning,
			Confidence: checks.ConfidenceHigh,
			Detail:     "repo publishes non-immutable releases",
			DocURL:     "https://example.com/docs",
		}},
	}
	PresentResults(u, report, true, false)

	got := buf.String()
	if !strings.Contains(got, "repo publishes non-immutable releases") {
		t.Errorf("RepoFindings missing from terminal output:\n%s", got)
	}
	if !strings.Contains(got, "example.com/docs") {
		t.Errorf("RepoFindings DocURL missing from terminal output:\n%s", got)
	}
}

// TestPresentReadOnlyFailures_ForgeryReachesTerminal is the regression guard
// for the --verify/--no-fix output bug: an error-level finding must reach the
// terminal even when the narration log sinks to io.Discard. PresentResults
// alone routes the error block through the discarded log, so read-only modes
// exited non-zero showing nothing about which workflow failed.
func TestPresentReadOnlyFailures_ForgeryReachesTerminal(t *testing.T) {
	u, buf := newTestUI()
	report := &checks.Report{
		Workflows: []checks.WorkflowReport{{
			Path: ".github/workflows/ci.yml",
			Findings: []checks.Finding{{
				WorkflowPath: ".github/workflows/ci.yml",
				Category:     checks.UnreachablePin,
				Severity:     checks.SeverityError,
				Confidence:   checks.ConfidenceHigh,
				Dependency:   &dep.Dependency{NWO: "octo/action", Ref: "main", SHA: "aaaa"},
				Detail:       "pinned aaaa is not an ancestor of bbbb",
				Remediation:  "investigate immediately",
				DocURL:       "https://example.com/docs",
			}},
		}},
	}

	// Prove the pre-fix behavior: PresentResults leaves the error block in
	// the discarded log, so nothing surfaces.
	PresentResults(u, report, false, false)
	if strings.Contains(buf.String(), "Pinned commit not in upstream history") {
		t.Fatalf("guard invalid: PresentResults unexpectedly surfaced the error block:\n%s", buf.String())
	}

	hasFixable := PresentReadOnlyFailures(u, report)
	got := buf.String()

	for _, want := range []string{
		"1 of 1 workflow failed",
		"Pinned commit not in upstream history",
		"octo/action@main",
		"pinned aaaa is not an ancestor of bbbb",
		"investigate immediately",
		".github/workflows/ci.yml",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("PresentReadOnlyFailures output missing %q\nfull output:\n%s", want, got)
		}
	}
	if hasFixable {
		t.Errorf("forgery is investigation-only; hasFixable should be false")
	}
}

// TestPresentReadOnlyFailures_FixableReported verifies an auto-fixable
// error-level finding (a directly-used unpinned action) surfaces and reports
// hasFixable=true so the caller can honestly print the re-run hint.
func TestPresentReadOnlyFailures_FixableReported(t *testing.T) {
	u, buf := newTestUI()
	report := &checks.Report{
		Workflows: []checks.WorkflowReport{{
			Path: ".github/workflows/ci.yml",
			Findings: []checks.Finding{{
				WorkflowPath: ".github/workflows/ci.yml",
				Category:     checks.NotPinned,
				Severity:     checks.SeverityError,
				Confidence:   checks.ConfidenceHigh,
				ActionRef:    &parserlock.ActionRef{Owner: "actions", Repo: "checkout", Ref: "v4"},
				Dependency:   &dep.Dependency{NWO: "actions/checkout", Ref: "v4"},
				Detail:       "no lockfile entry",
			}},
		}},
	}

	hasFixable := PresentReadOnlyFailures(u, report)
	got := buf.String()

	if !strings.Contains(got, "actions/checkout@v4") {
		t.Errorf("expected failing dep in output:\n%s", got)
	}
	if !hasFixable {
		t.Errorf("not-pinned is auto-fixable; hasFixable should be true")
	}
}

// TestPresentReadOnlyFailures_ValidReportSilent verifies a clean report
// produces no output and reports nothing fixable.
func TestPresentReadOnlyFailures_ValidReportSilent(t *testing.T) {
	u, buf := newTestUI()
	report := &checks.Report{
		Workflows: []checks.WorkflowReport{{
			Path: ".github/workflows/ci.yml",
			Findings: []checks.Finding{{
				WorkflowPath: ".github/workflows/ci.yml",
				Category:     checks.Valid,
				Severity:     checks.SeverityOK,
				Confidence:   checks.ConfidenceHigh,
			}},
		}},
	}

	if hasFixable := PresentReadOnlyFailures(u, report); hasFixable {
		t.Errorf("valid report should report nothing fixable")
	}
	if got := buf.String(); got != "" {
		t.Errorf("valid report should produce no output, got:\n%s", got)
	}
}

// TestPresentReadOnlyFailures_LocalActionNotFixable guards the inference bug
// where hasFixable was derived from !IsAlertedCategory: local-action is an
// error the tool can't auto-remediate, so the "Re-run without --no-fix" hint
// must not be offered for it.
func TestPresentReadOnlyFailures_LocalActionNotFixable(t *testing.T) {
	u, buf := newTestUI()
	report := &checks.Report{
		Workflows: []checks.WorkflowReport{{
			Path: ".github/workflows/ci.yml",
			Findings: []checks.Finding{{
				WorkflowPath: ".github/workflows/ci.yml",
				Category:     checks.LocalAction,
				Severity:     checks.SeverityError,
				Confidence:   checks.ConfidenceHigh,
				Detail:       "local action ./my-action",
			}},
		}},
	}

	hasFixable := PresentReadOnlyFailures(u, report)
	if got := buf.String(); !strings.Contains(got, "local action ./my-action") {
		t.Errorf("expected local-action detail in output:\n%s", got)
	}
	if hasFixable {
		t.Errorf("local-action is not auto-fixable; hasFixable should be false")
	}
}

// TestPresentReadOnlyFailures_MissingRefNotFixable keeps the "Re-run
// without --no-fix" hint off a ref that doesn't exist: it shares the fixable
// not-pinned category, but a re-run can't create the ref.
func TestPresentReadOnlyFailures_MissingRefNotFixable(t *testing.T) {
	u, _ := newTestUI()
	report := &checks.Report{
		Workflows: []checks.WorkflowReport{{
			Path: ".github/workflows/ci.yml",
			Findings: []checks.Finding{{
				WorkflowPath: ".github/workflows/ci.yml",
				Category:     checks.NotPinned,
				Severity:     checks.SeverityError,
				Confidence:   checks.ConfidenceHigh,
				Dependency:   &dep.Dependency{NWO: "actions/checkout", Ref: "v999.0.0"},
				Detail:       "ref v999.0.0 does not exist in actions/checkout",
				RefMissing:   true,
			}},
		}},
	}

	if PresentReadOnlyFailures(u, report) {
		t.Error("a missing ref is not auto-fixable; hasFixable should be false")
	}
}

// TestPresentReadOnlyFailures_UnreachablePinShowsReleases verifies the
// read-only renderer includes the upstream releases link for an
// unreachable-pin finding, matching the detailed (fix-mode) renderer.
func TestPresentReadOnlyFailures_UnreachablePinShowsReleases(t *testing.T) {
	u, buf := newTestUI()
	report := &checks.Report{
		Workflows: []checks.WorkflowReport{{
			Path: ".github/workflows/ci.yml",
			Findings: []checks.Finding{{
				WorkflowPath: ".github/workflows/ci.yml",
				Category:     checks.UnreachablePin,
				Severity:     checks.SeverityError,
				Confidence:   checks.ConfidenceHigh,
				Dependency:   &dep.Dependency{NWO: "octo/action", Ref: "main", SHA: "aaaa"},
				Detail:       "pinned aaaa is not an ancestor of bbbb",
			}},
		}},
	}

	PresentReadOnlyFailures(u, report)
	if got := buf.String(); !strings.Contains(got, "https://github.com/octo/action/releases") {
		t.Errorf("expected upstream releases link in read-only output:\n%s", got)
	}
}

func TestFindingIconFollowsSeverity(t *testing.T) {
	tests := []struct {
		name     string
		severity checks.Severity
		want     string
	}{
		{name: "error is a cross", severity: checks.SeverityError, want: "✗ Repository renamed"},
		{name: "warning is a bang", severity: checks.SeverityWarning, want: "! Repository renamed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := checks.Finding{Category: checks.RepoRenamed, Severity: tt.severity, Detail: "moved"}

			var buf bytes.Buffer
			renderFindingDetail(ui.NewPlain(&buf), f, "octo/a@v1")
			if !strings.Contains(buf.String(), tt.want) {
				t.Errorf("fix-mode row = %q, want %q", buf.String(), tt.want)
			}

			buf.Reset()
			renderTermFindingDetail(ui.NewPlain(&buf), f, "octo/a@v1")
			if !strings.Contains(buf.String(), tt.want) {
				t.Errorf("read-only row = %q, want %q", buf.String(), tt.want)
			}
		})
	}
}

func TestPresentReadOnlyFailures_HintOnlyWhenPlainRunCanFix(t *testing.T) {
	stale := checks.Finding{Category: checks.Stale, Severity: checks.SeverityError, Detail: "unused"}
	unreachable := checks.Finding{
		Category:   checks.UnreachablePin,
		Severity:   checks.SeverityError,
		Dependency: &dep.Dependency{NWO: "actions/checkout", Ref: "v4.2.2", SHA: "1111"},
		Detail:     "not in history",
	}
	tests := []struct {
		name     string
		findings []checks.Finding
		want     bool
	}{
		{name: "fixable alone offers a re-run", findings: []checks.Finding{stale}, want: true},
		{name: "unfixable error suppresses the re-run hint", findings: []checks.Finding{stale, unreachable}, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			report := &checks.Report{Workflows: []checks.WorkflowReport{{Path: ".github/workflows/ci.yml", Findings: tt.findings}}}
			var buf bytes.Buffer
			if got := PresentReadOnlyFailures(ui.NewPlain(&buf), report); got != tt.want {
				t.Errorf("hasFixable = %v, want %v\n%s", got, tt.want, buf.String())
			}
		})
	}
}

func retryableFinding(nwo, ref string, reason string, rateLimited bool, at time.Time) checks.Finding {
	return checks.Finding{
		WorkflowPath: ".github/workflows/ci.yml",
		Category:     checks.ReachabilityUnknown,
		Severity:     checks.SeverityError,
		Confidence:   checks.ConfidenceLow,
		Dependency:   &dep.Dependency{NWO: nwo, Ref: ref},
		Detail:       "could not verify locked " + nwo,
		Retryable:    true,
		RetryReason:  reason,
		RateLimited:  rateLimited,
		RetryAt:      at,
	}
}

// TestPresentReadOnlyFailures_GroupsRetryable: checks GitHub didn't answer
// share one block, not one per pin, and the verdict line counts them apart
// from definitive errors.
func TestPresentReadOnlyFailures_GroupsRetryable(t *testing.T) {
	reset := time.Date(2026, 1, 2, 14, 5, 0, 0, time.UTC)
	t.Cleanup(func() { now = time.Now })
	now = func() time.Time { return reset.Add(-38 * time.Minute) }
	refused := "GitHub API unreachable (connection refused)"
	limited := "GitHub API rate limit exhausted"
	forged := checks.Finding{
		WorkflowPath: ".github/workflows/ci.yml",
		Category:     checks.UnreachablePin,
		Severity:     checks.SeverityError,
		Dependency:   &dep.Dependency{NWO: "octo/forged", Ref: "v1"},
		Detail:       "locked commit is not in octo/forged's history",
	}
	tests := []struct {
		name     string
		findings []checks.Finding
		want     []string
		unwanted []string
	}{
		{
			name: "outage",
			findings: []checks.Finding{
				retryableFinding("actions/checkout", "v4", refused, false, time.Time{}),
				retryableFinding("actions/setup-go", "v5", refused, false, time.Time{}),
				retryableFinding("actions/checkout", "v4", refused, false, time.Time{}),
			},
			want: []string{
				"Couldn't verify 2 pins\n",
				"  actions/checkout@v4, actions/setup-go@v5\n",
				"  ↳ GitHub API unreachable (connection refused); re-run this check once it is reachable. Nothing is known to be wrong with the lockfile.\n",
			},
			unwanted: []string{"workflow failed", "could not verify locked"},
		},
		{
			name: "rate limit",
			findings: []checks.Finding{
				retryableFinding("actions/checkout", "v4", limited, true, reset),
				retryableFinding("actions/cache", "v4", limited, true, reset),
			},
			want: []string{
				"Couldn't verify 2 pins: GitHub API rate limit exhausted until 14:05 UTC (in 38m)\n",
				"  ↳ re-run after 14:05 UTC, or use a token with a higher rate limit\n",
			},
		},
		{
			name: "partial failure",
			findings: []checks.Finding{
				forged,
				retryableFinding("actions/checkout", "v4", refused, false, time.Time{}),
				retryableFinding("actions/setup-go", "v5", refused, false, time.Time{}),
			},
			want: []string{
				"1 of 1 workflow failed: 1 error, 2 pins couldn't be verified\n",
				"octo/forged@v1",
				"Couldn't verify 2 pins\n",
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			u, buf := newTestUI()
			report := &checks.Report{Workflows: []checks.WorkflowReport{{Path: ".github/workflows/ci.yml", Findings: tt.findings}}}
			PresentReadOnlyFailures(u, report)
			got := buf.String()
			for _, want := range tt.want {
				assert.Contains(t, got, want)
			}
			for _, unwanted := range tt.unwanted {
				assert.NotContains(t, got, unwanted)
			}
		})
	}
}
