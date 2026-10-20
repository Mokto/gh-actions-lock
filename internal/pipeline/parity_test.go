package pipeline

import (
	"errors"
	"strings"
	"testing"

	parserlock "github.com/github/actions-lockfile/go/pkg/lockfile"
	"github.com/github/gh-actions-lock/internal/dep"
	"github.com/github/gh-actions-lock/internal/ghapi"
	"github.com/github/gh-actions-lock/internal/lockfile"
	"github.com/github/gh-actions-lock/internal/pipeline/checks"
	"github.com/github/gh-actions-lock/internal/resolve"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParityFinding(t *testing.T) {
	const (
		sha   = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		other = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	)
	pin := func(ref string) ghapi.PinCheck {
		return ghapi.PinCheck{Owner: "krzema12", Repo: "github-actions-typing", SHA: sha, Head: ref}
	}
	intact := ghapi.PinState{NameWithOwner: "krzema12/github-actions-typing", OwnerID: 1, RepoID: 2, CommitFound: true, HeadOID: sha}

	tests := []struct {
		name        string
		ref         string
		parent      string
		state       ghapi.PinState
		want        checks.Category // empty means no finding
		wantWarn    bool
		wantRemedy  string
		wantDetail  string
		wantObserve string
	}{
		{name: "intact exact tag", ref: "v2.2.2", state: intact},
		{name: "intact mutable ref", ref: "v2", state: ghapi.PinState{NameWithOwner: "krzema12/github-actions-typing", CommitFound: true, HeadOID: sha}},
		{name: "case-only name difference is not a move", ref: "v2", state: ghapi.PinState{NameWithOwner: "Krzema12/GitHub-Actions-Typing", CommitFound: true, HeadOID: sha}},
		{
			name: "transferred repository names the new location", ref: "v2.2.2",
			state: ghapi.PinState{NameWithOwner: "typesafegithub/github-actions-typing", CommitFound: true, HeadOID: sha},
			want:  checks.RepoRenamed, wantWarn: true,
			wantRemedy: "run `gh actions-lock` to rewrite it as `uses: typesafegithub/github-actions-typing@v2.2.2`",
		},
		{
			name: "transferred transitive dependency points at its parent", ref: "v2", parent: "octo/composite@v1",
			state: ghapi.PinState{NameWithOwner: "typesafegithub/github-actions-typing", CommitFound: true, HeadOID: sha},
			want:  checks.RepoRenamed, wantWarn: true,
			wantRemedy: "upgrade octo/composite@v1 to a version that uses typesafegithub/github-actions-typing",
		},
		{
			name: "transfer keeps the repo ID and is a warning", ref: "v2.2.2",
			state: ghapi.PinState{NameWithOwner: "typesafegithub/github-actions-typing", OwnerID: 77, RepoID: 2, CommitFound: true, HeadOID: sha},
			want:  checks.RepoRenamed, wantWarn: true,
		},
		{name: "owner ID change alone is not identity", ref: "v2", state: ghapi.PinState{NameWithOwner: "krzema12/github-actions-typing", OwnerID: 77, RepoID: 2, CommitFound: true, HeadOID: sha}},
		{name: "repo ID changed", ref: "v2", state: ghapi.PinState{NameWithOwner: "krzema12/github-actions-typing", OwnerID: 1, RepoID: 99, CommitFound: true, HeadOID: sha}, want: checks.RepoReplaced},
		{
			name: "replaced transitive dependency points at its parent", ref: "v2", parent: "octo/composite@v1",
			state: ghapi.PinState{NameWithOwner: "krzema12/github-actions-typing", RepoID: 99, CommitFound: true, HeadOID: sha}, want: checks.RepoReplaced,
			wantDetail: "krzema12/github-actions-typing (used by octo/composite@v1) now resolves to a different repository than the one locked (repo ID 99, locked 2)",
			wantRemedy: "do not trust it: the locked repository was deleted and its name taken over, possibly by an attacker. Stop using octo/composite@v1 or upgrade it to a version that doesn't use krzema12/github-actions-typing",
		},
		{name: "redirect to a different repo ID blocks", ref: "v2", state: ghapi.PinState{NameWithOwner: "mallory/github-actions-typing", RepoID: 99, CommitFound: true, HeadOID: sha}, want: checks.RepoReplaced},
		{
			name: "missing repository leads with relock", ref: "v2", state: ghapi.PinState{RepoMissing: true}, want: checks.RepoUnavailable,
			wantDetail: "krzema12/github-actions-typing is missing or not visible to this token",
			wantRemedy: "if the repository moved or was deleted, update `uses:` and run `gh actions-lock`; if it's private or internal, use a token that can read it or authorize SSO for krzema12",
		},
		{
			name: "missing repository on fallback leads with access", ref: "v2", state: ghapi.PinState{RepoMissing: true, ViaFallback: true}, want: checks.RepoUnavailable,
			wantRemedy: "if it's private or internal, use a token that can read it or authorize SSO for krzema12; if the repository moved or was deleted, update `uses:` and run `gh actions-lock`",
		},
		{name: "missing ID is not a change", ref: "v2", state: ghapi.PinState{NameWithOwner: "krzema12/github-actions-typing", CommitFound: true, HeadOID: sha}},
		{name: "commit gone", ref: "v2", state: ghapi.PinState{NameWithOwner: "krzema12/github-actions-typing"}, want: checks.UnreachablePin},
		{
			name: "commit gone names both causes and the tag's target", ref: "v2.2.2", state: ghapi.PinState{NameWithOwner: "krzema12/github-actions-typing", HeadOID: other}, want: checks.UnreachablePin,
			wantDetail: "locked commit aaaaaaaaaaaa is not in krzema12/github-actions-typing's history: it was force-pushed away, or the lockfile was edited; v2.2.2 currently points at bbbbbbbbbbbb",
			wantRemedy: "verify it, then run `gh actions-lock --accept-moved`",
		},
		// Tags move: a moved exact tag is left to the history check like any ref.
		{name: "moved exact tag is not a finding by itself", ref: "v2.2.2", state: ghapi.PinState{NameWithOwner: "krzema12/github-actions-typing", CommitFound: true, HeadOID: other}},
		{
			name: "deleted tag asks for a new ref", ref: "v2.2.2", state: ghapi.PinState{NameWithOwner: "krzema12/github-actions-typing", CommitFound: true}, want: checks.UnreachablePin,
			wantRemedy: "point `uses:` at a tag or branch that exists, then run `gh actions-lock`",
		},
		{name: "network error is inconclusive", ref: "v2", state: ghapi.PinState{Err: errors.New("timeout")}, want: checks.ReachabilityUnknown},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			lp := lockfile.LockedPin{
				Pin:    parserlock.Pin{NWO: "krzema12/github-actions-typing", Owner: "krzema12", Repo: "github-actions-typing", Ref: tt.ref},
				Action: parserlock.Action{Ref: tt.ref, Commit: "sha1-" + sha, OwnerID: 1, RepoID: 2},
				Parent: tt.parent,
			}
			ref := parserlock.ActionRef{Owner: lp.Pin.Owner, Repo: lp.Pin.Repo, Ref: tt.ref}
			f, ok := parityFinding(checks.ParsedWorkflow{Path: ".github/workflows/ci.yml"}, ref, lp, pin(tt.ref), tt.state)
			if tt.want == "" {
				assert.False(t, ok, "unexpected finding: %+v", f)
				return
			}
			assert.True(t, ok)
			assert.Equal(t, tt.want, f.Category)
			assert.Equal(t, tt.wantWarn || tt.want == checks.ReachabilityUnknown, f.Severity == checks.SeverityWarning)
			if tt.wantDetail != "" {
				assert.Equal(t, tt.wantDetail, f.Detail)
			}
			if tt.wantRemedy != "" {
				assert.Equal(t, tt.wantRemedy, f.Remediation)
			}
			assert.Equal(t, tt.wantObserve, f.ObservedSHA)
		})
	}
}

