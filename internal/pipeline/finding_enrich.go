package pipeline

import (
	"github.com/github/gh-actions-lock/internal/dep"
	"github.com/github/gh-actions-lock/internal/ghapi"
	"github.com/github/gh-actions-lock/internal/pipeline/checks"
)

// attachParent looks up the dep's composite-expansion parents (if any)
// and surfaces the first one as checks.Finding.ParentNWO. Direct (workflow-level)
// uses don't get a parent attached even if one exists in the graph.
//
// Findings emitted by RunChecks already carry an ActionRef for direct uses
// and a Dependency synthesized from the workflow ref / lockfile pin. This
// is purely about pointing the user at the composite that pulled in a
// transitively-pinned dep.
func attachParent(f *checks.Finding, depByKey map[dep.Key]dep.Dependency, directNWOs map[ghapi.Repo]bool, parentMap dep.ParentMap) {
	if f.Dependency == nil {
		return
	}
	owner, repo := f.Dependency.OwnerRepo()
	if directNWOs[ghapi.ForRepo(owner, repo)] {
		return
	}
	if parents := parentMap[f.Dependency.ID()]; len(parents) > 0 {
		f.ParentNWO = parents[0]
	}
}

// isTransitivePin reports whether the finding refers to a dep reached via
// composite expansion (i.e. has parents in the parent map).
func isTransitivePin(f checks.Finding, depByKey map[dep.Key]dep.Dependency, parentMap dep.ParentMap) bool {
	if f.Dependency == nil {
		return false
	}
	if _, ok := depByKey[f.Dependency.ID()]; !ok {
		return false
	}
	return len(parentMap[f.Dependency.ID()]) > 0
}

// populateInventoryParents fills in the Parents field for transitive inventory
// entries (those not marked Direct and without parents yet) by looking up each
// entry's dep key in parentMap.
func populateInventoryParents(inventory []checks.InventoryEntry, parentMap dep.ParentMap) {
	for i := range inventory {
		if inventory[i].Direct || len(inventory[i].Parents) > 0 {
			continue
		}
		parents := parentMap[inventory[i].Dep.ID()]
		if len(parents) > 0 {
			inventory[i].Parents = append([]string(nil), parents...)
		}
	}
}
