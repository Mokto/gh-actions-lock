package tag

import (
	"context"
	"testing"
	"time"

	"github.com/github/gh-actions-lock/internal/ghapi"
	"github.com/stretchr/testify/assert"

	"github.com/github/gh-actions-lock/internal/ghapi/httpmock"
)

// TestMatchesSHA verifies a pin matches the tag's peeled commit SHA.
// Resolution peels annotated tags to the commit before any SHA is written,
// so the commit SHA is the only identifier callers compare against.
func TestMatchesSHA(t *testing.T) {
	tag := Info{Name: "v9.0.0", SHA: "3a2844b"}

	if !tag.MatchesSHA("3a2844b") {
		t.Error("expected match on peeled commit SHA")
	}
	if !tag.MatchesSHA("3A2844B") {
		t.Error("expected case-insensitive match on commit SHA")
	}
	if tag.MatchesSHA("deadbee") {
		t.Error("unexpected match on unrelated SHA")
	}
	if tag.MatchesSHA("") {
		t.Error("empty SHA should never match")
	}
}

// TestListTags_SemverOrdering verifies tags are ordered by semantic version,
// not lexically. A string compare would place "v9.0.0" ahead of "v10.0.0";
// semver ordering must put v10 first.
func TestListTags_SemverOrdering(t *testing.T) {
	reg := &httpmock.Registry{}
	reg.Register(
		httpmock.REST("GET", `repos/actions/checkout/tags`),
		httpmock.JSONResponse([]map[string]any{
			{"name": "v9.0.0", "commit": map[string]any{"sha": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}},
			{"name": "v10.0.0", "commit": map[string]any{"sha": "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}},
			{"name": "v8.1.0", "commit": map[string]any{"sha": "cccccccccccccccccccccccccccccccccccccccc"}},
		}),
	)
	reg.Register(
		httpmock.REST("GET", `repos/actions/checkout/releases`),
		httpmock.JSONResponse([]map[string]any{}),
	)

	tl := NewListerForTest(t, reg)
	tags, err := tl.ListTags(context.Background(), "actions", "checkout")
	if err != nil {
		t.Fatalf("ListTags: %v", err)
	}

	var got []string
	for _, tg := range tags {
		got = append(got, tg.Name)
	}
	want := []string{"v10.0.0", "v9.0.0", "v8.1.0"}
	if len(got) != len(want) {
		t.Fatalf("expected %v, got %v", want, got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("semver ordering wrong: expected %v, got %v", want, got)
		}
	}
}

func TestBestPatchTagForSHA_RefFamily(t *testing.T) {
	const sha = "ffffffffffffffffffffffffffffffffffffffff"

	tests := []struct {
		name string
		ref  string
		tags any
		want string
	}{
		{
			name: "major ref excludes another major at the same commit",
			ref:  "v18",
			tags: httpmock.TagListResponse("v3.12.0", sha),
		},
		{
			name: "major ref accepts its major",
			ref:  "v4",
			tags: httpmock.TagListResponse("v5.0.0", sha, "v4.2.1", sha),
			want: "v4.2.1",
		},
		{
			name: "minor ref accepts its minor",
			ref:  "v4.2",
			tags: httpmock.TagListResponse("v4.3.0", sha, "v4.2.1", sha),
			want: "v4.2.1",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reg := &httpmock.Registry{}
			reg.Register(
				httpmock.REST("GET", `repos/actions/checkout/tags`),
				httpmock.JSONResponse(tt.tags),
			)
			reg.Register(
				httpmock.REST("GET", `repos/actions/checkout/releases`),
				httpmock.JSONResponse([]map[string]any{}),
			)

			tl := NewListerForTest(t, reg)
			got, err := tl.BestPatchTagForSHA(context.Background(), "actions", "checkout", sha, tt.ref)
			if err != nil {
				t.Fatalf("BestPatchTagForSHA: %v", err)
			}
			if got != tt.want {
				t.Fatalf("BestPatchTagForSHA() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestRepoInfoIsInternal(t *testing.T) {
	assert.True(t, RepoInfo{Visibility: "private"}.IsInternal())
	assert.True(t, RepoInfo{Visibility: "internal"}.IsInternal())
	assert.False(t, RepoInfo{Visibility: "public"}.IsInternal())
	assert.False(t, RepoInfo{}.IsInternal())
}

func TestIsTagTooNew(t *testing.T) {
	tl := NewLister(nil, CooldownConfig{DefaultDays: 7})
	repo := ghapi.ForRepo("actions", "checkout")

	recent := time.Now().Add(-2 * 24 * time.Hour).Format(time.RFC3339) // within cooldown
	old := time.Now().Add(-30 * 24 * time.Hour).Format(time.RFC3339)   // past cooldown
	tl.releaseDates.Put(repo, map[string]string{
		"v4.2.1": recent,
		"v4.2.0": old,
	})

	assert.True(t, tl.isTagTooNew("actions", "checkout", "v4.2.1"))
	assert.False(t, tl.isTagTooNew("actions", "checkout", "v4.2.0"))
	// A tag with no known release date is never filtered.
	assert.False(t, tl.isTagTooNew("actions", "checkout", "v9.9.9"))

	// A zero/negative cooldown disables the age check entirely.
	tl0 := NewLister(nil, CooldownConfig{DefaultDays: 0})
	tl0.releaseDates.Put(repo, map[string]string{"v4.2.1": recent})
	assert.False(t, tl0.isTagTooNew("actions", "checkout", "v4.2.1"))
}

// TestTagLister_CaseInsensitiveCacheKeys locks in the discipline that all
// Lister in-memory caches (cache, repoCache, releaseDates) are keyed by
// cachekey.ForRepo, which lowercases owner and repo. Two callers naming the
// same repo with different case must share one cache slot — otherwise we
// would issue duplicate API requests and the case variants would drift.
//
// The cache key type (ghapi.Repo) does not by itself lowercase its fields,
// so constructing a key without ForRepo would reintroduce the collision.
// This test guards against that regression by driving the public API with
// two case variants and asserting only one cache entry results.
//
// Mechanism: register HTTP stubs with case-insensitive regexes so they
// match either case variant. Each stub is single-use; if the second call
// is not a cache hit it will re-issue the request and fail with
// "no registered HTTP stubs matched".
func TestTagLister_CaseInsensitiveCacheKeys(t *testing.T) {
	reg := &httpmock.Registry{}
	// (?i) so the stub matches whichever case the first caller sent. Each
	// stub fires exactly once; a second call that isn't served from cache
	// would error out with no-stubs-matched.
	reg.Register(
		httpmock.REST("GET", `(?i)repos/octocat/hello-world/tags`),
		httpmock.JSONResponse([]map[string]any{
			{"name": "v1.0.0", "commit": map[string]any{"sha": "abc123"}},
		}),
	)
	reg.Register(
		httpmock.REST("GET", `(?i)repos/octocat/hello-world/releases`),
		httpmock.JSONResponse([]map[string]any{
			{"tag_name": "v1.0.0", "published_at": "2024-01-01T00:00:00Z", "immutable": false},
		}),
	)
	reg.Register(
		// repos/<owner>/<repo> with no trailing path segment.
		httpmock.REST("GET", `(?i)repos/octocat/hello-world$`),
		httpmock.JSONResponse(map[string]any{
			"default_branch": "main",
			"visibility":     "public",
			"pushed_at":      "2024-01-01T00:00:00Z",
		}),
	)

	tl := NewListerForTest(t, reg)

	// First pass: canonical lowercase.
	if _, err := tl.ListTags(context.Background(), "octocat", "hello-world"); err != nil {
		t.Fatalf("ListTags lowercase: %v", err)
	}
	if _, err := tl.GetRepoInfo(context.Background(), "octocat", "hello-world"); err != nil {
		t.Fatalf("GetRepoInfo lowercase: %v", err)
	}
	if got := tl.ReleaseDate("octocat", "hello-world", "v1.0.0"); got == "" {
		t.Fatalf("ReleaseDate lowercase: expected populated date, got empty")
	}

	// Second pass: mixed case for the same repo. Every cache must hit.
	// If any of these fall through to the network, the registry will
	// fail with "no registered HTTP stubs matched" because each stub
	// has already fired.
	if _, err := tl.ListTags(context.Background(), "Octocat", "Hello-World"); err != nil {
		t.Fatalf("ListTags mixed case: %v", err)
	}
	if _, err := tl.GetRepoInfo(context.Background(), "OCTOCAT", "HELLO-WORLD"); err != nil {
		t.Fatalf("GetRepoInfo upper case: %v", err)
	}
	if got := tl.ReleaseDate("Octocat", "Hello-World", "v1.0.0"); got == "" {
		t.Fatalf("ReleaseDate mixed case: expected cache hit, got empty")
	}

	// Belt and suspenders: each cache must hold exactly one entry. A
	// direct map index with un-normalized input would compile silently
	// and reintroduce the collision class cachekey.ForRepo prevents.
	gotTags := tl.tagCache.Len()
	gotRepo := tl.repoCache.Len()
	gotDates := tl.releaseDates.Len()

	if gotTags != 1 {
		t.Errorf("tags cache: expected 1 entry, got %d", gotTags)
	}
	if gotRepo != 1 {
		t.Errorf("repo cache: expected 1 entry, got %d", gotRepo)
	}
	if gotDates != 1 {
		t.Errorf("releaseDates cache: expected 1 entry, got %d", gotDates)
	}

	reg.Verify(t)
}
