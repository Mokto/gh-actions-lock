package main

import (
	"bytes"
	"cmp"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	parserlock "github.com/github/actions-lockfile/go/pkg/lockfile"
	"github.com/github/gh-actions-lock/cmd/gh-actions-lock/format"
	"github.com/github/gh-actions-lock/internal/ghapi/httpmock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

func TestLocalActionsAreAllowedWithoutFollowingOrRewriting(t *testing.T) {
	const workflow = `name: ci
on: push
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: ./.github/actions/local
`
	for _, fixture := range []struct {
		name, content string
	}{
		{name: "missing action"},
		{name: "malformed action", content: "runs: ["},
		{name: "unresolvable nested dependency", content: "runs:\n  using: composite\n  steps:\n    - uses: nonexistent/unreachable@missing\n"},
		{name: "nested workspace and invalid self refs", content: "runs:\n  using: composite\n  steps:\n    - uses: ./cycle\n    - uses: $/bad@v1\n"},
	} {
		for _, mode := range []struct {
			name string
			args []string
		}{
			{name: "lock"},
			{name: "sticky compatibility flag", args: []string{"--no-migrate-local-actions"}},
			{name: "relock", args: []string{"--relock"}},
			{name: "accept moved", args: []string{"--accept-moved"}},
			{name: "read only", args: []string{"--no-fix"}},
			{name: "no onboarding", args: []string{"--no-onboard"}},
			{name: "verify", args: []string{"--verify"}},
			{name: "verify local", args: []string{"--verify-local"}},
		} {
			t.Run(fixture.name+"/"+mode.name, func(t *testing.T) {
				wf := writeTempWorkflow(t, workflow)
				writeTempLockfile(t, ".", filepath.Base(wf), nil)
				require.NoError(t, os.Mkdir(".git", 0o755))
				if fixture.content != "" {
					require.NoError(t, os.MkdirAll(".github/actions/local", 0o755))
					require.NoError(t, os.WriteFile(".github/actions/local/action.yml", []byte(fixture.content), 0o600))
				}
				before := snapshotWorkflows(t)
				transport := &requestCountingTransport{}
				args := append([]string{"--json=valid,findings"}, mode.args...)
				stdout, _, err := runCommandWithHTTP(t, transport, append(args, wf)...)
				require.NoError(t, err)
				assert.Zero(t, transport.calls.Load(), "nothing behind ./ may be resolved")
				var payload struct {
					Valid    bool             `json:"valid"`
					Findings []format.Finding `json:"findings"`
				}
				require.NoError(t, json.Unmarshal([]byte(stdout), &payload))
				assert.True(t, payload.Valid)
				require.Len(t, payload.Findings, 1)
				assert.Equal(t, "local-action", payload.Findings[0].Category)
				assert.Equal(t, "info", payload.Findings[0].Severity)
				assert.Contains(t, payload.Findings[0].Detail, "never followed or locked")
				assert.Contains(t, payload.Findings[0].Detail, "uses: dependencies are not covered")
				_, stderr, err := runCommandWithHTTP(t, transport, append(mode.args, wf)...)
				require.NoError(t, err)
				assert.Contains(t, stderr, "never followed or locked")
				assert.Contains(t, stderr, "uses: dependencies are not covered")
				assert.NotContains(t, stderr, "complete lockfile coverage")
				assert.Zero(t, transport.calls.Load())
				after := snapshotWorkflows(t)
				if mode.name == "read only" || mode.name == "verify" || mode.name == "verify local" {
					assert.Equal(t, before, after, "read-only modes must not write any file")
				}
				delete(before, ".github/workflows/actions.lock")
				delete(after, ".github/workflows/actions.lock")
				assert.Equal(t, before, after, "workflow and action files must remain byte-identical")
			})
		}
	}
}

func TestLocalActionsCanBeOnboardedWithNoLocalManifest(t *testing.T) {
	wf := writeTempWorkflow(t, "jobs:\n  test:\n    runs-on: ubuntu-latest\n    steps:\n      - uses: ./absent\n")
	transport := &requestCountingTransport{}
	_, stderr, err := runCommandWithHTTP(t, transport, wf)
	require.NoError(t, err)
	assert.Contains(t, stderr, "never followed or locked")
	assert.Contains(t, readTempLockfilePins(t), "'"+wf+"': []")
	assert.Zero(t, transport.calls.Load())
}

func TestLocalActionDoesNotHideUnpinnedRemoteAction(t *testing.T) {
	wf := writeTempWorkflow(t, "jobs:\n  test:\n    runs-on: ubuntu-latest\n    steps:\n      - uses: ./absent\n      - uses: owner/action@v1\n")
	writeTempLockfile(t, ".", filepath.Base(wf), nil)
	stdout, _, err := runCommandWithHTTP(t, &httpmock.Registry{}, "--verify-local", "--json=valid,findings", wf)
	require.ErrorIs(t, err, errSilent)
	assert.Contains(t, stdout, `"valid": false`)
	assert.Contains(t, stdout, `"category": "local-action"`)
	assert.Contains(t, stdout, `"category": "not-pinned"`)
	assert.Contains(t, stdout, "owner/action@v1")
}

func TestOpaqueLocalActionDoesNotHideSelfRepositoryRemotePins(t *testing.T) {
	for _, mode := range []struct {
		name string
		args []string
	}{
		{name: "initial lock"},
		{name: "compatibility flag", args: []string{"--no-migrate-local-actions"}},
		{name: "relock", args: []string{"--relock"}},
	} {
		t.Run(mode.name, func(t *testing.T) {
			wf := writeTempWorkflow(t, "jobs:\n  test:\n    runs-on: ubuntu-latest\n    steps:\n      - uses: $/.github/actions/outer\n      - uses: ./absent\n")
			require.NoError(t, os.Mkdir(".git", 0o755))
			require.NoError(t, os.MkdirAll(".github/actions/outer", 0o755))
			require.NoError(t, os.MkdirAll(".github/actions/opaque", 0o755))
			require.NoError(t, os.WriteFile(".github/actions/outer/action.yml", []byte("runs:\n  using: composite\n  steps:\n    - uses: actions/setup-go@v5.0.0\n    - uses: ./.github/actions/opaque\n"), 0o600))
			require.NoError(t, os.WriteFile(".github/actions/opaque/action.yml", []byte("runs: ["), 0o600))
			before := snapshotWorkflows(t)
			reg := &httpmock.Registry{}
			registerSetupGo(reg)

			_, stderr, err := runCommandWithHTTP(t, reg, append(mode.args, wf)...)
			require.NoError(t, err, stderr)
			assert.Contains(t, stderr, "never followed or locked")
			file, err := parserlock.Parse([]byte(readTempLockfilePins(t)))
			require.NoError(t, err)
			assert.Equal(t, []string{"actions/setup-go@v5.0.0"}, file.Workflows[wf])
			require.Len(t, file.Dependencies, 1)
			assert.Equal(t, "sha1-"+parityTestMoved, file.Dependencies["actions/setup-go@v5.0.0"].Commit)
			after := snapshotWorkflows(t)
			delete(after, parserlock.Path)
			assert.Equal(t, before, after)

			reg = &httpmock.Registry{}
			registerParity(reg)
			registerSetupGo(reg)
			beforeVerify := snapshotWorkflows(t)
			stdout, _, err := runCommandWithHTTP(t, reg, "--verify", "--json=valid,findings", wf)
			require.NoError(t, err, stdout)
			var payload struct {
				Valid    bool             `json:"valid"`
				Findings []format.Finding `json:"findings"`
			}
			require.NoError(t, json.Unmarshal([]byte(stdout), &payload))
			assert.True(t, payload.Valid)
			require.Len(t, payload.Findings, 1)
			assert.Equal(t, "local-action", payload.Findings[0].Category)
			assert.Equal(t, "info", payload.Findings[0].Severity)
			assert.Contains(t, payload.Findings[0].Detail, "./.github/actions/opaque")
			assert.Equal(t, beforeVerify, snapshotWorkflows(t))
		})
	}
}

func TestRemoteCompositeLocalActionsRemainVisibleAcrossOnlineModes(t *testing.T) {
	for _, mode := range []struct {
		name string
		args []string
	}{
		{name: "initial lock"},
		{name: "sticky"},
		{name: "verify", args: []string{"--verify"}},
		{name: "read only", args: []string{"--no-fix"}},
		{name: "no onboarding", args: []string{"--no-onboard"}},
		{name: "relock", args: []string{"--relock"}},
		{name: "accept moved", args: []string{"--accept-moved"}},
	} {
		t.Run(mode.name, func(t *testing.T) {
			wf := writeTempWorkflow(t, cleanWorkflow)
			reg := &httpmock.Registry{}
			registerSetupGoManifest(reg, "runs:\n  using: composite\n  steps:\n    - uses: ./workspace-helper\n")
			if mode.name != "initial lock" {
				stdout, stderr, err := runCommandWithHTTP(t, reg, "--json=valid,findings,pins", wf)
				require.NoError(t, err, stdout, stderr)
				reg = &httpmock.Registry{}
				registerParity(reg)
				registerSetupGoManifest(reg, "runs:\n  using: composite\n  steps:\n    - uses: ./workspace-helper\n")
			}
			args := append([]string{"--json=valid,findings,pins"}, mode.args...)
			stdout, _, err := runCommandWithHTTP(t, reg, append(args, wf)...)
			require.NoError(t, err, stdout)
			var payload struct {
				Valid    bool             `json:"valid"`
				Findings []format.Finding `json:"findings"`
			}
			require.NoError(t, json.Unmarshal([]byte(stdout), &payload))
			if mode.name == "initial lock" {
				assert.False(t, payload.Valid, "JSON retains pre-fix findings")
				require.Len(t, payload.Findings, 2)
			} else {
				assert.True(t, payload.Valid, stdout)
				require.Len(t, payload.Findings, 1)
			}
			assert.Equal(t, "local-action", payload.Findings[0].Category)
			assert.Equal(t, "info", payload.Findings[0].Severity)
			assert.Contains(t, payload.Findings[0].Detail, "actions/setup-go@v5.0.0")
			assert.Contains(t, payload.Findings[0].Detail, "./workspace-helper")
			assert.Contains(t, payload.Findings[0].Detail, "uses: dependencies are not covered")
		})
	}
}

func TestLockedLocalActionNoticeSurvivesNewRemoteRoot(t *testing.T) {
	for _, mode := range []struct {
		name string
		args []string
	}{
		{name: "fix"},
		{name: "read only", args: []string{"--no-fix"}},
		{name: "verify", args: []string{"--verify"}},
	} {
		for _, jsonOutput := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/json=%t", mode.name, jsonOutput), func(t *testing.T) {
				const manifest = "runs:\n  using: composite\n  steps:\n    - uses: ./workspace-helper\n"
				path := writeTempWorkflow(t, cleanWorkflow)
				reg := &httpmock.Registry{}
				registerSetupGoManifest(reg, manifest)
				_, _, err := runCommandWithHTTP(t, reg, path)
				require.NoError(t, err)
				require.NoError(t, os.WriteFile(path, []byte(cleanWorkflow+"      - uses: actions/checkout@v4.2.1\n"), 0o600))
				reg = &httpmock.Registry{}
				registerParity(reg)
				registerSetupGoManifest(reg, manifest)
				reg.Register(httpmock.GraphQLForRepo("actions", "checkout"), httpmock.JSONResponse(map[string]any{
					"data": map[string]any{"a0": testRepoResponse("actions/checkout", parityTestSHA, nodeActionYAML)},
				}))
				reg.Register(httpmock.REST("GET", `repos/actions/checkout$`), httpmock.JSONResponse(map[string]any{
					"default_branch": "main", "id": 1, "owner": map[string]any{"id": 1},
				}))
				args := append([]string{"--no-narrow"}, mode.args...)
				if jsonOutput {
					args = append(args, "--json=valid,findings")
				}
				stdout, stderr, err := runCommandWithHTTP(t, reg, append(args, path)...)
				if mode.name == "fix" {
					require.NoError(t, err, stderr)
				} else {
					require.ErrorIs(t, err, errSilent)
				}
				if !jsonOutput {
					assert.Contains(t, stderr, "./workspace-helper")
					assert.Contains(t, stderr, "uses: dependencies are not covered")
					return
				}
				var payload struct {
					Valid    bool             `json:"valid"`
					Findings []format.Finding `json:"findings"`
				}
				require.NoError(t, json.Unmarshal([]byte(stdout), &payload))
				assert.False(t, payload.Valid, "the new root retains its pre-fix not-pinned finding")
				var notice bool
				for _, f := range payload.Findings {
					if f.Category == "local-action" && strings.Contains(f.Detail, "./workspace-helper") {
						assert.Equal(t, "info", f.Severity)
						notice = true
					}
				}
				assert.True(t, notice, "locked manifests must disclose opaque dependencies beside an unrecorded root")
			})
		}
	}
}

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
	goneRef   string
	manifests map[string]string
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
			case "fresh":
				yml = "runs:\n  using: composite\n  steps:\n    - uses: acme/child@v2\n"
			case "old", "new":
				data[fmt.Sprintf("a%d", i)] = testRepoResponse("acme/new", graphLeafSHA, nodeActionYAML)
				continue
			case "leaf":
				if g.leafUsesParent {
					yml = "runs:\n  using: composite\n  steps:\n    - uses: acme/parent@v1.0.0\n"
				}
			case "other", "actions", "tools", "eslint", "jest":
			default:
				return nil, fmt.Errorf("unexpected fixture repository %s", name)
			}
			path := strings.TrimSuffix(body.Variables[fmt.Sprintf("yml%d", i)], "/action.yml")
			if path == "action.yml" {
				path = ""
			}
			fullName := "acme/" + name
			if path != "" {
				fullName += "/" + path
			}
			if manifest, ok := g.manifests[fullName+"@"+ref]; ok {
				yml = manifest
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
		if g.movedParent && req.URL.Path == "/repos/acme/parent/compare/"+graphParentSHA+"..."+graphLeafSHA {
			return httpmock.JSONResponse(map[string]any{
				"status": "ahead", "ahead_by": 1, "merge_base_commit": map[string]any{"sha": graphParentSHA},
			})(req)
		}
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
		case "/repos/acme/parent", "/repos/acme/child", "/repos/acme/leaf", "/repos/acme/old", "/repos/acme/new", "/repos/acme/other", "/repos/acme/fresh", "/repos/acme/actions", "/repos/acme/tools", "/repos/acme/eslint", "/repos/acme/jest":
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

func TestCompleteness_OnlineModesInspectLockedManifests(t *testing.T) {
	for _, mode := range []string{"--verify", "--no-fix", "--no-onboard", ""} {
		t.Run("mode="+mode, func(t *testing.T) {
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
					args := []string{path}
					if mode != "" {
						args = append([]string{mode}, args...)
					}
					p, err := runParity(t, transport, args...)
					require.ErrorIs(t, err, errSilent)
					assert.False(t, p.Valid)
					require.Len(t, p.Findings, 1)
					assert.Contains(t, p.Findings[0].Detail, "lockfile omits that edge")
					assert.Equal(t, "acme/parent@v1.0.0", p.Findings[0].RequiredBy)
					assert.Equal(t, before, snapshotWorkflows(t))
				})
			}
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

func TestCompleteness_OnlineManifestFailureIsBlocking(t *testing.T) {
	for _, mode := range []string{"--verify", "--no-fix", "--no-onboard", ""} {
		t.Run("mode="+mode, func(t *testing.T) {
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
					args := []string{path}
					if mode != "" {
						args = append([]string{mode}, args...)
					}
					p, err := runParity(t, &graphTransport{failChild: true}, args...)
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
		})
	}
}

func TestCompleteness_WorkflowEditsCannotBypassLockedManifests(t *testing.T) {
	for _, mode := range []string{"", "--no-fix", "--no-onboard", "--verify"} {
		t.Run("mode="+mode, func(t *testing.T) {
			for _, edit := range []struct {
				name   string
				before string
				after  string
			}{
				{name: "adding a direct action", after: "      - uses: acme/other@v1\n"},
				{name: "bumping a direct action", before: "      - uses: acme/other@v1\n", after: "      - uses: acme/other@v2\n"},
				{name: "removing a direct action", before: "      - uses: acme/other@v1\n"},
			} {
				t.Run(edit.name, func(t *testing.T) {
					for _, damage := range []string{"omitted edge", "unreadable manifest"} {
						t.Run(damage, func(t *testing.T) {
							path, file := generateGraph(t)
							if edit.before != "" {
								require.NoError(t, os.WriteFile(path, []byte(graphWorkflow+edit.before), 0o600))
								_, stderr, err := runCommandWithHTTP(t, &graphTransport{}, "--no-narrow", path)
								require.NoError(t, err, stderr)
								file, err = parserlock.Parse([]byte(readTempLockfilePins(t)))
								require.NoError(t, err)
							}
							transport := &graphTransport{failChild: damage == "unreadable manifest"}
							if damage == "omitted edge" {
								parent := file.Dependencies["acme/parent@v1.0.0"]
								parent.Uses = nil
								file.Dependencies["acme/parent@v1.0.0"] = parent
								writeGraph(t, file)
							}
							require.NoError(t, os.WriteFile(path, []byte(graphWorkflow+edit.after), 0o600))
							before := snapshotWorkflows(t)
							args := []string{"--no-narrow", path}
							if mode != "" {
								args = append([]string{mode}, args...)
							}
							p, err := runParity(t, transport, args...)
							assert.ErrorIs(t, err, errSilent)
							assert.False(t, p.Valid)
							var found bool
							for _, f := range p.Findings {
								if strings.Contains(f.Detail, "lockfile omits that edge") ||
									strings.Contains(f.Detail, "could not verify locked dependency manifests") {
									found = true
									assert.Equal(t, "error", f.Severity)
								}
							}
							assert.True(t, found, "locked graph damage must be diagnosed: %+v", p.Findings)
							assert.Equal(t, before, snapshotWorkflows(t))
						})
					}
				})
			}
		})
	}
}

func TestCompleteness_ReadOnlyRefreshInspectsRetainedGraph(t *testing.T) {
	for _, mode := range [][]string{
		{"--verify", "--relock"},
		{"--verify", "--accept-moved"},
		{"--no-fix", "--relock"},
		{"--no-fix", "--accept-moved"},
	} {
		t.Run(strings.Join(mode, " "), func(t *testing.T) {
			for _, damage := range []string{"omitted edge", "unreadable manifest"} {
				t.Run(damage, func(t *testing.T) {
					path, file := generateGraph(t)
					if damage == "omitted edge" {
						parent := file.Dependencies["acme/parent@v1.0.0"]
						parent.Uses = nil
						file.Dependencies["acme/parent@v1.0.0"] = parent
						writeGraph(t, file)
					}
					before := snapshotWorkflows(t)
					args := append(slices.Clone(mode), "--no-narrow", path)
					p, err := runParity(t, &graphTransport{failChild: damage == "unreadable manifest"}, args...)
					assert.ErrorIs(t, err, errSilent)
					assert.False(t, p.Valid)
					var found bool
					for _, f := range p.Findings {
						if damage == "omitted edge" && strings.Contains(f.Detail, "lockfile omits that edge") {
							found = true
							assert.Equal(t, "not-pinned", f.Category)
							if f.Dependency != "acme/child@v2" {
								assert.Equal(t, "acme/parent@v1.0.0", f.Dependency)
								continue
							}
							assert.Equal(t, "acme/parent@v1.0.0", f.RequiredBy)
							assert.Equal(t, "error", f.Severity)
						}
						if damage == "unreadable manifest" && strings.Contains(f.Detail, "could not verify locked dependency manifests") {
							found = true
							assert.Equal(t, "reachability-unknown", f.Category)
							assert.Equal(t, "error", f.Severity)
						}
					}
					assert.True(t, found, "retained graph damage must be diagnosed: %+v", p.Findings)
					assert.Equal(t, before, snapshotWorkflows(t))
				})
			}
		})
	}
}

func TestCompleteness_ScopedOnboardingInspectsReusedPins(t *testing.T) {
	for _, reuse := range []struct {
		name, root, parent string
	}{
		{name: "locked root from another workflow", root: "acme/parent@v1.0.0", parent: "acme/parent@v1.0.0"},
		{name: "cached transitive below a fresh root", root: "acme/fresh@v1", parent: "acme/child@v2"},
	} {
		t.Run(reuse.name, func(t *testing.T) {
			for _, mode := range []string{"", "--no-fix", "--no-onboard", "--verify"} {
				t.Run("mode="+mode, func(t *testing.T) {
					for _, damage := range []string{"omitted edge", "unreadable manifest"} {
						t.Run(damage, func(t *testing.T) {
							_, file := generateGraph(t)
							const path = ".github/workflows/new.yml"
							require.NoError(t, os.WriteFile(path, []byte(strings.Replace(graphWorkflow, "acme/parent@v1.0.0", reuse.root, 1)), 0o600))
							if damage == "omitted edge" {
								parent := file.Dependencies[reuse.parent]
								parent.Uses = nil
								file.Dependencies[reuse.parent] = parent
								writeGraph(t, file)
							}
							before := snapshotWorkflows(t)
							args := []string{"--no-narrow", path}
							if mode != "" {
								args = append([]string{mode}, args...)
							}
							p, err := runParity(t, &graphTransport{failChild: damage == "unreadable manifest"}, args...)
							assert.ErrorIs(t, err, errSilent)
							assert.False(t, p.Valid)
							var found bool
							for _, f := range p.Findings {
								if damage == "omitted edge" && strings.Contains(f.Detail, "lockfile omits that edge") {
									found = true
									assert.Equal(t, "not-pinned", f.Category)
									assert.Equal(t, reuse.parent, f.RequiredBy)
									assert.Equal(t, "error", f.Severity)
								}
								if damage == "unreadable manifest" && strings.Contains(f.Detail, "could not verify locked dependency manifests") {
									found = true
									assert.Equal(t, "reachability-unknown", f.Category)
									assert.Equal(t, "error", f.Severity)
								}
							}
							assert.True(t, found, "reused locked pin must be inspected: %+v", p.Findings)
							assert.Equal(t, before, snapshotWorkflows(t))
						})
					}
				})
			}
		})
	}
}

func TestCompleteness_ScopedOnboardingPreservesReusedClosure(t *testing.T) {
	for _, root := range []string{"acme/parent@v1.0.0", "acme/fresh@v1"} {
		t.Run(root, func(t *testing.T) {
			old, file := generateGraph(t)
			const path = ".github/workflows/new.yml"
			require.NoError(t, os.WriteFile(path, []byte(strings.Replace(graphWorkflow, "acme/parent@v1.0.0", root, 1)), 0o600))
			_, stderr, err := runCommandWithHTTP(t, &graphTransport{}, "--no-narrow", path)
			require.NoError(t, err, stderr)
			after, err := parserlock.Parse([]byte(readTempLockfilePins(t)))
			require.NoError(t, err)
			assert.Equal(t, file.Workflows[old], after.Workflows[old])
			assert.Equal(t, []string{root}, after.Workflows[path])
			assert.Equal(t, []string{"acme/leaf@v3"}, after.Dependencies["acme/child@v2"].Uses)
			before := snapshotWorkflows(t)
			_, stderr, err = runCommandWithHTTP(t, &graphTransport{}, "--verify", path)
			require.NoError(t, err, stderr)
			assert.Equal(t, before, snapshotWorkflows(t))
		})
	}
}

type manifestTransport func(*http.Request) (*http.Response, error)

func (f manifestTransport) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func TestCompleteness_WorkflowRootsShareManifestBatch(t *testing.T) {
	for _, shared := range []bool{false, true} {
		for _, fullSHA := range []bool{false, true} {
			t.Run(fmt.Sprintf("shared=%v/full-SHA=%v", shared, fullSHA), func(t *testing.T) {
				path, other := seedManifestWorkflows(t, shared)
				if fullSHA {
					require.NoError(t, os.WriteFile(path, []byte(strings.Replace(graphWorkflow, "acme/parent@v1.0.0", "acme/leaf@"+graphLeafSHA, 1)), 0o600))
				}
				before := snapshotWorkflows(t)
				var batches atomic.Int64
				var inspected sync.Map
				transport := manifestTransport(func(req *http.Request) (*http.Response, error) {
					if !httpmock.GraphQL(`expr0=[0-9a-fA-F]{40}\^\{commit\}`)(req) {
						return (&graphTransport{}).RoundTrip(req)
					}
					batches.Add(1)
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
						inspected.Store(name, true)
						assert.Equal(t, graphLeafSHA+"^{commit}", body.Variables[fmt.Sprintf("expr%d", i)])
						data[fmt.Sprintf("a%d", i)] = testRepoResponse("acme/"+name, graphLeafSHA, nodeActionYAML)
					}
					return httpmock.JSONResponse(map[string]any{"data": data})(req)
				})
				p, err := runParity(t, transport, "--verify", path, other)
				require.NoError(t, err, "%+v", p.Findings)
				assert.True(t, p.Valid)
				assert.EqualValues(t, 1, batches.Load(), "both one-level roots fit one manifest batch")
				_, leaf := inspected.Load("leaf")
				_, second := inspected.Load("other")
				assert.True(t, leaf, "the full-SHA remap must retain the first root")
				assert.Equal(t, !shared, second)
				assert.Equal(t, before, snapshotWorkflows(t))
			})
		}
	}
}

func seedManifestWorkflows(t *testing.T, shared bool) (string, string) {
	t.Helper()
	path := writeTempWorkflow(t, strings.Replace(graphWorkflow, "acme/parent@v1.0.0", "acme/leaf@v3", 1))
	otherRef := "acme/other@v1"
	if shared {
		otherRef = "acme/leaf@v3"
	}
	const other = ".github/workflows/other.yml"
	require.NoError(t, os.WriteFile(other, []byte(strings.Replace(graphWorkflow, "acme/parent@v1.0.0", otherRef, 1)), 0o600))
	_, stderr, err := runCommandWithHTTP(t, &graphTransport{}, "--no-narrow", path, other)
	require.NoError(t, err, stderr)
	return path, other
}

func TestCompleteness_BatchedFailuresBelongToTheirWorkflows(t *testing.T) {
	for _, tt := range []struct {
		name     string
		manifest string
		denied   bool
		category string
	}{
		{name: "denied alias", manifest: nodeActionYAML, denied: true, category: "reachability-unknown"},
		{name: "missing manifest", category: "reachability-unknown"},
		{name: "malformed manifest", manifest: "runs: [", category: "reachability-unknown"},
		{name: "unlocked child", manifest: "runs:\n  using: composite\n  steps:\n    - uses: acme/unrecorded@v1\n", category: "not-pinned"},
		{name: "uses cycle", manifest: "runs:\n  using: composite\n  steps:\n    - uses: acme/other@v1\n", category: "not-pinned"},
	} {
		for _, reverse := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/reversed=%v", tt.name, reverse), func(t *testing.T) {
				path, other := seedManifestWorkflows(t, false)
				before := snapshotWorkflows(t)
				var batches atomic.Int64
				transport := manifestTransport(func(req *http.Request) (*http.Response, error) {
					if !httpmock.GraphQL(`expr0=[0-9a-fA-F]{40}\^\{commit\}`)(req) {
						return (&graphTransport{}).RoundTrip(req)
					}
					batches.Add(1)
					var body struct {
						Variables map[string]string `json:"variables"`
					}
					if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
						return nil, err
					}
					data := map[string]any{}
					var failures []map[string]any
					for i := 0; ; i++ {
						name, ok := body.Variables[fmt.Sprintf("name%d", i)]
						if !ok {
							break
						}
						alias := fmt.Sprintf("a%d", i)
						yml := nodeActionYAML
						if name == "other" {
							yml = tt.manifest
							if tt.denied {
								failures = append(failures, map[string]any{
									"type": "FORBIDDEN", "message": "manifest denied", "path": []string{alias, "object", "fileYaml"},
								})
							}
						}
						data[alias] = testRepoResponse("acme/"+name, graphLeafSHA, yml)
					}
					return httpmock.JSONResponse(map[string]any{"data": data, "errors": failures})(req)
				})
				paths := []string{path, other}
				if reverse {
					slices.Reverse(paths)
				}
				p, err := runParity(t, transport, append([]string{"--verify"}, paths...)...)
				require.ErrorIs(t, err, errSilent)
				assert.False(t, p.Valid)
				var failures int
				for _, finding := range p.Findings {
					if finding.Severity != "error" {
						continue
					}
					failures++
					assert.Equal(t, other, finding.Workflow)
					assert.Equal(t, tt.category, finding.Category)
				}
				assert.Positive(t, failures)
				assert.EqualValues(t, 1, batches.Load(), "alias, parse and graph failures do not retry the traversal")
				assert.Equal(t, before, snapshotWorkflows(t))
			})
		}
	}
}

