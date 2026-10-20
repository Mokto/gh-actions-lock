// Package e2e runs the scenario catalog (test/scenarios/catalog.yml) against
// the compiled gh-actions-lock binary. Stub scenarios talk to a fake GitHub
// over TLS; live scenarios use real credentials.
package e2e

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cli/go-gh/v2/pkg/jq"
	"github.com/creack/pty"
	"github.com/github/gh-actions-lock/internal/ghapi/httpmock"
	"github.com/github/gh-actions-lock/internal/testfixture"
	"github.com/github/gh-actions-lock/test/scenarios"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

const (
	lockPath  = ".github/workflows/actions.lock"
	stubToken = "gho_fake_default_stub_token"
)

// Build compiles the binary into dir, instrumented for coverage if cover.
func Build(dir string, cover bool) (string, error) {
	bin := filepath.Join(dir, "gh-actions-lock")
	args := []string{"build", "-o", bin}
	if cover {
		args = append(args, "-cover")
	}
	args = append(args, "github.com/github/gh-actions-lock/cmd/gh-actions-lock")
	if out, err := exec.Command("go", args...).CombinedOutput(); err != nil {
		return "", fmt.Errorf("go build: %w\n%s", err, out)
	}
	return bin, nil
}

// Runner runs catalog scenarios against a built binary.
type Runner struct {
	Bin string
	// CoverDir, if set, receives coverage from a -cover binary.
	CoverDir string
	// Update rewrites golden_json in the catalog instead of comparing.
	Update bool

	mu sync.Mutex
}

type result struct {
	exit   int
	stdout string
	output string
}

// Run lays down the scenario's repository, runs the binary, and checks the
// scenario's expectations.
func (r *Runner) Run(t *testing.T, sc scenarios.Scenario) {
	dir := t.TempDir()
	env := environ()
	if sc.Live() {
		token := liveToken()
		if token == "" {
			t.Skip("no GH_TOKEN, GITHUB_TOKEN, or gh auth token")
		}
		env["GH_TOKEN"] = token
	}
	if sc.LiveRepo != "" {
		if out, err := exec.Command("git", "clone", "--depth=1", "--quiet", "https://github.com/"+sc.LiveRepo+".git", dir).CombinedOutput(); err != nil {
			t.Skipf("clone %s failed: %v\n%s", sc.LiveRepo, err, out)
		}
	} else {
		writeRepo(t, dir, sc.Fixtures)
	}
	if sc.Fixtures.DeleteLockfile {
		require.NoError(t, os.RemoveAll(filepath.Join(dir, lockPath)))
	}

	env["GH_ACTIONS_LOCK_CACHE_DIR"] = t.TempDir()
	if r.CoverDir != "" {
		env["GOCOVERDIR"] = r.CoverDir
	}
	if sc.Stub != "" {
		profile, ok := stubProfiles[sc.Stub]
		require.True(t, ok, "unknown stub profile %q", sc.Stub)
		reg := &httpmock.Registry{}
		profile(reg)
		srv := httptest.NewTLSServer(reg)
		t.Cleanup(srv.Close)
		for _, k := range []string{"GH_TOKEN", "GITHUB_TOKEN", "GH_ENTERPRISE_TOKEN", "GITHUB_ENTERPRISE_TOKEN"} {
			delete(env, k)
		}
		env["GH_HOST"] = srv.Listener.Addr().String()
		env["GH_ENTERPRISE_TOKEN"] = stubToken
		env["GH_ACTIONS_LOCK_INSECURE"] = "1"
		env["GH_CONFIG_DIR"] = t.TempDir()
		env["GH_ACTIONS_LOCK_CONFIG"] = os.DevNull
	}
	for k, v := range sc.Env {
		if v == nil {
			delete(env, k)
		} else {
			env[k] = *v
		}
	}
	if len(sc.Input) > 0 {
		// CI suppresses prompts.
		delete(env, "CI")
	}

	res := r.exec(t, dir, env, sc.Flags, sc.Input)
	r.check(t, sc, dir, res)
	if len(sc.Expect.ThenRun) > 0 {
		then := r.exec(t, dir, env, sc.Expect.ThenRun, nil)
		assert.Equal(t, 0, then.exit, "then_run %v:\n%s%s", sc.Expect.ThenRun, then.stdout, then.output)
	}
}

func environ() map[string]string {
	env := map[string]string{}
	for _, kv := range os.Environ() {
		k, v, _ := strings.Cut(kv, "=")
		env[k] = v
	}
	return env
}

