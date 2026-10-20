package ghapi

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestValidRefName(t *testing.T) {
	valid := []string{"v4", "v4.2.1", "main", "releases/v4", "feature/x-y_z", "v1.0.0-rc.1"}
	invalid := []string{
		"", "@", "main~1", "v1^", "v1^{commit}", "v1@{1}", "a..b", "refs:x", "a?b", "a*b", "a[b", `a\b`,
		"a b", "a\tb", "a\x7fb", "-x", "/x", "x/", "x.", "x//y", ".x", "x/.y", "x.lock", "x.lock/y", "\xff",
	}
	for _, name := range valid {
		assert.True(t, ValidRefName(name), name)
	}
	for _, name := range invalid {
		assert.False(t, ValidRefName(name), "%q", name)
	}
}
