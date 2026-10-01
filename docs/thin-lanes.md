# Create and work in thin lanes

Use thin lanes for agent work that needs a sparse working copy and bounded
history in a separate ccx-owned store.

## Create a thin lane

From a full checkout with trunk checked out, create a lane:

```sh
ccx vcs stack new agent-work --thin
```

ccx creates the thin store on first use, then cuts the lane as a linked
worktree of that store. From a store lane, every new lane is a linked
worktree of the same store and is sparse like its caller, with or without
`--thin` and regardless of the `CCX_STACK_NEW` default. Every lane also checks
out tracked top-level `.claude` and `.agents` directories for project hooks,
settings, skills, and instructions, alongside root files and inherited or
`--include` directories. Only `--no-checkout` skips materializing files
there; stacking works as usual.

The store is a non-bare clone at `~/.claude/stores/<key>/<repo>`. Only a
checkout at the exact path its own origin derives counts as a thin store.
`<repo>` is the source checkout's directory name. Lanes use the same pool
as other lanes: `~/.claude/worktrees/<repo>/<lane>`. Checkouts of one remote
share a store only when their directories have the same name; a checkout
named differently gets its own store under the same key.

`<key>` is the first 12 hex digits of SHA-256 over the canonical remote. For
hosted remotes (URLs or scp-style `user@host:path`), that is the lowercased
host, plus `:port` only for a non-default port, then `/` and the path with
surrounding slashes and one trailing `.git` removed, preserving path case.
Default ports are 22 for ssh, 80 for http, 443 for https, and 9418 for git.

A local origin (an absolute path or `file://` URL) is used as given after
cleaning, preserving case and `.git`. A `file://` URL naming a host other
than `localhost` is refused. Different spellings of one repository can get
separate stores; ccx never merges remotes it cannot prove identical.

On first use, the default clone uses
`git clone --no-local --no-checkout --depth=256 --filter=blob:none --no-tags --single-branch --branch <trunk> --ref-format=files`.
`--depth N` changes the initial history depth; its default is 256. Passing
`--depth` explicitly when the store already exists is refused. Existing
stores are never deepened implicitly.

The store fetches only trunk, sets `origin/HEAD` and trunk tracking, and keeps
only root files in its own sparse cone checkout. It persists
`feature.experimental=true`, `feature.manyFiles=true`, and `pack.threads=2`.
ccx initializes Graphite there when the source uses Graphite. Creation only
checks out tracked files: it runs no Git hooks, project hooks, or setup
scripts and installs no dependencies. Set up dependencies manually inside
the lane.

Output segments are joined by ` · `: `created thin store <path>` on first
use, an optional deepening report, `cut <name> onto <parent>`, then the new
lane's path last. Enter that path to work in the lane.

To share the full checkout's history instead, run this from that checkout:

```sh
ccx vcs stack new full-work --full-history
```

`--full-history` creates a linked worktree of the checkout you run it from.
The flag is refused inside the ccx thin store.

## Make thin lanes the default for agents

Thin lanes cannot see the source checkout's local cc-notes data, and notes
written in a thin lane stay in the store. ccx does not copy or sync notes.
Keep thin mode opt-in: use `--thin`, or set `CCX_STACK_NEW=thin` per agent
only where that separation is acceptable. Do not set it globally. In an
agent shell:

```sh
export CCX_STACK_NEW=thin
ccx vcs stack new agent-default
```

Keep it unset in human terminals to retain the existing behavior: a linked
worktree sharing the checkout's history. `CCX_STACK_NEW=full` selects that
same behavior. Inside the store, the `full` environment default still cuts
a linked worktree of the store, sparse like its caller; it does not supply
full history.

Explicit `--thin` and `--full-history` flags override the environment
default. Any value other than `thin` or `full` is an error.

## Check out more directories

Thin lanes inherit the source checkout's per-worktree sparse patterns when
active. Every lane also checks out root files and tracked top-level
`.claude` and `.agents` directories; creation runs no hooks or setup scripts
and installs no dependencies. For a repository with `src` and `tools`
directories, include both when creating the lane:

```sh
ccx vcs stack new agent-files --thin --include src --include tools
```

`--include DIR` is repeatable and, outside the store, requires `--thin` or
`--sparse`. It is refused with `--no-checkout`. From inside a lane, expand
the checkout as work requires:

```sh
git sparse-checkout add src tools
```

Other paths imported by agent instructions are not discovered automatically.
For a `CLAUDE.md` or `AGENTS.md` import such as `@docs/object-hierarchy.md`,
pass `--include docs` at creation or run `git sparse-checkout add docs`
inside the lane.

Blobs download on demand. Dependency setup remains manual; keep mutable
install state and build outputs private to each workspace, as described in
the [stack conflict guide](stack-rebase.md).

## Stack on a published parent

When a parent exists only in the source checkout, use `--published-parent`.
From the source checkout of a Graphite stack, create a child of a parent
already published onto trunk:

```sh
ccx vcs stack new agent-child --thin --parent parent-work --published-parent
```