var liveToken = sync.OnceValue(func() string {
	if t := cmp.Or(os.Getenv("GH_TOKEN"), os.Getenv("GITHUB_TOKEN")); t != "" {
		return t
	}
	out, _ := exec.Command("gh", "auth", "token").Output()
	return strings.TrimSpace(string(out))
})

func writeRepo(t *testing.T, dir string, fx scenarios.Fixtures) {
	files := map[string]string{}
	for name, wf := range fx.Workflows {
		files[".github/workflows/"+name] = renderWorkflow(wf)
	}
	lock := fx.Lockfile
	if fx.LockfileTemplate != "" {
		var ok bool
		lock, ok = testfixture.LockfileTemplates()[fx.LockfileTemplate]
		require.True(t, ok, "unknown lockfile_template %q", fx.LockfileTemplate)
	}
	if lock != "" {
		files[lockPath] = lock
	}
	maps.Copy(files, fx.Files)
	for rel, body := range files {
		path := filepath.Join(dir, rel)
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(t, os.WriteFile(path, []byte(body), 0o644))
	}
	for _, args := range [][]string{
		{"init", "-q"},
		{"add", "-A"},
		{"-c", "user.email=test@test", "-c", "user.name=Test", "-c", "commit.gpgsign=false",
			"commit", "-q", "--allow-empty", "-m", "init"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_NOSYSTEM=1")
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, "git %v: %s", args, out)
	}
}

func renderWorkflow(wf scenarios.WorkflowFixture) string {
	if wf.Raw != "" {
		return wf.Raw
	}
	var b strings.Builder
	fmt.Fprintf(&b, "name: %s\non: workflow_dispatch\njobs:\n  build:\n    runs-on: ubuntu-latest\n    steps:\n", cmp.Or(wf.Name, "CI"))
	for _, a := range wf.Actions {
		fmt.Fprintf(&b, "      - uses: %s\n", a)
	}
	return b.String()
}

func (r *Runner) exec(t *testing.T, dir string, env map[string]string, args []string, input []scenarios.Prompt) result {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, r.Bin, args...)
	cmd.Dir = dir
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	if len(input) > 0 {
		return runPTY(t, cmd, input)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	return result{exit: exitCode(t, cmd.Run()), stdout: stdout.String(), output: stderr.String()}
}

func exitCode(t *testing.T, err error) int {
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	require.NoError(t, err)
	return 0
}

var ansiEscape = regexp.MustCompile(`\x1b\[[0-9;]*[a-zA-Z]`)

// runPTY plays the terminal: it answers cursor-position queries and types
// each response once its prompt has rendered and output has settled.
func runPTY(t *testing.T, cmd *exec.Cmd, prompts []scenarios.Prompt) result {
	f, err := pty.Start(cmd)
	require.NoError(t, err)
	defer f.Close()

	chunks := make(chan []byte)
	go func() {
		defer close(chunks)
		buf := make([]byte, 4096)
		for {
			n, err := f.Read(buf)
			if n > 0 {
				chunks <- bytes.Clone(buf[:n])
			}
			if err != nil {
				return
			}
		}
	}()

	var out strings.Builder
	var settled <-chan time.Time
	answered := 0
	for chunks != nil {
		select {
		case c, ok := <-chunks:
			if !ok {
				chunks = nil
				break
			}
			out.Write(c)
			if bytes.Contains(c, []byte("\x1b[6n")) {
				_, _ = f.WriteString("\x1b[1;1R")
			}
			if len(prompts) > 0 && strings.Contains(ansiEscape.ReplaceAllString(out.String()[answered:], ""), prompts[0].Prompt) {
				settled = time.After(300 * time.Millisecond)
			}
		case <-settled:
			_, _ = f.WriteString(prompts[0].Response + "\r")
			prompts, settled, answered = prompts[1:], nil, out.Len()
		}
	}
	all := out.String()
	return result{exit: exitCode(t, cmd.Wait()), stdout: all, output: all}
}