// TestFreshClosure_EntryRef: a fresh pin is checked against its own ref. A
// full-SHA key has none yet, whatever tag or branch it carried: planning
// picks one by reverse lookup.
// TestFreshClosure_FullSHAInAnyCase: one run records a full SHA under one
// spelling, so a workflow spelling it in another case still checks the pin
// it resolved.
func TestFreshClosure_FullSHAInAnyCase(t *testing.T) {
	const sha = "373d742f768e37e4e68d84ada06be93fb2ce64d8"
	pw := checks.ParsedWorkflow{Path: ".github/workflows/ci.yml", Refs: []parserlock.ActionRef{{Owner: "actions", Repo: "checkout", Ref: strings.ToUpper(sha)}}}
	got := freshClosure([]dep.Dependency{{NWO: "actions/checkout", Ref: sha, SHA: sha}}, nil)(pw, resolve.RefSet{})
	require.Len(t, got, 1)
	assert.Equal(t, sha, got[0].lp.Pin.Ref)
}

func TestFreshClosure_EntryRef(t *testing.T) {
	const sha = "373d742f768e37e4e68d84ada06be93fb2ce64d8"
	tests := []struct {
		name string
		d    dep.Dependency
		want string
	}{
		{"tag key", dep.Dependency{NWO: "actions/checkout", Ref: "v6", SHA: sha}, "v6"},
		{"SHA key on a tag", dep.Dependency{NWO: "actions/checkout", Ref: sha, SHA: sha, Tag: "v6.1.0", Branch: "main"}, ""},
		{"SHA key on a branch", dep.Dependency{NWO: "actions/checkout", Ref: sha, SHA: sha, Branch: "main"}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pw := checks.ParsedWorkflow{Path: ".github/workflows/ci.yml", Refs: []parserlock.ActionRef{{Owner: "actions", Repo: "checkout", Ref: tt.d.Ref}}}
			got := freshClosure([]dep.Dependency{tt.d}, nil)(pw, resolve.RefSet{})
			require.Len(t, got, 1)
			assert.Equal(t, tt.want, got[0].lp.Action.Ref)
		})
	}
}

