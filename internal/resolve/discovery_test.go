package resolve

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	parserlock "github.com/github/actions-lockfile/go/pkg/lockfile"
	"github.com/github/gh-actions-lock/internal/dep"
	"github.com/github/gh-actions-lock/internal/ghapi"
	"github.com/github/gh-actions-lock/internal/ghapi/httpmock"
	"github.com/github/gh-actions-lock/internal/pinpool"
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
			actionYML: "runs:\n  using: composite\n  steps:\n    - uses: ./opaque\n    - uses: " + rejected + "\n    - uses: ./also-opaque\n",
		},
	})

	_, _, err := r.ResolveAllRecursive(context.Background(), []parserlock.ActionRef{
		{Owner: "owner", Repo: "composite", Ref: "v1"},
	})

	require.Error(t, err)
	assert.Contains(t, err.Error(), rejected)
}

func TestResolveGraphKeepsOpaquePathsSeparateFromDependencies(t *testing.T) {
	const parentSHA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	const remoteSHA = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	for _, locked := range []bool{false, true} {
		name := "live refs"
		if locked {
			name = "locked commits"
		}
		t.Run(name, func(t *testing.T) {
			r := &Resolver{MaxRecursionDepth: DefaultMaxRecursionDepth}
			entries := []resolvedEntry{
				{
					dep: dep.Dependency{NWO: "owner/parent", Path: "root", Ref: "Main", SHA: parentSHA},
					actionYML: "runs:\n  using: composite\n  steps:\n" +
						"    - uses: ./opaque\n    - uses: $/sibling\n    - uses: other/remote@v2\n",
				},
				{
					dep:       dep.Dependency{NWO: "owner/parent", Path: "sibling", Ref: "Main", SHA: parentSHA},
					actionYML: "runs:\n  using: composite\n  steps:\n    - uses: ./nested-opaque\n",
				},
				{
					dep:       dep.Dependency{NWO: "other/remote", Ref: "v2", SHA: remoteSHA},
					actionYML: "runs:\n  using: node20\n",
				},
			}
			var recorded []dep.Dependency
			for _, entry := range entries {
				owner, repo := entry.dep.OwnerRepo()
				ref := entry.dep.Ref
				cache := &r.cache
				if locked {
					ref = entry.dep.SHA
					cache = &r.lockedContentCache
				}
				cache.Put(ghapi.ForActionRef(owner, repo, entry.dep.Path, ref), entry)
				recorded = append(recorded, entry.dep)
			}
			refs := []parserlock.ActionRef{{Owner: "owner", Repo: "parent", Path: "root", Ref: "Main"}}
			var graph *Graph
			var err error
			if locked {
				graph, err = r.ResolveLockedGraph(context.Background(), refs, recorded)
			} else {
				graph, err = r.ResolveGraph(context.Background(), refs)
			}
			require.NoError(t, err)
			assert.Equal(t, []string{
				LocalActionWarning("owner/parent/root@Main", "./opaque"),
				LocalActionWarning("owner/parent/sibling@Main", "./nested-opaque"),
			}, graph.LocalActions)
			deps, parents, err := graph.Flatten()
			require.NoError(t, err)
			require.Len(t, deps, 2)
			assert.Equal(t, "owner/parent@Main", deps[0].Key())
			assert.Equal(t, "other/remote@v2", deps[1].Key())
			assert.Equal(t, []string{"owner/parent@Main"}, parents[dep.RefKeyOf("other/remote@v2")])
			assert.Len(t, graph.nodes, 3, "workspace paths must never become graph nodes")
		})
	}
}