func (r *Runner) check(t *testing.T, sc scenarios.Scenario, dir string, res result) {
	e := sc.Expect
	if e.Exit != nil {
		assert.Equal(t, *e.Exit, res.exit, "exit code\nstdout:\n%s\noutput:\n%s", res.stdout, res.output)
	}
	for _, s := range e.OutputContains {
		assert.Contains(t, res.output, s)
	}
	for _, s := range e.OutputExcludes {
		assert.NotContains(t, res.output, s)
	}
	for s, n := range e.OutputCount {
		assert.Equal(t, n, strings.Count(res.output, s), "occurrences of %q in:\n%s", s, res.output)
	}
	for _, s := range e.StdoutContains {
		assert.Contains(t, res.stdout, s)
	}
	for _, s := range e.StdoutExcludes {
		assert.NotContains(t, res.stdout, s)
	}
	if e.StdoutIsJSON {
		assert.True(t, json.Valid([]byte(res.stdout)), "stdout is not JSON:\n%s", res.stdout)
	}

	lockBytes, lockErr := os.ReadFile(filepath.Join(dir, lockPath))
	lock := string(lockBytes)
	if e.LockfileExists {
		assert.NoError(t, lockErr, "lockfile exists")
	}
	for _, s := range e.LockfileContains {
		assert.Contains(t, lock, s, "lockfile")
	}
	for _, s := range e.LockfileExcludes {
		assert.NotContains(t, lock, s, "lockfile")
	}
	if e.LockfileCommentMatches != "" {
		assert.Regexp(t, "(?m)"+e.LockfileCommentMatches, lock, "lockfile")
	}
	if e.LockfileCommentExcludes != "" {
		assert.NotRegexp(t, "(?m)"+e.LockfileCommentExcludes, lock, "lockfile")
	}
	if e.LockfileDepsCoverDirect || e.LockfileDepsCoverIndirect {
		checkDeps(t, dir, lockBytes, e.LockfileDepsCoverDirect, e.LockfileDepsCoverIndirect)
	}
	for rel, subs := range e.FilesContain {
		body, _ := os.ReadFile(filepath.Join(dir, rel))
		for _, s := range subs {
			assert.Contains(t, string(body), s, rel)
		}
	}
	for rel, subs := range e.FilesExclude {
		body, _ := os.ReadFile(filepath.Join(dir, rel))
		for _, s := range subs {
			assert.NotContains(t, string(body), s, rel)
		}
	}
	for _, c := range e.JQ {
		checkJQ(t, res.stdout, c)
	}
	if e.GoldenJSON != nil {
		r.checkGolden(t, sc.Name, e.GoldenJSON, res.stdout)
	}
}

// checkDeps asserts the lockfile is closed over its references: direct
// workflow entries are dependencies that the workflow file still uses, and
// every dependency's own uses are dependencies.
func checkDeps(t *testing.T, dir string, lock []byte, direct, indirect bool) {
	var lf struct {
		Workflows    map[string][]string `yaml:"workflows"`
		Dependencies map[string]struct {
			Uses []string `yaml:"uses"`
		} `yaml:"dependencies"`
	}
	require.NoError(t, yaml.Unmarshal(lock, &lf), "parse lockfile:\n%s", lock)
	require.NotNil(t, lf.Dependencies, "lockfile has no dependencies:\n%s", lock)
	if direct {
		require.NotNil(t, lf.Workflows, "lockfile has no workflows:\n%s", lock)
		for wf, refs := range lf.Workflows {
			src, err := os.ReadFile(filepath.Join(dir, wf))
			for _, ref := range refs {
				assert.Contains(t, lf.Dependencies, ref, "direct dep from %s", wf)
				nwo, tag, ok := strings.Cut(ref, "@")
				if err != nil || !ok {
					continue
				}
				uses := `uses:\s*` + regexp.QuoteMeta(nwo) + `(?:/[^@]+)?@` + regexp.QuoteMeta(tag)
				assert.Regexp(t, uses, string(src), "%s no longer uses %s", wf, ref)
			}
		}
	}
	if indirect {
		for pin, dep := range lf.Dependencies {
			for _, u := range dep.Uses {
				assert.Contains(t, lf.Dependencies, u, "indirect dep used by %s", pin)
			}
		}
	}
}

func checkJQ(t *testing.T, stdout string, c scenarios.JQCheck) {
	var out bytes.Buffer
	if err := jq.Evaluate(strings.NewReader(stdout), &out, c.Expr); err != nil {
		assert.Fail(t, "jq "+c.Expr, "%v\nstdout:\n%s", err, stdout)
		return
	}
	got := strings.TrimSpace(out.String())
	if c.Equals != nil {
		assert.Equal(t, *c.Equals, got, "jq %s", c.Expr)
	}
	if c.Contains != nil {
		assert.Contains(t, got, *c.Contains, "jq %s", c.Expr)
	}
	if c.NotEquals != nil {
		assert.NotEqual(t, *c.NotEquals, got, "jq %s", c.Expr)
	}
	if c.Matches != nil {
		assert.Regexp(t, *c.Matches, got, "jq %s", c.Expr)
	}
	if c.GreaterThan != nil {
		n, err := strconv.ParseFloat(got, 64)
		if assert.NoError(t, err, "jq %s", c.Expr) {
			assert.Greater(t, n, *c.GreaterThan, "jq %s", c.Expr)
		}
	}
}

