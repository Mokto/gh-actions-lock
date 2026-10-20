package main

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	parserlock "github.com/github/actions-lockfile/go/pkg/lockfile"
	"github.com/github/gh-actions-lock/cmd/gh-actions-lock/format"
	"github.com/github/gh-actions-lock/internal/ghapi/httpmock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

const graphWorkflow = `
name: ci
on: push
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: acme/parent@v1.0.0
`

const (
	graphParentSHA = "1111111111111111111111111111111111111111"
	graphChildSHA  = "2222222222222222222222222222222222222222"
	graphLeafSHA   = "3333333333333333333333333333333333333333"
)

type graphTransport struct {
	failChild        bool
	movedParent      bool
	movedChild       bool
	unreachableChild bool
	// leafUsesParent closes the graph into a parent → child → leaf → parent cycle.
	leafUsesParent bool
	// parentUsesOwnSub makes acme/parent and its sub-actions use $/sub, so
	// acme/parent/sub calls itself.
	parentUsesOwnSub bool
	// parentUsesSubAtV1 makes acme/parent use acme/parent/sub@v1, a node
	// action, naming v1 even when the workflow narrows acme/parent.
	parentUsesSubAtV1 bool
	// parentUsesChildSHA makes acme/parent use acme/child at its full SHA.
	parentUsesChildSHA bool
	// parentUsesOld makes acme/parent use acme/old@v1, a node action whose
	// repository was renamed to acme/new.
	parentUsesOld bool
	// childSHA overrides acme/child's commit, and parentUsesChildAt makes
	// acme/parent use acme/child at that spelling of it.
	childSHA, parentUsesChildAt string
	// childOnMain answers that acme/child's commit is in its main branch.
	childOnMain bool
	// goneRef is a tag or branch name parity reports as no longer existing.
	goneRef string
}

func (g *graphTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if httpmock.GraphQL(`file: file\(path`)(req) {
		var body struct {
			Variables map[string]string `json:"variables"`
		}
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			return nil, err
		}
		data := map[string]any{}
		for i := 0; ; i++ {
			name, ok := body.Variables[fmt.Sprintf("name%d", i)]
			if !ok {
				break
			}
			ref := strings.TrimSuffix(body.Variables[fmt.Sprintf("expr%d", i)], "^{commit}")
			sha, yml := graphLeafSHA, nodeActionYAML
			switch strings.ToLower(name) {
			case "parent":
				sha = graphParentSHA
				yml = "runs:\n  using: composite\n  steps:\n    - uses: acme/child@v2\n"
				if g.parentUsesOwnSub {
					yml = "runs:\n  using: composite\n  steps:\n    - uses: $/sub\n"
				}
				if g.parentUsesChildAt != "" {
					yml = "runs:\n  using: composite\n  steps:\n    - uses: acme/child@" + g.parentUsesChildAt + "\n"
				}
				if g.parentUsesOld {
					yml = "runs:\n  using: composite\n  steps:\n    - uses: acme/old@v1\n"
				}
				if g.parentUsesChildSHA {
					yml = "runs:\n  using: composite\n  steps:\n    - uses: acme/child@" + graphChildSHA + "\n"
				}
				if g.parentUsesSubAtV1 {
					yml = "runs:\n  using: composite\n  steps:\n    - uses: acme/parent/sub@v1\n"
					if body.Variables[fmt.Sprintf("yml%d", i)] == "sub/action.yml" {
						yml = nodeActionYAML
					}
				}
				if g.movedParent && ref == "v1.0.0" {
					sha, yml = graphLeafSHA, nodeActionYAML
				}
			case "child":
				if g.failChild {
					return httpmock.StatusResponse(http.StatusForbidden)(req)
				}
				sha = graphChildSHA
				if g.childSHA != "" {
					sha = g.childSHA
				}
				if g.movedChild {
					sha = graphLeafSHA
				}
				yml = "runs:\n  using: composite\n  steps:\n    - uses: acme/leaf@v3\n"
			case "old", "new":
				data[fmt.Sprintf("a%d", i)] = testRepoResponse("acme/new", graphLeafSHA, nodeActionYAML)
				continue
			case "leaf":
				if g.leafUsesParent {
					yml = "runs:\n  using: composite\n  steps:\n    - uses: acme/parent@v1.0.0\n"
				}
			default:
				return nil, fmt.Errorf("unexpected fixture repository %s", name)
			}
			data[fmt.Sprintf("a%d", i)] = testRepoResponse("acme/"+name, sha, yml)
		}
		return httpmock.JSONResponse(map[string]any{"data": data})(req)
	}
	if g.goneRef != "" && httpmock.GraphQL(`commit: object\(oid`)(req) {
		var body struct {
			Variables map[string]any `json:"variables"`
		}
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			return nil, err
		}
		data := map[string]any{}
		for i := 0; ; i++ {
			owner, ok := body.Variables[fmt.Sprintf("owner%d", i)].(string)
			if !ok {
				break
			}
			oid := map[string]any{"oid": body.Variables[fmt.Sprintf("oid%d", i)]}
			repo := map[string]any{
				"nameWithOwner": owner + "/" + body.Variables[fmt.Sprintf("name%d", i)].(string),
				"commit":        oid, "tag": oid, "tagRef": oid, "head": oid, "headRef": oid,
			}
			if body.Variables[fmt.Sprintf("tagRef%d", i)] == "refs/tags/"+g.goneRef {
				repo["tag"], repo["tagRef"], repo["head"], repo["headRef"] = nil, nil, nil, nil
			}
			data[fmt.Sprintf("a%d", i)] = repo
		}
		return httpmock.JSONResponse(map[string]any{"data": data})(req)
	}
	if httpmock.GraphQL(`commit: object\(oid`)(req) {
		if g.unreachableChild {
			var body struct {
				Variables map[string]string `json:"variables"`
			}
			if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
				return nil, err
			}
			data := map[string]any{}
			for i := 0; ; i++ {
				name, ok := body.Variables[fmt.Sprintf("name%d", i)]
				if !ok {
					break
				}
				sha := body.Variables[fmt.Sprintf("oid%d", i)]
				repo := testRepoResponse("acme/"+name, sha, nodeActionYAML)
				if name == "child" && sha == graphChildSHA {
					repo["commit"] = nil
				}
				data[fmt.Sprintf("a%d", i)] = repo
			}
			return httpmock.JSONResponse(map[string]any{"data": data})(req)
		}
		return parityOK(req)
	}
	if g.childOnMain && strings.HasPrefix(req.URL.Path, "/repos/acme/child/compare/") {
		return httpmock.JSONResponse(httpmock.CompareAncestorResponse(cmp.Or(g.childSHA, graphChildSHA)))(req)
	}
	if req.Method == http.MethodGet && strings.HasPrefix(req.URL.Path, "/repos/acme/") && strings.Contains(req.URL.Path, "/git/ref/tags/") {
		// No branch here is shadowed by a same-named tag.
		return httpmock.StatusResponse(http.StatusNotFound)(req)
	}
	if req.Method == http.MethodGet && strings.HasPrefix(req.URL.Path, "/repos/acme/") {
		switch req.URL.Path {
		case "/repos/acme/parent/tags":
			return httpmock.JSONResponse(httpmock.TagListResponse("v1.0.0", graphParentSHA, "v1", graphParentSHA))(req)
		case "/repos/acme/child/branches":
			return httpmock.JSONResponse(httpmock.BranchListResponse("main", graphChildSHA))(req)
		case "/repos/acme/new/tags", "/repos/acme/old/tags":
			return httpmock.JSONResponse(httpmock.TagListResponse("v1.0.0", graphLeafSHA, "v1", graphLeafSHA))(req)
		case "/repos/acme/child/tags":
			return httpmock.JSONResponse(httpmock.TagListResponse("v2.1.0", graphChildSHA, "v2", graphChildSHA))(req)
		case "/repos/acme/parent/branches":
			return httpmock.JSONResponse(httpmock.BranchListResponse("main", graphParentSHA))(req)
		case "/repos/acme/parent", "/repos/acme/child", "/repos/acme/leaf", "/repos/acme/old", "/repos/acme/new":
			return httpmock.JSONResponse(map[string]any{
				"id": 1, "owner": map[string]any{"id": 1}, "default_branch": "main",
			})(req)
		}
	}
	return nil, fmt.Errorf("unexpected fixture request %s %s", req.Method, req.URL)
}