func TestResolveLockedRecursiveCachesContentWithoutLosingLogicalRefs(t *testing.T) {
	const sha = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	reg := &httpmock.Registry{}
	defer reg.Verify(t)
	for _, path := range []string{"root", "sibling"} {
		reg.Register(httpmock.GraphQL(`file: file\(path`), func(req *http.Request) (*http.Response, error) {
			var body struct {
				Variables map[string]string `json:"variables"`
			}
			if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
				return nil, err
			}
			assert.Equal(t, sha+"^{commit}", body.Variables["expr0"])
			assert.Equal(t, path+"/action.yml", body.Variables["yml0"])
			yml := "runs:\n  using: node20\n"
			if path == "root" {
				yml = "runs:\n  using: composite\n  steps:\n    - uses: $/sibling\n"
			}
			return httpmock.JSONResponse(map[string]any{"data": map[string]any{
				"a0": map[string]any{
					"nameWithOwner": "new/action",
					"object": map[string]any{
						"oid": sha, "file": map[string]any{"object": map[string]any{"text": yml}},
					},
				},
			}})(req)
		})
	}
	r, err := New("github.com", pinpool.New(2, nil), WithTransport(reg))
	require.NoError(t, err)
	for _, ref := range []string{"Main", "main"} {
		d := dep.Dependency{NWO: "old/action", Ref: ref, SHA: sha}
		r.SeedFromLockfile([]dep.Dependency{d})
		deps, parents, err := r.ResolveLockedRecursive(context.Background(),
			[]parserlock.ActionRef{{Owner: "old", Repo: "action", Path: "root", Ref: ref}},
			[]dep.Dependency{d})
		require.NoError(t, err)
		require.Len(t, deps, 1, "same-tarball paths collapse into one dependency")
		assert.Equal(t, "old/action", deps[0].NWO, "locked discovery retains the recorded name across a redirect")
		assert.Equal(t, ref, deps[0].Ref, "content cache must not retain the first logical ref")
		assert.Empty(t, parents, "same-tarball uses must not produce a self-edge")
	}
	assert.Len(t, reg.Requests, 2, "immutable root and sibling content reused across logical refs")
}

func TestResolveLockedRecursiveMissingChildDoesNotResolveLiveRef(t *testing.T) {
	const sha = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	reg := &httpmock.Registry{}
	defer reg.Verify(t)
	reg.Register(httpmock.GraphQL(`file: file\(path`), httpmock.JSONResponse(map[string]any{
		"data": map[string]any{"a0": map[string]any{
			"nameWithOwner": "o/root",
			"object": map[string]any{
				"oid":  sha,
				"file": map[string]any{"object": map[string]any{"text": "runs:\n  using: composite\n  steps:\n    - uses: o/child@Main\n"}},
			},
		}},
	}))
	r, err := New("github.com", pinpool.New(2, nil), WithTransport(reg))
	require.NoError(t, err)
	_, parents, err := r.ResolveLockedRecursive(context.Background(),
		[]parserlock.ActionRef{{Owner: "o", Repo: "root", Ref: "v1"}},
		[]dep.Dependency{{NWO: "o/root", Ref: "v1", SHA: sha}})
	var missing *UnlockedRefError
	require.ErrorAs(t, err, &missing)
	assert.Equal(t, "Main", missing.Ref.Ref)
	assert.Equal(t, []string{"o/root@v1"}, parents[dep.RefKeyOf("o/child@Main")])
	g, err := r.ResolveLockedGraph(context.Background(),
		[]parserlock.ActionRef{{Owner: "o", Repo: "root", Ref: "v1"}},
		[]dep.Dependency{{NWO: "o/root", Ref: "v1", SHA: sha}})
	require.ErrorAs(t, err, &missing)
	_, scoped, err := g.Inspect([]parserlock.ActionRef{{Owner: "o", Repo: "root", Ref: "v1"}})
	require.ErrorAs(t, err, &missing, "an unresolved child is not in resolution order but still belongs to its root")
	assert.Equal(t, parents, scoped)
	assert.Len(t, reg.Requests, 1)
}

