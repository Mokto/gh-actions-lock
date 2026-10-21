package resolve

import (
	"context"
	"go/importer"
	"go/token"
	"go/types"
	"reflect"
	"strings"
	"testing"

	parserlock "github.com/github/actions-lockfile/go/pkg/lockfile"
	"github.com/github/gh-actions-lock/internal/dep"
	"github.com/github/gh-actions-lock/internal/ghapi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGraphDepthIsIndependentOfOtherRoots(t *testing.T) {
	for _, shared := range []bool{false, true} {
		t.Run(map[bool]string{false: "chain alone", true: "chain and middle root"}[shared], func(t *testing.T) {
			names := []string{"a", "b", "c", "d", "e", "f"}
			entries := map[ghapi.ActionRef]resolvedEntry{}
			for i, name := range names {
				yml := "runs:\n  using: node20\n"
				if i+1 < len(names) {
					yml = "runs:\n  using: composite\n  steps:\n    - uses: o/" + names[i+1] + "@v1\n"
				}
				entries[ghapi.ForActionRef("o", name, "", "v1")] = resolvedEntry{
					dep: dep.Dependency{NWO: "o/" + name, Ref: "v1", SHA: strings.Repeat(name, 40)}, actionYML: yml,
				}
			}
			r := seedCache(&Resolver{MaxRecursionDepth: 4}, entries)
			root := parserlock.ActionRef{Owner: "o", Repo: "a", Ref: "v1"}
			middle := parserlock.ActionRef{Owner: "o", Repo: "d", Ref: "v1"}
			roots := []parserlock.ActionRef{root}
			if shared {
				roots = append(roots, middle)
			}
			graph, _ := r.ResolveGraph(context.Background(), roots)
			_, _, err := graph.Inspect([]parserlock.ActionRef{root})
			require.ErrorContains(t, err, "max depth")
			_, err = graph.Manifests([]parserlock.ActionRef{root})
			require.ErrorContains(t, err, "max depth")
			if shared {
				_, _, err = graph.Inspect([]parserlock.ActionRef{middle})
				require.NoError(t, err)
				_, err = graph.Manifests([]parserlock.ActionRef{middle})
				require.NoError(t, err)
			}
		})
	}
}

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

func TestGraphFlattenPlanned(t *testing.T) {
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
			wantEdges: dep.ParentMap{dep.RefKeyOf("vendor/leaf@v1"): {"o/action@v1.0.0"}},
		},
		{
			name:     "sub-action named at the old ref keeps its entry",
			uses:     "o/action/sub@v1",
			wantDeps: []string{"o/action@v1.0.0", "o/action@v1", "vendor/leaf@v1"},
			wantEdges: dep.ParentMap{
				dep.RefKeyOf("o/action@v1"):    {"o/action@v1.0.0"},
				dep.RefKeyOf("vendor/leaf@v1"): {"o/action@v1"},
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
			entries, parents, err := g.FlattenPlanned(Decisions{Roots: map[EntryKey]string{entryKeyOf("o/action", "v1"): "v1.0.0"}})
			deps := entryDepsOf(entries)
			require.NoError(t, err)
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
		entries, parents, err := g.FlattenPlanned(Decisions{Roots: map[EntryKey]string{entryKeyOf("o/action", "v1"): "v1.0.0"}})
		deps := entryDepsOf(entries)
		require.NoError(t, err)
		assert.Equal(t, []string{"o/action@v1.0.0", "o/action@v1"}, depKeys(deps))
		assert.Equal(t, dep.ParentMap{dep.RefKeyOf("o/action@v1"): {"o/action@v1.0.0"}}, parents)
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
		deps, parents, err := g.Flatten()
		require.NoError(t, err)
		assert.Equal(t, []string{"new/action@v1", "old/action@v1", "c/comp@v1"}, depKeys(deps))
		assert.Equal(t, []parserlock.ActionRef{old}, deps[0].OriginalRefs, "the workflow root is rewritten to the new name")
		assert.Empty(t, deps[1].OriginalRefs, "the composite's reference is not")
		assert.Equal(t, dep.ParentMap{dep.RefKeyOf("old/action@v1"): {"c/comp@v1"}}, parents)
	})
}

func depKeys(deps []dep.Dependency) []string {
	var keys []string
	for _, d := range deps {
		keys = append(keys, d.Key())
	}
	return keys
}

