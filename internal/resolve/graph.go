package resolve

import (
	"fmt"
	"slices"
	"strings"

	parserlock "github.com/github/actions-lockfile/go/pkg/lockfile"
	"github.com/github/gh-actions-lock/internal/dep"
	"github.com/github/gh-actions-lock/internal/ghapi"
)

// Graph is discovery's result. Nodes are action files keyed by owner/repo
// (case-folded), path and exact ref; edges are the `uses:` their composite
// manifests declare. The lockfile has one entry per owner/repo@ref, so
// Flatten derives that view in one place: keying edges that way during
// discovery loses sub-action edges, cycles within a repository, and the
// refs a manifest names.
type Graph struct {
	nodes map[ghapi.ActionRef]*graphNode
	// order lists resolved nodes in resolution order.
	order []ghapi.ActionRef
	roots []ghapi.ActionRef
}

type graphNode struct {
	// ref is the reference as first requested; it names an unresolved node.
	ref      parserlock.ActionRef
	dep      dep.Dependency
	resolved bool
	children []ghapi.ActionRef
}

func newGraph(roots []parserlock.ActionRef) *Graph {
	g := &Graph{nodes: make(map[ghapi.ActionRef]*graphNode)}
	for _, ref := range roots {
		key := g.node(ref)
		if !slices.Contains(g.roots, key) {
			g.roots = append(g.roots, key)
		}
	}
	return g
}

func (g *Graph) node(ref parserlock.ActionRef) ghapi.ActionRef {
	key := cacheKey(ref)
	if _, ok := g.nodes[key]; !ok {
		g.nodes[key] = &graphNode{ref: ref}
	}
	return key
}

func (g *Graph) setResolved(ref parserlock.ActionRef, d dep.Dependency) {
	n := g.nodes[g.node(ref)]
	if n.resolved {
		return
	}
	n.dep, n.resolved = d, true
	g.order = append(g.order, cacheKey(ref))
}

func (g *Graph) addEdge(parent, child parserlock.ActionRef) {
	n := g.nodes[g.node(parent)]
	if c := g.node(child); !slices.Contains(n.children, c) {
		n.children = append(n.children, c)
	}
}

// conflictingCommits rejects a graph that resolved one owner/repo@ref to two
// commits, e.g. a branch that advanced between fetching a composite and a
// sub-action in the same repository. The lockfile entry would mix both.
func (g *Graph) conflictingCommits() error {
	commits := make(map[string]string, len(g.order))
	for _, key := range g.order {
		d := g.nodes[key].dep
		fk := flatKey(d.NWO, d.Ref)
		if prev, ok := commits[fk]; ok && !strings.EqualFold(prev, d.SHA) {
			return fmt.Errorf("%s resolved to both %s and %s in one run; retry", d.Key(), prev, d.SHA)
		}
		commits[fk] = d.SHA
	}
	return nil
}

// UsesCycleError reports a composite action that reaches itself, through
// other repositories or its own sub-actions.
type UsesCycleError struct {
	Path []string
}

func (e *UsesCycleError) Error() string {
	return "composite action reaches itself: " + strings.Join(e.Path, " → ")
}

func (e *UsesCycleError) Unwrap() error { return dep.ErrUsesCycle }

// cycle returns the first uses cycle among resolved nodes, or nil. It runs
// before Flatten, which merges sub-actions and would hide a cycle among them.
func (g *Graph) cycle() *UsesCycleError {
	const (
		active = 1
		done   = 2
	)
	state := make(map[ghapi.ActionRef]int, len(g.nodes))
	var stack []ghapi.ActionRef
	var found []ghapi.ActionRef
	var visit func(ghapi.ActionRef) bool
	visit = func(key ghapi.ActionRef) bool {
		switch state[key] {
		case active:
			found = append(stack[slices.Index(stack, key):], key)
			return true
		case done:
			return false
		}
		state[key] = active
		stack = append(stack, key)
		for _, c := range g.nodes[key].children {
			if visit(c) {
				return true
			}
		}
		stack = stack[:len(stack)-1]
		state[key] = done
		return false
	}
	for _, key := range g.order {
		if visit(key) {
			path := make([]string, len(found))
			for i, k := range found {
				ref := g.nodes[k].ref
				path[i] = ref.FullName() + "@" + ref.Ref
			}
			return &UsesCycleError{Path: path}
		}
	}
	return nil
}

// Flatten returns one dependency per owner/repo@ref, in resolution order,
// and the edges between them as child key → parent keys. An edge to an
// action that didn't resolve keeps the name its parent's manifest uses.
func (g *Graph) Flatten() ([]dep.Dependency, dep.ParentMap) {
	var deps []dep.Dependency
	rep := make(map[string]int)
	for _, key := range g.order {
		d := g.nodes[key].dep
		fk := flatKey(d.NWO, d.Ref)
		i, ok := rep[fk]
		if !ok {
			rep[fk] = len(deps)
			deps = append(deps, d)
			continue
		}
		for _, ref := range d.OriginalRefs {
			if !slices.Contains(deps[i].OriginalRefs, ref) {
				deps[i].OriginalRefs = append(deps[i].OriginalRefs, ref)
			}
		}
	}
	name := func(n *graphNode) string {
		if n.resolved {
			return deps[rep[flatKey(n.dep.NWO, n.dep.Ref)]].Key()
		}
		return n.ref.NWO() + "@" + n.ref.Ref
	}
	parents := make(dep.ParentMap)
	for _, key := range g.order {
		n := g.nodes[key]
		pk := name(n)
		for _, c := range n.children {
			ck := name(g.nodes[c])
			// Sub-actions of one owner/repo@ref share its lockfile entry.
			if foldNWO(ck) == foldNWO(pk) || slices.Contains(parents[ck], pk) {
				continue
			}
			parents[ck] = append(parents[ck], pk)
		}
	}
	return deps, parents
}

func flatKey(nwo, ref string) string {
	return strings.ToLower(nwo) + "@" + ref
}

// foldNWO folds the owner/repo of an owner/repo@ref key. Refs can contain
// "@"; owner/repo can't.
func foldNWO(key string) string {
	nwo, ref, _ := strings.Cut(key, "@")
	return flatKey(nwo, ref)
}