// checkGolden compares --json stdout to the catalog body. cli_version is a
// build stamp, so it is ignored.
func (r *Runner) checkGolden(t *testing.T, name string, want map[string]any, stdout string) {
	var got map[string]any
	if !assert.NoError(t, json.Unmarshal([]byte(stdout), &got), "golden_json: stdout is not JSON:\n%s", stdout) {
		return
	}
	if r.Update {
		require.NoError(t, r.updateGolden(name, got))
		return
	}
	// Round-trip so numbers decode the same way on both sides.
	b, err := json.Marshal(want)
	require.NoError(t, err)
	var w map[string]any
	require.NoError(t, json.Unmarshal(b, &w))
	delete(w, "cli_version")
	delete(got, "cli_version")
	assert.Equal(t, w, got, "golden_json (refresh with -update)")
}

// updateGolden replaces the scenario's golden_json block in catalog.yml,
// leaving the rest of the file untouched.
func (r *Runner) updateGolden(name string, got map[string]any) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	path := scenarios.Path()
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return err
	}
	var key *yaml.Node
	for _, sc := range mapValue(doc.Content[0], "scenarios").Content {
		if n := mapValue(sc, "name"); n != nil && n.Value == name {
			key = mapKey(mapValue(sc, "expect"), "golden_json")
		}
	}
	if key == nil {
		return fmt.Errorf("scenario %s has no golden_json", name)
	}

	lines := strings.Split(string(data), "\n")
	start, indent := key.Line-1, key.Column-1
	end := start + 1
	for end < len(lines) && (strings.TrimSpace(lines[end]) == "" || len(lines[end])-len(strings.TrimLeft(lines[end], " ")) > indent) {
		end++
	}
	for end > start+1 && strings.TrimSpace(lines[end-1]) == "" {
		end--
	}
	var body bytes.Buffer
	enc := yaml.NewEncoder(&body)
	enc.SetIndent(2)
	if err := enc.Encode(got); err != nil {
		return err
	}
	block := []string{lines[start]}
	for _, l := range strings.Split(strings.TrimRight(body.String(), "\n"), "\n") {
		block = append(block, strings.Repeat(" ", indent+2)+l)
	}
	lines = slices.Replace(lines, start, end, block...)
	return os.WriteFile(path, []byte(strings.Join(lines, "\n")), 0o644)
}

func mapKey(n *yaml.Node, key string) *yaml.Node {
	if n == nil || n.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		if n.Content[i].Value == key {
			return n.Content[i]
		}
	}
	return nil
}

func mapValue(n *yaml.Node, key string) *yaml.Node {
	k := mapKey(n, key)
	if k == nil {
		return nil
	}
	return n.Content[slices.Index(n.Content, k)+1]
}

// Stub profiles are the fake GitHub APIs a `stub:` scenario can name.
// Unmatched requests get a 404.

var (
	mainSHA     = testfixture.Synthetic('a')
	checkout    = testfixture.CheckoutV4
	ssoResponse = reply(http.StatusForbidden, map[string]string{
		"message":           "Resource protected by organization SAML enforcement. You must grant your OAuth token access to this organization.",
		"documentation_url": "https://docs.github.com/articles/authenticating-to-a-github-organization-with-saml-single-sign-on/",
	}, "X-GitHub-SSO", "required; url=https://github.com/orgs/actions/sso?authorization_request=ABCDEF123")
)

const (
	fullManifest  = "name: Checkout\ndescription: Checkout\nruns:\n  using: node20\n  main: dist/index.js\n"
	shortManifest = "name: Checkout\ndescription: Checkout\n"
)

