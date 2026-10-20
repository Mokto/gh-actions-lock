package pipeline

import (
	"context"
	"testing"

	parserlock "github.com/github/actions-lockfile/go/pkg/lockfile"
	"github.com/github/gh-actions-lock/internal/dep"

	"github.com/github/gh-actions-lock/internal/lockfile"
	"github.com/github/gh-actions-lock/internal/pipeline/checks"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type noopMeta struct{}

func (noopMeta) RepoIDs(context.Context, string, string, string) (int64, int64, error) {
	return 0, 0, nil
}

func TestDiagnoseOneParsed_LocalAction_NotOnboarded(t *testing.T) {
	pw := checks.ParsedWorkflow{
		Path:       ".github/workflows/ci.yml",
		LocalPaths: []string{"./my-local-action"},
	}
	wr := diagnoseOneParsed(context.Background(), pw, nil, nil, nil)

	assert.Len(t, wr.Findings, 1)
	assert.Equal(t, checks.LocalAction, wr.Findings[0].Category)
	assert.Equal(t, checks.SeverityWarning, wr.Findings[0].Severity)
}

func TestDiagnoseOneParsed_LocalAction_AlreadyOnboarded(t *testing.T) {
	dir := t.TempDir()
	store, err := lockfile.LoadState(dir, noopMeta{})
	require.NoError(t, err)

	wfKey := ".github/workflows/ci.yml"
	require.NoError(t, store.Set(context.Background(), wfKey, nil, nil, nil))

	pw := checks.ParsedWorkflow{
		Path:       wfKey,
		LocalPaths: []string{"./my-local-action"},
	}
	wr := diagnoseOneParsed(context.Background(), pw, nil, store, nil)

	assert.Len(t, wr.Findings, 1)
	assert.Equal(t, checks.LocalAction, wr.Findings[0].Category)
	assert.Equal(t, checks.SeverityError, wr.Findings[0].Severity)
	assert.Contains(t, wr.Findings[0].Remediation, "remove")
}

func TestDiagnoseOneParsed_SelfRepositoryOnly(t *testing.T) {
	pw := checks.ParsedWorkflow{
		Path:               ".github/workflows/ci.yml",
		SelfRepositoryRefs: []string{"$/actions/foo"},
	}
	wr := diagnoseOneParsed(context.Background(), pw, nil, nil, nil)

	require.Len(t, wr.Findings, 1)
	assert.Equal(t, checks.SelfRepositoryAction, wr.Findings[0].Category)
	assert.Equal(t, checks.SeverityInfo, wr.Findings[0].Severity)
}

func TestDiagnoseOneParsed_SelfRepositoryReusableWorkflow(t *testing.T) {
	pw := checks.ParsedWorkflow{
		Path:               ".github/workflows/ci.yml",
		SelfRepositoryRefs: []string{"$/.github/workflows/reusable.yml"},
	}
	wr := diagnoseOneParsed(context.Background(), pw, nil, nil, nil)

	require.Len(t, wr.Findings, 1)
	assert.Equal(t, checks.SelfRepositoryAction, wr.Findings[0].Category)
	assert.Equal(t, checks.SeverityInfo, wr.Findings[0].Severity)
}

func TestDiagnoseOneParsed_InvalidSelfRepositoryRef(t *testing.T) {
	pw := checks.ParsedWorkflow{
		Path:                  ".github/workflows/ci.yml",
		SelfRepositoryRefErrs: []string{"$/actions/foo@v1"},
	}
	wr := diagnoseOneParsed(context.Background(), pw, nil, nil, nil)

	require.Len(t, wr.Findings, 1)
	assert.Equal(t, checks.InvalidSelfRepositoryRef, wr.Findings[0].Category)
	assert.Equal(t, checks.SeverityError, wr.Findings[0].Severity)
	assert.Contains(t, wr.Findings[0].Remediation, "@ref")
}

func TestDiagnoseOneParsed_InvalidSelfRepositoryRefWithLocalAction(t *testing.T) {
	pw := checks.ParsedWorkflow{
		Path:                  ".github/workflows/ci.yml",
		LocalPaths:            []string{"./local"},
		SelfRepositoryRefErrs: []string{"$/actions/foo@v1"},
	}
	wr := diagnoseOneParsed(context.Background(), pw, nil, nil, nil)

	require.Len(t, wr.Findings, 2)
	assert.Equal(t, checks.InvalidSelfRepositoryRef, wr.Findings[0].Category)
	assert.Equal(t, checks.LocalAction, wr.Findings[1].Category)
	assert.False(t, wr.IsValid())
}

func TestDiagnoseOneParsed_SelfRepositoryResolutionError(t *testing.T) {
	pw := checks.ParsedWorkflow{
		Path:                         ".github/workflows/ci.yml",
		SelfRepositoryResolutionErrs: []string{"can't inspect self repository action $/missing"},
	}
	wr := diagnoseOneParsed(context.Background(), pw, nil, nil, nil)

	require.Len(t, wr.Findings, 1)
	assert.Equal(t, checks.InvalidSelfRepositoryRef, wr.Findings[0].Category)
	assert.Equal(t, checks.SeverityError, wr.Findings[0].Severity)
	assert.False(t, wr.IsValid())
}

func TestIndexDeps(t *testing.T) {
	deps := []dep.Dependency{
		{NWO: "actions/checkout", Ref: "v4"},
		{NWO: "actions/setup-node", Ref: "v3"},
	}
	got := indexDeps(deps)

	assert.Len(t, got, 2)
	assert.Equal(t, "actions/checkout", got["actions/checkout@v4"].NWO)
	assert.Equal(t, "actions/setup-node", got["actions/setup-node@v3"].NWO)
}

func TestIndexDeps_LastWins(t *testing.T) {
	// Two deps sharing a key (NWO@Ref) collapse to one entry; the later wins.
	deps := []dep.Dependency{
		{NWO: "actions/checkout", Ref: "v4", SHA: "aaa"},
		{NWO: "actions/checkout", Ref: "v4", SHA: "bbb"},
	}
	got := indexDeps(deps)

	assert.Len(t, got, 1)
	assert.Equal(t, "bbb", got["actions/checkout@v4"].SHA)
}

func TestPrecheckWorkflow_NotPinnedRemediability(t *testing.T) {
	for _, tt := range []struct {
		name           string
		workflow       checks.ParsedWorkflow
		wantRemediable bool
	}{
		{
			name:     "load error",
			workflow: checks.ParsedWorkflow{Path: "broken.yml", LoadErr: assert.AnError},
		},
		{
			name: "dependency inventory error",
			workflow: checks.ParsedWorkflow{
				Path:    "repairable.yml",
				Refs:    []parserlock.ActionRef{{Owner: "actions", Repo: "checkout", Ref: "v4"}},
				DepsErr: assert.AnError,
			},
			wantRemediable: true,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			report, terminal := precheckWorkflow(tt.workflow, nil)
			assert.True(t, terminal)
			assert.Len(t, report.Findings, 1)
			assert.Equal(t, tt.wantRemediable, report.Findings[0].IsRemediableNotPinned())
		})
	}
}

func TestHasIssues(t *testing.T) {
	tests := []struct {
		name     string
		findings []checks.Finding
		want     bool
	}{
		{
			name:     "no findings",
			findings: nil,
			want:     false,
		},
		{
			name: "error severity is always an issue",
			findings: []checks.Finding{
				{Category: checks.UnreachablePin, Severity: checks.SeverityError},
			},
			want: true,
		},
		{
			name: "valid finding is not an issue",
			findings: []checks.Finding{
				{Category: checks.Valid, Severity: checks.SeverityOK},
			},
			want: false,
		},
		{
			name: "inconclusive warning is not blocking",
			findings: []checks.Finding{
				{Category: checks.ReachabilityUnknown, Severity: checks.SeverityWarning},
			},
			want: false,
		},
		{
			name: "not-pinned warning is an issue",
			findings: []checks.Finding{
				{Category: checks.NotPinned, Severity: checks.SeverityWarning},
			},
			want: true,
		},
		{
			name: "run-only warning is not an issue",
			findings: []checks.Finding{
				{Category: checks.RunOnly, Severity: checks.SeverityWarning},
			},
			want: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, hasIssues(tt.findings))
		})
	}
}