// TestParityFinding_FullSHAKey covers the verdicts for a full-SHA key
// checked against its recorded ref.
func TestParityFinding_FullSHAKey(t *testing.T) {
	const (
		sha  = "373d742f768e37e4e68d84ada06be93fb2ce64d8"
		live = "d23441a49e5d9e4ba0dd8e0f0b5b86ff8ad2e2d5"
	)
	tests := []struct {
		name       string
		head       string
		state      ghapi.PinState
		want       checks.Category
		wantDetail string
	}{
		{name: "commit in recorded ref", head: "v6.1.0", state: ghapi.PinState{NameWithOwner: "actions/checkout", CommitFound: true, HeadOID: live}},
		{
			name: "commit outside recorded ref", head: "v6.1.0",
			state: ghapi.PinState{NameWithOwner: "actions/checkout", HeadOID: live}, want: checks.UnreachablePin,
			wantDetail: "locked commit 373d742f768e is not in actions/checkout's history: it was force-pushed away, or the lockfile was edited; v6.1.0 currently points at d23441a49e5d",
		},
		{
			name: "recorded ref missing", head: "does-not-exist",
			state: ghapi.PinState{NameWithOwner: "actions/checkout", CommitFound: true}, want: checks.UnreachablePin,
			wantDetail: "ref does-not-exist is not a tag or branch of actions/checkout; the Actions backend will reject this pin",
		},
		{name: "inconclusive stays a warning", head: "v6.1.0", state: ghapi.PinState{Err: errors.New("timeout")}, want: checks.ReachabilityUnknown},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			lp := lockfile.LockedPin{
				Pin:    parserlock.Pin{NWO: "actions/checkout", Owner: "actions", Repo: "checkout", Ref: sha},
				Action: parserlock.Action{Ref: tt.head, Commit: "sha1-" + sha, OwnerID: 1, RepoID: 2},
			}
			ref := parserlock.ActionRef{Owner: "actions", Repo: "checkout", Ref: sha}
			pc := ghapi.PinCheck{Owner: "actions", Repo: "checkout", SHA: sha, Head: tt.head}
			f, ok := parityFinding(checks.ParsedWorkflow{Path: ".github/workflows/ci.yml"}, ref, lp, pc, tt.state)
			if tt.want == "" {
				assert.False(t, ok, "unexpected finding: %+v", f)
				return
			}
			assert.True(t, ok)
			assert.Equal(t, tt.want, f.Category)
			assert.Equal(t, tt.want == checks.ReachabilityUnknown, f.Severity == checks.SeverityWarning)
			if tt.wantDetail != "" {
				assert.Equal(t, tt.wantDetail, f.Detail)
			}
			assert.False(t, f.RefMissing, "the workflow ref is a SHA; only the recorded ref is gone")
			assert.NotEmpty(t, f.Remediation)
		})
	}
}