func TestCompleteness_BatchedProxyReadsDoNotRefetchFailures(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusForbidden, http.StatusServiceUnavailable} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			path, other := seedManifestWorkflows(t, false)
			t.Setenv("GH_ACTIONS_LOCK_DEPENDABOT_PROXY", "1")
			reg := &httpmock.Registry{}
			defer reg.Verify(t)
			for repo, ref := range map[string]string{"leaf": "v3", "other": "v1"} {
				reg.Register(httpmock.REST("GET", `^/repos/acme/`+repo+`$`), httpmock.JSONResponse(map[string]any{
					"full_name": "acme/" + repo, "id": 1, "owner": map[string]any{"id": 1},
				}))
				reg.Register(httpmock.REST("GET", `^/repos/acme/`+repo+`/git/ref/tags/`+ref+`$`),
					httpmock.JSONResponse(httpmock.GitRefResponse("refs/tags/"+ref, graphLeafSHA)))
				reg.Register(httpmock.REST("GET", `^/repos/acme/`+repo+`/commits/`+graphLeafSHA+`$`),
					httpmock.JSONResponse(map[string]any{"sha": graphLeafSHA}))
				var respond httpmock.Responder = httpmock.JSONResponse(map[string]any{
					"encoding": "base64", "content": base64.StdEncoding.EncodeToString([]byte(nodeActionYAML)),
				})
				if repo == "other" && status != http.StatusOK {
					respond = httpmock.StatusResponse(status)
				}
				reg.Register(httpmock.REST("GET", `^/repos/acme/`+repo+`/contents/action\.yml$`), respond)
			}
			var commits, manifests atomic.Int64
			transport := manifestTransport(func(req *http.Request) (*http.Response, error) {
				if strings.Contains(req.URL.Path, "/commits/") {
					commits.Add(1)
				}
				if strings.Contains(req.URL.Path, "/contents/") {
					manifests.Add(1)
				}
				return reg.RoundTrip(req)
			})
			useDefaultTransport(t, transport)
			before := snapshotWorkflows(t)
			if status == http.StatusServiceUnavailable {
				_, _, err := runCommandWithHTTP(t, transport, "--verify", path, other)
				require.Error(t, err)
				assert.NotErrorIs(t, err, errSilent, "a proxy outage remains a retry rather than a finding")
				assert.ErrorContains(t, err, "could not verify locked dependency manifests")
			} else {
				p, err := runParity(t, transport, "--verify", path, other)
				if status == http.StatusOK {
					require.NoError(t, err, "%+v", p.Findings)
					assert.True(t, p.Valid)
				} else {
					require.ErrorIs(t, err, errSilent)
					assert.False(t, p.Valid)
					for _, finding := range p.Findings {
						if finding.Severity == "error" {
							assert.Equal(t, other, finding.Workflow)
							assert.Equal(t, "reachability-unknown", finding.Category)
						}
					}
				}
			}
			assert.EqualValues(t, 2, commits.Load(), "each ref has one immutable commit lookup; parity uses the matching tag target")
			assert.EqualValues(t, 2, manifests.Load(), "each ref has one immutable manifest read, including failures")
			assert.Equal(t, before, snapshotWorkflows(t))
		})
	}
}

