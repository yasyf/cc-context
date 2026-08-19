# Changelog

All notable changes to this project are documented here.
The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added
- **`ccx vcs ship --no-commit` ships a change that is already committed.** With
  the work committed and the working copy clean, ship refused — and the only way
  through was `--amend`, which re-runs a commit-shaped mutation (`jj squash`,
  `git commit --amend`, `gt modify`) over a commit that may already be pushed.
  `--no-commit` drops the commit stage and runs the rest: it pushes the commit in
  place and opens or updates its pull request, which is what the refusal's own
  `jj bookmark move … && jj git push --bookmark …` hint spelled out by hand. The
  motivating case is attaching a body to a PR `gt submit` opened bodyless. It
  refuses a dirty working copy, since uncommitted work would otherwise be left
  out of a branch and a pull request the same run updates, and it cuts no branch:
  placing an existing commit onto a new one is a history rewrite, not a push. The
  empty-working-copy refusal is unchanged for every run that does not pass it.
- **`ccx vcs info` places a working copy inside its repository.** A linked
  checkout — `git worktree add`, `jj workspace add`, or a git worktree carrying
  its own colocated `.jj` — reports its `shape`, the repository's own working copy
  as `main-root`, the admin dir every sibling contends over as `common-dir`, and
  the `repo-key` that separates two siblings from two unrelated repositories.
  Every git-backed checkout reports `trunk-held`, the working copy currently
  holding trunk. That last line is the answer to a `gt restack` that prints
  `Did not restack branch B because it is checked out in worktree W` and then
  exits 0: the branch it skipped is held somewhere else, and nothing else in the
  output said so. A trunk no checkout holds prints nothing — a detached main
  working copy under colocated jj is the usual reason, and it is not a failure.
  A linked worktree of a bare repository, or of one made with
  `git init --separate-git-dir`, reports no `main-root` at all, because there
  is no main working copy — git's own `git worktree list` cannot recover one
  either.
- **`ccx vcs worktree list|add|rm|repair`.** Management verbs for the working
  copies `ccx vcs info` learned to place. `list` reports every checkout git
  registers, each resolved to its shape, with branch, lock, and prunable state
  inline and a `defect` where resolution fails — a dangling gitdir pointer is
  that row's answer, never an exit 1, following the `checkout_error`
  precedent — budget-capped with a withheld count, `--json` for the raw
  report. `add` dispatches on the checkout's shape (`jj workspace add` from a
  jj workspace, `git worktree add` from anything else) and mints the path
  outside the repository tree, under
  `$HOME/.ccx/worktrees/<basename>-<key>/<name>` — a pool keyed by repository
  identity, the key an eight-character digest of the repo key, so every
  sibling of one repository mints into the same directory however far apart
  their roots sit; `--jj colocate` is refused
  with jj's own words, since jj 0.43 rejects a colocated repo inside a git
  worktree. `rm` removes only what `add` minted: a worktree merely sharing the
  name is refused by the path it lives at and left to `git worktree remove`,
  since resolving it by basename would hand rm a tree the user never pointed
  ccx at. It refuses while the target holds trunk and names it — every
  restack rebases onto it — except where no trunk resolves at all, a repo
  with no remote, say — where the guard has nothing to protect and the add/rm
  round trip closes; only the clean miss skips, a lookup that fails still
  errors. A jj workspace is removed by forgetting it and deleting its tree,
  because forget alone leaves a live-looking directory behind — and because
  forget also drops changes jj never snapshotted, rm runs the dirty-tree
  refusal git has and jj lacks, standing down only under `--force`, the flag
  the git lane forwards to `git worktree remove`. The lookup-fails-still-errors
  rule holds where rm resolves its target, too: a workspace whose `.jj/repo`
  pointer is unreadable or resolves to nothing surfaces that failure instead
  of `no working copy named "<name>"` — a refusal reading as a clean miss
  invites deleting the tree by hand, stranding exactly the stale registry
  entry forget-and-delete exists to avoid.
  `repair` re-points a dangling gitdir pointer — the exact breakage
  `info` could already diagnose and nothing could fix — running git from the
  repository the pointer itself names; `--dry-run` prints the command instead
  of running it.
- **`ccx vcs info` reports how each downstack pull request ended, and whether it
  is green.** The one batched query the gt lane already ran for the whole stack
  now also selects each pull request's `state`, `mergedAt`, and head-commit check
  rollup: `--json` entries carry `state`, `merged`, `merged_at`, and `checks`, and
  the report line reads `branch → PR #7 (body, merged, checks success)`. `merged`
  is the verdict `state` alone cannot give. Graphite's merge queue squash-merges a
  whole stack into one trunk commit, so every pull request it lands reports
  `CLOSED` with a null `mergedAt` and no merge commit, closed by the queue's own
  account — which is what separates one the queue landed from one a human
  abandoned. That signature is Graphite's, so only the gt lane reads it.

### Changed
- **`ccx vcs info` reports the states it used to exit 1 on.** A working copy whose
  gitdir pointer resolves to nothing lands in a new `checkout_error` field and a
  `checkout` line rather than aborting the command, and graphite state that cannot
  be walked into a trunk and a downstack lands in `graphite.stack_error` and a
  `stack` line. Both follow the precedent `github_error` set: an input nobody can
  read is what someone runs `info` to find out, and refusing to print the branch,
  the dirtiness, and the repository around it withholds the rest of the answer
  too. `--json` gains `worktree`, `trunk_holder`, `checkout_error`, and
  `graphite.stack_error`; every existing field keeps its shape and meaning, `root`
  included — it is this working copy, never the repository's.
- **Repository caches are keyed on the repository, not the checkout.** The
  GitHub record, the Graphite reachability verdict, and the contribution
  guidelines were each cached per working copy: ccx identified a working copy
  by what manages it and where it sits, and that pair was standing in for a
  third fact — which repository it belongs to. With one checkout the
  conflation is invisible. Across linked worktrees sharing one `.git`, one ref
  namespace, and one Graphite database it is a whole failure class: measured
  here, six checkouts of one repository each held their own cache directory,
  so each paid its own `gh repo view` and its own reachability probe — 20s
  timeout, 6.67s median — for a verdict that is identical by construction. All
  three records now key on the admin dir every sibling shares. The
  semantic-search index deliberately does not — it derives from working-tree
  content, which genuinely differs between siblings. Two things a user will
  observe: every existing cache record is orphaned on first run — the key is
  the directory name, so an old record is never mis-read, only ignored, and
  one refetch per repository restores it — and sibling checkouts now contend
  on one lock rather than N, so total cost drops to one probe, but a single
  command on a cold cache can wait behind a sibling's.
- **The downstack pull-request lookup is one call, not one per branch.**
  `ccx vcs info` and every gt-lane `ccx vcs ship` resolved the downstack's
  pull requests with a `gh pr view` per branch, and ship did it from two
  places that each re-derived the stack, so a two-branch stack cost four
  lookups. It is now a single `gh api graphql` naming every branch: on a
  two-branch stack, a ship with no PR flags went from 2 `gh` calls to 1, and
  one carrying `--pr-body-file` from 5 to 2. Selection is unchanged — no state
  filter and newest first, both matching `gh pr view` — so a branch resubmitted
  after its first pull request closed still resolves to the live one, and
  `--pr-body-file` writes the body there rather than onto the corpse.
- **`ccx vcs reviews` talks to GitHub's API directly instead of shelling out
  to `gh`.** A watch ran four `gh` subprocesses per pull request per 30-second
  poll cycle for its whole life — a three-branch stack watched for ten minutes
  spawned ~243 processes — plus one per target at setup. A cycle is now one
  GraphQL query resolving every open target's state, batched under aliases,
  plus three REST reads per target, with zero subprocesses; setup is at most
  two GraphQL calls however many targets. The new `internal/ghapi` client
  resolves its token in gh's own precedence — `GH_TOKEN`, then `GITHUB_TOKEN`,
  then `gh auth token` — so gh stays the sole authority over its
  keyring-backed store, and re-resolves once on a 401, so a long watch
  survives a rotated token. Pagination follows the `Link: rel="next"` header,
  and a rate-limited request waits out a reset landing within 60s (at most 3
  times) but returns immediately on anything further out: an hour-long
  primary-limit reset is the watch's own 30-second cadence's problem, never
  one call's. A branch with no pull request is now a query answering zero
  nodes rather than an error whose text gets matched — the
  `strings.Contains(err.Error(), "no pull requests found")` test is gone.
- **`ccx vcs ship` breaks a nearest-bookmark tie by who holds the
  candidates.** Two bookmarks at the same nearest position resolved through
  the trunk alias alone; when a sibling working copy has one of them checked
  out, that bookmark is the sibling's, not an equal alternative here. Ship now
  keeps the one candidate no sibling holds and says so — `bookmark <kept>
  (chosen over <other> held in <path>)` — answering only when exactly one is
  left standing, because holder data is not total: a colocated jj working copy
  is detached and reports no holder at all. The same lookup explains the one
  heal git refuses for a reason outside this working copy's own state:
  recovering a detached HEAD onto a branch a sibling holds now names the
  holder instead of surfacing git's bare refusal.

### Fixed
- **`ccx vcs reviews` ended a queue-merged watch as closed.** The terminal verdict
  read `mergedAt` and a `MERGED` state alone, neither of which a pull request the
  Graphite merge queue landed carries — so `ccx vcs reviews` and `ship --reviews`
  both closed a watch of a successful merge with `◆ pr#N closed` and counted it in
  the closed column. On the gt lane the closing account now settles it.
- **`--new-branch` was refused over an ambiguity it makes irrelevant.** When
  several bookmarks tied for nearest, ship refused before it ever consulted
  `--new-branch` — so a flag whose whole meaning is "do not use the current
  branch" was blocked on naming the current branch. The refusal's own advice
  could not be followed either: `--branch` only breaks the tie by naming one of
  the tied bookmarks, so in a repository where each candidate is held by a
  worktree, the only way through was to hijack one of them. `--new-branch` now
  settles the tie itself, and no longer pays for the unheld-candidate probe it
  never used.
