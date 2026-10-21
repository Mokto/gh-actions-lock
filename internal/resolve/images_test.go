package resolve

import (
	"testing"

	parserlock "github.com/github/actions-lockfile/go/pkg/lockfile"
	"github.com/github/gh-actions-lock/internal/dep"
	"github.com/github/gh-actions-lock/internal/ghapi"
	"github.com/github/gh-actions-lock/internal/uses"
	"github.com/stretchr/testify/assert"
)

func TestManifestImages(t *testing.T) {
	tests := map[string]struct {
		yml  string
		want []uses.Image
	}{
		"docker action by tag": {
			yml:  "runs:\n  using: docker\n  image: docker://ghcr.io/o/a:1\n",
			want: []uses.Image{{Name: "ghcr.io/o/a:1", Where: "o/a@v1", Action: "o/a@v1"}},
		},
		"docker action by digest": {
			yml: "runs:\n  using: docker\n  image: docker://ghcr.io/o/a@sha256:abc\n",
		},
		"dockerfile build": {
			yml:  "runs:\n  using: docker\n  image: Dockerfile\n",
			want: []uses.Image{{Name: "Dockerfile", Kind: uses.ImageBuild, Where: "o/a@v1", Action: "o/a@v1"}},
		},
		"composite expression step": {
			yml:  "runs:\n  using: composite\n  steps:\n    - uses: docker://${{ inputs.image }}\n",
			want: []uses.Image{{Name: "${{ inputs.image }}", Kind: uses.ImageExpression, Where: "o/a@v1", Action: "o/a@v1"}},
		},
		"composite docker step": {
			yml:  "runs:\n  using: composite\n  steps:\n    - uses: docker://alpine:3\n    - uses: actions/checkout@v4\n",
			want: []uses.Image{{Name: "alpine:3", Where: "o/a@v1", Action: "o/a@v1"}},
		},
		"node action": {
			yml: "runs:\n  using: node20\n  main: index.js\n",
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, tt.want, ManifestImages(tt.yml, "o/a@v1"))
		})
	}
}

func TestContainerImagesFollowsCachedComposites(t *testing.T) {
	r := seedCache(&Resolver{}, map[ghapi.ActionRef]resolvedEntry{
		ghapi.ForActionRef("o", "comp", "", "v1"): {
			dep:       dep.Dependency{NWO: "o/comp", Ref: "v1"},
			actionYML: "runs:\n  using: composite\n  steps:\n    - uses: o/dock/sub@v2\n",
		},
		ghapi.ForActionRef("o", "dock", "sub", "v2"): {
			dep:       dep.Dependency{NWO: "o/dock", Ref: "v2"},
			actionYML: "runs:\n  using: docker\n  image: docker://ghcr.io/o/dock:2\n",
		},
		ghapi.ForActionRef("o", "seeded", "", "v1"): {
			dep:    dep.Dependency{NWO: "o/seeded", Ref: "v1"},
			seeded: true,
		},
	})

	got := r.ContainerImages([]parserlock.ActionRef{
		{Owner: "o", Repo: "comp", Ref: "v1"},
		{Owner: "o", Repo: "seeded", Ref: "v1"},
	})

	assert.Equal(t, []uses.Image{{Name: "ghcr.io/o/dock:2", Where: "o/dock/sub@v2", Action: "o/dock/sub@v2"}}, got)
}
