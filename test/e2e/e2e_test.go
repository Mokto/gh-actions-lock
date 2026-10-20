package e2e

import (
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/github/gh-actions-lock/test/scenarios"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Environment:
//
//	E2E_LIVE=1        also run scenarios that need real GitHub credentials
//	E2E_TAGS=a,b      run only scenarios with one of these tags; "live"
//	                  selects every live scenario
//	E2E_COVERDIR=dir  build the binary with -cover and collect coverage
var update = flag.Bool("update", false, "rewrite golden_json in catalog.yml from actual output")

var runner *Runner

func TestMain(m *testing.M) {
	flag.Parse()
	dir, err := os.MkdirTemp("", "gh-actions-lock-e2e")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	coverDir := os.Getenv("E2E_COVERDIR")
	if coverDir != "" {
		if coverDir, err = CoverDir(coverDir); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	}
	bin, err := Build(dir, coverDir != "")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	runner = &Runner{Bin: bin, CoverDir: coverDir, Update: *update}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

func TestScenarios(t *testing.T) {
	cat, err := scenarios.Load()
	require.NoError(t, err)
	live := os.Getenv("E2E_LIVE") == "1"
	tags := strings.FieldsFunc(os.Getenv("E2E_TAGS"), func(r rune) bool { return r == ',' || r == ' ' })

	for _, sc := range cat.Scenarios {
		if !selected(sc, tags) {
			continue
		}
		t.Run(sc.Name, func(t *testing.T) {
			if sc.Live() {
				if !live {
					t.Skip("needs real GitHub; set E2E_LIVE=1")
				}
			} else {
				t.Parallel()
			}
			runner.Run(t, sc)
		})
	}
}

func selected(sc scenarios.Scenario, tags []string) bool {
	if len(tags) == 0 {
		return true
	}
	for _, tag := range tags {
		if sc.HasTag(tag) || (tag == "live" && sc.Live()) {
			return true
		}
	}
	return false
}

// TestScenarioStateStaysInScenarioHome: the binary writes a run log under
// the user cache dir on every run and reads a user config file. Both must
// resolve inside the scenario's own home, never the developer's.
func TestScenarioStateStaysInScenarioHome(t *testing.T) {
	cat, err := scenarios.Load()
	require.NoError(t, err)
	i := slices.IndexFunc(cat.Scenarios, func(s scenarios.Scenario) bool { return s.Name == "api_404_not_found" })
	require.GreaterOrEqual(t, i, 0)
	sc := cat.Scenarios[i]

	env, home := runner.scenarioEnv(t, sc)
	assert.Equal(t, os.DevNull, env["GH_ACTIONS_LOCK_CONFIG"])
	for _, k := range []string{"HOME", "XDG_CACHE_HOME", "XDG_CONFIG_HOME", "GH_CONFIG_DIR"} {
		assert.True(t, strings.HasPrefix(env[k], home), "%s=%q is outside the scenario home", k, env[k])
	}

	dir := t.TempDir()
	writeRepo(t, dir, sc.Fixtures)
	res := runner.exec(t, dir, env, sc.Flags, nil)
	require.NotZero(t, res.exit, "the scenario should fail and log it")

	var logs []string
	require.NoError(t, filepath.WalkDir(home, func(path string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && filepath.Base(filepath.Dir(filepath.Dir(path))) == "gh-actions-lock" {
			logs = append(logs, path)
		}
		return err
	}))
	assert.NotEmpty(t, logs, "run log was not written under the scenario home")
}

func TestUsesRef(t *testing.T) {
	tests := []struct {
		name string
		src  string
		nwo  string
		ref  string
		want bool
	}{
		{name: "exact step", src: "    - uses: actions/checkout@v4\n", nwo: "actions/checkout", ref: "v4", want: true},
		{name: "job-level reusable workflow", src: "    uses: octo/wf/.github/workflows/ci.yml@main\n", nwo: "octo/wf", ref: "main", want: true},
		{name: "subpath", src: "  - uses: actions/cache/save@v4\n", nwo: "actions/cache", ref: "v4", want: true},
		{name: "quoted with comment", src: "  - uses: 'actions/checkout@v4' # pinned\n", nwo: "actions/checkout", ref: "v4", want: true},
		{name: "owner and repo fold case", src: "  - uses: Actions/Checkout@v4\n", nwo: "actions/checkout", ref: "v4", want: true},
		{name: "longer ref is not a match", src: "  - uses: actions/checkout@v4.2.2\n", nwo: "actions/checkout", ref: "v4", want: false},
		{name: "ref case is significant", src: "  - uses: actions/checkout@Main\n", nwo: "actions/checkout", ref: "main", want: false},
		{name: "longer repository name is not a match", src: "  - uses: actions/checkout-extra@v4\n", nwo: "actions/checkout", ref: "v4", want: false},
		{name: "commented out", src: "  # - uses: actions/checkout@v4\n", nwo: "actions/checkout", ref: "v4", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, usesRef(tt.src, tt.nwo, tt.ref))
		})
	}
}

// TestCoverDirIsAbsolute: scenarios run from their own repository, so a
// relative coverage directory must be resolved before it reaches GOCOVERDIR.
func TestCoverDirIsAbsolute(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	got, err := CoverDir("cov")
	require.NoError(t, err)
	assert.True(t, filepath.IsAbs(got), got)
	assert.DirExists(t, got)
	want, err := filepath.EvalSymlinks(filepath.Join(root, "cov"))
	require.NoError(t, err)
	resolved, err := filepath.EvalSymlinks(got)
	require.NoError(t, err)
	assert.Equal(t, want, resolved)
}

func TestRemoteUses(t *testing.T) {
	src := `jobs:
  build:
    steps:
      - uses: actions/checkout@v4
      - uses: 'actions/cache/save@v4' # subpath
      - uses: ./local/action
      - uses: $/tools/act
      - uses: docker://alpine:3
      - uses: ${{ matrix.action }}
  call:
    uses: octo/wf/.github/workflows/ci.yml@main
`
	assert.Equal(t, []string{"actions/checkout@v4", "actions/cache@v4"}, remoteUses(src))
}

// TestScenarioEnvDropsAmbientHost: a developer's GH_HOST must not redirect
// live scenarios away from github.com.
func TestScenarioEnvDropsAmbientHost(t *testing.T) {
	t.Setenv("GH_HOST", "example.ghe.com")
	t.Setenv("GH_ENTERPRISE_TOKEN", "ambient")
	t.Setenv("GH_TOKEN", "live-token")
	env, _ := runner.scenarioEnv(t, scenarios.Scenario{Name: "live", NeedsToken: true})
	assert.NotContains(t, env, "GH_HOST")
	assert.NotContains(t, env, "GH_ENTERPRISE_TOKEN")
}
