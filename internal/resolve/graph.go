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
	// host is the resolver's host, which an empty Dependency.Hostname means.
	host string
}

type graphNode struct {
	// ref is the reference as first requested; it names an unresolved node.
	ref      parserlock.ActionRef
	dep      dep.Dependency
	resolved bool
	children []graphEdge
}

// graphEdge is one `uses:` as its manifest writes it. A manifest can't be
// rewritten, so its edge keeps that repository and ref whatever the workflow
// does with the same action. A `$/…` (self) edge is the exception: it
// resolves at whatever ref its parent runs at.
type graphEdge struct {
	key      ghapi.ActionRef
	self     bool
	declared parserlock.ActionRef
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
	e := graphEdge{key: g.node(child), self: self, declared: child}
	for _, have := range n.children {
		// The same action named with `$/…` and literally is two references.
		if have.key == e.key && have.self == e.self && strings.EqualFold(have.declared.NWO(), e.declared.NWO()) {
			return
		}
	}
	n.children = append(n.children, e)
}

// conflictingCommits rejects a graph that resolved one owner/repo@ref to two
// commits, e.g. a branch that advanced between fetching a composite and a
// sub-action in the same repository. The lockfile entry would mix both.
func (g *Graph) conflictingCommits() error {
	commits := make(map[EntryKey]string, len(g.order))
	for _, key := range g.order {
		d := g.nodes[key].dep
		fk := entryKeyOf(d.NWO, d.Ref)
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
			if visit(c.key) {
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
func (g *Graph) Flatten() ([]dep.Dependency, dep.ParentMap, error) {
	entries, parents, err := g.FlattenPlanned(Decisions{})
	deps := make([]dep.Dependency, len(entries))
	for i, e := range entries {
		deps[i] = e.Dep
	}
	return deps, parents, err
}

// EntryKey identifies one lockfile entry the graph produced: owner/repo
// folded, ref as the referrer declared it. Only the graph makes them, in
// FlattenPlanned's result; callers compare them and hand them back in
// Decisions, and can't build one from a spelling of their own.
type EntryKey struct {
	key string
}

// Entry is one lockfile entry the graph produced.
type Entry struct {
	Key EntryKey
	Dep dep.Dependency
	// Root reports a workflow root's entry; an entry kept for a manifest's
	// name is not one.
	Root bool
}

// entryKeyOf is the graph's entry identity (E1g). It compares spellings
// within one graph only; see the construction notes on FlattenPlanned.
func entryKeyOf(nwo, ref string) EntryKey {
	return EntryKey{key: strings.ToLower(nwo) + "@" + ref}
}

// sameEntry reports whether two owner/repo@ref names this graph produced
// share an entry. Refs can contain "@"; owner/repo can't.
func sameEntry(a, b string) bool {
	an, ar, _ := strings.Cut(a, "@")
	bn, br, _ := strings.Cut(b, "@")
	return entryKeyOf(an, ar) == entryKeyOf(bn, br)
}

// Decisions are planning's choices about entries the graph returned. The
// graph applies them while flattening, so it stays the one place where
// identities are merged.
type Decisions struct {
	// Roots maps a workflow root's entry to the ref to record it under, at
	// the same commit: narrowing (v2 → v2.1.0) or reverse lookup.
	Roots map[EntryKey]string
	// Meta is the tag and branch reverse lookup found for an entry's commit.
	Meta map[EntryKey]RefMeta
}

// RefMeta is a commit's tag and branch, as dep.Dependency records them.
type RefMeta struct {
	Tag, Branch string
}

// FlattenPlanned is Flatten with planning's decisions applied.
//
// A rewrite belongs to the referrer that can be rewritten: only a workflow
// root takes a new ref or a redirect's canonical repository. A manifest's
// edge keeps the repository and ref it declares, so an action both uses
// becomes two entries rather than one renamed entry. `$/…` sub-actions follow
// their parent. roots holds the Dependency.Key of each workflow root's
// entry: an entry kept for a manifest's name is not one.
//
// Two identities that land on one entry must be the same commit on the
// same host, or the entry would record one and the edges of both.
func (g *Graph) FlattenPlanned(d Decisions) (entries []Entry, parents dep.ParentMap, err error) {
	// item is one lockfile identity of a node: the repository and ref its
	// referrer names it by.
	type item struct {
		key ghapi.ActionRef
		nwo string
		ref string
	}
	rootItems := make(map[item]bool)
	var queue []item
	for _, key := range g.roots {
		n := g.nodes[key]
		it := item{key: key, nwo: n.ref.NWO(), ref: n.ref.Ref}
		if n.resolved {
			it.nwo, it.ref = n.dep.NWO, n.dep.Ref
			if newRef, ok := d.Roots[entryKeyOf(n.dep.NWO, n.dep.Ref)]; ok {
				it.ref = newRef
			}
		}
		rootItems[it] = true
		queue = append(queue, it)
	}
	children := func(it item) []item {
		n := g.nodes[it.key]
		out := make([]item, len(n.children))
		for i, e := range n.children {
			c := g.nodes[e.key]
			switch {
			case e.self:
				out[i] = item{key: e.key, nwo: it.nwo, ref: it.ref}
			case c.resolved && strings.EqualFold(e.declared.NWO(), c.dep.NWO):
				out[i] = item{key: e.key, nwo: c.dep.NWO, ref: e.declared.Ref}
			default:
				out[i] = item{key: e.key, nwo: e.declared.NWO(), ref: e.declared.Ref}
			}
		}
		return out
	}
	reached := make(map[item]bool)
	byKey := make(map[ghapi.ActionRef][]item)
	for len(queue) > 0 {
		it := queue[0]
		queue = queue[1:]
		if reached[it] {
			continue
		}
		reached[it] = true
		byKey[it.key] = append(byKey[it.key], it)
		queue = append(queue, children(it)...)
	}

	depOf := func(it item) dep.Dependency {
		n := g.nodes[it.key]
		out := n.dep
		if m, ok := d.Meta[entryKeyOf(n.dep.NWO, n.dep.Ref)]; ok {
			out.Tag, out.Branch = m.Tag, m.Branch
		}
		out.NWO, out.Ref = it.nwo, it.ref
		if !rootItems[it] {
			// Only a workflow root is rewritten to a redirect's target.
			out.OriginalRefs = nil
		}
		return out
	}
	var emitted []item
	for _, key := range g.order {
		emitted = append(emitted, byKey[key]...)
	}

	var deps []dep.Dependency
	rep := make(map[EntryKey]int)
	for _, it := range emitted {
		e := depOf(it)
		fk := entryKeyOf(e.NWO, e.Ref)
		i, ok := rep[fk]
		if !ok {
			rep[fk] = len(deps)
			deps = append(deps, e)
			continue
		}
		if !strings.EqualFold(deps[i].SHA, e.SHA) || g.hostOf(deps[i]) != g.hostOf(e) {
			return nil, nil, fmt.Errorf("%s would record both %s and %s; retry", e.Key(), describeTarget(deps[i]), describeTarget(e))
		}
		for _, ref := range e.OriginalRefs {
			if !slices.Contains(deps[i].OriginalRefs, ref) {
				deps[i].OriginalRefs = append(deps[i].OriginalRefs, ref)
			}
		}
	}
	name := func(it item) string {
		if i, ok := rep[entryKeyOf(it.nwo, it.ref)]; ok {
			return deps[i].Key()
		}
		return it.nwo + "@" + it.ref
	}
	parents = make(dep.ParentMap)
	roots := make(map[string]bool, len(rootItems))
	for it := range rootItems {
		roots[name(it)] = true
	}
	for _, it := range emitted {
		pk := name(it)
		for _, c := range children(it) {
			ck := name(c)
			// Sub-actions of one owner/repo@ref share its lockfile entry.
			if sameEntry(ck, pk) || slices.Contains(parents[ck], pk) {
				continue
			}
			parents[ck] = append(parents[ck], pk)
		}
	}
	entries = make([]Entry, len(deps))
	for i, d := range deps {
		entries[i] = Entry{Key: entryKeyOf(d.NWO, d.Ref), Dep: d, Root: roots[d.Key()]}
	}
	return entries, parents, nil
}

// hostOf is d's host; empty means the resolver's own.
func (g *Graph) hostOf(d dep.Dependency) string {
	if d.Hostname == "" {
		return g.host
	}
	return d.Hostname
}

func describeTarget(d dep.Dependency) string {
	if d.Hostname == "" {
		return d.SHA
	}
	return d.Hostname + ":" + d.SHA
}