func TestResolveLockedGraphScopesAliasFailures(t *testing.T) {
	const sha = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	reg := &httpmock.Registry{}
	defer reg.Verify(t)
	reg.Register(httpmock.GraphQL(`file: file\(path`), httpmock.JSONResponse(map[string]any{
		"data": map[string]any{
			"a0": map[string]any{"nameWithOwner": "o/healthy", "object": map[string]any{
				"oid": sha, "file": map[string]any{"object": map[string]any{"text": "runs:\n  using: node20\n"}},
			}},
			"a1": map[string]any{"nameWithOwner": "o/failed", "object": map[string]any{"oid": sha}},
		},
		"errors": []map[string]any{{"type": "FORBIDDEN", "message": "manifest denied", "path": []string{"a1", "object", "file"}}},
	}))
	r, err := New("github.com", pinpool.New(2, nil), WithTransport(reg))
	require.NoError(t, err)
	healthy := parserlock.ActionRef{Owner: "o", Repo: "healthy", Ref: "v1"}
	failed := parserlock.ActionRef{Owner: "o", Repo: "failed", Ref: "v1"}
	g, err := r.ResolveLockedGraph(context.Background(), []parserlock.ActionRef{healthy, failed}, []dep.Dependency{
		{NWO: "o/healthy", Ref: "v1", SHA: sha}, {NWO: "o/failed", Ref: "v1", SHA: sha},
	})
	require.ErrorContains(t, err, "manifest denied")
	_, _, err = g.Inspect([]parserlock.ActionRef{healthy})
	require.NoError(t, err)
	_, _, err = g.Inspect([]parserlock.ActionRef{failed})
	require.ErrorContains(t, err, "o/failed@v1")
	require.ErrorContains(t, err, "manifest denied")
	assert.Len(t, reg.Requests, 1, "scoped views never fetch or retry a failed alias")
}

func TestResolveLockedGraphBatchLimit(t *testing.T) {
	const sha = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	for _, count := range []int{1, batchActionFileSize, batchActionFileSize + 1} {
		t.Run(fmt.Sprintf("%d roots", count), func(t *testing.T) {
			reg := &httpmock.Registry{}
			defer reg.Verify(t)
			batches := (count + batchActionFileSize - 1) / batchActionFileSize
			for range batches {
				reg.Register(httpmock.GraphQL(`file: file\(path`), func(req *http.Request) (*http.Response, error) {
					var body struct {
						Variables map[string]string `json:"variables"`
					}
					if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
						return nil, err
					}
					data := map[string]any{}
					for i := 0; ; i++ {
						name, ok := body.Variables[fmt.Sprintf("name%d", i)]
						if !ok {
							break
						}
						assert.Less(t, i, batchActionFileSize)
						assert.Equal(t, sha+"^{commit}", body.Variables[fmt.Sprintf("expr%d", i)])
						data[fmt.Sprintf("a%d", i)] = map[string]any{
							"nameWithOwner": "o/" + name, "object": map[string]any{
								"oid": sha, "file": map[string]any{"object": map[string]any{"text": "runs:\n  using: node20\n"}},
							},
						}
					}
					return httpmock.JSONResponse(map[string]any{"data": data})(req)
				})
			}
			var roots []parserlock.ActionRef
			var deps []dep.Dependency
			for i := range count {
				name := fmt.Sprintf("r%d", i)
				roots = append(roots, parserlock.ActionRef{Owner: "o", Repo: name, Ref: "v1"})
				deps = append(deps, dep.Dependency{NWO: "o/" + name, Ref: "v1", SHA: sha})
			}
			r, err := New("github.com", pinpool.New(2, nil), WithTransport(reg))
			require.NoError(t, err)
			g, err := r.ResolveLockedGraph(context.Background(), roots, deps)
			require.NoError(t, err)
			for _, root := range roots {
				found, parents, err := g.Inspect([]parserlock.ActionRef{root})
				require.NoError(t, err)
				assert.Equal(t, []string{root.NWO() + "@v1"}, depKeys(found))
				assert.Empty(t, parents)
			}
			assert.Len(t, reg.Requests, batches)
		})
	}
}

