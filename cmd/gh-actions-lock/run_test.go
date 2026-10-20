package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	parserlock "github.com/github/actions-lockfile/go/pkg/lockfile"
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
