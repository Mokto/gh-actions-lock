package workflowfile

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestMalformedRemoteRef(t *testing.T) {
	tests := []struct {
		use  string
		want bool
	}{
		{"actions/checkout@v4", false},
		{"actions/checkout/sub@v4", false},
		{"./local", false},
		{"./.github/workflows/reusable.yml", false},
		{"docker://alpine:3", false},
		{"${{ matrix.action }}", false},
		{"$/actions/thing", false},
		{"octo/workflows/.github/workflows/release.yml@v1", false},
		{"", false},
		{"actions/checkout@main:action.yml", true},
		{"actions/checkout", true},
		{"actions/check out@v4", true},
		{"actions/checkout/../x@v4", true},
		{"octo/workflows/.github/workflows/nested/release.yml@v1", true},
	}
	for _, tt := range tests {
		assert.Equal(t, tt.want, MalformedRemoteRef(tt.use), "%q", tt.use)
	}
}
