package pipeline

import (
	"testing"

	"github.com/github/gh-actions-lock/internal/dep"
	"github.com/github/gh-actions-lock/internal/pipeline/checks"
	"github.com/stretchr/testify/assert"
)

func TestIsTransitivePin(t *testing.T) {
	d := dep.Dependency{NWO: "actions/cache", Ref: "v4"}
	depByKey := map[dep.Key]dep.Dependency{d.ID(): d}
	withParents := dep.ParentMap{d.ID(): {"some/composite@v1"}}

	t.Run("transitive when indexed and has parents", func(t *testing.T) {
		f := checks.Finding{Dependency: &d}
		assert.True(t, isTransitivePin(f, depByKey, withParents))
	})

	t.Run("not transitive without a dependency", func(t *testing.T) {
		f := checks.Finding{}
		assert.False(t, isTransitivePin(f, depByKey, withParents))
	})

	t.Run("not transitive when absent from the index", func(t *testing.T) {
		other := dep.Dependency{NWO: "actions/checkout", Ref: "v4"}
		f := checks.Finding{Dependency: &other}
		assert.False(t, isTransitivePin(f, depByKey, withParents))
	})

	t.Run("not transitive when the dep has no parents", func(t *testing.T) {
		noParents := dep.ParentMap{d.ID(): nil}
		f := checks.Finding{Dependency: &d}
		assert.False(t, isTransitivePin(f, depByKey, noParents))
	})
}

func TestPopulateInventoryParents(t *testing.T) {
	transitive := dep.Dependency{NWO: "actions/cache", Ref: "v4"}
	direct := dep.Dependency{NWO: "actions/checkout", Ref: "v4"}
	parentMap := dep.ParentMap{transitive.ID(): {"some/composite@v1"}}

	inventory := []checks.InventoryEntry{
		{Dep: transitive, Direct: false},
		{Dep: direct, Direct: true},
	}
	populateInventoryParents(inventory, parentMap)

	// The transitive entry is backfilled from the parent map.
	assert.Equal(t, []string{"some/composite@v1"}, inventory[0].Parents)
	// Direct entries are never given parents.
	assert.Nil(t, inventory[1].Parents)
}

func TestPopulateInventoryParents_KeepsExistingParents(t *testing.T) {
	d := dep.Dependency{NWO: "actions/cache", Ref: "v4"}
	inventory := []checks.InventoryEntry{
		{Dep: d, Direct: false, Parents: []string{"already/set@v1"}},
	}
	// parentMap would offer a different parent, but existing parents win.
	populateInventoryParents(inventory, dep.ParentMap{d.ID(): {"other/comp@v2"}})
	assert.Equal(t, []string{"already/set@v1"}, inventory[0].Parents)
}