func TestCompleteness_MissingMetadataIsNotARetry(t *testing.T) {
	for _, scoped := range []bool{false, true} {
		t.Run(fmt.Sprintf("new-workflow=%v", scoped), func(t *testing.T) {
			path, file := generateGraph(t)
			delete(file.Dependencies, "acme/child@v2")
			writeGraph(t, file)
			args := []string{"--verify", "--relock", path}
			if scoped {
				path = ".github/workflows/new.yml"
				require.NoError(t, os.WriteFile(path, []byte(graphWorkflow), 0o600))
				args = []string{"--verify", path}
			}
			before := snapshotWorkflows(t)
			p, err := runParity(t, &graphTransport{}, args...)
			require.ErrorIs(t, err, errSilent)
			var missing int
			for _, f := range p.Findings {
				if f.Severity != "error" {
					continue
				}
				assert.Equal(t, "not-pinned", f.Category)
				if scoped && f.Dependency == "acme/parent@v1.0.0" && f.RequiredBy == "" {
					continue
				}
				assert.Equal(t, "acme/child@v2", f.Dependency)
				assert.Equal(t, "acme/parent@v1.0.0", f.RequiredBy)
				assert.Contains(t, f.Remediation, "--relock")
				assert.Contains(t, f.Remediation, "restore")
				missing++
			}
			assert.Equal(t, 1, missing)
			assert.Equal(t, before, snapshotWorkflows(t))
		})
	}
}

