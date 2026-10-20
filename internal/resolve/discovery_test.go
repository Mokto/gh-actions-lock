package resolve

import (
	"context"
	"strings"
	"testing"

	parserlock "github.com/github/actions-lockfile/go/pkg/lockfile"
	"github.com/github/gh-actions-lock/internal/dep"
	"github.com/github/gh-actions-lock/internal/ghapi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRemoteCompositeRejectsMalformedRemoteRef: a fetched composite's
// malformed remote uses: fails resolution rather than being skipped.
func TestRemoteCompositeRejectsMalformedRemoteRef(t *testing.T) {
	const rejected = "actions/checkout@main:action.yml"
	r := seedCache(&Resolver{MaxRecursionDepth: DefaultMaxRecursionDepth}, map[ghapi.ActionRef]resolvedEntry{
		ghapi.ForActionRef("owner", "composite", "", "v1"): {
			dep:       dep.Dependency{NWO: "owner/composite", Ref: "v1", SHA: strings.Repeat("a", 40)},
			actionYML: "runs:\n  using: composite\n  steps:\n    - uses: " + rejected + "\n",
		},
	})

	_, _, err := r.ResolveAllRecursive(context.Background(), []parserlock.ActionRef{
		{Owner: "owner", Repo: "composite", Ref: "v1"},
	})

	require.Error(t, err)
	assert.Contains(t, err.Error(), rejected)
}
