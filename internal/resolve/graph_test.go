package resolve

import (
	"context"
	"testing"

	parserlock "github.com/github/actions-lockfile/go/pkg/lockfile"
	"github.com/github/gh-actions-lock/internal/dep"
	"github.com/github/gh-actions-lock/internal/ghapi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGraphCycleWithinRepository(t *testing.T) {
	const sha = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	for _, tt := range []struct {
		name      string
		two       string
		wantCycle []string
	}{
		{name: "sibling sub-actions that call each other", two: "$/one", wantCycle: []string{"o/action/one@v1", "o/action/two@v1", "o/action/one@v1"}},
		{name: "acyclic sibling sub-actions", two: "vendor/leaf@v1"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r := seedCache(&Resolver{MaxRecursionDepth: DefaultMaxRecursionDepth}, map[ghapi.ActionRef]resolvedEntry{
				ghapi.ForActionRef("o", "action", "one", "v1"): {
					dep:       dep.Dependency{NWO: "o/action", Path: "one", Ref: "v1", SHA: sha},
					actionYML: "runs:\n  using: composite\n  steps:\n    - uses: $/two\n",
				},
				ghapi.ForActionRef("o", "action", "two", "v1"): {
					dep:       dep.Dependency{NWO: "o/action", Path: "two", Ref: "v1", SHA: sha},
					actionYML: "runs:\n  using: composite\n  steps:\n    - uses: " + tt.two + "\n",
				},
				ghapi.ForActionRef("vendor", "leaf", "", "v1"): {
					dep:       dep.Dependency{NWO: "vendor/leaf", Ref: "v1", SHA: sha},
					actionYML: "runs:\n  using: node20\n",
				},
			})
			_, _, err := r.ResolveAllRecursive(context.Background(), []parserlock.ActionRef{{Owner: "o", Repo: "action", Path: "one", Ref: "v1"}})
			if tt.wantCycle == nil {
				require.NoError(t, err)
				return
			}
			var cycle *UsesCycleError
			require.ErrorAs(t, err, &cycle)
			assert.Equal(t, tt.wantCycle, cycle.Path)
			assert.ErrorIs(t, err, dep.ErrUsesCycle)
		})
	}
}