- **`ccx vcs ship` submitted a graphite stack twice.** A downstack deeper than
  one branch ran `gt submit --dry-run` before the real submit, paying a second
  full network pass to print the branch list — a list ship had already resolved
  locally, and printed wrong, since without `--no-stack` the dry run covered the
  upstack the real submit drops. Its report was also discarded outright off a
  terminal. Ship now names the chain from the downstack it already holds and
  submits once. Nothing is lost from the pre-flight: `shipPreflightGT` already
  refuses a `needs_restack` anywhere on the downstack, adopts an untracked
  branch, and refuses `--amend` on trunk, all before any commit forms and with
  no network round trip.
- **`ccx vcs ship` ran the repo's pre-commit hooks in total silence.** The first
  prek pass was spawned with a nil writer, which routes the child to
  `/dev/null`, so a hook suite that takes minutes — a monorepo's formatters,
  linters, and pipeline codegen — reported nothing at all while it ran, and
  `--no-watch` did not shorten it, since it drops only the CI watch at the very
  end. That pass now streams to stderr on a terminal, with a lead-in line
  separating it from the auto-fix retry that runs the same hooks again. Off a
  terminal it stays quiet: there the only reader is a capture, which would take
  the first pass's pre-fix output for the verdict the retry actually decides.
- **`ccx vcs info` failures named a command nobody ran.** Resolving the git lane's
  trunk goes through two helpers ship and restack own, whose errors read
  `ship: git config branch.<b>.remote: …` and
  `restack: git symbolic-ref refs/remotes/origin/HEAD: …` — so a failed report
  sent the reader off to a command they had not invoked. Both helpers now take
  their caller's own prefix.
- **Ignore rules silently stopped applying in a linked git worktree.** A linked
  worktree's `.git` is a file holding a `gitdir:` pointer, not a directory, so
  `ccx repo find` joining `<root>/.git/info/exclude` resolved to nothing —
  every `info/exclude` rule that worked in the main checkout silently stopped
  applying there, no error, just more files than the ignore rules said. The
  same file-not-directory fact put the pointer itself in the listing: the
  walker excludes VCS directories, not files, and ccx walks hidden entries, so
  `ccx repo find ".git"` returned a row from every linked worktree. Both now
  resolve through the common dir.
- **`ccx vcs ship --no-push` left the jj bookmark behind.** The commit landed
  in `@-` but the bookmark never moved — a jj bookmark does not follow
  `jj commit` the way a git branch ref follows `git commit` — so after N
  `--no-push` ships it sat N commits back. The failure was silent and
  deferred: a later `jj git push` pushed the un-moved bookmark and reported
  `Nothing changed`, so the commits looked pushed and were not — reproduced
  live, four successive ships left `main` four commits behind. The move now
  runs inside the `--no-push` branch only; the push path keeps its own,
  because the rebase decision reads `bookmarks(exact:...) & ::@-` and jj's
  `::@-` includes `@-` itself, so a bookmark moved early would suppress a
  rebase the push still needs.
- **`ccx vcs diff` rendered a modified file as a whole-file addition when its
  name carries a rune Go's `%q` escapes.** The jj lane addressed every
  per-file blob read with a `root:%q` fileset pattern, and `%q` spells a
  zero-width joiner, a non-breaking space, or an ideographic space as
  `\uXXXX` — an escape jj's string grammar does not have (its vocabulary is
  `\t \r \n \0 \e \xHH`), so the pattern failed to parse. The existence probe
  then swallowed exactly that failure: an `err == nil &&` read the parse
  error as "path absent from the base revision", the base blob came back
  empty, and the modification rendered as an addition of the whole file —
  exit 0, nothing on stderr, a reviewer reading a diff that was not the diff.
  `ccx vcs show` shares the plan machinery and told the same fiction, and the
  same pattern fed ship's hunk selection, which refused such a path outright.
  Both halves are fixed. Every fileset and revset pattern now goes through
  one escaper writing the only two bytes jj's grammar needs escaped — `\` and
  `"`; raw UTF-8 is legal inside the quotes. And the existence probe
  enumerates instead of probing on both backends, so only an empty listing
  reads as absence and every failure propagates: the git side moves off
  `cat-file -e`, which exits 128 alike for a path the tree lacks and a tree
  it cannot read, onto a `--literal-pathspecs` listing — a name carrying glob
  metacharacters matches itself instead of its neighbors — with the index read
  at stage 0, which a conflicted path never carries.
- **A jj bookmark named with `@` could not ship.** `exact:foo@bar` reads to jj
  as a symbol carrying a remote, so the bookmark move and
  `jj git push --bookmark` both refused to parse it — after the commit had
  already landed — and auto-discovery broke one step earlier: jj quotes a
  name it would otherwise reread as a symbol, so the readback returned the
  name quotes included and ship failed with `bookmark "\"foo@bar\"" not
  found`. Every pattern and revset ship builds now carries the name as jj's
  quoted exact string — Go's `%q`, which the revsets used, writes escapes
  jj's grammar lacks, so a name carrying a rune like a non-breaking space
  broke them the same way — the readback, trunk discovery included,
  round-trips through `escape_json()` and a JSON decode rather than
  reimplementing jj's own escape table, and the recovery hints arrive
  shell-quoted, so the command a refusal says to paste is one jj accepts.
  Such a bookmark is discovered, moved, and pushed intact.
- **`ccx vcs restack` reported success over branches Graphite skipped.** gt
  splits one sync across its two streams and exits 0 either way: it names a
  branch it declined to restack — checked out in a worktree, frozen,
  merging — on stdout, and reports a trunk it could not pull as an
  `ERROR:`-prefixed line on stderr; off a terminal ccx discarded both on a
  zero exit, and the verdict came from before/after SHA comparisons of HEAD
  and the local trunk ref, which gt can fail to advance — a sibling checkout
  holding trunk with conflicting unstaged changes, a trunk that cannot
  fast-forward — while still exiting 0 without declining a single branch. So
  a skipped branch read as restacked, a stack sitting behind the trunk
  everyone else sees read as current, and the one line saying why never
  reached the user. Every stack branch now takes its own ancestry verdict
  against the remote-tracking trunk — the ref gt's own fetch writes before it
  ever touches the local branch; one that does not exist refuses with the
  `git fetch` to run, and the probe asks for it fully qualified, because git
  resolves a short `origin/main` through local branches and tags first, so a
  decoy named `origin/main` would answer in the verified ref's place with
  nothing but a stderr warning. Everything gt printed is kept: a decline is
  read off whichever stream carried it and believed over ancestry — a branch
  gt named never counts as restacked, and one that nonetheless sits on trunk
  says both facts rather than folding into either bucket —
  `restacked 2 of 3 · trunk main · skipped b (frozen; already on refs/remotes/origin/main)`
  vs `skipped b (merging)` — and gt's own `ERROR:`/`WARNING:` lines are
  re-emitted to stderr, so a report that says the stack is behind arrives
  with gt's reason why; exit 0 stays a success, since the verdict already
  carries that fact. And the summary names the working copy holding trunk —
  `trunk main (checked out in /w/trunk)` — because when the whole stack reads
  behind with nothing declined, that checkout is where the pull stopped. A
  pre-flight refuses before gt runs at all when another working copy holds a
  stack branch, since git will not move a branch a sibling checkout has
  checked out.
- **A trunk that is not `main` surfaced git's raw fatal.** With a remote HEAD
  nobody set, resolving the default branch falls back to probing
  `refs/remotes/<remote>/main` then `master` — but the probe ran
  `git show-ref --verify` without `--quiet`, and without it a missing ref is
  a `fatal` at exit 128, never the exit 1 the fallback continues on. The
  `master` arm was unreachable, so `ccx vcs restack` and `ccx vcs info`
  answered any such repo whose trunk is not `main` with git's own
  `not a valid ref` instead of the trunk — or instead of the
  `git remote set-head` hint written for exactly that state.
- **`ccx vcs ship` could adopt a git tag as trunk.** `git symbolic-ref
  --short refs/remotes/origin/HEAD` prints whatever origin/HEAD names at
  exit 0, a tag as readily as a branch, and ship trimmed `origin/` off the
  answer without checking the trim took — so an origin/HEAD pointed at
  `refs/tags/v1` made `v1` the trunk. No branch carries that name, so every
  ship read as off-trunk, and the on-trunk protections — starting a branch on
  someone else's repository rather than committing where a protect hook will
  reject it — silently disarmed. A target outside `origin/` now refuses with
  the `git remote set-head origin -a` to run; the empty answer keeps its one
  meaning, the unresolved ref of a local-only repository. restack and info
  already refused this through their own resolver; ship now matches.
- **`ccx vcs ship`'s hook run refuses a held index lock and scrubs git's
  environment from hook children.** A git index lock another process held
  failed the hooks opaquely; ship now refuses up front with the lock's path
  and mtime, because only the age separates a live sibling session (seconds)
  from a crashed process's leftovers (hours) — and the lock checked is this
  checkout's own, beside its index, which sibling worktrees do not share. And
  hook children inherited `GIT_DIR`/`GIT_WORK_TREE`, each pinning git to
  whatever checkout invoked ccx — which, invoked from a linked worktree, is
  not the checkout being committed; both are scrubbed from the environment
  prek's children see.
- **The first `ccx format` after install could time out.** `runEngine` started
  its 10s per-call deadline before the one-time WASM compile, so the compile
  spent the call's own budget: a cold compile measured 0.35s on a warm host
  and 10.4s under `-race` — past the whole limit. The engine now loads before
  the call deadline starts, so a slow host's first conversion pays the compile
  in full and the 10s bound only the call it was written for.

## [0.39.0] - 2026-07-28

### Fixed
- **The graphite reachability gate was inert in every release since 0.35.0.** The
  gate exists to keep `ccx vcs ship` off the gt lane in a repo Graphite cannot
  submit for — the failure it was written for is a commit cut onto a gt branch
  and then a `gt submit` that refuses. Its probe was capped at 5s. Measured over
  60 runs across three repos, `gt auth --no-interactive` answers in 7.0s at the
  median, 10.9s at p95, and 13.1s at the slowest, with a separate reading at
  17.9s: the cap sat below every single observation, so every probe timed out,
  every timeout read as "unknown", and unknown kept the lane. `ccx vcs info`
  reported `reachable: true` for a probe that never answered. The cap is now 20s,
  and the constant carries the distribution it came from — the 5s value was
  derived from one ~1.5s sample, which is the whole defect.

