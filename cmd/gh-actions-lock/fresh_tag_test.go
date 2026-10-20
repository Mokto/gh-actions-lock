package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/github/gh-actions-lock/internal/ghapi/httpmock"
	"github.com/github/gh-actions-lock/internal/pin"
	"github.com/github/gh-actions-lock/internal/pipeline/checks"
	"github.com/github/gh-actions-lock/internal/tag"
)

// TestInjectFreshTagFindings_PositiveCooldownSkips proves the per-entry gate:
// an action with a positive effective cooldown is not nudged, because the
// resolver's cooldown filter already gated fresh tags for it.
func TestInjectFreshTagFindings_PositiveCooldownSkips(t *testing.T) {
	fresh := time.Now().Add(-24 * time.Hour).Format(time.RFC3339)
	reg := &httpmock.Registry{}
	reg.Register(
		httpmock.REST("GET", `(?i)repos/actions/checkout/releases`),
		httpmock.JSONResponse([]map[string]any{
			{"tag_name": "v4", "published_at": fresh, "immutable": false},
		}),
	)
	tagger := tag.NewListerForTest(t, reg)

	record := &pin.Record{Entries: []pin.Entry{
		{NWO: "actions/checkout", Ref: "v4", SHA: "s1", Resolution: pin.Pinned},
	}}
	report := &checks.Report{}
	cfg := tag.CooldownConfig{RepoOverrides: map[string]int{"actions/checkout": 7}}
	injectFreshTagFindings(context.Background(), report, record, tagger, cfg)

	if len(report.RepoFindings) != 0 {
		t.Fatalf("RepoFindings = %d, want 0 (positive cooldown skips the nudge)", len(report.RepoFindings))
	}
}

func TestFreshTagFinding(t *testing.T) {
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	iso := func(d time.Duration) string { return now.Add(-d).Format(time.RFC3339) }

	tests := []struct {
		name   string
		iso    string
		wantOK bool
	}{
		{"fresh", iso(24 * time.Hour), true},
		{"just under window", iso(freshTagWindow - time.Hour), true},
		{"exactly at window", iso(freshTagWindow), false},
		{"old", iso(30 * 24 * time.Hour), false},
		{"empty", "", false},
		{"unparseable", "not-a-date", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f, ok := freshTagFinding("actions/checkout", "v4.2.1", tt.iso, now)
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tt.wantOK)
			}
			if ok {
				if f.Category != checks.FreshTag || f.Severity != checks.SeverityInfo {
					t.Errorf("finding category/severity = %s/%s", f.Category, f.Severity)
				}
			}
		})
	}
}

func TestAppendCooldownConfigFindings(t *testing.T) {
	report := &checks.Report{}
	appendCooldownConfigFindings(report, []string{"a", "b"})
	if len(report.RepoFindings) != 2 {
		t.Fatalf("RepoFindings = %d, want 2", len(report.RepoFindings))
	}
	if report.RepoFindings[0].Category != checks.CooldownConfigIgnored {
		t.Errorf("category = %s, want cooldown-config-ignored", report.RepoFindings[0].Category)
	}
	appendCooldownConfigFindings(report, nil)
	if len(report.RepoFindings) != 2 {
		t.Errorf("nil warnings must add nothing; got %d", len(report.RepoFindings))
	}
}

// TestInjectFreshTagFindings covers the resolution gate: a Pinned entry (an
// initial pin or an update that changed the SHA) fires the fresh-tag nudge,
// while a Verified entry (unchanged re-run) stays quiet.
func TestInjectFreshTagFindings(t *testing.T) {
	fresh := time.Now().Add(-24 * time.Hour).Format(time.RFC3339)
	reg := &httpmock.Registry{}
	reg.Register(
		httpmock.REST("GET", `(?i)repos/actions/checkout/tags`),
		httpmock.JSONResponse([]map[string]any{
			{"name": "v4", "commit": map[string]any{"sha": "abc123"}},
		}),
	)
	reg.Register(
		httpmock.REST("GET", `(?i)repos/actions/checkout/releases`),
		httpmock.JSONResponse([]map[string]any{
			{"tag_name": "v4", "published_at": fresh, "immutable": false},
		}),
	)
	reg.Register(
		httpmock.REST("GET", `(?i)repos/actions/checkout$`),
		httpmock.JSONResponse(map[string]any{
			"default_branch": "main", "visibility": "public",
			"pushed_at": "2024-01-01T00:00:00Z",
		}),
	)
	tagger := tag.NewListerForTest(t, reg)

	record := &pin.Record{Entries: []pin.Entry{
		{NWO: "actions/checkout", Ref: "v4", SHA: "s1", Resolution: pin.Pinned},
		{NWO: "actions/setup-go", Ref: "v5", SHA: "s2", Resolution: pin.Verified},
	}}
	report := &checks.Report{}
	injectFreshTagFindings(context.Background(), report, record, tagger, tag.CooldownConfig{})

	if len(report.RepoFindings) != 1 {
		t.Fatalf("RepoFindings = %d, want 1 (Pinned fires, Verified quiet)", len(report.RepoFindings))
	}
	if got := report.RepoFindings[0]; got.Category != checks.FreshTag {
		t.Errorf("category = %s, want fresh-tag", got.Category)
	}
}

