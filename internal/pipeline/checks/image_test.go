package checks

import (
	"testing"

	"github.com/github/gh-actions-lock/internal/uses"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAddContainerImage(t *testing.T) {
	t.Run("one finding per image naming every use", func(t *testing.T) {
		wr := WorkflowReport{Path: "ci.yml"}
		AddContainerImage(&wr, uses.Image{Name: "alpine:3", Where: "job build"})
		AddContainerImage(&wr, uses.Image{Name: "alpine:3", Where: "job build"})
		AddContainerImage(&wr, uses.Image{Name: "alpine:3", Where: "o/a@v1", Action: "o/a@v1"})
		AddContainerImage(&wr, uses.Image{Name: "redis:7", Where: "job build service redis"})

		require.Len(t, wr.Findings, 2)
		f := wr.Findings[0]
		assert.Equal(t, ContainerImage, f.Category)
		assert.Equal(t, SeverityWarning, f.Severity)
		assert.True(t, f.IsValid())
		assert.Equal(t, "container image alpine:3 is not pinned by digest; used by job build, o/a@v1", f.Detail)
		assert.Contains(t, f.Remediation, "pin it as alpine:3@sha256:")
		assert.Empty(t, f.ParentNWO, "the repository also uses it")
	})

	t.Run("a single remote action is required_by and owns the fix", func(t *testing.T) {
		wr := WorkflowReport{Path: "ci.yml"}
		AddContainerImage(&wr, uses.Image{Name: "ghcr.io/o/a:1", Where: "o/a@v1", Action: "o/a@v1"})

		f := wr.Findings[0]
		assert.Equal(t, "o/a@v1", f.ParentNWO)
		assert.Contains(t, f.Remediation, "only the maintainer of o/a@v1")
		assert.False(t, wr.NeedsAttention())
	})

	t.Run("an expression image says it's chosen at runtime", func(t *testing.T) {
		wr := WorkflowReport{Path: "ci.yml"}
		AddContainerImage(&wr, uses.Image{Name: "${{ matrix.image }}", Kind: uses.ImageExpression, Where: "job build container"})
		AddContainerImage(&wr, uses.Image{Name: "${{ matrix.image }}", Kind: uses.ImageExpression, Where: "job test container"})

		require.Len(t, wr.Findings, 1)
		f := wr.Findings[0]
		assert.Equal(t, "container image `${{ matrix.image }}` is chosen at runtime, so the lockfile can't check it; used by job build container, job test container", f.Detail)
		assert.Contains(t, f.Remediation, "pin each value of the matrix or input by digest")
		assert.True(t, f.IsValid())
	})

	t.Run("each Dockerfile-built action is its own finding", func(t *testing.T) {
		wr := WorkflowReport{Path: "ci.yml"}
		AddContainerImage(&wr, uses.Image{Name: "Dockerfile", Kind: uses.ImageBuild, Where: "o/a@v1", Action: "o/a@v1"})
		AddContainerImage(&wr, uses.Image{Name: "Dockerfile", Kind: uses.ImageBuild, Where: "o/b@v1", Action: "o/b@v1"})

		require.Len(t, wr.Findings, 2)
		f := wr.Findings[0]
		assert.Equal(t, "o/a@v1 builds its container from Dockerfile; the lockfile doesn't cover its base image; used by o/a@v1", f.Detail)
		assert.Equal(t, "only the maintainer of o/a@v1 can pin its base image by digest", f.Remediation)
		assert.Equal(t, "o/a@v1", f.ParentNWO)
	})
}
