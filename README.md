# gh-actions-lock

A [GitHub CLI](https://cli.github.com/) extension that pins every direct and transitive action your workflows use to a verified commit, recorded in `.github/workflows/actions.lock`.

> [!WARNING]
> **Public preview.** gh-actions-lock is pre-1.0. The lockfile format, flags, and behavior may change between releases.

## Install

Requires an authenticated `gh`. github.com and GitHub Enterprise Cloud with data residency (`*.ghe.com`) are supported; GitHub Enterprise Server is not.

```bash
gh extension install github/gh-actions-lock
```

To teach your AI coding agent the CLI:

```bash
gh skill install github/gh-actions-lock
```

## Quickstart

From the repository checkout:

```bash
gh actions-lock            # onboard or fix: pin every resolvable action and write actions.lock
gh actions-lock --verify   # confirm, without writing
git add .github/workflows/
```

Rerun `gh actions-lock` after adding a workflow or changing any `uses:`.

### GitHub Enterprise Cloud with data residency

Run `gh auth login --hostname TENANT.ghe.com`, then run `gh actions-lock` from the tenant repository checkout. The host comes from the repository remote; you don't need `--hostname` or an exported token.

## Modes

| Command | Writes | Network | Use for |
|---|---|---|---|
| `gh actions-lock` | yes | yes | Onboarding and fixing the lockfile |
| `gh actions-lock --relock` | yes | yes | Advancing moved refs (`main`, `v4`) to current upstream commits |
| `gh actions-lock --verify` | no | yes | CI: the same checks as a fix run |
| `gh actions-lock --verify-local` | no | no | Pre-commit: every `uses:` has a lockfile entry |
| `gh actions-lock --no-fix` | no | yes | Read-only report |

Other flags:

- `<workflow-path>...` scopes a run to specific workflows. Only a full-directory run prunes entries for deleted workflows.
- `--json [fields]` prints machine-readable output to stdout.
- `--accept-moved` accepts pins whose locked commit is no longer upstream. Check why it moved first; never use it in automation.
- `--migrate-local-actions` / `--ignore-local-actions` handle `./…` actions that use remote actions.
- `--no-narrow` keeps partial versions (`v4`) as written instead of narrowing to a full tag.

`gh actions-lock --help` is authoritative.

## Exit codes

| Code | Meaning |
|---|---|
| 0 | Nothing to fix, or every finding was fixed |
| 1 | Blocking findings remain |
| 2 | The tool failed (bad flag, IO, network, malformed lockfile); see stderr |

## CI

Copy [`.github/workflows/verify-lockfile.yml`](.github/workflows/verify-lockfile.yml) into your repository, then run `gh actions-lock` once so that workflow is locked too.

## Learn more

- [Workflow dependency lockfiles](https://docs.github.com/en/actions/concepts/security/workflow-dependency-lockfiles)
- [Lock workflow dependencies](https://docs.github.com/en/actions/how-tos/secure-your-work/lock-workflow-dependencies)
- [Enforce workflow lockfiles](https://docs.github.com/en/actions/how-tos/administer/enforce-workflow-lockfiles)
- [Troubleshooting](docs/troubleshooting.md)

## Contributing and support

See [CONTRIBUTING.md](./CONTRIBUTING.md). Support is best-effort: file [issues](https://github.com/github/gh-actions-lock/issues) and see [SUPPORT.md](./SUPPORT.md). Maintained by @github/actions-dispatch-reviewers ([CODEOWNERS](./CODEOWNERS)). Licensed under [MIT](./LICENSE).