func TestResolveRecursiveRejectsMalformedManifest(t *testing.T) {
	for name, locked := range map[string]bool{"live refs": false, "locked commits": true} {
		t.Run(name, func(t *testing.T) {
			const sha = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
			reg := &httpmock.Registry{}
			defer reg.Verify(t)
			reg.Register(httpmock.GraphQL(`file: file\(path`), httpmock.JSONResponse(map[string]any{
				"data": map[string]any{"a0": map[string]any{
					"nameWithOwner": "o/root",
					"object":        map[string]any{"oid": sha, "file": map[string]any{"object": map[string]any{"text": "runs: ["}}},
				}},
			}))
			r, err := New("github.com", pinpool.New(2, nil), WithTransport(reg))
			require.NoError(t, err)
			refs := []parserlock.ActionRef{{Owner: "o", Repo: "root", Ref: "v1"}}
			if locked {
				_, _, err = r.ResolveLockedRecursive(context.Background(), refs,
					[]dep.Dependency{{NWO: "o/root", Ref: "v1", SHA: sha}})
			} else {
				_, _, err = r.ResolveAllRecursive(context.Background(), refs)
			}
			require.Error(t, err)
			assert.ErrorContains(t, err, "reading manifest for o/root@v1")
		})
	}
}

// TestResolveRecursiveMissingManifest: live resolution and --verify agree
// that a commit without an action manifest can't establish coverage, so a
// refresh can't write a graph that --verify then rejects.
func TestResolveRecursiveMissingManifest(t *testing.T) {
	for _, tt := range []struct {
		name       string
		path       string
		dockerfile string
		wantErr    bool
	}{
		{name: "missing action cannot establish coverage", path: "action", wantErr: true},
		{name: "callee workflow owns its own lockfile", path: ".github/workflows/callee.yml"},
		// The Actions backend runs a Dockerfile in the action directory.
		{name: "Dockerfile-only action", path: "action", dockerfile: "dockerfile"},
		{name: "lowercase dockerfile-only action", path: "action", dockerfile: "dockerfileLower"},
	} {
		for mode, locked := range map[string]bool{"live refs": false, "locked commits": true} {
			t.Run(tt.name+"/"+mode, func(t *testing.T) {
				const sha = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
				reg := &httpmock.Registry{}
				defer reg.Verify(t)
				fetches := 1
				if tt.wantErr && !locked {
					fetches = 2
				}
				object := map[string]any{"oid": sha}
				if tt.dockerfile != "" {
					object[tt.dockerfile] = map[string]any{"oid": sha}
				}
				for range fetches {
					reg.Register(httpmock.GraphQL(`file: file\(path`), httpmock.JSONResponse(map[string]any{
						"data": map[string]any{"a0": map[string]any{
							"nameWithOwner": "o/root", "object": object,
						}},
					}))
				}
				r, err := New("github.com", pinpool.New(2, nil), WithTransport(reg))
				require.NoError(t, err)
				refs := []parserlock.ActionRef{{Owner: "o", Repo: "root", Path: tt.path, Ref: "v1"}}
				var deps []dep.Dependency
				var parents dep.ParentMap
				if locked {
					deps, parents, err = r.ResolveLockedRecursive(context.Background(), refs,
						[]dep.Dependency{{NWO: "o/root", Ref: "v1", SHA: sha}})
				} else {
					deps, parents, err = r.ResolveAllRecursive(context.Background(), refs)
				}
				assert.Empty(t, parents)
				if !tt.wantErr {
					require.NoError(t, err)
					return
				}
				require.ErrorContains(t, err, "manifest is missing")
				if !locked {
					assert.Empty(t, deps, "a live commit without a manifest is not resolved")
					_, _, err = r.ResolveAllRecursive(context.Background(), refs)
					require.ErrorContains(t, err, "manifest is missing", "the failure is not cached as a leaf")
				}
			})
		}
	}
}

