// Package scenarios loads the end-to-end scenario catalog that test/e2e
// runs against the compiled binary.
package scenarios

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"

	"gopkg.in/yaml.v3"
)

// Catalog is the top-level structure of catalog.yml.
type Catalog struct {
	Version    int        `yaml:"version"`
	Categories []Category `yaml:"categories"`
	Scenarios  []Scenario `yaml:"scenarios"`
}

// Category groups related scenarios.
type Category struct {
	Name        string `yaml:"name"`
	Description string `yaml:"description"`
}

// Scenario describes a single test scenario.
type Scenario struct {
	Name        string   `yaml:"name"`
	Category    string   `yaml:"category"`
	Description string   `yaml:"description"`
	NeedsToken  bool     `yaml:"needs_token"`
	Tags        []string `yaml:"tags"`
	Flags       []string `yaml:"flags"`
	LiveRepo    string   `yaml:"live_repo"`
	// Stub names the fake GitHub profile (test/e2e) the run talks to.
	Stub string `yaml:"stub"`
	// Input answers interactive prompts; the run gets a pseudo-terminal.
	Input []Prompt `yaml:"input"`

	// Env sets or unsets process environment variables for the run. A nil
	// value unsets the variable (YAML null), letting scenarios strip ambient
	// auth or redirect GH_HOST to prove offline behavior.
	Env map[string]*string `yaml:"env,omitempty"`

	Fixtures Fixtures `yaml:"fixtures"`
	Expect   Expect   `yaml:"expect"`
}

// Prompt is a response typed once Prompt text appears on the terminal.
type Prompt struct {
	Prompt   string `yaml:"prompt"`
	Response string `yaml:"response"`
}

// Fixtures describes the file-system setup for a scenario.
type Fixtures struct {
	Workflows        map[string]WorkflowFixture `yaml:"workflows"`
	Lockfile         string                     `yaml:"lockfile"`
	LockfileTemplate string                     `yaml:"lockfile_template"`
	// DeleteLockfile removes any lockfile before the run, e.g. one checked
	// into a cloned live_repo.
	DeleteLockfile bool `yaml:"delete_lockfile"`
	// Files lays down arbitrary repo-relative files before the run, e.g.
	// in-repo composite action.yml definitions that the default-on migration
	// should also rewrite. Paths are relative to the repo root, not the
	// workflows dir.
	Files map[string]string `yaml:"files"`
}

// WorkflowFixture is either a structured action list or raw YAML.
type WorkflowFixture struct {
	Name    string   `yaml:"name"`
	Actions []string `yaml:"actions"`
	Raw     string   `yaml:"raw"`
}

// JQCheck is a single jq-based assertion on JSON stdout. Results compare as
// `jq -r` output; a nil field is not checked.
type JQCheck struct {
	Expr        string   `yaml:"expr"`
	Equals      *string  `yaml:"equals,omitempty"`
	Contains    *string  `yaml:"contains,omitempty"`
	NotEquals   *string  `yaml:"not_equals,omitempty"`
	Matches     *string  `yaml:"matches,omitempty"`
	GreaterThan *float64 `yaml:"greater_than,omitempty"`
}

// Expect declares assertions on the scenario outcome. Output is stderr,
// where the CLI writes its UI; under Input it is the merged terminal stream.
type Expect struct {
	// Exit is the exact exit code.
	Exit           *int     `yaml:"exit"`
	OutputContains []string `yaml:"output_contains"`
	OutputExcludes []string `yaml:"output_excludes"`
	// OutputCount asserts how many times each substring appears in output.
	OutputCount    map[string]int `yaml:"output_count,omitempty"`
	StdoutContains []string       `yaml:"stdout_contains"`
	StdoutExcludes []string       `yaml:"stdout_excludes,omitempty"`
	StdoutIsJSON   bool           `yaml:"stdout_is_json"`
	LockfileExists bool           `yaml:"lockfile_exists"`
	// LockfileContains / LockfileExcludes assert substrings of the lockfile
	// after the run, e.g. that `$/` self refs are never recorded.
	LockfileContains []string `yaml:"lockfile_contains,omitempty"`
	LockfileExcludes []string `yaml:"lockfile_excludes,omitempty"`
	// LockfileCommentMatches / LockfileCommentExcludes are multi-line
	// regular expressions over the whole lockfile.
	LockfileCommentMatches  string `yaml:"lockfile_comment_matches,omitempty"`
	LockfileCommentExcludes string `yaml:"lockfile_comment_excludes,omitempty"`
	// LockfileDepsCoverDirect asserts every workflow entry has a dependency
	// and is still a `uses:` in its workflow file.
	LockfileDepsCoverDirect bool `yaml:"lockfile_deps_cover_direct,omitempty"`
	// LockfileDepsCoverIndirect asserts every dependency's `uses:` entry is
	// itself a dependency.
	LockfileDepsCoverIndirect bool `yaml:"lockfile_deps_cover_indirect,omitempty"`
	// FilesContain / FilesExclude assert substrings in repo-relative files
	// after the run, used to verify `./`→`$/` rewrites in workflows and
	// in-repo composite action.yml files.
	FilesContain map[string][]string `yaml:"files_contain,omitempty"`
	FilesExclude map[string][]string `yaml:"files_exclude,omitempty"`
	JQ           []JQCheck           `yaml:"jq,omitempty"`
	// GoldenJSON is the exact --json stdout, ignoring cli_version. Refresh
	// it with `go test ./test/e2e -update`.
	GoldenJSON map[string]any `yaml:"golden_json,omitempty"`
	// ThenRun reruns the binary with these args in the same repo and
	// expects exit 0, e.g. to prove the written lockfile verifies.
	ThenRun []string `yaml:"then_run,omitempty"`
}

// Live reports whether the scenario needs real GitHub credentials.
func (s *Scenario) Live() bool { return s.NeedsToken || s.LiveRepo != "" }

// HasTag reports whether the scenario has the given tag.
func (s *Scenario) HasTag(tag string) bool {
	for _, t := range s.Tags {
		if t == tag {
			return true
		}
	}
	return false
}

// Path returns the location of catalog.yml in the source tree.
func Path() string {
	_, thisFile, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(thisFile), "catalog.yml")
}

// Load reads the catalog at Path.
func Load() (*Catalog, error) { return LoadFrom(Path()) }

// LoadFrom reads and parses a catalog from the given path.
func LoadFrom(path string) (*Catalog, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Catalog
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil {
		return nil, err
	}
	return &c, nil
}

// ByCategory returns scenarios matching the given category name.
func (c *Catalog) ByCategory(cat string) []Scenario {
	var out []Scenario
	for _, s := range c.Scenarios {
		if s.Category == cat {
			out = append(out, s)
		}
	}
	return out
}

// ByTag returns scenarios having the given tag.
func (c *Catalog) ByTag(tag string) []Scenario {
	var out []Scenario
	for _, s := range c.Scenarios {
		if s.HasTag(tag) {
			out = append(out, s)
		}
	}
	return out
}

// Names returns all scenario names.
func (c *Catalog) Names() []string {
	names := make([]string, len(c.Scenarios))
	for i, s := range c.Scenarios {
		names[i] = s.Name
	}
	return names
}