// TestMovedTagFinding: a moved-ahead tag warning names the composite that
// pins the commit, and its advice must be something that follows the tag:
// --relock re-resolves a tag `uses:`, but never changes a full-SHA one.
func TestMovedTagFinding(t *testing.T) {
	const (
		sha  = "373d742f768e37e4e68d84ada06be93fb2ce64d8"
		live = "d23441a49e5d9e4ba0dd8e0f0b5b86ff8ad2e2d5"
	)
	tests := []struct {
		name       string
		ref        string
		parent     string
		wantParent string
		wantAdvice string
	}{
		{name: "direct tag pin", ref: "v6.1.0",
			wantAdvice: "the Actions backend accepts this pin; run `gh actions-lock --relock` to lock the tag's current commit"},
		{name: "transitive tag pin", ref: "v6.1.0", parent: "octo/composite@v1", wantParent: "octo/composite@v1",
			wantAdvice: "the Actions backend accepts this pin; run `gh actions-lock --relock` to lock the tag's current commit"},
		{name: "direct full-SHA pin", ref: sha,
			wantAdvice: "the Actions backend accepts this pin; to follow the tag, change `uses:` to actions/checkout/sub@v6.1.0 and run `gh actions-lock`"},
		{name: "transitive full-SHA pin", ref: sha, parent: "octo/composite@v1", wantParent: "octo/composite@v1",
			wantAdvice: "the Actions backend accepts this pin; only upgrading octo/composite@v1 follows the tag"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			lp := lockfile.LockedPin{
				Pin:    parserlock.Pin{NWO: "actions/checkout", Owner: "actions", Repo: "checkout", Ref: tt.ref},
				Action: parserlock.Action{Ref: "v6.1.0", Commit: "sha1-" + sha},
				Parent: tt.parent,
			}
			ref := parserlock.ActionRef{Owner: "actions", Repo: "checkout", Path: "sub", Ref: tt.ref}
			pc := ghapi.PinCheck{Owner: "actions", Repo: "checkout", SHA: sha, Head: "v6.1.0"}
			f := movedTagFinding(checks.ParsedWorkflow{Path: ".github/workflows/ci.yml"}, ref, lp, pc, ghapi.PinState{CommitFound: true, HeadOID: live})
			assert.Equal(t, checks.SeverityWarning, f.Severity)
			assert.Equal(t, tt.wantParent, f.ParentNWO)
			assert.Equal(t, tt.ref, f.Dependency.Ref)
			assert.Equal(t, tt.wantAdvice, f.Remediation)
			if tt.parent != "" {
				assert.True(t, strings.HasSuffix(f.Detail, " (used by "+tt.parent+")"), f.Detail)
			}
		})
	}
}