var stubProfiles = map[string]func(*httpmock.Registry){
	"sso_403": func(reg *httpmock.Registry) {
		reg.RegisterReusable(anyRequest, ssoResponse)
	},
	"not_found":    errorAll(http.StatusNotFound, "Not Found"),
	"server_error": errorAll(http.StatusInternalServerError, "Internal Server Error"),
	"unauthorized": errorAll(http.StatusUnauthorized, "Bad credentials"),
	"rate_limited": func(reg *httpmock.Registry) {
		reg.RegisterReusable(anyRequest, func(req *http.Request) (*http.Response, error) {
			reset := strconv.FormatInt(time.Now().Add(2*time.Second).Unix(), 10)
			return reply(http.StatusTooManyRequests, map[string]string{"message": "API rate limit exceeded"},
				"Retry-After", "1", "X-RateLimit-Remaining", "0", "X-RateLimit-Reset", reset)(req)
		})
	},
	// SSO on actions/*; any other repository is missing.
	"mixed": func(reg *httpmock.Registry) {
		reg.RegisterReusable(httpmock.REST("POST", ".*"), ssoResponse)
		reg.RegisterReusable(httpmock.REST("GET", "/repos/actions/"), ssoResponse)
	},
	"checkout": func(reg *httpmock.Registry) {
		checkoutAPI(reg, checkout.SHA, fullManifest, false)
		get(reg, `/repos/actions/checkout/releases`, reply(http.StatusOK, releases("v4.2.2", "2024-01-01T00:00:00Z")))
	},
	// v4 was released a day ago, inside the fresh-tag window.
	"checkout_fresh": func(reg *httpmock.Registry) {
		checkoutAPI(reg, checkout.SHA, fullManifest, false)
		published := time.Now().Add(-24 * time.Hour).UTC().Format(time.RFC3339)
		get(reg, `/repos/actions/checkout/releases`, reply(http.StatusOK, releases("v4", published)))
	},
	// The locked repository ID now answers to a new name.
	"renamed": func(reg *httpmock.Registry) {
		reg.RegisterReusable(httpmock.REST("POST", `/graphql$`), parity("actions/checkout-renamed"))
	},
	// v4 moved to a commit whose history lacks the locked one.
	"forgery": func(reg *httpmock.Registry) {
		checkoutAPI(reg, testfixture.Synthetic('b'), shortManifest, true)
		get(reg, `/repos/actions/checkout/compare/`, reply(http.StatusOK, map[string]any{
			"status": "diverged", "merge_base_commit": map[string]string{"sha": testfixture.Synthetic('c')},
		}))
	},
	// main moved ahead of the locked commit.
	"relock": func(reg *httpmock.Registry) {
		checkoutAPI(reg, mainSHA, shortManifest, true)
		get(reg, `/repos/actions/checkout/compare/`, reply(http.StatusOK, map[string]any{
			"status": "behind", "merge_base_commit": map[string]string{"sha": checkout.SHA},
		}))
	},
	// REST only: a GraphQL POST gets a 404 and fails the run.
	"dependabot_proxy": func(reg *httpmock.Registry) {
		get(reg, `/git/ref/tags/v4$`, reply(http.StatusOK, httpmock.GitRefResponse("refs/tags/v4", mainSHA)))
		get(reg, `/commits/`, reply(http.StatusOK, map[string]string{"sha": mainSHA}))
		get(reg, `/contents/`, reply(http.StatusOK,
			"name: 'Checkout'\ndescription: 'Checkout'\ninputs: {}\nruns:\n  using: node20\n  main: dist/index.js\n",
			"Content-Type", "text/plain"))
		get(reg, `/branches`, reply(http.StatusOK, httpmock.BranchListResponse("main", mainSHA)))
		get(reg, `/tags`, reply(http.StatusOK, httpmock.TagListResponse("v4", mainSHA)))
		get(reg, `/repos/actions/checkout$`, reply(http.StatusOK, map[string]any{
			"default_branch": "main", "visibility": "private", "pushed_at": "2024-01-01T00:00:00Z",
			"id": 1, "owner": map[string]any{"id": checkout.OwnerID},
		}))
		get(reg, `/compare/`, reply(http.StatusOK, map[string]any{
			"status": "behind", "merge_base_commit": map[string]string{"sha": mainSHA},
		}))
		get(reg, `/releases`, reply(http.StatusOK, releases("v4", "2024-01-01T00:00:00Z")))
	},
}

func anyRequest(*http.Request) bool { return true }

func get(reg *httpmock.Registry, path string, resp httpmock.Responder) {
	reg.RegisterReusable(httpmock.REST("GET", path), resp)
}

