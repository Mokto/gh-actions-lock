# Agent instructions

## Code style

Write Go that follows the [Google Go Style Guide](https://google.github.io/styleguide/go/guide) and lines up with [cli/cli](https://github.com/cli/cli) conventions. Keep it readable and simple.

### Comments

Doc comments earn their keep by saying something the code doesn't. Skip:

- Restating the signature ("Foo does foo").
- Meta-commentary about what the package _isn't_ ("intentionally pure", "no HTTP, no filesystem"). If it's true, the imports show it.
- Stability sermons on every type. One short note in the package doc is enough.
- Decorative em-dashes, hedging, or apologetic phrasing.

Keep:

- Why a non-obvious choice was made.
- Invariants the type or function relies on.
- Pointers to related code when the link isn't obvious from names.

When in doubt, fewer words. If a comment reads like marketing or a victory lap, delete it.

### Naming and shape

- Prefer short, lowercase package names. No underscores.
- Exported identifiers get a doc comment that starts with the identifier name.
- Errors are values; wrap with `fmt.Errorf("...: %w", err)` and expose sentinels with `errors.Is`.
- Don't pre-declare `var foo Type` then assign — declare at first use.

### Tests

- Table-driven by default; subtests named with what they assert, not how.
- One assertion per logical claim; multiple `assert.X` calls beat one giant struct compare.
- Don't golden-snapshot what you can assert structurally.
- Name each test file after the source file it tests: `foo.go` → `foo_test.go`.
  No topic or scenario test files; `TestEveryTestFileHasSource` enforces it.
- Real upstream commits come from `internal/testfixture` pins, which record
  their release and are checked against their recorded ref by
  `make test-fixtures`. Use `testfixture.Synthetic` for commits that only
  exist in a fake API.
- Fake GitHub with `httpmock` (`StubRepo`, `ParityIntact`, `RoundTripFunc`,
  `CountingTransport`) rather than a new per-file transport.

### Where does a test belong?

There are two test layers with different guarantees. Put a test in the
cheapest layer that can actually prove the thing, and don't restate the same
claim in both.

- **Go unit** (`internal/**/*_test.go`) — internal logic: traversal, ref
  classification, error types, dedup. Fast, hermetic, no token. This is the
  primary safety net. Logic like BFS or `$/` classification can *only* live
  here; it can't be expressed in the catalog.
- **Go command test** (`cmd/**/*_test.go` with `httpmock`) — full CLI wiring
  end to end, but with a stubbed resolver so it stays deterministic. Runs in
  the blocking `test` CI job with no token. Reach for this to prove the
  command plumbs its pieces together.
- **Catalog scenario** (`test/scenarios/catalog.yml`, run by `test/e2e`)
  — the *real compiled binary* against the real API or a fake GitHub named
  by `stub:` (profiles in `test/e2e/e2e.go`). Use it only for what a Go mock
  can't prove: real arg parsing, the real lockfile emitter, live transitive
  resolution over real fixtures.

Pitfalls that have bitten us:

- **`needs_token: true` scenarios do not guard merge.** They run only with
  `E2E_LIVE=1`, in the non-blocking `integration-live` CI job. Never let a
  token-gated scenario be the *only* coverage for a behavior — back it with
  a Go test.
- **Every other scenario must be tagged `stub`.** Those run hermetically in
  `go test ./...`; `TestEveryScenarioRunsInCI` rejects a scenario that is
  neither.
- **Same-assertion duplication is not defense-in-depth.** If a Go command
  test already asserts the observable behavior deterministically, add a
  catalog scenario only for the real-binary / real-API delta, and say so in
  its `description`. Otherwise you're paying for a second, slower, flakier
  copy of a test you already have.
