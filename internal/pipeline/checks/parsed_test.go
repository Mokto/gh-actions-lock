package checks

import (
	"strings"
	"testing"

	parserlock "github.com/github/actions-lockfile/go/pkg/lockfile"
	"github.com/github/gh-actions-lock/internal/dep"
	"github.com/stretchr/testify/assert"
)

// TestParsedWorkflowMatchesFullSHAsCaseInsensitively: a full SHA names a
// commit, so an uppercase `uses:` SHA finds the tag entry recorded at that
// commit; a tag or branch name still matches only exactly.
func TestParsedWorkflowMatchesFullSHAsCaseInsensitively(t *testing.T) {
	const sha = "abcdef0123456789abcdef0123456789abcdef01"
	pw := ParsedWorkflow{ExistingDeps: []dep.Dependency{
		{NWO: "o/root", Ref: "v1.0.0", SHA: sha},
		{NWO: "o/branch", Ref: "main", SHA: sha},
	}}
	for _, tt := range []struct {
		name string
		ref  parserlock.ActionRef
		want bool
	}{
		{name: "uppercase full SHA", ref: parserlock.ActionRef{Owner: "O", Repo: "Root", Ref: strings.ToUpper(sha)}, want: true},
		{name: "lowercase full SHA", ref: parserlock.ActionRef{Owner: "o", Repo: "root", Ref: sha}, want: true},
		{name: "branch in another case", ref: parserlock.ActionRef{Owner: "o", Repo: "branch", Ref: "Main"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			pw.Refs = []parserlock.ActionRef{tt.ref}
			recorded, _ := pw.PartitionRefs()
			assert.Equal(t, tt.want, len(recorded) == 1)
			assert.Equal(t, tt.want, len(pw.RecordedDeps(pw.Refs)) == 1)
		})
	}
}
