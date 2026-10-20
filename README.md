# gh-actions-lock

Lock your workflow dependencies.

> [!WARNING]
> **Public preview.** gh-actions-lock is pre-1.0 and under active development. The
> lockfile format, command flags, and behavior may change without notice between
> releases. Use it, file issues, and expect rough edges.

## Background

gh-actions-lock is part of GitHub's Workflow Dependency Pinning effort. It gives repositories a lockfile that pins every workflow dependency to a verified commit, so what runs on the runner is exactly what you locked. Development is ongoing and behavior may still change.

Contributions are welcome. See [CONTRIBUTING.md](./CONTRIBUTING.md) to get started.

## Requirements

GitHub Enterprise Server (GHES) is not supported.

Requires the [`gh` CLI](https://cli.github.com/). Install it first, then install the extension:

```bash
gh extension install github/gh-actions-lock
```

## Usage

Scan every workflow under `.github/workflows/` directory, pin each resolvable action to a SHA, and update the lockfile:

```bash
gh actions-lock
```

After the initial run to onboard workflows, you will need to run `gh actions-lock` when:
- A new workflow is created that has `uses` dependencies.
- An existing workflow adds or removes `uses` dependencies.

A full-directory run (`gh actions-lock` with no path arguments) also prunes lockfile entries for workflows that have been deleted from `.github/workflows/`, dropping any dependencies left orphaned by the removal. Scoped runs that name specific workflows never prune out-of-scope entries.

Locked pins are sticky: a normal run only re-resolves refs the lockfile does
not record yet. Every locked pin, including transitive ones, is re-checked the
way the runner checks it at job start: the repository was not renamed or
transferred, the pinned commit still exists, and it is still in the history of
the tag or branch the lockfile recorded. A failed check blocks the run.

Writes are all or nothing: if a run finds any error it cannot fix itself, it
changes no workflow and no lockfile entry, not even for workflows that are
otherwise fine.

To check without writing anything, for example in CI, run:

```bash
gh actions-lock --verify
```

Every mode checks each locked commit against the lockfile entry's `ref`, which
is looked up only as an existing tag, then branch, of exactly that name, the
way the Actions backend resolver does. Every pin, whether an exact tag
(`v4.2.1`), a partial version (`v4`), a branch (`main`) or a full SHA (`uses:
owner/repo@<sha>`), must have its commit in that ref's history, so a fork
commit can't pass as upstream. Tags can move: the Actions backend accepts an
exact tag that moved ahead of the commit, so runs allow it and `--verify` only
warns. A lockfile entry whose `ref` the Actions backend resolver would reject
fails the run (exit 2), naming the first such entry in the file and its line:
it must be a short tag or branch name the backend accepts, never a SHA, a
`refs/` name, revision syntax such as `main~1`, or a `git describe` name, and
it must equal the `uses:` ref unless that is a full SHA. A `uses:` ref that is
not such a tag or branch, such as `pull/1/head` or `main~1`, is never locked.

One exception loads instead: an entry keyed by a full SHA whose `ref` is that
same SHA. The Actions backend still rejects it, so `--verify`, `--no-fix` and
`--verify-local` report it as a `not-pinned` error (exit 1) and write nothing,
wherever it sits: in a scanned workflow, in a workflow the run didn't scan or
that was deleted, under a composite, or used by nothing. A plain run or
`--relock` records the tag at that commit, else a branch containing it, keeps
the full-SHA `uses:`, and drops the entry if nothing uses it; a commit no tag
or branch contains still fails the run and nothing is written. A run given
workflow paths leaves another workflow's such entry as it is, warns about it
(exit 0), and doesn't report every workflow valid.
Every online run that retains recorded pins also reads their action manifests
at the locked commits to check that the recorded transitive graph covers their
dependencies, without advancing to current upstream commits. This includes pins
reused from another workflow and read-only runs with refresh flags. Writable
refreshes bypass those reads only for entries the planned write actually
replaces or removes. Shared entries retained for unselected workflows are
still checked.
Selected workflows share batched manifest reads; failures remain attributed
only to the workflows that reach them.
Retained action paths are reconstructed from their owning workflows, including
owners outside a selected write scope. Missing or changed owner sources that
cannot establish those paths block the run with restore and `--relock` guidance;
adding a new sub-action path cannot repair damage to a retained path.

`--verify-local` runs offline and never writes. It checks direct refs and all
recorded transitive edges for missing dependency entries, but cannot discover an
edge erased from the lockfile or establish that recorded commits are
trustworthy; use `--verify` in CI for that.

Pins to branches or partial versions (e.g. `main`, `v4`) are not advanced when
upstream moves. To regenerate the lockfile from the current upstream commits,
run:

```bash
gh actions-lock --relock
```

`--relock` re-resolves the complete graph, even when no recorded ref has moved,
and restores missing entries and edges. A normal sticky write rejects dangling
recorded references: restore missing entries from version control to keep the old
commits, or explicitly relock to select current upstream commits.
Refreshes reject conflicting commits for a shared dependency rather than
combining manifests from different commits into one lockfile entry.
A composite action that reaches itself through `uses:` can't be locked; the run
fails before writing any file.
Suspicious pins whose recorded commit is no longer
reachable upstream are left as errors — use `--accept-moved` to re-resolve
those as well.

### GitHub Enterprise Cloud with data residency

Authenticate `gh` to your tenant, then
run the extension from your tenant repository checkout:

```bash
gh auth login --hostname octocorp.ghe.com
# From the repository checkout:
gh actions-lock
```

With no conflicting environment overrides, the CLI infers the host from the
repository remote and uses the credentials stored by `gh` for that host. You do
not need to export a token or pass `--hostname` on every run. The account must
have read access to the tenant repositories used by your workflows.

### Self repository actions (`$/…`)

`uses: $/…` references an action or reusable workflow in the **same repository** as
the defining file, resolved at the **running commit** of that workflow or action.
This is independent of the workspace populated by `actions/checkout`. The reference
is **inherently pinned** and needs no lockfile entry. It is valid anywhere a relative
`./…` reference is:

```yaml
steps:
  - uses: $/actions/my-action          # same-repo action, inherently pinned
jobs:
  call:
    uses: $/.github/workflows/reusable.yml  # same-repo reusable workflow
```

A trailing `@ref` (e.g. `$/actions/my-action@v1`) is rejected — the ref is always
the running commit.

For a workflow triggered by `pull_request_target`, `$/…` in that workflow uses
the default-branch commit selected for the run (`github.sha`), regardless of the
PR's base branch (see [GitHub's event semantics](https://github.blog/changelog/2025-11-07-actions-pull_request_target-and-environment-branch-protections-changes/)).
A checkout of the same repository with no `ref`, or with `ref: ${{ github.sha }}`,
uses that commit too. Checking out `ref: ${{ github.event.pull_request.head.sha }}`
can make `./…` use different code. The CLI never converts `./…` references to `$/…`.

### Local path actions (`./…`)

Local path actions are allowed. `gh actions-lock` cannot and will not follow
`./…`: these paths refer to the runner workspace, which checkouts, downloads,
and scripts can change. Local actions are never locked, and their own `uses:`
dependencies are **not covered by the lockfile**.

Locking, sticky runs, `--verify`, and `--verify-local` report this limit as a
non-blocking `local-action` notice, including in JSON. Other directly named
actions and dependencies reached through `$/…` or remote composites are still
checked and locked normally. A local action does not excuse missing or rejected
pins elsewhere in the workflow.

Local references and action files are left unchanged; automatic migration is
no longer performed. `--no-migrate-local-actions` remains accepted as a
compatibility no-op. Using `$/…` is an explicit source choice with different
runtime semantics, not an automatic replacement for `./…`.

## How it works

A repo gets a lockfile (located at [`.github/workflows/actions.lock`](https://github.com/github/gh-actions-lock/blob/main/.github/workflows/actions.lock)) and workflows are onboarded to the lockfile on a per-workflow basis. 

For actions covered by the lockfile, onboarded workflows enforce that dependencies are present and that the locked commit is executed on the runner. Workspace-local `./…` actions and their dependencies are not covered. Lockfiles are also verified for forgeries. The sha must exist in the refs it's stated to exist in. Repository identity is recorded and redirects and mismatches are blocked at runtime.

Finally, locked actions must have a branch that the commit being locked exists within. This is to make impostor commit style attacks harder.

## Limitations

Workspace-local `./…` actions are allowed but outside lockfile coverage.
The CLI never follows these paths or their nested dependencies, even when a
matching action file exists in the current checkout. Verification checks the
remaining lockfile coverage; a successful check does not validate local actions.

## License

This project is licensed under the terms of the MIT open source license. See [LICENSE](./LICENSE) for the full terms.

## Maintainers

gh-actions-lock is maintained by @github/actions-dispatch-reviewers. See [CODEOWNERS](./CODEOWNERS).

## Support

Support is best-effort and community-based. Please file bugs and feature requests as [GitHub issues](https://github.com/github/gh-actions-lock/issues). See [SUPPORT.md](./SUPPORT.md) for details.