func generateGraph(t *testing.T) (string, parserlock.File) {
	t.Helper()
	path := writeTempWorkflow(t, graphWorkflow)
	_, stderr, err := runCommandWithHTTP(t, &graphTransport{}, "--no-narrow", path)
	require.NoError(t, err, stderr)
	file, err := parserlock.Parse([]byte(readTempLockfilePins(t)))
	require.NoError(t, err)
	require.Len(t, file.Dependencies, 3)
	assert.Equal(t, []string{"acme/child@v2"}, file.Dependencies["acme/parent@v1.0.0"].Uses)
	assert.Equal(t, []string{"acme/leaf@v3"}, file.Dependencies["acme/child@v2"].Uses)
	return path, file
}

func writeGraph(t *testing.T, file parserlock.File) {
	t.Helper()
	b, err := yaml.Marshal(file)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(parserlock.Path, b, 0o600))
}

func TestCompleteness_DanglingEntriesFailWithoutWrites(t *testing.T) {
	for _, tt := range []struct {
		name string
		args []string
	}{
		{name: "offline verification", args: []string{"--verify-local"}},
		{name: "online verification", args: []string{"--verify"}},
		{name: "sticky write with onboarding disabled", args: []string{"--no-onboard"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			path, file := generateGraph(t)
			delete(file.Dependencies, "acme/child@v2")
			writeGraph(t, file)
			before := snapshotWorkflows(t)
			var transport http.RoundTripper = &graphTransport{}
			if tt.args[0] == "--verify-local" {
				counter := &requestCountingTransport{}
				transport = counter
				t.Cleanup(func() { assert.Zero(t, counter.calls.Load()) })
			}
			p, err := runParity(t, transport, append(tt.args, "--no-narrow", path)...)
			require.ErrorIs(t, err, errSilent)
			assert.False(t, p.Valid)
			require.Len(t, p.Findings, 1)
			assert.Equal(t, "not-pinned", p.Findings[0].Category)
			assert.Equal(t, "error", p.Findings[0].Severity)
			assert.Contains(t, p.Findings[0].Detail, "acme/child@v2")
			assert.Equal(t, "acme/parent@v1.0.0", p.Findings[0].RequiredBy)
			assert.Equal(t, before, snapshotWorkflows(t))
		})
	}
}

func TestCompleteness_RelockRebuildsWithoutRefMovement(t *testing.T) {
	for _, tt := range []struct {
		name     string
		missing  string
		dropEdge bool
		flag     string
	}{
		{name: "missing root", missing: "acme/parent@v1.0.0", flag: "--relock"},
		{name: "missing edge and child", missing: "acme/child@v2", dropEdge: true, flag: "--relock"},
		{name: "accept-moved repairs missing child", missing: "acme/child@v2", flag: "--accept-moved"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			path, file := generateGraph(t)
			delete(file.Dependencies, tt.missing)
			if tt.dropEdge {
				parent := file.Dependencies["acme/parent@v1.0.0"]
				parent.Uses = nil
				file.Dependencies["acme/parent@v1.0.0"] = parent
			}
			writeGraph(t, file)

			_, stderr, err := runCommandWithHTTP(t, &graphTransport{}, tt.flag, "--no-narrow", path)
			require.NoError(t, err, stderr)
			after := readTempLockfilePins(t)
			repaired, err := parserlock.Parse([]byte(after))
			require.NoError(t, err)
			assert.Len(t, repaired.Dependencies, 3)
			assert.Equal(t, "sha1-"+graphParentSHA, repaired.Dependencies["acme/parent@v1.0.0"].Commit)
			assert.Equal(t, "sha1-"+graphChildSHA, repaired.Dependencies["acme/child@v2"].Commit)
			assert.Equal(t, "sha1-"+graphLeafSHA, repaired.Dependencies["acme/leaf@v3"].Commit)
			assert.Equal(t, []string{"acme/child@v2"}, repaired.Dependencies["acme/parent@v1.0.0"].Uses)
			assert.Equal(t, []string{"acme/leaf@v3"}, repaired.Dependencies["acme/child@v2"].Uses)

			if tt.dropEdge {
				before := snapshotWorkflows(t)
				_, stderr, err = runCommandWithHTTP(t, &graphTransport{}, "--relock", "--no-narrow", path)
				require.NoError(t, err, stderr)
				assert.Equal(t, before, snapshotWorkflows(t), "a repaired graph must converge")
			}
		})
	}
}

func TestCompleteness_VerifyInspectsLockedManifests(t *testing.T) {
	for _, keepChild := range []bool{false, true} {
		t.Run(fmt.Sprintf("child recorded=%v", keepChild), func(t *testing.T) {
			path, file := generateGraph(t)
			parent := file.Dependencies["acme/parent@v1.0.0"]
			parent.Uses = nil
			file.Dependencies["acme/parent@v1.0.0"] = parent
			if !keepChild {
				delete(file.Dependencies, "acme/child@v2")
			}
			writeGraph(t, file)
			before := snapshotWorkflows(t)

			_, _, err := runCommandWithHTTP(t, &httpmock.Registry{}, "--verify-local", path)
			require.NoError(t, err, "offline verification cannot infer an erased remote edge")
			transport := &graphTransport{movedParent: true}
			p, err := runParity(t, transport, "--verify", path)
			require.ErrorIs(t, err, errSilent)
			assert.False(t, p.Valid)
			require.Len(t, p.Findings, 1)
			assert.Contains(t, p.Findings[0].Detail, "lockfile omits that edge")
			assert.Equal(t, "acme/parent@v1.0.0", p.Findings[0].RequiredBy)
			assert.Equal(t, before, snapshotWorkflows(t))
		})
	}
}

func TestCompleteness_RelockReplacesObsoleteEdges(t *testing.T) {
	path, file := generateGraph(t)
	parent := file.Dependencies["acme/parent@v1.0.0"]
	parent.Uses = append(parent.Uses, "acme/obsolete@v1")
	file.Dependencies["acme/parent@v1.0.0"] = parent
	obsolete := file.Dependencies["acme/leaf@v3"]
	obsolete.Ref = "v1"
	file.Dependencies["acme/obsolete@v1"] = obsolete
	writeGraph(t, file)
	_, stderr, err := runCommandWithHTTP(t, &graphTransport{}, "--relock", "--no-narrow", path)
	require.NoError(t, err, stderr)
	file, err = parserlock.Parse([]byte(readTempLockfilePins(t)))
	require.NoError(t, err)
	assert.Len(t, file.Dependencies, 3)
	assert.Equal(t, []string{"acme/child@v2"}, file.Dependencies["acme/parent@v1.0.0"].Uses)
	assert.NotContains(t, file.Dependencies, "acme/obsolete@v1")
}

