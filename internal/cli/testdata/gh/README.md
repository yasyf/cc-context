# Recorded `gh` and GitHub API goldens

GitHub is a network boundary, so `gh` stays mocked in tests — but nothing here is
hand-written. Every byte was captured from a real `gh` run by
`scripts/record-gh-goldens.sh` (`task record-gh`), which is the whole point: a
fake replaying these payloads cannot invent a shape GitHub never produced.

Re-record after a `gh` upgrade, and whenever a production call's argv changes:

```sh
task record-gh                    # every read scenario
task record-gh -- --only run-     # scenarios whose name contains "run-"
task record-gh -- --write OWNER/N # + the pull request write verbs
```

`VERSION` holds the `gh` these were captured from. A `--only` run refuses when it
disagrees, so a partial re-record can never mix two versions into one tree.

## Layout

One JSON file per scenario. `cli/<scenario>.json` is what a PATH fake replays —
the production sites in `internal/cli` and `internal/vcs` still shell out to the
`gh` binary:

| Key | Contents |
|---|---|
| `argv` | the recorded invocation |
| `exit` | the exit status |
| `stdout` | `gh`'s standard output, byte for byte |
| `stderr` | `gh`'s standard error, byte for byte |

`api/<scenario>.json` is what an `httptest.Server` serves — `internal/ghapi`
speaks HTTP directly, so `ccx vcs reviews` never runs `gh` at all:

| Key | Contents |
|---|---|
| `argv` | the `gh api -i` invocation that captured the response |
| `exit` | that invocation's exit status |
| `status` | the HTTP status code |
| `headers` | the response headers a parser reads (see below) |
| `body` | the response body, byte for byte |
| `stderr` | `gh`'s standard error, byte for byte |

A scenario is one container rather than a file per stream because this repo's
commit hooks (`trailing-whitespace`, `end-of-file-fixer`) rewrite raw files, and
a golden a hook edited is no longer what `gh` printed — `run-log-failed` ends
eleven log lines in whitespace, and every GraphQL payload here ends without a
final newline. As JSON strings those payloads occupy no end of line and no end of
file, so both hooks are no-ops on them; `scripts/goldenjson.py` writes the
container. `internal/cli/ghgolden_test.go` reads it back, and its
`ghGoldenUnnormalized` table fails the build if a payload loses the bytes a
hook would have taken.

`headers` keeps `Content-Type`, `Link`, and `ETag`, and on a 403 or 429 also
`Retry-After` and the `X-RateLimit-*` pair. Everything else GitHub sends is
per-request noise — a `Date`, a request id, an edge region, a running quota
count — that no parser reads and that would rewrite every golden on every
recording.

`Link` is load-bearing: `ghapi.Paginate` walks its `rel="next"` chain, and
`resolveRef` follows an absolute URL as given. GitHub's `Link` names
`https://api.github.com/repositories/<id>/...`, so a replay must rewrite that
host onto its own test server, or the walk leaves the process and hits the
network on page two. Rewrite it in the handler, not in the golden.

## Provenance

Read scenarios run against two public repositories: `yasyf/cc-context` itself,
and `cli/cli` for the surfaces cc-context has none of (contribution guidelines,
review comments, a draft pull request).

| Scenario group | Consumed by | Source |
|---|---|---|
| `repo-view-*`, `viewer-graphql` | `internal/vcs.fetchRepo` / `fetchViewer` | cc-context (own, ADMIN), cli/cli (foreign), a name that does not resolve |
| `guidelines-*` | `internal/cli.fetchGuidelines` | cc-context (every field empty) and cli/cli (templates, code of conduct, `CONTRIBUTING.md`) |
| `pr-list-*` | `internal/cli.lookupPR` | no pull request, an open one, a draft one |
| `downstack-graphql-*` | `internal/cli.resolveDownstackPRs` | cc-context branches: one with a single pull request, one carrying two (the descending-order case), one with none |
| `run-*` | ship's CI watch and `--log-failed` triage | cc-context workflow runs 30744524405 (success), 30270014111 and 30223463656 (failed) |
| `reviews-*` | `internal/cli` reviews, through `internal/ghapi` | cli/cli#13982 (open: 15 inline comments, 2 issue comments, and a `CHANGES_REQUESTED` review) and cli/cli#13084 (merged, so `mergedAt` is non-null) |

## Where a recording deviates from production argv

Three scenarios could not be captured with the exact argv production sends. Each
still holds bytes GitHub produced; only the request differs, and only as noted.

- **`cli/viewer-graphql`** asks for `organizations(first:1)` where production asks
  for `first:100`. The signed-in account belongs to twelve organizations and only
  five of those memberships are public, so a verbatim recording would publish
  seven private memberships in a public repository. The parser loops over
  `nodes`, which a one-element array exercises.
- **`api/reviews-paginate-page{1,2,3}`** ask for `per_page=1` where production
  asks for `per_page=100`. No feed on a reachable pull request answers
  `per_page=100` in more than one page, and a single page carries no `rel="next"`
  for `ghapi.Paginate` to walk.
- **`cli/repo-view-foreign`**, **`cli/repo-view-missing`**, and
  **`cli/guidelines-repo-view-populated`** name their repository positionally.
  Production runs `gh repo view` with no argument, against the working
  directory's repository, and there is only one of those to record from.

## Not yet recorded — 2026-08-02

**The pull request write verbs: `gh pr create`, `gh pr edit`, `gh pr ready`.**
`shipPRCreate` parses one thing from them — the URL `gh pr create` prints, which
`prNumberFromURL` reads the number out of — and that is exactly the kind of
payload a hand-written fake gets subtly wrong, so it is worth recording.

They are not recorded here because recording them means opening a real pull
request. `scripts/record-gh-goldens.sh --write OWNER/NAME` does the whole dance
— create the scratch repository if absent, push a branch, run the three verbs,
then close the pull request and delete the branch — but it leaves the scratch
repository behind, because the token `gh` currently holds carries
`admin:enterprise, admin:org, gist, repo, workflow` and repository deletion needs
`delete_repo`. Fill this gap by running that command with a `delete_repo`-scoped
token, or against a scratch repository you intend to keep.

Until then no fake may hand-write these payloads. A test that needs one takes the
gap as a reason to record, not as a licence to invent.

**A rate-limited response (403/429 with `Retry-After`).** `ghapi.retryDelay` reads
those headers, and there is no way to make GitHub refuse on demand. The header
allowlist already keeps them, so a recording made during a real limit lands
complete.