func TestResolveRecursiveMixedCaseSiblingDoesNotCreateSelfEdge(t *testing.T) {
	for name, locked := range map[string]bool{"live refs": false, "locked commits": true} {
		t.Run(name, func(t *testing.T) {
			const sha = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
			entries := map[ghapi.ActionRef]resolvedEntry{
				ghapi.ForActionRef("o", "action", "", "Main"): {
					dep:       dep.Dependency{NWO: "o/action", Ref: "Main", SHA: sha},
					actionYML: "runs:\n  using: composite\n  steps:\n    - uses: O/Action/sibling@Main\n",
				},
				ghapi.ForActionRef("o", "action", "sibling", "Main"): {
					dep:       dep.Dependency{NWO: "o/action", Path: "sibling", Ref: "Main", SHA: sha},
					actionYML: "runs:\n  using: composite\n  steps:\n    - uses: vendor/child@Main\n",
				},
				ghapi.ForActionRef("vendor", "child", "", "Main"): {
					dep:       dep.Dependency{NWO: "vendor/child", Ref: "Main", SHA: sha},
					actionYML: "runs:\n  using: node20\n",
				},
			}
			r := seedCache(&Resolver{MaxRecursionDepth: DefaultMaxRecursionDepth}, entries)
			refs := []parserlock.ActionRef{{Owner: "o", Repo: "action", Ref: "Main"}}
			var deps []dep.Dependency
			var parents dep.ParentMap
			var err error
			if locked {
				for _, entry := range entries {
					owner, repo := entry.dep.OwnerRepo()
					r.lockedContentCache.Put(ghapi.ForActionRef(owner, repo, entry.dep.Path, sha), entry)
				}
				deps, parents, err = r.ResolveLockedRecursive(context.Background(), refs,
					[]dep.Dependency{{NWO: "o/action", Ref: "Main", SHA: sha}, {NWO: "vendor/child", Ref: "Main", SHA: sha}})
			} else {
				deps, parents, err = r.ResolveAllRecursive(context.Background(), refs)
			}
			require.NoError(t, err)
			assert.Len(t, deps, 2)
			assert.Len(t, parents, 1, "same-tarball sibling must not produce a self-edge")
			assert.Equal(t, []string{"o/action@Main"}, parents[dep.RefKeyOf("vendor/child@Main")])
		})
	}
}

// TestResolveLockedRecursiveFoldsOnlyOwnerRepoCasing: a locked commit is
// found whatever the owner/repo casing, but never under a differently cased
// ref, which git treats as another ref.
func TestResolveLockedRecursiveFoldsOnlyOwnerRepoCasing(t *testing.T) {
	const sha = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	locked := []dep.Dependency{{NWO: "o/action", Ref: "main", SHA: sha}}
	t.Run("owner/repo casing", func(t *testing.T) {
		r := &Resolver{MaxRecursionDepth: DefaultMaxRecursionDepth}
		r.lockedContentCache.Put(ghapi.ForActionRef("o", "action", "", sha), resolvedEntry{
			dep:       dep.Dependency{NWO: "o/action", Ref: "main", SHA: sha},
			actionYML: "runs:\n  using: node20\n",
		})
		_, _, err := r.ResolveLockedRecursive(context.Background(),
			[]parserlock.ActionRef{{Owner: "O", Repo: "Action", Ref: "main"}}, locked)
		require.NoError(t, err)
	})
	t.Run("ref casing", func(t *testing.T) {
		r := &Resolver{MaxRecursionDepth: DefaultMaxRecursionDepth}
		_, _, err := r.ResolveLockedRecursive(context.Background(),
			[]parserlock.ActionRef{{Owner: "O", Repo: "Action", Ref: "Main"}}, locked)
		var missing *UnlockedRefError
		require.ErrorAs(t, err, &missing)
		assert.Equal(t, "Main", missing.Ref.Ref)
	})
}

