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