func TestCompleteness_VerifyManifestFailureIsBlocking(t *testing.T) {
	for _, tt := range []struct {
		name       string
		dropEdge   bool
		categories []string
	}{
		{name: "complete recorded graph", categories: []string{"reachability-unknown"}},
		{name: "omitted edge does not hide inspection failure", dropEdge: true, categories: []string{"not-pinned", "reachability-unknown"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			path, file := generateGraph(t)
			if tt.dropEdge {
				parent := file.Dependencies["acme/parent@v1.0.0"]
				parent.Uses = nil
				file.Dependencies["acme/parent@v1.0.0"] = parent
				writeGraph(t, file)
			}
			before := snapshotWorkflows(t)
			p, err := runParity(t, &graphTransport{failChild: true}, "--verify", path)
			require.ErrorIs(t, err, errSilent)
			assert.False(t, p.Valid)
			var categories []string
			for _, f := range p.Findings {
				categories = append(categories, f.Category)
				assert.Equal(t, "error", f.Severity)
			}
			assert.ElementsMatch(t, tt.categories, categories)
			assert.Equal(t, before, snapshotWorkflows(t))
		})
	}
}

func TestCompleteness_ScopedRunsIgnoreUnrelatedDamage(t *testing.T) {
	path, file := generateGraph(t)
	const other = ".github/workflows/other.yml"
	require.NoError(t, os.WriteFile(other, []byte(strings.Replace(graphWorkflow, "acme/parent@v1.0.0", "acme/missing@v1", 1)), 0o600))
	file.Workflows[other] = []string{"acme/missing@v1"}
	writeGraph(t, file)
	for _, mode := range []string{"--verify-local", "--verify", "--relock"} {
		_, stderr, err := runCommandWithHTTP(t, &graphTransport{}, mode, "--no-narrow", path)
		require.NoError(t, err, stderr)
		result, err := parserlock.Parse([]byte(readTempLockfilePins(t)))
		require.NoError(t, err)
		assert.Equal(t, []string{"acme/missing@v1"}, result.Workflows[other])
		assert.NotContains(t, result.Dependencies, "acme/missing@v1")
	}
}

func TestCompleteness_ScopedRelockCannotAdvanceSharedChild(t *testing.T) {
	path, file := generateGraph(t)
	const other = ".github/workflows/other.yml"
	require.NoError(t, os.WriteFile(other, []byte(graphWorkflow), 0o600))
	file.Workflows[other] = []string{"acme/parent@v1.0.0"}
	writeGraph(t, file)
	before := snapshotWorkflows(t)
	_, _, err := runCommandWithHTTP(t, &graphTransport{movedChild: true}, "--relock", "--no-narrow", path)
	require.ErrorContains(t, err, "cannot refresh shared pin")
	assert.Equal(t, before, snapshotWorkflows(t))
}

func TestCompleteness_RelockCannotAcceptUnreachableTransitivePin(t *testing.T) {
	for name, root := range map[string]string{
		"tag root": "acme/parent@v1.0.0",
		// The recorded closure hangs off acme/parent@v1.0.0, not the SHA.
		"full-SHA root": "acme/parent@" + graphParentSHA + " # v1.0.0",
	} {
		t.Run(name, func(t *testing.T) {
			path, _ := generateGraph(t)
			require.NoError(t, os.WriteFile(path, []byte(strings.Replace(graphWorkflow, "acme/parent@v1.0.0", root, 1)), 0o600))
			before := snapshotWorkflows(t)
			transport := &graphTransport{movedChild: true, unreachableChild: true}
			p, err := runParity(t, transport, "--relock", "--no-narrow", path)
			require.ErrorIs(t, err, errSilent)
			assert.False(t, p.Valid)
			found := false
			for _, f := range p.Findings {
				if f.Category == "unreachable-pin" {
					found = true
					assert.Equal(t, "acme/parent@v1.0.0", f.RequiredBy)
				}
			}
			assert.True(t, found, "%+v", p.Findings)
			assert.Equal(t, before, snapshotWorkflows(t))

			_, stderr, err := runCommandWithHTTP(t, transport, "--accept-moved", "--no-narrow", path)
			require.NoError(t, err, stderr)
			file, err := parserlock.Parse([]byte(readTempLockfilePins(t)))
			require.NoError(t, err)
			assert.Equal(t, "sha1-"+graphLeafSHA, file.Dependencies["acme/child@v2"].Commit)
		})
	}
}

func TestCompleteness_SHARootRetainsCoverageChecks(t *testing.T) {
	for _, mode := range []string{"--verify-local", "--verify"} {
		t.Run(mode, func(t *testing.T) {
			path, file := generateGraph(t)
			root := "acme/parent@" + graphParentSHA + " # v1.0.0"
			require.NoError(t, os.WriteFile(path, []byte(strings.Replace(graphWorkflow, "acme/parent@v1.0.0", root, 1)), 0o600))
			_, stderr, err := runCommandWithHTTP(t, &graphTransport{}, mode, path)
			require.NoError(t, err, stderr)
			delete(file.Dependencies, "acme/child@v2")
			writeGraph(t, file)
			before := snapshotWorkflows(t)
			_, stderr, err = runCommandWithHTTP(t, &graphTransport{}, mode, path)
			require.ErrorIs(t, err, errSilent, stderr)
			assert.Equal(t, before, snapshotWorkflows(t))
		})
	}
}

func TestCompleteness_BlockingDamageCannotWriteOtherWorkflows(t *testing.T) {
	for _, tt := range []struct {
		name      string
		args      []string
		failChild bool
	}{
		{name: "sticky write rejects recorded damage"},
		{name: "relock fails closed on discovery failure", args: []string{"--relock"}, failChild: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, file := generateGraph(t)
			const other = ".github/workflows/new.yml"
			otherContent := []byte(strings.Replace(graphWorkflow, "acme/parent@v1.0.0", "acme/leaf@v3", 1))
			require.NoError(t, os.WriteFile(other, otherContent, 0o600))
			delete(file.Dependencies, "acme/child@v2")
			writeGraph(t, file)
			before := snapshotWorkflows(t)
			args := append(tt.args, "--no-narrow")
			_, stderr, err := runCommandWithHTTP(t, &graphTransport{failChild: tt.failChild}, args...)
			require.Error(t, err, stderr)
			assert.Equal(t, before, snapshotWorkflows(t))
		})
	}
}

