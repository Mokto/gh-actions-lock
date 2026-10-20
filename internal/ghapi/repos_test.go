package ghapi

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/github/gh-actions-lock/internal/ghapi/httpmock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOrderedBranches(t *testing.T) {
	branches := []BranchHead{
		{Name: "dev", SHA: "1", Protected: false},
		{Name: "main", SHA: "2", Protected: true},
		{Name: "release/v4", SHA: "3", Protected: true},
		{Name: "alpha", SHA: "4", Protected: false},
		{Name: "staging", SHA: "5", Protected: true},
	}

	ordered := OrderedBranches(branches, "release/v4", "v4", "main")

	want := []string{"release/v4", "main", "staging", "alpha", "dev"}
	if len(ordered) != len(want) {
		t.Fatalf("expected %d branches, got %d", len(want), len(ordered))
	}
	for i, name := range want {
		if ordered[i].Name != name {
			t.Errorf("position %d: expected %q, got %q", i, name, ordered[i].Name)
		}
	}
}

func TestOrderedBranches_EmptyHints(t *testing.T) {
	branches := []BranchHead{
		{Name: "b", SHA: "1"},
		{Name: "a", SHA: "2"},
	}
	ordered := OrderedBranches(branches, "", "", "")
	if len(ordered) != 2 {
		t.Fatalf("expected 2, got %d", len(ordered))
	}
	if ordered[0].Name != "a" {
		t.Errorf("expected lex-sorted, got %q first", ordered[0].Name)
	}
}

func TestEscapeBranchPath(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"main", "main"},
		{"release/v4", "release/v4"},
		{"feat/hello world", "feat/hello%20world"},
	}
	for _, tt := range tests {
		got := escapeBranchPath(tt.in)
		if got != tt.want {
			t.Errorf("escapeBranchPath(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestCompareCommits_SameSHA(t *testing.T) {
	c := &Client{}
	ok, err := c.CompareCommits(context.Background(), "o", "r", "abc", "abc")
	if err != nil || !ok {
		t.Fatalf("expected true/nil for same SHA, got %v/%v", ok, err)
	}
}

func TestCompareCommits_Ancestor(t *testing.T) {
	reg := &httpmock.Registry{}
	reg.Register(
		httpmock.REST("GET", "repos/o/r/compare/old...new"),
		httpmock.JSONResponse(map[string]any{
			"status":            "behind",
			"merge_base_commit": map[string]string{"sha": "old"},
		}),
	)

	c := newTestClient(t, reg)
	ok, err := c.CompareCommits(context.Background(), "o", "r", "old", "new")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("expected true for ancestor")
	}
}

func TestCompareCommits_NotAncestor(t *testing.T) {
	reg := &httpmock.Registry{}
	reg.Register(
		httpmock.REST("GET", "repos/o/r/compare/old...new"),
		httpmock.JSONResponse(map[string]any{
			"status":            "diverged",
			"merge_base_commit": map[string]string{"sha": "other"},
		}),
	)

	c := newTestClient(t, reg)
	ok, err := c.CompareCommits(context.Background(), "o", "r", "old", "new")
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("expected false for non-ancestor")
	}
}

func TestCompareCommits_404(t *testing.T) {
	reg := &httpmock.Registry{}
	reg.Register(
		httpmock.REST("GET", "repos/o/r/compare/old...new"),
		httpmock.StatusResponse(http.StatusNotFound),
	)

	c := newTestClient(t, reg)
	ok, err := c.CompareCommits(context.Background(), "o", "r", "old", "new")
	if err != nil {
		t.Fatalf("expected nil error for 404, got %v", err)
	}
	if ok {
		t.Fatal("expected false for 404")
	}
}

func TestCompareCommits_Cache(t *testing.T) {
	calls := 0
	reg := &httpmock.Registry{}
	reg.Register(
		httpmock.REST("GET", "repos/o/r/compare/old...new"),
		func(req *http.Request) (*http.Response, error) {
			calls++
			return httpmock.JSONResponse(map[string]any{
				"status":            "behind",
				"merge_base_commit": map[string]string{"sha": "old"},
			})(req)
		},
	)

	c := newTestClient(t, reg)
	_, _ = c.CompareCommits(context.Background(), "o", "r", "old", "new")
	_, _ = c.CompareCommits(context.Background(), "o", "r", "old", "new")
	if calls != 1 {
		t.Fatalf("expected 1 call (second should cache), got %d", calls)
	}
}

