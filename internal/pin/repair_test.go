package pin

import (
	"context"
	"strings"
	"testing"

	parserlock "github.com/github/actions-lockfile/go/pkg/lockfile"
	"github.com/github/gh-actions-lock/internal/resolve"
	"github.com/stretchr/testify/assert"
)

// TestProveIdentity_NoClientFailsClosed: without an API client no
// candidate's identity can be proven, so every candidate is refused.
func TestProveIdentity_NoClientFailsClosed(t *testing.T) {
	sha := strings.Repeat("a", 40)
	key := "actions/cache@" + sha
	recorded := map[string]parserlock.Action{key: {Ref: sha, Commit: "sha1-" + sha, OwnerID: 1, RepoID: 1}}

	failed := proveIdentity(context.Background(), &resolve.Resolver{}, recorded)
	assert.Contains(t, failed, key)
	assert.Empty(t, proveIdentity(context.Background(), &resolve.Resolver{}, nil), "no candidates, nothing to prove")
}

// TestIdentityAdvice: a failed proof never points at --relock, which
// re-records the same entry; a composite's pin names the composite.
func TestIdentityAdvice(t *testing.T) {
	tests := []struct {
		name    string
		parents []string
		want    string
	}{
		{name: "workflow pin", want: "bad; nothing is written. Retry if the check was inconclusive, or change to a verified repository and ref"},
		{name: "composite pin", parents: []string{"octo/z@v1", "octo/a@v1"},
			want: "bad; nothing is written. Retry if the check was inconclusive, or change to a verified repository and ref; for a pin a composite uses, upgrade or replace octo/a@v1, octo/z@v1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := identityAdvice("bad", tt.parents)
			assert.Equal(t, tt.want, got)
			assert.NotContains(t, got, "--relock")
		})
	}
}
