package workflowfile

import (
	"testing"

	"github.com/github/gh-actions-lock/internal/uses"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestContainerImages(t *testing.T) {
	f, err := Parse("ci.yml", []byte(`
jobs:
  build:
    runs-on: ubuntu-latest
    container: node:20
    services:
      redis:
        image: redis:7
      db: postgres@sha256:abc
      dyn:
        image: ${{ matrix.image }}
    steps:
      - uses: docker://alpine:3.18
      - uses: docker://alpine@sha256:0123
      - uses: actions/checkout@v4
      - parallel:
          - uses: docker://busybox
  test:
    runs-on: ubuntu-latest
    container:
      image: node:20
`))
	require.NoError(t, err)

	assert.Equal(t, []uses.Image{
		{Name: "alpine:3.18", Where: "job build"},
		{Name: "busybox", Where: "job build"},
		{Name: "node:20", Where: "job build container"},
		{Name: "redis:7", Where: "job build service redis"},
		{Name: "${{ matrix.image }}", Kind: uses.ImageExpression, Where: "job build service dyn"},
		{Name: "node:20", Where: "job test container"},
	}, f.ContainerImages())
}