// reply responds with status, body (JSON-encoded unless a string), and
// header name/value pairs.
func reply(status int, body any, header ...string) httpmock.Responder {
	return func(req *http.Request) (*http.Response, error) {
		s, ok := body.(string)
		if !ok {
			b, err := json.Marshal(body)
			if err != nil {
				return nil, err
			}
			s = string(b)
		}
		h := http.Header{"Content-Type": {"application/json"}}
		for i := 0; i+1 < len(header); i += 2 {
			h.Set(header[i], header[i+1])
		}
		return &http.Response{StatusCode: status, Header: h, Body: io.NopCloser(strings.NewReader(s)), Request: req}, nil
	}
}

func errorAll(status int, message string) func(*httpmock.Registry) {
	return func(reg *httpmock.Registry) {
		reg.RegisterReusable(anyRequest, reply(status, map[string]string{"message": message}))
	}
}

func releases(tag, published string) []map[string]any {
	return []map[string]any{{"tag_name": tag, "published_at": published, "immutable": false}}
}

// checkoutAPI serves actions/checkout with its refs resolving to live. With
// compare, GraphQL ancestry checks report the locked commit as behind.
func checkoutAPI(reg *httpmock.Registry, live, manifest string, compare bool) {
	reg.RegisterReusable(httpmock.REST("POST", `/graphql$`), func(req *http.Request) (*http.Response, error) {
		raw, err := io.ReadAll(req.Body)
		if err != nil {
			return nil, err
		}
		req.Body = io.NopCloser(bytes.NewReader(raw))
		var body struct {
			Query string `json:"query"`
		}
		_ = json.Unmarshal(raw, &body)
		q := body.Query
		switch {
		case strings.Contains(q, "commit: object(oid"):
			return parity(checkout.Repo)(req)
		case strings.Contains(q, "expression"):
			return reply(http.StatusOK, map[string]any{"data": map[string]any{"a0": map[string]any{
				"nameWithOwner": checkout.Repo,
				"object":        map[string]any{"oid": live, "file": map[string]any{"object": map[string]any{"text": manifest}}},
			}}})(req)
		case compare && strings.Contains(q, "compare"):
			repo := map[string]any{}
			for _, m := range regexp.MustCompile(`b(\d+):`).FindAllStringSubmatch(q, -1) {
				repo["b"+m[1]] = map[string]any{"compare": map[string]string{"status": "BEHIND"}}
			}
			return reply(http.StatusOK, map[string]any{"data": map[string]any{"repo": repo}})(req)
		}
		return reply(http.StatusOK, `{"data":{}}`)(req)
	})
	get(reg, `/repos/actions/checkout$`, reply(http.StatusOK, map[string]any{
		"default_branch": "main", "visibility": "public", "pushed_at": "2024-01-01T00:00:00Z",
		"id": checkout.RepoID, "owner": map[string]any{"id": checkout.OwnerID},
	}))
	get(reg, `/repos/actions/checkout/tags`, reply(http.StatusOK, httpmock.TagListResponse(
		"v4", checkout.SHA, "v4.2.2", checkout.SHA,
		"v4.2.1", testfixture.Synthetic('1'), "v3", testfixture.Synthetic('2'))))
	get(reg, `/repos/actions/checkout/git/ref/heads/main$`, reply(http.StatusOK, map[string]any{
		"ref": "refs/heads/main", "object": map[string]string{"sha": mainSHA, "type": "commit"},
	}))
	get(reg, `/repos/actions/checkout/branches`, reply(http.StatusOK, httpmock.BranchListResponseProtected("main", mainSHA, false)))
	get(reg, `/repos/actions/checkout/git/matching-refs/`, reply(http.StatusOK, "[]"))
}

// parity answers the runner-parity query: the repository resolves as nwo
// under the locked repository ID, and every requested object exists.
func parity(nwo string) httpmock.Responder {
	return func(req *http.Request) (*http.Response, error) {
		var body struct {
			Variables map[string]any `json:"variables"`
		}
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			return nil, err
		}
		data := map[string]any{}
		for k, v := range body.Variables {
			i, ok := strings.CutPrefix(k, "oid")
			if _, err := strconv.Atoi(i); !ok || err != nil {
				continue
			}
			oid := map[string]any{"oid": v}
			data["a"+i] = map[string]any{"nameWithOwner": nwo, "databaseId": checkout.RepoID,
				"commit": oid, "tag": oid, "head": oid, "tagRef": map[string]any{}, "headRef": map[string]any{}}
		}
		return reply(http.StatusOK, map[string]any{"data": data})(req)
	}
}
