package testfixture

import (
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"

	"github.com/cli/go-gh/v2/pkg/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// guardEnv opts in to the networked fixture guard so `make test` stays
// hermetic.
const guardEnv = "GH_ACTIONS_LOCK_FIXTURE_GUARD"

// TestPinsAreInRecordedRefHistory proves each pin's commit is contained in
// its recorded ref and that the recorded repository identity is right. A
// v6 checkout commit once sat under @v4 in the live fixtures unnoticed.
func TestPinsAreInRecordedRefHistory(t *testing.T) {
	if os.Getenv(guardEnv) != "1" {
		t.Skipf("set %s=1 to check fixtures against the live API", guardEnv)
	}
	client, err := api.DefaultRESTClient()
	require.NoError(t, err, "fixture guard needs GitHub auth (GH_TOKEN or gh auth login)")

	for _, p := range Pins() {
		t.Run(p.Uses(), func(t *testing.T) {
			if p.Absent {
				var commit struct{ SHA string }
				err := client.Get("repos/"+p.Repo+"/commits/"+p.SHA, &commit)
				assert.Error(t, err, "%s must not exist in %s", p.SHA, p.Repo)
				return
			}
			if p.Ref == "" {
				var commit struct{ SHA string }
				require.NoError(t, client.Get("repos/"+p.Repo+"/commits/"+p.SHA, &commit))
				assert.Equal(t, p.SHA, commit.SHA)
				return
			}

			// compare/{sha}...{ref}: "ahead" or "identical" means the ref
			// contains the commit; "behind" or "diverged" means it does not.
			var cmp struct{ Status string }
			require.NoError(t, client.Get("repos/"+p.Repo+"/compare/"+p.SHA+"..."+p.Ref, &cmp))
			assert.Contains(t, []string{"ahead", "identical"}, cmp.Status,
				"%s (%s) is not in the history of %s", p.SHA, p.Source, p.Ref)

			if p.RepoID == 0 {
				return
			}
			var repo struct {
				ID    int64
				Owner struct{ ID int64 }
			}
			require.NoError(t, client.Get("repos/"+p.Repo, &repo))
			assert.Equal(t, p.RepoID, repo.ID, "repo_id")
			assert.Equal(t, p.OwnerID, repo.Owner.ID, "owner_id")
		})
	}
}

func TestIsSynthetic(t *testing.T) {
	assert.True(t, IsSynthetic(Synthetic('a')))
	assert.True(t, IsSynthetic("abc123abc123abc123abc123abc123abc123abc1"))
	for _, p := range Pins() {
		assert.False(t, IsSynthetic(p.SHA), p.Uses())
	}
}

// orphanTestFiles predate the foo.go → foo_test.go rule and are being
// merged into the test file of the source they exercise.
var orphanTestFiles = map[string]bool{
	"cmd/gh-actions-lock/atomic_test.go":            true,
	"cmd/gh-actions-lock/check_json_golden_test.go": true,
	"cmd/gh-actions-lock/command_test.go":           true,
	"cmd/gh-actions-lock/freshtag_test.go":          true,
	"cmd/gh-actions-lock/migrate_test.go":           true,
	"cmd/gh-actions-lock/missing_ref_test.go":       true,
	"cmd/gh-actions-lock/no_onboard_sha_test.go":    true,
	"cmd/gh-actions-lock/parallel_steps_test.go":    true,
	"cmd/gh-actions-lock/parity_test.go":            true,
	"cmd/gh-actions-lock/proxima_test.go":           true,
	"cmd/gh-actions-lock/prune_workflow_test.go":    true,
	"cmd/gh-actions-lock/reporoot_test.go":          true,
	"cmd/gh-actions-lock/resolve_cooldown_test.go":  true,
	"cmd/gh-actions-lock/selfrepository_test.go":    true,
}

// TestEveryTestFileHasSource enforces that x_test.go sits beside x.go, so a
// reader finds a function's tests in exactly one place.
func TestEveryTestFileHasSource(t *testing.T) {
	root := repoRoot(t)
	var orphans []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && (strings.HasPrefix(d.Name(), ".") || d.Name() == "testdata") {
			return filepath.SkipDir
		}
		if d.IsDir() || !strings.HasSuffix(path, "_test.go") {
			return nil
		}
		if _, err := os.Stat(strings.TrimSuffix(path, "_test.go") + ".go"); err == nil {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if !orphanTestFiles[rel] {
			orphans = append(orphans, rel)
		}
		return nil
	})
	require.NoError(t, err)
	sort.Strings(orphans)
	assert.Empty(t, orphans, "name each test file after the source file it tests")
}

func repoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	require.True(t, ok)
	return filepath.Join(filepath.Dir(file), "..", "..")
}
