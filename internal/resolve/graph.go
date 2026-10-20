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
	// self marks children named with `$/…`: they resolve at whatever ref
	// their parent runs at, so a renamed parent renames them too.
	self map[ghapi.ActionRef]bool
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

func (g *Graph) addEdge(parent, child parserlock.ActionRef, self bool) {
	n := g.nodes[g.node(parent)]
	c := g.node(child)
	if !slices.Contains(n.children, c) {
		n.children = append(n.children, c)
	}
	if self {
		if n.self == nil {
			n.self = make(map[ghapi.ActionRef]bool)
		}
		n.self[c] = true
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
	return g.FlattenRenamed(nil)
}

// FlattenRenamed is Flatten with workflow roots recorded under other refs at
// the same commit, as narrowing (v2 → v2.1.0) and reverse lookup choose.
// renames maps a root's owner/repo@ref to its new ref. A renamed root keeps
// its edges, and its `$/…` sub-actions follow it. The old ref stays only
// where another action's manifest still names it.
func (g *Graph) FlattenRenamed(renames map[string]string) ([]dep.Dependency, dep.ParentMap) {
	type item struct {
		key ghapi.ActionRef
		// ref overrides the node's ref for a renamed root and its `$/…`
		// sub-actions.
		ref string
	}
	renamed := make(map[ghapi.ActionRef]bool)
	var roots []item
	for _, key := range g.roots {
		n := g.nodes[key]
		it := item{key: key}
		if newRef, ok := renames[flatKey(n.dep.NWO, n.dep.Ref)]; ok && n.resolved {
			it.ref = newRef
			renamed[key] = true
		}
		roots = append(roots, it)
	}
	children := func(it item) []item {
		n := g.nodes[it.key]
		out := make([]item, len(n.children))
		for i, c := range n.children {
			out[i] = item{key: c}
			if it.ref != "" && n.self[c] {
				out[i].ref = it.ref
			}
		}
		return out
	}
	reached := make(map[item]bool)
	var refs map[ghapi.ActionRef][]string
	queue := roots
	for len(queue) > 0 {
		it := queue[0]
		queue = queue[1:]
		if reached[it] {
			continue
		}
		reached[it] = true
		if it.ref != "" {
			if refs == nil {
				refs = make(map[ghapi.ActionRef][]string)
			}
			refs[it.key] = append(refs[it.key], it.ref)
		}
		queue = append(queue, children(it)...)
	}

	depOf := func(it item) dep.Dependency {
		d := g.nodes[it.key].dep
		if it.ref != "" {
			d.Ref = it.ref
		} else if renamed[it.key] {
			// Kept only because a composite names it: not a root here.
			d.OriginalRefs = nil
		}
		return d
	}
	var emitted []item
	for _, key := range g.order {
		for _, ref := range refs[key] {
			emitted = append(emitted, item{key: key, ref: ref})
		}
		if reached[item{key: key}] {
			emitted = append(emitted, item{key: key})
		}
	}

	var deps []dep.Dependency
	rep := make(map[string]int)
	for _, it := range emitted {
		d := depOf(it)
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
	name := func(it item) string {
		n := g.nodes[it.key]
		if n.resolved {
			d := depOf(it)
			return deps[rep[flatKey(d.NWO, d.Ref)]].Key()
		}
		ref := n.ref.Ref
		if it.ref != "" {
			ref = it.ref
		}
		return n.ref.NWO() + "@" + ref
	}
	parents := make(dep.ParentMap)
	for _, it := range emitted {
		pk := name(it)
		for _, c := range children(it) {
			ck := name(c)
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