func TestCompleteness_RetainsSiblingManifestPaths(t *testing.T) {
	for _, damaged := range []bool{false, true} {
		for _, reverse := range []bool{false, true} {
			t.Run(fmt.Sprintf("damaged=%v/reverse=%v", damaged, reverse), func(t *testing.T) {
				path := writeTempWorkflow(t, strings.Replace(graphWorkflow, "acme/parent@v1.0.0", "acme/actions/b@v1", 1))
				actions := []string{"acme/actions/a@v1", "acme/actions/b@v1"}
				if reverse {
					slices.Reverse(actions)
				}
				transport := &graphTransport{manifests: map[string]string{
					"acme/actions/a@v1": nodeActionYAML, "acme/actions/a@" + graphLeafSHA: nodeActionYAML,
					"acme/actions/b@v1":              "runs:\n  using: composite\n  steps:\n    - uses: acme/leaf@v3\n",
					"acme/actions/b@" + graphLeafSHA: "runs:\n  using: composite\n  steps:\n    - uses: acme/leaf@v3\n",
					"acme/fresh@v1":                  "runs:\n  using: composite\n  steps:\n    - uses: " + actions[0] + "\n    - uses: " + actions[1] + "\n",
				}}
				_, stderr, err := runCommandWithHTTP(t, transport, "--no-narrow", path)
				require.NoError(t, err, stderr)
				file, err := parserlock.Parse([]byte(readTempLockfilePins(t)))
				require.NoError(t, err)
				if damaged {
					action := file.Dependencies["acme/actions@v1"]
					action.Uses = nil
					file.Dependencies["acme/actions@v1"] = action
					writeGraph(t, file)
				}
				const fresh = ".github/workflows/new.yml"
				require.NoError(t, os.WriteFile(fresh, []byte(strings.Replace(graphWorkflow, "acme/parent@v1.0.0", "acme/fresh@v1", 1)), 0o600))
				before := snapshotWorkflows(t)
				p, err := runParity(t, transport, "--no-narrow", fresh)
				if damaged {
					require.ErrorIs(t, err, errSilent)
					assert.False(t, p.Valid)
					assert.True(t, slices.ContainsFunc(p.Findings, func(f format.Finding) bool {
						return f.Category == "not-pinned" && f.Dependency == "acme/leaf@v3" && f.RequiredBy == "acme/actions@v1"
					}), "%+v", p.Findings)
					assert.Equal(t, before, snapshotWorkflows(t))
				} else {
					require.NoError(t, err, "%+v", p.Findings)
				}
			})
		}
	}
}

