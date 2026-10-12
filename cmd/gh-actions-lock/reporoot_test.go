package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/github/gh-actions-lock/internal/ghapi/httpmock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A partial checkout without .git, with a `$/` composite outside .github,
// relocks only when GH_ACTIONS_LOCK_REPO_ROOT names the root.
func TestRun_RepoRootEnvWithoutGit(t *testing.T) {
	tests := []struct {
		name    string
		setRoot bool
		wantErr string
	}{
		{name: "env set locks composite deps", setRoot: true},
		{name: "env unset fails without .git", wantErr: "not in a git repository"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			write := func(rel, content string) {
				full := filepath.Join(dir, rel)
				require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o755))
				require.NoError(t, os.WriteFile(full, []byte(content), 0o600))
			}
			write("tools/act/action.yml",
				"runs:\n  using: composite\n  steps:\n    - uses: actions/setup-go@v5.0.0\n")
			write(".github/workflows/ci.yml",
				"name: ci\non: push\njobs:\n  b:\n    runs-on: ubuntu-latest\n    steps:\n      - uses: $/tools/act\n")
			t.Chdir(dir)
			t.Setenv("GH_ACTIONS_LOCK_REPO_ROOT", "")
			if tt.setRoot {
				t.Setenv("GH_ACTIONS_LOCK_REPO_ROOT", dir)
			}
			reg := &httpmock.Registry{}
			registerSetupGo(reg)

			_, stderr, err := runCommandWithHTTP(t, reg)

			if tt.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error()+stderr, tt.wantErr)
				return
			}
			require.NoError(t, err, stderr)
			assert.Contains(t, readTempLockfilePins(t), "'actions/setup-go@v5.0.0'")
		})
	}
}
