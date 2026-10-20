package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/github/gh-actions-lock/cmd/gh-actions-lock/format"
	"github.com/github/gh-actions-lock/internal/ghapi"

	parserlock "github.com/github/actions-lockfile/go/pkg/lockfile"
	"github.com/github/gh-actions-lock/internal/ghapi/httpmock"
	"github.com/github/gh-actions-lock/internal/testfixture"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

var graphWorkflow = workflowUsing("acme/parent@v1.0.0")

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
			sha, yml := graphLeafSHA, httpmock.NodeActionYAML
			switch strings.ToLower(name) {
			case "parent":
				sha = graphParentSHA
				yml = "runs:\n  using: composite\n  steps:\n    - uses: acme/child@v2\n"
				if g.parentUsesOwnSub {
					yml = "runs:\n  using: composite\n  steps:\n    - uses: $/sub\n"
				}
				if g.parentUsesSubAtV1 {
					yml = "runs:\n  using: composite\n  steps:\n    - uses: acme/parent/sub@v1\n"
					if body.Variables[fmt.Sprintf("yml%d", i)] == "sub/action.yml" {
						yml = httpmock.NodeActionYAML
					}
				}
				if g.movedParent && ref == "v1.0.0" {
					sha, yml = graphLeafSHA, httpmock.NodeActionYAML
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
			case "leaf":
				if g.leafUsesParent {
					yml = "runs:\n  using: composite\n  steps:\n    - uses: acme/parent@v1.0.0\n"
				}
			default:
				return nil, fmt.Errorf("unexpected fixture repository %s", name)
			}
			data[fmt.Sprintf("a%d", i)] = httpmock.RepoResponse("acme/"+name, sha, yml)
		}
		return httpmock.JSONResponse(map[string]any{"data": data})(req)
	}
	if httpmock.ParityQuery(req) {
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
				repo := httpmock.RepoResponse("acme/"+name, sha, httpmock.NodeActionYAML)
				if name == "child" && sha == graphChildSHA {
					repo["commit"] = nil
				}
				data[fmt.Sprintf("a%d", i)] = repo
			}
			return httpmock.JSONResponse(map[string]any{"data": data})(req)
		}
		return httpmock.ParityIntact(req)
	}
	if req.Method == http.MethodGet && strings.HasPrefix(req.URL.Path, "/repos/acme/") {
		switch req.URL.Path {
		case "/repos/acme/parent/tags":
			return httpmock.JSONResponse(httpmock.TagListResponse("v1.0.0", graphParentSHA, "v1", graphParentSHA))(req)
		case "/repos/acme/child/tags":
			return httpmock.JSONResponse(httpmock.TagListResponse("v2.1.0", graphChildSHA, "v2", graphChildSHA))(req)
		case "/repos/acme/parent/git/ref/tags/main":
			return httpmock.StatusResponse(http.StatusNotFound)(req)
		case "/repos/acme/parent/branches":
			return httpmock.JSONResponse(httpmock.BranchListResponse("main", graphParentSHA))(req)
		case "/repos/acme/parent", "/repos/acme/child", "/repos/acme/leaf":
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
			p, err := runCheckJSON(t, transport, append(tt.args, "--no-narrow", path)...)
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
			p, err := runCheckJSON(t, transport, "--verify", path)
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
			p, err := runCheckJSON(t, &graphTransport{failChild: true}, "--verify", path)
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
			p, err := runCheckJSON(t, transport, "--relock", "--no-narrow", path)
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
	reg.Register(httpmock.ParityQuery, httpmock.ParityIntact)
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
	path := writeTempWorkflow(t, workflowUsing("actions/checkout@v4"), "actions/checkout@v4=sha1-"+parityTestSHA)

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
			p, err := runCheckJSON(t, transport, path)
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

	p, err := runCheckJSON(t, &graphTransport{}, "--verify", path)
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

func TestCheckCommand_JSONWithHTTPMocks(t *testing.T) {
	reg := &httpmock.Registry{}
	defer reg.Verify(t)

	reg.StubRepo("actions/checkout", testfixture.CheckoutV6.SHA)
	reg.StubRepo("actions/setup-go", testfixture.SetupGoV6.SHA)

	workflowPath := writeTempWorkflow(t, workflowUsing("actions/checkout@v6", "actions/setup-go@v6"),
		"actions/checkout@v6=sha1-"+testfixture.CheckoutV6.SHA,
		"actions/setup-go@v6=sha1-"+testfixture.SetupGoV6.SHA,
	)

	payload, err := runCheckJSON(t, reg, "--relock", "--no-fix", workflowPath)
	require.NoError(t, err)
	assert.True(t, payload.Valid)
	assert.Empty(t, payload.Findings)
}

func TestCheck_BareSHAUsesExactMajorTagAndVerifiesLocally(t *testing.T) {
	const (
		sha         = "b6e2e70617bc3265edd6dab6c906732b2f1ae151"
		ancestorSHA = "09f2f74827fd0000000000000000000000000000"
	)

	reg := &httpmock.Registry{}

	httpmock.NoTagsNamed(reg, "dawidd6", "action-download-artifact")
	reg.StubRepo("dawidd6/action-download-artifact", sha)
	reg.Register(
		httpmock.REST("GET", `repos/dawidd6/action-download-artifact$`),
		httpmock.JSONResponse(map[string]any{
			"default_branch": "main",
			"id":             2,
			"owner":          map[string]any{"id": 1},
		}),
	)
	reg.Register(
		httpmock.REST("GET", `repos/dawidd6/action-download-artifact/git/ref/heads/main`),
		httpmock.JSONResponse(map[string]any{
			"ref": "refs/heads/main", "object": map[string]any{"sha": sha, "type": "commit"},
		}),
	)
	reg.Register(
		httpmock.REST("GET", `repos/dawidd6/action-download-artifact/tags`),
		httpmock.JSONResponse(httpmock.TagListResponse(
			"v21", sha,
			"v3.1.4", ancestorSHA,
		)),
	)
	reg.Register(
		httpmock.REST("GET", `repos/dawidd6/action-download-artifact/releases`),
		httpmock.JSONResponse([]map[string]any{}),
	)
	reg.Register(
		httpmock.REST("GET", `repos/dawidd6/action-download-artifact/compare/09f2f74827fd0000000000000000000000000000\.\.\.b6e2e70617bc3265edd6dab6c906732b2f1ae151`),
		httpmock.JSONResponse(httpmock.CompareAncestorResponse(ancestorSHA)),
	)

	workflowPath := writeTempWorkflow(t, workflowUsing("dawidd6/action-download-artifact@b6e2e70617bc3265edd6dab6c906732b2f1ae151"))

	_, _, err := runCommandWithHTTP(t, reg, workflowPath)
	require.NoError(t, err)

	workflow, err := os.ReadFile(workflowPath)
	require.NoError(t, err)
	assert.Contains(t, string(workflow), "dawidd6/action-download-artifact@v21")
	assert.NotContains(t, string(workflow), "@v3.1.4")

	lock := readTempLockfilePins(t)
	assert.Contains(t, lock, "'dawidd6/action-download-artifact@v21':")
	assert.Contains(t, lock, "ref: 'v21'")
	assert.Contains(t, lock, "commit: 'sha1-"+sha+"'")
	assert.NotContains(t, lock, "v3.1.4")

	_, _, err = runCommandWithHTTP(t, &httpmock.Registry{}, "--verify-local", workflowPath)
	require.NoError(t, err)
}

func TestCheck_ChangedTagAtSameCommitRekeysAndVerifiesLocally(t *testing.T) {
	const sha = "94de994a9f6fffee200243214e17002e2920bb59"

	reg := &httpmock.Registry{}
	reg.StubRepo("dawidd6/action-send-mail", sha)
	reg.Register(
		httpmock.REST("GET", `repos/dawidd6/action-send-mail/tags`),
		httpmock.JSONResponse(httpmock.TagListResponse("v18", sha, "v3.12.0", sha)),
	)
	reg.Register(
		httpmock.REST("GET", `repos/dawidd6/action-send-mail/releases`),
		httpmock.JSONResponse([]map[string]any{}),
	)

	workflowPath := writeTempWorkflow(t, workflowUsing("dawidd6/action-send-mail@v18"), "dawidd6/action-send-mail@v3.12.0=sha1-"+sha)

	_, _, err := runCommandWithHTTP(t, reg, workflowPath)
	require.NoError(t, err)

	lock := readTempLockfilePins(t)
	assert.Contains(t, lock, "'dawidd6/action-send-mail@v18':")
	assert.Contains(t, lock, "ref: 'v18'")
	assert.Contains(t, lock, "commit: 'sha1-"+sha+"'")
	assert.NotContains(t, lock, "v3.12.0")

	_, _, err = runCommandWithHTTP(t, &httpmock.Registry{}, "--verify-local", workflowPath)
	require.NoError(t, err)
}

// TestCheck_RelockAncestry covers how --relock classifies a locked commit
// against the live ref: unchanged is clean, a moved ref whose history still
// contains the lock is a benign move, a lock outside that history is a
// forgery, and an unanswerable compare is reported as unknown rather than
// guessed.
func TestCheck_RelockAncestry(t *testing.T) {
	locked := testfixture.Synthetic('a')
	live := testfixture.Synthetic('b')
	compare := func(status string, mergeBase string) httpmock.Responder {
		return httpmock.JSONResponse(map[string]any{
			"status":            status,
			"merge_base_commit": map[string]any{"sha": mergeBase},
		})
	}
	tests := []struct {
		name     string
		live     string
		compare  httpmock.Responder
		wantErr  bool
		want     string
		excluded []string
	}{
		{name: "unchanged commit is clean", live: locked},
		{
			name: "lock in moved ref history is ref-moved", live: live,
			compare: compare("ahead", locked), want: "ref-moved",
			excluded: []string{"unreachable-pin"},
		},
		{
			name: "lock outside ref history is unreachable-pin", live: live,
			compare: compare("diverged", testfixture.Synthetic('c')), wantErr: true, want: "unreachable-pin",
			excluded: []string{"ref-moved"},
		},
		{
			name: "rate-limited compare is ancestry-unknown", live: live,
			compare: httpmock.StatusResponse(429), want: "ancestry-unknown",
			excluded: []string{"unreachable-pin", "ref-moved", "valid"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reg := &httpmock.Registry{}
			defer reg.Verify(t)
			reg.StubRepo("example/action", tt.live)
			if tt.compare != nil {
				reg.Register(httpmock.REST("GET", "repos/example/action/compare/"), tt.compare)
			}
			path := writeTempWorkflow(t, workflowUsing("example/action@v1"), "example/action@v1=sha1-"+locked)

			payload, err := runCheckJSON(t, reg, "--relock", "--no-fix", path)
			if tt.wantErr {
				require.ErrorIs(t, err, errSilent)
			} else {
				require.NoError(t, err)
			}
			assert.Equal(t, !tt.wantErr, payload.Valid)
			if tt.want == "" {
				assert.Empty(t, payload.Findings)
				return
			}
			categories := payload.categories()
			assert.True(t, categories[tt.want], "%+v", payload.Findings)
			for _, c := range tt.excluded {
				assert.False(t, categories[c], "unexpected %s: %+v", c, payload.Findings)
			}
			if tt.want == "ancestry-unknown" {
				assert.Contains(t, payload.Findings[0].Detail, "ancestry check inconclusive")
			}
		})
	}
}

func TestCheckCommand_JSONDependenciesWithRequiredBy(t *testing.T) {
	reg := &httpmock.Registry{}
	defer reg.Verify(t)

	compositeYAML := "name: Setup Go\nruns:\n  using: composite\n  steps:\n    - uses: actions/cache/save@v4\n"

	// Per-ref resolution queries (parallel resolver resolves one ref per worker).
	reg.StubRepo("actions/checkout", testfixture.CheckoutV6.SHA)
	reg.Register(
		httpmock.GraphQLForRepo("actions", "setup-go"),
		httpmock.JSONResponse(map[string]any{
			"data": map[string]any{
				"a0": httpmock.RepoResponse("actions/setup-go", "d35c59abb061a4a6fb18e82ac0862c26744d6ab5", compositeYAML),
			},
		}),
	)
	// Transitive dep discovered from composite (cache/save).
	reg.StubRepo("actions/cache", "5a3ec84eff668545956fd18022155c47e93e2684")

	workflowPath := writeTempWorkflow(t, workflowUsing("actions/checkout@v6", "actions/setup-go@v6"),
		"actions/checkout@v6",
		"actions/setup-go@v6",
		// Transitive dependency (via actions/setup-go@v6).
		"actions/cache@v4",
	)

	// Test per-workflow dependencies view
	stdout, _, err := runCommandWithHTTP(t, reg,
		"--relock", "--no-fix", "--json=workflows", workflowPath,
	)
	require.NoError(t, err)

	var payload struct {
		Workflows []struct {
			Path         string              `json:"path"`
			Valid        bool                `json:"valid"`
			Dependencies []format.Dependency `json:"dependencies"`
		} `json:"workflows"`
	}
	require.NoError(t, json.Unmarshal([]byte(stdout), &payload))
	require.Len(t, payload.Workflows, 1)

	wf := payload.Workflows[0]
	assert.True(t, wf.Valid)
	require.Len(t, wf.Dependencies, 3)

	// Find the transitive dep
	var transitiveDep *format.Dependency
	for i := range wf.Dependencies {
		if wf.Dependencies[i].NWO == "actions/cache" {
			transitiveDep = &wf.Dependencies[i]
			break
		}
	}
	require.NotNil(t, transitiveDep, "transitive dep actions/cache should be present")
	assert.False(t, transitiveDep.Direct)
	assert.Equal(t, []string{"actions/setup-go@v6"}, transitiveDep.RequiredBy)

	// Direct deps should NOT have required_by
	for _, d := range wf.Dependencies {
		if d.Direct {
			assert.Empty(t, d.RequiredBy, "direct dep %s should not have required_by", d.NWO)
		}
	}
}

func TestCheckCommand_JSONDependenciesInfersRequiredByWithoutComments(t *testing.T) {
	reg := &httpmock.Registry{}
	defer reg.Verify(t)

	compositeYAML := "name: Setup Go\nruns:\n  using: composite\n  steps:\n    - uses: actions/cache/save@v4\n"

	reg.Register(
		httpmock.GraphQLForRepo("actions", "setup-go"),
		httpmock.JSONResponse(map[string]any{
			"data": map[string]any{
				"a0": httpmock.RepoResponse("actions/setup-go", "d35c59abb061a4a6fb18e82ac0862c26744d6ab5", compositeYAML),
			},
		}),
	)
	reg.StubRepo("actions/cache", "5a3ec84eff668545956fd18022155c47e93e2684")

	workflowPath := writeTempWorkflow(t, workflowUsing("actions/setup-go@v6"),
		"actions/setup-go@v6",
		"actions/cache@v4",
	)

	stdout, _, err := runCommandWithHTTP(t, reg,
		"--relock", "--no-fix", "--json=workflows", workflowPath,
	)
	require.NoError(t, err)

	var payload struct {
		Workflows []struct {
			Dependencies []format.Dependency `json:"dependencies"`
		} `json:"workflows"`
	}
	require.NoError(t, json.Unmarshal([]byte(stdout), &payload))
	require.Len(t, payload.Workflows, 1)

	var transitiveDep *format.Dependency
	for i := range payload.Workflows[0].Dependencies {
		if payload.Workflows[0].Dependencies[i].NWO == "actions/cache" {
			transitiveDep = &payload.Workflows[0].Dependencies[i]
			break
		}
	}
	require.NotNil(t, transitiveDep, "transitive dep actions/cache should be present")
	assert.False(t, transitiveDep.Direct)
	assert.Equal(t, []string{"actions/setup-go@v6"}, transitiveDep.RequiredBy)
}

func TestCheckCommand_JSONDefaultFieldsExcludesDependencies(t *testing.T) {
	reg := &httpmock.Registry{}
	defer reg.Verify(t)

	reg.StubRepo("actions/checkout", testfixture.CheckoutV6.SHA)

	workflowPath := writeTempWorkflow(t, workflowUsing("actions/checkout@v6"),
		"actions/checkout@v6",
	)

	// --json with no value should use the default fields (valid,findings,workflows)
	stdout, _, err := runCommandWithHTTP(t, reg,
		"--relock", "--no-fix", "--json", workflowPath,
	)
	require.NoError(t, err)

	var raw map[string]json.RawMessage
	require.NoError(t, json.Unmarshal([]byte(stdout), &raw))

	// Default should include valid, findings, workflows
	assert.Contains(t, raw, "valid")
	assert.Contains(t, raw, "findings")
	assert.Contains(t, raw, "workflows")
	// Default should NOT include dependencies (opt-in only)
	assert.NotContains(t, raw, "dependencies", "default --json should not include top-level dependencies to avoid duplication with workflows")
}

// TestCheck_SeedFromLockfile_SkipsHTTPForCachedDeps verifies that
// SeedFromLockfile pre-warms the resolution cache so known deps skip
// network calls, while new deps still resolve from the network.
// The workflow has two deps: checkout (in lockfile) and setup-go (not in
// lockfile). Only setup-go should hit the HTTP mock.
func TestCheck_SeedFromLockfile_SkipsHTTPForCachedDeps(t *testing.T) {
	reg := &httpmock.Registry{}
	defer reg.Verify(t)

	checkoutSHA := testfixture.CheckoutV6.SHA
	setupGoSHA := testfixture.SetupGoV6.SHA

	// Only register an HTTP stub for setup-go (the NEW dep).
	// No stub for checkout — the seed must serve it from cache.
	reg.StubRepo("actions/setup-go", setupGoSHA)

	// Workflow references both, but lockfile only has checkout.
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, ".github", "workflows"), 0o755))
	wfBody := strings.TrimSpace(workflowUsing("actions/checkout@v6", "actions/setup-go@v6")) + "\n"
	wfPath := filepath.Join(dir, ".github", "workflows", "workflow.yml")
	require.NoError(t, os.WriteFile(wfPath, []byte(wfBody), 0o600))

	// Lockfile records ONLY checkout — setup-go is "new".
	lockYAML := "version: '" + parserlock.Version + "'\ndependencies:\n" +
		"  'actions/checkout@v6" + "':\n" +
		"    ref: 'v6'\n    commit: 'sha1-" + checkoutSHA + "'\n    owner_id: 1\n    repo_id: 1\n" +
		"workflows:\n" +
		"  '.github/workflows/workflow.yml':\n" +
		"    - 'actions/checkout@v6" + "'\n"
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".github", "workflows", "actions.lock"), []byte(lockYAML), 0o600))
	t.Chdir(dir)

	// Locked checkout is only parity-checked, never re-resolved.
	reg.Register(httpmock.ParityQuery, httpmock.ParityIntact)
	stdout, _, err := runCommandWithHTTP(t, reg,
		"--no-fix", "--json=valid,findings",
		".github/workflows/workflow.yml",
	)
	// setup-go is resolved but not yet pinned → "not-pinned" finding → errSilent.
	// That's expected: we're testing that checkout was served from cache, not
	// that the overall check passes.
	require.ErrorIs(t, err, errSilent)

	var payload struct {
		Valid    bool             `json:"valid"`
		Findings []format.Finding `json:"findings"`
	}
	require.NoError(t, json.Unmarshal([]byte(stdout), &payload))

	// The finding should be about setup-go being unpinned, NOT about checkout.
	// If checkout required an HTTP call, reg.Verify would fail (no stub registered).
	require.Len(t, payload.Findings, 1)
	assert.Equal(t, "not-pinned", payload.Findings[0].Category)
	assert.Contains(t, payload.Findings[0].Dependency, "setup-go")
}

// TestCheck_NoFix_WritesNothing proves --no-fix is read-only: it reports
// findings and returns a non-zero exit, but never touches the workflow or the
// lockfile on disk. setup-go is unpinned (a blocking finding under --no-fix),
// so the run exits errSilent while leaving both files byte-for-byte unchanged.
func TestCheck_NoFix_WritesNothing(t *testing.T) {
	reg := &httpmock.Registry{}
	defer reg.Verify(t)

	checkoutSHA := testfixture.CheckoutV6.SHA
	setupGoSHA := testfixture.SetupGoV6.SHA

	// setup-go is the unpinned (new) dep — it resolves from the mock. checkout
	// is seeded from the lockfile and must not hit the network.
	reg.StubRepo("actions/setup-go", setupGoSHA)

	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, ".github", "workflows"), 0o755))
	wfBody := strings.TrimSpace(workflowUsing("actions/checkout@v6", "actions/setup-go@v6")) + "\n"
	wfPath := filepath.Join(dir, ".github", "workflows", "workflow.yml")
	require.NoError(t, os.WriteFile(wfPath, []byte(wfBody), 0o600))

	lockYAML := "version: '" + parserlock.Version + "'\ndependencies:\n" +
		"  'actions/checkout@v6" + "':\n" +
		"    ref: 'v6'\n    commit: 'sha1-" + checkoutSHA + "'\n    owner_id: 1\n    repo_id: 1\n" +
		"workflows:\n" +
		"  '.github/workflows/workflow.yml':\n" +
		"    - 'actions/checkout@v6" + "'\n"
	lockPath := filepath.Join(dir, ".github", "workflows", "actions.lock")
	require.NoError(t, os.WriteFile(lockPath, []byte(lockYAML), 0o600))
	t.Chdir(dir)

	wfBefore, err := os.ReadFile(wfPath)
	require.NoError(t, err)
	lockBefore, err := os.ReadFile(lockPath)
	require.NoError(t, err)

	// Terminal mode (no --json), read-only. setup-go is unpinned → !valid →
	// errSilent.
	_, _, runErr := runCommandWithHTTP(t, reg,
		"--no-fix", ".github/workflows/workflow.yml",
	)
	require.ErrorIs(t, runErr, errSilent)

	wfAfter, err := os.ReadFile(wfPath)
	require.NoError(t, err)
	lockAfter, err := os.ReadFile(lockPath)
	require.NoError(t, err)

	assert.Equal(t, string(wfBefore), string(wfAfter), "--no-fix must not modify the workflow")
	assert.Equal(t, string(lockBefore), string(lockAfter), "--no-fix must not modify the lockfile")
}

// TestCheck_DefaultJSON_AutofixWrites proves --json is a renderer, not a
// read-only switch: a bare --json run (no --no-fix) still autofixes. setup-go
// is unpinned but fixable, so the pipeline pins it, writes the lockfile, and
// exits 0 — while stdout carries the JSON render. The single dep uses a
// full-semver ref (v6.0.0), so neither tag narrowing nor reverse lookup needs
// the network; the remaining metadata stub covers the lockfile write.
func TestCheck_DefaultJSON_AutofixWrites(t *testing.T) {
	reg := &httpmock.Registry{}
	defer reg.Verify(t)

	setupGoSHA := testfixture.SetupGoV6.SHA

	// Resolve setup-go@v6.0.0 → setupGoSHA.
	reg.StubRepo("actions/setup-go", setupGoSHA)
	repoMeta := httpmock.JSONResponse(map[string]any{
		"id":    2,
		"owner": map[string]any{"id": 1},
	})
	reg.Register(httpmock.REST("GET", `repos/actions/setup-go$`), repoMeta)

	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, ".github", "workflows"), 0o755))
	wfBody := strings.TrimSpace(workflowUsing("actions/setup-go@v6.0.0")) + "\n"
	wfPath := filepath.Join(dir, ".github", "workflows", "workflow.yml")
	require.NoError(t, os.WriteFile(wfPath, []byte(wfBody), 0o600))
	t.Chdir(dir)

	// Bare --json: renderer only, autofix still runs.
	stdout, _, err := runCommandWithHTTP(t, reg,
		"--json=valid,findings",
		".github/workflows/workflow.yml",
	)
	require.NoError(t, err)

	// stdout is the JSON render — proves --json still selects the renderer.
	var payload struct {
		Valid    bool             `json:"valid"`
		Findings []format.Finding `json:"findings"`
	}
	require.NoError(t, json.Unmarshal([]byte(stdout), &payload))

	// The lockfile gained the setup-go pin — proves --json no longer implies
	// read-only.
	pins := readTempLockfilePins(t)
	assert.Contains(t, pins, "actions/setup-go", "autofix should have pinned setup-go")
}