func TestGetDefaultBranch(t *testing.T) {
	reg := &httpmock.Registry{}
	reg.Register(
		httpmock.REST("GET", "repos/o/r"),
		httpmock.JSONResponse(map[string]any{"default_branch": "main"}),
	)

	c := newTestClient(t, reg)
	got := c.GetDefaultBranch(context.Background(), "o", "r")
	if got != "main" {
		t.Fatalf("expected main, got %q", got)
	}
}

func TestListBranches(t *testing.T) {
	reg := &httpmock.Registry{}
	reg.Register(
		httpmock.REST("GET", "repos/o/r/branches"),
		httpmock.JSONResponse([]map[string]any{
			{"name": "main", "commit": map[string]string{"sha": "aaa"}, "protected": true},
			{"name": "dev", "commit": map[string]string{"sha": "bbb"}, "protected": false},
		}),
	)

	c := newTestClient(t, reg)
	branches, err := c.ListBranches(context.Background(), "o", "r")
	if err != nil {
		t.Fatal(err)
	}
	if len(branches) != 2 {
		t.Fatalf("expected 2 branches, got %d", len(branches))
	}
	if branches[0].Name != "main" || !branches[0].Protected {
		t.Fatalf("unexpected first branch: %+v", branches[0])
	}
}

func TestRepoIDs(t *testing.T) {
	reg := &httpmock.Registry{}
	reg.Register(
		httpmock.REST("GET", "repos/o/r"),
		httpmock.JSONResponse(map[string]any{
			"id":    42,
			"owner": map[string]any{"id": 7},
		}),
	)

	c := newTestClient(t, reg)
	ownerID, repoID, err := c.RepoIDs(context.Background(), "o", "r")
	if err != nil {
		t.Fatal(err)
	}
	if ownerID != 7 || repoID != 42 {
		t.Fatalf("expected owner=7 repo=42, got owner=%d repo=%d", ownerID, repoID)
	}
}

func TestRepoIDs_ZeroIDs(t *testing.T) {
	reg := &httpmock.Registry{}
	reg.Register(
		httpmock.REST("GET", "repos/o/r"),
		httpmock.JSONResponse(map[string]any{
			"id":    0,
			"owner": map[string]any{"id": 0},
		}),
	)

	c := newTestClient(t, reg)
	_, _, err := c.RepoIDs(context.Background(), "o", "r")
	if err == nil {
		t.Fatal("expected error for zero IDs")
	}
}

func TestListTags(t *testing.T) {
	reg := &httpmock.Registry{}
	reg.Register(
		httpmock.REST("GET", "repos/o/r/tags"),
		httpmock.JSONResponse([]map[string]any{
			{"name": "v1.0.0", "commit": map[string]string{"sha": "aaa"}},
			{"name": "v2.0.0", "commit": map[string]string{"sha": "bbb"}},
		}),
	)

	c := newTestClient(t, reg)
	tags, err := c.ListTags(context.Background(), "o", "r")
	if err != nil {
		t.Fatal(err)
	}
	if len(tags) != 2 {
		t.Fatalf("expected 2 tags, got %d", len(tags))
	}
	if tags[0].Name != "v1.0.0" {
		t.Fatalf("unexpected first tag: %+v", tags[0])
	}
}

func TestGetBranchHead(t *testing.T) {
	reg := &httpmock.Registry{}
	reg.Register(
		httpmock.REST("GET", "repos/o/r/git/ref/heads/main"),
		httpmock.JSONResponse(map[string]any{
			"ref":    "refs/heads/main",
			"object": map[string]any{"sha": "abc123", "type": "commit"},
		}),
	)

	c := newTestClient(t, reg)
	bh, ok := c.GetBranchHead(context.Background(), "o", "r", "main")
	if !ok {
		t.Fatal("expected ok=true")
	}
	if bh.Name != "main" || bh.SHA != "abc123" {
		t.Fatalf("unexpected result: %+v", bh)
	}
}

func TestGetBranchHead_Empty(t *testing.T) {
	c := &Client{}
	_, ok := c.GetBranchHead(context.Background(), "o", "r", "")
	if ok {
		t.Fatal("expected ok=false for empty name")
	}
}

func TestMatchingHeadRefs(t *testing.T) {
	reg := &httpmock.Registry{}
	reg.Register(
		httpmock.REST("GET", "repos/o/r/git/matching-refs/heads/release/"),
		httpmock.JSONResponse([]map[string]any{
			{"ref": "refs/heads/release/v3", "object": map[string]any{"sha": "aaa", "type": "commit"}},
			{"ref": "refs/heads/release/v4", "object": map[string]any{"sha": "bbb", "type": "commit"}},
		}),
	)

	c := newTestClient(t, reg)
	branches := c.MatchingHeadRefs(context.Background(), "o", "r", "release/")
	if len(branches) != 2 {
		t.Fatalf("expected 2 branches, got %d", len(branches))
	}
	if branches[0].Name != "release/v3" {
		t.Fatalf("unexpected first branch: %+v", branches[0])
	}
}