ccx verifies the parent's publication receipt against the source checkout
and the remote, brings the parent into the store at its published head, and
copies the receipt. The store's Graphite metadata records that parent as
frozen: no submit from the store pushes or rewrites it. Only parents
published onto trunk can be adopted. An unpublished parent is refused,
naming `--published-parent` or `--full-history`.

Every store push path (`stack submit`, stack publication, `ship` on the git
and gt lanes, and `ccx vcs push`) refuses before pushing a branch with ccx's
adoption mark, even if an interrupted adoption left it without a frozen
Graphite record. The refusal directs you to publish from the source
checkout that owns the branch. Retry the adopting `stack new` to repair an
interrupted adoption.

When the source republishes an adopted parent, the next
`stack new ... --published-parent` from the source refreshes the store's
branch, adoption mark, frozen record, and receipt to the new published
head. Existing children keep their recorded fork, so their next rebase or
submit replays only their own commits onto that head; the parent itself is
never pushed.

### Deepen to reach the parent's published base

If the published base lies beyond the store's history, creation refuses.
Retry with `--deepen` to permit bounded deepening:

```sh
ccx vcs stack new agent-child --thin --parent parent-work --published-parent \
  --deepen --max-depth 4096
```

`--max-depth N` caps deepening and defaults to 4096. ccx fetches trunk with
`git fetch --deepen=<k>` in doubling steps, then verifies the base. It never
uses `--depth` to deepen, because that could hide history. Success prints
`deepened <store> by <n> commits to reach <base>` before the lane's `cut`
segment. A refused deepen can leave the store deeper; history only grows.

## Check what ccx refuses and why

| Refusal | Action |
| --- | --- |
| `--depth` or `--deepen` without a thin lane cut from a full checkout | Pass them with `--thin` from the full checkout, or drop them |
| Explicit `--depth` with an existing store | Omit `--depth`; it applies only when creating the store |
| `--full-history` inside the thin store | Run it from the full source checkout |
| `--include` outside the store without `--thin` or `--sparse` | Select `--thin` or `--sparse`, or run from inside the store |
| Invalid `CCX_STACK_NEW` value | Set `thin` or `full`, or unset it |
| The store's own same-named parent differs from the source parent | Creation refuses before changes and names both commits; cut from the store's checkout of that branch, or use `--full-history` |
| A source-only parent lacks a verified publication onto trunk | Use `--published-parent` after publishing onto trunk, or `--full-history` from the full checkout |
| A store push targets a branch with ccx's adoption mark, even without a frozen Graphite record | Publish from the source checkout that owns it; retry the adopting `stack new` to repair an interrupted adoption |
| The published base is outside the store's history | Retry with `--deepen` and an explicit `--max-depth` cap if needed |

In a shallow repository, `ccx vcs stack rebase`, `submit`, `restack`, and
`ship` refuse before anything moves when the shallow boundary cuts a
branch's history. The refusal names the branch and the explicit command
`git -C <store> fetch --deepen=<commits> origin <trunk>`, where `<store>` is
the store's main checkout even when the refusal comes from a lane. Run that
command to grow the available history before retrying. These operations
never deepen an existing store implicitly. Object and history probes report
Git failures as errors instead of treating them as missing commits.

## Remove a lane

Leave the lane and run removal from the source checkout or another checkout
in the store:

```sh
ccx vcs worktree rm agent-work
```

`ccx vcs worktree rm --path <absolute-path>` also works. ccx removes the lane
through the store's own worktree registry, using the same macOS cleanup
daemon flow as other lanes. Logical removal means the original path is gone
and Git no longer registers it; physical deletion continues in the queue.
Use `--wait` to wait for physical deletion. The protections and queue
commands are covered in [Worktree removal and cleanup](worktree-cleanup.md).

ccx never deletes the store. Unpushed lane branches remain there after their
working copies are removed.

## Register the store with Orca

From a store lane, resolve the store path and register it once:

```sh
store_git_dir=$(git rev-parse --path-format=absolute --git-common-dir)
store=${store_git_dir%/.git}
orca repo add --path "$store"
```

Orca then lists store lanes as worktrees of that repository. ccx does not
call Orca.

## Account for the limits

- cc-notes keeps its records (`refs/cc-notes/*` objects and caches) in each
  checkout's Git common directory. The thin store is an independent clone
  that fetches trunk only, so source-local notes are absent and lane notes
  stay in the store. ccx does not copy or sync notes; the source checkout's
  notes are untouched. Keep thin mode opt-in per agent; do not set it globally.
- ccx downloads a remote branch head someone else based below the shallow
  boundary before refusing for incomplete ancestry.
- Reflog fork-point evidence in the store is thinner than in a long-lived
  full repository.
- Run `ccx vcs stack list` from a store lane; the source checkout does not
  list store lanes.
- History operations take time proportional to the history actually present.
- Concurrent operations use Git's own locks on the store. There is no
  ccx-wide lock.

The store's only fetch refspec is trunk. Plain `git push` therefore does not
update `refs/remotes/origin/<lane>`. After each push it makes from the store,
ccx records that ref with Git's `update by push` reflog entry, so leases and
ship's push checks work. A default `git fetch` never tries to fetch unpublished
or deleted lane branches.
