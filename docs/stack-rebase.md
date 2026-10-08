# Rebase a stack and resolve conflicts

Stack replay requires Git 2.56 or newer. `ccx vcs stack rebase` replays each
branch from its saved old base and head onto its new base, then pushes the
stack. Clean replay writes Git objects without checking out files, changing an
index, or running hooks. Merge commits are linearized, as with
`git rebase --no-rebase-merges`.

From a checkout of the stack, preview the branches and parents:

```sh
ccx vcs stack rebase --dry-run
```

Run the rebase after reviewing the plan:

```sh
ccx vcs stack rebase
```

Use `--no-push` to keep the rewrite local. `--parent branch=parent` changes a
branch's parent; `--linearize a,b,c` chains named branches in that order. The
branch-order flag is separate from replay's handling of merge commits.

Either flag moves only the branches it names a parent for and the branches
stacked above them. The new parent and every branch below it stay where they
are: at their published heads when the run pushes, at their local heads when
it does not. To put your branch on another lane's pushed branch without
rebasing that lane, run this from your branch's worktree:

```sh
ccx vcs stack rebase --parent mine=theirs
```

gt does not need to track the other lane's branch already. The run tracks it
on its nearest tracked ancestor, or on trunk, the way `ship` adopts a branch.
A parent that exists only on the remote is refused with the fetch that brings
it local:

```sh
git fetch origin refs/heads/theirs:refs/heads/theirs
```

A reorder that puts a branch below the base its pull request targets is
refused before anything is pushed. The push leaves that base holding the
pull request's head, which GitHub reads as merged. GitHub closes the pull request
and deletes its branch, and no push order avoids that. The refusal names each
`#<n> (<branch>) into <base>`. Retarget those pull requests onto trunk with
`gh pr edit <n> --base <trunk>`, then run `ccx vcs stack continue`; the submit
sets every real base afterward.

A submit also refuses while the Graphite merge queue is restacking a pull
request. When a parent lands, the queue parks each child on
`graphite-base/<n>` and replays `graphite-base/<n>..head` onto trunk. A
submit that moves the child onto a new parent during that window moves
`graphite-base/<n>` too, and the replay then drops the new parent's commits.
A submit that moves the child onto trunk races the queue's own force-push,
which can land a replay of the landed parent over it. The refusal names the
pull request and the landed parent. Wait until the pull
request's base leaves `graphite-base/<n>`, then run the same command again.

`ccx vcs stack restack` selects the repository's backend: stack replay for
Graphite, branch replay for plain Git, and fetch plus rebase for jj.
`ccx vcs stack submit` restacks and submits the whole stack, except that a
published branch on trunk that still merges cleanly keeps its old base. The
plan names such branches on an `old base kept` line; pass `--restack` to
replay them onto the fetched trunk, such as when the stack needs a fix that
landed there.

## Resolve a conflict in its workspace

A conflict stops publication and opens the reported `conflict-<branch>`
workspace with a real rebase in progress and rerere disabled. Enter that
workspace before editing. The source checkout keeps its files, index, and
sparse settings.

Internal conflict and rehearsal workspaces start sparse. They contain root
files and expand to include each conflicted file's directory, including paths
from renames, deletions, and later conflicts. Expansion keeps directories
already included. Ordinary user worktrees stay full unless sparse creation
was requested explicitly.

Inspect the conflict and current sparse selection:

```sh
git status
git sparse-checkout list
```

If resolving the conflict requires more files, add their directories. For
example, a repository with `src` and `tools` directories can materialize both:

```sh
git sparse-checkout add src tools
```

Resolve and stage the conflicted files, then resume the stack:

```sh
ccx vcs stack continue
```

Continue opens a pull request only when the command that started the run gave
the branch a `--pr-title` and a `--pr-body-file`. Any other branch with no pull
request is pushed but not submitted. Open it from its checkout:

```sh
ccx vcs ship --no-commit --tip-only --pr-title "<title>" --pr-body-file <body.md>
```

To abandon the run, use `ccx vcs stack abort`. Run these commands from the
branch's checkout or its conflict workspace; use `--stack` with the bottom
branch's name when selecting a run explicitly. Native `git replay` has no
continue or abort operation.