// TestGraphRejectsMergingDifferentTargets: a root renamed onto an entry that
// resolved elsewhere (a tag moved between discovery and lookup) must not
// merge into it, or the entry records one commit and both manifests' edges.
func TestGraphRejectsMergingDifferentTargets(t *testing.T) {
	const (
		first  = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		second = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	)
	for _, tt := range []struct {
		name     string
		sha      string
		hostname string
		ok       bool
	}{
		{name: "different commit", sha: second},
		{name: "different host", sha: first, hostname: "tenant.ghe.com"},
		{name: "empty host is the resolver's", sha: first, hostname: "github.com", ok: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r := seedCache(&Resolver{MaxRecursionDepth: DefaultMaxRecursionDepth, hostname: "github.com"}, map[ghapi.ActionRef]resolvedEntry{
				ghapi.ForActionRef("o", "a", "", "v2"): {
					dep:       dep.Dependency{NWO: "o/a", Ref: "v2", SHA: first},
					actionYML: "runs:\n  using: node20\n",
				},
				ghapi.ForActionRef("o", "a", "", "v2.1.0"): {
					dep:       dep.Dependency{NWO: "o/a", Ref: "v2.1.0", SHA: tt.sha, Hostname: tt.hostname},
					actionYML: "runs:\n  using: node20\n",
				},
			})
			g, err := r.ResolveGraph(context.Background(), []parserlock.ActionRef{
				{Owner: "o", Repo: "a", Ref: "v2"}, {Owner: "o", Repo: "a", Ref: "v2.1.0"},
			})
			require.NoError(t, err)
			_, _, err = g.FlattenPlanned(Decisions{Roots: map[EntryKey]string{entryKeyOf("o/a", "v2"): "v2.1.0"}})
			if tt.ok {
				require.NoError(t, err)
				return
			}
			require.ErrorContains(t, err, "o/a@v2.1.0 would record both")
		})
	}
}
func entryDepsOf(entries []Entry) []dep.Dependency {
	deps := make([]dep.Dependency, len(entries))
	for i, e := range entries {
		deps[i] = e.Dep
	}
	return deps
}

// TestEntryKeyHasNoFactory proves graph entry keys can only come from the
// graph's result: EntryKey has no exported field, and the only exported
// function or method in this package that produces an EntryKey or an Entry
// is (*Graph).FlattenPlanned. A caller can't wrap a workflow or recorded
// spelling into one.
func TestEntryKeyHasNoFactory(t *testing.T) {
	ek := reflect.TypeFor[EntryKey]()
	for i := range ek.NumField() {
		assert.False(t, ek.Field(i).IsExported(), "EntryKey field %s is exported", ek.Field(i).Name)
	}
	assert.Zero(t, ek.NumMethod(), "EntryKey methods could convert to or from a spelling")

	pkg, err := importer.ForCompiler(token.NewFileSet(), "source", nil).Import("github.com/github/gh-actions-lock/internal/resolve")
	require.NoError(t, err)
	produces := func(sig *types.Signature) bool {
		for i := range sig.Results().Len() {
			s := sig.Results().At(i).Type().String()
			if strings.Contains(s, "resolve.EntryKey") || strings.Contains(s, "resolve.Entry") {
				return true
			}
		}
		return false
	}
	var producers []string
	scope := pkg.Scope()
	for _, name := range scope.Names() {
		obj := scope.Lookup(name)
		if !obj.Exported() {
			continue
		}
		if fn, ok := obj.(*types.Func); ok && produces(fn.Type().(*types.Signature)) {
			producers = append(producers, name)
		}
		if tn, ok := obj.(*types.TypeName); ok {
			ms := types.NewMethodSet(types.NewPointer(tn.Type()))
			for i := range ms.Len() {
				m := ms.At(i).Obj()
				if m.Exported() && produces(m.Type().(*types.Signature)) {
					producers = append(producers, name+"."+m.Name())
				}
			}
		}
	}
	assert.Equal(t, []string{"Graph.FlattenPlanned"}, producers)
}

