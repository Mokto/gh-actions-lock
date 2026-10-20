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

func TestGraphFlattenRenamed(t *testing.T) {
	const sha = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	for _, tt := range []struct {
		name      string
		uses      string
		wantDeps  []string
		wantEdges dep.ParentMap
	}{
		{
			name:      "$/ sub-action follows its renamed root",
			uses:      "$/sub",
			wantDeps:  []string{"o/action@v1.0.0", "vendor/leaf@v1"},
			wantEdges: dep.ParentMap{"vendor/leaf@v1": {"o/action@v1.0.0"}},
		},
		{
			name:     "sub-action named at the old ref keeps its entry",
			uses:     "o/action/sub@v1",
			wantDeps: []string{"o/action@v1.0.0", "o/action@v1", "vendor/leaf@v1"},
			wantEdges: dep.ParentMap{
				"o/action@v1":    {"o/action@v1.0.0"},
				"vendor/leaf@v1": {"o/action@v1"},
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r := seedCache(&Resolver{MaxRecursionDepth: DefaultMaxRecursionDepth}, map[ghapi.ActionRef]resolvedEntry{
				ghapi.ForActionRef("o", "action", "", "v1"): {
					dep:       dep.Dependency{NWO: "o/action", Ref: "v1", SHA: sha},
					actionYML: "runs:\n  using: composite\n  steps:\n    - uses: " + tt.uses + "\n",
				},
				ghapi.ForActionRef("o", "action", "sub", "v1"): {
					dep:       dep.Dependency{NWO: "o/action", Path: "sub", Ref: "v1", SHA: sha},
					actionYML: "runs:\n  using: composite\n  steps:\n    - uses: vendor/leaf@v1\n",
				},
				ghapi.ForActionRef("vendor", "leaf", "", "v1"): {
					dep:       dep.Dependency{NWO: "vendor/leaf", Ref: "v1", SHA: sha},
					actionYML: "runs:\n  using: node20\n",
				},
			})
			g, err := r.ResolveGraph(context.Background(), []parserlock.ActionRef{{Owner: "o", Repo: "action", Ref: "v1"}})
			require.NoError(t, err)
			deps, parents, _ := g.FlattenRenamed(map[string]string{"o/action@v1": "v1.0.0"})
			var keys []string
			for _, d := range deps {
				keys = append(keys, d.Key())
			}
			assert.Equal(t, tt.wantDeps, keys)
			assert.Equal(t, tt.wantEdges, parents)
		})
	}
}

// TestGraphRewritesBelongToTheReferrer: only a workflow root takes a new
// ref or a redirect's canonical repository; a manifest's edge keeps what it
// declares, so an action both name is split, never renamed.
func TestGraphRewritesBelongToTheReferrer(t *testing.T) {
	const sha = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	t.Run("$/ and literal forms of one sub-action", func(t *testing.T) {
		r := seedCache(&Resolver{MaxRecursionDepth: DefaultMaxRecursionDepth}, map[ghapi.ActionRef]resolvedEntry{
			ghapi.ForActionRef("o", "action", "", "v1"): {
				dep:       dep.Dependency{NWO: "o/action", Ref: "v1", SHA: sha},
				actionYML: "runs:\n  using: composite\n  steps:\n    - uses: $/sub\n    - uses: o/action/sub@v1\n",
			},
			ghapi.ForActionRef("o", "action", "sub", "v1"): {
				dep:       dep.Dependency{NWO: "o/action", Path: "sub", Ref: "v1", SHA: sha},
				actionYML: "runs:\n  using: node20\n",
			},
		})
		g, err := r.ResolveGraph(context.Background(), []parserlock.ActionRef{{Owner: "o", Repo: "action", Ref: "v1"}})
		require.NoError(t, err)
		deps, parents, _ := g.FlattenRenamed(map[string]string{"o/action@v1": "v1.0.0"})
		assert.Equal(t, []string{"o/action@v1.0.0", "o/action@v1"}, depKeys(deps))
		assert.Equal(t, dep.ParentMap{"o/action@v1": {"o/action@v1.0.0"}}, parents)
	})
	t.Run("redirected repository a composite also names", func(t *testing.T) {
		old := parserlock.ActionRef{Owner: "old", Repo: "action", Ref: "v1"}
		r := seedCache(&Resolver{MaxRecursionDepth: DefaultMaxRecursionDepth}, map[ghapi.ActionRef]resolvedEntry{
			ghapi.ForActionRef("old", "action", "", "v1"): {
				dep:       dep.Dependency{NWO: "new/action", Ref: "v1", SHA: sha, OriginalRefs: []parserlock.ActionRef{old}},
				actionYML: "runs:\n  using: node20\n",
			},
			ghapi.ForActionRef("c", "comp", "", "v1"): {
				dep:       dep.Dependency{NWO: "c/comp", Ref: "v1", SHA: sha},
				actionYML: "runs:\n  using: composite\n  steps:\n    - uses: old/action@v1\n",
			},
		})
		g, err := r.ResolveGraph(context.Background(), []parserlock.ActionRef{old, {Owner: "c", Repo: "comp", Ref: "v1"}})
		require.NoError(t, err)
		deps, parents := g.Flatten()
		assert.Equal(t, []string{"new/action@v1", "old/action@v1", "c/comp@v1"}, depKeys(deps))
		assert.Equal(t, []parserlock.ActionRef{old}, deps[0].OriginalRefs, "the workflow root is rewritten to the new name")
		assert.Empty(t, deps[1].OriginalRefs, "the composite's reference is not")
		assert.Equal(t, dep.ParentMap{"old/action@v1": {"c/comp@v1"}}, parents)
	})
}

func depKeys(deps []dep.Dependency) []string {
	var keys []string
	for _, d := range deps {
		keys = append(keys, d.Key())
	}
	return keys
}