// TestCheck_Relock_DetectsRefMovementDespiteLockfile is a regression test
// ensuring --relock does NOT seed the resolution cache. If seeding occurred,
// the resolver would return the stale lockfile SHA and the ref-moved finding
// would be suppressed — exactly the bug we fixed.
func TestCheck_Relock_DetectsRefMovementDespiteLockfile(t *testing.T) {
	reg := &httpmock.Registry{}
	defer reg.Verify(t)

	staleSHA := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	liveSHA := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

	// HTTP returns the LIVE SHA (different from what the lockfile recorded).
	reg.StubRepo("example/action", liveSHA)

	// Compare API: staleSHA is a legitimate ancestor of liveSHA (tag moved forward).
	reg.Register(
		httpmock.REST("GET", "repos/example/action/compare/"),
		httpmock.JSONResponse(map[string]any{
			"status": "ahead",
			"merge_base_commit": map[string]any{
				"sha": staleSHA,
			},
		}),
	)

	workflowPath := writeTempWorkflow(t, workflowUsing("example/action@v1"),
		"example/action@v1=sha1-"+staleSHA,
	)

	stdout, _, err := runCommandWithHTTP(t, reg,
		"--relock", "--no-fix", "--json=valid,findings", workflowPath,
	)
	// ref-moved is a warning (valid=true), not an error.
	require.NoError(t, err, "ref-moved is a warning, should not error")

	var payload struct {
		Valid    bool             `json:"valid"`
		Findings []format.Finding `json:"findings"`
	}
	require.NoError(t, json.Unmarshal([]byte(stdout), &payload))
	assert.True(t, payload.Valid, "ref-moved is a warning")

	hasRefMoved := false
	for _, f := range payload.Findings {
		if f.Category == "ref-moved" {
			hasRefMoved = true
			assert.Equal(t, "run `gh actions-lock --relock` to refresh the lock entry", f.Remediation)
		}
	}
	assert.True(t, hasRefMoved,
		"--relock must detect ref movement (stale lockfile SHA vs live SHA); "+
			"if this fails, SeedFromLockfile is poisoning the resolution cache during relock: %+v",
		payload.Findings)
}

func TestCheckCommand_JSONDeduplicatesDependencies(t *testing.T) {
	// When two workflow files share the same dep, top-level dependencies
	// should deduplicate them.
	reg := &httpmock.Registry{}
	defer reg.Verify(t)

	// No HTTP stub: both workflows are fully recorded in the lockfile, so
	// the fast path skips every network round-trip. The dedup logic under
	// test operates purely on the inventory built from disk.

	wf1 := writeTempWorkflow(t, workflowUsing("actions/checkout@v6"),
		"actions/checkout@v6",
	)

	wf2Path := filepath.Join(filepath.Dir(wf1), "workflow2.yml")
	require.NoError(t, os.WriteFile(wf2Path, []byte(strings.TrimSpace(`
name: deploy
on: push
jobs:
  deploy:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v6
`)+"\n"), 0o600))

	// Add wf2's deps to the lockfile (writeTempWorkflow only seeded wf1).
	writeTempLockfile(t, ".", "workflow.yml",
		[]string{"actions/checkout@v6"})
	// Replace with a multi-workflow lockfile.
	lockYAML := "version: " + parserlock.Version + "\n" +
		"dependencies:\n" +
		"  actions/checkout@v6:\n" +
		"    ref: v6\n    commit: sha1-" + testfixture.CheckoutV6.SHA + "\n    owner_id: 1\n    repo_id: 1\n" +
		"workflows:\n" +
		"  .github/workflows/workflow.yml:\n" +
		"    - actions/checkout@v6\n" +
		"  .github/workflows/workflow2.yml:\n" +
		"    - actions/checkout@v6\n"
	require.NoError(t, os.WriteFile(filepath.Join(".github", "workflows", "actions.lock"), []byte(lockYAML), 0o600))

	stdout, _, err := runCommandWithHTTP(t, reg,
		"--no-fix", "--json=dependencies", wf1, wf2Path,
	)
	require.NoError(t, err)

	var payload struct {
		Dependencies []format.Dependency `json:"dependencies"`
	}
	require.NoError(t, json.Unmarshal([]byte(stdout), &payload))

	// Should be deduplicated — 1 dep, not 2
	assert.Len(t, payload.Dependencies, 1, "top-level dependencies should be deduplicated across workflows")
	assert.Equal(t, "actions/checkout", payload.Dependencies[0].NWO)
}

func TestCheckCommand_JSONLoadErrorIsInvalid(t *testing.T) {
	reg := &httpmock.Registry{}
	defer reg.Verify(t)

	missingPath := filepath.Join(t.TempDir(), "missing.yml")
	stdout, _, err := runCommandWithHTTP(t, reg, "--no-fix", "--json=valid,findings", missingPath)
	require.ErrorIs(t, err, errSilent)

	var payload struct {
		Valid    bool             `json:"valid"`
		Findings []format.Finding `json:"findings"`
	}
	require.NoError(t, json.Unmarshal([]byte(stdout), &payload))
	assert.False(t, payload.Valid)
	require.Len(t, payload.Findings, 1)
	assert.Equal(t, "error", payload.Findings[0].Severity)
}

func TestCheckCommand_LoadErrorFailsFixMode(t *testing.T) {
	for _, tt := range []struct {
		name string
		args []string
	}{
		{name: "migration enabled"},
		{name: "migration disabled", args: []string{"--no-migrate-local-actions"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			reg := &httpmock.Registry{}
			defer reg.Verify(t)

			workflowPath := writeTempWorkflow(t, "name: [")
			args := append(tt.args, workflowPath)
			_, stderr, err := runCommandWithHTTP(t, reg, args...)
			require.Error(t, err)
			if tt.name == "migration enabled" {
				assert.Contains(t, err.Error(), "parsing workflow YAML")
			} else {
				require.ErrorIs(t, err, errSilent)
			}
			assert.NotContains(t, stderr, "All 1 workflow valid")
		})
	}
}

// TestCheck_Relock_BumpsMovedBranchRef covers github/actions-dispatch#751:
// a branch ref (main) whose upstream head advanced is trusted as-is on a
// normal run. --relock must
// re-resolve it and rewrite the lockfile to the new live SHA.
func TestCheck_Relock_BumpsMovedBranchRef(t *testing.T) {
	reg := &httpmock.Registry{}

	staleSHA := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	liveSHA := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

	reg.StubRepo("example/action", liveSHA)
	// staleSHA is a legitimate ancestor of liveSHA (branch advanced) → ref-moved.
	reg.Register(
		httpmock.REST("GET", "repos/example/action/compare/"),
		httpmock.JSONResponse(map[string]any{
			"status": "ahead",
			"merge_base_commit": map[string]any{
				"sha": staleSHA,
			},
		}),
	)
	// Reverse-lookup stubs: main resolves to the live head; no tags.
	reg.Register(
		httpmock.REST("GET", "repos/example/action/branches"),
		httpmock.JSONResponse([]any{
			map[string]any{"name": "main", "commit": map[string]any{"sha": liveSHA}},
		}),
	)
	reg.Register(
		httpmock.REST("GET", "repos/example/action/tags"),
		httpmock.JSONResponse([]any{}),
	)

	workflowPath := writeTempWorkflow(t, workflowUsing("example/action@main"),
		"example/action@main=sha1-"+staleSHA,
	)

	stdout, _, err := runCommandWithHTTP(t, reg,
		"--relock", "--no-narrow", workflowPath,
	)
	require.NoError(t, err, "stdout:\n%s", stdout)

	pins := readTempLockfilePins(t)
	assert.Contains(t, pins, liveSHA, "--relock should rewrite the lockfile to the live SHA")
	assert.NotContains(t, pins, staleSHA, "--relock should drop the stale SHA")
}

// TestCheck_DefaultRun_DoesNotBumpBranchRef is the counterpart to the relock
// test: without --relock a mutable branch ref is trusted from the lockfile
// (fast path, no network) and its recorded SHA is left as-is.
func TestCheck_DefaultRun_DoesNotBumpBranchRef(t *testing.T) {
	reg := &httpmock.Registry{}
	defer reg.Verify(t) // no stubs should be hit on the fast path

	staleSHA := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

	workflowPath := writeTempWorkflow(t, workflowUsing("example/action@main"),
		"example/action@main=sha1-"+staleSHA,
	)

	_, _, err := runCommandWithHTTP(t, reg, workflowPath)
	require.NoError(t, err)

	assert.Contains(t, readTempLockfilePins(t), staleSHA,
		"a default run must not bump a trusted branch ref")
}

// TestCheck_MixedCaseUsesAcrossWorkflowsPinsEach pins the same action written
// with two casings in two workflows. The resolver cache folds owner/repo case,
// so both workflows get back the first spelling; every workflow must still
// record the pin as a direct dependency instead of an empty list.
func TestCheck_MixedCaseUsesAcrossWorkflowsPinsEach(t *testing.T) {
	reg := &httpmock.Registry{}
	defer reg.Verify(t)

	checkoutSHA := testfixture.CheckoutV4.SHA
	reg.StubRepo("actions/checkout", checkoutSHA)
	reg.Register(httpmock.REST("GET", `repos/actions/checkout$`), httpmock.JSONResponse(map[string]any{
		"id":    2,
		"owner": map[string]any{"id": 1},
	}))

	dir := t.TempDir()
	wfDir := filepath.Join(dir, ".github", "workflows")
	require.NoError(t, os.MkdirAll(wfDir, 0o755))
	write := func(name, uses string) {
		body := "on: push\njobs:\n  a:\n    runs-on: ubuntu-latest\n    steps:\n      - uses: " + uses + "\n"
		require.NoError(t, os.WriteFile(filepath.Join(wfDir, name), []byte(body), 0o600))
	}
	write("a.yml", "actions/checkout@v4.2.2")
	write("b.yml", "Actions/Checkout@v4.2.2")
	t.Chdir(dir)

	_, _, err := runCommandWithHTTP(t, reg)
	require.NoError(t, err)

	data, err := os.ReadFile(filepath.Join(wfDir, "actions.lock"))
	require.NoError(t, err)
	file, err := parserlock.Parse(data)
	require.NoError(t, err)
	want := []string{"actions/checkout@v4.2.2"}
	assert.Equal(t, want, file.Workflows[".github/workflows/a.yml"])
	assert.Equal(t, want, file.Workflows[".github/workflows/b.yml"])
}

// TestCheck_BareSHAOffHistoryRefuses: a bare SHA no upstream tag or branch
// contains (a fork commit served under the upstream name) is refused with
// its own finding, the same in the terminal and in --json, and nothing is
// written.
func TestCheck_BareSHAOffHistoryRefuses(t *testing.T) {
	var (
		impostor = "11d0dfc58a242e392d9695aa26812e1d1eb8e439"
		head     = testfixture.CheckoutV4.SHA
	)
	stubs := func() *httpmock.Registry {
		reg := &httpmock.Registry{}
		reg.Register(
			httpmock.GraphQLForRepo("actions", "checkout"),
			httpmock.JSONResponse(map[string]any{
				"data": map[string]any{"a0": httpmock.RepoResponse("actions/checkout", impostor, httpmock.NodeActionYAML)},
			}),
		)
		reg.Register(
			httpmock.REST("GET", `repos/actions/checkout$`),
			httpmock.JSONResponse(map[string]any{"default_branch": "main", "id": 2, "owner": map[string]any{"id": 1}}),
		)
		reg.Register(
			httpmock.REST("GET", `repos/actions/checkout/git/ref/heads/main`),
			httpmock.JSONResponse(map[string]any{"ref": "refs/heads/main", "object": map[string]any{"sha": head, "type": "commit"}}),
		)
		reg.Register(httpmock.REST("GET", `repos/actions/checkout/tags`), httpmock.JSONResponse(httpmock.TagListResponse("v4.2.2", head)))
		reg.Register(httpmock.REST("GET", `repos/actions/checkout/releases`), httpmock.JSONResponse([]map[string]any{}))
		for range 4 {
			reg.Register(httpmock.REST("GET", `repos/actions/checkout/branches`), httpmock.JSONResponse(httpmock.BranchListResponse("main", head)))
			reg.Register(
				httpmock.REST("GET", `repos/actions/checkout/compare/`),
				httpmock.JSONResponse(map[string]any{"status": "diverged", "merge_base_commit": map[string]any{"sha": head}}),
			)
		}
		return reg
	}
	workflowPath := writeTempWorkflow(t, `
name: ci
on: push
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@`+impostor+`
`)

	_, stderr, err := runCommandWithHTTP(t, stubs(), workflowPath)
	require.ErrorIs(t, err, errSilent)
	assert.Contains(t, stderr, "Pinned commit not in upstream history actions/checkout@"+impostor)
	assert.Contains(t, stderr, "fork or impostor commit")
	assert.NotContains(t, stderr, "could not be resolved")
	assert.NotContains(t, stderr, "Not pinned")
	assert.NoFileExists(t, filepath.Join(filepath.Dir(workflowPath), "actions.lock"))

	stdout, _, err := runCommandWithHTTP(t, stubs(), "--json=valid,findings", workflowPath)
	require.Error(t, err)
	var payload struct {
		Valid    bool             `json:"valid"`
		Findings []format.Finding `json:"findings"`
	}
	require.NoError(t, json.Unmarshal([]byte(stdout), &payload), stdout)
	assert.False(t, payload.Valid)
	var cats []string
	for _, f := range payload.Findings {
		cats = append(cats, f.Category)
	}
	assert.Contains(t, cats, "unreachable-pin")
	assert.NotContains(t, cats, "not-pinned")

	_, _, err = runCommandWithHTTP(t, &httpmock.Registry{}, "--verify-local", workflowPath)
	require.ErrorIs(t, err, errSilent, "an unlocked bare SHA must not pass the offline gate")
}

// TestFreshMalformedRemoteRefCannotOnboard: a remote-looking uses: value the
// parser rejects is never skipped, in a workflow or a $/ composite, in
// any mode or transport. Nothing is fetched or written.
func TestFreshMalformedRemoteRefCannotOnboard(t *testing.T) {
	for _, rejected := range []string{"actions/checkout@main:action.yml", "actions/checkout/.github/workflows/ci.yml@v4"} {
		t.Run(rejected, func(t *testing.T) { testMalformedRemoteRefCannotOnboard(t, rejected) })
	}
}

func testMalformedRemoteRefCannotOnboard(t *testing.T, rejected string) {
	for _, surface := range []string{"workflow", "self composite"} {
		for _, proxy := range []bool{false, true} {
			for _, refresh := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/proxy=%t/refresh=%t", surface, proxy, refresh), func(t *testing.T) {
					if proxy {
						t.Setenv("GH_ACTIONS_LOCK_DEPENDABOT_PROXY", "1")
					}
					dir := t.TempDir()
					require.NoError(t, os.Mkdir(filepath.Join(dir, ".git"), 0o755))
					workflowPath := filepath.Join(dir, ".github", "workflows", "ci.yml")
					require.NoError(t, os.MkdirAll(filepath.Dir(workflowPath), 0o755))
					use := rejected
					var actionPath string
					var action []byte
					if surface == "self composite" {
						use = "$/outer"
						actionPath = filepath.Join(dir, "outer", "action.yml")
						action = []byte("runs:\n  using: composite\n  steps:\n    - uses: " + rejected + "\n")
						require.NoError(t, os.MkdirAll(filepath.Dir(actionPath), 0o755))
						require.NoError(t, os.WriteFile(actionPath, action, 0o600))
					}
					workflow := []byte("on: push\njobs:\n  test:\n    runs-on: ubuntu-latest\n    steps:\n      - uses: " + use + "\n")
					require.NoError(t, os.WriteFile(workflowPath, workflow, 0o600))
					t.Chdir(dir)
					transport := &requestCountingTransport{}
					args := []string{"--no-interactive", "--no-narrow", workflowPath}
					if refresh {
						args = append(args, "--relock", "--accept-moved")
					}

					_, stderr, err := runCommandWithHTTP(t, transport, args...)

					assert.Error(t, err, stderr)
					assert.Contains(t, stderr, rejected)
					assert.Zero(t, transport.calls.Load())
					assert.NoFileExists(t, filepath.Join(filepath.Dir(workflowPath), "actions.lock"))
					got, readErr := os.ReadFile(workflowPath)
					require.NoError(t, readErr)
					assert.Equal(t, workflow, got)
					if actionPath != "" {
						got, readErr := os.ReadFile(actionPath)
						require.NoError(t, readErr)
						assert.Equal(t, action, got)
					}
				})
			}
		}
	}
}

// TestRemoteCompositeMalformedRefBlocks: a fetched composite whose uses:
// can't be parsed blocks the run instead of locking without that child.
func TestRemoteCompositeMalformedRefBlocks(t *testing.T) {
	tests := map[string]string{
		"actions/checkout@main:action.yml":             "uses: actions/checkout@main:action.yml is not an action reference the Actions backend accepts",
		"actions/checkout/.github/workflows/ci.yml@v4": "reusable workflows should be referenced at the top-level `jobs.<job_id>.uses` key, not within steps",
	}
	for use, want := range tests {
		t.Run(use, func(t *testing.T) { testRemoteCompositeMalformedRef(t, use, want) })
	}
}

func testRemoteCompositeMalformedRef(t *testing.T, use, want string) {
	const sha = "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
	composite := "name: c\nruns:\n  using: composite\n  steps:\n    - uses: " + use + "\n"
	reg := &httpmock.Registry{}
	reg.Register(httpmock.GraphQLForRepo("octo", "composite"), httpmock.JSONResponse(map[string]any{
		"data": map[string]any{"a0": httpmock.RepoResponse("octo/composite", sha, composite)},
	}))
	path := writeTempWorkflow(t, workflowUsing("octo/composite@v1"))

	_, stderr, err := runCommandWithHTTP(t, reg, "--no-narrow", "--no-interactive", path)
	require.ErrorIs(t, err, errSilent, stderr)
	assert.Contains(t, stderr, want)
	assert.NoFileExists(t, filepath.Join(filepath.Dir(path), "actions.lock"))
}

const cleanWorkflow = `name: clean
on: push
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/setup-go@v5.0.0
`

// writeCleanWorkflow adds a workflow the run could pin on its own, plus a
// local action the run would migrate to `$/…` when withLocal is set.
func writeCleanWorkflow(t *testing.T, reg *httpmock.Registry, withLocal bool) string {
	t.Helper()
	registerSetupGo(reg)
	require.NoError(t, os.MkdirAll(".git", 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(".github", "actions", "local"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(".github", "actions", "local", "action.yml"),
		[]byte("runs:\n  using: composite\n  steps:\n    - run: echo hi\n      shell: bash\n"), 0o600))
	path := filepath.Join(".github", "workflows", "clean.yml")
	body := cleanWorkflow
	if withLocal {
		body += "      - uses: ./.github/actions/local\n"
	}
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
	return filepath.ToSlash(path)
}

// registerSetupGo stubs resolution of actions/setup-go@v5.0.0.
func registerSetupGo(reg *httpmock.Registry) {
	reg.Register(httpmock.GraphQLForRepo("actions", "setup-go"), httpmock.JSONResponse(map[string]any{
		"data": map[string]any{"a0": httpmock.RepoResponse("actions/setup-go", parityTestMoved, httpmock.NodeActionYAML)},
	}))
	reg.Register(httpmock.REST("GET", `^/repos/actions/setup-go/tags`),
		httpmock.JSONResponse(httpmock.TagListResponse("v5.0.0", parityTestMoved)))
	reg.Register(httpmock.REST("GET", `repos/actions/setup-go$`), httpmock.JSONResponse(map[string]any{
		"default_branch": "main", "id": 2, "owner": map[string]any{"id": 1},
	}))
}

// snapshotWorkflows reads every file under .github so a test can assert the
// run left the tree byte-identical.
func snapshotWorkflows(t *testing.T) map[string]string {
	t.Helper()
	files := map[string]string{}
	require.NoError(t, filepath.WalkDir(".github", func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := os.ReadFile(p)
		files[p] = string(b)
		return err
	}))
	return files
}

// repoIDMismatch answers parity as intact except for actions/checkout, which
// now resolves to a different repository ID than the one locked.
type repoIDMismatch struct{}

