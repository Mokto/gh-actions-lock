package checks

import (
	"testing"

	"github.com/github/gh-actions-lock/internal/dep"
	"github.com/stretchr/testify/assert"
)

func TestPrunesDependency(t *testing.T) {
	d := &dep.Dependency{NWO: "actions/checkout", Ref: "v4", SHA: "373d742f768e37e4e68d84ada06be93fb2ce64d8"}
	tests := []struct {
		name                string
		f                   Finding
		acceptMoved, relock bool
		want                bool
	}{
		{"stale always", Finding{Category: Stale, Dependency: d}, false, false, true},
		{"ref-moved under relock", Finding{Category: RefMoved, Dependency: d}, false, true, true},
		{"ref-moved under accept-moved", Finding{Category: RefMoved, Dependency: d}, true, true, true},
		{"ref-moved on a plain run", Finding{Category: RefMoved, Dependency: d}, false, false, false},
		{"unreachable-pin under accept-moved", Finding{Category: UnreachablePin, Dependency: d}, true, true, true},
		{"unreachable-pin under relock alone", Finding{Category: UnreachablePin, Dependency: d}, false, true, false},
		{"same-ID rename warning", Finding{Category: RepoRenamed, Severity: SeverityWarning, Dependency: d}, false, false, true},
		{"blocking rename", Finding{Category: RepoRenamed, Severity: SeverityError, Dependency: d}, false, false, false},
		{"inconclusive history", Finding{Category: ReachabilityUnknown, Dependency: d}, true, true, false},
		{"no dependency", Finding{Category: Stale}, false, false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, PrunesDependency(tt.f, tt.acceptMoved, tt.relock))
		})
	}
}

func TestPruneEffectOf(t *testing.T) {
	d := &dep.Dependency{NWO: "actions/checkout", Ref: "v4", SHA: "373d742f768e37e4e68d84ada06be93fb2ce64d8"}
	tests := []struct {
		name string
		f    Finding
		want PruneEffect
	}{
		{"a stale use is dropped", Finding{Category: Stale, Dependency: d}, Dropped},
		{"a same-ID rename re-keys under the new name", Finding{Category: RepoRenamed, Severity: SeverityWarning, Dependency: d}, Dropped},
		{"a moved ref re-resolves in place", Finding{Category: RefMoved, Dependency: d}, Reresolved},
		{"an accepted unreachable pin re-resolves in place", Finding{Category: UnreachablePin, Dependency: d}, Reresolved},
		{"an inconclusive check prunes nothing", Finding{Category: ReachabilityUnknown, Dependency: d}, NotPruned},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, PruneEffectOf(tt.f, true, true))
		})
	}
}

func TestPruneKey(t *testing.T) {
	assert.Equal(t, PruneKey("Actions/Checkout", "V4", "ABC"), PruneKey("actions/checkout", "v4", "abc"), "planning's existing identity folds the whole key")
}