// writeRepoDependabot writes a .github/dependabot.yml under a fresh temp dir
// and returns the dir (a stand-in repo root).
func writeRepoDependabot(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	gh := filepath.Join(dir, ".github")
	if err := os.MkdirAll(gh, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(gh, "dependabot.yml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestResolveCooldown_DependabotWins(t *testing.T) {
	// Both sources present: Dependabot is gospel, the config file is ignored.
	writeUserFileConfig(t, "cooldown_days: 9\n")
	dir := writeRepoDependabot(t, `
version: 2
updates:
  - package-ecosystem: "github-actions"
    cooldown:
      default-days: 5
`)
	cfg, configured, _ := ResolveCooldown(dir)
	if !configured || cfg.DefaultDays != 5 {
		t.Fatalf("got cfg=%+v configured=%v, want DefaultDays 5 (Dependabot wins)", cfg, configured)
	}
}

func TestResolveCooldown_FileFallback(t *testing.T) {
	// No Dependabot cooldown -> fall back to the user config file.
	writeUserFileConfig(t, "cooldown_days: 9\n")
	cfg, configured, _ := ResolveCooldown(t.TempDir())
	if !configured || cfg.DefaultDays != 9 {
		t.Fatalf("got cfg=%+v configured=%v, want DefaultDays 9 (file fallback)", cfg, configured)
	}
}

func TestResolveCooldown_NeitherIsUnconfigured(t *testing.T) {
	// No Dependabot, no config file -> unconfigured, so the caller applies the
	// global fresh-tag warn rather than a silent filter.
	t.Setenv("GH_ACTIONS_LOCK_CONFIG", filepath.Join(t.TempDir(), "none.yml"))
	cfg, configured, _ := ResolveCooldown(t.TempDir())
	if configured || cfg.DefaultDays != 0 {
		t.Fatalf("got cfg=%+v configured=%v, want empty / false", cfg, configured)
	}
}

func TestResolveCooldown_MalformedDependabotFallsBackAndWarns(t *testing.T) {
	// Malformed Dependabot still surfaces a warning but falls through to the
	// config file rather than blocking.
	writeUserFileConfig(t, "cooldown_days: 6\n")
	dir := writeRepoDependabot(t, "updates: [ this is : broken\n")
	cfg, configured, warnings := ResolveCooldown(dir)
	if !configured || cfg.DefaultDays != 6 {
		t.Errorf("got cfg=%+v configured=%v, want DefaultDays 6", cfg, configured)
	}
	if len(warnings) != 1 {
		t.Errorf("warnings = %v, want 1 (malformed dependabot)", warnings)
	}
}

func TestResolveCooldown_DependabotZeroCannotDowngradeFile(t *testing.T) {
	// A repo's default-days: 0 must NOT override the operator's stricter file
	// policy — it falls through instead of silently disabling cooldown.
	writeUserFileConfig(t, "cooldown_days: 14\n")
	dir := writeRepoDependabot(t, `
version: 2
updates:
  - package-ecosystem: "github-actions"
    cooldown:
      default-days: 0
`)
	cfg, configured, _ := ResolveCooldown(dir)
	if !configured || cfg.DefaultDays != 14 {
		t.Fatalf("got cfg=%+v configured=%v, want DefaultDays 14 (file, not the repo's 0)", cfg, configured)
	}
}

func TestResolveCooldown_DependabotZeroAloneIsUnconfigured(t *testing.T) {
	// default-days: 0 with no file policy resolves to unconfigured, so the
	// fresh-tag warn fires rather than a silent no-filter.
	t.Setenv("GH_ACTIONS_LOCK_CONFIG", filepath.Join(t.TempDir(), "none.yml"))
	dir := writeRepoDependabot(t, `
version: 2
updates:
  - package-ecosystem: "github-actions"
    cooldown:
      default-days: 0
`)
	if _, configured, _ := ResolveCooldown(dir); configured {
		t.Error("configured = true, want false (default-days 0 is not a policy)")
	}
}

func TestResolveCooldown_UnsupportedKeysOnlyFallsThroughButWarns(t *testing.T) {
	// A cooldown block with only unsupported keys (no default-days) applies no
	// filter, falls through to the file policy, and still surfaces the warning.
	writeUserFileConfig(t, "cooldown_days: 8\n")
	dir := writeRepoDependabot(t, `
version: 2
updates:
  - package-ecosystem: "github-actions"
    cooldown:
      semver-major-days: 30
`)
	cfg, configured, warnings := ResolveCooldown(dir)
	if !configured || cfg.DefaultDays != 8 {
		t.Errorf("got cfg=%+v configured=%v, want DefaultDays 8 (file)", cfg, configured)
	}
	if len(warnings) != 1 {
		t.Errorf("warnings = %v, want 1 (unsupported semver key)", warnings)
	}
}

// writeUserFileConfig points GH_ACTIONS_LOCK_CONFIG at a temp config.yml.
func writeUserFileConfig(t *testing.T, body string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yml")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GH_ACTIONS_LOCK_CONFIG", path)
}
