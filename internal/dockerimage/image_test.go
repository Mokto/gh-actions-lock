package dockerimage

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestClassify(t *testing.T) {
	tests := []struct {
		image string
		want  Kind
	}{
		{"docker://alpine@sha256:0123456789abcdef", Digest},
		{"docker://alpine@sha256:not-hex", Digest},
		{"docker://ghcr.io/o/img:1.0@sha256:0123456789abcdef", Digest},
		{"docker://localhost:5000/img@sha256:0123456789abcdef", Digest},
		{"docker://alpine@SHA256:0123456789ABCDEF", Digest},
		{"  docker://alpine@sha256:0123456789abcdef  ", Digest},
		{"docker://alpine:3.18", Tagged},
		{"docker://ghcr.io/ossf/scorecard-action:v2.4.4", Tagged},
		{"docker://localhost:5000/img:1", Tagged},
		{" docker://alpine:3 ", Tagged},
		{"docker://alpine", Untagged},
		{"docker://localhost:5000/img", Untagged},
		{"docker://", Empty},
		{"docker://  ", Empty},
		{"docker://${{ env.IMAGE }}", Dynamic},
		{"docker://ghcr.io/o/img:${{ inputs.tag }}", Dynamic},
		{"${{ inputs.img }}", Dynamic},
		{"Dockerfile", Built},
		{"./docker/Dockerfile", Built},
	}
	for _, tt := range tests {
		t.Run(tt.image, func(t *testing.T) {
			assert.Equal(t, tt.want, Classify(tt.image))
		})
	}
}

func TestImageKindForJobImages(t *testing.T) {
	tests := []struct {
		ref  string
		want Kind
	}{
		{"node:20", Tagged},
		{"ghcr.io/o/img@sha256:0123456789abcdef", Digest},
		{"ghcr.io/o/img:1.0@sha256:0123456789abcdef", Digest},
		{"localhost:5000/img", Untagged},
		{"redis", Untagged},
		{"docker://alpine:3", Tagged},
		{"${{ matrix.image }}", Dynamic},
		{"node:${{ matrix.node }}", Dynamic},
		{"Dockerfile", Untagged},
	}
	for _, tt := range tests {
		t.Run(tt.ref, func(t *testing.T) {
			assert.Equal(t, tt.want, Image{Ref: tt.ref, Location: "jobs.j.container"}.Kind())
		})
	}
}

func TestImageRequiredByIsTheDependencyKey(t *testing.T) {
	assert.Equal(t, "o/r@v1", Image{Via: "o/r@v1"}.RequiredBy())
	assert.Equal(t, "o/r@v1", Image{Via: "o/r/scan/deep@v1"}.RequiredBy())
	assert.Empty(t, Image{Local: "$/a"}.RequiredBy())
}

func TestImageMutable(t *testing.T) {
	tests := []struct {
		image Image
		want  bool
	}{
		{Image{Ref: "docker://alpine:3"}, true},
		{Image{Ref: "docker://alpine"}, true},
		{Image{Ref: "Dockerfile", Local: "$/a"}, true},
		{Image{Ref: "docker://alpine@sha256:0"}, false},
		{Image{Ref: "docker://${{ env.X }}"}, false},
		{Image{Ref: "${{ matrix.image }}", Location: "jobs.j.container"}, false},
		{Image{Ref: "docker://"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.image.Ref, func(t *testing.T) {
			assert.Equal(t, tt.want, tt.image.Mutable())
		})
	}
}

func TestImageDetailAndRemediation(t *testing.T) {
	tests := []struct {
		name        string
		image       Image
		detail      string
		remediation string
	}{
		{
			name:        "workflow step by tag",
			image:       Image{Ref: "docker://alpine:3.18"},
			detail:      "alpine:3.18 is not covered by the actions lockfile; container image tags are mutable",
			remediation: "pin the image by digest (image@sha256:…)",
		},
		{
			name:        "workflow step untagged",
			image:       Image{Ref: "docker://alpine"},
			detail:      "alpine is not covered by the actions lockfile; an untagged image pulls the mutable latest tag",
			remediation: "pin the image by digest (image@sha256:…)",
		},
		{
			name:        "job container omits the job so jobs deduplicate",
			image:       Image{Ref: "node:20", Location: "jobs.build.container"},
			detail:      "node:20 is not covered by the actions lockfile; container image tags are mutable",
			remediation: "pin the image by digest (image@sha256:…)",
		},
		{
			name:        "remote action by tag",
			image:       Image{Ref: "docker://alpine:3", Via: "o/r@v1"},
			detail:      "alpine:3 (pulled by o/r@v1) is not covered by the actions lockfile; the lock pins the action's commit, not the image, and container image tags are mutable",
			remediation: "ask the action's maintainer to pin the image by digest (image@sha256:…)",
		},
		{
			name:        "remote action Dockerfile",
			image:       Image{Ref: "Dockerfile", Via: "o/r@v1"},
			detail:      "Dockerfile (built by o/r@v1) is not covered by the actions lockfile; the lock pins the action's commit, but the image and its base images are built on the runner at run time",
			remediation: "ask the action's maintainer to publish a prebuilt image pinned by digest, or to pin the Dockerfile's base images by digest",
		},
		{
			name:        "own action Dockerfile",
			image:       Image{Ref: "Dockerfile", Local: "$/actions/build"},
			detail:      "Dockerfile (built by $/actions/build) is not covered by the actions lockfile; the image and its base images are built on the runner at run time",
			remediation: "pin the Dockerfile's base images by digest (FROM image@sha256:…), or run a prebuilt image pinned by digest",
		},
		{
			name:        "own action by tag",
			image:       Image{Ref: "docker://alpine:3", Local: "$/actions/scan"},
			detail:      "alpine:3 (pulled by $/actions/scan) is not covered by the actions lockfile; container image tags are mutable",
			remediation: "pin the image by digest (image@sha256:…)",
		},
		{
			name:        "Dockerfile with no owner",
			image:       Image{Ref: "Dockerfile"},
			detail:      "Dockerfile is not covered by the actions lockfile; the image and its base images are built on the runner at run time",
			remediation: "pin the Dockerfile's base images by digest (FROM image@sha256:…), or run a prebuilt image pinned by digest",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.detail, tt.image.Detail())
			assert.Equal(t, tt.remediation, tt.image.Remediation())
			assert.NotContains(t, tt.image.Detail(), "by )")
		})
	}
}