func TestResolveAllRecursiveCanonicalizesParentCasing(t *testing.T) {
	const sha = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	for _, ref := range []string{"v1", "Release@2024"} {
		t.Run(ref, func(t *testing.T) {
			r := seedCache(&Resolver{MaxRecursionDepth: DefaultMaxRecursionDepth}, map[ghapi.ActionRef]resolvedEntry{
				ghapi.ForActionRef("vendor", "child", "", ref): {
					dep:       dep.Dependency{NWO: "vendor/child", Ref: ref, SHA: sha},
					actionYML: "runs:\n  using: node20\n",
				},
				ghapi.ForActionRef("o", "parent", "", "v1"): {
					dep:       dep.Dependency{NWO: "o/parent", Ref: "v1", SHA: sha},
					actionYML: "runs:\n  using: composite\n  steps:\n    - uses: Vendor/Child@" + ref + "\n",
				},
			})
			_, parents, err := r.ResolveAllRecursive(context.Background(), []parserlock.ActionRef{
				{Owner: "vendor", Repo: "child", Ref: ref},
				{Owner: "o", Repo: "parent", Ref: "v1"},
			})
			require.NoError(t, err)
			assert.Equal(t, dep.ParentMap{dep.RefKeyOf("vendor/child@" + ref): {"o/parent@v1"}}, parents,
				"edges are keyed by the resolved dependency, so the writer finds them; refs keep their case")
		})
	}
}

func TestResolveAllRecursiveRejectsConflictingCommits(t *testing.T) {
	const (
		first  = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		second = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	)
	r := seedCache(&Resolver{MaxRecursionDepth: DefaultMaxRecursionDepth}, map[ghapi.ActionRef]resolvedEntry{
		ghapi.ForActionRef("o", "action", "", "main"): {
			dep:       dep.Dependency{NWO: "o/action", Ref: "main", SHA: first},
			actionYML: "runs:\n  using: composite\n  steps:\n    - uses: o/action/sub@main\n",
		},
		// main advanced before the sub-action was fetched.
		ghapi.ForActionRef("o", "action", "sub", "main"): {
			dep:       dep.Dependency{NWO: "o/action", Path: "sub", Ref: "main", SHA: second},
			actionYML: "runs:\n  using: node20\n",
		},
	})
	_, _, err := r.ResolveAllRecursive(context.Background(), []parserlock.ActionRef{{Owner: "o", Repo: "action", Ref: "main"}})
	require.ErrorContains(t, err, "o/action@main resolved to both")
}

// TestResolveAllRecursiveFetchesSubActionsAtStickyCommit: a lockfile seed
// carries no path, so a new workflow using actions/cache@v4 and
// actions/cache/save@v4 must fetch save at the seeded commit, not at where
// v4 points now; otherwise the shared entry splits across two commits and
// every retry conflicts again.
func TestResolveAllRecursiveFetchesSubActionsAtStickyCommit(t *testing.T) {
	const (
		locked = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		moved  = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	)
	reg := &httpmock.Registry{}
	defer reg.Verify(t)
	reg.Register(httpmock.GraphQL(`file: file\(path`), func(req *http.Request) (*http.Response, error) {
		var body struct {
			Variables map[string]string `json:"variables"`
		}
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			return nil, err
		}
		oid := strings.TrimSuffix(body.Variables["expr0"], "^{commit}")
		if oid == "v4" {
			oid = moved
		}
		return httpmock.JSONResponse(map[string]any{"data": map[string]any{"a0": map[string]any{
			"nameWithOwner": "actions/cache",
			"object":        map[string]any{"oid": oid, "file": map[string]any{"object": map[string]any{"text": "runs:\n  using: node20\n"}}},
		}}})(req)
	})
	r, err := New("github.com", pinpool.New(2, nil), WithTransport(reg))
	require.NoError(t, err)
	r.SeedFromLockfile([]dep.Dependency{{NWO: "actions/cache", Ref: "v4", SHA: locked}})

	g, err := r.ResolveGraph(context.Background(), []parserlock.ActionRef{
		{Owner: "actions", Repo: "cache", Ref: "v4"},
		{Owner: "actions", Repo: "cache", Path: "save", Ref: "v4"},
	})
	require.NoError(t, err)
	deps, _, err := g.Flatten()
	require.NoError(t, err)
	require.Len(t, deps, 1)
	assert.Equal(t, locked, deps[0].SHA)
}
