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

`ccx vcs stack restack` selects the repository's backend: stack replay for
Graphite, branch replay for plain Git, and fetch plus rebase for jj.
`ccx vcs stack submit` restacks and submits the whole stack.

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

## Check publication before retrying

Separate stacks can run concurrently. The existing run records select the run
for continue and abort; cleanup does not add a shared stack lock or a fetch
retry loop. Saved source heads, replay output pins, publication receipts,
atomic publication, and remote leases still protect the rewrite.

A lease or publication refusal requires inspecting the reported state before
retrying. A cleanup receipt describes workspace removal; it does not prove
that a stack was pushed or submitted. The per-branch publication report remains
the record of those outcomes.