func TestCompleteness_CancelledManifestInspectionWritesNothing(t *testing.T) {
	path, _ := generateGraph(t)
	before := snapshotWorkflows(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	reg := &httpmock.Registry{}
	defer reg.Verify(t)
	registerParity(reg)
	reg.Register(httpmock.GraphQL(`file: file\(path`), func(req *http.Request) (*http.Response, error) {
		cancel()
		return nil, req.Context().Err()
	})
	_, stderr, err := runCommandWithHTTPContext(t, ctx, reg, "--verify", path)
	require.ErrorIs(t, err, context.Canceled, stderr)
	assert.Equal(t, before, snapshotWorkflows(t))
}

// TestCompleteness_ProxyTransientManifestReadIsRetry: behind the Dependabot
// proxy a transient manifest read under --verify is a retry (exit 2), like
// resolution and parity, not a blocking finding.
func TestCompleteness_ProxyTransientManifestReadIsRetry(t *testing.T) {
	t.Setenv("GH_ACTIONS_LOCK_DEPENDABOT_PROXY", "1")
	reg := &httpmock.Registry{}
	useDefaultTransport(t, reg)
	reg.Register(httpmock.REST("GET", `^/repos/actions/checkout$`), httpmock.JSONResponse(map[string]any{
		"full_name": "actions/checkout", "id": 1, "owner": map[string]any{"id": 1},
	}))
	for range 2 {
		reg.Register(httpmock.REST("GET", `^/repos/actions/checkout/commits/`+parityTestSHA+`$`), httpmock.JSONResponse(map[string]any{"sha": parityTestSHA}))
	}
	reg.Register(httpmock.REST("GET", `^/repos/actions/checkout/git/ref/tags/v4$`), httpmock.JSONResponse(httpmock.GitRefResponse("refs/tags/v4", parityTestSHA)))
	for range 10 {
		reg.Register(httpmock.REST("GET", `^/repos/actions/checkout/contents/action\.ya?ml$`), httpmock.StatusResponse(http.StatusServiceUnavailable))
	}
	path := writeTempWorkflow(t, `
name: ci
on: push
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
`, "actions/checkout@v4=sha1-"+parityTestSHA)

	_, stderr, err := runCommandWithHTTP(t, reg, "--verify", path)
	require.Error(t, err, stderr)
	assert.NotErrorIs(t, err, errSilent, "a transient read under the proxy is a retry")
	assert.Contains(t, err.Error(), "could not verify locked dependency manifests")
}

// TestCompleteness_UsesCycleWritesNothing: a composite that reaches itself
// can't run or be recorded, through other repositories or its own
// sub-actions, so onboarding it blocks and writes nothing.
func TestCompleteness_UsesCycleWritesNothing(t *testing.T) {
	for name, transport := range map[string]*graphTransport{
		"across repositories": {leafUsesParent: true},
		"within a repository": {parentUsesOwnSub: true},
	} {
		t.Run(name, func(t *testing.T) {
			path := writeTempWorkflow(t, graphWorkflow)
			before := snapshotWorkflows(t)
			p, err := runParity(t, transport, path)
			require.ErrorIs(t, err, errSilent)
			var details []string
			for _, f := range p.Findings {
				details = append(details, f.Detail)
			}
			assert.Contains(t, strings.Join(details, "\n"), "composite action reaches itself")
			assert.Equal(t, before, snapshotWorkflows(t))
		})
	}
}

// TestCompleteness_RelockWorkflowWithoutActionsReleasesSharedPins: a workflow
// that dropped its last action is refreshed with the rest, so its old graph
// can't block another workflow from advancing a pin they used to share.
func TestCompleteness_RelockWorkflowWithoutActionsReleasesSharedPins(t *testing.T) {
	path, file := generateGraph(t)
	const other = ".github/workflows/other.yml"
	require.NoError(t, os.WriteFile(other, []byte(graphWorkflow), 0o600))
	file.Workflows[other] = []string{"acme/parent@v1.0.0"}
	writeGraph(t, file)
	require.NoError(t, os.WriteFile(path, []byte(strings.Replace(graphWorkflow, "uses: acme/parent@v1.0.0", "run: echo hi", 1)), 0o600))

	_, stderr, err := runCommandWithHTTP(t, &graphTransport{movedChild: true}, "--relock", "--no-narrow")
	require.NoError(t, err, stderr)
	file, err = parserlock.Parse([]byte(readTempLockfilePins(t)))
	require.NoError(t, err)
	assert.Equal(t, "sha1-"+graphLeafSHA, file.Dependencies["acme/child@v2"].Commit)
	assert.Empty(t, file.Workflows[".github/workflows/ci.yml"])
}

// TestCompleteness_VerifyChecksFullSHAKeyUnderItsOwnEntry: a full-SHA root
// with its own entry is checked against that entry's edges, not those of a
// sibling entry that happens to pin the same commit.
func TestCompleteness_VerifyChecksFullSHAKeyUnderItsOwnEntry(t *testing.T) {
	path, file := generateGraph(t)
	shaKey := "acme/parent@" + graphParentSHA
	entry := file.Dependencies["acme/parent@v1.0.0"]
	entry.Uses = nil
	file.Dependencies[shaKey] = entry
	wfKey := ".github/workflows/" + filepath.Base(path)
	file.Workflows[wfKey] = append(file.Workflows[wfKey], shaKey)
	writeGraph(t, file)
	require.NoError(t, os.WriteFile(path, []byte(graphWorkflow+"      - uses: "+shaKey+"\n"), 0o600))
	before := snapshotWorkflows(t)

	p, err := runParity(t, &graphTransport{}, "--verify", path)
	require.ErrorIs(t, err, errSilent)
	var blocking []string
	for _, f := range p.Findings {
		if f.Severity == "error" {
			blocking = append(blocking, f.RequiredBy+": "+f.Detail)
		}
	}
	assert.Equal(t, []string{shaKey + ": locked manifest for " + shaKey + " uses acme/child@v2 but the lockfile omits that edge"}, blocking)
	assert.Equal(t, before, snapshotWorkflows(t))
}

// TestCompleteness_TerminalPointsDamagedGraphsAtRelock: a plain run can't
// repair a damaged recorded graph, so read-only modes must not suggest one,
// and must show the --relock remediation.
func TestCompleteness_TerminalPointsDamagedGraphsAtRelock(t *testing.T) {
	for _, tt := range []struct {
		mode      string
		plainHint string
	}{
		{mode: "--verify", plainHint: "Re-run without --verify"},
		{mode: "--verify-local", plainHint: "Run without --verify-local"},
	} {
		t.Run(tt.mode, func(t *testing.T) {
			path, file := generateGraph(t)
			delete(file.Dependencies, "acme/child@v2")
			writeGraph(t, file)
			_, stderr, err := runCommandWithHTTP(t, &graphTransport{}, tt.mode, path)
			require.ErrorIs(t, err, errSilent, stderr)
			assert.Contains(t, stderr, "gh actions-lock --relock")
			assert.NotContains(t, stderr, tt.plainHint)
		})
	}
}

// TestCompleteness_NarrowingKeepsRefsCompositesName: narrowing a workflow's
// v2 to v2.1.0 must not drop v2 when a composite's manifest still names it,
// or the next --verify (and the Actions backend) rejects the lock.
func TestCompleteness_NarrowingKeepsRefsCompositesName(t *testing.T) {
	for _, tt := range []struct {
		name      string
		workflow  string
		transport *graphTransport
		wantEdges map[string][]string
	}{
		{
			name:      "dependency the workflow and a composite both use",
			workflow:  graphWorkflow + "      - uses: acme/child@v2\n",
			transport: &graphTransport{},
			wantEdges: map[string][]string{
				"acme/parent@v1.0.0": {"acme/child@v2"},
				"acme/child@v2":      {"acme/leaf@v3"},
				"acme/child@v2.1.0":  {"acme/leaf@v3"},
			},
		},
		{
			name:      "full SHA the workflow and a composite both use",
			workflow:  graphWorkflow + "      - uses: acme/child@" + graphChildSHA + "\n",
			transport: &graphTransport{parentUsesChildSHA: true},
			wantEdges: map[string][]string{
				"acme/parent@v1.0.0":          {"acme/child@" + graphChildSHA},
				"acme/child@" + graphChildSHA: {"acme/leaf@v3"},
				"acme/child@v2.1.0":           {"acme/leaf@v3"},
			},
		},
		{
			name:      "sub-action named at the ref the workflow narrows",
			workflow:  strings.Replace(graphWorkflow, "acme/parent@v1.0.0", "acme/parent@v1", 1),
			transport: &graphTransport{parentUsesSubAtV1: true},
			wantEdges: map[string][]string{
				"acme/parent@v1.0.0": {"acme/parent@v1"},
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			path := writeTempWorkflow(t, tt.workflow)
			_, stderr, err := runCommandWithHTTP(t, tt.transport, path)
			require.NoError(t, err, stderr)
			file, err := parserlock.Parse([]byte(readTempLockfilePins(t)))
			require.NoError(t, err)
			edges := map[string][]string{}
			for key, action := range file.Dependencies {
				if len(action.Uses) > 0 {
					edges[key] = action.Uses
				}
			}
			assert.Equal(t, tt.wantEdges, edges)

			_, stderr, err = runCommandWithHTTP(t, tt.transport, "--verify", path)
			require.NoError(t, err, stderr)
		})
	}
}

// TestCompleteness_RedirectKeepsCompositeNameOutOfWorkflowRoots: the
// workflow's uses: of a renamed repository moves to the new name, while a
// composite that names the old one keeps it as its own entry, reachable only
// through that composite.
func TestCompleteness_RedirectKeepsCompositeNameOutOfWorkflowRoots(t *testing.T) {
	path := writeTempWorkflow(t, graphWorkflow+"      - uses: acme/old@v1\n")
	transport := &graphTransport{parentUsesOld: true}
	_, stderr, err := runCommandWithHTTP(t, transport, "--no-narrow", path)
	require.NoError(t, err, stderr)
	wf, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Contains(t, string(wf), "uses: acme/new@v1")
	file, err := parserlock.Parse([]byte(readTempLockfilePins(t)))
	require.NoError(t, err)
	assert.Equal(t, []string{"acme/new@v1", "acme/parent@v1.0.0"}, file.Workflows[".github/workflows/"+filepath.Base(path)])
	assert.Equal(t, []string{"acme/old@v1"}, file.Dependencies["acme/parent@v1.0.0"].Uses)
	assert.Contains(t, file.Dependencies, "acme/old@v1")

	_, stderr, err = runCommandWithHTTP(t, transport, "--verify", path)
	require.NoError(t, err, stderr)
}

// TestCompleteness_RedirectNarrowedOntoAnotherRoot: a redirected root that
// narrows onto an entry another root already uses keeps its redirect, so
// the workflow's old name is still rewritten and covered.
func TestCompleteness_RedirectNarrowedOntoAnotherRoot(t *testing.T) {
	// Order matters to the bug: the root that loses the merge comes last.
	workflow := strings.Replace(graphWorkflow, "acme/parent@v1.0.0", "acme/old@v1", 1) + "      - uses: acme/new@v1.0.0\n"
	path := writeTempWorkflow(t, workflow)
	transport := &graphTransport{}
	_, stderr, err := runCommandWithHTTP(t, transport, path)
	require.NoError(t, err, stderr)
	wf, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.NotContains(t, string(wf), "acme/old@", "the redirected name is rewritten")
	file, err := parserlock.Parse([]byte(readTempLockfilePins(t)))
	require.NoError(t, err)
	assert.Equal(t, []string{"acme/new@v1.0.0"}, file.Workflows[".github/workflows/"+filepath.Base(path)])

	_, stderr, err = runCommandWithHTTP(t, transport, "--verify", path)
	require.NoError(t, err, stderr)
}

// TestCompleteness_RedirectedRootAndAliasBothNarrowable: narrowing a
// redirected root keeps it the workflow's root under its new name, while a
// composite's alias for the old name stays a transitive entry at its own ref.
func TestCompleteness_RedirectedRootAndAliasBothNarrowable(t *testing.T) {
	path := writeTempWorkflow(t, graphWorkflow+"      - uses: acme/old@v1\n")
	transport := &graphTransport{parentUsesOld: true}
	_, stderr, err := runCommandWithHTTP(t, transport, path)
	require.NoError(t, err, stderr)
	file, err := parserlock.Parse([]byte(readTempLockfilePins(t)))
	require.NoError(t, err)
	assert.Equal(t, []string{"acme/new@v1.0.0", "acme/parent@v1.0.0"}, file.Workflows[".github/workflows/"+filepath.Base(path)])
	assert.Equal(t, []string{"acme/old@v1"}, file.Dependencies["acme/parent@v1.0.0"].Uses)

	_, stderr, err = runCommandWithHTTP(t, transport, "--verify", path)
	require.NoError(t, err, stderr)
}

// TestCompleteness_VerifyFindsFullSHAEntryInAnyCase: a full SHA names a
// commit in either case, so --verify walks the lockfile entry the
// workflow's differently cased SHA names instead of calling it unlocked.
func TestCompleteness_VerifyFindsFullSHAEntryInAnyCase(t *testing.T) {
	reg := &httpmock.Registry{}
	registerParity(reg)
	registerNodeManifests(reg)
	path := writeTempWorkflow(t, `
name: ci
on: push
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@`+strings.ToUpper(parityTestSHA)+`
`, "actions/checkout@"+parityTestSHA+"=sha1-"+parityTestSHA)
	lock := readTempLockfilePins(t)
	lock = strings.Replace(lock, "ref: '"+parityTestSHA+"'", "ref: 'v4.2.1'", 1)
	require.NoError(t, os.WriteFile(filepath.Join(".github", "workflows", "actions.lock"), []byte(lock), 0o600))

	p, err := runParity(t, reg, "--verify", path)
	require.NoError(t, err, "%+v", p.Findings)
	for _, f := range p.Findings {
		assert.NotEqual(t, "error", f.Severity, "%+v", f)
	}
}

// TestCompleteness_VerifyMatchesTransitiveFullSHAInAnyCase: a composite's
// manifest can name a child by a full SHA cased differently from its
// recorded key. --verify must match that edge and still check the child's
// own descendants.
func TestCompleteness_VerifyMatchesTransitiveFullSHAInAnyCase(t *testing.T) {
	const child = "abcdef0123456789abcdef0123456789abcdef01"
	for _, tt := range []struct {
		name      string
		eraseLeaf bool
	}{
		{name: "complete graph passes"},
		{name: "erased descendant edge still blocks", eraseLeaf: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			path := writeTempWorkflow(t, graphWorkflow)
			childUses := "\n    uses:\n      - 'acme/leaf@v3'"
			if tt.eraseLeaf {
				childUses = ""
			}
			lock := "version: 'v0.0.3'\nworkflows:\n  '.github/workflows/" + filepath.Base(path) + "':\n    - 'acme/parent@v1.0.0'\n" +
				"dependencies:\n" +
				"  'acme/parent@v1.0.0':\n    ref: 'v1.0.0'\n    commit: 'sha1-" + graphParentSHA + "'\n    owner_id: 1\n    repo_id: 1\n    uses:\n      - 'acme/child@" + child + "'\n" +
				"  'acme/child@" + child + "':\n    ref: 'v2'\n    commit: 'sha1-" + child + "'\n    owner_id: 1\n    repo_id: 1" + childUses + "\n" +
				"  'acme/leaf@v3':\n    ref: 'v3'\n    commit: 'sha1-" + graphLeafSHA + "'\n    owner_id: 1\n    repo_id: 1\n"
			require.NoError(t, os.WriteFile(filepath.Join(".github", "workflows", "actions.lock"), []byte(lock), 0o600))
			transport := &graphTransport{childSHA: child, parentUsesChildAt: strings.ToUpper(child)}

			p, err := runParity(t, transport, "--verify", path)
			if !tt.eraseLeaf {
				require.NoError(t, err, "%+v", p.Findings)
				return
			}
			require.ErrorIs(t, err, errSilent)
			var blocking []string
			for _, f := range p.Findings {
				if f.Severity == "error" {
					blocking = append(blocking, f.Detail)
				}
			}
			assert.Equal(t, []string{"locked manifest for acme/child@" + child + " uses acme/leaf@v3 but the lockfile omits that edge"}, blocking)
		})
	}
}