func TestGraphInspectScopesFailuresAndEdges(t *testing.T) {
	const sha = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	for _, tt := range []struct {
		name     string
		manifest string
		depth    int
		want     string
	}{
		{name: "parse failure", manifest: "runs: [", want: "reading manifest"},
		{name: "local path", manifest: "runs:\n  using: composite\n  steps:\n    - uses: ./local\n", want: "local path"},
		{name: "invalid self ref", manifest: "runs:\n  using: composite\n  steps:\n    - uses: $/sub@v1\n", want: "forbidden @ref"},
		{name: "malformed nested ref", manifest: "runs:\n  using: composite\n  steps:\n    - uses: o/action@v1:action.yml\n", want: "action.yml"},
		{name: "cycle", manifest: "runs:\n  using: composite\n  steps:\n    - uses: o/root@v1\n", want: "reaches itself"},
		{name: "depth limit", manifest: "runs:\n  using: node20\n", depth: 1, want: "max depth"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			depth := tt.depth
			if depth == 0 {
				depth = DefaultMaxRecursionDepth
			}
			r := seedCache(&Resolver{MaxRecursionDepth: depth}, map[ghapi.ActionRef]resolvedEntry{
				ghapi.ForActionRef("o", "root", "", "v1"): {
					dep:       dep.Dependency{NWO: "o/root", Ref: "v1", SHA: sha},
					actionYML: "runs:\n  using: composite\n  steps:\n    - uses: o/failed@v1\n",
				},
				ghapi.ForActionRef("o", "failed", "", "v1"): {
					dep: dep.Dependency{NWO: "o/failed", Ref: "v1", SHA: sha}, actionYML: tt.manifest,
				},
				ghapi.ForActionRef("o", "healthy", "", "v1"): {
					dep: dep.Dependency{NWO: "o/healthy", Ref: "v1", SHA: sha}, actionYML: "runs:\n  using: node20\n",
				},
			})
			bad := parserlock.ActionRef{Owner: "o", Repo: "root", Ref: "v1"}
			good := parserlock.ActionRef{Owner: "o", Repo: "healthy", Ref: "v1"}
			g, err := r.ResolveGraph(context.Background(), []parserlock.ActionRef{bad, good})
			require.ErrorContains(t, err, tt.want, "the existing aggregate wrapper still reports the failure")

			deps, parents, err := g.Inspect([]parserlock.ActionRef{good})
			require.NoError(t, err, "unrelated failure must not leak into a healthy root")
			assert.Equal(t, []string{"o/healthy@v1"}, depKeys(deps))
			assert.Empty(t, parents)

			_, parents, err = g.Inspect([]parserlock.ActionRef{bad})
			require.ErrorContains(t, err, tt.want)
			assert.Equal(t, []string{"o/root@v1"}, parents[dep.RefKeyOf("o/failed@v1")])
			parents[dep.RefKeyOf("o/failed@v1")] = nil
			_, again, err := g.Inspect([]parserlock.ActionRef{bad, bad})
			require.ErrorContains(t, err, tt.want)
			assert.Equal(t, []string{"o/root@v1"}, again[dep.RefKeyOf("o/failed@v1")], "each view owns its flattened edges")
		})
	}
}

func TestGraphInspectRejectsUntraversedRoot(t *testing.T) {
	g := newGraph(nil)
	_, _, err := g.Inspect([]parserlock.ActionRef{{Owner: "o", Repo: "missing", Ref: "v1"}})
	require.ErrorContains(t, err, "was not traversed")
}

func TestGraphManifestsPreserveSiblingPaths(t *testing.T) {
	const sha = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	root := parserlock.ActionRef{Owner: "o", Repo: "root", Ref: "v1"}
	r := seedCache(&Resolver{MaxRecursionDepth: DefaultMaxRecursionDepth}, map[ghapi.ActionRef]resolvedEntry{
		ghapi.ForActionRef("o", "root", "", "v1"): {
			dep:       dep.Dependency{NWO: "o/root", Ref: "v1", SHA: sha},
			actionYML: "runs:\n  using: composite\n  steps:\n    - uses: o/actions/a@v1\n    - uses: o/actions/b@v1\n",
		},
		ghapi.ForActionRef("o", "actions", "a", "v1"): {
			dep: dep.Dependency{NWO: "o/actions", Path: "a", Ref: "v1", SHA: sha}, actionYML: "runs:\n  using: node20\n",
		},
		ghapi.ForActionRef("o", "actions", "b", "v1"): {
			dep:       dep.Dependency{NWO: "o/actions", Path: "b", Ref: "v1", SHA: sha},
			actionYML: "runs:\n  using: composite\n  steps:\n    - uses: o/leaf@v1\n",
		},
		ghapi.ForActionRef("o", "leaf", "", "v1"): {
			dep: dep.Dependency{NWO: "o/leaf", Ref: "v1", SHA: sha}, actionYML: "runs:\n  using: node20\n",
		},
	})
	g, err := r.ResolveGraph(context.Background(), []parserlock.ActionRef{root})
	require.NoError(t, err)
	manifests, err := g.Manifests([]parserlock.ActionRef{root})
	require.NoError(t, err)
	var paths []string
	for _, manifest := range manifests {
		if manifest.Ref.Repo != "actions" {
			continue
		}
		paths = append(paths, manifest.Ref.Path)
		assert.Equal(t, sha, manifest.Entry.Dep.SHA)
		if manifest.Ref.Path == "a" {
			assert.Empty(t, manifest.Parents)
		} else {
			assert.Equal(t, dep.ParentMap{dep.RefKeyOf("o/leaf@v1"): {"o/actions@v1"}}, manifest.Parents)
		}
	}
	assert.Equal(t, []string{"a", "b"}, paths)
}
