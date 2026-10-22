# gh-actions-lock

> [!WARNING]
> **Public preview.** gh-actions-lock is pre-1.0. The lockfile format, flags, and behavior may change between releases.

A [GitHub CLI](https://cli.github.com/) extension that pins every direct and transitive action your workflows use to a verified commit, recorded in `.github/workflows/actions.lock`.

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
git add .github/workflows/ # check into source
```

Rerun `gh actions-lock` after adding a workflow or changing any `uses:`.

Make sure you don't check in a corrupt lockfile. Copy [`.github/workflows/verify-lockfile.yml`](.github/workflows/verify-lockfile.yml) into your repository, modify it for your needs, then run `gh actions-lock` once so that workflow is locked too.

### GitHub Enterprise Cloud with data residency

Run `gh auth login --hostname TENANT.ghe.com`, then run `gh actions-lock` from the tenant repository checkout. The host comes from the repository remote; you don't need `--hostname` or an exported token.

## Modes

| Command | Writes | Network | Use for |
|---|---|---|---|
| `gh actions-lock` | yes | yes | Onboarding and fixing the lockfile |
| `gh actions-lock --relock` | yes | yes | Advancing moved refs (`main`, `v4`) to current upstream commits |
| `gh actions-lock --verify` | no | yes | CI: the same checks as a fix run |
| `gh actions-lock --verify-local` | no | no | Pre-commit: every `uses:` has a lockfile entry (coverage only, not provenance) |
| `gh actions-lock --no-fix` | no | yes | Read-only report |

Other flags:

- `<workflow-path>...` scopes a run to specific workflows. Only a full-directory run prunes entries for deleted workflows.
- `--json [fields]` prints machine-readable output to stdout.
- `--accept-moved` accepts pins whose locked commit is no longer upstream. Check why it moved first; never use it in automation.
- `--no-narrow` keeps partial versions (`v4`) as written instead of narrowing to a full tag.

`gh actions-lock --help` is authoritative.

## Limitations

- Reusable workflows called at `jobs.<id>.uses` from another repository are not locked ([#179](https://github.com/github/gh-actions-lock/issues/179)).
- Local `./…` actions are never followed or locked, so one that uses remote actions is a blocking error until you pass `--migrate-local-actions` (rewrites same-repo `./…` to `$/…`) or `--ignore-local-actions`.
- Container images are not locked, and any image without an `@sha256:` digest, including one chosen by a `${{ }}` expression, is a warning ([#236](https://github.com/github/gh-actions-lock/issues/236)).
- `--verify-local` is an offline coverage check that doesn't verify provenance; `--verify` checks provenance before merge, and the runner checks it again at job start.
- A lockfile change that moves a floating ref (`v4`, `main`) back to an older commit in its history isn't detected, so review lockfile diffs or pin exact tags.

## Learn more

- [Workflow dependency lockfiles](https://docs.github.com/en/actions/concepts/security/workflow-dependency-lockfiles)
- [Lock workflow dependencies](https://docs.github.com/en/actions/how-tos/secure-your-work/lock-workflow-dependencies)
- [Enforce workflow lockfiles](https://docs.github.com/en/actions/how-tos/administer/enforce-workflow-lockfiles)
- [Troubleshooting](docs/troubleshooting.md)

## Contributing

See [CONTRIBUTING.md](./CONTRIBUTING.md)

## Support

See [SUPPORT.md](./SUPPORT.md).

## License
Licensed under [MIT](./LICENSE).