// shaSpellingLock records acme/child at a full SHA under each of keys, as
// roots of the workflow at path. Each key records ref v2 and the leaf edge.
func shaSpellingLock(t *testing.T, path string, keys ...string) {
	t.Helper()
	var roots, deps strings.Builder
	for _, k := range keys {
		roots.WriteString("    - 'acme/child@" + k + "'\n")
		deps.WriteString("  'acme/child@" + k + "':\n    ref: 'v2'\n    commit: 'sha1-" + strings.ToLower(k) + "'\n    owner_id: 1\n    repo_id: 1\n    uses:\n      - 'acme/leaf@v3'\n")
	}
	lock := "version: 'v0.0.3'\nworkflows:\n  '.github/workflows/" + filepath.Base(path) + "':\n" + roots.String() +
		"dependencies:\n" + deps.String() +
		"  'acme/leaf@v3':\n    ref: 'v3'\n    commit: 'sha1-" + graphLeafSHA + "'\n    owner_id: 1\n    repo_id: 1\n"
	require.NoError(t, os.WriteFile(filepath.Join(".github", "workflows", "actions.lock"), []byte(lock), 0o600))
}

// TestRun_WorkflowSHASpellingReachesRecordedEntry: a full SHA names one
// commit in any case, so a workflow planned for another reason reuses the
// entry it already has under another spelling instead of resolving it again.
func TestRun_WorkflowSHASpellingReachesRecordedEntry(t *testing.T) {
	const child = "abcdef0123456789abcdef0123456789abcdef01"
	for _, spelling := range []string{strings.ToUpper(child), child} {
		t.Run(spelling[:6], func(t *testing.T) {
			path := writeTempWorkflow(t, graphWorkflow+"      - uses: acme/child@"+spelling+"\n")
			shaSpellingLock(t, path, child)

			_, stderr, err := runCommandWithHTTP(t, &graphTransport{childSHA: child}, "--no-narrow", path)
			require.NoError(t, err, stderr)
			file, err := parserlock.Parse([]byte(readTempLockfilePins(t)))
			require.NoError(t, err)
			assert.Equal(t, []string{"acme/child@" + child, "acme/parent@v1.0.0"}, file.Workflows[".github/workflows/"+filepath.Base(path)])
			assert.Equal(t, "v2", file.Dependencies["acme/child@"+child].Ref)
			assert.NotContains(t, file.Dependencies, "acme/child@"+strings.ToUpper(child))
		})
	}
}