func TestTagsAtCommit_ReadsEveryPage(t *testing.T) {
	for _, proxy := range []bool{false, true} {
		t.Run(map[bool]string{false: "authenticated", true: "dependabot proxy"}[proxy], func(t *testing.T) {
			if proxy {
				t.Setenv("GH_ACTIONS_LOCK_DEPENDABOT_PROXY", "1")
			}
			reg := &httpmock.Registry{}
			defer reg.Verify(t)
			var first []string
			for i := range 100 {
				first = append(first, fmt.Sprintf("v%03d", i), "zzz")
			}
			reg.Register(httpmock.RESTWithQuery("GET", `^/repos/o/r/tags$`, "&page=1"), httpmock.JSONResponse(httpmock.TagListResponse(first...)))
			reg.Register(httpmock.RESTWithQuery("GET", `^/repos/o/r/tags$`, "&page=2"), httpmock.JSONResponse(httpmock.TagListResponse("twin", "abc", "other", "zzz")))

			c := newTestClient(t, reg)
			c.anonHTTP = &http.Client{Transport: reg}
			names, err := c.TagsAtCommit(context.Background(), "o", "r", "abc")
			require.NoError(t, err)
			assert.Equal(t, []string{"twin"}, names)
		})
	}
}

// TestGetBranchHead_SharedRefLayer: branch heads go through the shared exact
// ref lookup, so they work under the Dependabot proxy and read only a ref
// that exists by exactly that name.
func TestGetBranchHead_SharedRefLayer(t *testing.T) {
	for _, proxy := range []bool{false, true} {
		t.Run(map[bool]string{false: "authenticated", true: "dependabot proxy"}[proxy], func(t *testing.T) {
			if proxy {
				t.Setenv("GH_ACTIONS_LOCK_DEPENDABOT_PROXY", "1")
			}
			reg := &httpmock.Registry{}
			reg.Register(httpmock.REST("GET", `^/repos/o/r/git/ref/heads/main$`), httpmock.JSONResponse(httpmock.GitRefResponse("refs/heads/main", "abc")))
			reg.Register(httpmock.REST("GET", `^/repos/o/r/git/ref/heads/rel$`),
				httpmock.JSONResponse([]any{httpmock.GitRefResponse("refs/heads/rel/x", "abc")}))
			c := newTestClient(t, reg)
			c.anonHTTP = &http.Client{Transport: reg}

			bh, ok := c.GetBranchHead(context.Background(), "o", "r", "main")
			assert.True(t, ok)
			assert.Equal(t, "abc", bh.SHA)
			_, ok = c.GetBranchHead(context.Background(), "o", "r", "rel")
			assert.False(t, ok, "a prefix match is not the branch")
		})
	}
}

// countingTransport is a RoundTripper that records how many times each URL
// path is requested and serves a canned response per endpoint shape. Unlike
// httpmock.Registry (whose stubs match once), it answers repeated requests so
// a missing-singleflight stampede shows up as a count > 1.
type countingTransport struct {
	mu     sync.Mutex
	counts map[string]int
	delay  time.Duration // widen the cache-miss window so concurrent callers overlap
}

func newCountingTransport(delay time.Duration) *countingTransport {
	return &countingTransport{counts: map[string]int{}, delay: delay}
}

func (t *countingTransport) count(substr string) int {
	t.mu.Lock()
	defer t.mu.Unlock()
	total := 0
	for p, n := range t.counts {
		if strings.Contains(p, substr) {
			total += n
		}
	}
	return total
}

func (t *countingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	t.mu.Lock()
	t.counts[req.URL.Path]++
	t.mu.Unlock()

	if t.delay > 0 {
		time.Sleep(t.delay)
	}

	p := req.URL.Path
	switch {
	case strings.Contains(p, "/compare/"):
		return httpmock.JSONResponse(map[string]any{
			"status":            "behind",
			"merge_base_commit": map[string]string{"sha": "old"},
		})(req)
	case strings.Contains(p, "/git/ref/heads/"):
		return httpmock.JSONResponse(map[string]any{
			"ref":    "refs/heads/main",
			"object": map[string]string{"sha": "headsha", "type": "commit"},
		})(req)
	default: // repos/{owner}/{repo}
		return httpmock.JSONResponse(map[string]any{
			"default_branch": "main",
			"id":             int64(20),
			"owner":          map[string]any{"id": int64(10)},
		})(req)
	}
}

