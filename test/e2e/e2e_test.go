package e2e

import (
	"flag"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/github/gh-actions-lock/test/scenarios"
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