// TestRun_AmbiguousSHASpellingRefusesWrite: when a full SHA the workflow
// reaches is recorded under several spellings, their refs and edges may
// differ and no rule picks one, so no write mode writes the workflow, even
// when one spelling matches exactly: a blocking finding names them all.
func TestRun_AmbiguousSHASpellingRefusesWrite(t *testing.T) {
	const child = "abcdef0123456789abcdef0123456789abcdef01"
	upper := strings.ToUpper(child)
	for _, tt := range []struct {
		name      string
		spelling  string
		args      []string
		upperHost string
		// fullScan scans every workflow, as --relock must to collapse.
		fullScan bool
	}{
		{name: "no exact spelling", spelling: "AbCdEf0123456789abcdef0123456789abcdef01"},
		{name: "exact spelling", spelling: child},
		{name: "accept-moved", spelling: child, args: []string{"--accept-moved"}},
		// --accept-moved implies a relock internally, but only an explicit
		// --relock may collapse spellings.
		{name: "accept-moved of every workflow", spelling: child, args: []string{"--accept-moved"}, fullScan: true},
		{name: "spellings on two hosts", spelling: child, upperHost: "github.com"},
		// A partial scan can't rewrite the workflows that keep the other
		// spelling, so --relock can't collapse it.
		{name: "relock of some workflows", spelling: child, args: []string{"--relock"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			path := writeTempWorkflow(t, graphWorkflow+"      - uses: acme/child@"+tt.spelling+"\n")
			shaSpellingLock(t, path, child, upper)
			if tt.upperHost != "" {
				lock := strings.Replace(readTempLockfilePins(t), "  'acme/child@"+upper+"':\n", "  'acme/child@"+upper+"':\n    hostname: '"+tt.upperHost+"'\n", 1)
				require.NoError(t, os.WriteFile(filepath.Join(".github", "workflows", "actions.lock"), []byte(lock), 0o600))
			}
			before := readTempLockfilePins(t)
			wfBefore := snapshotWorkflows(t)

			args := append(tt.args, "--no-narrow")
			if !tt.fullScan {
				args = append(args, path)
			}
			p, err := runParity(t, &graphTransport{childSHA: child, childOnMain: true}, args...)
			require.ErrorIs(t, err, errSilent)
			var ambiguous []format.Finding
			for _, f := range p.Findings {
				if f.Severity == "error" && strings.Contains(f.Detail, "several spellings") {
					ambiguous = append(ambiguous, f)
				}
			}
			require.Len(t, ambiguous, 1, "%+v", p.Findings)
			assert.Contains(t, ambiguous[0].Detail, "acme/child@"+child)
			assert.Contains(t, ambiguous[0].Detail, "acme/child@"+upper)
			assert.NotContains(t, ambiguous[0].Remediation, "--relock")
			assert.Equal(t, before, readTempLockfilePins(t))
			assert.Equal(t, wfBefore, snapshotWorkflows(t))
		})
	}
}

// TestRun_VerifyChecksEveryAmbiguousSHASpelling: when a workflow's SHA
// matches several recorded spellings and none exactly, --verify checks each
// one's graph rather than whichever a map happens to return.
func TestRun_VerifyChecksEveryAmbiguousSHASpelling(t *testing.T) {
	const child = "abcdef0123456789abcdef0123456789abcdef01"
	upper := strings.ToUpper(child)
	for _, tt := range []struct {
		name  string
		erase string
		gone  string
		exact bool
	}{
		{name: "both complete passes"},
		{name: "lowercase entry missing an edge blocks", erase: child},
		{name: "uppercase entry missing an edge blocks", erase: upper},
		{name: "uppercase entry whose ref is gone blocks", gone: upper},
		{name: "an exact spelling doesn't hide the other's missing edge", erase: upper, exact: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			spelling := "AbCdEf0123456789abcdef0123456789abcdef01"
			if tt.gone != "" || tt.exact {
				// An exact match doesn't make the other spelling's entry moot.
				spelling = child
			}
			path := writeTempWorkflow(t, "on: push\njobs:\n  test:\n    runs-on: ubuntu-latest\n    steps:\n      - uses: acme/child@"+spelling+"\n")
			shaSpellingLock(t, path, child, upper)
			if tt.erase != "" {
				lock := strings.Replace(readTempLockfilePins(t), "  'acme/child@"+tt.erase+"':\n    ref: 'v2'\n    commit: 'sha1-"+child+"'\n    owner_id: 1\n    repo_id: 1\n    uses:\n      - 'acme/leaf@v3'\n", "  'acme/child@"+tt.erase+"':\n    ref: 'v2'\n    commit: 'sha1-"+child+"'\n    owner_id: 1\n    repo_id: 1\n", 1)
				require.NoError(t, os.WriteFile(filepath.Join(".github", "workflows", "actions.lock"), []byte(lock), 0o600))
			}

			transport := &graphTransport{childSHA: child, childOnMain: true}
			if tt.gone != "" {
				// The workflow's root is the healthy spelling; the other is
				// recorded for another workflow.
				lock := strings.Replace(readTempLockfilePins(t), "  'acme/child@"+tt.gone+"':\n    ref: 'v2'", "  'acme/child@"+tt.gone+"':\n    ref: 'gone'", 1)
				lock = strings.Replace(lock, "    - 'acme/child@"+tt.gone+"'\n", "", 1)
				lock = strings.Replace(lock, "workflows:\n", "workflows:\n  '.github/workflows/other.yml':\n    - 'acme/child@"+tt.gone+"'\n", 1)
				require.NoError(t, os.WriteFile(filepath.Join(".github", "workflows", "actions.lock"), []byte(lock), 0o600))
				transport.goneRef = "gone"
			}

			p, err := runParity(t, transport, "--verify", path)
			var blocking []string
			for _, f := range p.Findings {
				if f.Severity == "error" {
					blocking = append(blocking, f.Detail)
				}
			}
			if tt.gone != "" {
				require.ErrorIs(t, err, errSilent)
				require.Len(t, blocking, 1, "%v", blocking)
				assert.Contains(t, blocking[0], "ref gone is not a tag or branch of acme/child")
				return
			}
			if tt.erase == "" {
				require.NoError(t, err, "%v", blocking)
				return
			}
			require.ErrorIs(t, err, errSilent)
			assert.Equal(t, []string{"locked manifest for acme/child@" + tt.erase + " uses acme/leaf@v3 but the lockfile omits that edge"}, blocking)
		})
	}
}