On macOS, a completed conflict workspace still occupied by a shell, editor,
or agent stays in place until inactive. Stack completion succeeds while its
[cleanup job](worktree-cleanup.md) waits. Leave the workspace when finished;
cleanup does not stop the session holding it.

## Regenerate conflicted output explicitly

A conflict in a path declared under `[[generated]]` in `.ccx.toml` stops for
manual action. Rebase and continue do not run generators automatically.

1. Resolve and stage every conflict outside the declared generated paths.
   If `.ccx.toml` conflicts, resolve and stage it too; regeneration reads the
   declarations from the workspace's index.
2. Materialize the directories needed for dependency setup with
   `git sparse-checkout add`, or use `git sparse-checkout disable` for the full
   tree. Run the project's dependency setup inside this workspace.
3. Run `ccx vcs stack regenerate` with repeatable `--include` flags for any
   additional directories the generators read, or `--full` for the whole tree.
   These flags materialize files and run the generators in the same command.

For generators that need `src` and `tools`, run:

```sh
ccx vcs stack regenerate --include src --include tools
```

For generators that need the whole tree, use this instead:

```sh
ccx vcs stack regenerate --full
```

`--include` and `--full` are mutually exclusive. Regeneration runs the declared
generators for conflicted generated files and generated files touched by the
stopped commit, then stages their output. It does not install dependencies.
Keep dependency installation out of generator commands in `.ccx.toml`.

Shared download and compiler caches can remain shared. Keep mutable
`node_modules`, package-manager install state, and build outputs private to
each workspace. A shared cache does not provide an installed dependency tree.

Review the generated changes, then resume:

```sh
git diff --cached
ccx vcs stack continue
```

## Repair a pull request Graphite does not track

After a push, the verdict line for each open pull request ends with its
GitHub mergeability. It reads `untracked by graphite` instead when Graphite's
`mergeability-status` holds no row for the pull request after 20 seconds, the
state `stack-enqueue` reads as `UNTRACKED` however green GitHub shows it. A
parent that landed under the pull request leaves it there. Graphite's
server-side stack graph keeps the old parent, and a resubmit of an unchanged
head does not rewrite it.

`ccx vcs stack submit` repairs it. It republishes each untracked branch with a
new head, the same tree, parents, author, and message under a later committer
date, restacks the branches above it, and checks Graphite again:

```text
repairing · Graphite holds no mergeability record for:
#29427 feature/api · parent dev · Graphite tracks no stack for it · its server-side parent is still feature/pools
repaired · #29427 republished with a fresh head and tracked by Graphite
```

When Graphite still holds no row, the command exits non-zero naming each pull
request and the parent Graphite last recorded for it.

A fresh head does not help when Graphite's stack record for the pull request
still holds pull requests that already landed or closed, which happens when
its old parent merged through the queue. Graphite's `pull-request-info` lists
them beside the pull request even after its newest version records the right
base. `stack submit` does not republish such a pull request, since each
republish only restarts CI, and exits non-zero naming the landed pull
requests:

```text
#32336 yasyf/api-sandsql-team-storage · parent dev · Graphite tracks no stack for it · its server-side stack still holds #32379, #32405, which already landed or closed
```

To clear the record, close and reopen each pull request on GitHub so
Graphite rebuilds it, then rerun `ccx vcs stack submit`. `stack rebase` and
`stack continue` exit non-zero for an untracked pull request without repairing
it; run `ccx vcs stack submit` to repair.

## Check publication before retrying

A run locks the branches it writes, not the whole stack, so two lanes of one
stack can each ship their own branch at the same time. A run refuses only when
another run writes one of its branches. The refusal names that run's process,
the branches it writes, and the `ccx vcs stack continue --stack <branch>` and
`ccx vcs stack abort --stack <branch>` commands that finish or drop it. When a
run's process has exited and every branch it writes has landed or been
deleted, the next stack write discards it and prints a
`discarded the stack rebase of ...` line. Saved source heads, replay output
pins, publication receipts, atomic publication, and remote leases still
protect the rewrite.

A lease or publication refusal requires inspecting the reported state before
retrying. A cleanup receipt describes workspace removal; it does not prove
that a stack was pushed or submitted. The per-branch publication report remains
the record of those outcomes.
