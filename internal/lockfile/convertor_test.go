package lockfile

import (
	"testing"

	parserlock "github.com/github/actions-lockfile/go/pkg/lockfile"
	"github.com/stretchr/testify/assert"
)

func TestValidEntryRef(t *testing.T) {
	tests := []struct {
		name string
		key  string
		ref  string
		want bool
	}{
		{"tag key records its tag", "v4.2.1", "v4.2.1", true},
		{"branch key records its branch", "main", "main", true},
		{"full-SHA key records a tag", entryRefSHA, "v6.1.0", true},
		{"full-SHA key records a branch", entryRefSHA, "main", true},
		{"empty ref", entryRefSHA, "", false},
		{"full-SHA key records the SHA", entryRefSHA, entryRefSHA, false},
		{"non-SHA key records a SHA", "v4", entryRefSHA, false},
		{"non-SHA key records another ref", "v4", "main", false},
		{"ref case differs from key", "Main", "main", false},
		{"qualified tag", entryRefSHA, "refs/tags/v6.1.0", false},
		{"qualified branch", entryRefSHA, "refs/heads/main", false},
		{"pull request ref", entryRefSHA, "refs/pull/1/head", false},
		{"qualified ref on a matching key", "refs/heads/main", "refs/heads/main", false},
		// Revision syntax would be evaluated, not looked up literally.
		{"parent of a branch", entryRefSHA, "main~1", false},
		{"parent of a tag", entryRefSHA, "v1^", false},
		{"reflog entry", entryRefSHA, "v1@{1}", false},
		{"range", entryRefSHA, "a..b", false},
		{"path in a ref", entryRefSHA, "refs:x", false},
		{"revision syntax on a matching key", "main~1", "main~1", false},
		{"describe name", entryRefSHA, "v6.1.0-1-g373d742f768e", false},
		{"describe name on a matching key", "v1-g1234", "v1-g1234", false},
		{"three hex digits is a plain name", entryRefSHA, "v1-gabc", true},
		{"uppercase G is a plain name", entryRefSHA, "v1-G1234", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pin := parserlock.Pin{Owner: "actions", Repo: "checkout", Ref: tt.key}
			assert.Equal(t, tt.want, ValidEntryRef(pin, tt.ref))
		})
	}
}