func (repoIDMismatch) RoundTrip(req *http.Request) (*http.Response, error) {
	body, err := io.ReadAll(req.Body)
	if err != nil {
		return nil, err
	}
	req.Body = io.NopCloser(bytes.NewReader(body))
	resp, err := httpmock.ParityIntact(req)
	if err != nil {
		return nil, err
	}
	var payload struct {
		Data map[string]map[string]any `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, err
	}
	for _, a := range payload.Data {
		if a["nameWithOwner"] == "actions/checkout" {
			a["databaseId"] = 99
		}
	}
	return httpmock.JSONResponse(payload)(req)
}

// TestRun_CleanWorkflowPins is the control for the tests below: on its own,
// the clean workflow is pinned and migrated.
func TestRun_CleanWorkflowPins(t *testing.T) {
	reg := &httpmock.Registry{}
	writeTempWorkflow(t, "name: ci\non: push\n")
	clean := writeCleanWorkflow(t, reg, true)

	_, stderr, err := runCommandWithHTTP(t, reg, clean)
	require.NoError(t, err, stderr)
	assert.Contains(t, readTempLockfilePins(t), "actions/setup-go")
	b, err := os.ReadFile(clean)
	require.NoError(t, err)
	assert.Contains(t, string(b), "$/.github/actions/local")
	assert.Contains(t, stderr, "Migrated 1 local action")
}

func TestRun_BlockingErrorWritesNothing(t *testing.T) {
	for name, args := range map[string][]string{"terminal": nil, "json": {"--json"}} {
		t.Run(name, func(t *testing.T) {
			reg := &httpmock.Registry{}
			reg.Register(httpmock.ParityQuery, repoIDMismatch{}.RoundTrip)
			blocked := writeTempWorkflow(t, workflowUsing("actions/checkout@v4.2.1"), "actions/checkout@v4.2.1=sha1-"+parityTestSHA)
			clean := writeCleanWorkflow(t, reg, true)
			before := snapshotWorkflows(t)

			_, stderr, err := runCommandWithHTTP(t, reg, append(args, blocked, clean)...)
			require.ErrorIs(t, err, errSilent)
			assert.Equal(t, before, snapshotWorkflows(t), "no file may change")
			if name == "terminal" {
				assert.Contains(t, stderr, "different repository than the one locked")
				assert.Contains(t, stderr, "Nothing was written")
				assert.NotContains(t, stderr, "Migrated")
				assert.Contains(t, stderr, "1 of 2 workflows failed")
				assert.NotContains(t, stderr, "Not pinned", "pins the run would have made are not errors")
			}
		})
	}
}

func TestRun_UnloadableWorkflowWritesNothing(t *testing.T) {
	reg := &httpmock.Registry{}
	broken := writeTempWorkflow(t, "name: ci\non: [push\n")
	clean := writeCleanWorkflow(t, reg, false)
	before := snapshotWorkflows(t)

	_, stderr, err := runCommandWithHTTP(t, reg, "--no-migrate-local-actions", broken, clean)
	require.ErrorIs(t, err, errSilent)
	assert.Equal(t, before, snapshotWorkflows(t), "no file may change")
	assert.NoFileExists(t, filepath.Join(".github", "workflows", "actions.lock"))
	assert.Contains(t, stderr, "failed to load workflow")
	assert.Contains(t, stderr, "Nothing was written")
}

// TestRun_UnresolvedWritesNothing: an action that can't be resolved aborts
// the run, and the output must not claim the rolled-back migration landed.
func TestRun_UnresolvedWritesNothing(t *testing.T) {
	// acme/gone has no stub, so it can't be resolved.
	reg := &httpmock.Registry{}
	broken := writeTempWorkflow(t, workflowUsing("acme/gone@v1.0.0"))
	clean := writeCleanWorkflow(t, reg, true)
	before := snapshotWorkflows(t)

	_, stderr, err := runCommandWithHTTP(t, reg, broken, clean)
	require.ErrorIs(t, err, errSilent)
	assert.Equal(t, before, snapshotWorkflows(t), "no file may change")
	assert.Contains(t, stderr, "could not be resolved")
	assert.Contains(t, stderr, "Nothing was written")
	assert.NotContains(t, stderr, "Migrated")
}

// TestMissingRefNamesTheCause proves a ref that does not exist fails with an
// error finding that names it, rather than a fixable ref-changed plus a
// reachability warning.
func TestMissingRefNamesTheCause(t *testing.T) {
	refs := []struct {
		name, ref, detail, remedy string
	}{
		{"tag", "v999.0.0", "ref v999.0.0 does not exist in actions/checkout",
			"point `uses:` at a tag or branch that exists"},
		{"commit", "7b403c9ec14bd3ae0bbf793c2bee8815a7ac920b", "commit 7b403c9ec14b does not exist or is not reachable in actions/checkout",
			"pin a commit that exists in actions/checkout, or point `uses:` at a tag or branch"},
		{"short sha", "7b403c9", "ref 7b403c9 does not resolve in actions/checkout",
			"if this is a commit, use the full 40-character SHA; otherwise point `uses:` at a tag or branch that exists"},
	}
	for _, tt := range refs {
		for _, proxy := range []bool{false, true} {
			t.Run(tt.name+"/"+map[bool]string{false: "graphql", true: "dependabot proxy"}[proxy], func(t *testing.T) {
				testMissingRef(t, tt.ref, tt.detail, tt.remedy, proxy)
			})
		}
	}
}

func testMissingRef(t *testing.T, ref, detail, remedy string, proxy bool) {
	t.Helper()
	reg := &httpmock.Registry{}
	// Each resolve pass (plan, then diagnosis) asks again, for both
	// the JSON and the terminal run.
	for range 6 {
		registerMissingRef(reg, ref, proxy)
	}
	if proxy {
		t.Setenv("GH_ACTIONS_LOCK_DEPENDABOT_PROXY", "1")
		useDefaultTransport(t, reg)
	}
	path := writeTempWorkflow(t, workflowUsing("actions/checkout@v4.2.2"), "actions/checkout@v4.2.2")
	wf, err := os.ReadFile(path)
	require.NoError(t, err)
	wf = []byte(strings.Replace(string(wf), "@v4.2.2", "@"+ref, 1))
	require.NoError(t, os.WriteFile(path, wf, 0o600))
	lockBefore := readTempLockfilePins(t)

	stdout, _, err := runCommandWithHTTP(t, reg,
		"--no-onboard", "--no-narrow", "--no-interactive", "--json=findings", path)
	require.ErrorIs(t, err, errSilent)
	var out struct {
		Findings []struct {
			Category    string `json:"category"`
			Severity    string `json:"severity"`
			Detail      string `json:"detail"`
			Remediation string `json:"remediation"`
			DocURL      string `json:"doc_url"`
		} `json:"findings"`
	}
	require.NoError(t, json.Unmarshal([]byte(stdout), &out), stdout)
	found := false
	for _, f := range out.Findings {
		if f.Category == "not-pinned" && f.Severity == "error" {
			found = true
			assert.Equal(t, detail, f.Detail)
			assert.Equal(t, remedy, f.Remediation)
			assert.Empty(t, f.DocURL, "the security-hardening page doesn't explain a missing ref")
		}
	}
	assert.True(t, found, stdout)
	assert.Equal(t, lockBefore, readTempLockfilePins(t))
	after, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, string(wf), string(after))

	_, stderr, err := runCommandWithHTTP(t, reg,
		"--no-onboard", "--no-narrow", "--no-interactive", path)
	require.ErrorIs(t, err, errSilent)
	assert.Contains(t, stderr, "Ref does not exist actions/checkout@"+ref)
	assert.Contains(t, stderr, detail)
	assert.Contains(t, stderr, remedy)
	assert.NotContains(t, stderr, "Re-run without")
	assert.NotContains(t, stderr, "Not pinned")
	assert.NotContains(t, stderr, "security-hardening")
}

func registerMissingRef(reg *httpmock.Registry, ref string, proxy bool) {
	if !proxy {
		reg.Register(httpmock.GraphQL(`repository\(owner:`), httpmock.JSONResponse(map[string]any{
			"data": map[string]any{"a0": map[string]any{"nameWithOwner": "actions/checkout", "object": nil}},
		}))
		return
	}
	reg.Register(httpmock.REST("GET", `^/repos/actions/checkout$`), httpmock.JSONResponse(map[string]any{
		"full_name": "actions/checkout", "id": 1, "owner": map[string]any{"id": 1},
	}))
	reg.Register(httpmock.REST("GET", `^/repos/actions/checkout/commits/`+regexp.QuoteMeta(ref)+`$`),
		httpmock.StatusResponse(http.StatusUnprocessableEntity))
}

// writeParallelStepsRepo writes workflow.yml plus a lockfile that already pins
// checkout@v6 and setup-go@v6 (mutable refs trusted without a network call),
// and chdirs into the repo. Returns the lockfile path.
func writeParallelStepsRepo(t *testing.T, workflow string) string {
	t.Helper()
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, ".github", "workflows"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".github", "workflows", "workflow.yml"), []byte(workflow), 0o600))
	lockYAML := "version: '" + parserlock.Version + "'\ndependencies:\n" +
		"  'actions/checkout@v6':\n    ref: 'v6'\n    commit: 'sha1-" + testfixture.CheckoutV6.SHA + "'\n    owner_id: 1\n    repo_id: 1\n" +
		"  'actions/setup-go@v6':\n    ref: 'v6'\n    commit: 'sha1-" + testfixture.SetupGoV6.SHA + "'\n    owner_id: 1\n    repo_id: 1\n" +
		"workflows:\n  '.github/workflows/workflow.yml':\n    - 'actions/checkout@v6'\n    - 'actions/setup-go@v6'\n"
	lockPath := filepath.Join(dir, ".github", "workflows", "actions.lock")
	require.NoError(t, os.WriteFile(lockPath, []byte(lockYAML), 0o600))
	t.Chdir(dir)
	return lockPath
}

func TestCheck_ParallelStepGroups(t *testing.T) {
	t.Run("full scan keeps a dependency used only in a parallel group", func(t *testing.T) {
		reg := &httpmock.Registry{}
		defer reg.Verify(t)
		lockPath := writeParallelStepsRepo(t, `on: push
jobs:
  build:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v6
      - parallel:
          - uses: actions/setup-go@v6
          - run: echo hi
`)
		_, _, err := runCommandWithHTTP(t, reg, "--json=valid")
		require.NoError(t, err)
		got, err := os.ReadFile(lockPath)
		require.NoError(t, err)
		assert.Contains(t, string(got), "'actions/setup-go@v6'")
	})

	t.Run("full scan drops a dependency removed from a parallel group", func(t *testing.T) {
		reg := &httpmock.Registry{}
		defer reg.Verify(t)
		lockPath := writeParallelStepsRepo(t, `on: push
jobs:
  build:
    runs-on: ubuntu-latest
    steps:
      - parallel:
          - uses: actions/checkout@v6
          - run: echo hi
`)
		_, _, err := runCommandWithHTTP(t, reg, "--json=valid")
		require.NoError(t, err)
		got, err := os.ReadFile(lockPath)
		require.NoError(t, err)
		assert.Contains(t, string(got), "'actions/checkout@v6'")
		assert.NotContains(t, string(got), "setup-go")
	})

	t.Run("verify-local rejects a parallel action missing from the lockfile", func(t *testing.T) {
		reg := &httpmock.Registry{}
		defer reg.Verify(t)
		lockPath := writeParallelStepsRepo(t, `on: push
jobs:
  build:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v6
      - parallel:
          - uses: actions/setup-go@v6
          - uses: actions/setup-node@v6
`)
		before, err := os.ReadFile(lockPath)
		require.NoError(t, err)

		stdout, _, _ := runCommandWithHTTP(t, reg, "--verify-local", "--json=valid,findings")
		var payload struct {
			Valid    bool             `json:"valid"`
			Findings []format.Finding `json:"findings"`
		}
		require.NoError(t, json.Unmarshal([]byte(stdout), &payload), stdout)
		assert.False(t, payload.Valid)
		var flagged bool
		for _, f := range payload.Findings {
			if f.Severity == "error" && f.Dependency == "actions/setup-node@v6" {
				flagged = true
			}
		}
		assert.True(t, flagged, "findings: %+v", payload.Findings)

		after, err := os.ReadFile(lockPath)
		require.NoError(t, err)
		assert.Equal(t, string(before), string(after))
	})
}

// writeStaleLockfileRepo builds a scratch repo whose lockfile records two
// workflows — workflow.yml (present on disk, uses checkout@v6) and deleted.yml
// (no file on disk, uses setup-go@v6) — and chdirs into it. Both pins are
// mutable v6 refs so they're trusted from the lockfile without any network
// call. Returns the lockfile path.
func writeStaleLockfileRepo(t *testing.T) string {
	t.Helper()
	checkoutSHA := testfixture.CheckoutV6.SHA
	setupGoSHA := testfixture.SetupGoV6.SHA

	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, ".github", "workflows"), 0o755))
	wfBody := strings.TrimSpace(workflowUsing("actions/checkout@v6")) + "\n"
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".github", "workflows", "workflow.yml"), []byte(wfBody), 0o600))

	lockYAML := "version: '" + parserlock.Version + "'\ndependencies:\n" +
		"  'actions/checkout@v6':\n" +
		"    ref: 'v6'\n    commit: 'sha1-" + checkoutSHA + "'\n    owner_id: 1\n    repo_id: 1\n" +
		"  'actions/setup-go@v6':\n" +
		"    ref: 'v6'\n    commit: 'sha1-" + setupGoSHA + "'\n    owner_id: 1\n    repo_id: 1\n" +
		"workflows:\n" +
		"  '.github/workflows/workflow.yml':\n    - 'actions/checkout@v6'\n" +
		"  '.github/workflows/deleted.yml':\n    - 'actions/setup-go@v6'\n"
	lockPath := filepath.Join(dir, ".github", "workflows", "actions.lock")
	require.NoError(t, os.WriteFile(lockPath, []byte(lockYAML), 0o600))
	t.Chdir(dir)
	return lockPath
}

// TestCheck_FullScan_PrunesDeletedWorkflow proves a default full-directory fix
// run drops the lockfile entry for a workflow that no longer exists on disk,
// and that the now-orphaned dependency is garbage-collected with it.
func TestCheck_FullScan_PrunesDeletedWorkflow(t *testing.T) {
	reg := &httpmock.Registry{}
	defer reg.Verify(t)

	lockPath := writeStaleLockfileRepo(t)

	// No explicit workflow-path arg → full scan → prune authority.
	_, _, err := runCommandWithHTTP(t, reg, "--json=valid,workflows")
	require.NoError(t, err)

	lockAfter, err := os.ReadFile(lockPath)
	require.NoError(t, err)
	got := string(lockAfter)
	assert.Contains(t, got, "workflow.yml", "live workflow must be retained")
	assert.Contains(t, got, "actions/checkout", "live dep must be retained")
	assert.NotContains(t, got, "deleted.yml", "stale workflow entry must be pruned")
	assert.NotContains(t, got, "setup-go", "orphaned dep must be garbage-collected")
}

// TestCheck_PartialInvocation_DoesNotPrune proves an explicit-path invocation
// has no authority to declare another workflow deleted: the stale entry and its
// dep survive untouched.
func TestCheck_PartialInvocation_DoesNotPrune(t *testing.T) {
	reg := &httpmock.Registry{}
	defer reg.Verify(t)

	lockPath := writeStaleLockfileRepo(t)

	_, _, err := runCommandWithHTTP(t, reg, "--json=valid,workflows", ".github/workflows/workflow.yml")
	require.NoError(t, err)

	lockAfter, err := os.ReadFile(lockPath)
	require.NoError(t, err)
	got := string(lockAfter)
	assert.Contains(t, got, "deleted.yml", "partial run must not prune out-of-scope workflows")
	assert.Contains(t, got, "setup-go", "partial run must not GC an out-of-scope workflow's dep")
}

// TestCheck_NoFix_ReportsStaleWorkflow proves read-only mode surfaces a stale
// entry as a non-blocking info finding without touching the lockfile.
func TestCheck_NoFix_ReportsStaleWorkflow(t *testing.T) {
	reg := &httpmock.Registry{}
	defer reg.Verify(t)

	lockPath := writeStaleLockfileRepo(t)
	lockBefore, err := os.ReadFile(lockPath)
	require.NoError(t, err)

	stdout, _, err := runCommandWithHTTP(t, reg, "--no-fix", "--json=valid,findings")
	require.NoError(t, err, "stale-workflow is non-blocking, exit stays 0")

	var payload struct {
		Valid    bool             `json:"valid"`
		Findings []format.Finding `json:"findings"`
	}
	require.NoError(t, json.Unmarshal([]byte(stdout), &payload))
	assert.True(t, payload.Valid)

	var found *format.Finding
	for i := range payload.Findings {
		if payload.Findings[i].Category == "stale-workflow" {
			found = &payload.Findings[i]
			break
		}
	}
	require.NotNil(t, found, "expected a stale-workflow finding: %+v", payload.Findings)
	assert.Equal(t, "info", found.Severity)
	assert.Contains(t, found.Detail, "deleted.yml")

	lockAfter, err := os.ReadFile(lockPath)
	require.NoError(t, err)
	assert.Equal(t, string(lockBefore), string(lockAfter), "--no-fix must not modify the lockfile")
}

// writeLastWorkflowDeletedRepo builds a scratch repo whose lockfile records a
// single workflow that no longer exists on disk (the workflows directory holds
// only the lockfile). It chdirs into the repo and returns the lockfile path.
func writeLastWorkflowDeletedRepo(t *testing.T) string {
	t.Helper()
	setupGoSHA := testfixture.SetupGoV6.SHA

	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, ".github", "workflows"), 0o755))

	lockYAML := "version: '" + parserlock.Version + "'\ndependencies:\n" +
		"  'actions/setup-go@v6':\n" +
		"    ref: 'v6'\n    commit: 'sha1-" + setupGoSHA + "'\n    owner_id: 1\n    repo_id: 1\n" +
		"workflows:\n" +
		"  '.github/workflows/deleted.yml':\n    - 'actions/setup-go@v6'\n"
	lockPath := filepath.Join(dir, ".github", "workflows", "actions.lock")
	require.NoError(t, os.WriteFile(lockPath, []byte(lockYAML), 0o600))
	t.Chdir(dir)
	return lockPath
}

// TestCheck_FullScan_PrunesLastDeletedWorkflow proves that deleting the final
// workflow doesn't strand its lockfile entry: a full scan that discovers zero
// workflows still loads the lockfile and prunes the stale entry (and its
// orphaned dep) instead of aborting with "no workflow files found".
func TestCheck_FullScan_PrunesLastDeletedWorkflow(t *testing.T) {
	reg := &httpmock.Registry{}
	defer reg.Verify(t)

	lockPath := writeLastWorkflowDeletedRepo(t)

	_, _, err := runCommandWithHTTP(t, reg, "--json=valid,workflows")
	require.NoError(t, err, "an empty scan with a stale lockfile must prune, not error")

	// Pruning the last workflow empties the lockfile; the tool cleans up the
	// now-empty file. Either way the stale entry must not survive.
	lockAfter, err := os.ReadFile(lockPath)
	if err == nil {
		got := string(lockAfter)
		assert.NotContains(t, got, "deleted.yml", "stale workflow entry must be pruned")
		assert.NotContains(t, got, "setup-go", "orphaned dep must be garbage-collected")
	} else {
		assert.True(t, os.IsNotExist(err), "unexpected error reading lockfile: %v", err)
	}
}

// TestCheck_NoFix_ReportsLastDeletedWorkflow proves read-only mode still
// surfaces the stale entry when the last workflow is gone, without touching the
// lockfile.
func TestCheck_NoFix_ReportsLastDeletedWorkflow(t *testing.T) {
	reg := &httpmock.Registry{}
	defer reg.Verify(t)

	lockPath := writeLastWorkflowDeletedRepo(t)
	lockBefore, err := os.ReadFile(lockPath)
	require.NoError(t, err)

	stdout, _, err := runCommandWithHTTP(t, reg, "--no-fix", "--json=valid,findings")
	require.NoError(t, err, "stale-workflow is non-blocking, exit stays 0")

	var payload struct {
		Findings []format.Finding `json:"findings"`
	}
	require.NoError(t, json.Unmarshal([]byte(stdout), &payload))
	var found bool
	for i := range payload.Findings {
		if payload.Findings[i].Category == "stale-workflow" {
			found = true
			assert.Contains(t, payload.Findings[i].Detail, "deleted.yml")
		}
	}
	assert.True(t, found, "expected a stale-workflow finding: %+v", payload.Findings)

	lockAfter, err := os.ReadFile(lockPath)
	require.NoError(t, err)
	assert.Equal(t, string(lockBefore), string(lockAfter), "--no-fix must not modify the lockfile")
}

// TestCheck_EmptyRepo_NoLockfile_Errors proves the helpful "no workflow files"
// error is preserved when there's nothing on disk and nothing to prune.
func TestCheck_EmptyRepo_NoLockfile_Errors(t *testing.T) {
	reg := &httpmock.Registry{}
	defer reg.Verify(t)

	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, ".github", "workflows"), 0o755))
	t.Chdir(dir)

	_, _, err := runCommandWithHTTP(t, reg, "--json=valid,workflows")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no workflow files found")
}

func TestTenantHostPrunesBeforeVerifyingHosts(t *testing.T) {
	for _, tt := range []struct {
		name       string
		args       []string
		last       bool
		renamed    bool
		transitive bool
		foreign    bool
		namesake   bool
		public     bool
		mismatch   bool
		outOfScope bool
		wantError  bool
	}{
		{name: "inaccessible stale pin is removed"},
		{name: "last deleted workflow is removed", last: true},
		{name: "renamed workflow preserves its historical pin", renamed: true},
		{name: "stale foreign host does not bind the resolver", foreign: true},
		{name: "stale namesake cannot seed identity caches", namesake: true},
		{name: "stale namesake cannot bind a conflicting host", namesake: true, public: true},
		{name: "partial scan ignores out of scope pins", args: []string{".github/workflows/workflow.yml"}, outOfScope: true},
		{name: "readonly ignores stale pins", args: []string{"--no-fix"}, outOfScope: true},
		{name: "retained transitive pin is still verified", transitive: true, wantError: true},
		{name: "retained identity mismatch leaves disk untouched", mismatch: true, wantError: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var path string
			if tt.last {
				path = writeLastWorkflowDeletedRepo(t)
			} else {
				path = writeStaleLockfileRepo(t)
			}
			raw, err := os.ReadFile(path)
			require.NoError(t, err)
			before := string(raw)
			if tt.renamed {
				before = strings.Replace(before, ".github/workflows/workflow.yml", ".github/workflows/old.yml", 1)
			}
			if tt.transitive {
				before = strings.Replace(before, "  'actions/checkout@v6':\n",
					"  'actions/checkout@v6':\n    uses: ['actions/setup-go@v6']\n", 1)
			}
			if tt.foreign {
				before = strings.Replace(before, "  'actions/setup-go@v6':\n",
					"  'actions/setup-go@v6':\n    hostname: 'other.ghe.com'\n", 1)
			}
			if tt.namesake {
				before = strings.ReplaceAll(before, "actions/setup-go@v6", "actions/checkout@v5")
				before = strings.Replace(before, "  'actions/checkout@v5':\n    ref: 'v6'",
					"  'actions/checkout@v5':\n    ref: 'v5'", 1)
				if tt.public {
					before = strings.Replace(before, "  'actions/checkout@v5':\n",
						"  'actions/checkout@v5':\n    hostname: 'github.com'\n", 1)
				}
				before = strings.Replace(before, "sha1-"+testfixture.SetupGoV6.SHA+"'\n    owner_id: 1\n    repo_id: 1",
					"sha1-"+testfixture.SetupGoV6.SHA+"'\n    owner_id: 2\n    repo_id: 2", 1)
			}
			require.NoError(t, os.WriteFile(path, []byte(before), 0o600))
			staleCalls := 0
			transport := httpmock.RoundTripFunc(func(req *http.Request) (*http.Response, error) {
				assert.Equal(t, "api.tenant.ghe.com", req.URL.Host)
				switch req.URL.Path {
				case "/repos/actions/checkout":
					id := 1
					if tt.mismatch {
						id = 2
					}
					return httpmock.JSONResponse(map[string]any{"id": id, "owner": map[string]any{"id": 1}})(req)
				case "/repos/actions/setup-go":
					staleCalls++
					return httpmock.StatusResponse(http.StatusNotFound)(req)
				case "/graphql":
					if tt.mismatch {
						return parityRepoID(req, http.StatusOK, 2)
					}
					return httpmock.ParityIntact(req)
				case "/repos/actions/checkout/tags", "/repos/actions/checkout/releases":
					return httpmock.JSONResponse([]any{})(req)
				default:
					t.Errorf("unexpected request: %s", req.URL)
					return httpmock.StatusResponse(http.StatusInternalServerError)(req)
				}
			})
			args := append([]string{"--hostname", "tenant.ghe.com", "--no-interactive", "--no-narrow",
				"--no-migrate-local-actions", "--json=valid,findings"}, tt.args...)
			stdout, _, err := runCommandWithHTTP(t, transport, args...)
			if tt.outOfScope {
				require.NoError(t, err)
				assert.Zero(t, staleCalls)
				after, readErr := os.ReadFile(path)
				require.NoError(t, readErr)
				assert.Contains(t, string(after), "actions/setup-go")
				return
			}
			if tt.wantError {
				require.ErrorIs(t, err, errSilent)
				after, readErr := os.ReadFile(path)
				require.NoError(t, readErr)
				assert.Equal(t, before, string(after))
				if tt.mismatch {
					assert.Contains(t, stdout, "repo-replaced")
					assert.Zero(t, staleCalls)
				} else {
					assert.Contains(t, stdout, "repo-unavailable")
					assert.Positive(t, staleCalls)
				}
				return
			}
			require.NoError(t, err)
			assert.Zero(t, staleCalls)
			assert.Contains(t, stdout, "stale-workflow")
			after, err := os.ReadFile(path)
			if tt.last {
				assert.ErrorIs(t, err, os.ErrNotExist)
				return
			}
			require.NoError(t, err)
			assert.Contains(t, string(after), "actions/checkout@v6")
			assert.Contains(t, string(after), "repo_id: 1")
			assert.Contains(t, string(after), "sha1-"+testfixture.CheckoutV6.SHA)
			assert.NotContains(t, string(after), "deleted.yml")
			assert.NotContains(t, string(after), "old.yml")
			assert.NotContains(t, string(after), "actions/setup-go")
			assert.NotContains(t, string(after), "actions/checkout@v5")
			assert.NotContains(t, string(after), "repo_id: 2")
		})
	}
}

// TestMigrateLocalActions_CompositeActionFiles verifies that migration
// rewrites `./…` not just in workflow files but also inside in-repo composite
// action definitions (action.yml), including ones nested a few directories deep.
// A `./…` whose target does not resolve to an in-repo action file is left
// untouched — that is the equivalence guard that keeps `$/` a safe replacement.
func TestMigrateLocalActions_CompositeActionFiles(t *testing.T) {
	dir := t.TempDir()
	write := func(rel, content string) {
		full := filepath.Join(dir, rel)
		require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o755))
		require.NoError(t, os.WriteFile(full, []byte(content), 0o644))
	}

	require.NoError(t, os.Mkdir(filepath.Join(dir, ".git"), 0o755))
	write(".github/workflows/ci.yml",
		"name: CI\non: push\njobs:\n  b:\n    runs-on: ubuntu-latest\n    steps:\n      - uses: ./my-action\n")
	write("my-action/action.yml",
		"name: My Action\nruns:\n  using: composite\n  steps:\n    - uses: ./helper\n    - uses: ./nonexistent\n")
	write("helper/action.yml",
		"name: Helper\nruns:\n  using: composite\n  steps:\n    - uses: ./nested/deep\n")
	write("nested/deep/action.yml",
		"name: Deep\nruns:\n  using: composite\n  steps:\n    - run: echo deep\n      shell: bash\n")

	t.Chdir(dir)

	total, _, err := migrateLocalActions([]string{filepath.Join(".github", "workflows", "ci.yml")})
	require.NoError(t, err)

	// workflow ./my-action, composite ./helper, composite ./nested/deep = 3.
	assert.Equal(t, 3, total)

	read := func(rel string) string {
		b, err := os.ReadFile(filepath.Join(dir, rel))
		require.NoError(t, err)
		return string(b)
	}
	assert.Contains(t, read(".github/workflows/ci.yml"), "uses: $/my-action")
	assert.Contains(t, read("my-action/action.yml"), "uses: $/helper")
	assert.Contains(t, read("helper/action.yml"), "uses: $/nested/deep")
	// Unresolvable local path stays put — nothing at repo-root/nonexistent.
	assert.Contains(t, read("my-action/action.yml"), "uses: ./nonexistent")
}

func TestMigrateLocalActions_StructuralErrorPreventsAllWrites(t *testing.T) {
	dir := t.TempDir()
	write := func(rel, content string) string {
		full := filepath.Join(dir, rel)
		require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o755))
		require.NoError(t, os.WriteFile(full, []byte(content), 0o644))
		return full
	}
	require.NoError(t, os.Mkdir(filepath.Join(dir, ".git"), 0o755))
	write("local/action.yml", "runs:\n  using: composite\n")
	firstPath := write(".github/workflows/first.yml",
		"jobs:\n  build:\n    steps:\n      - uses: ./local\n")
	secondPath := write(".github/workflows/second.yml",
		"jobs:\n  build:\n    steps:\n      - uses: $/bad@v1\n")
	firstBefore, err := os.ReadFile(firstPath)
	require.NoError(t, err)

	total, _, err := migrateLocalActions([]string{firstPath, secondPath})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid self repository reference")
	assert.Zero(t, total)
	firstAfter, readErr := os.ReadFile(firstPath)
	require.NoError(t, readErr)
	assert.Equal(t, firstBefore, firstAfter)
}

func TestMigrateLocalActions_RejectsActionFileSymlinkEscape(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(dir, ".git"), 0o755))
	workflowPath := filepath.Join(dir, ".github", "workflows", "ci.yml")
	require.NoError(t, os.MkdirAll(filepath.Dir(workflowPath), 0o755))
	workflow := []byte("jobs:\n  build:\n    steps:\n      - uses: ./escape\n")
	require.NoError(t, os.WriteFile(workflowPath, workflow, 0o644))

	outsidePath := filepath.Join(t.TempDir(), "action.yml")
	outside := []byte("runs:\n  using: composite\n  steps:\n    - uses: ./helper\n")
	require.NoError(t, os.WriteFile(outsidePath, outside, 0o644))
	escapeDir := filepath.Join(dir, "escape")
	require.NoError(t, os.MkdirAll(escapeDir, 0o755))
	if err := os.Symlink(outsidePath, filepath.Join(escapeDir, "action.yml")); err != nil {
		t.Skipf("creating symlink: %v", err)
	}

	total, _, err := migrateLocalActions([]string{workflowPath})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "outside repository root")
	assert.Zero(t, total)
	gotWorkflow, readErr := os.ReadFile(workflowPath)
	require.NoError(t, readErr)
	assert.Equal(t, workflow, gotWorkflow)
	gotOutside, readErr := os.ReadFile(outsidePath)
	require.NoError(t, readErr)
	assert.Equal(t, outside, gotOutside)
}

// TestMigrateLocalActions_EndToEnd runs the real command (migration is on by
// default) against a scratch repo whose local composite chain
// reaches a pinnable remote action, then asserts on BOTH sides of
// the mutation: the rewritten workflow/action files AND the generated
// lockfile. It proves the two behaviours compose correctly in one pass:
//   - `./…` same-repo refs are rewritten to `$/…` in the workflow and in the
//     in-repo composite action.yml files (including nested ones);
//   - `$/…` self refs are inherently pinned, so they never appear in the
//     lockfile, while the genuine remote dep (actions/setup-go) is pinned to
//     its resolved commit.
func TestMigrateLocalActions_EndToEnd(t *testing.T) {
	reg := &httpmock.Registry{}
	defer reg.Verify(t)

	setupGoSHA := testfixture.SetupGoV6.SHA

	reg.StubRepo("actions/setup-go", setupGoSHA)
	reg.Register(
		httpmock.REST("GET", `repos/actions/setup-go/tags`),
		httpmock.JSONResponse([]map[string]any{
			{"name": "v6.0.0", "commit": map[string]any{"sha": setupGoSHA}},
		}),
	)
	reg.Register(httpmock.REST("GET", `repos/actions/setup-go$`), httpmock.JSONResponse(map[string]any{
		"default_branch": "main",
		"id":             2,
		"owner":          map[string]any{"id": 1},
	}))

	dir := t.TempDir()
	write := func(rel, content string) {
		full := filepath.Join(dir, rel)
		require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o755))
		require.NoError(t, os.WriteFile(full, []byte(content), 0o600))
	}
	require.NoError(t, os.Mkdir(filepath.Join(dir, ".git"), 0o755))

	// The workflow only names the local composite. The remote dependency lives
	// three levels down the in-repo action chain.
	write(".github/workflows/workflow.yml", strings.TrimSpace(workflowUsing("./my-action"))+"\n")
	// In-repo composite chain, all same-repo local refs.
	write("my-action/action.yml", "name: My Action\nruns:\n  using: composite\n  steps:\n    - uses: ./helper\n")
	write("helper/action.yml", "name: Helper\nruns:\n  using: composite\n  steps:\n    - uses: ./nested/deep\n")
	write("nested/deep/action.yml", "name: Deep\nruns:\n  using: composite\n  steps:\n    - uses: actions/setup-go@v6\n")

	t.Chdir(dir)

	_, _, err := runCommandWithHTTP(t, reg)
	require.NoError(t, err)

	read := func(rel string) string {
		b, err := os.ReadFile(filepath.Join(dir, rel))
		require.NoError(t, err)
		return string(b)
	}

	// Workflow: local ref migrated; the remote ref remains in the action file.
	wf := read(".github/workflows/workflow.yml")
	assert.Contains(t, wf, "uses: $/my-action")
	assert.NotContains(t, wf, "uses: ./my-action")
	assert.NotContains(t, wf, "actions/setup-go")

	// In-repo composite action files migrated, including the nested one.
	assert.Contains(t, read("my-action/action.yml"), "uses: $/helper")
	assert.Contains(t, read("helper/action.yml"), "uses: $/nested/deep")
	assert.Contains(t, read("nested/deep/action.yml"), "uses: actions/setup-go@v6.0.0",
		"remote refs inside $/ actions are narrowed in their action file")

	// Lockfile: pins the remote dep, records nothing for the self refs.
	lock := readTempLockfilePins(t)
	assert.Contains(t, lock, "actions/setup-go", "remote dep should be pinned")
	assert.Contains(t, lock, "'actions/setup-go@v6.0.0':", "narrowed ref matches the rewritten action file")
	assert.Contains(t, lock, setupGoSHA)
	assert.NotContains(t, lock, "$/", "self refs are inherently pinned; no lockfile entry")
	assert.NotContains(t, lock, "my-action", "in-repo composite must not be pinned")
	assert.NotContains(t, lock, "helper")
}

// TestMigrateLocalActions_DefaultOnAndOptOut proves the default-on wiring:
// a plain fix run rewrites an in-repo `./.github/actions/foo` composite ref to
// `$/…` with no network, --no-fix leaves the file untouched, and the
// --no-migrate-local-actions opt-out suppresses the rewrite on a fix run.
func TestMigrateLocalActions_DefaultOnAndOptOut(t *testing.T) {
	const workflow = `name: CI
on: push
jobs:
  build:
    runs-on: ubuntu-latest
    steps:
      - uses: ./.github/actions/foo
`
	setup := func(t *testing.T) string {
		dir := t.TempDir()
		require.NoError(t, os.Mkdir(filepath.Join(dir, ".git"), 0o755))
		wf := filepath.Join(dir, ".github", "workflows", "ci.yml")
		require.NoError(t, os.MkdirAll(filepath.Dir(wf), 0o755))
		require.NoError(t, os.WriteFile(wf, []byte(workflow), 0o600))
		act := filepath.Join(dir, ".github", "actions", "foo", "action.yml")
		require.NoError(t, os.MkdirAll(filepath.Dir(act), 0o755))
		require.NoError(t, os.WriteFile(act,
			[]byte("name: Foo\nruns:\n  using: composite\n  steps:\n    - run: echo hi\n      shell: bash\n"), 0o600))
		t.Chdir(dir)
		return wf
	}
	read := func(t *testing.T, path string) string {
		b, err := os.ReadFile(path)
		require.NoError(t, err)
		return string(b)
	}

	t.Run("default fix run rewrites to $/", func(t *testing.T) {
		transport := &requestCountingTransport{}
		wf := setup(t)
		_, _, err := runCommandWithHTTP(t, transport, wf)
		require.NoError(t, err)
		got := read(t, wf)
		assert.Contains(t, got, "uses: $/.github/actions/foo")
		assert.NotContains(t, got, "uses: ./.github/actions/foo")
		assert.Zero(t, transport.calls.Load(), "self ref is inherently pinned; no network")
	})

	t.Run("--no-fix leaves file untouched", func(t *testing.T) {
		transport := &requestCountingTransport{}
		wf := setup(t)
		_, _, err := runCommandWithHTTP(t, transport, "--no-fix", wf)
		require.NoError(t, err, "command must succeed so the assertion proves the guard, not an early failure")
		assert.Equal(t, workflow, read(t, wf))
	})

	t.Run("--no-migrate-local-actions suppresses rewrite", func(t *testing.T) {
		transport := &requestCountingTransport{}
		wf := setup(t)
		_, _, err := runCommandWithHTTP(t, transport, "--no-migrate-local-actions", wf)
		require.NoError(t, err, "command must succeed so the assertion proves the opt-out, not an early failure")
		got := read(t, wf)
		assert.Contains(t, got, "uses: ./.github/actions/foo", "opt-out leaves the ./… ref in place")
		assert.NotContains(t, got, "$/", "opt-out means no migration to $/")
	})
}

func TestNoMigrateLocalActions_PreservesExistingPins(t *testing.T) {
	const workflowWithRemoteAction = `name: CI
on: push
jobs:
  build:
    runs-on: ubuntu-latest
    steps:
      - uses: ./.github/actions/foo
      - uses: actions/checkout@v4
`
	const workflowWithOnlyLocalAction = `name: CI
on: push
jobs:
  build:
    runs-on: ubuntu-latest
    steps:
      - uses: ./.github/actions/foo
`
	const lock = `version: 'v0.0.2'
dependencies:
  'actions/checkout@v4':
    ref: 'v4'
    commit: 'sha1-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa'
    owner_id: 1
    repo_id: 1
workflows:
  '.github/workflows/ci.yml':
    - 'actions/checkout@v4'
`

	for name, workflow := range map[string]string{
		"remote and local actions": workflowWithRemoteAction,
		"only local action":        workflowWithOnlyLocalAction,
		"malformed workflow":       "name: [",
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			t.Chdir(dir)
			workflowPath := filepath.Join(".github", "workflows", "ci.yml")
			require.NoError(t, os.MkdirAll(filepath.Dir(workflowPath), 0o755))
			require.NoError(t, os.WriteFile(workflowPath, []byte(workflow), 0o600))
			lockPath := filepath.Join(".github", "workflows", "actions.lock")
			require.NoError(t, os.WriteFile(lockPath, []byte(lock), 0o600))

			transport := &requestCountingTransport{}
			_, _, err := runCommandWithHTTP(t, transport, "--relock", "--no-migrate-local-actions")
			require.Error(t, err)
			got, readErr := os.ReadFile(lockPath)
			require.NoError(t, readErr)
			assert.Equal(t, 2, strings.Count(string(got), "'actions/checkout@v4'"))
			assert.NotContains(t, string(got), "'.github/workflows/ci.yml': []")
		})
	}
}

type requestCountingTransport struct {
	calls atomic.Int64
	// parity answers parity queries without counting them.
	parity bool
	// tags, when set, answers tag and release listings (tag name to commit) without
	// counting them: a SHA repair must prove its tag peels to the commit.
	tags map[string]string
}

func (t *requestCountingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if t.parity && httpmock.ParityQuery(req) {
		return httpmock.ParityIntact(req)
	}
	if t.tags != nil && strings.HasSuffix(req.URL.Path, "/releases") {
		return httpmock.JSONResponse([]any{})(req)
	}
	if t.tags != nil && strings.HasSuffix(req.URL.Path, "/tags") {
		var list []any
		for name, sha := range t.tags {
			list = append(list, map[string]any{"name": name, "commit": map[string]any{"sha": sha}})
		}
		return httpmock.JSONResponse(list)(req)
	}
	t.calls.Add(1)
	return nil, errors.New("unexpected HTTP request")
}

func TestInvalidSelfRepositoryRefDoesNotPinOtherRefs(t *testing.T) {
	transport := &requestCountingTransport{}

	dir := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(dir, ".git"), 0o755))
	workflowPath := filepath.Join(dir, ".github", "workflows", "ci.yml")
	require.NoError(t, os.MkdirAll(filepath.Dir(workflowPath), 0o755))
	workflow := []byte(`name: CI
on: push
jobs:
  build:
    runs-on: ubuntu-latest
    steps:
      - uses: $/actions/bad@v1
      - uses: actions/checkout@v4
`)
	require.NoError(t, os.WriteFile(workflowPath, workflow, 0o600))
	t.Chdir(dir)

	_, _, err := runCommandWithHTTP(t, transport, workflowPath)
	// Migration runs by default and validates self repository refs first, so an
	// invalid `$/…` ref aborts the run before any dependency resolution.
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid self repository reference")
	assert.Zero(t, transport.calls.Load(), "invalid syntax should be rejected before dependency resolution")

	gotWorkflow, err := os.ReadFile(workflowPath)
	require.NoError(t, err)
	assert.Equal(t, workflow, gotWorkflow)

	lockPath := filepath.Join(dir, ".github", "workflows", "actions.lock")
	if lockContent, readErr := os.ReadFile(lockPath); readErr == nil {
		assert.NotContains(t, string(lockContent), "actions/checkout")
	} else {
		assert.ErrorIs(t, readErr, os.ErrNotExist)
	}
}

func TestInvalidSelfRepositoryRefPreventsMigrationAndNetwork(t *testing.T) {
	transport := &requestCountingTransport{}

	dir := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(dir, ".git"), 0o755))
	localActionPath := filepath.Join(dir, "actions", "local", "action.yml")
	require.NoError(t, os.MkdirAll(filepath.Dir(localActionPath), 0o755))
	require.NoError(t, os.WriteFile(localActionPath, []byte("runs:\n  using: composite\n"), 0o600))

	workflowPath := filepath.Join(dir, ".github", "workflows", "ci.yml")
	require.NoError(t, os.MkdirAll(filepath.Dir(workflowPath), 0o755))
	workflow := []byte(`name: CI
on: push
jobs:
  build:
    runs-on: ubuntu-latest
    steps:
      - uses: ./actions/local
      - uses: "$/actions/bad@${{ matrix.ref }}"
      - uses: actions/checkout@v4
`)
	require.NoError(t, os.WriteFile(workflowPath, workflow, 0o600))
	t.Chdir(dir)

	_, _, err := runCommandWithHTTP(t, transport, workflowPath)

	require.Error(t, err)
	assert.Zero(t, transport.calls.Load(), "invalid syntax should stop migration before dependency resolution")
	gotWorkflow, readErr := os.ReadFile(workflowPath)
	require.NoError(t, readErr)
	assert.Equal(t, workflow, gotWorkflow)
	lockPath := filepath.Join(dir, ".github", "workflows", "actions.lock")
	_, readErr = os.Stat(lockPath)
	assert.ErrorIs(t, readErr, os.ErrNotExist)
}

func TestExistingSHARefRewritesSelfRepositoryAction(t *testing.T) {
	const sha = "bcd2ba49218906704ab6c1aa796996da409d3eb1"
	transport := &requestCountingTransport{parity: true, tags: map[string]string{"v3.2.0": sha}}

	dir := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(dir, ".git"), 0o755))
	actionPath := filepath.Join(dir, ".github", "actions", "local", "action.yml")
	require.NoError(t, os.MkdirAll(filepath.Dir(actionPath), 0o755))
	require.NoError(t, os.WriteFile(actionPath, []byte(`name: Local
runs:
  using: composite
  steps:
    - uses: actions/create-github-app-token@`+sha+`
`), 0o600))

	workflowPath := filepath.Join(dir, ".github", "workflows", "ci.yml")
	require.NoError(t, os.MkdirAll(filepath.Dir(workflowPath), 0o755))
	workflow := []byte(`name: CI
on: push
jobs:
  build:
    runs-on: ubuntu-latest
    steps:
      - uses: $/.github/actions/local
`)
	require.NoError(t, os.WriteFile(workflowPath, workflow, 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(filepath.Dir(workflowPath), "other.yml"), []byte(`name: Other
on: push
jobs:
  build:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/create-github-app-token@v3.2.0
`), 0o600))
	lockPath := filepath.Join(dir, ".github", "workflows", "actions.lock")
	originalLock := []byte(`version: 'v0.0.2'
workflows:
    '.github/workflows/ci.yml':
        - 'actions/create-github-app-token@` + sha + `'
    '.github/workflows/other.yml':
        - 'actions/create-github-app-token@v3.2.0'
dependencies:
    'actions/create-github-app-token@` + sha + `':
        ref: 'v3.2.0'
        commit: 'sha1-` + sha + `'
        owner_id: 44036562
        repo_id: 642580244
        uses:
            - 'actions/checkout@v4'
    'actions/create-github-app-token@v3.2.0':
        ref: 'v3.2.0'
        commit: 'sha1-` + sha + `'
        owner_id: 44036562
        repo_id: 642580244
        uses:
            - 'actions/setup-go@v5'
    'actions/checkout@v4':
        ref: 'v4'
        commit: 'sha1-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa'
        owner_id: 44036562
        repo_id: 197814629
    'actions/setup-go@v5':
        ref: 'v5'
        commit: 'sha1-bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb'
        owner_id: 44036562
        repo_id: 485264523
`)
	require.NoError(t, os.WriteFile(lockPath, originalLock, 0o600))
	t.Chdir(dir)

	_, _, err := runCommandWithHTTP(t, transport, "--no-narrow", filepath.Join(".github", "workflows", "ci.yml"))
	require.NoError(t, err)
	lockAfterNoNarrow, err := os.ReadFile(lockPath)
	require.NoError(t, err)

	_, _, err = runCommandWithHTTP(t, transport, filepath.Join(".github", "workflows", "ci.yml"))
	require.ErrorContains(t, err, "partial workflow scan")
	action, readErr := os.ReadFile(actionPath)
	require.NoError(t, readErr)
	assert.Contains(t, string(action), "uses: actions/create-github-app-token@"+sha)
	lock, readErr := os.ReadFile(lockPath)
	require.NoError(t, readErr)
	assert.Equal(t, lockAfterNoNarrow, lock)

	_, _, err = runCommandWithHTTP(t, transport)
	require.NoError(t, err)
	assert.Zero(t, transport.calls.Load())

	action, err = os.ReadFile(actionPath)
	require.NoError(t, err)
	assert.Contains(t, string(action), "uses: actions/create-github-app-token@v3.2.0")

	lock, err = os.ReadFile(lockPath)
	require.NoError(t, err)
	assert.Contains(t, string(lock), "'actions/create-github-app-token@v3.2.0'")
	assert.Contains(t, string(lock), "'actions/checkout@v4'")
	assert.Contains(t, string(lock), "'actions/setup-go@v5'")
	assert.Contains(t, string(lock), "uses:\n            - 'actions/checkout@v4'")
	assert.Contains(t, string(lock), "- 'actions/setup-go@v5'")
	assert.NotContains(t, string(lock), "'actions/create-github-app-token@"+sha+"'")
}

const (
	parityTestSHA   = "9ddf35b71a482be7d8922b28e8d00df16b77e315"
	parityTestMoved = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

func registerNodeManifests(reg *httpmock.Registry) {
	reg.Register(httpmock.GraphQL(`file: file\(path`), func(req *http.Request) (*http.Response, error) {
		var body struct {
			Variables map[string]string `json:"variables"`
		}
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			return nil, err
		}
		data := map[string]any{}
		for i := 0; ; i++ {
			owner, ok := body.Variables[fmt.Sprintf("owner%d", i)]
			if !ok {
				break
			}
			data[fmt.Sprintf("a%d", i)] = httpmock.RepoResponse(
				owner+"/"+body.Variables[fmt.Sprintf("name%d", i)],
				strings.TrimSuffix(body.Variables[fmt.Sprintf("expr%d", i)], "^{commit}"),
				httpmock.NodeActionYAML,
			)
		}
		return httpmock.JSONResponse(map[string]any{"data": data})(req)
	})
}

// registerLockedCheckoutManifest serves actions/checkout's manifest at
// parityTestSHA for --verify's locked-commit graph check.
func registerLockedCheckoutManifest(reg *httpmock.Registry, proxy bool) {
	if !proxy {
		registerNodeManifests(reg)
		return
	}
	reg.Register(httpmock.REST("GET", `^/repos/actions/checkout/commits/`+parityTestSHA+`$`),
		httpmock.JSONResponse(map[string]any{"sha": parityTestSHA}))
	reg.Register(httpmock.REST("GET", `^/repos/actions/checkout/contents/action\.yml$`),
		httpmock.JSONResponse(map[string]any{"encoding": "base64", "content": base64.StdEncoding.EncodeToString([]byte(httpmock.NodeActionYAML))}))
}

var transferWorkflow = workflowUsing("krzema12/github-actions-typing@v2.2.2")

func transferParity(repoID int) httpmock.Responder {
	return httpmock.JSONResponse(map[string]any{
		"data": map[string]any{"a0": map[string]any{
			"nameWithOwner": "typesafegithub/github-actions-typing",
			"databaseId":    repoID,
			"owner":         map[string]any{"databaseId": 42},
			"commit":        map[string]any{"oid": parityTestSHA},
			"tag":           map[string]any{"oid": parityTestSHA},
			"tagRef":        map[string]any{"oid": parityTestSHA},
		}},
	})
}

// TestParity_TransferredRepoWarns is the #110 case under --verify: the
// locked repository was transferred and kept its repo ID, so the runner
// follows the redirect. The CLI warns with the replacement `uses:` line.
func TestParity_TransferredRepoWarns(t *testing.T) {
	reg := &httpmock.Registry{}
	defer reg.Verify(t)
	reg.Register(httpmock.ParityQuery, transferParity(1))
	registerNodeManifests(reg)
	path := writeTempWorkflow(t, transferWorkflow, "krzema12/github-actions-typing@v2.2.2=sha1-"+parityTestSHA)

	_, stderr, err := runCommandWithHTTP(t, reg, "--verify", path)
	require.NoError(t, err, stderr)
	assert.Contains(t, stderr, "rewrite it as `uses: typesafegithub/github-actions-typing@v2.2.2`")
	assert.Contains(t, stderr, path)
	assert.Contains(t, stderr, "All 1 workflow valid")
}

// TestParity_TransferredRepoRewrites: in fix mode a recorded same-ID
// redirect moves `uses:` and the lockfile key to the canonical name.
func TestParity_TransferredRepoRewrites(t *testing.T) {
	reg := &httpmock.Registry{}
	defer reg.Verify(t)
	reg.Register(httpmock.ParityQuery, transferParity(1))
	reg.Register(httpmock.REST("GET", `^/repos/typesafegithub/github-actions-typing$`), httpmock.JSONResponse(map[string]any{
		"full_name": "typesafegithub/github-actions-typing", "id": 1, "owner": map[string]any{"id": 42},
	}))
	path := writeTempWorkflow(t, transferWorkflow, "krzema12/github-actions-typing@v2.2.2=sha1-"+parityTestSHA)

	_, stderr, err := runCommandWithHTTP(t, reg, path)
	require.NoError(t, err, stderr)
	assertTransferRewritten(t, path, stderr)
	assert.NotContains(t, stderr, "rewrite it as", "the run already rewrote it")
}

// TestParity_MixedCaseRenameRewrites: the lockfile key is lowercase but
// `uses:` isn't; the rewrite must match the workflow's own text.
func TestParity_MixedCaseRenameRewrites(t *testing.T) {
	reg := &httpmock.Registry{}
	reg.Register(httpmock.ParityQuery, transferParity(1))
	reg.Register(httpmock.REST("GET", `^/repos/typesafegithub/github-actions-typing$`), httpmock.JSONResponse(map[string]any{
		"full_name": "typesafegithub/github-actions-typing", "id": 1, "owner": map[string]any{"id": 42},
	}))
	path := writeTempWorkflow(t, strings.Replace(transferWorkflow, "krzema12/github-actions-typing", "Krzema12/GitHub-Actions-Typing", 1),
		"krzema12/github-actions-typing@v2.2.2=sha1-"+parityTestSHA)

	_, stderr, err := runCommandWithHTTP(t, reg, path)
	require.NoError(t, err, stderr)
	wf, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Contains(t, string(wf), "uses: typesafegithub/github-actions-typing@v2.2.2")
	assert.NotContains(t, readTempLockfilePins(t), "krzema12")
}

// TestParity_RenameKeepsLockedCommit: a rename is the same repository, so
// the rewrite keeps the recorded commit and its `uses:` even when the
// mutable ref has since moved.
func TestParity_RenameKeepsLockedCommit(t *testing.T) {
	reg := &httpmock.Registry{}
	reg.Register(httpmock.ParityQuery, renamedParity)
	reg.Register(httpmock.GraphQLForRepo("krzema12", "github-actions-typing"), httpmock.JSONResponse(map[string]any{
		"data": map[string]any{"a0": httpmock.RepoResponse("typesafegithub/github-actions-typing", parityTestMoved, httpmock.NodeActionYAML)},
	}))
	reg.Register(httpmock.REST("GET", `^/repos/typesafegithub/github-actions-typing$`), httpmock.JSONResponse(map[string]any{
		"full_name": "typesafegithub/github-actions-typing", "id": 1, "owner": map[string]any{"id": 42},
	}))
	path := writeTempWorkflow(t, strings.Replace(transferWorkflow, "@v2.2.2", "@main", 1))
	lock := "version: '" + parserlock.Version + "'\n" + `dependencies:
  'krzema12/github-actions-typing@main':
    ref: 'main'
    commit: 'sha1-` + parityTestSHA + `'
    owner_id: 1
    repo_id: 1
    uses:
      - 'actions/setup-node@v4'
  'actions/setup-node@v4':
    ref: 'v4'
    commit: 'sha1-` + parityTestMoved + `'
    owner_id: 2
    repo_id: 2
workflows:
  '.github/workflows/workflow.yml':
    - 'krzema12/github-actions-typing@main'
`
	require.NoError(t, os.WriteFile(filepath.Join(".github", "workflows", "actions.lock"), []byte(lock), 0o600))

	_, stderr, err := runCommandWithHTTP(t, reg, path)
	require.NoError(t, err, stderr)
	wf, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Contains(t, string(wf), "uses: typesafegithub/github-actions-typing@main")
	got := readTempLockfilePins(t)
	assert.NotContains(t, got, "krzema12")
	assert.Contains(t, got, `'typesafegithub/github-actions-typing@main':
        ref: 'main'
        commit: 'sha1-`+parityTestSHA+`'`)
	assert.Contains(t, got, `uses:
            - 'actions/setup-node@v4'`)
}

// renamedParity answers like httpmock.ParityIntact, with krzema12/github-actions-typing
// redirected to typesafegithub under the same repo ID.
func renamedParity(req *http.Request) (*http.Response, error) {
	resp, err := httpmock.ParityIntact(req)
	if err != nil {
		return nil, err
	}
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	body := strings.Replace(string(b), `"krzema12/github-actions-typing"`, `"typesafegithub/github-actions-typing","databaseId":1`, 1)
	resp.Body = io.NopCloser(strings.NewReader(body))
	return resp, nil
}

func registerTransferResolve(reg *httpmock.Registry, repoID int) {
	reg.Register(httpmock.GraphQLForRepo("krzema12", "github-actions-typing"), httpmock.JSONResponse(map[string]any{
		"data": map[string]any{"a0": httpmock.RepoResponse("typesafegithub/github-actions-typing", parityTestSHA, httpmock.NodeActionYAML)},
	}))
	reg.Register(httpmock.REST("GET", `^/repos/typesafegithub/github-actions-typing$`), httpmock.JSONResponse(map[string]any{
		"full_name": "typesafegithub/github-actions-typing", "id": repoID, "owner": map[string]any{"id": 42},
	}))
	reg.Register(httpmock.REST("GET", `^/repos/typesafegithub/github-actions-typing/tags`),
		httpmock.JSONResponse(httpmock.TagListResponse("v2.2.2", parityTestSHA)))
}

func assertTransferRewritten(t *testing.T, path, stderr string) {
	t.Helper()
	wf, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Contains(t, string(wf), "uses: typesafegithub/github-actions-typing@v2.2.2")
	lock := readTempLockfilePins(t)
	assert.Contains(t, lock, "typesafegithub/github-actions-typing@v2.2.2")
	assert.NotContains(t, lock, "krzema12")
	assert.Contains(t, stderr, "Rewrote krzema12/github-actions-typing@v2.2.2 → typesafegithub/github-actions-typing@v2.2.2 in "+path)
}

// TestParity_RepoIDMismatchSameNameBlocks: the name still resolves, but to
// a different repository. Identity is the repo ID alone.
func TestParity_RepoIDMismatchSameNameBlocks(t *testing.T) {
	for name, mode := range map[string][]string{"default": nil, "verify": {"--verify"}} {
		t.Run(name, func(t *testing.T) {
			reg := &httpmock.Registry{}
			defer reg.Verify(t)
			reg.Register(httpmock.ParityQuery, httpmock.JSONResponse(map[string]any{
				"data": map[string]any{"a0": map[string]any{
					"nameWithOwner": "actions/checkout",
					"databaseId":    99,
					"commit":        map[string]any{"oid": parityTestSHA},
					"tag":           map[string]any{"oid": parityTestSHA},
					"tagRef":        map[string]any{"oid": parityTestSHA},
				}},
			}))
			path := writeTempWorkflow(t, workflowUsing("actions/checkout@v4.2.1"), "actions/checkout@v4.2.1=sha1-"+parityTestSHA)

			_, stderr, err := runCommandWithHTTP(t, reg, append(mode, path)...)
			require.ErrorIs(t, err, errSilent)
			assert.Contains(t, stderr, "different repository than the one locked (repo ID 99, locked 1)")
		})
	}
}

// TestParity_ReclaimedNameBlocks: the name now resolves to a different repo
// ID, so someone else owns it. Both modes block and say why on the terminal.
func TestParity_ReclaimedNameBlocks(t *testing.T) {
	for name, mode := range map[string][]string{"default": nil, "verify": {"--verify"}} {
		t.Run(name, func(t *testing.T) {
			reg := &httpmock.Registry{}
			defer reg.Verify(t)
			reg.Register(httpmock.ParityQuery, transferParity(99))
			path := writeTempWorkflow(t, transferWorkflow, "krzema12/github-actions-typing@v2.2.2=sha1-"+parityTestSHA)

			_, stderr, err := runCommandWithHTTP(t, reg, append(mode, path)...)
			require.ErrorIs(t, err, errSilent)
			assert.Contains(t, stderr, "different repository than the one locked")
		})
	}
}

// TestParity_RelockReclaimedNameKeepsLock: re-resolving a reclaimed name
// yields an attacker's commit. The recorded repo ID must catch it before
// the new commit reaches the lockfile.
func TestParity_RelockReclaimedNameKeepsLock(t *testing.T) {
	reg := &httpmock.Registry{}
	reg.Register(httpmock.ParityQuery, transferParity(99))
	reg.Register(httpmock.GraphQLForRepo("krzema12", "github-actions-typing"), httpmock.JSONResponse(map[string]any{
		"data": map[string]any{"a0": httpmock.RepoResponse("krzema12/github-actions-typing", parityTestMoved, httpmock.NodeActionYAML)},
	}))
	path := writeTempWorkflow(t, transferWorkflow, "krzema12/github-actions-typing@v2.2.2=sha1-"+parityTestSHA)

	// The tag fast-forwarded, so only the repo ID gives the takeover away.
	reg.Register(httpmock.REST("GET", `/compare/`), httpmock.JSONResponse(map[string]any{"status": "ahead", "merge_base_commit": map[string]any{"sha": parityTestSHA}}))
	reg.Register(httpmock.REST("GET", `^/repos/krzema12/github-actions-typing/tags`),
		httpmock.JSONResponse(httpmock.TagListResponse("v2.2.2", parityTestMoved)))

	_, stderr, err := runCommandWithHTTP(t, reg, "--relock", path)
	require.ErrorIs(t, err, errSilent)
	assert.Contains(t, stderr, "different repository than the one locked")
	lock := readTempLockfilePins(t)
	assert.Contains(t, lock, parityTestSHA)
	assert.NotContains(t, lock, parityTestMoved, "the reclaimed repo's commit must not be written")
}

// TestParity_MovedExactTagFollowsHistory: tags move, and the Actions
// backend accepts a pin whose commit is still in the tag's history. --verify
// warns about it; no mode blocks. A tag that lost the commit blocks.
func TestParity_MovedExactTagFollowsHistory(t *testing.T) {
	tests := []struct {
		name   string
		status string
		base   string
		args   []string
		want   string
		block  bool
	}{
		{"moved ahead warns under verify", "ahead", parityTestSHA, []string{"--verify"}, "ref-moved", false},
		{"moved ahead passes a default run", "ahead", parityTestSHA, []string{"--no-onboard", "--no-narrow", "--no-interactive"}, "", false},
		{"lost commit blocks verify", "diverged", "cccccccccccccccccccccccccccccccccccccccc", []string{"--verify"}, "unreachable-pin", true},
		{"lost commit blocks a default run", "diverged", "cccccccccccccccccccccccccccccccccccccccc", []string{"--no-onboard", "--no-narrow", "--no-interactive"}, "unreachable-pin", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reg := &httpmock.Registry{}
			defer reg.Verify(t)
			reg.Register(httpmock.ParityQuery, httpmock.JSONResponse(map[string]any{
				"data": map[string]any{"a0": map[string]any{
					"nameWithOwner": "actions/checkout",
					"commit":        map[string]any{"oid": parityTestSHA},
					"tag":           map[string]any{"oid": parityTestMoved},
					"tagRef":        map[string]any{"oid": parityTestMoved},
				}},
			}))
			reg.Register(httpmock.REST("GET", `repos/actions/checkout/compare/`+parityTestSHA+`\.\.\.`+parityTestMoved),
				httpmock.JSONResponse(map[string]any{"status": tt.status, "merge_base_commit": map[string]any{"sha": tt.base}}))
			path := writeTempWorkflow(t, workflowUsing("actions/checkout@v4.2.1"), "actions/checkout@v4.2.1=sha1-"+parityTestSHA)
			if !tt.block && tt.args[0] == "--verify" {
				registerNodeManifests(reg)
			}
			p, err := runCheckJSON(t, reg, append(tt.args, path)...)
			lock := readTempLockfilePins(t)
			assert.Contains(t, lock, "sha1-"+parityTestSHA)
			assert.NotContains(t, lock, parityTestMoved, "the lock keeps its commit")
			if tt.want == "" {
				require.NoError(t, err)
				assert.True(t, p.Valid)
				assert.Empty(t, p.Findings)
				return
			}
			require.Len(t, p.Findings, 1, "%+v", p.Findings)
			assert.Equal(t, tt.want, p.Findings[0].Category)
			if !tt.block {
				require.NoError(t, err)
				assert.True(t, p.Valid)
				assert.Equal(t, "warning", p.Findings[0].Severity)
				assert.Equal(t, "tag v4.2.1 now points at bbbbbbbbbbbb; locked commit 9ddf35b71a48 is still in its history", p.Findings[0].Detail)
				return
			}
			require.ErrorIs(t, err, errSilent)
			assert.False(t, p.Valid)
			assert.Equal(t, "error", p.Findings[0].Severity)
		})
	}
}

// TestParity_TerminalSeverity: the terminal must agree with the exit code.
// A fail-closed history check prints as an error, never a warning; a moved
// exact tag under --verify prints as a warning on a passing run.
func TestParity_TerminalSeverity(t *testing.T) {
	workflow := workflowUsing("actions/checkout@v4.2.1")
	t.Run("inconclusive is an error", func(t *testing.T) {
		path := writeTempWorkflow(t, workflow, "actions/checkout@v4.2.1=sha1-"+parityTestSHA)
		_, stderr, err := runCommandWithHTTP(t, parityDown{&httpmock.Registry{}}, "--verify", path)
		require.ErrorIs(t, err, errSilent, stderr)
		assert.Contains(t, stderr, "✗ reachability unknown actions/checkout@v4.2.1")
		assert.NotContains(t, stderr, "actions/checkout@v4.2.1: could not verify", "an error must not also print as a warning")
		assert.Contains(t, stderr, "could not verify locked actions/checkout@9ddf35b71a48: GitHub returned 502 Bad Gateway")
		assert.NotContains(t, stderr, "api.github.com", "the request URL stays out of the terminal")
	})
	t.Run("moved exact tag warns", func(t *testing.T) {
		reg := &httpmock.Registry{}
		defer reg.Verify(t)
		reg.Register(httpmock.ParityQuery, httpmock.JSONResponse(map[string]any{
			"data": map[string]any{"a0": map[string]any{
				"nameWithOwner": "actions/checkout",
				"commit":        map[string]any{"oid": parityTestSHA},
				"tag":           map[string]any{"oid": parityTestMoved},
				"tagRef":        map[string]any{"oid": parityTestMoved},
			}},
		}))
		reg.Register(httpmock.REST("GET", `repos/actions/checkout/compare/`),
			httpmock.JSONResponse(map[string]any{"status": "ahead", "merge_base_commit": map[string]any{"sha": parityTestSHA}}))
		registerNodeManifests(reg)
		path := writeTempWorkflow(t, workflow, "actions/checkout@v4.2.1=sha1-"+parityTestSHA)
		_, stderr, err := runCommandWithHTTP(t, reg, "--verify", path)
		require.NoError(t, err, stderr)
		assert.Contains(t, stderr, "actions/checkout@v4.2.1: tag v4.2.1 now points at bbbbbbbbbbbb; locked commit 9ddf35b71a48 is still in its history")
		assert.Contains(t, stderr, "All 1 workflow valid")
		assert.NotContains(t, stderr, "✗")
	})
}

// TestParity_DefaultRunPrintsParityFinding: parity findings have no pin
// record entry, so the fix-mode summary must still print them.
func TestParity_DefaultRunPrintsParityFinding(t *testing.T) {
	reg := &httpmock.Registry{}
	defer reg.Verify(t)
	reg.Register(httpmock.ParityQuery, httpmock.JSONResponse(map[string]any{
		"data": map[string]any{"a0": map[string]any{
			"nameWithOwner": "actions/checkout",
			"commit":        map[string]any{"oid": parityTestSHA},
			"tag":           map[string]any{"oid": parityTestMoved},
			"tagRef":        map[string]any{"oid": parityTestMoved},
		}},
	}))
	reg.Register(httpmock.REST("GET", `repos/actions/checkout/compare/`),
		httpmock.JSONResponse(map[string]any{"status": "diverged", "merge_base_commit": map[string]any{"sha": parityTestMoved}}))
	path := writeTempWorkflow(t, workflowUsing("actions/checkout@v4.2.1"), "actions/checkout@v4.2.1=sha1-"+parityTestSHA)

	_, stderr, err := runCommandWithHTTP(t, reg, path)
	require.ErrorIs(t, err, errSilent)
	assert.Contains(t, stderr, "is not in actions/checkout's history")
}

// TestParity_UnchangedRerunRequestCount guards API kindness: an unchanged
// rerun re-resolves nothing and costs one GraphQL request for the whole
// closure, ref lookups included. A mutable ref that moved upstream stays
// locked (sticky) and adds one compare call.
// Online verification additionally fetches manifests by locked SHA.
func TestParity_UnchangedRerunRequestCount(t *testing.T) {
	reg := &httpmock.Registry{}
	defer reg.Verify(t)
	path := writeParityFixture(t)

	for _, args := range [][]string{{path}, {"--verify", path}} {
		reg.Register(httpmock.ParityQuery, httpmock.ParityIntact)
		wantRequests := 1
		if args[0] == "--verify" {
			registerNodeManifests(reg)
			wantRequests = 2
		}
		ct := &httpmock.CountingTransport{Next: reg}
		p, err := runCheckJSON(t, ct, args...)
		require.NoError(t, err)
		assert.True(t, p.Valid, "%+v", p.Findings)
		assert.Equal(t, wantRequests, ct.Count(), "%v: parity and immutable manifest batches", args)
	}
}

// TestParity_UnchangedRerunRequestCountRESTOnly covers the Dependabot proxy
// path: one repos/{owner}/{repo} call per unique repository plus one commits
// call per ref that still points at its locked commit.
func TestParity_UnchangedRerunRequestCountRESTOnly(t *testing.T) {
	t.Setenv("GH_ACTIONS_LOCK_DEPENDABOT_PROXY", "1")
	reg := &httpmock.Registry{}
	defer reg.Verify(t)
	// Anonymous REST calls use http.DefaultClient.
	ct := &httpmock.CountingTransport{Next: reg}
	orig := http.DefaultTransport
	http.DefaultTransport = ct
	t.Cleanup(func() { http.DefaultTransport = orig })

	for _, repo := range []string{"checkout", "setup-go"} {
		reg.Register(httpmock.REST("GET", `^/repos/actions/`+repo+`$`), httpmock.JSONResponse(map[string]any{
			"full_name": "actions/" + repo, "id": 1, "owner": map[string]any{"id": 1},
		}))
	}
	reg.Register(httpmock.REST("GET", `^/repos/actions/checkout/git/ref/tags/v4$`), httpmock.JSONResponse(httpmock.GitRefResponse("refs/tags/v4", parityTestSHA)))
	reg.Register(httpmock.REST("GET", `^/repos/actions/checkout/git/ref/tags/v4\.2\.1$`), httpmock.JSONResponse(httpmock.GitRefResponse("refs/tags/v4.2.1", parityTestSHA)))
	reg.Register(httpmock.REST("GET", `^/repos/actions/setup-go/git/ref/tags/v5$`), httpmock.JSONResponse(httpmock.GitRefResponse("refs/tags/v5", parityTestSHA)))
	path := writeParityFixture(t)

	p, err := runCheckJSON(t, ct, path)
	require.NoError(t, err)
	assert.True(t, p.Valid, "%+v", p.Findings)
	assert.Equal(t, 5, ct.Count(), "2 repos + 3 pins: %v", ct.Paths())
}

func writeParityFixture(t *testing.T) string {
	t.Helper()
	return writeTempWorkflow(t, workflowUsing("actions/checkout@v4", "actions/checkout@v4.2.1", "actions/setup-go@v5"),
		"actions/checkout@v4=sha1-"+parityTestSHA,
		"actions/checkout@v4.2.1=sha1-"+parityTestSHA,
		"actions/setup-go@v5=sha1-"+parityTestSHA,
	)
}

// TestParity_FirstRunTransferRewrites is #110 on first generation:
// resolution follows the rename, the workflow and lockfile take the
// canonical name, and the same-run parity check costs one request.
func TestParity_FirstRunTransferRewrites(t *testing.T) {
	reg := &httpmock.Registry{}
	defer reg.Verify(t)
	reg.Register(httpmock.ParityQuery, transferParity(502427408))
	registerTransferResolve(reg, 502427408)
	path := writeTempWorkflow(t, transferWorkflow)

	ct := &httpmock.CountingTransport{Next: reg}
	_, stderr, err := runCommandWithHTTP(t, ct, path)
	require.NoError(t, err, stderr)
	assertTransferRewritten(t, path, stderr)
	assert.NotContains(t, stderr, "rewrite it as")
	assert.Equal(t, 2, ct.CountMatching(httpmock.GraphQL("")), "one resolve + one parity request: %v", ct.Paths())
	assert.Contains(t, readTempLockfilePins(t), "repo_id: 502427408")
}

// TestParity_RemoteCompositeTransferBlocks: a redirect inside a composite
// TestParity_RemoteCompositeRenameKeepsName: a redirect inside a remote
// composite can't be rewritten and the runner follows it while the repo ID
// matches, so the child stays keyed as written and only --verify warns.
func TestParity_RemoteCompositeRenameKeepsName(t *testing.T) {
	reg := &httpmock.Registry{}
	reg.Register(httpmock.GraphQLForRepo("acme", "comp"), httpmock.JSONResponse(map[string]any{
		"data": map[string]any{"a0": httpmock.RepoResponse("acme/comp", parityTestMoved,
			"runs:\n  using: composite\n  steps:\n    - uses: krzema12/github-actions-typing@v2.2.2\n")},
	}))
	reg.Register(httpmock.GraphQLForRepo("krzema12", "github-actions-typing"), httpmock.JSONResponse(map[string]any{
		"data": map[string]any{"a0": httpmock.RepoResponse("typesafegithub/github-actions-typing", parityTestSHA, httpmock.NodeActionYAML)},
	}))
	reg.Register(httpmock.ParityQuery, renamedParity)
	reg.Register(httpmock.REST("GET", `^/repos/acme/comp$`), httpmock.JSONResponse(map[string]any{
		"full_name": "acme/comp", "id": 3, "owner": map[string]any{"id": 4},
	}))
	reg.Register(httpmock.REST("GET", `^/repos/krzema12/github-actions-typing$`), httpmock.JSONResponse(map[string]any{
		"full_name": "typesafegithub/github-actions-typing", "id": 1, "owner": map[string]any{"id": 42},
	}))
	path := writeTempWorkflow(t, workflowUsing("acme/comp@v1.0.0"))

	_, stderr, err := runCommandWithHTTP(t, reg, path)
	require.NoError(t, err, stderr)
	assert.NotContains(t, stderr, "upgrade acme/comp@v1.0.0", "fix runs stay quiet; --verify reports it")
	lock := readTempLockfilePins(t)
	assert.Contains(t, lock, "'krzema12/github-actions-typing@v2.2.2':")
	assert.NotContains(t, lock, "typesafegithub")

	reg.Register(httpmock.ParityQuery, renamedParity)
	reg.Register(httpmock.GraphQLForRepo("acme", "comp"), httpmock.JSONResponse(map[string]any{
		"data": map[string]any{"a0": httpmock.RepoResponse("acme/comp", parityTestMoved,
			"runs:\n  using: composite\n  steps:\n    - uses: krzema12/github-actions-typing@v2.2.2\n")},
	}))
	reg.Register(httpmock.GraphQLForRepo("krzema12", "github-actions-typing"), httpmock.JSONResponse(map[string]any{
		"data": map[string]any{"a0": httpmock.RepoResponse("typesafegithub/github-actions-typing", parityTestSHA, httpmock.NodeActionYAML)},
	}))
	_, stderr, err = runCommandWithHTTP(t, reg, "--verify", path)
	require.NoError(t, err, stderr)
	assert.Contains(t, stderr, "upgrade acme/comp@v1.0.0")
}

// TestParity_FirstRunInconclusiveNotWritten: a fresh pin whose identity
// can't be confirmed fails closed.
func TestParity_FirstRunInconclusiveNotWritten(t *testing.T) {
	reg := &httpmock.Registry{}
	reg.Register(httpmock.GraphQLForRepo("actions", "checkout"), httpmock.JSONResponse(map[string]any{
		"data": map[string]any{"a0": httpmock.RepoResponse("actions/checkout", parityTestSHA, httpmock.NodeActionYAML)},
	}))
	reg.Register(httpmock.REST("GET", `^/repos/actions/checkout/tags`),
		httpmock.JSONResponse(httpmock.TagListResponse("v4.2.1", parityTestSHA)))
	path := writeTempWorkflow(t, workflowUsing("actions/checkout@v4.2.1"))

	_, stderr, err := runCommandWithHTTP(t, parityDown{reg}, path)
	require.ErrorIs(t, err, errSilent)
	assert.Contains(t, stderr, "could not verify locked actions/checkout")
	assert.NoFileExists(t, ".github/workflows/actions.lock")
}

// TestParity_FirstRunMixedCaseRefChecked: refs are case-sensitive, so a
// fresh `@Release` must still reach the parity check and fail closed.
func TestParity_FirstRunMixedCaseRefChecked(t *testing.T) {
	reg := &httpmock.Registry{}
	reg.Register(httpmock.GraphQLForRepo("actions", "checkout"), httpmock.JSONResponse(map[string]any{
		"data": map[string]any{"a0": httpmock.RepoResponse("actions/checkout", parityTestSHA, httpmock.NodeActionYAML)},
	}))
	reg.Register(httpmock.REST("GET", `^/repos/actions/checkout$`), httpmock.JSONResponse(map[string]any{
		"full_name": "actions/checkout", "id": 1, "owner": map[string]any{"id": 2},
	}))
	path := writeTempWorkflow(t, workflowUsing("actions/checkout@Release"))

	_, stderr, err := runCommandWithHTTP(t, parityDown{reg}, "--no-narrow", path)
	require.ErrorIs(t, err, errSilent)
	assert.Contains(t, stderr, "could not verify locked")
	assert.NoFileExists(t, ".github/workflows/actions.lock")
}

// TestParity_VerifyHintNamesVerify: a fixable --verify failure points at
// the flag the user actually passed.
func TestParity_VerifyHintNamesVerify(t *testing.T) {
	reg := &httpmock.Registry{}
	reg.Register(httpmock.GraphQLForRepo("actions", "checkout"), httpmock.JSONResponse(map[string]any{
		"data": map[string]any{"a0": httpmock.RepoResponse("actions/checkout", parityTestSHA, httpmock.NodeActionYAML)},
	}))
	reg.Register(httpmock.ParityQuery, httpmock.ParityIntact)
	path := writeTempWorkflow(t, workflowUsing("actions/checkout@v4"))

	_, stderr, err := runCommandWithHTTP(t, reg, "--verify", path)
	require.ErrorIs(t, err, errSilent)
	assert.Contains(t, stderr, "Re-run without --verify to apply fixes.")
}

// parityDown fails every parity query with a server error.
type parityDown struct{ inner http.RoundTripper }

func (p parityDown) RoundTrip(req *http.Request) (*http.Response, error) {
	if httpmock.ParityQuery(req) {
		return httpmock.StatusResponse(http.StatusBadGateway)(req)
	}
	return p.inner.RoundTrip(req)
}

// TestParity_FirstRunRequestCount: generation adds exactly one GraphQL
// parity request on top of resolution, and the pin is written.
func TestParity_FirstRunRequestCount(t *testing.T) {
	reg := &httpmock.Registry{}
	defer reg.Verify(t)
	reg.Register(httpmock.ParityQuery, httpmock.ParityIntact)
	reg.Register(httpmock.GraphQLForRepo("actions", "checkout"), httpmock.JSONResponse(map[string]any{
		"data": map[string]any{"a0": httpmock.RepoResponse("actions/checkout", parityTestSHA, httpmock.NodeActionYAML)},
	}))
	reg.Register(httpmock.REST("GET", `^/repos/actions/checkout$`), httpmock.JSONResponse(map[string]any{
		"full_name": "actions/checkout", "id": 1, "owner": map[string]any{"id": 1},
	}))
	reg.Register(httpmock.REST("GET", `^/repos/actions/checkout/tags`),
		httpmock.JSONResponse(httpmock.TagListResponse("v4.2.1", parityTestSHA)))
	path := writeTempWorkflow(t, workflowUsing("actions/checkout@v4.2.1"))

	ct := &httpmock.CountingTransport{Next: reg}
	_, _, err := runCommandWithHTTP(t, ct, path)
	require.NoError(t, err)
	assert.Equal(t, 2, ct.CountMatching(httpmock.GraphQL("")), "resolve + parity: %v", ct.Paths())
	assert.Contains(t, readTempLockfilePins(t), "actions/checkout@v4.2.1")
}

// useDefaultTransport routes anonymous REST calls, which use
// http.DefaultTransport, through rt for the rest of the test.
func useDefaultTransport(t *testing.T, rt http.RoundTripper) {
	t.Helper()
	orig := http.DefaultTransport
	http.DefaultTransport = rt
	t.Cleanup(func() { http.DefaultTransport = orig })
}

// TestParity_DependabotProxyInconclusiveAborts: under the Dependabot proxy
// a rate limit or server error on a recorded pin aborts with exit 2 so
// Dependabot retries, instead of passing on a warning. Outside the proxy
// the same failure stays a warning.
func TestParity_DependabotProxyInconclusiveAborts(t *testing.T) {
	for name, tc := range map[string]struct {
		resp   httpmock.Responder
		status string
	}{
		"rate-limited 403":           {httpmock.StatusResponseWithHeaders(http.StatusForbidden, map[string]string{"X-RateLimit-Remaining": "0"}), "GitHub returned 403 Forbidden"},
		"secondary rate-limited 403": {httpmock.StatusResponseWithHeaders(http.StatusForbidden, map[string]string{"Retry-After": "60"}), "GitHub returned 403 Forbidden"},
		"503":                        {httpmock.StatusResponse(http.StatusServiceUnavailable), "GitHub returned 503 Service Unavailable"},
	} {
		t.Run(name, func(t *testing.T) {
			for _, args := range [][]string{nil, {"--verify"}} {
				t.Setenv("GH_ACTIONS_LOCK_DEPENDABOT_PROXY", "1")
				reg := &httpmock.Registry{}
				reg.Register(httpmock.REST("GET", `^/repos/actions/checkout$`), tc.resp)
				useDefaultTransport(t, reg)
				path := writeTempWorkflow(t, workflowUsing("actions/checkout@v4.2.1"), "actions/checkout@v4.2.1=sha1-"+parityTestSHA)
				lockBefore := readTempLockfilePins(t)
				wfBefore, err := os.ReadFile(path)
				require.NoError(t, err)

				_, stderr, err := runCommandWithHTTP(t, reg, append(args, path)...)
				require.Error(t, err, stderr)
				assert.NotErrorIs(t, err, errSilent, "a tool failure exits 2")
				assert.Contains(t, err.Error(), tc.status)
				assert.Contains(t, err.Error(), "actions/checkout")
				assert.Equal(t, lockBefore, readTempLockfilePins(t))
				wfAfter, err := os.ReadFile(path)
				require.NoError(t, err)
				assert.Equal(t, string(wfBefore), string(wfAfter))
			}
		})
	}
}

// TestParity_InconclusiveBlocksWithoutProxy: a ref check that can't finish
// fails closed, exact tags included; only the proxy turns it into a retry.
func TestParity_InconclusiveBlocksWithoutProxy(t *testing.T) {
	reg := &httpmock.Registry{}
	registerNodeManifests(reg)
	path := writeTempWorkflow(t, workflowUsing("actions/checkout@v4.2.1"), "actions/checkout@v4.2.1=sha1-"+parityTestSHA)

	_, stderr, err := runCommandWithHTTP(t, parityDown{reg}, "--verify", path)
	require.ErrorIs(t, err, errSilent, stderr)
	assert.Contains(t, stderr, "could not verify locked actions/checkout")
}

// TestParity_DependabotProxyKeepsRenamedName: Dependabot reads back only
// actions.lock, so under the proxy a same-ID rename must not rewrite
// `uses:` or re-key the pin; the warning still reaches --json.
func TestParity_DependabotProxyKeepsRenamedName(t *testing.T) {
	t.Setenv("GH_ACTIONS_LOCK_DEPENDABOT_PROXY", "1")
	reg := &httpmock.Registry{}
	reg.Register(httpmock.REST("GET", `^/repos/krzema12/github-actions-typing$`), httpmock.JSONResponse(map[string]any{
		"full_name": "typesafegithub/github-actions-typing", "id": 1, "owner": map[string]any{"id": 42},
	}))
	reg.Register(httpmock.REST("GET", `^/repos/krzema12/github-actions-typing/git/ref/tags/v2\.2\.2$`),
		httpmock.JSONResponse(httpmock.GitRefResponse("refs/tags/v2.2.2", parityTestSHA)))
	useDefaultTransport(t, reg)
	path := writeTempWorkflow(t, transferWorkflow, "krzema12/github-actions-typing@v2.2.2=sha1-"+parityTestSHA)
	wfBefore, err := os.ReadFile(path)
	require.NoError(t, err)

	stdout, stderr, err := runCommandWithHTTP(t, reg, "--no-onboard", "--no-narrow", "--no-interactive", "--json=findings", path)
	require.NoError(t, err, stderr)
	var p checkResult
	require.NoError(t, json.Unmarshal([]byte(stdout), &p), stdout)
	var renamed bool
	for _, f := range p.Findings {
		renamed = renamed || f.Category == "repo-renamed"
	}
	assert.True(t, renamed, "%+v", p.Findings)
	wfAfter, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, string(wfBefore), string(wfAfter))
	lock := readTempLockfilePins(t)
	assert.Contains(t, lock, "'krzema12/github-actions-typing@v2.2.2':")
	assert.Contains(t, lock, "sha1-"+parityTestSHA)
	assert.NotContains(t, lock, "typesafegithub")
}

// TestParity_DependabotProxyFirstRunKeepsRenamedName: the same holds when
// resolution itself follows the rename on first generation.
func TestParity_DependabotProxyFirstRunKeepsRenamedName(t *testing.T) {
	t.Setenv("GH_ACTIONS_LOCK_DEPENDABOT_PROXY", "1")
	reg := &httpmock.Registry{}
	reg.Register(httpmock.REST("GET", `^/repos/krzema12/github-actions-typing$`), httpmock.JSONResponse(map[string]any{
		"full_name": "typesafegithub/github-actions-typing", "id": 1, "owner": map[string]any{"id": 42},
	}))
	commit := httpmock.JSONResponse(map[string]any{"sha": parityTestSHA})
	reg.Register(httpmock.REST("GET", `^/repos/typesafegithub/github-actions-typing/commits/v2\.2\.2$`), commit)
	reg.Register(httpmock.REST("GET", `^/repos/krzema12/github-actions-typing/git/ref/tags/v2\.2\.2$`), httpmock.JSONResponse(httpmock.GitRefResponse("refs/tags/v2.2.2", parityTestSHA)))
	reg.Register(httpmock.REST("GET", `^/repos/typesafegithub/github-actions-typing/contents/action\.yml$`),
		httpmock.JSONResponse(map[string]any{"encoding": "base64", "content": base64.StdEncoding.EncodeToString([]byte(httpmock.NodeActionYAML))}))
	useDefaultTransport(t, reg)
	path := writeTempWorkflow(t, transferWorkflow)

	_, stderr, err := runCommandWithHTTP(t, reg, "--no-narrow", "--no-interactive", path)
	require.NoError(t, err, stderr)
	wf, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, strings.TrimSpace(transferWorkflow)+"\n", string(wf))
	lock := readTempLockfilePins(t)
	assert.Contains(t, lock, "'krzema12/github-actions-typing@v2.2.2':")
	assert.NotContains(t, lock, "typesafegithub")
}

// TestParity_VerifyMutableRefHistory: --verify checks that a commit locked
// under a mutable ref is still in that ref's history, since a fork-network
// commit exists under the upstream name. A legitimately moved ref stays
// sticky and silent, and an inconclusive check fails closed. A plain run
// makes one compare call.
func TestParity_VerifyMutableRefHistory(t *testing.T) {
	const live = "cccccccccccccccccccccccccccccccccccccccc"
	compareURL := `repos/actions/checkout/compare/` + parityTestSHA + `\.\.\.` + live
	tests := []struct {
		name    string
		compare httpmock.Responder
		// want is the finding category, "" for a clean pass.
		want string
	}{
		{"impostor blocks", httpmock.JSONResponse(map[string]any{"status": "diverged", "merge_base_commit": map[string]any{"sha": parityTestMoved}}), "unreachable-pin"},
		{"moved forward passes", httpmock.JSONResponse(map[string]any{"status": "ahead", "merge_base_commit": map[string]any{"sha": parityTestSHA}}), ""},
		{"compare 503 blocks", httpmock.StatusResponse(http.StatusServiceUnavailable), "reachability-unknown"},
	}
	for _, tt := range tests {
		for _, proxy := range []bool{false, true} {
			t.Run(tt.name+"/"+map[bool]string{false: "graphql", true: "dependabot proxy"}[proxy], func(t *testing.T) {
				reg := &httpmock.Registry{}
				if proxy {
					t.Setenv("GH_ACTIONS_LOCK_DEPENDABOT_PROXY", "1")
					useDefaultTransport(t, reg)
					reg.Register(httpmock.REST("GET", `^/repos/actions/checkout$`), httpmock.JSONResponse(map[string]any{
						"full_name": "actions/checkout", "id": 1, "owner": map[string]any{"id": 1},
					}))
					reg.Register(httpmock.REST("GET", `^/repos/actions/checkout/commits/`+parityTestSHA+`$`), httpmock.JSONResponse(map[string]any{"sha": parityTestSHA}))
					reg.Register(httpmock.REST("GET", `^/repos/actions/checkout/git/ref/tags/v4$`), httpmock.JSONResponse(httpmock.GitRefResponse("refs/tags/v4", live)))
				} else {
					reg.Register(httpmock.GraphQL(`head: object\(expression`), httpmock.JSONResponse(map[string]any{
						"data": map[string]any{"a0": map[string]any{
							"nameWithOwner": "actions/checkout",
							"commit":        map[string]any{"oid": parityTestSHA},
							"tag":           map[string]any{"oid": live},
							"tagRef":        map[string]any{"oid": live},
						}},
					}))
				}
				for range 10 {
					reg.Register(httpmock.REST("GET", compareURL), tt.compare)
				}
				if tt.want == "" {
					registerLockedCheckoutManifest(reg, proxy)
				}
				path := writeTempWorkflow(t, workflowUsing("actions/checkout@v4"), "actions/checkout@v4=sha1-"+parityTestSHA)

				stdout, stderr, err := runCommandWithHTTP(t, reg, "--verify", "--json=valid,findings", path)
				if proxy && tt.want == "reachability-unknown" {
					require.Error(t, err, stderr)
					assert.NotErrorIs(t, err, errSilent, "inconclusive under the proxy is a retry")
					assert.Contains(t, err.Error(), "GitHub returned 503 Service Unavailable")
					return
				}
				var p checkResult
				require.NoError(t, json.Unmarshal([]byte(stdout), &p), stdout+stderr)
				switch tt.want {
				case "":
					require.NoError(t, err, stderr)
					assert.Empty(t, p.Findings, "a moved mutable ref stays sticky and silent")
				case "unreachable-pin":
					require.ErrorIs(t, err, errSilent)
					require.Len(t, p.Findings, 1)
					assert.Equal(t, "unreachable-pin", p.Findings[0].Category)
					assert.Equal(t, "error", p.Findings[0].Severity)
					assert.Contains(t, p.Findings[0].Detail, "is not in actions/checkout's history")
					assert.Contains(t, p.Findings[0].Detail, "v4 currently points at cccccccccccc")
					assert.NotEmpty(t, p.Findings[0].DocURL, "the impostor keeps its doc link")
				default:
					require.ErrorIs(t, err, errSilent, stderr)
					require.Len(t, p.Findings, 1)
					assert.Equal(t, tt.want, p.Findings[0].Category)
					assert.Equal(t, "error", p.Findings[0].Severity)
				}
			})
		}
	}

	for _, proxy := range []bool{false, true} {
		t.Run("plain run makes one compare call/"+map[bool]string{false: "graphql", true: "dependabot proxy"}[proxy], func(t *testing.T) {
			reg := &httpmock.Registry{}
			if proxy {
				t.Setenv("GH_ACTIONS_LOCK_DEPENDABOT_PROXY", "1")
				useDefaultTransport(t, reg)
				reg.Register(httpmock.REST("GET", `^/repos/actions/checkout$`), httpmock.JSONResponse(map[string]any{
					"full_name": "actions/checkout", "id": 1, "owner": map[string]any{"id": 1},
				}))
				reg.Register(httpmock.REST("GET", `^/repos/actions/checkout/commits/`+parityTestSHA+`$`), httpmock.JSONResponse(map[string]any{"sha": parityTestSHA}))
				reg.Register(httpmock.REST("GET", `^/repos/actions/checkout/git/ref/tags/v4$`), httpmock.JSONResponse(httpmock.GitRefResponse("refs/tags/v4", live)))
			} else {
				reg.Register(httpmock.GraphQL(`head: object\(expression`), httpmock.JSONResponse(map[string]any{
					"data": map[string]any{"a0": map[string]any{
						"nameWithOwner": "actions/checkout",
						"commit":        map[string]any{"oid": parityTestSHA},
						"tag":           map[string]any{"oid": live},
						"tagRef":        map[string]any{"oid": live},
					}},
				}))
			}
			reg.Register(httpmock.REST("GET", compareURL), httpmock.JSONResponse(map[string]any{"status": "ahead", "merge_base_commit": map[string]any{"sha": parityTestSHA}}))
			path := writeTempWorkflow(t, workflowUsing("actions/checkout@v4"), "actions/checkout@v4=sha1-"+parityTestSHA)

			_, stderr, err := runCommandWithHTTP(t, reg, "--no-onboard", "--no-narrow", "--no-interactive", path)
			require.NoError(t, err, stderr)
			var compares int
			for _, req := range reg.Requests {
				if strings.Contains(req.URL.Path, "/compare/") {
					compares++
				}
			}
			assert.Equal(t, 1, compares)
		})
	}
}

// TestParity_VerifyMissingMutableRefBlocks: under --verify a mutable ref that
// is no longer a tag or branch is a definitive answer the Actions backend
// resolver rejects, so it blocks rather than passing on the commit alone.
func TestParity_VerifyMissingMutableRefBlocks(t *testing.T) {
	for _, proxy := range []bool{false, true} {
		t.Run(map[bool]string{false: "graphql", true: "dependabot proxy"}[proxy], func(t *testing.T) {
			reg := &httpmock.Registry{}
			if proxy {
				t.Setenv("GH_ACTIONS_LOCK_DEPENDABOT_PROXY", "1")
				useDefaultTransport(t, reg)
			}
			// One pass for --json, one for the terminal.
			for range 2 {
				if proxy {
					reg.Register(httpmock.REST("GET", `^/repos/actions/checkout$`), httpmock.JSONResponse(map[string]any{
						"full_name": "actions/checkout", "id": 1, "owner": map[string]any{"id": 1},
					}))
					reg.Register(httpmock.REST("GET", `^/repos/actions/checkout/commits/`+parityTestSHA+`$`), httpmock.JSONResponse(map[string]any{"sha": parityTestSHA}))
					reg.Register(httpmock.REST("GET", `^/repos/actions/checkout/git/ref/tags/v4$`), httpmock.StatusResponse(http.StatusNotFound))
					reg.Register(httpmock.REST("GET", `^/repos/actions/checkout/git/ref/heads/v4$`), httpmock.StatusResponse(http.StatusNotFound))
				} else {
					reg.Register(httpmock.GraphQL(`head: object\(expression`), httpmock.JSONResponse(map[string]any{
						"data": map[string]any{"a0": map[string]any{
							"nameWithOwner": "actions/checkout",
							"commit":        map[string]any{"oid": parityTestSHA},
							"head":          nil,
							"headRef":       nil,
						}},
					}))
				}
			}
			path := writeTempWorkflow(t, workflowUsing("actions/checkout@v4"), "actions/checkout@v4=sha1-"+parityTestSHA)

			stdout, stderr, err := runCommandWithHTTP(t, reg, "--verify", "--json=valid,findings", path)
			require.ErrorIs(t, err, errSilent, stderr)
			var p checkResult
			require.NoError(t, json.Unmarshal([]byte(stdout), &p), stdout+stderr)
			require.Len(t, p.Findings, 1)
			assert.Equal(t, "unreachable-pin", p.Findings[0].Category)
			assert.Equal(t, "error", p.Findings[0].Severity)
			assert.Equal(t, "ref v4 is not a tag or branch of actions/checkout; the Actions backend will reject this pin", p.Findings[0].Detail)
			assert.Equal(t, "point `uses:` at a tag or branch that exists, then run `gh actions-lock`", p.Findings[0].Remediation)
			assert.Empty(t, p.Findings[0].DocURL, "the hardening guide is unrelated to a missing ref")

			stdout, stderr, err = runCommandWithHTTP(t, reg, "--verify", path)
			require.ErrorIs(t, err, errSilent)
			term := stdout + stderr
			assert.Contains(t, term, "Ref no longer exists actions/checkout@v4")
			assert.Contains(t, term, "ref v4 is not a tag or branch of actions/checkout; the Actions backend will reject this pin")
			assert.Contains(t, term, "point `uses:` at a tag or branch that exists, then run `gh actions-lock`")
			assert.NotContains(t, term, "not in upstream history")
			assert.NotContains(t, term, "/releases")
			assert.NotContains(t, term, "see:")
		})
	}
}

// TestParity_DefaultAndNoFixCheckMutableHistory: fix runs check a mutable
// ref's history like --verify (#174), and block without rewriting the lock.
func TestParity_DefaultAndNoFixCheckMutableHistory(t *testing.T) {
	const live = "cccccccccccccccccccccccccccccccccccccccc"
	tests := []struct {
		name    string
		tag     any
		compare httpmock.Responder
		want    string
	}{
		{"impostor blocks", map[string]any{"oid": live}, httpmock.JSONResponse(map[string]any{"status": "diverged", "merge_base_commit": map[string]any{"sha": parityTestMoved}}), "unreachable-pin"},
		{"inconclusive blocks", map[string]any{"oid": live}, httpmock.StatusResponse(http.StatusServiceUnavailable), "reachability-unknown"},
		{"missing ref blocks", nil, nil, "unreachable-pin"},
	}
	modes := map[string][]string{
		"default": {"--no-onboard", "--no-narrow", "--no-interactive"},
		"no-fix":  {"--no-fix"},
	}
	for _, tt := range tests {
		for mode, args := range modes {
			t.Run(tt.name+"/"+mode, func(t *testing.T) {
				reg := &httpmock.Registry{}
				reg.Register(httpmock.GraphQL(`head: object\(expression`), httpmock.JSONResponse(map[string]any{
					"data": map[string]any{"a0": map[string]any{
						"nameWithOwner": "actions/checkout",
						"commit":        map[string]any{"oid": parityTestSHA},
						"tag":           tt.tag,
						"tagRef":        tt.tag,
					}},
				}))
				if tt.compare != nil {
					for range 10 {
						reg.Register(httpmock.REST("GET", `repos/actions/checkout/compare/`+parityTestSHA+`\.\.\.`+live), tt.compare)
					}
				}
				path := writeTempWorkflow(t, workflowUsing("actions/checkout@v4"), "actions/checkout@v4=sha1-"+parityTestSHA)
				lockBefore := readTempLockfilePins(t)

				p, err := runCheckJSON(t, reg, append(args, path)...)
				require.ErrorIs(t, err, errSilent)
				require.Len(t, p.Findings, 1, "%+v", p.Findings)
				assert.Equal(t, tt.want, p.Findings[0].Category)
				assert.Equal(t, "error", p.Findings[0].Severity)
				assert.Equal(t, lockBefore, readTempLockfilePins(t), "a blocked pin must not be rewritten")
			})
		}
	}
}

// TestParity_VerifyRefCaseSensitive: refs differing only in case are
// distinct refs to the runner, so each gets its own history check.
func TestParity_VerifyRefCaseSensitive(t *testing.T) {
	for _, proxy := range []bool{false, true} {
		t.Run(map[bool]string{false: "graphql", true: "dependabot proxy"}[proxy], func(t *testing.T) {
			reg := &httpmock.Registry{}
			var heads []string
			if proxy {
				t.Setenv("GH_ACTIONS_LOCK_DEPENDABOT_PROXY", "1")
				useDefaultTransport(t, reg)
				for range 2 {
					reg.Register(httpmock.REST("GET", `^/repos/actions/checkout$`), httpmock.JSONResponse(map[string]any{
						"full_name": "actions/checkout", "id": 1, "owner": map[string]any{"id": 1},
					}))
					reg.Register(httpmock.REST("GET", `^/repos/actions/checkout/commits/`+parityTestSHA+`$`), httpmock.JSONResponse(map[string]any{"sha": parityTestSHA}))
				}
				for _, ref := range []string{"Main", "main"} {
					reg.Register(httpmock.REST("GET", `^/repos/actions/checkout/git/ref/tags/`+ref+`$`), func(req *http.Request) (*http.Response, error) {
						heads = append(heads, ref)
						return httpmock.JSONResponse(httpmock.GitRefResponse("refs/tags/"+ref, parityTestSHA))(req)
					})
				}
			} else {
				reg.Register(httpmock.GraphQL(`head: object\(expression`), func(req *http.Request) (*http.Response, error) {
					body, _ := io.ReadAll(req.Body)
					for _, ref := range []string{"Main", "main"} {
						if strings.Contains(string(body), `"refs/tags/`+ref+`^{commit}"`) {
							heads = append(heads, ref)
						}
					}
					repo := map[string]any{"nameWithOwner": "actions/checkout", "commit": map[string]any{"oid": parityTestSHA}, "head": map[string]any{"oid": parityTestSHA}, "headRef": map[string]any{}}
					return httpmock.JSONResponse(map[string]any{"data": map[string]any{"a0": repo, "a1": repo}})(req)
				})
			}
			path := writeTempWorkflow(t, workflowUsing("actions/checkout@Main", "actions/checkout@main"), "actions/checkout@Main=sha1-"+parityTestSHA, "actions/checkout@main=sha1-"+parityTestSHA)

			for range 2 {
				registerLockedCheckoutManifest(reg, proxy)
			}
			_, stderr, err := runCommandWithHTTP(t, reg, "--verify", path)
			require.NoError(t, err, stderr)
			assert.ElementsMatch(t, []string{"Main", "main"}, heads)
		})
	}
}

// TestParity_CancelDuringAncestryAborts: a cancellation while a run
// checks history must not turn into a normal result or, behind the proxy,
// into an inconclusive-lookup error.
func TestParity_CancelDuringAncestryAborts(t *testing.T) {
	const live = "cccccccccccccccccccccccccccccccccccccccc"
	for _, proxy := range []bool{false, true} {
		t.Run(map[bool]string{false: "graphql", true: "dependabot proxy"}[proxy], func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			reg := &httpmock.Registry{}
			if proxy {
				t.Setenv("GH_ACTIONS_LOCK_DEPENDABOT_PROXY", "1")
				useDefaultTransport(t, reg)
				reg.Register(httpmock.REST("GET", `^/repos/actions/checkout$`), httpmock.JSONResponse(map[string]any{
					"full_name": "actions/checkout", "id": 1, "owner": map[string]any{"id": 1},
				}))
				reg.Register(httpmock.REST("GET", `^/repos/actions/checkout/commits/`+parityTestSHA+`$`), httpmock.JSONResponse(map[string]any{"sha": parityTestSHA}))
				reg.Register(httpmock.REST("GET", `^/repos/actions/checkout/git/ref/tags/v4$`), httpmock.JSONResponse(httpmock.GitRefResponse("refs/tags/v4", live)))
			} else {
				reg.Register(httpmock.GraphQL(`head: object\(expression`), httpmock.JSONResponse(map[string]any{
					"data": map[string]any{"a0": map[string]any{
						"nameWithOwner": "actions/checkout",
						"commit":        map[string]any{"oid": parityTestSHA},
						"head":          map[string]any{"oid": live},
						"headRef":       map[string]any{"oid": live},
					}},
				}))
			}
			for range 10 {
				reg.Register(httpmock.REST("GET", `repos/actions/checkout/compare/`), func(req *http.Request) (*http.Response, error) {
					cancel()
					return nil, ctx.Err()
				})
			}
			path := writeTempWorkflow(t, workflowUsing("actions/checkout@v4"), "actions/checkout@v4=sha1-"+parityTestSHA)
			lockBefore := readTempLockfilePins(t)

			_, stderr, err := runCommandWithHTTPContext(t, ctx, reg, "--verify", path)
			require.ErrorIs(t, err, context.Canceled, stderr)
			assert.NotContains(t, err.Error(), "could not", "the cancellation is reported, not the lookup it interrupted")
			assert.Equal(t, lockBefore, readTempLockfilePins(t))
		})
	}
}

// fullSHAModes are the run modes that must all check a full-SHA key's
// provenance (#174): a fix run that trusts the lock would launder a
// fork-only commit by rewriting `uses:` to the recorded tag.
var fullSHAModes = map[string][]string{
	"default": nil,
	"verify":  {"--verify"},
	"no-fix":  {"--no-fix"},
	"relock":  {"--relock"},
}

// TestParity_FullSHAKeyProvenance is #174: a fork-network commit resolves
// under the upstream name, so every mode checks a locked commit against the
// lock entry's ref, looked up only as a tag, then a branch.
func TestParity_FullSHAKeyProvenance(t *testing.T) {
	const live = "cccccccccccccccccccccccccccccccccccccccc"
	diverged := httpmock.JSONResponse(map[string]any{"status": "diverged", "merge_base_commit": map[string]any{"sha": parityTestMoved}})
	ahead := httpmock.JSONResponse(map[string]any{"status": "ahead", "merge_base_commit": map[string]any{"sha": parityTestSHA}})
	tests := []struct {
		name    string
		ref     string
		tag     any
		head    any
		compare httpmock.Responder
		// want is the blocking finding category, "" for a pass.
		want   string
		detail string
	}{
		{
			name: "fork commit under recorded tag blocks", ref: "v6.1.0",
			head: map[string]any{"oid": live}, compare: diverged,
			want: "unreachable-pin", detail: "v6.1.0 currently points at cccccccccccc",
		},
		{
			// Unqualified, pull/1/head would resolve to refs/pull/1/head.
			name: "recorded pull request ref blocks", ref: "pull/1/head",
			head: nil,
			want: "unreachable-pin", detail: "ref pull/1/head is not a tag or branch of actions/checkout",
		},
		{
			name: "inconclusive history check blocks", ref: "main",
			head: map[string]any{"oid": live}, compare: httpmock.StatusResponse(http.StatusForbidden),
			want: "reachability-unknown", detail: "checking main history",
		},
		{
			name: "commit in recorded ref passes", ref: "main",
			head: map[string]any{"oid": live}, compare: ahead,
		},
	}
	for _, tt := range tests {
		for mode, args := range fullSHAModes {
			t.Run(tt.name+"/"+mode, func(t *testing.T) {
				reg := &httpmock.Registry{}
				reg.Register(httpmock.ParityQuery, func(req *http.Request) (*http.Response, error) {
					var body struct {
						Variables map[string]any `json:"variables"`
					}
					require.NoError(t, json.NewDecoder(req.Body).Decode(&body))
					assert.Equal(t, "refs/tags/"+tt.ref+"^{commit}", body.Variables["tag0"], "the recorded ref is looked up only as a tag or branch")
					assert.Equal(t, "refs/heads/"+tt.ref+"^{commit}", body.Variables["head0"], "the recorded ref is looked up only as a tag or branch")
					data := map[string]any{}
					for i := 0; ; i++ {
						if _, ok := body.Variables[fmt.Sprintf("name%d", i)]; !ok {
							break
						}
						tag, head := tt.tag, tt.head
						// The full-SHA pin --relock just resolved has no ref
						// to look up.
						if _, ok := body.Variables[fmt.Sprintf("tag%d", i)]; !ok {
							tag, head = nil, nil
						}
						data[fmt.Sprintf("a%d", i)] = map[string]any{
							"nameWithOwner": "actions/checkout",
							"databaseId":    1,
							"commit":        map[string]any{"oid": parityTestSHA},
							"tag":           tag,
							"tagRef":        tag,
							"head":          head,
							"headRef":       head,
						}
					}
					return httpmock.JSONResponse(map[string]any{"data": data})(req)
				})
				if tt.compare != nil {
					reg.Register(httpmock.REST("GET", `repos/actions/checkout/compare/`+parityTestSHA+`\.\.\.`+live), tt.compare)
				}
				if tt.want == "" {
					reg.Register(httpmock.REST("GET", `repos/actions/checkout/tags`), httpmock.JSONResponse(httpmock.TagListResponse("v6.1.0", parityTestSHA)))
					if mode == "verify" || mode == "relock" {
						registerNodeManifests(reg)
					}
					if mode == "relock" {
						httpmock.NoTagsNamed(reg, "actions", "checkout")
						reg.Register(httpmock.REST("GET", `repos/actions/checkout$`),
							httpmock.JSONResponse(map[string]any{"default_branch": "main"}))
						reg.Register(httpmock.REST("GET", `repos/actions/checkout/git/ref/heads/main`),
							httpmock.JSONResponse(map[string]any{
								"ref": "refs/heads/main", "object": map[string]any{"sha": parityTestSHA, "type": "commit"},
							}))
					}
				}
				path := writeFullSHAWorkflow(t, tt.ref)

				assertFullSHARun(t, reg, args, path, tt.want, tt.detail)
			})
		}
	}
}

// TestParity_InvalidEntryRefIsParseError: an entry ref the Actions backend
// resolver would reject makes the lockfile invalid in every mode, with
// exit 2 and nothing rewritten, so a fix run can't launder it into a `uses:`
// rewrite. TestValidEntryRef covers the other rejected forms.
func TestParity_InvalidEntryRefIsParseError(t *testing.T) {
	tests := []struct {
		name string
		key  string
		ref  string
	}{
		{"full-SHA key records the SHA", parityTestSHA, parityTestSHA},
		{"full-SHA key records a qualified branch", parityTestSHA, "refs/heads/main"},
		{"full-SHA key records revision syntax", parityTestSHA, "main~1"},
		{"full-SHA key records a describe name", parityTestSHA, "v6.1.0-1-g373d742f768e"},
	}
	for _, tt := range tests {
		for mode, args := range fullSHAModes {
			t.Run(tt.name+"/"+mode, func(t *testing.T) {
				reg := &httpmock.Registry{}
				defer reg.Verify(t)
				path := writeTempWorkflow(t, `
name: ci
on: push
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@`+tt.key+`
`, "actions/checkout@"+tt.key+"=sha1-"+parityTestSHA)
				lockPath := filepath.Join(".github", "workflows", "actions.lock")
				lock := strings.Replace(readTempLockfilePins(t), "ref: '"+tt.key+"'", "ref: '"+tt.ref+"'", 1)
				require.NoError(t, os.WriteFile(lockPath, []byte(lock), 0o600))
				wfBefore, err := os.ReadFile(path)
				require.NoError(t, err)

				_, stderr, err := runCommandWithHTTP(t, reg, append(args, "--no-interactive", path)...)
				require.Error(t, err, stderr)
				assert.NotErrorIs(t, err, errSilent, "a bad entry ref is a parse error (exit 2), not a finding")
				assert.Contains(t, err.Error(), "actions/checkout@"+tt.key)
				assert.Empty(t, reg.Requests, "nothing is resolved from an invalid lockfile")
				wfAfter, err := os.ReadFile(path)
				require.NoError(t, err)
				assert.Equal(t, string(wfBefore), string(wfAfter))
				assert.Equal(t, lock, readTempLockfilePins(t))
			})
		}
	}
}

// TestParity_NonBranchRefRefused: a key whose ref is neither a tag nor a
// branch resolves by rev-parse rules but the Actions backend resolver rejects
// it, so no mode may write a lock for it, including --relock, which otherwise
// drops findings that re-resolving would repair. An existing lock entry
// recording a refs/ name is already invalid.
func TestParity_NonBranchRefRefused(t *testing.T) {
	for _, ref := range []string{"pull/2/head", "refs/pull/2/head"} {
		for _, locked := range []bool{false, true} {
			for mode, args := range map[string][]string{"default": nil, "relock": {"--relock"}} {
				name := ref + "/" + mode + "/" + map[bool]string{false: "first run", true: "locked"}[locked]
				t.Run(name, func(t *testing.T) {
					reg := &httpmock.Registry{}
					// The parity stub goes first: the resolve stub matches any query for the repo.
					reg.Register(httpmock.ParityQuery, func(req *http.Request) (*http.Response, error) {
						var body struct {
							Variables map[string]any `json:"variables"`
						}
						require.NoError(t, json.NewDecoder(req.Body).Decode(&body))
						assert.Equal(t, "refs/tags/"+ref+"^{commit}", body.Variables["tag0"])
						assert.Equal(t, "refs/heads/"+ref+"^{commit}", body.Variables["head0"])
						return httpmock.JSONResponse(map[string]any{
							"data": map[string]any{"a0": map[string]any{
								"nameWithOwner": "actions/checkout",
								"databaseId":    1,
								"commit":        map[string]any{"oid": parityTestSHA},
							}},
						})(req)
					})
					reg.Register(httpmock.GraphQLForRepo("actions", "checkout"), httpmock.JSONResponse(map[string]any{
						"data": map[string]any{"a0": httpmock.RepoResponse("actions/checkout", parityTestSHA, httpmock.NodeActionYAML)},
					}))
					reg.Register(httpmock.REST("GET", `^/repos/actions/checkout$`), httpmock.JSONResponse(map[string]any{
						"full_name": "actions/checkout", "id": 1, "owner": map[string]any{"id": 1},
					}))
					var pins []string
					if locked {
						pins = []string{"actions/checkout@" + ref + "=sha1-" + parityTestSHA}
					}
					path := writeTempWorkflow(t, `
name: ci
on: push
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@`+ref+`
`, pins...)
					lockPath := filepath.Join(".github", "workflows", "actions.lock")
					lockBefore, _ := os.ReadFile(lockPath)
					wfBefore, err := os.ReadFile(path)
					require.NoError(t, err)

					cmdArgs := append([]string{"--no-narrow", "--no-interactive"}, args...)
					if locked && strings.HasPrefix(ref, "refs/") {
						_, _, err := runCommandWithHTTP(t, reg, append(cmdArgs, path)...)
						require.Error(t, err)
						assert.NotErrorIs(t, err, errSilent, "an invalid lockfile exits 2")
						assert.Contains(t, err.Error(), "actions/checkout@"+ref)
					} else {
						p, err := runCheckJSON(t, reg, append(cmdArgs, path)...)
						require.ErrorIs(t, err, errSilent)
						var unreachable []string
						for _, f := range p.Findings {
							if f.Category == "unreachable-pin" {
								unreachable = append(unreachable, f.Detail)
							}
						}
						require.Len(t, unreachable, 1, "%+v", p.Findings)
						assert.Contains(t, unreachable[0], "ref "+ref+" is not a tag or branch of actions/checkout")
					}
					if locked {
						lockAfter, err := os.ReadFile(lockPath)
						require.NoError(t, err)
						assert.Equal(t, string(lockBefore), string(lockAfter))
					} else {
						assert.NoFileExists(t, lockPath)
					}
					wfAfter, err := os.ReadFile(path)
					require.NoError(t, err)
					assert.Equal(t, string(wfBefore), string(wfAfter))
				})
			}
		}
	}
}

// TestParity_RevisionSyntaxRefRefused: a `uses:` ref the Actions backend
// doesn't accept as a tag or branch name, such as `main~1` or a
// `git describe` name, may still resolve by rev-parse rules. Parity never
// builds a lookup expression from it, so nothing is locked, even when a tag
// by that name exists.
func TestParity_RevisionSyntaxRefRefused(t *testing.T) {
	for _, ref := range []string{"main~1", "v6.1.0-1-g373d742f768e"} {
		t.Run(ref, func(t *testing.T) { testRefusedRefName(t, ref) })
		t.Run(ref+"/dependabot proxy", func(t *testing.T) {
			t.Setenv("GH_ACTIONS_LOCK_DEPENDABOT_PROXY", "1")
			reg := &httpmock.Registry{}
			reg.Register(httpmock.REST("GET", `^/repos/actions/checkout$`), httpmock.JSONResponse(map[string]any{
				"full_name": "actions/checkout", "id": 1, "owner": map[string]any{"id": 1},
			}))
			// Resolution still evaluates the name; parity must refuse it.
			for range 3 {
				reg.Register(httpmock.REST("GET", `^/repos/actions/checkout/commits/`), httpmock.JSONResponse(map[string]any{"sha": parityTestSHA}))
			}
			reg.Register(httpmock.REST("GET", `^/repos/actions/checkout/contents/action\.yml$`), func(req *http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(httpmock.NodeActionYAML)), Request: req}, nil
			})
			reg.Register(httpmock.REST("GET", `^/repos/actions/checkout/git/ref/`), func(req *http.Request) (*http.Response, error) {
				t.Errorf("a refused name must not be looked up: %s", req.URL.Path)
				return httpmock.StatusResponse(http.StatusNotFound)(req)
			})
			useDefaultTransport(t, reg)
			path := writeTempWorkflow(t, `
name: ci
on: push
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@`+ref+`
`)

			_, stderr, err := runCommandWithHTTP(t, reg, "--no-narrow", "--no-interactive", path)
			require.ErrorIs(t, err, errSilent, stderr)
			assert.Contains(t, stderr, refusalDetail(ref))
			assert.NoFileExists(t, filepath.Join(".github", "workflows", "actions.lock"))
		})
	}
}

func testRefusedRefName(t *testing.T, ref string) {
	reg := &httpmock.Registry{}
	// One run for --json, one for the terminal.
	for range 2 {
		reg.Register(httpmock.ParityQuery, func(req *http.Request) (*http.Response, error) {
			var body struct {
				Variables map[string]any `json:"variables"`
			}
			require.NoError(t, json.NewDecoder(req.Body).Decode(&body))
			assert.NotContains(t, body.Variables, "tag0", "revision syntax must not reach a lookup expression")
			assert.NotContains(t, body.Variables, "head0", "revision syntax must not reach a lookup expression")
			return httpmock.JSONResponse(map[string]any{
				"data": map[string]any{"a0": map[string]any{
					"nameWithOwner": "actions/checkout",
					"databaseId":    1,
					"commit":        map[string]any{"oid": parityTestSHA},
				}},
			})(req)
		})
		reg.Register(httpmock.GraphQLForRepo("actions", "checkout"), httpmock.JSONResponse(map[string]any{
			"data": map[string]any{"a0": httpmock.RepoResponse("actions/checkout", parityTestSHA, httpmock.NodeActionYAML)},
		}))
		reg.Register(httpmock.REST("GET", `^/repos/actions/checkout$`), httpmock.JSONResponse(map[string]any{
			"full_name": "actions/checkout", "id": 1, "owner": map[string]any{"id": 1},
		}))
	}
	path := writeTempWorkflow(t, `
name: ci
on: push
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@`+ref+`
`)

	p, err := runCheckJSON(t, reg, "--no-narrow", "--no-interactive", path)
	require.ErrorIs(t, err, errSilent)
	var unreachable []string
	for _, f := range p.Findings {
		if f.Category == "unreachable-pin" {
			unreachable = append(unreachable, f.Detail)
		}
	}
	require.Len(t, unreachable, 1, "%+v", p.Findings)
	assert.Contains(t, unreachable[0], refusalDetail(ref))
	assert.NoFileExists(t, filepath.Join(".github", "workflows", "actions.lock"))

	_, stderr, err := runCommandWithHTTP(t, reg, "--no-narrow", "--no-interactive", path)
	require.ErrorIs(t, err, errSilent)
	title := "Not a tag or branch"
	if ghapi.DescribeName(ref) {
		title = "Ref name refused"
	}
	assert.Contains(t, stderr, title+" actions/checkout@"+ref, "the lockfile never recorded it")
	assert.NotContains(t, stderr, "Ref no longer exists")
}

// TestParity_RelockKeepsFreshPinFailures: --relock ignores recorded pins'
// findings because it replaces them, but a pin it just resolved is what it
// would write, so its failures still block.
func TestParity_RelockKeepsFreshPinFailures(t *testing.T) {
	reg := &httpmock.Registry{}
	// The parity stub goes first: the resolve stub matches any query for the repo.
	reg.Register(httpmock.ParityQuery, httpmock.JSONResponse(map[string]any{
		"data": map[string]any{"a0": map[string]any{
			"nameWithOwner": "actions/checkout",
			"databaseId":    1,
			"commit":        map[string]any{"oid": parityTestSHA},
			"tag":           map[string]any{"oid": parityTestMoved},
			"tagRef":        map[string]any{"oid": parityTestMoved},
		}},
	}))
	reg.Register(httpmock.GraphQLForRepo("actions", "checkout"), httpmock.JSONResponse(map[string]any{
		"data": map[string]any{"a0": httpmock.RepoResponse("actions/checkout", parityTestSHA, httpmock.NodeActionYAML)},
	}))
	reg.Register(httpmock.REST("GET", `^/repos/actions/checkout$`), httpmock.JSONResponse(map[string]any{
		"full_name": "actions/checkout", "id": 1, "owner": map[string]any{"id": 1},
	}))
	reg.Register(httpmock.REST("GET", `compare/`), httpmock.JSONResponse(map[string]any{
		"status": "diverged", "merge_base_commit": map[string]any{"sha": "cccccccccccccccccccccccccccccccccccccccc"},
	}))
	path := writeTempWorkflow(t, workflowUsing("actions/checkout@v1.0.0"))

	p, err := runCheckJSON(t, reg, "--relock", "--no-narrow", "--no-interactive", path)
	require.ErrorIs(t, err, errSilent)
	var categories []string
	for _, f := range p.Findings {
		categories = append(categories, f.Category)
	}
	assert.Contains(t, categories, "unreachable-pin")
	assert.NoFileExists(t, filepath.Join(".github", "workflows", "actions.lock"))
}

// TestParity_AcceptMovedRepairsGoneRecordedRef: the recorded tag of a
// full-SHA entry was deleted. The finding recommends --accept-moved, so
// --accept-moved must record a branch that contains the commit, and still
// refuse a commit no branch contains.
func TestParity_AcceptMovedRepairsGoneRecordedRef(t *testing.T) {
	for _, status := range []string{"behind", "diverged"} {
		t.Run(status, func(t *testing.T) { testAcceptMovedGoneRef(t, status) })
	}
}

func testAcceptMovedGoneRef(t *testing.T, status string) {
	const head = "dddddddddddddddddddddddddddddddddddddddd"
	base := parityTestSHA
	if status == "diverged" {
		base = "cccccccccccccccccccccccccccccccccccccccc"
	}
	reg := &httpmock.Registry{}
	// Every pin check finds the commit; no recorded ref resolves.
	for range 4 {
		reg.Register(httpmock.ParityQuery, func(req *http.Request) (*http.Response, error) {
			var body struct {
				Variables map[string]any `json:"variables"`
			}
			require.NoError(t, json.NewDecoder(req.Body).Decode(&body))
			data := map[string]any{}
			for k := range body.Variables {
				if i, ok := strings.CutPrefix(k, "oid"); ok {
					data["a"+i] = map[string]any{
						"nameWithOwner": "actions/checkout",
						"databaseId":    1,
						"owner":         map[string]any{"databaseId": 1},
						"commit":        map[string]any{"oid": parityTestSHA},
					}
				}
			}
			return httpmock.JSONResponse(map[string]any{"data": data})(req)
		})
	}
	for range 4 {
		reg.Register(httpmock.GraphQLForRepo("actions", "checkout"), httpmock.JSONResponse(map[string]any{
			"data": map[string]any{"a0": httpmock.RepoResponse("actions/checkout", parityTestSHA, httpmock.NodeActionYAML)},
		}))
		reg.Register(httpmock.REST("GET", `repos/actions/checkout$`),
			httpmock.JSONResponse(map[string]any{"full_name": "actions/checkout", "default_branch": "main", "id": 1, "owner": map[string]any{"id": 1}}))
		reg.Register(httpmock.REST("GET", `repos/actions/checkout/git/ref/heads/main`),
			httpmock.JSONResponse(map[string]any{"ref": "refs/heads/main", "object": map[string]any{"sha": head, "type": "commit"}}))
		reg.Register(httpmock.REST("GET", `repos/actions/checkout/tags`), httpmock.JSONResponse(httpmock.TagListResponse()))
		reg.Register(httpmock.REST("GET", `repos/actions/checkout/releases`), httpmock.JSONResponse([]map[string]any{}))
		reg.Register(httpmock.REST("GET", `repos/actions/checkout/branches`), httpmock.JSONResponse(httpmock.BranchListResponse("main", head)))
		reg.Register(httpmock.REST("GET", `repos/actions/checkout/compare/`),
			httpmock.JSONResponse(map[string]any{"status": status, "merge_base_commit": map[string]any{"sha": base}}))
	}
	httpmock.NoTagsNamed(reg, "actions", "checkout")
	path := writeFullSHAWorkflow(t, "v6.1.0")

	lockBefore := readTempLockfilePins(t)

	p, err := runCheckJSON(t, reg, "--accept-moved", "--no-interactive", path)
	lock := readTempLockfilePins(t)
	if status == "diverged" {
		require.ErrorIs(t, err, errSilent)
		assert.Equal(t, lockBefore, lock, "no branch contains the commit")
		return
	}
	require.NoError(t, err, "%+v", p.Findings)
	assert.Contains(t, lock, "ref: 'main'")
	assert.Contains(t, lock, "sha1-"+parityTestSHA)
}

// TestParity_FullSHAKeyTitleMatchesCause: a full-SHA pin's recorded ref
// that is gone is titled as a missing ref, and only a ref that exists but
// lost the commit is titled as history.
func TestParity_FullSHAKeyTitleMatchesCause(t *testing.T) {
	const live = "cccccccccccccccccccccccccccccccccccccccc"
	tests := []struct {
		name    string
		head    any
		compare bool
		want    string
		notWant string
	}{
		{"recorded ref gone", nil, false, "Ref no longer exists actions/checkout@" + parityTestSHA, "Pinned commit not in upstream history"},
		{"recorded ref lost the commit", map[string]any{"oid": live}, true, "Pinned commit not in upstream history actions/checkout@" + parityTestSHA, "Ref no longer exists"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reg := &httpmock.Registry{}
			reg.Register(httpmock.ParityQuery, httpmock.JSONResponse(map[string]any{
				"data": map[string]any{"a0": map[string]any{
					"nameWithOwner": "actions/checkout",
					"databaseId":    1,
					"commit":        map[string]any{"oid": parityTestSHA},
					"head":          tt.head,
					"headRef":       tt.head,
				}},
			}))
			if tt.compare {
				reg.Register(httpmock.REST("GET", `compare/`), httpmock.JSONResponse(map[string]any{
					"status": "diverged", "merge_base_commit": map[string]any{"sha": parityTestMoved},
				}))
			}
			path := writeFullSHAWorkflow(t, "main")

			_, stderr, err := runCommandWithHTTP(t, reg, "--verify", path)
			require.ErrorIs(t, err, errSilent)
			assert.Contains(t, stderr, tt.want)
			assert.NotContains(t, stderr, tt.notWant)
		})
	}
}

// TestParity_FullSHAKeyMovedTagWarns: a full-SHA `uses:` already draws a
// sha-as-ref warning for the same dependency; the moved recorded tag must
// still show under --verify.
func TestParity_FullSHAKeyMovedTagWarns(t *testing.T) {
	reg := &httpmock.Registry{}
	defer reg.Verify(t)
	reg.Register(httpmock.ParityQuery, httpmock.JSONResponse(map[string]any{
		"data": map[string]any{"a0": map[string]any{
			"nameWithOwner": "actions/checkout",
			"databaseId":    1,
			"commit":        map[string]any{"oid": parityTestSHA},
			"tag":           map[string]any{"oid": parityTestMoved},
			"tagRef":        map[string]any{},
		}},
	}))
	reg.Register(httpmock.REST("GET", `repos/actions/checkout/compare/`+parityTestSHA+`\.\.\.`+parityTestMoved),
		httpmock.JSONResponse(map[string]any{"status": "ahead", "merge_base_commit": map[string]any{"sha": parityTestSHA}}))
	registerNodeManifests(reg)
	path := writeFullSHAWorkflow(t, "v6.1.0")

	_, stderr, err := runCommandWithHTTP(t, reg, "--verify", path)
	require.NoError(t, err, stderr)
	assert.Contains(t, stderr, "tag v6.1.0 now points at bbbbbbbbbbbb; locked commit 9ddf35b71a48 is still in its history")
	assert.Equal(t, 1, strings.Count(stderr, "pinned to a bare SHA"), "the sha-as-ref notice still shows once")
}

// TestParity_AcceptMovedKeepsTransitiveFullSHAPinBlocking: a composite's
// full-SHA child whose recorded ref is gone isn't repaired by
// --accept-moved, which doesn't rebuild an unchanged composite's
// dependencies, so it must keep blocking rather than pass unchanged.
func TestParity_AcceptMovedKeepsTransitiveFullSHAPinBlocking(t *testing.T) {
	const parentSHA = "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
	composite := "name: c\nruns:\n  using: composite\n  steps:\n    - uses: actions/checkout@" + parityTestSHA + "\n"
	reg := &httpmock.Registry{}
	reg.RegisterReusable(httpmock.ParityQuery, func(req *http.Request) (*http.Response, error) {
		var body struct {
			Variables map[string]any `json:"variables"`
		}
		require.NoError(t, json.NewDecoder(req.Body).Decode(&body))
		data := map[string]any{}
		for k, v := range body.Variables {
			i, ok := strings.CutPrefix(k, "oid")
			if !ok {
				continue
			}
			repo := map[string]any{
				"nameWithOwner": body.Variables["owner"+i].(string) + "/" + body.Variables["name"+i].(string),
				"databaseId":    1,
				"commit":        map[string]any{"oid": v},
			}
			if v == parentSHA {
				repo["tag"], repo["tagRef"] = map[string]any{"oid": parentSHA}, map[string]any{}
			}
			data["a"+i] = repo
		}
		return httpmock.JSONResponse(map[string]any{"data": data})(req)
	})
	reg.RegisterReusable(httpmock.GraphQLForRepo("octo", "composite"), httpmock.JSONResponse(map[string]any{
		"data": map[string]any{"a0": httpmock.RepoResponse("octo/composite", parentSHA, composite)},
	}))
	reg.RegisterReusable(httpmock.GraphQLForRepo("actions", "checkout"), httpmock.JSONResponse(map[string]any{
		"data": map[string]any{"a0": httpmock.RepoResponse("actions/checkout", parityTestSHA, httpmock.NodeActionYAML)},
	}))
	path := writeTempWorkflow(t, workflowUsing("octo/composite@v1"))
	lock := "version: '" + parserlock.Version + "'\ndependencies:\n" +
		"  'octo/composite@v1':\n    ref: 'v1'\n    commit: 'sha1-" + parentSHA + "'\n    owner_id: 1\n    repo_id: 1\n" +
		"    uses:\n      - 'actions/checkout@" + parityTestSHA + "'\n" +
		"  'actions/checkout@" + parityTestSHA + "':\n    ref: 'v4-gone'\n    commit: 'sha1-" + parityTestSHA + "'\n    owner_id: 1\n    repo_id: 1\n" +
		"workflows:\n  '.github/workflows/workflow.yml':\n    - 'octo/composite@v1'\n"
	lockPath := filepath.Join(".github", "workflows", "actions.lock")
	require.NoError(t, os.WriteFile(lockPath, []byte(lock), 0o600))

	_, stderr, err := runCommandWithHTTP(t, reg, "--accept-moved", "--no-narrow", "--no-interactive", path)
	require.ErrorIs(t, err, errSilent, stderr)
	assert.Contains(t, stderr, "ref v4-gone is not a tag or branch of actions/checkout")
	assert.Contains(t, stderr, "upgrade or replace octo/composite@v1 with a version whose pin of actions/checkout can be verified, or stop using it")
	assert.NotContains(t, stderr, "--accept-moved", "an override that can't repair it is never suggested")
	after, rerr := os.ReadFile(lockPath)
	require.NoError(t, rerr)
	assert.Equal(t, lock, string(after), "nothing is written")
}

// refusalDetail is the finding detail for a `uses:` ref parity refuses.
func refusalDetail(ref string) string {
	if ghapi.DescribeName(ref) {
		return "ref " + ref + " looks like `git describe` output"
	}
	return "ref " + ref + " is not a tag or branch of actions/checkout"
}

// writeFullSHAWorkflow writes a workflow using actions/checkout at
// parityTestSHA, locked to that commit with the given recorded ref.
func writeFullSHAWorkflow(t *testing.T, ref string) string {
	t.Helper()
	path := writeTempWorkflow(t, `
name: ci
on: push
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@`+parityTestSHA+`
`, "actions/checkout@"+parityTestSHA+"=sha1-"+parityTestSHA)
	lockPath := filepath.Join(".github", "workflows", "actions.lock")
	lock := readTempLockfilePins(t)
	lock = strings.Replace(lock, "ref: '"+parityTestSHA+"'", "ref: '"+ref+"'", 1)
	require.NoError(t, os.WriteFile(lockPath, []byte(lock), 0o600))
	return path
}

// assertFullSHARun runs one mode and checks the verdict. A blocked run must
// leave both files untouched: no rewrite of `uses:` to the recorded ref.
func assertFullSHARun(t *testing.T, reg *httpmock.Registry, args []string, path, want, detail string) {
	t.Helper()
	lockBefore := readTempLockfilePins(t)
	wfBefore, err := os.ReadFile(path)
	require.NoError(t, err)
	stdout, stderr, err := runCommandWithHTTP(t, reg, append(args, "--no-interactive", "--json=valid,findings", path)...)
	var p checkResult
	require.NoError(t, json.Unmarshal([]byte(stdout), &p), stdout+stderr)
	var blocking []format.Finding
	for _, f := range p.Findings {
		if f.Severity == "error" {
			blocking = append(blocking, f)
		}
	}
	if want == "" {
		require.NoError(t, err, stdout+stderr)
		assert.True(t, p.Valid)
		assert.Empty(t, blocking)
		return
	}
	require.ErrorIs(t, err, errSilent, stderr)
	assert.False(t, p.Valid)
	require.Len(t, blocking, 1, "findings: %+v", p.Findings)
	assert.Equal(t, want, blocking[0].Category)
	assert.Contains(t, blocking[0].Detail, detail)
	wfAfter, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, string(wfBefore), string(wfAfter), "a blocked pin must not be rewritten")
	assert.Contains(t, string(wfAfter), "actions/checkout@"+parityTestSHA)
	assert.Equal(t, lockBefore, readTempLockfilePins(t), "a blocked pin must not reach the lockfile")
}

const tenantSHA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

const publicSHA = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

func tenantHostFixture(t *testing.T, publicStatus int) http.RoundTripper {
	t.Helper()
	return httpmock.RoundTripFunc(func(req *http.Request) (*http.Response, error) {
		tenant := req.URL.Host == "api.tenant.ghe.com"
		if tenant {
			assert.NotEmpty(t, req.Header.Get("Authorization"))
		} else {
			assert.Equal(t, "api.github.com", req.URL.Host)
			assert.Empty(t, req.Header.Get("Authorization"))
			assert.Empty(t, req.Header.Get("Cookie"))
		}
		path := req.URL.Path
		switch {
		case tenant && path == "/repos/tenant/internal":
			return httpmock.JSONResponse(map[string]any{
				"visibility": "internal", "id": 111, "owner": map[string]any{"id": 11},
			})(req)
		case path == "/repos/actions/public":
			if tenant {
				return httpmock.StatusResponse(http.StatusNotFound)(req)
			}
			if publicStatus != 200 {
				return httpmock.StatusResponse(publicStatus)(req)
			}
			return httpmock.JSONResponse(map[string]any{
				"visibility": "public", "id": 222, "owner": map[string]any{"id": 22},
			})(req)
		case tenant && path == "/graphql":
			var body struct {
				Variables map[string]string `json:"variables"`
			}
			if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
				return nil, err
			}
			assert.Equal(t, "internal", body.Variables["name0"])
			composite := "runs:\n  using: composite\n  steps:\n    - uses: actions/public@v2\n"
			return httpmock.JSONResponse(map[string]any{"data": map[string]any{
				"a0": httpmock.RepoResponse("tenant/internal", tenantSHA, composite),
			}})(req)
		case !tenant && (path == "/repos/actions/public/commits/v2" || path == "/repos/actions/public/commits/"+publicSHA):
			return httpmock.JSONResponse(map[string]any{"sha": publicSHA})(req)
		case !tenant && path == "/repos/actions/public/git/ref/tags/v2":
			return httpmock.JSONResponse(httpmock.GitRefResponse("refs/tags/v2", publicSHA))(req)
		case !tenant && path == "/repos/actions/public/contents/action.yml":
			assert.Equal(t, publicSHA, req.URL.Query().Get("ref"))
			return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(httpmock.NodeActionYAML)), Request: req}, nil
		case strings.HasSuffix(path, "/releases"):
			return httpmock.JSONResponse([]any{})(req)
		}
		return nil, fmt.Errorf("unexpected request: %s %s", req.Method, req.URL)
	})
}

func TestTenantHostGenerationPreservesHosts(t *testing.T) {
	path := writeTempWorkflow(t, workflowUsing("tenant/internal@v1", "actions/public@v2"))
	args := []string{"--hostname", "tenant.ghe.com", "--no-interactive", "--no-narrow", "--no-migrate-local-actions", "--json", path}
	_, _, err := runCommandWithHTTP(t, tenantHostFixture(t, 200), args...)
	require.NoError(t, err)
	raw := readTempLockfilePins(t)
	file, err := parserlock.Parse([]byte(raw))
	require.NoError(t, err)
	assert.Equal(t, "v0.0.3", file.Version)
	require.Len(t, file.Dependencies, 2)
	local := file.Dependencies["tenant/internal@v1"]
	assert.Empty(t, local.Hostname)
	assert.Equal(t, "sha1-"+tenantSHA, local.Commit)
	assert.EqualValues(t, 11, local.OwnerID)
	assert.EqualValues(t, 111, local.RepoID)
	assert.Equal(t, []string{"actions/public@v2"}, local.Uses)
	public := file.Dependencies["actions/public@v2"]
	assert.Equal(t, "github.com", public.Hostname)
	assert.Equal(t, "sha1-"+publicSHA, public.Commit)
	assert.EqualValues(t, 22, public.OwnerID)
	assert.EqualValues(t, 222, public.RepoID)

	_, _, err = runCommandWithHTTP(t, tenantHostFixture(t, 200), args...)
	require.NoError(t, err)
	assert.Equal(t, raw, readTempLockfilePins(t), "repeat generation must retain hosts and IDs")

	base := tenantHostFixture(t, 200)
	pinnedTransport := httpmock.RoundTripFunc(func(req *http.Request) (*http.Response, error) {
		assert.False(t, req.URL.Host == "api.tenant.ghe.com" && req.URL.Path == "/repos/actions/public",
			"recorded dotcom pins must bypass tenant namesakes")
		return base.RoundTrip(req)
	})
	_, _, err = runCommandWithHTTP(t, pinnedTransport,
		"--hostname", "tenant.ghe.com", "--verify", "--json", path)
	require.NoError(t, err)
	assert.Equal(t, raw, readTempLockfilePins(t))

	// Convert the previous preview's explicit home host without moving its pin.
	explicit := strings.Replace(raw, "'tenant/internal@v1':\n", "'tenant/internal@v1':\n        hostname: 'tenant.ghe.com'\n", 1)
	require.NotEqual(t, raw, explicit)
	require.NoError(t, os.WriteFile(parserlock.Path, []byte(explicit), 0o600))
	_, _, err = runCommandWithHTTP(t, tenantHostFixture(t, 200), args...)
	require.NoError(t, err)
	assert.Equal(t, raw, readTempLockfilePins(t))
}

func TestTenantHostOmittedPinsCannotMoveToDotcomOrTenantNamesakes(t *testing.T) {
	for _, status := range []int{200, 401, 403, 404, 503} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			path := writeTempWorkflow(t, workflowUsing("actions/public@v2"), "actions/public@v2=sha1-"+publicSHA)
			raw := readTempLockfilePins(t)
			raw = strings.ReplaceAll(raw, "owner_id: 1", "owner_id: 22")
			raw = strings.ReplaceAll(raw, "repo_id: 1", "repo_id: 222")
			require.NoError(t, os.WriteFile(parserlock.Path, []byte(raw), 0o600))
			transport := httpmock.RoundTripFunc(func(req *http.Request) (*http.Response, error) {
				assert.Equal(t, "api.tenant.ghe.com", req.URL.Host, "omitted pins must not fall back to dotcom")
				assert.NotEmpty(t, req.Header.Get("Authorization"))
				if req.URL.Path == "/graphql" {
					return parityRepoID(req, status, 333)
				}
				assert.Equal(t, "/repos/actions/public", req.URL.Path)
				if status != 200 {
					return httpmock.StatusResponse(status)(req)
				}
				return httpmock.JSONResponse(map[string]any{
					"visibility": "internal", "id": 333, "owner": map[string]any{"id": 33},
				})(req)
			})
			stdout, _, err := runCommandWithHTTP(t, transport,
				"--hostname", "tenant.ghe.com", "--no-narrow", "--no-migrate-local-actions", "--json", path)
			switch status {
			case 200:
				require.ErrorIs(t, err, errSilent)
				assert.Contains(t, stdout, "repo-replaced")
			case 404:
				require.ErrorIs(t, err, errSilent)
				assert.Contains(t, stdout, "repo-unavailable")
			default:
				require.Error(t, err)
				assert.NotErrorIs(t, err, errSilent, "inconclusive identity must exit 2")
				assert.ErrorContains(t, err, fmt.Sprintf("HTTP %d", status))
			}
			assert.Equal(t, raw, readTempLockfilePins(t))
		})
	}
}

// parityRepoID answers a parity query as a pin whose repository now has
// repoID, or as a missing repository when status is 404.
func parityRepoID(req *http.Request, status int, repoID int) (*http.Response, error) {
	var body struct {
		Variables map[string]any `json:"variables"`
	}
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
		return nil, err
	}
	if status == http.StatusNotFound {
		return httpmock.JSONResponse(map[string]any{"data": map[string]any{"a0": nil}})(req)
	}
	oid := map[string]any{"oid": body.Variables["oid0"]}
	return httpmock.JSONResponse(map[string]any{"data": map[string]any{"a0": map[string]any{
		"nameWithOwner": fmt.Sprintf("%s/%s", body.Variables["owner0"], body.Variables["name0"]),
		"databaseId":    repoID,
		"commit":        oid,
		"tag":           oid,
		"tagRef":        oid,
	}}})(req)
}

func TestTenantHostIncompleteGenerationDoesNotWrite(t *testing.T) {
	for _, status := range []int{404, 403, 429, 500} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			path := writeTempWorkflow(t, workflowUsing("tenant/internal@v1"))
			before, err := os.ReadFile(path)
			require.NoError(t, err)
			stdout, _, err := runCommandWithHTTP(t, tenantHostFixture(t, status),
				"--hostname", "tenant.ghe.com", "--no-narrow", "--no-migrate-local-actions", "--json", path)
			require.ErrorIs(t, err, errSilent)
			var result struct {
				Valid bool `json:"valid"`
			}
			require.NoError(t, json.Unmarshal([]byte(stdout), &result))
			assert.False(t, result.Valid)
			_, err = os.Stat(parserlock.Path)
			assert.True(t, os.IsNotExist(err))
			after, err := os.ReadFile(path)
			require.NoError(t, err)
			assert.Equal(t, before, after)
		})
	}
}

func TestTenantHostLegacyPinsRequireDotcomIdentity(t *testing.T) {
	for _, matches := range []bool{true, false} {
		t.Run(fmt.Sprintf("matching IDs=%v", matches), func(t *testing.T) {
			path := writeTempWorkflow(t, workflowUsing("actions/public@v2"), "actions/public@v2=sha1-"+publicSHA)
			raw := strings.ReplaceAll(readTempLockfilePins(t), "v0.0.3", "v0.0.2")
			if matches {
				raw = strings.ReplaceAll(raw, "owner_id: 1", "owner_id: 22")
				raw = strings.ReplaceAll(raw, "repo_id: 1", "repo_id: 222")
			}
			require.NoError(t, os.WriteFile(parserlock.Path, []byte(raw), 0o600))
			stdout, _, err := runCommandWithHTTP(t, tenantHostFixture(t, 200),
				"--hostname", "tenant.ghe.com", "--no-narrow", "--no-migrate-local-actions", "--json", path)
			if matches {
				require.NoError(t, err)
				assert.Contains(t, readTempLockfilePins(t), "hostname: 'github.com'")
			} else {
				require.ErrorIs(t, err, errSilent)
				assert.Contains(t, stdout, "repo-replaced")
				assert.Equal(t, raw, readTempLockfilePins(t))
			}
		})
	}
}

// goldenFixtureDir is the source-of-truth fixture for the JSON golden test.
// The .github/workflows/ subtree is copied verbatim into a tempdir before
// each run so the test doesn't mutate state under testdata/.
const goldenFixtureDir = "testdata/golden-json"

func TestCheckCommand_JSONGolden(t *testing.T) {
	reg := &httpmock.Registry{}
	defer reg.Verify(t)

	const (
		checkoutSHA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		setupGoSHA  = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
		cacheSHA    = "cccccccccccccccccccccccccccccccccccccccc"
		helperSHA   = "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
	)

	// setup-go is a composite action that fans out to actions/cache AND
	// a helper-only dep. The helper appears only in the transitive
	// closure (no direct workflow ref), so the formatter populates its
	// required_by[] from the resolver's parent map — exactly the
	// transitive-coverage signal we want the golden to lock down.
	compositeYAML := "name: Setup Go\n" +
		"runs:\n" +
		"  using: composite\n" +
		"  steps:\n" +
		"    - uses: actions/cache@v4\n" +
		"    - uses: helper/only-transitive@v1\n"

	// Direct refs from the workflow: checkout@v6, setup-go@v6, cache@v3.
	// The resolver batches them into a single GraphQL request keyed by
	// owner/name pairs (a0/a1/a2).
	reg.Register(
		httpmock.GraphQLForRepo("actions", "checkout"),
		httpmock.JSONResponse(map[string]any{
			"data": map[string]any{
				"a0": httpmock.RepoResponse("actions/checkout", checkoutSHA, httpmock.NodeActionYAML),
				"a1": httpmock.RepoResponse("actions/setup-go", setupGoSHA, compositeYAML),
				"a2": httpmock.RepoResponse("actions/cache", cacheSHA, httpmock.NodeActionYAML),
			},
		}),
	)

	// Transitive batch discovered from the setup-go composite: cache@v4
	// (same NWO as a direct workflow ref) and helper/only-transitive@v1
	// (transitive-only, gives us a populated required_by[] in the JSON).
	reg.Register(
		httpmock.GraphQLForRepo("actions", "cache"),
		httpmock.JSONResponse(map[string]any{
			"data": map[string]any{
				"a0": httpmock.RepoResponse("actions/cache", cacheSHA, httpmock.NodeActionYAML),
				"a1": httpmock.RepoResponse("helper/only-transitive", helperSHA, httpmock.NodeActionYAML),
			},
		}),
	)

	// Resolve the fixture path against the package directory BEFORE
	// chdir'ing into the tempdir — UPDATE_GOLDEN rewrites the source
	// expected.json, not a copy under the tempdir.
	srcDir, err := filepath.Abs(goldenFixtureDir)
	require.NoError(t, err)
	dir := t.TempDir()
	copyFixtureTree(t, srcDir, dir)
	t.Chdir(dir)

	stdout, _, err := runCommandWithHTTP(t, reg,
		// The fixture's lockfile addresses the workflow as
		// .github/workflows/ci.yml, so we run check on that exact path.
		"--relock", "--no-fix", "--json=valid,findings,workflows,dependencies",
		".github/workflows/ci.yml",
	)
	// We expect findings (ref-changed + stale), so the command exits
	// non-zero. JSON output is still on stdout per the contract.
	require.ErrorIs(t, err, errSilent, "JSON mode should exit non-zero when findings are present")

	got := normalizeGoldenJSON(t, []byte(stdout))

	goldenPath := filepath.Join(srcDir, "expected.json")
	if os.Getenv("UPDATE_GOLDEN") == "1" {
		writeGolden(t, goldenPath, got)
		return
	}

	wantRaw, err := os.ReadFile(goldenPath)
	require.NoError(t, err, "read golden %s — run with UPDATE_GOLDEN=1 to create it", goldenPath)
	want := normalizeGoldenJSON(t, wantRaw)

	diffs := diffAdditive("", want, got)
	if len(diffs) == 0 {
		return
	}
	t.Fatalf("JSON contract drift detected (re-run with UPDATE_GOLDEN=1 only if the change is intentional and additive):\n  %s",
		strings.Join(diffs, "\n  "))
}

// normalizeGoldenJSON parses the raw JSON, masks volatile fields, and
// canonicalizes array ordering so the golden compares deterministically
// across runs and machines.
func normalizeGoldenJSON(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var obj map[string]any
	require.NoError(t, json.Unmarshal(raw, &obj), "parse JSON")

	// Mask the cli_version: it varies by build mode ("(devel)" for `go
	// test`, a real semver tag for installed extension). Lockfile schema
	// version is deterministic (v0.0.1 today) but mask it too so a future
	// schema bump can ship additive fields without rewriting the golden
	// for the version string.
	if _, ok := obj["cli_version"]; ok {
		obj["cli_version"] = "<MASKED>"
	}
	if _, ok := obj["lockfile_version"]; ok {
		obj["lockfile_version"] = "<MASKED>"
	}

	// Sort arrays whose element order isn't part of the contract. The
	// JSON formatter is mostly deterministic today, but lockfile-derived
	// maps can shuffle if the engine changes iteration order.
	if findings, ok := obj["findings"].([]any); ok {
		obj["findings"] = sortByKey(findings, findingSortKey)
	}
	if deps, ok := obj["dependencies"].([]any); ok {
		obj["dependencies"] = sortByKey(deps, depSortKey)
	}
	if wfs, ok := obj["workflows"].([]any); ok {
		obj["workflows"] = sortByKey(wfs, workflowSortKey)
		// Sort nested arrays inside each workflow.
		for _, w := range obj["workflows"].([]any) {
			wm, ok := w.(map[string]any)
			if !ok {
				continue
			}
			if fs, ok := wm["findings"].([]any); ok {
				wm["findings"] = sortByKey(fs, findingSortKey)
			}
			if ds, ok := wm["dependencies"].([]any); ok {
				wm["dependencies"] = sortByKey(ds, depSortKey)
			}
		}
	}
	return obj
}

// findingSortKey returns a stable sort key for a Finding object.
// Category + workflow + detail picks a unique row in our fixture.
func findingSortKey(v any) string {
	m, _ := v.(map[string]any)
	return fmt.Sprintf("%v|%v|%v|%v", m["category"], m["workflow"], m["dependency"], m["detail"])
}

// depSortKey returns a stable sort key for a Dependency object.
func depSortKey(v any) string {
	m, _ := v.(map[string]any)
	return fmt.Sprintf("%v|%v|%v", m["nwo"], m["ref"], m["sha"])
}

// workflowSortKey returns a stable sort key for a Workflow object.
func workflowSortKey(v any) string {
	m, _ := v.(map[string]any)
	s, _ := m["path"].(string)
	return s
}

func sortByKey(arr []any, keyFn func(any) string) []any {
	out := make([]any, len(arr))
	copy(out, arr)
	sort.SliceStable(out, func(i, j int) bool {
		return keyFn(out[i]) < keyFn(out[j])
	})
	return out
}

// diffAdditive recursively compares want to got and returns a list of
// human-readable diffs. The comparison is structural-additive: every key
// in want must exist in got with the same (recursively-compared) value,
// but extra keys in got are allowed (new optional fields). Array element
// counts must match — adding or removing elements is a shape change.
func diffAdditive(path string, want, got any) []string {
	switch w := want.(type) {
	case map[string]any:
		g, ok := got.(map[string]any)
		if !ok {
			return []string{fmt.Sprintf("type mismatch at %s: want object, got %T", path, got)}
		}
		var diffs []string
		// Deterministic key order so failure messages are stable.
		keys := make([]string, 0, len(w))
		for k := range w {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			child := joinPath(path, k)
			wv := w[k]
			gv, present := g[k]
			if !present {
				diffs = append(diffs, fmt.Sprintf("missing field at %s (breaking removal/rename — additive changes must keep prior fields)", child))
				continue
			}
			diffs = append(diffs, diffAdditive(child, wv, gv)...)
		}
		return diffs
	case []any:
		g, ok := got.([]any)
		if !ok {
			return []string{fmt.Sprintf("type mismatch at %s: want array, got %T", path, got)}
		}
		if len(w) != len(g) {
			return []string{fmt.Sprintf("array length changed at %s: want %d elements, got %d", path, len(w), len(g))}
		}
		var diffs []string
		for i := range w {
			diffs = append(diffs, diffAdditive(fmt.Sprintf("%s[%d]", path, i), w[i], g[i])...)
		}
		return diffs
	default:
		if !equalScalar(want, got) {
			return []string{fmt.Sprintf("value changed at %s: want %v (%T), got %v (%T)", path, want, want, got, got)}
		}
		return nil
	}
}

func equalScalar(a, b any) bool {
	// JSON numbers all decode to float64, so equality is direct.
	return fmt.Sprintf("%v", a) == fmt.Sprintf("%v", b)
}

func joinPath(parent, child string) string {
	if parent == "" {
		return child
	}
	return parent + "." + child
}

// writeGolden serializes the masked, sorted payload back to disk in pretty
// form. UPDATE_GOLDEN=1 is the only caller.
func writeGolden(t *testing.T, path string, obj map[string]any) {
	t.Helper()
	buf, err := json.MarshalIndent(obj, "", "  ")
	require.NoError(t, err)
	buf = append(buf, '\n')
	require.NoError(t, os.WriteFile(path, buf, 0o600))
	t.Logf("wrote golden %s (%d bytes)", path, len(buf))
}

// copyFixtureTree mirrors the .github/workflows/ subtree from srcDir to
// dstDir. The fixture is text-only YAML/JSON, so we don't preserve modes
// beyond 0o600.
func copyFixtureTree(t *testing.T, srcDir, dstDir string) {
	t.Helper()
	walkRoot := filepath.Join(srcDir, ".github")
	require.NoError(t, filepath.Walk(walkRoot, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(srcDir, p)
		if err != nil {
			return err
		}
		target := filepath.Join(dstDir, rel)
		if info.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o600)
	}), "copy fixture tree")
}

// silence unused-import linter if our pipeline gets stricter; http is
// referenced only transitively today.
var _ http.RoundTripper = (*httpmock.Registry)(nil)