### Changed
- **An unanswered probe demotes to jj/git instead of keeping the gt lane.** The
  verdict is now three-valued — reachable, refused, unknown — and only an
  explicit yes keeps the lane; gt's own ready line is the only thing that counts
  as consent, so an exit 0 that never confirms submittability is unknown rather
  than assent. A demoted lane names its reason on the ship report line as it
  already did for a refusal. This is deliberately fail-safe: the gate cannot go
  inert again without also being visible, and the cost is that a network blip on
  a genuinely-synced repo pushes a plain branch outside the live stack.
- **An unknown verdict is cached for 60s.** Reachable answers cache for 24h and
  refusals for 1h; unknown now caches too, briefly. Under demote-on-unknown a
  cached unknown is a cached *demotion*, so caching it costs nothing in safety —
  while not caching it makes every command during an outage pay a fresh 20s
  probe to re-derive the same answer. 60s is short enough that a transient blip
  self-heals almost immediately. The on-disk record's schema version bumps, so
  verdicts cached by an older binary read as a miss.
- **`graphite.reachable` in `ccx vcs info --json` is a string, not a boolean.** It
  carries the probe's own verdict — `"yes"`, `"no"`, or `"unknown"` — with a new
  `graphite.reason` explaining anything but a yes, and stays absent for a repo
  never probed at all. A boolean could not distinguish a repo Graphite refuses
  from one nobody could get an answer about, which is exactly the conflation that
  hid the inert gate. The human line gains
  `reachability unknown (gt auth did not answer within 20s)`.
- **A probe cannot be outlived by a process it forked.** The deadline killed `gt`
  itself, but a grandchild holding the inherited stdout pipe kept the read
  blocked, so a forking probe could outlast its own cap. Probes now lead their
  own process group and cancellation SIGKILLs the group; `exec.Cmd.WaitDelay`
  backstops every `render.RunCLI*` helper, which previously would block forever
  on a descendant that outlived its parent. Only the probe path takes the changed
  signal semantics — the unbounded callers get the backstop alone.
- **`vcs.GraphiteRepo` distinguishes "no Graphite config" from "could not tell".**
  It collapsed every lookup failure into "not a graphite repo", so a linked
  worktree whose gitdir pointer git cannot resolve — a broken repository —
  answered the same as an ordinary repo without Graphite. Only a missing config
  answers false now; anything else propagates.
- **`ccx vcs restack` reports a declined graphite lane.** It resolved the lane and
  then dropped the explanation on the floor, so a restack that quietly ran on
  jj/git looked identical to one that took the gt lane. `ship` and `reviews`
  already surfaced this.

## [0.38.0] - 2026-07-28

### Removed
- **BREAKING: `--scope` is gone; `-g/--glob` is the one path selector.** It was a
  second spelling of the same idea, and the code had already admitted as much —
  the glob module owned the metacharacter test that gated scope validation, the
  ripgrep lane *derived* a scope by peeling the anchor off the globs, and a
  file-valued scope was silently coerced into a path operand. Two selectors that
  compose by accident teach a model of narrowing that does not exist, so
  the flag is deleted outright, with no alias and no deprecation window: consumers
  are agents that re-read `--help`, the docs, and the live MCP schema each session,
  and cobra's `unknown flag` is the loud failure the alternative would have hidden.
  Migrate `--scope D` to `-g D`, `--scope D --glob G` to `-g 'D/**/G'` (slash-less
  `G`) or `-g 'D/G'` (slashed `G`), and a file-valued `--scope F` to a positional
  `F` on `code grep` or `-g F` on `vcs diff`/`vcs show`. `code symbol`, `vcs diff`,
  and `vcs show` gain `-g` to replace it; on `repo find` the globs are positionals,
  so `--scope D` becomes a bare `D` operand rather than a flag; `code deps` loses
  selection entirely, since its `--scope` was validated on every call and then read
  by nothing. One frame difference survives the unification: a slashed glob is
  cwd-relative on the search ops and repo-root-relative on `vcs diff`/`vcs show`,
  which filter git's changed-file list rather than walking the tree.
- **BREAKING: the MCP selector fields collapse to one `globs` array.** `glob` and
  `scope` on `ccx_code_grep`, `ccx_repo_find`, `ccx_code_symbol`, `ccx_vcs_diff`,
  and `ccx_code_replace` each become a single ordered `globs: string[]`, so a
  client can express the exclusion and ordering the CLI has taken since 0.37.0
  instead of one glob plus a directory. `ccx_code_deps` drops selection with the
  CLI flag. In `ccx exec`, `grep(text, globs=…)`, `symbol(name, globs=…)`,
  `find(globs)`, and `diff(source, globs=…)` change shape the same way, and
  `deps(path)` loses its parameter.

### Changed
- **`vcs diff`/`vcs show` filter changed files through `MatchGlobs`, not a path
  prefix.** The filter stays stat-free, so a path the diff *deletes* still matches
  the glob naming it — a prefix test that touched the disk would drop exactly the
  files a diff exists to report.
- **`code symbol` takes its outline root from the anchor its includes share.** A
  glob narrower than its anchor, or one that only excludes, then decides membership
  through `MatchGlobs`, and the reference scan carries the same list — so
  `-g '*.go'` narrows a whole-tree lookup, which no directory scope could express.

## [0.37.0] - 2026-07-27

### Changed
- **`--glob` is a repeatable, ordered glob list, with `-g` as its shorthand.**
  `backend.Args.Glob` becomes `Globs []string`, and every engine evaluates the
  list through `backend.MatchGlobs` in ripgrep's dialect: a leading `!` excludes,
  last match wins, includes form an OR-whitelist. Repeating the flag was
  previously silent last-wins — `-g '*.go' -g '*.md'` searched only the Markdown
  — and now returns the union. `ccx repo find` takes several globs the same way.
  A metachar-free glob naming a real directory normalizes to `dir/**`, since
  ripgrep matches nothing against a bare directory name; this is `repo find`'s
  existing rule promoted so every lane shares it.

### Fixed
- **A slashed glob matches the full path instead of collapsing to its basename.**
  `-g 'internal/semsearch/*.go'` was peeled to `-g '*.go'`, which ripgrep matches
  against the basename at any depth, so it returned all 52 `.go` files beneath
  that tree rather than the 2 direct children. Globs now ride in full-path form
  under a relative anchor. An absolute operand is the exception and drops the
  anchor: ripgrep reads a leading `/` as gitignore's root anchor and strips it,
  so an absolute glob matches nothing at all — behavior there is unchanged.
- **`ccx code replace` honors a glob over explicit file operands.** ast-grep's
  `--globs` never filtered an explicit file operand — positively or negatively —
  so `code replace -g '!vendor/**' <file>` searched the file regardless. Explicit
  operands now route through the same prefilter the grep lane uses: directories
  pass through for native recursion, regular files survive only if the glob list
  keeps them, and filtering everything away is a loud error rather than an empty
  result.
- **An absolute glob outside `--scope` no longer silently overrides it.** In
  `repo find`, such a glob re-rooted the walk and ignored the scope that was
  asked for; it now selects nothing, which is what the contradictory input means.

Anchoring stays deliberately strict: it fires only when every include carries the
same literal anchor, because ripgrep applies each `-g` under every operand, so
peeling disjoint anchors would silently widen the search.

## [0.36.0] - 2026-07-27

### Fixed
- **A `grep -v <dep-name>` filter stage no longer blocks the whole pipeline.**
  `grep -rn foo . | grep -v node_modules` was denied with the dep-reader steer —
  a message telling the caller to stop reading dependency source, for a command
  that was excluding it. `v` was missing from the guard's `BOUNDED_BOOL_SHORT`
  arity table, so the tail stage failed to lex; the policy steer then fell back
  to the raw path-like tokens, which carry the *pattern* token, and
  `has_dependency_segment` matched it. Steers run before the pipe escape, so one
  filter stage killed the pipeline. Membership in these tables is arity, never
  rewritability: `-v` still declines the rewrite, pinned by a test, so an
  inverted grep can never become a non-inverted `ccx code grep`. The same hole is
  closed for `--invert-match` and for rg's `-P`/`-U`/`-b` — which means those
  tree-wide floods now block as `grep -P` always has, with the `… | rg` escape
  hatch unchanged. `-V`/`--version` stays out of rg's table, since an
  operand-less rg is tree-shaped and lexing it would block a version probe.
- **A blocked search now names what refused to map.** The flood block offered
  "a PCRE/exotic-regex pattern, an unmappable flag, or a mixed/out-of-repo
  target" and left the reader to guess — which is exactly how the incident above
  got misdiagnosed as an `--include` failure. `grep_parse`/`rg_parse` now return
  a `Decline` carrying the offending flag or pattern verbatim, so the message
  reads ``this one didn't map: the `-rv` bundle carries `-v`, which has no ccx
  equivalent``. Policy steers, which have no parse behind them, stay reasonless
  rather than inventing a cause.
- **Negated globs fail loudly instead of returning the wrong files.** A leading
  `!` in `--glob` was half-wired: with explicit path operands the search routed
  through `doublestar.Match`, which has no top-level negation, so a mixed
  directory-and-file operand list silently dropped the files while the directory
  kept the search alive; and the system-grep fallback emitted a literal
  `--include=!foo`, which grep treats as an fnmatch pattern — zero hits, no
  error. Matching is now negation-aware, and the fallback refuses rather than
  translating. Refusing is deliberate: exclusion is order-dependent (on BSD grep,
  reversing `--include`/`--exclude` lets the excluded file back in) and the
  fallback takes whatever `grep` is on `PATH`, so any mapping is a silent
  semantics fork.

### Added
- **`backend.MatchGlobs`**, an ordered ripgrep-dialect glob evaluator: a leading
  `!` excludes, last match wins, includes form an OR-whitelist, an
  exclusion-only list means everything-except, a slash-less glob matches the
  basename at any depth, and a slashed glob matches the cwd-relative path. It is
  stat-free, so a deleted path still matches. Two rules had to be corrected
  against real ripgrep rather than assumed: rg prunes directories during the
  walk, so an exclusion matching an ancestor kills the subtree and no later
  include revives a file under it — though each ancestor takes its own
  last-match-wins vote, so a later glob matching the directory itself does rescue
  it; and doublestar's `a/**` matches the bare directory `a` where rg's does not,
  compiled away by rewriting a trailing `/**` to `/**/*`. Groundwork for
  collapsing `--glob`/`--scope`/`--exclude` onto one selector.