func TestCompleteness_ReadOnlyRefreshIgnoresLiveClosure(t *testing.T) {
	for _, mode := range []string{"--verify", "--no-fix"} {
		for _, refresh := range []string{"--relock", "--accept-moved"} {
			t.Run(mode+"/"+refresh, func(t *testing.T) {
				path := writeTempWorkflow(t, graphWorkflow)
				const other = ".github/workflows/other.yml"
				require.NoError(t, os.WriteFile(other, []byte(strings.Replace(graphWorkflow, "acme/parent@v1.0.0", "acme/child@v2", 1)), 0o600))
				seed := &graphTransport{manifests: map[string]string{
					"acme/parent@v1.0.0": nodeActionYAML, "acme/parent@" + graphParentSHA: nodeActionYAML,
				}}
				_, stderr, err := runCommandWithHTTP(t, seed, "--no-narrow", path, other)
				require.NoError(t, err, stderr)
				file, err := parserlock.Parse([]byte(readTempLockfilePins(t)))
				require.NoError(t, err)
				child := file.Dependencies["acme/child@v2"]
				child.Uses = nil
				file.Dependencies["acme/child@v2"] = child
				writeGraph(t, file)
				before := snapshotWorkflows(t)
				live := &graphTransport{movedParent: true, manifests: map[string]string{
					"acme/parent@v1.0.0":            "runs:\n  using: composite\n  steps:\n    - uses: acme/child@v2\n",
					"acme/parent@" + graphParentSHA: nodeActionYAML,
				}}
				p, err := runParity(t, live, mode, refresh, "--no-narrow", path)
				require.NoError(t, err, "%+v", p.Findings)
				assert.True(t, p.Valid, "%+v", p.Findings)
				assert.Equal(t, before, snapshotWorkflows(t))
			})
		}
	}
}

func TestCompleteness_OwnedSHAIdentityPrecedesGlobalAlias(t *testing.T) {
	path, file := generateGraph(t)
	action := file.Dependencies["acme/parent@v1.0.0"]
	file.Dependencies["acme/parent@"+graphParentSHA] = action
	const other = ".github/workflows/other.yml"
	file.Workflows[other] = []string{"acme/parent@" + graphParentSHA}
	shaWorkflow := strings.Replace(graphWorkflow, "acme/parent@v1.0.0", "acme/parent@"+graphParentSHA, 1)
	require.NoError(t, os.WriteFile(path, []byte(shaWorkflow), 0o600))
	require.NoError(t, os.WriteFile(other, []byte(shaWorkflow), 0o600))
	action.Uses = nil
	file.Dependencies["acme/parent@v1.0.0"] = action
	writeGraph(t, file)
	before := snapshotWorkflows(t)
	p, err := runParity(t, &graphTransport{}, "--verify", path)
	require.ErrorIs(t, err, errSilent)
	assert.True(t, slices.ContainsFunc(p.Findings, func(f format.Finding) bool {
		return f.Category == "not-pinned" && f.Dependency == "acme/child@v2" && f.RequiredBy == "acme/parent@v1.0.0"
	}), "%+v", p.Findings)
	assert.Equal(t, before, snapshotWorkflows(t))
}

