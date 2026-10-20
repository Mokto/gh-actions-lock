package scenarios

import (
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"strings"
	"testing"

	"github.com/github/gh-actions-lock/internal/testfixture"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoad(t *testing.T) {
	cat, err := Load()
	require.NoError(t, err)
	assert.Greater(t, len(cat.Scenarios), 20, "expected at least 20 scenarios")
	assert.Greater(t, len(cat.Categories), 5, "expected at least 5 categories")
}

func TestCatalogNames(t *testing.T) {
	cat, err := Load()
	require.NoError(t, err)

	names := cat.Names()
	assert.Greater(t, len(names), 0)

	seen := map[string]bool{}
	for _, n := range names {
		assert.NotEmpty(t, n, "scenario name must not be empty")
		assert.False(t, seen[n], "duplicate scenario name: %s", n)
		seen[n] = true
	}
}

func TestCatalogByCategory(t *testing.T) {
	cat, err := Load()
	require.NoError(t, err)

	for _, c := range cat.Categories {
		scenarios := cat.ByCategory(c.Name)
		assert.NotEmpty(t, scenarios, "category %q has no scenarios", c.Name)
	}
}

func TestCatalogByTag(t *testing.T) {
	cat, err := Load()
	require.NoError(t, err)

	smoke := cat.ByTag("smoke")
	assert.Greater(t, len(smoke), 0, "expected at least one 'smoke' scenario")

	stub := cat.ByTag("stub")
	assert.Greater(t, len(stub), 0, "expected at least one 'stub' scenario")
}

func TestEveryScenarioHasCategory(t *testing.T) {
	cat, err := Load()
	require.NoError(t, err)

	validCats := map[string]bool{}
	for _, c := range cat.Categories {
		validCats[c.Name] = true
	}

	for _, s := range cat.Scenarios {
		assert.True(t, validCats[s.Category],
			"scenario %q references unknown category %q", s.Name, s.Category)
	}
}

func TestEveryScenarioHasExpectation(t *testing.T) {
	cat, err := Load()
	require.NoError(t, err)

	for _, s := range cat.Scenarios {
		assert.False(t, reflect.ValueOf(s.Expect).IsZero(),
			"scenario %q has no expectations defined", s.Name)
	}
}

// TestEveryScenarioRunsInCI keeps each scenario in exactly one CI lane:
// hermetic `stub` scenarios guard merge from `go test ./...`, live ones run
// in the non-blocking integration-live job.
func TestEveryScenarioRunsInCI(t *testing.T) {
	cat, err := Load()
	require.NoError(t, err)

	for _, s := range cat.Scenarios {
		assert.NotEqual(t, s.Live(), s.HasTag("stub"),
			"scenario %q must be tagged stub or need a token, not both or neither", s.Name)
	}
}

func TestLockfileTemplatesExist(t *testing.T) {
	cat, err := Load()
	require.NoError(t, err)

	templates := testfixture.LockfileTemplates()
	for _, s := range cat.Scenarios {
		if name := s.Fixtures.LockfileTemplate; name != "" {
			assert.Contains(t, templates, name, "scenario %q", s.Name)
		}
	}
}

func TestLiveScenariosRequireToken(t *testing.T) {
	cat, err := Load()
	require.NoError(t, err)

	for _, s := range cat.Scenarios {
		if s.HasTag("real_repo") {
			assert.True(t, s.NeedsToken,
				"live repo scenario %q must set needs_token: true", s.Name)
			assert.NotEmpty(t, s.LiveRepo,
				"live repo scenario %q must set live_repo", s.Name)
		}
	}
}

func TestScenarioMatrix(t *testing.T) {
	cat, err := Load()
	require.NoError(t, err)

	t.Logf("Scenario matrix: %d scenarios across %d categories",
		len(cat.Scenarios), len(cat.Categories))

	for _, c := range cat.Categories {
		scenarios := cat.ByCategory(c.Name)
		t.Logf("  %-20s %d scenarios  %s", c.Name, len(scenarios), c.Description)
	}
}

// TestCatalogSHAsAreNamedPins keeps every real-looking catalog commit tied
// to a testfixture.Pin, which the fixture guard checks against the live API.
// A commit must match a pin exactly: by repository where the catalog names
// one (`owner/repo@sha`), and by repository and ref in expected JSON
// (`nwo`/`ref`/`sha`). Synthetic SHAs ("aaaa…") only ever meet the stub.
func TestCatalogSHAsAreNamedPins(t *testing.T) {
	_, thisFile, _, _ := runtime.Caller(0)
	data, err := os.ReadFile(filepath.Join(filepath.Dir(thisFile), "catalog.yml"))
	require.NoError(t, err)

	for _, sha := range regexp.MustCompile(`\b[0-9a-f]{40}\b`).FindAllString(string(data), -1) {
		if !testfixture.IsSynthetic(sha) {
			assert.NotEmpty(t, pinsMatching("", "", sha), "catalog SHA %s is not a testfixture pin", sha)
		}
	}
	for _, m := range regexp.MustCompile(`([\w.-]+/[\w.-]+)@([0-9a-f]{40})\b`).FindAllStringSubmatch(string(data), -1) {
		if !testfixture.IsSynthetic(m[2]) {
			assert.NotEmpty(t, pinsMatching(m[1], "", m[2]), "%s is not a pin of %s", m[2], m[1])
		}
	}

	cat, err := Load()
	require.NoError(t, err)
	for _, sc := range cat.Scenarios {
		walkTriples(sc.Expect.GoldenJSON, func(nwo, ref, sha string) {
			if !testfixture.IsSynthetic(sha) {
				assert.NotEmpty(t, pinsMatching(nwo, ref, sha), "%s: %s@%s=%s is not a testfixture pin", sc.Name, nwo, ref, sha)
			}
		})
	}
}

// pinsMatching returns pins with sha whose repository (case-insensitive)
// and ref (exact) match when given.
func pinsMatching(repo, ref, sha string) []testfixture.Pin {
	var out []testfixture.Pin
	for _, p := range testfixture.Pins() {
		if p.SHA == sha && (repo == "" || strings.EqualFold(p.Repo, repo)) && (ref == "" || p.Ref == ref) {
			out = append(out, p)
		}
	}
	return out
}

// walkTriples calls fn for every object in v carrying nwo, ref and sha.
func walkTriples(v any, fn func(nwo, ref, sha string)) {
	switch v := v.(type) {
	case map[string]any:
		nwo, _ := v["nwo"].(string)
		ref, _ := v["ref"].(string)
		sha, _ := v["sha"].(string)
		if nwo != "" && sha != "" {
			fn(nwo, ref, sha)
		}
		for _, child := range v {
			walkTriples(child, fn)
		}
	case []any:
		for _, child := range v {
			walkTriples(child, fn)
		}
	}
}

func TestPinsMatchingIsExact(t *testing.T) {
	sha := testfixture.CheckoutV4.SHA
	assert.NotEmpty(t, pinsMatching("Actions/Checkout", "v4", sha), "owner/repo fold case")
	assert.Empty(t, pinsMatching("actions/checkout", "v4.2.2", sha), "a ref must match exactly")
	assert.Empty(t, pinsMatching("actions/checkout", "V4", sha), "refs are case-sensitive")
	assert.Empty(t, pinsMatching("actions/setup-go", "", sha), "a commit belongs to one repository")
	assert.Empty(t, pinsMatching("actions/checkout", "", sha[:39]+"0"), "a SHA must match exactly")
}
