## Go Style

Target Go 1.23+. Run `task build`, `task test` (`go test -race`), and `task lint`.

Building from source also needs the Rust toolchain (stable, per `format-core/rust-toolchain.toml`) and the `wasm32-unknown-unknown` target. The format engine is `format-core` compiled to WASM and `go:embed`'d, so `task wasm` builds and stages it into `internal/format/formatcore.wasm` (gitignored) before any Go compile — the `build`, `test`, `vet`, and `lint` tasks depend on it. `brew install yasyf/tap/ccx` installs a prebuilt binary that needs no toolchain; `brew install --HEAD` builds from source, pulling in `rustup` and `go-task` as build-only dependencies (Homebrew's `rust` ships no wasm32 std) so the formula can provision the toolchain + target and run `task wasm` before the Go compile.

`format-core` is also consumed directly as a Rust crate by downstreams (cc-squash tag-pins it via a cargo git dependency), so its selection and encoding behavior is a public contract: the crate owns byte-level format selection (which encoding is leanest for a payload — token accounting stays downstream), the golden corpus in `format-core/core/tests/` gates behavior both native and through-WASM, and any behavior change ships in a tagged release so downstreams adopt it by a deliberate tag bump, never silently.

**Comments are terse and used sparingly — the code documents itself** through names, types, and organization. The one exception is documentation-generation comments: godoc on exported types, funcs, and the package, each starting with the identifier's name (`// NewRootCmd builds …`); unexported helpers get none. Beyond godoc, comment only for TODOs, non-obvious workarounds, or disabled code — never to restate the signature.

**Errors wrap with `%w`.** Return failures up the stack with `fmt.Errorf("…: %w", err)` and inspect them with `errors.Is` / `errors.As`, never string matching. See STYLEGUIDE.md § Error Handling.

**Structured logging via `log/slog`.** Diagnostics go through the configured default logger (`slog.Info`, `slog.Debug`) with key-value attrs — never `fmt.Println` for logging. See `internal/log`.

@STYLEGUIDE.md

## Python Style

The capt-hook guard pack under `plugin/capt-hook/hooks/` follows `plugin/capt-hook/hooks/STYLEGUIDE.md` (Python 3.13+): public-surface tests, inline `tests={}` rows, and no leading underscores on module-level helpers.

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