## [0.35.0] - 2026-07-27

### Fixed
- **The Graphite lane is gated before any mutation.** A live
  `.git/.graphite_repo_config` routed `ccx vcs ship` to the gt lane on its own —
  a file that proves only that `gt init` once ran and carries no repo identity
  at all (trunk names and fetch timestamps, identical in every repo that has
  one), so nothing local can say whether Graphite can actually submit. On repos
  that were not synced, ship committed onto a gt branch and discovered the
  failure at `gt submit` (`ERROR: Graphite could not verify you have access to
  <owner>/<repo>`), leaving a manual rescue: fast-forward trunk to the commit,
  delete the gt branch, push by hand. Three gates now run before anything
  mutates, each demoting to the jj/git lane with the reason in a leading
  `lane <kind> (<reason>)` report segment: `ccx.nogt` set in git config (a
  durable per-repo opt-out), GitHub reporting the repository is someone else's,
  and a `gt auth --no-interactive` probe under a 5s timeout. A timeout or an
  unreachable Graphite server *keeps* the gt lane rather than demoting — a
  network blip must never push a branch outside a real stack. Verdicts cache per
  repo: 24h positive, 1h negative, unknown never. The same resolution now backs
  `ccx vcs restack` (a declined lane falls back to the jj/git rebase) and
  `ccx vcs reviews --stack` (which reports why the lane was declined instead of
  failing on a missing stack).
- **Linked worktrees detect their Graphite config.** `GraphiteRepo` stat'd
  `<root>/.git/.graphite_repo_config`, but `.git` is a *file* in a linked
  worktree, so detection always missed and ship silently took the jj/git lane in
  every one. The config is now resolved through the git common dir — the main
  worktree's `.git`, where it actually lives.
- **A refusal in the plain-git lane leaves the working copy untouched.** The
  lane cut the new branch with `git switch -c` before committing, so a failing
  prek hook or a failing `git commit` returned its error with the user standing
  on a branch that had not existed a moment earlier, and no rollback anywhere.
  The switch-and-commit region now carries a rollback that disarms structurally
  on success; a failure switches back and deletes the branch ship cut, and a
  rollback that itself fails joins its error to the original rather than
  replacing it.
- **The jj lane no longer refuses on a non-trunk bookmark.** `nearest bookmark
  %q is not trunk — pass --bookmark %q` fired on the first ship in every new
  workspace, and the answer was invariably the bookmark the caller was already
  standing on. Ship now appends to it and names it in the report.
- **A derived branch name no longer carries the session trailer.** On trunk the
  gt lane passed the commit message to `gt create`, which derives a branch name
  from it — including the `Claude-Session-Id` trailer ship appends. Ship now
  derives the name itself from the subject alone, before the trailer, and always
  passes it explicitly.

### Added
- **Branch selection resolves before the commit forms**, one decision every lane
  consumes, reported as a `branch <name>` or `created <name>` segment. On trunk
  ship appends in your own repositories and starts a branch elsewhere, so a
  commit is never formed where a `protect-<trunk>` hook will reject it and leave
  it dangling; a detached HEAD or an ambiguous trunk refuses rather than
  guesses. New flags: `--branch <name>` (commit onto that branch, creating it
  here when it does not exist), `--new-branch[=<name>]` (always start one,
  deriving the name from the commit subject when bare — an explicit name is
  spelled `--new-branch=name`, since cobra parses a bare `--new-branch name` as
  a path operand), `--append` (refuse rather than branch on trunk),
  `--allow-trunk` (let `--branch` advance a trunk you do not own), and
  `--parent <branch>` (graphite lane only). `--create` and `--bookmark` remain
  as aliases — of `--new-branch` (deprecated) and `--branch` (jj-only).
- **`ccx vcs ship` owns pull requests in every lane** through repeatable,
  branch-scoped `--pr-title` and `--pr-body-file` (`<branch>=<value>`, a bare
  value applying to the tip; `-` reads stdin once), plus `--no-pr`. The gt lane
  sets the title and body `gt submit --no-edit` leaves empty — the only way a
  downstack PR gets a body at all; the jj/git lane creates the PR it previously
  never opened, with explicit `--repo` and `--base` so a fork does not resolve
  to its parent, and never `--fill`, which would publish the commit's
  `Claude-Session-Id` trailer into the description. A field is written only when
  the invocation restates it, so a re-ship leaves a hand-edited description
  alone, and a ship that names no PR flag makes no `gh pr` call — an unflagged
  ship behaves exactly as before. `--draft`/`--publish` are no longer gt-only
  and convert an existing PR in either direction.
- **`ccx vcs info`** (alias `lane`) reports the lane a mutating command would
  take here and why — vcs kind, root, branch, trunk, dirty state, Graphite
  config/CLI/reachability, the GitHub repository record, and on the gt lane the
  downstack with each branch's PR and whether that PR has a body. `--json`,
  `--refresh`, `--no-gt`. It fails soft when GitHub is unreachable and mutates
  nothing.
- **`ccx vcs guidelines`** (alias `contributing`) fetches and caches a
  repository's contribution documents: every PR-template variant,
  `CONTRIBUTING.md`, the code of conduct, and issue config. It deliberately does
  not summarize — a consumer reproducing a PR template needs it verbatim,
  checkboxes and `<!-- -->` guidance included. The network payload caches for a
  day; local files re-read every run, since a branch switch changes
  `CONTRIBUTING.md` on disk. `--json`, `--refresh`, `--full` (issue-template
  bodies), `--budget` (per document).

## [0.34.0] - 2026-07-26

### Fixed
- **`ccx vcs ship` runs the repo's prek hooks once, not twice.** Ship ran its own
  scoped `uvx prek` pass and then let the commit fire git's `pre-commit` hook,
  which re-ran the same hooks through prek's unstaged-stash path — a path that
  checks out from the current index rather than a pinned tree and reports a
  failed restore only on stderr, so a `gt modify` dropped the staged edits and
  still exited 0. The second run added nothing, so ship now passes `--no-verify`
  to the git and gt commits it has already hooked itself. Repos ship did not hook are untouched:
  no prek config, a missing `uvx`, a jj repo with no `.git`, `--no-verify`, and
  hunk-scoped selections all leave the repo's own git hooks running, as does a
  prek config declaring a `commit-msg` or `prepare-commit-msg` stage, which
  `prek run --files` never reaches.

## [0.33.0] - 2026-07-24

### Changed
- **capt-hook guard pack: fail-open doctrine.** Guards fire only on positively
  identified offenses; every ambiguity — unparseable flags, unstattable operands,
  command substitutions, env prefixes, an untrusted cwd — falls through to allow.
  Any downstream pipe runs raw as post-processing, explicit-file and `~`-operand
  searches run raw, and an unresolvable `ccx` binary never blocks.

### Fixed
- **The grep/rg dependency steer is gitignore-driven.** The dep-reader steer now
  fires only on unambiguous dependency segments (`.git`, `.jj`, `.hg`, `.svn`,
  `.venv`, `node_modules`, `site-packages`, `dist-packages`) or directory
  operands the cwd repo's own `git check-ignore` reports ignored (batched, with
  a per-operand fallback so one out-of-repo operand can't mask an ignored one) —
  searches under `~/.config/`, `.github/`, or `~/.claude/plugins/` run raw again, an
  ignored plain file (`app.log` under `*.log`) stays raw as a bounded search,
  and `node_modules/express` (no leading dot) is now correctly steered. The
  steers also scan parsed operands first, so a pattern that merely looks like a
  dep path (`grep '.venv' README.md`) no longer blocks; raw tokens remain the
  unparseable-flag fallback.

## [0.32.0] - 2026-07-23

### Added
- **Secret masking now covers every content-bearing read surface, not just
  `ccx code read`.** `ccx code grep` masks each file's match+context block as one
  text — so multiline rules like private-key fire across lines — and masks the
  header's query echo; `ccx code symbol` masks doc comments, caller/test rows, and
  degraded matches alongside signatures and bodies; `ccx code outline` masks the
  full source before windowing, so a truncated head window cannot leak a secret's
  prefix; `ccx vcs diff`/`show`/`history` mask per-file hunks (renamed files under
  both the old and new paths), the commit header, and history subjects. Every
  surface reports fired rules in the same footer as read and gains the read lane's
  `reveal_secrets` escape hatch (CLI flag, MCP param, and exec op) — wired to fall
  through to the permission dialog rather than auto-approving raw-secret output.

### Fixed
- **The `ccx exec` discovery probe kills its whole process group on timeout.** The
  `claude mcp list` probe now starts in its own process group, and cancellation
  SIGKILLs the group while the unreaped leader still pins the pgid — descendants of
  a TERM-ignoring child no longer outlive the CLI. Windows keeps the single-process
  behavior.
- **`ccx vcs ship` auto-tracks an untracked trunk bookmark on jj.** In a fresh
  `jj git init --colocate` repo the remote trunk (`main@origin`) arrives untracked,
  so `jj git fetch` never advanced the local bookmark — leaving ship's divergence
  check blind — and `jj git push --bookmark exact:main` refused outright with
  "Non-tracking remote bookmark". Before fetching, ship now scans
  `jj bookmark list <target> --all-remotes` for an untracked same-name counterpart
  and runs `jj bookmark track <target> --remote=<remote>` against the remote the
  counterpart actually sits on — honoring a non-origin remote instead of a
  hard-coded `origin`, and tracking the push target when several remotes carry one.
  The name is passed as an exact string pattern, so a bookmark carrying an `@`
  tracks correctly. Tracking mutates no working-copy state, so a later push refusal
  still leaves the working copy untouched.
