package dep

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestDependencyStringRoundTrip(t *testing.T) {
	tests := []struct {
		name string
		dep  Dependency
		want string
	}{
		{
			name: "sha1 dependency",
			dep: Dependency{
				NWO:      "actions/checkout",
				Ref:      "v4",
				SHA:      "11bd71901bbe5b1630ceea73d27597364c9af683",
				HashAlgo: "sha1",
			},
			want: "actions/checkout@v4:sha1-11bd71901bbe5b1630ceea73d27597364c9af683",
		},
		{
			name: "sha256 dependency",
			dep: Dependency{
				NWO:      "actions/checkout",
				Ref:      "v4",
				SHA:      "a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2",
				HashAlgo: "sha256",
			},
			want: "actions/checkout@v4:sha256-a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2",
		},
		{
			name: "auto-detect sha1 from length",
			dep: Dependency{
				NWO: "actions/checkout",
				Ref: "v4",
				SHA: "11bd71901bbe5b1630ceea73d27597364c9af683",
			},
			want: "actions/checkout@v4:sha1-11bd71901bbe5b1630ceea73d27597364c9af683",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, tt.dep.String())
		})
	}
}

func TestDependencyKey(t *testing.T) {
	d := Dependency{NWO: "actions/checkout", Ref: "v4", SHA: "abc"}
	assert.Equal(t, "actions/checkout@v4", d.Key())
}

func TestRekeyParentMap(t *testing.T) {
	rewrites := map[string]string{"o/c@v1": "o/c@v1.0.0"}
	for name, tc := range map[string]struct {
		pm   ParentMap
		want ParentMap
	}{
		"merged children union their parents": {
			pm:   ParentMap{"o/c@v1": {"o/a@v1", "o/b@v1"}, "o/c@v1.0.0": {"o/b@v1", "o/d@v1"}},
			want: ParentMap{"o/c@v1.0.0": {"o/a@v1", "o/b@v1", "o/d@v1"}},
		},
		"a parent rewritten onto its child is dropped": {
			pm:   ParentMap{"o/c@v1.0.0": {"o/c@v1", "o/a@v1"}},
			want: ParentMap{"o/c@v1.0.0": {"o/a@v1"}},
		},
		"a child whose only parent becomes itself is removed": {
			pm:   ParentMap{"o/c@v1.0.0": {"o/c@v1"}, "o/x@v2": {"o/c@v1"}},
			want: ParentMap{"o/x@v2": {"o/c@v1.0.0"}},
		},
	} {
		t.Run(name, func(t *testing.T) {
			before := ParentMap{}
			for k, v := range tc.pm {
				before[k] = append([]string(nil), v...)
			}
			got := RekeyParentMap(tc.pm, rewrites)
			assert.Len(t, got, len(tc.want))
			for child, parents := range tc.want {
				assert.ElementsMatch(t, parents, got[child], child)
			}
			assert.Equal(t, before, tc.pm, "the input is not mutated")
		})
	}
}
