# Recorded `gt` goldens

gt resolves a repository through Graphite's API before it touches git, so its
network verbs cannot run in a test — but nothing here is hand-written. Every
byte came out of gt 1.8.6 under `scripts/record-gt-goldens.sh`
(`task record-gt`), and a scenario nobody could record holds a README saying
what is missing instead of a plausible guess.

Re-record after a gt upgrade:

```sh
task record-gt                    # the scenarios that need no token
task record-gt -- --live          # + the ones that do (CCX_GT_RECORD_TOKEN)
task record-gt -- --bump --live   # re-pin VERSION to a new gt and record it all
```

`VERSION` holds the gt these came from, and the recorder refuses to run against
any other, so an upgrade is deliberate rather than a silent drift.

## Layout

Two files per scenario, `<name>.json` and `<name>.md`:

| File | Contents |
|---|---|
| `<name>.json` | `argv` (verb first), `stdout`, `stderr`, and `exit`, exactly as gt produced them |
| `<name>.md` | what produced the bytes, and which classifier they pin |

An `<name>.md` with no `<name>.json` beside it could not be recorded here.

The streams live inside JSON strings rather than as loose files because the
repo's commit hooks rewrite loose text — `trailing-whitespace` and
`end-of-file-fixer` between them strip and append the exact bytes these
goldens exist to preserve. Nine scenarios end a `stderr` line with a space,
which is gt's own `splog.error` template appending one when called without a
second argument; a hook stripping it would make the golden a quiet lie about
what gt printed. JSON escaping puts the payload out of their reach, and
`check-json` validates the container for free.

## What moves between recordings

The work root the recorder builds its repos under (`CCX_GT_RECORD_ROOT`,
default `/tmp/ccx-gt-record`) lands in any output naming a path, so those
scenarios differ between a macOS and a Linux recording. gt also fetches feature
flags at run time, so a flag flip can reword a message at an unchanged version.
`internal/cli/gtgolden_test.go` walks every scenario and fails when recorded
bytes stop classifying, which is how a reword surfaces.