// TestRun_VerifyLocalFollowsFullSHASpellings: offline verification reaches a
// recorded full SHA from a root or edge spelled in another case, so a lone
// alias is not a missing root, a missing child under it is named as the
// child, and every spelling of an ambiguous SHA is checked.
func TestRun_VerifyLocalFollowsFullSHASpellings(t *testing.T) {
	const sha = "abcdef0123456789abcdef0123456789abcdef01"
	upper := strings.ToUpper(sha)
	entry := func(key, uses string) string {
		e := "  'acme/action@" + key + "':\n    ref: 'v1.0.0'\n    commit: 'sha1-" + sha + "'\n    owner_id: 1\n    repo_id: 1\n"
		if uses != "" {
			e += "    uses:\n      - '" + uses + "'\n"
		}
		return e
	}
	leaf := "  'acme/leaf@v1':\n    ref: 'v1'\n    commit: 'sha1-" + graphLeafSHA + "'\n    owner_id: 1\n    repo_id: 1\n"
	for _, tt := range []struct {
		name, uses, root, deps string
		wantBlocking           []string
	}{
		{name: "uppercase root, lowercase entry", uses: upper, root: upper, deps: entry(sha, "")},
		{name: "lowercase root, uppercase entry", uses: sha, root: sha, deps: entry(upper, "")},
		{
			name: "missing child under an alias", uses: upper, root: upper, deps: entry(sha, "acme/leaf@v1"),
			wantBlocking: []string{"lockfile references acme/leaf@v1 but its dependency metadata is missing (used by acme/action@" + sha + ")"},
		},
		{name: "alias root with a complete child", uses: upper, root: upper, deps: entry(sha, "acme/leaf@v1") + leaf},
		{
			name: "dangling edge on the other spelling of an ambiguous SHA", uses: sha, root: sha,
			deps:         entry(sha, "") + entry(upper, "acme/gone@v1"),
			wantBlocking: []string{"lockfile references acme/gone@v1 but its dependency metadata is missing (used by acme/action@" + upper + ")"},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			path := writeTempWorkflow(t, "on: push\njobs:\n  test:\n    runs-on: ubuntu-latest\n    steps:\n      - uses: acme/action@"+tt.uses+"\n")
			lock := "version: 'v0.0.3'\nworkflows:\n  '.github/workflows/" + filepath.Base(path) + "':\n    - 'acme/action@" + tt.root + "'\ndependencies:\n" + tt.deps
			require.NoError(t, os.WriteFile(filepath.Join(".github", "workflows", "actions.lock"), []byte(lock), 0o600))

			p, err := runParity(t, nil, "--verify-local", path)
			var blocking []string
			for _, f := range p.Findings {
				if f.Severity == "error" {
					blocking = append(blocking, f.Detail)
				}
			}
			assert.Equal(t, tt.wantBlocking, blocking)
			if tt.wantBlocking == nil {
				require.NoError(t, err)
			} else {
				require.ErrorIs(t, err, errSilent)
			}
		})
	}
}

// TestRun_RelockCollapsesSHASpellings: --relock of every workflow rebuilds
// the graph, so a full SHA recorded under several spellings, and named in
// another by a manifest, is recorded once under one canonical spelling, and
// the result verifies.
func TestRun_RelockCollapsesSHASpellings(t *testing.T) {
	for _, args := range [][]string{{"--relock"}, {"--relock", "--accept-moved"}} {
		t.Run(strings.Join(args, " "), func(t *testing.T) { testRelockCollapsesSHASpellings(t, args) })
	}
}

func testRelockCollapsesSHASpellings(t *testing.T, args []string) {
	const child = "abcdef0123456789abcdef0123456789abcdef01"
	upper := strings.ToUpper(child)
	path := writeTempWorkflow(t, graphWorkflow+"      - uses: acme/child@AbCdEf0123456789abcdef0123456789abcdef01\n")
	shaSpellingLock(t, path, child, upper)
	transport := &graphTransport{childSHA: child, childOnMain: true, parentUsesChildAt: upper}

	_, stderr, err := runCommandWithHTTP(t, transport, append(args, "--no-narrow")...)
	require.NoError(t, err, stderr)
	lock := readTempLockfilePins(t)
	file, err := parserlock.Parse([]byte(lock))
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"acme/child@" + child, "acme/leaf@v3", "acme/parent@v1.0.0"}, slices.Collect(maps.Keys(file.Dependencies)))
	assert.Equal(t, []string{"acme/child@" + child}, file.Dependencies["acme/parent@v1.0.0"].Uses)
	assert.ElementsMatch(t, []string{"acme/child@" + child, "acme/parent@v1.0.0"}, file.Workflows[".github/workflows/"+filepath.Base(path)])
	assert.NotContains(t, lock, upper)
	assert.NotContains(t, lock, "AbCdEf")

	_, stderr, err = runCommandWithHTTP(t, transport, "--verify")
	require.NoError(t, err, stderr)

	_, stderr, err = runCommandWithHTTP(t, transport, append(args, "--no-narrow")...)
	require.NoError(t, err, stderr)
	assert.Equal(t, lock, readTempLockfilePins(t), "a second relock converges")
}

// TestRun_PlainRunKeepsLoneSHAAlias: a workflow root recorded in another
// case than its only entry names that entry, so a plain run keeps the entry
// as recorded and records the root under the entry's spelling, once.
func TestRun_PlainRunKeepsLoneSHAAlias(t *testing.T) {
	const sha = "abcdef0123456789abcdef0123456789abcdef01"
	upper := strings.ToUpper(sha)
	for _, tt := range []struct{ name, uses, entry string }{
		{name: "uppercase root, lowercase entry", uses: upper, entry: sha},
		{name: "lowercase root, uppercase entry", uses: sha, entry: upper},
	} {
		t.Run(tt.name, func(t *testing.T) {
			path := writeTempWorkflow(t, "on: push\njobs:\n  test:\n    runs-on: ubuntu-latest\n    steps:\n      - uses: acme/action@"+tt.uses+"\n")
			lock := "version: 'v0.0.3'\nworkflows:\n  '.github/workflows/" + filepath.Base(path) + "':\n    - 'acme/action@" + tt.uses + "'\ndependencies:\n" +
				"  'acme/action@" + tt.entry + "':\n    ref: 'v1.0.0'\n    commit: 'sha1-" + sha + "'\n    owner_id: 1\n    repo_id: 1\n"
			require.NoError(t, os.WriteFile(filepath.Join(".github", "workflows", "actions.lock"), []byte(lock), 0o600))

			_, stderr, err := runCommandWithHTTP(t, parityFallback{&httpmock.Registry{}}, "--no-narrow", path)
			require.NoError(t, err, stderr)
			first := readTempLockfilePins(t)
			file, err := parserlock.Parse([]byte(first))
			require.NoError(t, err)
			assert.Equal(t, []string{"acme/action@" + tt.entry}, file.Workflows[".github/workflows/"+filepath.Base(path)])
			assert.Equal(t, []string{"acme/action@" + tt.entry}, slices.Collect(maps.Keys(file.Dependencies)))
			assert.Equal(t, "v1.0.0", file.Dependencies["acme/action@"+tt.entry].Ref)

			_, stderr, err = runCommandWithHTTP(t, parityFallback{&httpmock.Registry{}}, "--no-narrow", path)
			require.NoError(t, err, stderr)
			assert.Equal(t, first, readTempLockfilePins(t), "a second run converges")
			_, stderr, err = runCommandWithHTTP(t, parityFallback{&httpmock.Registry{}}, "--verify-local", path)
			require.NoError(t, err, stderr)
		})
	}
}