func newCountingClient(t *testing.T, tr http.RoundTripper) *Client {
	t.Helper()
	c, err := New("github.com", WithClientTransport(tr))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// runConcurrent fans out fn across n goroutines that start together.
func runConcurrent(n int, fn func()) {
	var start sync.WaitGroup
	var done sync.WaitGroup
	start.Add(1)
	done.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer done.Done()
			start.Wait()
			fn()
		}()
	}
	start.Done()
	done.Wait()
}

func TestCompareCommits_CoalescesConcurrent(t *testing.T) {
	tr := newCountingTransport(2 * time.Millisecond)
	c := newCountingClient(t, tr)

	runConcurrent(50, func() {
		got, err := c.CompareCommits(context.Background(), "o", "r", "old", "new")
		if err != nil || !got {
			t.Errorf("CompareCommits = (%v, %v), want (true, nil)", got, err)
		}
	})

	if n := tr.count("/compare/"); n != 1 {
		t.Fatalf("expected 1 compare request, got %d", n)
	}
}

func TestGetBranchHead_CoalescesConcurrent(t *testing.T) {
	tr := newCountingTransport(2 * time.Millisecond)
	c := newCountingClient(t, tr)

	runConcurrent(50, func() {
		bh, ok := c.GetBranchHead(context.Background(), "o", "r", "main")
		if !ok || bh.SHA != "headsha" {
			t.Errorf("GetBranchHead = (%+v, %v), want sha=headsha ok=true", bh, ok)
		}
	})

	if n := tr.count("/git/ref/heads/"); n != 1 {
		t.Fatalf("expected 1 git/ref request, got %d", n)
	}
}

func TestRepoIDs_CoalescesConcurrent(t *testing.T) {
	tr := newCountingTransport(2 * time.Millisecond)
	c := newCountingClient(t, tr)

	runConcurrent(50, func() {
		owner, repo, err := c.RepoIDs(context.Background(), "o", "r")
		if err != nil || owner != 10 || repo != 20 {
			t.Errorf("RepoIDs = (%d, %d, %v), want (10, 20, nil)", owner, repo, err)
		}
	})

	if n := tr.count("repos/o/r"); n != 1 {
		t.Fatalf("expected 1 repo request, got %d", n)
	}
}

// RepoIDs and GetDefaultBranch both derive from repos/{owner}/{repo}; they
// must share a single round-trip rather than fetching it twice.
func TestRepoMetadata_SharedAcrossConsumers(t *testing.T) {
	tr := newCountingTransport(2 * time.Millisecond)
	c := newCountingClient(t, tr)

	runConcurrent(50, func() {
		if got := c.GetDefaultBranch(context.Background(), "o", "r"); got != "main" {
			t.Errorf("GetDefaultBranch = %q, want main", got)
		}
		if owner, repo, err := c.RepoIDs(context.Background(), "o", "r"); err != nil || owner != 10 || repo != 20 {
			t.Errorf("RepoIDs = (%d, %d, %v)", owner, repo, err)
		}
	})

	if n := tr.count("repos/o/r"); n != 1 {
		t.Fatalf("expected 1 shared repo request, got %d", n)
	}
}

// A coalesced caller fans out under a scan/errgroup context that cancels on
// first match or error. The shared request must not inherit that cancellation,
// otherwise one caller's cancel would poison the result for everyone waiting on
// it. These assert a pre-canceled caller still gets the real answer.

func TestCompareCommits_IgnoresCallerCancel(t *testing.T) {
	tr := newCountingTransport(0)
	c := newCountingClient(t, tr)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	got, err := c.CompareCommits(ctx, "o", "r", "old", "new")
	if err != nil || !got {
		t.Fatalf("CompareCommits with canceled ctx = (%v, %v), want (true, nil)", got, err)
	}
}

func TestGetBranchHead_IgnoresCallerCancel(t *testing.T) {
	tr := newCountingTransport(0)
	c := newCountingClient(t, tr)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	bh, ok := c.GetBranchHead(ctx, "o", "r", "main")
	if !ok || bh.SHA != "headsha" {
		t.Fatalf("GetBranchHead with canceled ctx = (%+v, %v), want sha=headsha ok=true", bh, ok)
	}
}

func TestRepoIDs_IgnoresCallerCancel(t *testing.T) {
	tr := newCountingTransport(0)
	c := newCountingClient(t, tr)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	owner, repo, err := c.RepoIDs(ctx, "o", "r")
	if err != nil || owner != 10 || repo != 20 {
		t.Fatalf("RepoIDs with canceled ctx = (%d, %d, %v), want (10, 20, nil)", owner, repo, err)
	}
}