- **`ccx vcs ship` survives a remote that advances mid-ship, on both backends.** A
  concurrent push landing between ship's fetch and its push previously surfaced as a
  raw rejection (`... (non-fast-forward)`) — and on jj repos left the local bookmark
  advanced, so even a manual ship re-run tripped the conflicted-bookmark refusal.
  Ship now classifies a rejected push (git's `(non-fast-forward)`/`(fetch first)`
  per-ref reasons; jj's "unexpectedly moved") and re-fetches, re-rebases, and
  re-pushes, up to 3 attempts; the jj lane first reverts its own bookmark move via a
  targeted `jj op revert`, so retries and manual re-runs both start from a clean
  bookmark and a concurrent local session's operations are never rolled back with
  it. Exhausted retries fail with the exact manual recovery steps instead of raw
  stderr.
- **`ccx vcs ship --skip-hunk` refuses on snapshot drift instead of sweeping in a
  foreign hunk.** Skip mode ("commit everything except the named hunks") fail-opened:
  a hunk written to a selected file between `ccx vcs hunks` and the commit was
  silently committed anyway. Ship now fingerprints each hunk-scoped file by its
  listed hunk digests and, at commit time on both backends — the jj diff-tool lane
  carries the set through the selection plan, the git temp-index lane reads it in
  process — refuses a skip-mode commit that carries a hunk absent from that set,
  naming the foreign hunk(s). Only mode is unaffected: its foreign hunks stay
  uncommitted by construction, which is the commit-around-a-concurrent-session
  workflow.

### Changed
- **The git lane of `ccx vcs ship` gains the fetch-first flow the jj lane already
  had**: fetch, ancestor check, and `git rebase --autostash` onto `origin/<branch>`
  before the push; a conflicting rebase aborts back to the pre-rebase state and
  reports the conflicted files; an autostash that conflicts on pop is surfaced with
  `git stash pop` instructions instead of being parked silently. The lane targets
  the branch's configured remote (`branch.<name>.remote`, falling back to origin)
  for the fetch, the rebase base, the push, and the report, instead of hard-coding
  origin. A rejected `--amend` push never auto-retries on either backend, and the
  git amend push now tries a plain push first and force-pushes only with a lease
  pinned to the commit being rewritten — an externally refreshed tracking ref (an
  IDE background fetch, say) can no longer turn the lease into a silent overwrite
  of a concurrent push.

## [0.31.0] - 2026-07-23

### Added
- **AST chunking for TSX, CSS, SCSS, and Vue.** New tree-sitter grammars, pinned to
  the same upstream revisions as semble's language pack, replace line-window chunking
  for these extensions; the embedded grammar set grows ~330 KB to ~4.4 MB. The
  63-repo/1251-query quality gate re-ran benchmark-neutral (overall NDCG@10 delta vs
  semble unchanged at −0.0007).

### Changed
- **`ccx web search` hybrid embeddings now run the native in-process engine.** The
  per-call `uv`/Python model2vec subprocess is gone; web search shares the resident
  WASM engine machinery with code search, on the same pinned `potion-base-8M` model,
  so cached page vectors stay valid. `uv` is no longer required for hybrid ranking —
  without weights (empty cache, offline) web search degrades to BM25-only, and a
  stalled weights download now fails within a bounded window (~5 minutes, the old
  subprocess path's first-run bound, restored for the native engine) instead of
  hanging engine construction.
- **The model-weights cache is namespaced per model** (`models/<repo>/<revision>`),
  making room for the second (web) model; the code model re-downloads once (~30 MB)
  on the first search after upgrade.

### Fixed
- **Gitignore-negated files with unmapped extensions now index** (line-chunked)
  instead of silently vanishing; a repo whose only file is such a file no longer
  errors `no indexable files`.
- **Index-cache crash safety.** The persisted index's manifest, chunks, and vectors
  now share a per-write generation nonce; a crash between the three writes leaves a
  torn cache that rebuilds instead of silently pairing new chunks with stale
  vectors. Existing caches rebuild once (schema bump).
- **Malformed UTF-8 decodes with Python parity end to end.** Invalid byte sequences
  produce one U+FFFD per maximal invalid subsequence (Python `errors="replace"`
  semantics) — including on the production indexing path, which previously collapsed
  each invalid run into a single replacement — matching semble's chunk boundaries
  for malformed files.
- **Embedding calls honor context cancellation** promptly when queued behind other
  work (an in-flight sub-15 ms WASM encode still runs to completion by design).

## [0.30.0] - 2026-07-23

### Changed
- **`ccx code search` and `ccx code related` now run a native in-process semantic
  engine.** The previous implementation shelled out to the external Python `semble`
  package through `uvx`; the whole pipeline — tree-sitter AST chunking, model2vec
  embeddings via an embedded WASM module, hand-rolled BM25, RRF fusion, and a
  code-tuned ranking stack — now runs inside the `ccx` binary, so no external
  process is spawned and `uv` is no longer required for search. Model weights
  (`potion-code-16M-v2`, a pinned HuggingFace revision) download once into a local
  cache on first use. On the semble benchmark (63 repos, 1251 queries, same
  machine) the native engine matches semble's ranking quality (NDCG@10 0.853) and
  is faster on both cold index build and warm query latency. AST chunking covers 19
  languages; other file types fall back to line-window chunking.

### Added
- **Per-request `--content` filter on `ccx code search` and `ccx code related`**, and
  on the MCP `search`/`related` tools (space-separated `code|docs|config|all`,
  default `code docs`) — narrowing by content type is now available over MCP, not
  just the CLI.

## [0.29.0] - 2026-07-21

### Added
- **`ccx code grep --files-with-matches`.** The new `-l` mode prints only the
  relative paths of matching files, with the usual budget cap and explicit overflow
  footer. Glob, scope, case, word, and regex filters remain available; context flags
  are rejected because file-only output has no match frames to expand.
- **`ccx code grep` auto-escalates to regex on zero literal matches.** The literal pass
  runs exactly as before; when it finds nothing and the pattern carries a regex
  metacharacter and compiles, the search reruns as a regex and the header says so —
  `(auto-regex)` on a hit, `no matches (literal or regex)` after a double miss — so
  `a|b` works without `--regex`. Explicit `--regex` output is byte-identical to before,
  metachar-free patterns never rerun, a backslash-bearing pattern stays literal on the
  system-grep fallback (POSIX ERE reads `\v` as `v`), and a BRE-flavored miss (`\|`)
  gets the Rust-syntax hint appended after budget capping so it can't be truncated away.
- **Missing path operands resolve to their unique extension sibling.** The path-taking
  ops (`grep` operands and scopes, `read`, `outline`, `edit`, `history`, structural
  search/replace, `find`/`symbol`/`deps` scopes) resolve `pkg/events` to
  `pkg/events.py` when exactly one `pkg/events.*` exists, prepending a
  `# note: pkg/events → pkg/events.py` line; several candidates error listing them and
  a true miss errors clean at exit 3 instead of surfacing raw engine stderr.
  Glob-shaped operands pass through untouched, `vcs diff` scopes stay exempt (a deleted
  file's diff is legitimate), and `history` never hard-fails a missing path.
- **`ccx code symbol --budget`.** The symbol card joins the budget-capped commands:
  default 2000 tokens on the CLI and MCP surfaces, while `ccx exec` leaves it uncapped
  by contract like every other op.

## [0.28.0] - 2026-07-18

### Changed
- **The grep, rg, and json guard lanes are per-occurrence.** A `;`/`&&`/`|`-joined line
  now splices: the flood-risk grep/rg occurrence rewrites to its ccx equivalent (and the
  json-emitting occurrence wraps in `ccx format --`) while sibling commands survive
  byte-for-byte, where the old lanes blocked or skipped compound lines wholesale. Block
  messages are computed from the live command line via capt-hook 9.28.0's callable
  `block=`, and the search guards now see through wrapper prefixes: a wrapped flood
  search (`sudo grep foo .`) blocks instead of slipping past on the wrapper name —
  wrapped occurrences match-to-block, never match-to-rewrite. The plugin's capt-hook
  floor rises to 9.28.0 and the pack manifest to 0.8.0.

### Added
- **rg gains a grep-style bounded stat lane.** An rg whose operands all stat as regular
  files summing under the read budget runs raw instead of blocking — rg is
  recursive-by-default, so the stat lane doubles as the recursion check. Count/list-only
  flags escape the size cap; `-o`, `--json`, or a `RIPGREP_CONFIG_PATH` in the
  environment forfeit the lane.

## [0.27.0] - 2026-07-18

### Fixed
- **`ccx exec` propagates not-found across the sandbox boundary.** A script that dies on
  a missing path now exits 3 like the direct CLI read: host-op failures carry a
  structured `err_code` through the monty wire, and an uncaught not-found wraps
  `codeexec.ErrNotFound` into the exit taxonomy. A script that catches the error and
  continues still exits 0.
- **`ccx vcs ship` resolves its push target before committing** — a bookmark refusal
  leaves the working copy untouched instead of stranding a commit — and refuses an
  empty working copy instead of cutting an empty duplicate.

## [0.26.0] - 2026-07-17

### Changed
- **`ccx vcs ship` runs the repo's prek pre-commit hooks before committing.** Auto-fixes
  are re-verified and folded into the commit; a hook that still fails after the retry
  aborts the ship with nothing committed. `--no-verify` skips the gate, and hunk-scoped
  ships skip it with a `hooks hunk-skip` report segment.

## [0.25.0] - 2026-07-17


### Changed
- **`ccx code symbol` is native.** Definitions come from a whole-scope ast-grep outline
  index, which resolves the Go types, consts, and vars the old engine never could; extra
  hits rank deterministically behind an `also defined:` line. Docs are extracted from
  source comments and docstrings. `--callers` shows word references with
  enclosing-function attribution and says so in its header; `--callees` is labeled
  syntactic. A miss walks exact → case-insensitive (disclosed) → definition-shaped text
  before exiting 3 with `symbol not found`.
- **`ccx code deps` is native.** Imports come from ast-grep with per-family
  local/std/external classification and `(unresolved)` where resolution would be a guess.
  Dependents come from an import-shape-filtered ripgrep scan scoped to the importing
  language; a language without a sound needle, like Rust or C#, says `dependents not
  scanned` instead of guessing. The output ends with its method line — syntactic, not a
  build graph — and a missing path exits 3.

### Removed
- **The tilth engine is gone.** Every op it served now runs natively, computed fresh from
  the working tree on each call — the stale-index bug class cannot recur. With it went the
  router and `Backend` interface (one dispatch table serves the CLI, the MCP facade, and
  exec), the pinned-binary download machinery, and the regex layers that reparsed engine
  output. semble is the one resident MCP engine, now with a session-level test; the anchor
  canary survives as `TestAnchorsEmittedAcrossOps`, pinning that every native op emits
  content anchors at generation time.

## [0.24.0] - 2026-07-17

### Changed
- **`ccx vcs diff` is native.** Changed symbols come from ast-grep outlines of the
  before/after blobs intersected with natively computed hunks — no external diff engine,
  no regex post-processing. Renames render as `## old → new` (a clean rename says so
  instead of masquerading as a new file), untracked files appear on the git lane, jj-only
  revsets get real per-file hunks instead of `--stat` counts, git-syntax sources
  (`HEAD~1..HEAD`) resolve through git in a colocated repo, symbol classification past 30
  files discloses itself, and `--full` inlines per-file hunks. `ccx vcs show` inherits all
  of it.
- **`ccx code outline` fallback is native.** Markdown gets an anchored ATX heading
  outline; languages without ast-grep outline rules get an honestly-labeled head window
  with a precise `ccx code read --section` continuation pointer — tilth signature mode is
  gone.
- **`ccx vcs history` summarizes commits through the native diff classifier** — the
  per-commit tilth shell-out and its output-scraping regex are deleted; the `(+a/-b)`
  line-count degradation and `(added)` root-commit paths stay.

## [0.23.0] - 2026-07-17

### Added
- **Hunk-scoped ship.** `ccx vcs hunks [paths...]` lists every pending hunk as a stable
  `file:A-B#hash` ref; repeatable `ccx vcs ship --skip-hunk <ref>` / `--only-hunk <ref>`
  commit a file partially. jj cuts the partial commit through its diff-editor protocol in
  one transaction (ccx re-invokes itself as the tool); git goes through a temp index. The
  working copy is never rewritten, excluded hunks stay uncommitted in `@`, an empty or
  drifted selection refuses instead of committing, and refs round-trip from any
  subdirectory. CI installs jj so the live jj-lane tests run there.

### Changed
- **`ccx code read` is served natively.** One `os.ReadFile` shaped into anchored output
  whose header reads `# read path:A-B#hash (k of N lines)`, so stale line labels are
  impossible by construction; tilth's index is no longer consulted. Sections take line
  ranges, `#hash` anchors, and markdown ATX headings (markdown files only); a non-markdown
  symbol lookup is redirected to `ccx code symbol --body`.
- **`ccx repo overview` is native.** Languages, dirs, entry points, manifests, test counts,
  git state, and 90-day churn come from one gitignore-honoring walk plus live git; the MCP
  surface now carries the language census the CLI used to append on its own.
- **ripgrep is the only grep engine.** The tilth literal lane and its stale-zero recheck
  band-aid are gone; every `ccx code grep` runs live ripgrep (system `grep` fills in, with
  its disclosure note), so a zero is a real zero and results honor `.gitignore` on every
  lane. Content anchors are stamped at generation from a per-call line cache, `--expand`
  means context lines around each hit, and the shapes are unchanged from the flagged lane
  agents already saw.

## [0.22.0] - 2026-07-16

### Changed
- **ast-grep is a normal PATH dependency, not a vendored download.** ccx no longer fetches
  a pinned ast-grep release on first use; it resolves `ast-grep` from PATH, probes the
  0.44.0 version floor once per binary path, and errors with an install hint when it's
  absent. The Homebrew formula already installs it; other installs need
  `brew install ast-grep` or `uv tool install ast-grep-cli`.

## [0.21.0] - 2026-07-16

### Added
- **`ccx code edit --match` addresses by exact text instead of a span.** The needle is
  byte-exact and multi-line (a CRLF file normalizes needle and content to its own EOL); zero
  matches error before any write, several error listing each candidate's `line#hash` anchor
  for a scoped re-run, `--at` composes with `--match` to confine the scan to a hash-verified
  span, and `--all` replaces every occurrence with a per-match stanza report in final-file
  coordinates. Content is written verbatim — trailing spaces and a trailing newline land on
  disk untrimmed — and every error path leaves the file byte-identical. Mirrored on the MCP
  `ccx_code_edit` tool (`match`, `all` params).

### Changed
- **`ccx vcs ship` fetches first and auto-rebases onto the target bookmark.** When the trunk
  (or `--bookmark`) target is no longer an ancestor of `@-`, ship rebases the local stack onto
  it and reports a `rebased N commit(s) onto X` segment; any conflict across the rewritten set
  rolls back via `jj op restore` and errors with the conflicted commits plus manual recovery
  steps. A missing or multi-head target bookmark refuses before any mutation, an empty
  `target..@-` refuses to move the bookmark backwards, and a rerun after a failed push takes
  the no-divergence path — resume never rebases twice. Git lane unchanged; `--no-push` still
  skips the network entirely.

### Fixed
- The guard pack pins its three read-only approvers to `PermissionRequest` only, ahead of
  capt-hook flipping the `approve()` default to `PreToolUse | PermissionRequest` — they must
  never compose with `repo_find_nudge` in one PreToolUse dispatch nor override settings deny
  rules.

## [0.19.0] - 2026-07-16

### Added
- **`ccx code read` masks detected secrets by default.** gitleaks' default rules (vendored from
  v8.30.1) run over read output before budget capping on all three lanes — CLI, MCP
  `ccx_code_read`, and the exec `read` host function. A finding of 16+ bytes keeps a 4-byte stub
  and collapses to `…[masked:<rule-id>]` (a shorter finding masks whole), and a footer names the
  fired rules. The noisy `generic-api-key` entropy catch-all fires only on env-shaped files
  (`.env`, `.env.*`, `*.env`, `.envrc`, `credentials`, `.netrc`) — a high-entropy secret
  hardcoded in `.yaml`/`.json`/source is left raw, deliberately: the rule is documented-noisy on
  ordinary source and would repaint lockfile hashes. Masking covers `code read` only;
  `grep`/`outline`/`symbol`/`diff` still emit raw content (backlogged). `--reveal-secrets`
  (`reveal_secrets` on MCP/exec) prints raw and now trips a permission prompt via the guard pack.
  Pack 0.7.0.

## [0.18.0] - 2026-07-15

### Changed
- **`ccx code read` fails loudly on unresolvable paths.** A leading `~` expands textually, the
  target is stat'd before dispatch, and a missing file exits 3 with `path not found` instead of
  silently degrading into a tilth content search. A directory errors with a `ccx code outline`
  pointer — the MCP `ccx_code_read` directory listing is gone, deliberately. The CLI, MCP, and
  exec lanes share the one check.
- **Guard-pack rewrites are per-occurrence.** The cat/sed/head/find/ls/git-pager/curl-dump
  rewrite guards splice only the qualifying command of a compound line (capt-hook 9.15's
  `rewrite_command_occurrences`): `cat big.go; echo done` rewrites the `cat` and keeps the `echo`
  byte-for-byte, `cd src && find . -name '*.go'` rewrites after the `cd` so the glob roots
  correctly, and an unrewritable flood segment blocks the whole line rather than running raw
  beside a rewritten sibling. Any-occurrence conditions close the `git diff; echo done` and
  `cat go.mod; echo x` allow-holes, and `git diff > out.patch` now runs — a file redirect does
  not flood context.
- **The bare-`cat` guard is size-gated.** It rewrites only an existing file over the large-read
  cap, expanding `~` itself and emitting the quoted absolute path — no more frozen-tilde phantom
  searches; small, nonexistent, or `$`-carrying operands run untouched, and a multi-file `cat`
  blocks only when its stat-able operands sum past the cap. Pack 0.6.0; capt-hook floor
  `>=9.15.0`.

### Added
- The common ccx MCP tools — `ccx_code_read`, `ccx_code_grep`, `ccx_code_outline`, `ccx_code_search` — carry the `anthropic/alwaysLoad` tool `_meta` flag, so Claude Code keeps them in the prompt under tool-search deferral (`ENABLE_TOOL_SEARCH`) instead of hiding them behind a `ToolSearch` round-trip. A guard redirect to one lands on an already-loaded tool; the rest of the surface stays deferred, loaded on demand. Per-tool `_meta`, not a server-level `alwaysLoad`, keeps the eager set to the four workhorses and the server's connect non-blocking.

## [0.17.0] - 2026-07-14

### Added
- `ccx vcs ship` takes trailing `[paths...]` to scope the commit: in jj the paths pass through as filesets and the remainder stays in the working copy; in git the blanket `git add -A` gives way to a pathspec-scoped add plus a partial commit. A working copy shared with a concurrent session no longer forces manual `jj` steps.
- The jj push lane only auto-advances the trunk bookmark. A non-trunk nearest bookmark refuses with its name in the error, and the new `--bookmark <name>` flag advances one deliberately; in a plain-git repo the flag is an error. Bookmarks now move by exact name (`exact:` anchored — jj otherwise reads bare names as globs) and push with `--bookmark`, so a second bookmark parked on the same commit no longer rides along; a `--bookmark` name that doesn't resolve refuses with `bookmark not found` instead of jj's silent exit-0 no-op. A scoped jj commit whose fileset matches no changes still ships an empty commit — same as an unscoped ship on a clean working copy.
- `ccx exec` caches MCP discovery on disk per project for 15 minutes, so a warm cache spawns no `claude mcp list` probe; a script that references no reflected tool skips reflection entirely — no probe, no notes. Changing `CCX_EXEC_MCP_ALLOW`/`CCX_EXEC_MCP_DENY` invalidates the cache, `CCX_EXEC_MCP=refresh` forces a fresh probe, and `CCX_EXEC_MCP_TIMEOUT` (Go duration, default `30s`, up from 10s) bounds it — an invalid duration is a hard error.

### Fixed
- A discovery probe that fails past the cache TTL falls back to the last good inventory with a note, and a deadline kill reports `claude mcp list timed out after 30s` instead of the bare `signal: killed`.

## [0.14.0] - 2026-07-13

### Added
- The captain-hook dependency is explicit: `plugin.json` declares `{ "name": "captain-hook", "marketplace": "captain-hook", "version": ">=9.9.0" }` and the repo `marketplace.json` allows the cross-marketplace dependency via `allowCrossMarketplaceDependenciesOn`. The allowance is load-bearing — without it Claude Code silently skips the declared dependency at install and the attached guard pack runs with no dispatcher.
- CI vets the attach-only pack contract: `uvx 'capt-hook>=9.9.0' pack lint plugin` checks the manifest, the canonical attach entry, the dependency floor, the marketplace allowance, and that the pack loads clean.
- The ccx guard pack auto-approves the read-only ccx surface — the server-pinned `mcp__cc-context__ccx_*` query tools and a fail-closed CLI allowlist — so query calls stop hitting permission prompts.

### Fixed
- `install-binary.sh` and the release version check take the first `"version"` match in `plugin.json` — the dependencies block carries version floors of its own, which corrupted the pinned release tag into a multi-line string.
- `ccx vcs show` validates refs behind `--end-of-options`, closing a flag-injection path where a crafted ref could clobber files; `ccx web` refuses link-local and cloud-metadata hosts (SSRF).
- A tilth grep reporting zero matches is re-verified through the live rg engine before being trusted — hits in capped or minified files no longer vanish silently.
- The MCP launch floors the semantic-search dependency at `semble[mcp]>=0.5`.

### Changed
- The SessionStart pack attach runs the canonical attach-only prefix, `uvx --isolated capt-hook pack attach "${CLAUDE_PLUGIN_ROOT}/hooks"`; the `install-binary.sh` entry is unchanged.
- README: the plugin installs from its own marketplace (`cc-context@cc-context`), with the captain-hook marketplace added first so the dependency auto-installs, plus an upgrade note — `claude plugin update` silently skips newly added dependencies. The prior `yasyf/cc-skills` instructions pointed at a marketplace that no longer lists the plugin.
- Docs reposition on the measured benchmark record ([bench/FINDINGS.md](bench/FINDINGS.md)): the README and the ccx skill lead with bounded, structured output and measured accuracy on targeted questions, the `symbol`/`overview` examples are regenerated from the 0.13.0 terse defaults, the exhaustive-enumeration caveat is stated where it applies, and session-level token-savings claims are retired. The shared guides fragment (`cc-skills:ccx`) carries the same scoping to every consuming repo.

## [0.13.0] - 2026-07-12

### Fixed
- Guard pack 0.4.0: the grep guard judges each grep statement on its own flags and operands (per-occurrence, matching the rg guard) instead of requiring the whole Bash line to be a single command. Explicit data-file targets (`.log`/`.json`/…) pass textually with no stat, so files created earlier in the same compound command or addressed relative to an in-command `cd` now run as-is (`-o` is allowed on a data-file target — its output tracks the matched data). Tree-wide, directory, and recursive greps still block, and so do the flood shapes an over-broad allow would miss: `-o` over a source file (its per-match filename/line/byte prefixes multiply output past the size cap), a `GREP_OPTIONS` env that injects flags the parser never sees, a pipe-sink grep that names file operands (it searches the files, ignoring stdin), and flag-supplied empty or `-f`-file patterns.

## [0.12.0] - 2026-07-10

### Changed
- The cc-context plugin no longer registers the `PreToolUse`, `PostToolUse`, or `PreCompact` hook events in `plugin/hooks/hooks.json`. That job moves to the captain-hook plugin (capt-hook 9.0.0 or newer), now the sole registrar of the `uvx capt-hook run <Event>` dispatch commands. Claude Code does not dedupe identical hook commands across sources, so a duplicate registration double-fires every guard. The `SessionStart` entry is unchanged. It still runs pack attach and `install-binary.sh`, and the guard pack dispatches through captain-hook.

## [0.11.0] - 2026-07-10

### Added
- `--regex`/`-E` on `ccx code grep` treats the query as a regular expression (ripgrep by default, `grep -E` ERE as the fallback), and explicit file operands — `ccx code grep <pattern> file1 file2` — scope the search to named paths. Both route to the rg/grep engine, so anchored (`^class `) and multi-file queries that the literal tilth path silently 0-matched now resolve. Wired across the CLI, MCP (`regex`/`paths` on `ccx_code_grep`), and `ccx exec`'s `grep()`.
- Guard pack: a dialect-safe regex `grep` rewrites to `ccx code grep --regex` — a position-aware validator admits only constructs whose semantics are identical in grep's BRE/ERE and Rust's regex (anchors only at the ends, quantifiers never leading, digit-only intervals, no bracket expressions or backslashes); `-F` always stays literal. A bounded `grep` over explicit existing files that ccx cannot express passes straight through when the named files total under the pack's large-read threshold, with positionals emitted after `--` so flag-shaped filenames stay filenames. Tree-wide unmappable shapes still block with a pointer at the `ccx` equivalent.

### Fixed
- `ccx vcs diff --scope <file>` no longer drops the whole diff when tilth attributes zero symbol changes — the collapsed `0 symbols touched` header is expanded and the raw hunk spliced back in, on both the CLI and MCP surfaces, including paths with spaces.
- `ccx vcs diff <bogus-ref>` errors loudly instead of silently reporting no changes; each Git ref endpoint must parse via `git rev-parse`, so multi-value sources like `HEAD^@` keep working.

### Changed
- An unbudgeted `-i`/`-w`/`--regex`/multi-file `ccx code grep` defaults to a 2000-token output cap at the CLI and MCP surfaces; the uncapped `ccx exec` contract is unchanged.

## [0.10.0] - 2026-07-10

### Added
- Dependency-source search: an explicitly anchored `--glob` (a literal directory or file prefix, e.g. `.venv/lib/…/pkg/*.py`) is searched even where ignore rules would hide it, on both the tilth and ripgrep engines and across CLI, MCP, and `ccx exec`. The anchor composes with an explicit `--scope`, and a glob naming an exact file anchors to its parent directory — ripgrep's explicit-path semantics throughout (`--no-ignore-parent` on the rg route).
- `ccx repo locate` resolves Python import names (`cc_transcript` ⇄ `cc-transcript`) and emits both a `repo` row (sibling checkout) and a `package` row (installed site-packages directory + `importlib.metadata` version, interpreter resolved `$VIRTUAL_ENV` → `./.venv` → PATH). The Python row's kind is now `package` (was `python`).
- Guard pack 0.2.0: unpiped `rg` is gated at grep parity — literal-safe invocations rewrite to `ccx code grep` (context flags map to `--expand=<count>`), unmappable ones block with a dependency-source steer (`ccx repo locate <pkg>` → `ccx code grep/outline/read`) — with an exemption when every explicit target is a data file (`.log`/`.json`/`.yaml`/…). Hidden-segment and git-ignored path operands now block for both `grep` and `rg` instead of rewriting to a glob a stale binary would silently 0-match.

### Fixed
- A no-match literal `ccx code grep` no longer exits 2 with tilth's `not found: <path>/<query>` path-fallback error — it prints the house no-match output; a nonexistent `--scope` still fails loudly.
- `ccx repo find --scope` reaches the tilth CLI route (the scope was silently dropped).

## [0.9.0] - 2026-07-09

### Added
- Guard pack: two hooks protecting the cc-guides rendered-artifact regime. A direct `Edit`/`Write`/`MultiEdit`/`NotebookEdit` of a rendered artifact is **blocked** — the predicate fires only when a sibling `.claude/fragments/<repo-relative-target>/layout.toml` exists AND the target's first two lines carry the `cc-guides … | GENERATED` banner, so an unmanaged file or a file that merely contains the word GENERATED is never touched — with a message steering to the fragments plus `cc-guides render`. An edit to a render **source** (any file under `.claude/fragments/`, or `guides/` in the cc-skills content repo) draws a one-shot nudge to re-render and commit the fragments and regenerated artifact together.

## [0.8.0] - 2026-07-08

### Added
- `-i`/`--ignore-case` and `-w`/`--word` on `ccx code grep`, routed to PATH-resolved ripgrep (`--json`, fixed-strings) and reshaped into the house grep format so anchors and budget capping apply unchanged. System `grep -rnFI` is the fallback when `rg` is absent, with filesystem-validated line parsing; hidden and binary files are skipped and `.gitignore` is not applied, disclosed in an engine note. Wired across the CLI, MCP (`ignoreCase`/`word`/`scope` on `ccx_code_grep`), `ccx exec`'s `grep()`, and the proxy dispatch.
- `--scope <path>` on `ccx code grep`, passed through to tilth.
- The plugin installer best-effort ensures ripgrep (`brew install ripgrep`, backgrounded) at session start; skipped silently without brew.
- Guard pack: block-only hooks now rewrite mappable commands in place via `updatedInput`, each with a disclosure note — raw `grep` → `ccx code grep`, bare `git diff`/`git show`/`git log -p <path>`/`jj diff` → the `ccx vcs` equivalents, unpiped `curl`/`wget` page dumps → `ccx web read --full`, unbounded large `Read`s → a 100-line window. Unmappable shapes keep the original block message; rewrites that need a newer binary gate on a `ccx_supports` probe of the installed CLI.

### Fixed
- `ccx vcs show <ref>` resolves git symbolic refs (HEAD, HEAD~N, branches, tags) in jj-colocated repos instead of handing them to `jj log -r`; embedded-`@` sources (`release@1` vs `main@origin`) classify by attempted git resolution, consistently across the show and diff paths.
- rg engine hardening: positional paths ride behind `--` so a flag-like scope cannot be misparsed; base64 `bytes` payloads in `rg --json` output decode instead of emitting blank match lines; the grep fallback's path validation requires regular files, so a directory named like a path prefix cannot steal the split.

### Changed
- CI builds on Go 1.26.5 (GO-2026-5856), uses `actions/cache@v5`, and runs the guard-pack pytest suite over the whole `plugin/hooks/` directory.

## [0.7.0] - 2026-07-08

### Added
- `ccx web` op family: `outline <url>` (heading tree with stable `§` section refs), `read <url> --section <ref>` (budget-capped section subtree with prev/next nav; `--full` for the whole page), and `search <url> "<question>"` (top-k relevant chunks with `§` cites; hybrid BM25 + local embeddings). Pages cache for 24h; `--refresh` refetches. Mirrored over MCP as `ccx_web_outline`/`ccx_web_read`/`ccx_web_search`.

## [0.6.1] - 2026-07-07

### Fixed
- The `ccx exec` host-call size valve now covers structured returns (maps and lists), not only strings — a large non-string host return no longer slips the per-call limit.

## [0.6.0] - 2026-07-07

### Added
- `ccx exec` works on Intel Macs (darwin/amd64) — every platform with uv now runs the sandbox.

### Changed
- The exec sandbox runs pydantic-monty 0.0.18 in a per-run Python subprocess launched via uv (already a formula runtime dependency). The pinned interpreter is 9 releases newer than the embedded binding and includes the upstream fixes for the partial-future-resolution bugs it needed a workaround for.
- Binaries shrink from ~25–27 MB to ~11 MB with the embedded runtime gone.

### Removed
- The embedded gomonty/monty runtime and its dylib.
- macOS notarization and the disable-library-validation entitlement (no dylib left to exempt).

## [0.5.1] - 2026-07-06

### Changed
- Format classifier: a prose-like field of 2 KiB or more unwraps to prose regardless of its share of the payload — a big body (release notes, a PR description) reads better unwrapped than TRON-compressed, even when metadata rides along.
- `ccx format` auto mode keeps the classifier's ranking on near-ties: a later candidate must beat an earlier one by more than 5% in bytes to displace it. The guard that auto output never exceeds compact JSON is unchanged.
- The plugin installer provenance stamp points at the canonical cc-skills template.

### Fixed
- `ccx exec` host calls awaited via `asyncio.gather` could execute more than once: the embedded runtime re-awaits still-pending calls after a partial resume, re-running the host function — a duplicated side-effecting tool call (`sh()`, an MCP tool). Each waiter now memoizes its result, so every host call runs exactly once.

## [0.5.0] - 2026-07-05

### Added
- Brew-first self-provisioning plugin installer: at session start the plugin resolves `ccx` via Homebrew when available, otherwise downloads the bare release binary and verifies its sha256 checksum. The downloaded payload lives under `CLAUDE_PLUGIN_DATA`, durable across plugin updates; `plugin/bin` holds only symlinks.
- Bare per-arch binaries published on each release alongside the archives, with sha256 checksums — the artifact the installer downloads and verifies.
- `ccx format [-- <cmd>]` re-encodes JSON/NDJSON (a wrapped command's stdout, or stdin as a pipe filter) into the leanest encoding for its shape, picked by a classifier: payloads under 200 bytes stay compact JSON; a prose-dominant payload unwraps to the prose plus XML-ish metadata tags; a uniform array of objects becomes a markdown table (small) or a CSV/TSV byte shootout (large), with TOON entering only at 100+ rows when it beats both; repeated nested shapes become TRON; heterogeneous arrays become JSONL; everything else stays compact JSON. Auto output never exceeds compact JSON by bytes; `--format=X` forces one encoder. Exposed over MCP as `BashFormat`.
- A TRON encoder: repeated key-sets compile to class declarations (`class A: region,zone,tier`) with each instance a positional call.

### Changed
- `ccx --version` on release builds prints the exact release tag (e.g. `v0.5.0`).
- `ccx exec` structured returns ride the same classifier as `ccx format` instead of always rendering as TOON or compact JSON.

### Removed
- The version-pinned bootstrap shim (`plugin/bin/ccx`); the self-provisioning installer replaces it.
- `ccx toon`, the `BashToon` MCP tool, and `--force-toon`; `ccx format`, `BashFormat`, and `--format=toon` replace them with no back-compat alias.
- The `ccx hello` placeholder command.

## [0.4.0] - 2026-07-03

### Added
- `ccx exec [script]` runs a Python (monty-subset) script in a sandbox whose async host functions are every ccx query op, a gated `sh(cmd)`, and every stateless MCP server's tools (auto-reflected from `claude mcp list`, no flag needed). Only the script's return value enters context, rendered as TOON or compact JSON and capped at `--budget`. Scripts arrive as an argument, `--file`, or stdin; `--list-tools` prints the host-function catalog and the Python-subset rules. Unavailable on Intel Macs (darwin/amd64 — the embedded monty runtime ships no library there); every other command works.
- `ccx_exec` and `ccx_exec_tools` MCP tools expose the exec surface on the facade, backed by a resident engine.
- `CCX_EXEC_MCP=off` disables MCP auto-reflection; `CCX_EXEC_MCP_DENY` / `CCX_EXEC_MCP_ALLOW` (comma-separated server names) override the stateless classifier. Built-in denies: cc-context itself, `plugin:cc-review:*`, and any command whose basename is `ccx`.

### Changed
- Binaries grow from ~11 MB to ~25–27 MB on monty-supported targets (the embedded Python runtime).

### Fixed
- Command results now print to stdout, not stderr (a cobra wiring bug). Behavior change: scripts that captured results from stderr must read stdout instead.
- Plugin hooks no longer run twice when the host project also wires capt-hook: the plugin attaches its pack once at SessionStart and dispatches every event through the canonical command Claude Code can dedup.

## [0.3.0] - 2026-07-03

### Added
- `ccx vcs ship [-m <msg>]` — jj-aware commit, push, and `gh run watch --exit-status` in one call.
- `ccx vcs show [ref]` — commit message plus a structural per-file diff of one commit.
- `ccx vcs history <path>` — per-commit changed symbols for a file, rename-aware.
- `ccx repo locate <name>` — resolve a sibling repo, Go module, or Python package to its on-disk path; exit 3 when unresolved.
- `ccx toon [-- <cmd>]` re-encodes a command's JSON/NDJSON stdout as TOON (or compact JSON when smaller), passes non-JSON through verbatim, and propagates the exit code; also a pipe filter. Exposed over MCP as `BashToon`. A guard auto-rewrites JSON-flagged commands (`--json`, `-o json`) to run through it.
- Content-hash anchors (`#hash`) on spans across outline, grep, symbol, diff, and search output.
- Guard pack: new blocks on `head`/`tail` of files, `git show`, `git`/`jj log -p`, pager diffs, and manifest `cat`, plus a stateful session gate against full-file and post-edit re-reads.

### Changed
- Breaking: commands are namespaced into `ccx code` / `ccx repo` / `ccx vcs` groups (`ccx outline` becomes `ccx code outline`, `ccx diff` becomes `ccx vcs diff`, and so on) with no back-compat aliases; `ccx toon` and `ccx mcp` stay top-level. MCP tools renamed to match (`ccx_code_read`, `ccx_vcs_diff`, and the rest).

## [0.2.1] - 2026-06-25

### Changed
- `ccx outline` routes through ast-grep, falling back to tilth.

### Fixed
- Structural diffs splice raw textual hunks under tilth's empty `(0 symbols)` sections, jj-aware.
- `ccx symbol` falls back to an ast-grep lookup when tilth misses a Go top-level type declaration.
- tilth's silent "not found" result now surfaces as a real error on both the CLI and MCP surfaces.

## [0.2.0] - 2026-06-23

### Added
- `ccx replace <pattern> <rewrite>` — ast-grep structural find-replace: preview by default, `--apply` to write, `--force` past the 20-file cap. Exposed over MCP as `ccx_replace`.
- `ccx search` routes by query kind: natural language runs semantic (semble), an ast-grep metavar pattern runs structural. `--semantic`/`--structural`/`--literal` override the route; `--explain` prints it.
- ast-grep is bundled through the same resolver as tilth: configured binary, then PATH, then a checksum-verified pinned download.
- Guard pack blocks raw `grep` and auto-rewrites `cat`, `sed` ranges, `ls -R`, and `find` enumeration to their ccx equivalents.

### Changed
- Distribution moved from a goreleaser cask to a Homebrew formula (`brew install yasyf/tap/ccx`) with ast-grep and uv as dependencies.

## [0.1.1] - 2026-06-21

### Changed
- `ccx outline` elides function bodies (signature mode) — roughly 75% smaller output.
- Search snippets default to 10 lines instead of the full chunk.
- `ccx overview` appends a language census.
- The unbounded-Read guard threshold dropped from 50 KB to 20 KB.

### Fixed
- `ccx related` splits `file:line` into the two arguments semble expects.
- The plugin bootstrap shim requested the wrong release asset name and 404'd.

## [0.1.0] - 2026-06-20

### Added
- Initial release: the `ccx` CLI and the `cc-context` facade MCP, a single Go binary over swappable backends — semble for semantic search, tilth for symbols, outlines, and diffs — with token-budgeted output, line numbers, and explicit overflow markers.
- jj-aware diff translation.
- Claude Code plugin: facade-only MCP registration, a bootstrap shim that provisions the `ccx` binary, a capt-hook guard pack that blocks token-heavy primitives (unbounded `Read`, bare `cat`, `ls -R`, broad `git diff`) with escape hatches, and the `ccx` skill.

[0.11.0]: https://github.com/yasyf/cc-context/compare/v0.10.0...v0.11.0
[0.8.0]: https://github.com/yasyf/cc-context/compare/v0.7.0...v0.8.0
[0.7.0]: https://github.com/yasyf/cc-context/compare/v0.6.1...v0.7.0
[0.6.1]: https://github.com/yasyf/cc-context/compare/v0.6.0...v0.6.1
[0.6.0]: https://github.com/yasyf/cc-context/compare/v0.5.1...v0.6.0
[0.5.1]: https://github.com/yasyf/cc-context/compare/v0.5.0...v0.5.1
[0.5.0]: https://github.com/yasyf/cc-context/compare/v0.4.0...v0.5.0
[0.4.0]: https://github.com/yasyf/cc-context/compare/v0.3.0...v0.4.0
[0.3.0]: https://github.com/yasyf/cc-context/compare/v0.2.1...v0.3.0
[0.2.1]: https://github.com/yasyf/cc-context/compare/v0.2.0...v0.2.1
[0.2.0]: https://github.com/yasyf/cc-context/compare/v0.1.1...v0.2.0
[0.1.1]: https://github.com/yasyf/cc-context/compare/v0.1.0...v0.1.1
[0.1.0]: https://github.com/yasyf/cc-context/releases/tag/v0.1.0