func TestCompleteness_NewPathPreservesRetainedPathCoverage(t *testing.T) {
	for _, damaged := range []bool{false, true} {
		t.Run(fmt.Sprintf("retained-path-damaged=%v", damaged), func(t *testing.T) {
			path := writeTempWorkflow(t, strings.Replace(graphWorkflow, "acme/parent@v1.0.0", "acme/tools/lint@v1", 1))
			transport := &graphTransport{manifests: map[string]string{
				"acme/tools/lint@v1":              "runs:\n  using: composite\n  steps:\n    - uses: acme/eslint@v1\n",
				"acme/tools/lint@" + graphLeafSHA: "runs:\n  using: composite\n  steps:\n    - uses: acme/eslint@v1\n",
				"acme/tools/test@v1":              "runs:\n  using: composite\n  steps:\n    - uses: acme/jest@v1\n",
				"acme/tools/test@" + graphLeafSHA: "runs:\n  using: composite\n  steps:\n    - uses: acme/jest@v1\n",
			}}
			_, stderr, err := runCommandWithHTTP(t, transport, "--no-narrow", path)
			require.NoError(t, err, stderr)
			file, err := parserlock.Parse([]byte(readTempLockfilePins(t)))
			require.NoError(t, err)
			if damaged {
				action := file.Dependencies["acme/tools@v1"]
				action.Uses = []string{"acme/jest@v1"}
				file.Dependencies["acme/tools@v1"] = action
				file.Dependencies["acme/jest@v1"] = file.Dependencies["acme/eslint@v1"]
				writeGraph(t, file)
			}
			const fresh = ".github/workflows/new.yml"
			require.NoError(t, os.WriteFile(fresh, []byte(strings.Replace(graphWorkflow, "acme/parent@v1.0.0", "acme/tools/test@v1", 1)), 0o600))
			before := snapshotWorkflows(t)
			p, err := runParity(t, transport, "--no-narrow", fresh)
			if damaged {
				require.ErrorIs(t, err, errSilent)
				assert.True(t, slices.ContainsFunc(p.Findings, func(f format.Finding) bool {
					return f.Category == "not-pinned" && f.Dependency == "acme/eslint@v1" && f.RequiredBy == "acme/tools@v1"
				}), "%+v", p.Findings)
				assert.Equal(t, before, snapshotWorkflows(t))
			} else {
				require.NoError(t, err, "%+v", p.Findings)
				after, err := parserlock.Parse([]byte(readTempLockfilePins(t)))
				require.NoError(t, err)
				assert.ElementsMatch(t, []string{"acme/eslint@v1", "acme/jest@v1"}, after.Dependencies["acme/tools@v1"].Uses)
				assert.Equal(t, file.Workflows[path], after.Workflows[path])
				_, stderr, err = runCommandWithHTTP(t, transport, "--verify", path, fresh)
				require.NoError(t, err, stderr)
			}
		})
	}
}

func TestCompleteness_RefreshPreservesUnselectedManifestPaths(t *testing.T) {
	for _, refresh := range []string{"--relock", "--accept-moved"} {
		for _, damaged := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/damaged=%v", refresh, damaged), func(t *testing.T) {
				path := writeTempWorkflow(t, strings.Replace(graphWorkflow, "acme/parent@v1.0.0", "acme/tools/lint@v1", 1))
				transport := &graphTransport{manifests: map[string]string{
					"acme/tools/lint@v1":              "runs:\n  using: composite\n  steps:\n    - uses: acme/eslint@v1\n",
					"acme/tools/lint@" + graphLeafSHA: "runs:\n  using: composite\n  steps:\n    - uses: acme/eslint@v1\n",
					"acme/tools/test@v1":              "runs:\n  using: composite\n  steps:\n    - uses: acme/jest@v1\n",
					"acme/tools/test@" + graphLeafSHA: "runs:\n  using: composite\n  steps:\n    - uses: acme/jest@v1\n",
				}}
				_, stderr, err := runCommandWithHTTP(t, transport, "--no-narrow", path)
				require.NoError(t, err, stderr)
				file, err := parserlock.Parse([]byte(readTempLockfilePins(t)))
				require.NoError(t, err)
				if damaged {
					action := file.Dependencies["acme/tools@v1"]
					action.Uses = nil
					file.Dependencies["acme/tools@v1"] = action
					writeGraph(t, file)
				}
				const fresh = ".github/workflows/new.yml"
				require.NoError(t, os.WriteFile(fresh, []byte(strings.Replace(graphWorkflow, "acme/parent@v1.0.0", "acme/tools/test@v1", 1)), 0o600))
				before := snapshotWorkflows(t)
				p, err := runParity(t, transport, refresh, "--no-narrow", fresh)
				if damaged {
					require.ErrorIs(t, err, errSilent)
					assert.False(t, p.Valid)
					assert.True(t, slices.ContainsFunc(p.Findings, func(f format.Finding) bool {
						return f.Category == "not-pinned" && f.Severity == "error" &&
							f.Dependency == "acme/eslint@v1" && f.RequiredBy == "acme/tools@v1"
					}), "%+v", p.Findings)
					assert.Equal(t, before, snapshotWorkflows(t))
				} else {
					require.NoError(t, err, "%+v", p.Findings)
					after, err := parserlock.Parse([]byte(readTempLockfilePins(t)))
					require.NoError(t, err)
					assert.ElementsMatch(t, []string{"acme/eslint@v1", "acme/jest@v1"}, after.Dependencies["acme/tools@v1"].Uses)
					assert.Equal(t, file.Workflows[path], after.Workflows[path])
				}
			})
		}
	}
}

