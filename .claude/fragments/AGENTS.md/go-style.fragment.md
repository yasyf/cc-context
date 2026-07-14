## Go Style

Target Go 1.23+. Run `task build`, `task test` (`go test -race`), and `task lint`.

Building from source also needs the Rust toolchain (stable, per `format-core/rust-toolchain.toml`) and the `wasm32-unknown-unknown` target. The format engine is `format-core` compiled to WASM and `go:embed`'d, so `task wasm` builds and stages it into `internal/format/formatcore.wasm` (gitignored) before any Go compile — the `build`, `test`, `vet`, and `lint` tasks depend on it. `brew install yasyf/tap/ccx` installs a prebuilt binary that needs no toolchain; `brew install --HEAD` builds from source, pulling in `rustup` and `go-task` as build-only dependencies (Homebrew's `rust` ships no wasm32 std) so the formula can provision the toolchain + target and run `task wasm` before the Go compile.

`format-core` is also consumed directly as a Rust crate by downstreams (cc-squash tag-pins it via a cargo git dependency), so its selection and encoding behavior is a public contract: the crate owns byte-level format selection (which encoding is leanest for a payload — token accounting stays downstream), the golden corpus in `format-core/core/tests/` gates behavior both native and through-WASM, and any behavior change ships in a tagged release so downstreams adopt it by a deliberate tag bump, never silently.

**Comments are terse and used sparingly — the code documents itself** through names, types, and organization. The one exception is documentation-generation comments: godoc on exported types, funcs, and the package, each starting with the identifier's name (`// NewRootCmd builds …`); unexported helpers get none. Beyond godoc, comment only for TODOs, non-obvious workarounds, or disabled code — never to restate the signature.

**Errors wrap with `%w`.** Return failures up the stack with `fmt.Errorf("…: %w", err)` and inspect them with `errors.Is` / `errors.As`, never string matching. See STYLEGUIDE.md § Error Handling.

**Structured logging via `log/slog`.** Diagnostics go through the configured default logger (`slog.Info`, `slog.Debug`) with key-value attrs — never `fmt.Println` for logging. See `internal/log`.

@STYLEGUIDE.md

## Python Style

The capt-hook guard pack under `plugin/hooks/` follows `plugin/hooks/STYLEGUIDE.md` (Python 3.13+): public-surface tests, inline `tests={}` rows, and no leading underscores on module-level helpers.

## General Rules

**Minimal changes.** Stay within scope; fix the issue, then stop.

**Match surrounding code.** Follow the conventions of the file you're in, then the module.

**No defensive coding.** No fallbacks, shims, or backwards-compat layers; no guards against impossible states. If unused, delete it. Crash on the unexpected.

**Search before writing.** Before creating a helper, query the codebase via `ccx code search` (intent or symbol queries both work). Sibling packages win over re-implementation.

**Code stewardship.** When you touch a file, fix nearby bugs, style violations, and broken tests; don't wave them off as pre-existing or out of scope.

**Observe, don't infer.** Inspect actual data — read fixtures, dump structs, run the code — before reasoning from assumption.

**Don't use external failures as an excuse to stop.** API quota, rate-limit, and outage errors rarely block the whole task; trace the catch sites and confirm a failure actually stops you before claiming it does.

**Verify before asserting.** Don't report something as working, fixed, blocked, or impossible until you've checked — run it, read the output, reproduce the failure. "It should work" is not "it works."

**Reproduce before fixing.** When something breaks, isolate the smallest failing case before editing or re-running. Re-running the whole command while changing code between runs hides the root cause; narrow to the one failing test or input first.

**Research after repeated failure.** After ~2 failed approaches, stop guessing and gather evidence — search the web, read the docs and source — before a third attempt.

**Get a second opinion on a plateau.** On a debugging plateau (2 failed attempts before a 3rd), a non-trivial architectural decision, or algorithmic/security-sensitive code, get an outside check (e.g. `/codex`) before committing to the approach.

