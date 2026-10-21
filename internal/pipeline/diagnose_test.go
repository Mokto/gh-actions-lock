package pipeline

import (
	"context"
	"testing"

	"github.com/github/gh-actions-lock/internal/dockerimage"
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
	assert.Equal(t, checks.SeverityInfo, wr.Findings[0].Severity)
	assert.Contains(t, wr.Findings[0].Detail, "never followed or locked")
	assert.True(t, wr.IsValid())
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
	assert.Equal(t, checks.SeverityInfo, wr.Findings[0].Severity)
	assert.Contains(t, wr.Findings[0].Detail, "uses: dependencies are not covered")
	assert.Empty(t, wr.Findings[0].Remediation)
	assert.True(t, wr.IsValid())
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

func containerImageFindings(wr checks.WorkflowReport) []checks.Finding {
	var images []checks.Finding
	for _, f := range wr.Findings {
		if f.Category == checks.ContainerImage {
			images = append(images, f)
		}
	}
	return images
}

func TestDiagnoseOneParsed_ContainerImages(t *testing.T) {
	tests := []struct {
		name      string
		images    []dockerimage.Image
		details   []string
		locations [][]string
	}{
		{
			name:    "tag and untagged warn; digest does not",
			images:  []dockerimage.Image{{Ref: "docker://alpine:3.18"}, {Ref: "docker://alpine"}, {Ref: "docker://alpine@sha256:0"}},
			details: []string{"alpine:3.18 is not covered by the actions lockfile; container image tags are mutable", "alpine is not covered by the actions lockfile; an untagged image pulls the mutable latest tag"},
		},
		{
			name: "expressions and empty names produce nothing",
			images: []dockerimage.Image{
				{Ref: "docker://${{ env.IMAGE }}"},
				{Ref: "${{ matrix.image }}", Location: "jobs.build.container"},
				{Ref: "${{ inputs.img }}", Local: "$/actions/run"},
				{Ref: "docker://"},
			},
		},
		{
			name: "one image across jobs is one finding with every location",
			images: []dockerimage.Image{
				{Ref: "node:20", Location: "jobs.build.container"},
				{Ref: "node:20", Location: "jobs.test.container"},
				{Ref: "node:20", Location: "jobs.test.container"},
				{Ref: "postgres@sha256:0", Location: "jobs.test.services.db"},
			},
			details:   []string{"node:20 is not covered by the actions lockfile; container image tags are mutable"},
			locations: [][]string{{"jobs.build.container", "jobs.test.container"}},
		},
		{
			name: "a docker:// step and a job container are one image",
			images: []dockerimage.Image{
				{Ref: "docker://node:20"},
				{Ref: "node:20", Location: "jobs.build.container"},
				{Ref: " docker://node:20 ", Local: "$/actions/lint"},
			},
			details: []string{
				"node:20 is not covered by the actions lockfile; container image tags are mutable",
				"node:20 (pulled by $/actions/lint) is not covered by the actions lockfile; container image tags are mutable",
			},
			locations: [][]string{{"jobs.build.container"}},
		},
		{
			name:    "own Dockerfile action names the action without a maintainer",
			images:  []dockerimage.Image{{Ref: "Dockerfile", Local: "$/actions/build"}},
			details: []string{"Dockerfile (built by $/actions/build) is not covered by the actions lockfile; the image and its base images are built on the runner at run time"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pw := checks.ParsedWorkflow{Path: ".github/workflows/ci.yml", ContainerImages: tt.images}
			wr := diagnoseOneParsed(context.Background(), pw, nil, nil, nil)

			images := containerImageFindings(wr)
			var details [][]string
			var got []string
			for _, f := range images {
				got = append(got, f.Detail)
				if f.Locations != nil {
					details = append(details, f.Locations)
				}
				assert.Equal(t, checks.SeverityWarning, f.Severity)
				assert.Equal(t, checks.ConfidenceHigh, f.Confidence)
				assert.Empty(t, f.ParentNWO)
				assert.NotContains(t, f.Remediation, "maintainer", "the user controls these images")
				assert.True(t, f.IsWarning())
			}
			assert.Equal(t, tt.details, got)
			assert.Equal(t, tt.locations, details)
			assert.True(t, wr.IsValid())
			assert.False(t, wr.NeedsAttention(), "a container image warning must not trigger a re-pin")
		})
	}
}