func TestCompleteness_UnprovenOwnerCannotAuthorizeNewPath(t *testing.T) {
	for _, source := range []string{"intact", "missing", "changed", "changed-path", "malformed"} {
		for _, mode := range []string{"--no-narrow", "--verify"} {
			t.Run(source+"/"+mode, func(t *testing.T) {
				path := writeTempWorkflow(t, strings.Replace(graphWorkflow, "acme/parent@v1.0.0", "acme/tools/lint@v1", 1))
				transport := &graphTransport{manifests: map[string]string{
					"acme/tools/lint@v1":              "runs:\n  using: composite\n  steps:\n    - uses: acme/eslint@v1\n",
					"acme/tools/lint@" + graphLeafSHA: "runs:\n  using: composite\n  steps:\n    - uses: acme/eslint@v1\n",
					"acme/tools/test@v1":              "runs:\n  using: composite\n  steps:\n    - uses: acme/jest@v1\n",
					"acme/tools/test@" + graphLeafSHA: "runs:\n  using: composite\n  steps:\n    - uses: acme/jest@v1\n",
				}}
				_, stderr, err := runCommandWithHTTP(t, transport, "--no-narrow", path)
				require.NoError(t, err, stderr)
				switch source {
				case "missing":
					require.NoError(t, os.Remove(path))
				case "changed":
					require.NoError(t, os.WriteFile(path, []byte(strings.Replace(graphWorkflow, "acme/parent@v1.0.0", "acme/other@v1", 1)), 0o600))
				case "changed-path":
					require.NoError(t, os.WriteFile(path, []byte(strings.Replace(graphWorkflow, "acme/parent@v1.0.0", "acme/tools/test@v1", 1)), 0o600))
				case "malformed":
					require.NoError(t, os.WriteFile(path, []byte("jobs: ["), 0o600))
				}
				const fresh = ".github/workflows/new.yml"
				require.NoError(t, os.WriteFile(fresh, []byte(strings.Replace(graphWorkflow, "acme/parent@v1.0.0", "acme/tools/test@v1", 1)), 0o640))
				before := snapshotWorkflows(t)
				info, err := os.Stat(fresh)
				require.NoError(t, err)
				p, err := runParity(t, transport, mode, fresh)
				if source == "intact" && mode == "--no-narrow" {
					require.NoError(t, err, "%+v", p.Findings)
					return
				}
				require.ErrorIs(t, err, errSilent)
				if source != "intact" {
					assert.True(t, slices.ContainsFunc(p.Findings, func(f format.Finding) bool {
						return f.Workflow == fresh && f.Category == "not-pinned" &&
							strings.Contains(f.Detail, "retained") && strings.Contains(f.Detail, path) &&
							strings.Contains(f.Remediation, "--relock")
					}), "%+v", p.Findings)
					assert.False(t, slices.ContainsFunc(p.Findings, func(f format.Finding) bool {
						return f.Category == "reachability-unknown"
					}), "%+v", p.Findings)
				}
				assert.Equal(t, before, snapshotWorkflows(t))
				after, err := os.Stat(fresh)
				require.NoError(t, err)
				assert.Equal(t, info.Mode(), after.Mode())
			})
		}
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

// hostFixture records acme/child at a full SHA on the selected tenant host
// and, when conflict, also under another spelling on github.com. Requests
// outside the injected transport fail.
func hostFixture(t *testing.T, conflict bool) string {
	t.Helper()
	const sha = "abcdef0123456789abcdef0123456789abcdef01"
	path := writeTempWorkflow(t, "on: push\njobs:\n  test:\n    runs-on: ubuntu-latest\n    steps:\n      - uses: acme/child@"+sha+"\n")
	t.Setenv("GH_HOST", "tenant.ghe.com")
	old := http.DefaultTransport
	http.DefaultTransport = denyTransport{}
	t.Cleanup(func() { http.DefaultTransport = old })
	entry := func(ref, host, uses string) string {
		e := "  'acme/child@" + ref + "':\n    hostname: '" + host + "'\n    ref: 'v2'\n    commit: 'sha1-" + sha + "'\n    owner_id: 1\n    repo_id: 1\n"
		if uses != "" {
			e += "    uses:\n      - '" + uses + "'\n"
		}
		return e
	}
	lock := "version: 'v0.0.3'\nworkflows:\n  '" + path + "':\n    - 'acme/child@" + sha + "'\ndependencies:\n" +
		entry(sha, "tenant.ghe.com", "acme/leaf@v3") +
		"  'acme/leaf@v3':\n    hostname: 'tenant.ghe.com'\n    ref: 'v3'\n    commit: 'sha1-" + graphLeafSHA + "'\n    owner_id: 1\n    repo_id: 1\n"
	if conflict {
		lock += entry(strings.ToUpper(sha), "github.com", "")
	}
	require.NoError(t, os.WriteFile(filepath.Join(".github", "workflows", "actions.lock"), []byte(lock), 0o600))
	return path
}

type denyTransport struct{}

func (denyTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return nil, fmt.Errorf("unexpected request outside the fixture: %s", req.URL)
}

// TestRun_RepositoryOnTwoHostsBlocks: one repository recorded on two hosts
// can't be resolved on either without guessing, so every mode reports a
// blocking finding, no host is asked about it, and nothing is written for
// any workflow, even one that doesn't use it. It is a diagnosis, not a tool
// failure. A single host is fine.
func TestRun_RepositoryOnTwoHostsBlocks(t *testing.T) {
	const sha = "abcdef0123456789abcdef0123456789abcdef01"
	for _, tt := range []struct {
		name string
		args []string
		// other scans only a workflow that doesn't use the repository.
		other bool
	}{
		{name: "plain", args: []string{"--no-narrow"}},
		{name: "verify-local", args: []string{"--verify-local"}},
		{name: "verify", args: []string{"--verify"}},
		{name: "relock", args: []string{"--relock", "--no-narrow"}},
		{name: "plain, another workflow only", args: []string{"--no-narrow"}, other: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			path := hostFixture(t, true)
			other := filepath.Join(".github", "workflows", "other.yml")
			require.NoError(t, os.WriteFile(other, []byte(graphWorkflow), 0o600))
			before := snapshotWorkflows(t)
			args := tt.args
			want := path
			if tt.other {
				args = append(args, other)
				want = filepath.ToSlash(other)
			}

			rt := &requestLog{rt: &graphTransport{childSHA: sha}}
			p, err := runParity(t, rt, args...)
			require.ErrorIs(t, err, errSilent)
			assert.False(t, p.Valid)
			hostFinding := slices.IndexFunc(p.Findings, func(f format.Finding) bool {
				return f.Severity == "error" && f.Workflow == want && strings.Contains(f.Detail, "acme/child is recorded on github.com and tenant.ghe.com")
			})
			assert.NotEqual(t, -1, hostFinding, "%+v", p.Findings)
			assert.Equal(t, before, snapshotWorkflows(t), "nothing is written")
			assert.Empty(t, rt.about("child"), "no host is asked about acme/child")
		})
	}

	t.Run("one host", func(t *testing.T) {
		hostFixture(t, false)
		transport := &graphTransport{childSHA: sha}
		_, stderr, err := runCommandWithHTTP(t, transport, "--no-narrow")
		require.NoError(t, err, stderr)
		before := snapshotWorkflows(t)
		for _, mode := range [][]string{{"--no-narrow"}, {"--verify-local"}, {"--verify"}} {
			_, stderr, err := runCommandWithHTTP(t, transport, mode...)
			require.NoError(t, err, "%v: %s", mode, stderr)
			assert.Equal(t, before, snapshotWorkflows(t))
		}
	})
}

// requestLog records the repository names its requests ask about.
type requestLog struct {
	rt   http.RoundTripper
	mu   sync.Mutex
	seen []string
}

func (l *requestLog) RoundTrip(req *http.Request) (*http.Response, error) {
	var names []string
	if parts := strings.Split(req.URL.Path, "/"); len(parts) > 3 && parts[1] == "repos" {
		names = append(names, parts[3])
	}
	if req.Body != nil {
		body, err := io.ReadAll(req.Body)
		if err != nil {
			return nil, err
		}
		req.Body = io.NopCloser(bytes.NewReader(body))
		var gql struct {
			Variables map[string]any `json:"variables"`
		}
		if json.Unmarshal(body, &gql) == nil {
			for k, v := range gql.Variables {
				if s, ok := v.(string); ok && strings.HasPrefix(k, "name") {
					names = append(names, s)
				}
			}
		}
	}
	l.mu.Lock()
	l.seen = append(l.seen, names...)
	l.mu.Unlock()
	return l.rt.RoundTrip(req)
}

func (l *requestLog) about(repo string) []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []string
	for _, n := range l.seen {
		if strings.EqualFold(n, repo) {
			out = append(out, n)
		}
	}
	return out
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

// TestContainerImagesNamedInRepository checks images named in the
// repository's own files in every mode, offline included, since they need no
// API request.
func TestContainerImagesNamedInRepository(t *testing.T) {
	files := map[string]string{
		".github/workflows/a.yml": `
on: push
jobs:
  build:
    runs-on: ubuntu-latest
    container: node:20
    services:
      db:
        image: postgres@sha256:0123456789abcdef
      cache: redis
      dynamic:
        image: ${{ matrix.image }}
    steps:
      - uses: $/actions/build
  test:
    runs-on: ubuntu-latest
    container:
      image: node:20
      credentials:
        username: u
        password: ${{ secrets.TOKEN }}
    steps:
      - run: true
`,
		".github/workflows/b.yml": `
on: push
jobs:
  lint:
    runs-on: ubuntu-latest
    container: node:20
    steps:
      - uses: docker://node:20
`,
		"actions/build/action.yml": "runs:\n  using: docker\n  image: Dockerfile\n",
		".github/workflows/actions.lock": "version: '" + parserlock.Version + "'\nworkflows:\n" +
			"  '.github/workflows/a.yml': []\n  '.github/workflows/b.yml': []\n",
	}
	node := "node:20 is not covered by the actions lockfile; container image tags are mutable"
	redis := "redis is not covered by the actions lockfile; an untagged image pulls the mutable latest tag"
	build := "Dockerfile (built by $/actions/build) is not covered by the actions lockfile; the image and its base images are built on the runner at run time"

	for _, mode := range []string{"", "--verify-local"} {
		for _, jsonOutput := range []bool{false, true} {
			t.Run(fmt.Sprintf("mode=%q/json=%t", mode, jsonOutput), func(t *testing.T) {
				dir := t.TempDir()
				require.NoError(t, os.Mkdir(filepath.Join(dir, ".git"), 0o755))
				for name, content := range files {
					path := filepath.Join(dir, filepath.FromSlash(name))
					require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
					require.NoError(t, os.WriteFile(path, []byte(strings.TrimLeft(content, "\n")), 0o600))
				}
				t.Chdir(dir)
				args := []string{"--no-interactive"}
				if mode != "" {
					args = append(args, mode)
				}
				if jsonOutput {
					args = append(args, "--json=valid,findings")
				}
				stdout, stderr, err := runCommandWithHTTP(t, &httpmock.Registry{}, args...)
				require.NoError(t, err, "container image warnings never fail the run: %s", stderr)
				for _, skipped := range []string{"postgres@sha256", "matrix.image"} {
					assert.NotContains(t, stdout+stderr, skipped, "digest pins and expressions produce nothing")
				}
				if !jsonOutput {
					assert.Equal(t, 1, strings.Count(stderr, node), "one image renders once:\n%s", stderr)
					assert.Contains(t, stderr, "in .github/workflows/a.yml (jobs.build.container, jobs.test.container)")
					assert.Contains(t, stderr, "in .github/workflows/b.yml (jobs.lint.container, jobs.lint.steps[0])")
					assert.Contains(t, stderr, redis)
					assert.Contains(t, stderr, build)
					assert.NotContains(t, stderr, "maintainer", "the repository owns every image here")
					return
				}
				var out struct {
					Valid    bool `json:"valid"`
					Findings []struct {
						Workflow string `json:"workflow"`
						Category string `json:"category"`
						Severity string `json:"severity"`
						Detail   string `json:"detail"`
					} `json:"findings"`
				}
				require.NoError(t, json.Unmarshal([]byte(stdout), &out))
				assert.True(t, out.Valid)
				var got []string
				for _, f := range out.Findings {
					if f.Category == "container-image" {
						assert.Equal(t, "warning", f.Severity)
						got = append(got, f.Workflow+": "+f.Detail)
					}
				}
				assert.ElementsMatch(t, []string{
					".github/workflows/a.yml: " + node,
					".github/workflows/a.yml: " + redis,
					".github/workflows/a.yml: " + build,
					".github/workflows/b.yml: " + node,
				}, got, "JSON findings are per workflow, once per image")
			})
		}
	}
}

// transportFunc adapts a function to http.RoundTripper.
type transportFunc func(*http.Request) (*http.Response, error)

func (f transportFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

// TestRemoteContainerImageWarnings reads images from the action files the
// resolver already fetches: the transport fails any other request.
func TestRemoteContainerImageWarnings(t *testing.T) {
	tests := []struct {
		name       string
		manifest   string
		dockerfile bool
		detail     string
	}{
		{
			name:     "docker action by tag",
			manifest: "runs:\n  using: docker\n  image: docker://alpine:3\n",
			detail:   "alpine:3 (pulled by actions/checkout@v4.2.2) is not covered by the actions lockfile; the lock pins the action's commit, not the image, and container image tags are mutable",
		},
		{
			name:     "docker action by digest",
			manifest: "runs:\n  using: docker\n  image: docker://alpine@sha256:0123456789abcdef\n",
		},
		{
			name:     "docker action Dockerfile",
			manifest: "runs:\n  using: docker\n  image: Dockerfile\n",
			detail:   "Dockerfile (built by actions/checkout@v4.2.2) is not covered by the actions lockfile; the lock pins the action's commit, but the image and its base images are built on the runner at run time",
		},
		{
			name:       "Dockerfile-only action",
			dockerfile: true,
			detail:     "Dockerfile (built by actions/checkout@v4.2.2) is not covered by the actions lockfile; the lock pins the action's commit, but the image and its base images are built on the runner at run time",
		},
		{
			name:     "composite docker step",
			manifest: "runs:\n  using: composite\n  steps:\n    - uses: docker://ghcr.io/o/img:1\n",
			detail:   "ghcr.io/o/img:1 (pulled by actions/checkout@v4.2.2) is not covered by the actions lockfile; the lock pins the action's commit, not the image, and container image tags are mutable",
		},
	}
	const sha = "cccccccccccccccccccccccccccccccccccccccc"
	for _, tt := range tests {
		for _, verify := range []bool{false, true} {
			for _, jsonOutput := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/verify=%t/json=%t", tt.name, verify, jsonOutput), func(t *testing.T) {
					var pins []string
					if verify {
						pins = []string{"actions/checkout@v4.2.2=sha1-" + sha}
					}
					path := writeTempWorkflow(t, "jobs:\n  build:\n    steps:\n      - uses: actions/checkout@v4.2.2\n", pins...)
					repo := testRepoResponse("actions/checkout", sha, tt.manifest)
					if tt.dockerfile {
						object := repo["object"].(map[string]any)
						object["file"] = nil
						object["dockerfile"] = map[string]any{"oid": sha}
					}
					rt := transportFunc(func(req *http.Request) (*http.Response, error) {
						switch {
						case httpmock.GraphQL(`commit: object\(oid`)(req):
							return parityOK(req)
						case httpmock.GraphQL(`file: file\(path`)(req):
							return httpmock.JSONResponse(map[string]any{"data": map[string]any{"a0": repo}})(req)
						case req.URL.Path == "/repos/actions/checkout":
							return httpmock.JSONResponse(map[string]any{"id": 1, "owner": map[string]any{"id": 1}})(req)
						case strings.HasSuffix(req.URL.Path, "/releases"):
							return httpmock.JSONResponse([]any{})(req)
						}
						return nil, fmt.Errorf("unexpected request: %s %s", req.Method, req.URL)
					})
					args := []string{"--no-narrow", "--no-interactive"}
					if verify {
						args = append(args, "--verify")
					}
					if jsonOutput {
						args = append(args, "--json=valid,findings")
					}
					stdout, stderr, err := runCommandWithHTTP(t, rt, append(args, path)...)
					require.NoError(t, err, "container image warnings never fail the run: %s", stderr)
					if !jsonOutput {
						if tt.detail == "" {
							assert.NotContains(t, stderr, "not covered by the actions lockfile")
						} else {
							assert.Contains(t, stderr, tt.detail)
						}
						return
					}
					var out struct {
						Findings []struct {
							Category   string `json:"category"`
							Severity   string `json:"severity"`
							RequiredBy string `json:"required_by"`
							Detail     string `json:"detail"`
						} `json:"findings"`
					}
					require.NoError(t, json.Unmarshal([]byte(stdout), &out))
					var images []string
					for _, f := range out.Findings {
						if f.Category != "container-image" {
							continue
						}
						images = append(images, f.Detail)
						assert.Equal(t, "warning", f.Severity)
						assert.Equal(t, "actions/checkout@v4.2.2", f.RequiredBy)
					}
					if tt.detail == "" {
						assert.Empty(t, images)
					} else {
						assert.Equal(t, []string{tt.detail}, images)
					}
				})
			}
		}
	}
}