**Don't contort code to satisfy a linter.** The compiler and `golangci-lint` serve the code, not the other way around. Don't widen a type to `any`, bolt on a needless type assertion, or sprinkle `//nolint` just to silence a diagnostic. If a clean fix isn't obvious, leave the diagnostic — a visible one is preferable to scar tissue.

**Mechanical linting.** The pre-commit hooks (prek: gofumpt + goimports + golangci-lint) format and lint on every `git commit` — run `uvx prek install` once to activate them. Leave formatting and linting to the hook; never run `gofumpt` or `golangci-lint` by hand (the `go` capt-hook pack blocks it). When reviewing code, don't flag mechanical lint violations (gofmt, import order, line length).

**Testing.** Tests live beside the code as `*_test.go`; run them with `task test` (`go test -race ./...`). Write table-driven tests with strict assertions against specific values, mock the boundaries your code talks to (network, filesystem, clock), and leave the code under test real.

**Writing docs.** When writing or revising docs, a README, a tutorial, a how-to, or reference, use the `writing-docs` skill (Diataxis modes, voice rules, and runnable code-sample rules) and run `slop-cop check <file> --lang=markdown` before you finish (slop-cop is a Go binary; if it's not on PATH, run the `/slop-cop-check` skill — never `uvx slop-cop`).

**Version control.** This repo is a colocated `jj` repo over git — prefer `jj` (`jj describe` / `jj commit`, `jj git push`) over raw `git` for day-to-day work. Commits stay atomic and scoped: one logical change each. For the routine commit, push, and watch-CI cycle, `ccx vcs ship -m "<msg>"` runs the whole dance in one call — a jj-aware commit, the push, and a watch over every workflow run on the pushed commit, with a per-run report and failure logs — instead of the three-to-six Bash calls it took by hand. A working copy shared with a concurrent session is no reason to bypass ship: `ccx vcs ship -m "<msg>" <paths...>` commits only your paths and leaves the rest of `@` untouched. The push only auto-advances the trunk bookmark — parked on someone else's bookmark, ship refuses, and `--bookmark <name>` advances a non-trunk bookmark deliberately. Drop to the manual `jj` steps when ship still doesn't fit, like a multi-commit split. A dirty tree is just the working-copy commit `@` — to land work on an updated remote, `jj git fetch` then `jj rebase` (your in-flight `@` rides along untouched); never `git stash` or a worktree + cherry-pick dance.

**Watch CI after every push.** A push that kicks off CI isn't done until every run is green. `ccx vcs ship` folds this in — it pushes, then watches every workflow run on the pushed commit (found by `--commit`, retrying registration for up to a minute) and reports each run's conclusion, duration, and URL, plus failing jobs and a `--budget`-capped log excerpt when a run goes red; on a terminal the watch streams live. A `CI error` segment means the watch itself hit an infra failure after a successful push — the report's `check:` line says how to resume, and re-running ship would cut a new commit. For a push ship didn't make, watch the run to completion yourself before you stop — `gh run watch "$(gh run list -L1 --json databaseId -q '.[0].databaseId')" --exit-status` — and never walk away from a red run: fix it or report it. (`--exit-status` exits non-zero when the run fails; give the run a moment to register before watching.)

**Releases.** Tagging `v*` triggers `.github/workflows/release.yml`, which runs goreleaser to build the binaries, cut a GitHub release, then render `.github/formula/ccx.rb.tmpl` and publish the Homebrew formula to `yasyf/homebrew-tap`. The version comes from the tag; bump `plugin/.claude-plugin/plugin.json` to the tag's version in the release commit — the `verify-plugin-version` job fails the release on a mismatch. The release refuses to run unless the tagged commit is on `main` — tag a merged commit (e.g. `git tag vX.Y.Z origin/main`), not a feature branch. One-time setup: a `HOMEBREW_TAP_TOKEN` repo secret with push access to the tap.
