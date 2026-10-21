# Releasing `gh-actions-lock`

## Cutting a release

From a clean, current `main`, preview the next release:

```sh
RELEASE_DRY_RUN=1 script/release patch
```

Then cut it:

```sh
script/release patch
```

To preview a release candidate:

```sh
RELEASE_DRY_RUN=1 script/release patch --rc
```

Then cut it:

```sh
script/release patch --rc
```

The first candidate is `vX.Y.Z-rc.1`; repeating the same bump increments
`rc.N`. Run the bump without `--rc` to publish the stable `vX.Y.Z`.

Release tags are immutable. If publishing fails, fix the publisher and run
the release script again for the next RC.

Use `patch` for compatible fixes, `minor` for compatible additions, and `major`
for breaking changes.

The script validates the repository, checks the current branch and `origin/main`,
then pushes and verifies an annotated `vX.Y.Z` or `vX.Y.Z-rc.N` tag.

The tag workflow verifies the tag, then publishes the binaries, attestations,
and GitHub release.

## Dependabot compatibility

Before releasing, decide whether
[Dependabot's CLI integration](https://github.com/dependabot/dependabot-core/blob/main/github_actions/lib/dependabot/github_actions/lockfile/cli_engine.rb)
must move with the release. Update `dependabot-core` when the release changes:

- CLI flags used by Dependabot.
- Findings JSON or exit codes.
- Relocking behavior.
- The lockfile schema.

Dependabot
[pins the CLI version and binary checksums](https://github.com/dependabot/dependabot-core/blob/main/github_actions/Dockerfile)
and currently
[accepts only lockfile schema `v0.0.2`](https://github.com/dependabot/dependabot-core/blob/main/github_actions/lib/dependabot/github_actions/constants.rb).
Add schema support there before this CLI emits a new version. Unrelated releases
do not need a Dependabot bump.

The findings JSON fields can stay the same while behavior changes, which still
needs this decision. Unreleased changes of that kind:

- A lockfile entry keyed by a full SHA whose `ref` is that same SHA no longer
  fails loading with exit 2. Read-only modes report it as a `not-pinned` error
  and exit 1; a fix run re-records its tag or branch and exits 0, or fails
  when no tag or branch contains the commit; a run given workflow paths warns
  about another workflow's such entry and exits 0.
- Container images the lockfile can't pin are reported as a new
  `container-image` finding category with `warning` severity, in JSON as well
  as the terminal and under `--verify-local`. That covers `docker://` steps, job
  `container:` and `services:` images, the repository's own `$/…` Docker actions
  and remote Docker, Dockerfile-only and composite actions. Remote images name
  the pulling action in `required_by`. Images in other repositories' called
  reusable workflows and in `./…` actions are not scanned. The category is
  additive: `valid`, exit codes and existing findings are unchanged, and
  Dependabot only acts on `error` findings.

## Workspace-local action coverage

Local `./…` actions no longer block locking or verification. They are never
followed, locked, or automatically migrated to `$/…`; their nested dependencies
remain outside lockfile coverage. The existing `local-action` finding is now
informational and visible in JSON. Validate these severity and exit-code changes
with Dependabot, including workflows that mix local actions with missing or
rejected remote pins. `--no-migrate-local-actions` remains a compatibility no-op.

Removing the migration pre-pass also makes invalid workflow contents, including
expressions in `uses:`, blocking findings (exit 1) in write modes, matching
read-only verification, instead of early tool failures (exit 2). They remain
rejected, report `valid: false` in JSON, and write nothing; include this change
in the Dependabot compatibility review.

Filesystem failures reading workflow sources still exit 2 in write modes.
Read-only modes retain their existing exit 1 invalid-finding behavior for these
read failures; this does not change the existing API retry or tool-error policy.