// TestRun_RelockCollapseDiscardsAlternateMetadata: one spelling of an
// ambiguous full SHA records a ref that no longer exists and a child its
// manifest doesn't use. --relock of every workflow replaces both spellings
// with one fresh entry, verified in this run, so the alternate's recorded
// ref and edges don't block and aren't written.
func TestRun_RelockCollapseDiscardsAlternateMetadata(t *testing.T) {
	const child = "abcdef0123456789abcdef0123456789abcdef01"
	upper := strings.ToUpper(child)
	path := writeTempWorkflow(t, graphWorkflow+"      - uses: acme/child@"+child+"\n")
	shaSpellingLock(t, path, child, upper)
	lock := strings.Replace(readTempLockfilePins(t),
		"  'acme/child@"+upper+"':\n    ref: 'v2'\n    commit: 'sha1-"+child+"'\n    owner_id: 1\n    repo_id: 1\n    uses:\n      - 'acme/leaf@v3'\n",
		"  'acme/child@"+upper+"':\n    ref: 'gone'\n    commit: 'sha1-"+child+"'\n    owner_id: 1\n    repo_id: 1\n    uses:\n      - 'acme/leaf@v3'\n      - 'acme/stale@v1'\n"+
			"  'acme/stale@v1':\n    ref: 'v1'\n    commit: 'sha1-"+graphLeafSHA+"'\n    owner_id: 1\n    repo_id: 1\n", 1)
	require.NoError(t, os.WriteFile(filepath.Join(".github", "workflows", "actions.lock"), []byte(lock), 0o600))
	transport := &graphTransport{childSHA: child, childOnMain: true, goneRef: "gone"}

	_, stderr, err := runCommandWithHTTP(t, transport, "--relock", "--no-narrow")
	require.NoError(t, err, stderr)
	after := readTempLockfilePins(t)
	file, err := parserlock.Parse([]byte(after))
	require.NoError(t, err)
	// acme/child@v2 is the composite's own child, at its live commit.
	assert.ElementsMatch(t, []string{"acme/child@" + child, "acme/child@v2", "acme/leaf@v3", "acme/parent@v1.0.0"}, slices.Collect(maps.Keys(file.Dependencies)))
	assert.Equal(t, []string{"acme/leaf@v3"}, file.Dependencies["acme/child@"+child].Uses)
	assert.NotEqual(t, "gone", file.Dependencies["acme/child@"+child].Ref)
	assert.NotContains(t, after, upper)

	_, stderr, err = runCommandWithHTTP(t, transport, "--verify")
	require.NoError(t, err, stderr)
}

func TestRun_ParitySSOGuidance(t *testing.T) {
	const message = "Resource protected by organization SAML enforcement. You must grant your Personal Access token access to this organization."
	for _, mode := range []struct {
		name string
		args []string
	}{
		{"fix", nil},
		{"no-fix", []string{"--no-fix"}},
		{"verify", []string{"--verify"}},
	} {
		t.Run(mode.name, func(t *testing.T) {
			t.Setenv("CODESPACES", "")
			reg := &httpmock.Registry{}
			reg.Register(httpmock.GraphQL(`commit: object\(oid`), func(req *http.Request) (*http.Response, error) {
				resp, err := httpmock.JSONResponse(map[string]string{"message": message})(req)
				if err != nil {
					return nil, err
				}
				resp.StatusCode = http.StatusForbidden
				return resp, nil
			})
			reg.Register(httpmock.REST("GET", `^/repos/krzema12/github-actions-typing$`), httpmock.StatusResponse(http.StatusNotFound))
			path := writeTempWorkflow(t, transferWorkflow, "krzema12/github-actions-typing@v2.2.2=sha1-"+parityTestSHA)
			lockBefore := readTempLockfilePins(t)

			_, stderr, err := runCommandWithHTTP(t, reg, append(mode.args, "--no-interactive", path)...)
			require.ErrorIs(t, err, errSilent)
			assert.Contains(t, stderr, "GitHub returned 403 Forbidden: "+message)
			assert.NotContains(t, stderr, "https://api.github.com/")
			assert.Equal(t, 1, strings.Count(stderr, message))
			if mode.name != "fix" {
				assert.Equal(t, 1, strings.Count(stderr, "gh auth refresh -h github.com"))
			}
			assert.Equal(t, lockBefore, readTempLockfilePins(t))
			reg.Verify(t)
		})
	}
}

func TestRun_ParitySSORateLimitedGuidance(t *testing.T) {
	const message = "Resource protected by organization SAML enforcement."
	for _, mode := range []struct {
		name string
		args []string
	}{
		{"fix", nil},
		{"no-fix", []string{"--no-fix"}},
		{"verify", []string{"--verify"}},
	} {
		t.Run(mode.name, func(t *testing.T) {
			t.Setenv("CODESPACES", "true")
			reg := &httpmock.Registry{}
			reg.Register(httpmock.GraphQL(`commit: object\(oid`), httpmock.JSONResponse(map[string]any{
				"data": map[string]any{"a0": map[string]any{
					"nameWithOwner": "krzema12/github-actions-typing",
					"databaseId":    1,
					"commit":        map[string]any{"oid": parityTestSHA},
					"tag":           map[string]any{"oid": parityTestMoved},
					"tagRef":        map[string]any{"oid": parityTestMoved},
				}},
			}))
			reg.RegisterReusable(httpmock.REST("HEAD", `^/orgs/krzema12$`), httpmock.StatusResponse(http.StatusOK))
			reg.Register(httpmock.REST("GET", `^/repos/krzema12/github-actions-typing/compare/`), func(req *http.Request) (*http.Response, error) {
				resp, err := httpmock.JSONResponse(map[string]string{"message": message})(req)
				if err != nil {
					return nil, err
				}
				resp.StatusCode = http.StatusForbidden
				return resp, nil
			})
			reg.Register(httpmock.REST("GET", `^/repos/krzema12/github-actions-typing/compare/`), httpmock.StatusResponse(http.StatusTooManyRequests))
			useDefaultTransport(t, reg)
			path := writeTempWorkflow(t, transferWorkflow, "krzema12/github-actions-typing@v2.2.2=sha1-"+parityTestSHA)
			lockBefore := readTempLockfilePins(t)

			_, stderr, err := runCommandWithHTTP(t, reg, append(mode.args, "--no-interactive", path)...)
			require.ErrorIs(t, err, errSilent)
			assert.Contains(t, stderr, "SSO authorization required")
			assert.Contains(t, stderr, "Authorize it at https://github.com/orgs/krzema12/sso and retry")
			assert.Contains(t, stderr, "anonymous fallback for public repositories was rate-limited")
			assert.NotContains(t, stderr, "https://api.github.com/")
			assert.Equal(t, 1, strings.Count(stderr, "Authorize it at"))
			if mode.name != "fix" {
				assert.Equal(t, 1, strings.Count(stderr, "Your codespace's GITHUB_TOKEN cannot be SSO-authorized."))
			}
			assert.Equal(t, lockBefore, readTempLockfilePins(t))
			reg.Verify(t)
		})
	}
}
