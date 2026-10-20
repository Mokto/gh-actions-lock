// Package httpmock provides HTTP test fakes for the ghapi client.
package httpmock

import (
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"sync"
)

// This file mirrors cli/cli's pkg/httpmock/legacy.go: a small set of
// cross-cutting GitHub REST response shapes used by tests across multiple
// packages. Keep the surface narrow — only fixtures that are duplicated in
// two or more test packages belong here. Per-test bespoke shapes should stay
// inline at the call site.

// BranchListResponse builds a REST list-branches response body. pairs are
// (name, sha) alternating, e.g. BranchListResponse("main", "abc", "dev", "def").
// All entries are marked protected:true so tests model trusted-upstream
// branches by default; use BranchListResponseProtected to mix protected and
// unprotected entries.
func BranchListResponse(pairs ...string) any {
	out := make([]map[string]any, 0, len(pairs)/2)
	for i := 0; i+1 < len(pairs); i += 2 {
		out = append(out, map[string]any{
			"name":      pairs[i],
			"commit":    map[string]any{"sha": pairs[i+1]},
			"protected": true,
		})
	}
	return out
}

// BranchListResponseProtected builds a REST list-branches response body
// where each entry is a (name, sha, protected) triple.
func BranchListResponseProtected(triples ...any) any {
	out := make([]map[string]any, 0, len(triples)/3)
	for i := 0; i+2 < len(triples); i += 3 {
		out = append(out, map[string]any{
			"name":      triples[i],
			"commit":    map[string]any{"sha": triples[i+1]},
			"protected": triples[i+2],
		})
	}
	return out
}

// TagListResponse builds a REST list-tags response body. pairs are
// (name, sha) alternating.
func TagListResponse(pairs ...string) any {
	out := make([]map[string]any, 0, len(pairs)/2)
	for i := 0; i+1 < len(pairs); i += 2 {
		out = append(out, map[string]any{
			"name":   pairs[i],
			"commit": map[string]any{"sha": pairs[i+1]},
		})
	}
	return out
}

// CompareAncestorResponse builds a REST compare response indicating the
// requested sha is an ancestor of the head, with the given merge-base sha.
func CompareAncestorResponse(mergeBaseSHA string) any {
	return map[string]any{
		"status":            "ahead",
		"merge_base_commit": map[string]any{"sha": mergeBaseSHA},
	}
}

// GitRefResponse is a git/ref body for the exact ref qualified (refs/tags/x
// or refs/heads/x) pointing at a commit.
func GitRefResponse(qualified, sha string) any {
	return map[string]any{"ref": qualified, "object": map[string]any{"sha": sha, "type": "commit"}}
}

// NoTagsNamed answers every exact tag lookup in owner/repo with 404, for
// tests whose branches have no same-named tag.
func NoTagsNamed(reg *Registry, owner, repo string) {
	reg.RegisterReusable(REST("GET", `(?i)repos/`+regexp.QuoteMeta(owner+"/"+repo)+`/git/ref/tags/`), StatusResponse(404))
}

// NodeActionYAML is a minimal JavaScript action definition, enough for
// resolution to treat the repository as a leaf action.
const NodeActionYAML = "name: Test Action\nruns:\n  using: node20\n"

// RepoResponse builds one aliased repository from the batched resolve query,
// answering the commit, tag, head and action-file lookups at oid.
func RepoResponse(nameWithOwner, oid, actionYAML string) map[string]any {
	return map[string]any{
		"nameWithOwner": nameWithOwner,
		"commit":        map[string]any{"oid": oid},
		"tag":           map[string]any{"oid": oid},
		"tagRef":        map[string]any{"oid": oid},
		"head":          map[string]any{"oid": oid},
		"headRef":       map[string]any{"oid": oid},
		"object": map[string]any{
			"oid": oid,
			"file": map[string]any{
				"object": map[string]any{"text": actionYAML},
			},
			"fileYaml": nil,
		},
	}
}

// StubRepo registers the resolve query for nameWithOwner ("owner/repo"),
// answering every ref at oid with a JavaScript action.
func (r *Registry) StubRepo(nameWithOwner, oid string) {
	owner, repo, _ := strings.Cut(nameWithOwner, "/")
	r.Register(GraphQLForRepo(owner, repo), JSONResponse(map[string]any{
		"data": map[string]any{"a0": RepoResponse(nameWithOwner, oid, NodeActionYAML)},
	}))
}

// ParityIntact answers a batched parity query as if every locked pin still
// matches its recorded name, commit and ref.
func ParityIntact(req *http.Request) (*http.Response, error) {
	var body struct {
		Variables map[string]any `json:"variables"`
	}
	if err := decodeJSONBody(req, &body); err != nil {
		return nil, err
	}
	data := map[string]any{}
	for i := 0; ; i++ {
		owner, ok := body.Variables[fmt.Sprintf("owner%d", i)].(string)
		if !ok {
			break
		}
		oid := map[string]any{"oid": body.Variables[fmt.Sprintf("oid%d", i)]}
		data[fmt.Sprintf("a%d", i)] = map[string]any{
			"nameWithOwner": owner + "/" + body.Variables[fmt.Sprintf("name%d", i)].(string),
			"commit":        oid,
			"tag":           oid,
			"tagRef":        oid,
			"head":          oid,
			"headRef":       oid,
		}
	}
	return JSONResponse(map[string]any{"data": data})(req)
}

// ParityQuery matches the batched parity query.
var ParityQuery = GraphQL(`commit: object\(oid`)

// RoundTripFunc adapts a function to http.RoundTripper.
type RoundTripFunc func(*http.Request) (*http.Response, error)

// RoundTrip calls f(req).
func (f RoundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

// CountingTransport counts requests that pass through to Next.
type CountingTransport struct {
	Next  http.RoundTripper
	mu    sync.Mutex
	count int
	reqs  []*http.Request
}

// RoundTrip records req and forwards it to Next.
func (c *CountingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	c.mu.Lock()
	c.count++
	c.reqs = append(c.reqs, req)
	c.mu.Unlock()
	return c.Next.RoundTrip(req)
}

// Count returns how many requests have passed through.
func (c *CountingTransport) Count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.count
}

// Paths returns the URL path of each recorded request, for failure messages.
func (c *CountingTransport) Paths() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, len(c.reqs))
	for i, r := range c.reqs {
		out[i] = r.URL.Path
	}
	return out
}

// CountMatching returns how many recorded requests satisfy m.
func (c *CountingTransport) CountMatching(m Matcher) int {
	c.mu.Lock()
	reqs := append([]*http.Request(nil), c.reqs...)
	c.mu.Unlock()
	n := 0
	for _, r := range reqs {
		if m(r) {
			n++
		}
	}
	return n
}