func TestCompositeEdges(t *testing.T) {
	const sha = "373d742f768e37e4e68d84ada06be93fb2ce64d8"
	pin := func(k string) parserlock.Pin {
		p, ok := parserlock.ParsePin(k)
		require.True(t, ok)
		return p
	}
	closure := []lockfile.LockedPin{
		{Key: "octo/composite@v1", Pin: pin("octo/composite@v1"), Action: parserlock.Action{Uses: []string{"actions/checkout@" + sha}}},
		{Key: "actions/checkout@" + sha, Pin: pin("actions/checkout@" + sha)},
	}
	edges := compositeEdges(closure)
	require.Len(t, edges, 1)
	require.Len(t, edges["actions/checkout@"+sha], 1, "a direct pin a composite also lists keeps that edge")
	assert.Equal(t, "octo/composite@v1", edges["actions/checkout@"+sha][0].Pin.String())
}

func TestHeldBy(t *testing.T) {
	const sha = "373d742f768e37e4e68d84ada06be93fb2ce64d8"
	pin := func(k, parent string) lockfile.LockedPin {
		p, ok := parserlock.ParsePin(k)
		require.True(t, ok)
		return lockfile.LockedPin{Pin: p, Parent: parent}
	}
	composite := []lockfile.LockedPin{pin("octo/root@v1", "")}
	tests := []struct {
		name       string
		lp         lockfile.LockedPin
		composites []lockfile.LockedPin
		want       string
	}{
		{"a composite's full-SHA child", pin("actions/checkout@"+sha, "octo/root@v1"), composite, "octo/root@v1"},
		{"a composite's mutable child is re-resolved by the rebuild", pin("actions/checkout@v4", "octo/root@v1"), composite, ""},
		{"a full-SHA entry shared with a direct use", pin("actions/checkout@"+sha, ""), composite, "octo/root@v1"},
		{"a mutable entry shared with a direct use is re-resolved in place", pin("actions/checkout@v4", ""), composite, ""},
		{"a direct pin no composite lists", pin("actions/checkout@"+sha, ""), nil, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, heldBy(tt.lp, tt.composites))
		})
	}
}

func TestPlannedAway(t *testing.T) {
	const sha = "373d742f768e37e4e68d84ada06be93fb2ce64d8"
	pin := func(k, parent string) lockfile.LockedPin {
		p, ok := parserlock.ParsePin(k)
		require.True(t, ok)
		return lockfile.LockedPin{Pin: p, Parent: parent}
	}
	composite := []lockfile.LockedPin{pin("octo/root@v1", "")}
	tests := []struct {
		name       string
		lp         lockfile.LockedPin
		composites []lockfile.LockedPin
		effect     checks.PruneEffect
		want       bool
	}{
		{"an unpruned entry is written back", pin("actions/checkout@v4", ""), nil, checks.NotPruned, false},
		{"a re-resolved mutable pin is overwritten", pin("actions/checkout@v4", ""), nil, checks.Reresolved, true},
		{"so is one a composite shares, under the same key", pin("actions/checkout@v4", ""), composite, checks.Reresolved, true},
		{"a re-resolved full-SHA pin may be re-keyed", pin("actions/checkout@"+sha, ""), composite, checks.Reresolved, false},
		{"a re-resolved full-SHA pin nothing else lists is replaced", pin("actions/checkout@"+sha, ""), nil, checks.Reresolved, true},
		{"a dropped use nothing else lists is removed", pin("actions/checkout@v4", ""), nil, checks.Dropped, true},
		{"a dropped use a composite lists leaves the entry", pin("actions/checkout@v4", ""), composite, checks.Dropped, false},
		{"a composite's child is never pruned", pin("actions/checkout@v4", "octo/root@v1"), composite, checks.Reresolved, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, plannedAway(tt.lp, tt.composites, tt.effect))
		})
	}
}
