package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/github/gh-actions-lock/internal/ghapi/httpmock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestVerifyLocalNormalizesHostname(t *testing.T) {
	for _, source := range []string{"flag", "environment", "repository"} {
		t.Run(source, func(t *testing.T) {
			path := writeTempWorkflow(t, `
name: ci
on: push
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: tenant/internal@v1
`, "tenant/internal@v1=sha1-"+tenantSHA)
			t.Setenv("GH_HOST", "")
			t.Setenv("GH_REPO", "")
			args := []string{"--verify-local", "--no-interactive", "--json", path}
			switch source {
			case "flag":
				args = append(args, "--hostname", "Tenant.GHE.com")
			case "environment":
				t.Setenv("GH_HOST", "Tenant.GHE.com")
			case "repository":
				t.Setenv("GH_REPO", "Tenant.GHE.com/tenant/repo")
			}
			before, err := os.ReadFile(path)
			require.NoError(t, err)
			lockBefore := readTempLockfilePins(t)
			reg := &httpmock.Registry{}
			defer reg.Verify(t)
			stdout, _, err := runCommandWithHTTP(t, reg, args...)
			require.NoError(t, err)
			assert.Contains(t, stdout, `"valid": true`)
			after, err := os.ReadFile(path)
			require.NoError(t, err)
			assert.Equal(t, before, after)
			assert.Equal(t, lockBefore, readTempLockfilePins(t))
		})
	}
}

func TestApplyVerifyFlags(t *testing.T) {
	opts := checkOptions{verify: true}
	applyVerifyFlags(&opts)
	assert.True(t, opts.noFix, "verify implies no-fix")

	opts = checkOptions{}
	applyVerifyFlags(&opts)
	assert.False(t, opts.noFix)
}

func TestValidateOutputFlags_VerifyConflicts(t *testing.T) {
	tests := []struct {
		name    string
		opts    checkOptions
		wantErr string
	}{
		{
			name:    "verify and verify-local are mutually exclusive",
			opts:    checkOptions{verify: true, verifyLocal: true},
			wantErr: "mutually exclusive",
		},
		{
			name:    "verify-local and accept-moved conflict",
			opts:    checkOptions{verifyLocal: true, acceptMoved: true},
			wantErr: "offline",
		},
		{
			name:    "verify-local and relock conflict",
			opts:    checkOptions{verifyLocal: true, relock: true},
			wantErr: "offline",
		},
		{
			name: "verify alone is valid",
			opts: checkOptions{verify: true},
		},
		{
			name: "verify-local alone is valid",
			opts: checkOptions{verifyLocal: true},
		},
		{
			name: "neither is valid",
			opts: checkOptions{},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.opts.validateOutputFlags()
			if tt.wantErr != "" {
				assert.ErrorContains(t, err, tt.wantErr)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestVerifyLocalReportsLocalActionsWithoutWriting(t *testing.T) {
	for _, extra := range [][]string{nil, {"--no-migrate-local-actions"}, {"--no-fix"}} {
		for _, jsonOut := range []bool{true, false} {
			t.Run(strings.Join(append(extra, fmt.Sprintf("json=%v", jsonOut)), " "), func(t *testing.T) {
				path := writeTempWorkflow(t, `
name: ci
on: push
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: tenant/internal@v1
      - uses: ./my-action
  call:
    uses: ./.github/workflows/reusable.yml
`, "tenant/internal@v1=sha1-"+tenantSHA)
				require.NoError(t, os.Mkdir(".git", 0o755))
				require.NoError(t, os.Mkdir("my-action", 0o755))
				require.NoError(t, os.WriteFile(filepath.Join("my-action", "action.yml"), []byte("runs:\n  using: composite\n  steps: []\n"), 0o644))
				require.NoError(t, os.WriteFile(filepath.Join(".github", "workflows", "reusable.yml"), []byte("on: workflow_call\njobs:\n  a:\n    runs-on: ubuntu-latest\n    steps:\n      - run: echo\n"), 0o644))
				before, err := os.ReadFile(path)
				require.NoError(t, err)
				lockBefore := readTempLockfilePins(t)

				args := append([]string{"--verify-local", "--no-interactive"}, extra...)
				if jsonOut {
					args = append(args, "--json=valid,findings")
				}
				stdout, stderr, err := runCommandWithHTTP(t, &httpmock.Registry{}, append(args, path)...)

				require.ErrorIs(t, err, errSilent)
				if jsonOut {
					assert.Contains(t, stdout, `"valid": false`)
					assert.Contains(t, stdout, `"category": "local-action"`)
					assert.Contains(t, stdout, `"severity": "error"`)
				} else {
					out := stdout + stderr
					assert.Contains(t, out, "local path actions")
					assert.Contains(t, out, "Run without --verify-local")
					assert.NotContains(t, out, "Migrated")
				}
				after, err := os.ReadFile(path)
				require.NoError(t, err)
				assert.Equal(t, string(before), string(after))
				action, err := os.ReadFile(filepath.Join("my-action", "action.yml"))
				require.NoError(t, err)
				assert.NotContains(t, string(action), "$/")
				assert.Equal(t, lockBefore, readTempLockfilePins(t))
			})
		}
	}
}
