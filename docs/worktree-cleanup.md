# Worktree removal and cleanup

On macOS, `ccx vcs worktree rm` returns once a per-user daemon has proven the
removal safe and locked the worktree's Git registration for its job. The tree
stays at its path until the daemon's durable queue deletes it there, then the
registration; nothing is moved. `--wait` waits for both, and a caller that
reuses the path needs it. Linux keeps synchronous removal.

## Removal commands

Run removal from another checkout of the same repository. Choose either a
worktree name or an absolute path.

| Command | Result |
| --- | --- |
| `ccx vcs worktree rm <name>` | Remove the named worktree |
| `ccx vcs worktree rm --path <absolute-path>` | Remove the registered worktree at that path |
| `ccx vcs worktree rm <name> --dry-run` | Preview removal without changing the tree or queue |
| `ccx vcs worktree rm --path <absolute-path> --dry-run` | Preview removal by path |
| `ccx vcs worktree rm <name> --wait` | Wait for physical deletion before returning |

`--wait` also works with `--path`. On macOS, removal reports a cleanup job ID
for later inspection.

A lane name is never cut twice. `ccx vcs worktree add`, `ccx vcs stack new`,
and the stack's conflict workspaces record every path they cut, and removal
records every path it takes. The record covers the path and its lane name,
`<repository pool>/<name>`, under every worktree root. A later cut of a recorded
name refuses and suggests the next free suffix, such as `feat-2`, whether the
old tree's deletion is still pending or long done. Conflict workspaces take
that suffix on their own. The record lives under
`~/.daemonkit/a/ccx-cleanup/names`, one file per key, so the check is one read
per key.

Removal refuses the main checkout, locked trees, active sessions, nested
worktrees, and unsupported submodule layouts. Dirty work requires `--force`.
That flag permits discarding uncommitted files only; it never overrides the
other protections or stops a process holding the tree.

Recovery refs preserve committed heads before deletion. They do not preserve
dirty files discarded with `--force`.

## Orphaned worktrees

A linked worktree is orphaned when its `.git` file names an admin dir that no
longer exists, for example, after its repository was re-cloned. Git no longer
registers it, so neither `git worktree remove` nor `git worktree repair` can
reach it. `ccx vcs worktree rm --path <absolute-path>` deletes an orphan in
place. It runs from any directory, since no repository registers the tree.

On macOS, the cleanup daemon deletes it. The command returns once the daemon
has journaled the orphan as a job and claimed it, and prints
`removed <name> · orphaned checkout · <path> · deletion queued <job-id>`.
`--wait` returns once the tree is gone. The daemon pins the tree and its
parent directory by inode and refuses the job if either changes. It also
refuses one that a process holds, or that its quarantine check cannot clear.
On Linux, the command deletes the tree itself and prints
`removed <name> · orphaned checkout · deleted <n> entries in place · recorded in <file>`.

Removal refuses an orphan whose resolved path is not exactly
`<pool>/<repo>/<name>` in one of the worktree pools: `~/.claude/worktrees` or
the `ccx.worktreeRoot` pool. It also refuses
one whose `.git` is not a regular file with a missing `gitdir:` target, and
one that a live process works in, holds a file open in, or was started on.
Linux has no such process check, so there `--force` is required.

Git cannot tell whether an orphan held uncommitted or unpushed work. Before
deleting anything, removal appends a JSON line with the orphan's path, the
admin dir its `.git` named, and the time to `worktree-rm/orphans.jsonl` under
ccx's cache directory. Deletion runs through held directory descriptors and
stops if the path comes to name a different tree. `--dry-run` runs the path,
`.git`, and live-process checks, prints `would remove`, and deletes nothing.
On macOS, `--force` waives none of these checks.

## Queue commands

The macOS daemon owns the deletion queue. It does not run stacks or change
their publication rules. The public command group is `ccx vcs cleanup`.

| Command | Result |
| --- | --- |
| `ccx vcs cleanup status [job-id] --json` | Report the queue, or one job, with progress and any blockage |
| `ccx vcs cleanup wait <job-id>` | Wait for one job to finish; report a blockage if it cannot proceed |
| `ccx vcs cleanup pause` | Stop physical deletion, retire inactive queued linked-worktree registrations in place, and queue new `worktree rm` removals as pending |
| `ccx vcs cleanup resume` | Resume the queue; refused while a maintenance hold stands |
| `ccx vcs cleanup hold acquire --id <id> --reason <text>` | Block worktree-registration retirement and deletion under a hold that `resume` cannot lift |
| `ccx vcs cleanup hold release --id <id>` | Lift the hold `<id>` names, leaving any ordinary pause in place |
| `ccx vcs cleanup hold status [--json]` | Report the hold, if any, and the ordinary pause apart from it |
| `ccx vcs cleanup retry <job-id>` | Retry a blocked job from its recorded phase after its cause is addressed |
| `ccx vcs cleanup cancel <job-id>` | Keep a queued, waiting, or pending worktree: restore a retired registration in place and drop the job |
| `ccx vcs cleanup watchers --json` | Inspect watcher roots and consumers without retiring them |

`pause` stops physical deletion while keeping queued jobs at
their recorded phases and retaining their directories and content in place.
During an ordinary pause, the daemon still checks inactivity and unregisters
inactive queued linked worktrees in place; live activity or unknown ownership
blocks logical retirement. While paused, the daemon accepts Expected-bound
deferred workspace releases and rejects `cleanup adopt` on legacy trees, naming
`ccx vcs cleanup resume`. A maintenance hold (`hold acquire`) blocks
worktree-registration retirement and deletion,
while `worktree rm` still journals `pending` queue intents and deferred
releases and `cleanup adopt` are refused, until `hold release` is called with
the matching `--id`, which leaves any ordinary pause intact. Both pause and hold
survive daemon restarts. Acquire a maintenance hold before running
registration-changing maintenance on an affected worktree entry (`repair`,
`move`, `lock`, `unlock`, `prune`, or `remove`) so it does not overlap logical
retirement; maintenance on unrelated entries and adding a new unique worktree
are unaffected.

A process scan cannot see an agent between its commands, so a paused queue
also waits until a registration's `HEAD` and `HEAD` reflog have rested for an
hour before retiring it. `cleanup status` shows the job's `retires after` time.
A lane that commits meanwhile changes `HEAD`, which blocks the job before
retirement and keeps the registration.

A `worktree rm` during a pause runs the initial main-working-copy, trunk, lock,
submodule, nesting, and dirty-tree refusals, then records a `pending`
job without moving or deleting the directory; inactivity is checked later
before logical retirement in place. `cleanup status` lists the job as `pending`,
and the receipt reads `queue paused until ccx vcs cleanup resume`.
`worktree rm --wait` and `cleanup wait` fail at once while the queue is paused,
naming `ccx vcs cleanup resume`, instead of waiting for a resume nobody may
run. On `resume`, the daemon repeats full preflight, identity, and activity
checks in queue order before it claims each registration and deletes the tree,
blocking any job that fails checks or whose tree changed.

A stack rebase, `stack continue`, or `stack abort` that meets a paused queue
still finishes and records its conflict workspace as a `pending` retained
directory: while paused, the daemon may retire the workspace's registration in
place once inactive, and `resume` finishes the deletion.

The daemon logs every `pause` and `resume` to `daemon.log` with the
requesting process ID, its ccx version, and its parent's process ID and
command line.

`ccx vcs cleanup status` and `ccx vcs cleanup wait` are read-only: they query
only the running daemon with one 5s hello and bounded requests, including
older versions that speak the same protocol. They never install a program
copy, stop or replace the daemon, apply the `LaunchAgent`, or take the start
lock, and both fail if no daemon is running.

`worktree rm --wait` uses the same read-only wait after removal, with a 5s
hello. Both wait commands poll status with a 10s limit per request and back
off up to 1s between polls. A poll that finds the socket missing or refusing
polls again for up to a minute before it fails; `worktree rm` following a blocked job's automatic
retry does the same. They succeed only when a poll shows the job
`done`, after its tree and registration are gone; `worktree rm --wait` then reports
`deleted`. A blockage fails the wait with its reason.

Each wait poll marks its job as awaited for the next 10s. The daemon deletes
awaited jobs, in queue order, ahead of every job nobody awaits. An awaited job
keeps the same rate cap and throttle as any other job, so a wait lasts as long
as its own tree's paced deletion, plus any awaited trees queued ahead of it.
Logical removals still go first, and a paused queue or a blocked job still
holds.

The job keeps the requesting command as its requester, so the daemon's later
retries do not count that command's own arguments naming the tree as activity
while that exact process still runs.

If a job disappears after either command has seen it, the wait exits 1. If
the daemon lists the record as damaged, the error names the job, its last
seen phase, and the damage.
Otherwise, the error names the job and its last seen phase and says its
completion cannot be proven: it may have finished and been pruned, or its
record was lost. `worktree rm --wait` counts its removal receipt as seeing
the job. Only a job `cleanup wait` has never seen is "not found" (exit 3).
`cleanup status`, `cleanup wait`, and `cleanup retry` reject an empty job ID.

`cleanup cancel` undoes a removal that has not begun to claim its registration. It checks
that the tree, its registration, its `.git` link, and the job's own lock are
the ones the job captured; `HEAD` may have moved. It then writes the captured
`gitdir` backlink back, removes the lock, and drops the job, so git lists the
worktree again. The job's recovery ref stays. A job past `pending`, an adopted
tree, a foreign lock, or a changed link is refused with nothing changed, and a
maintenance hold refuses it too.

Ordinary `ccx` cleanup queue-changing commands, including `worktree rm`, `cleanup pause`, `resume`, `retry`, `adopt`, `hold`, and deferred stack workspace removals, send one bounded hello to the already-running cleanup daemon. They accept any daemon with the same protocol version and never install, start, stop, or replace the daemon or apply a `LaunchAgent`. A missing daemon or an incompatible protocol version produces a visible failure before the cleanup request is sent.

An already-installed `LaunchAgent` can start the daemon at sign-in, and manual startup uses `ccx vcs cleanup serve` as an explicitly authorized service action. A software upgrade alone does not authorize restarting the daemon, and existing tasks remain with their running daemon.

Shutdown cancels the worker's current step. Git reads stop; an `update-ref`
already running finishes or reaches its budget before the step yields. One that
reaches its budget is recorded as a timeout, and the restart retries it with the
usual backoff. The job stops at its last durably saved phase, and a restart
resumes from that phase. Shutdown waits for at most one active Git command's budget, plus up to
five seconds if a descendant keeps its output pipes open.

During the v0.67.3 upgrade incident, the draining daemon accepted each new
connection and closed it at once without a reply, so status saw an empty
reply and the shutdown wait never saw a refusal.

Inspect queue progress as JSON:

```sh
ccx vcs cleanup status --json
```

Status bounds the number of jobs it returns and reports omissions. Its second
line counts every job in the queue by state, a blocked job as
`blocked <reason>`, so jobs past the limit stay visible. It does not scan the
remaining files to invent a total or an estimated completion time. Query the
job ID from a removal report to inspect a job omitted from the queue view.

### Maintenance hold

An operator who needs the queue stopped through maintenance takes a hold:
`ccx vcs cleanup hold acquire --id <id> --reason <text>`. The hold stops the
queue at the same points a pause does, and `acquire` returns once the deletion
in flight has stopped and its progress is journaled. It differs from a pause
in what lifts it:

- `resume` fails while the hold stands and changes nothing. The error names
  the holder, when it took the hold, and the reason. The daemon itself refuses,
  so a `resume` from an older ccx on the same protocol fails the same way.
- Only `hold release --id <id>` with the holder's `--id` lifts the hold. Any
  other `--id`, or a release with no hold standing, fails and leaves the queue
  as it was.
- `release` lifts the hold and nothing else. A queue paused before or during
  the hold stays paused until `resume`; one that was not paused runs again.
- `acquire` again with the same `--id` changes nothing and reports the hold
  that stands. Another `--id` fails, naming the holder.

The hold is journaled with the pause switch, so it survives a daemon restart
and an upgrade, and it never expires. `cleanup status` reports a held queue
exactly as a paused one, for every ccx version: the header reads
`deletion paused`, jobs waiting on deletion read `paused`, and `cleanup wait`
and `worktree rm --wait` fail at once as they do under a pause. Only
`hold status` names the holder. It reads `held by <id> since <time>: <reason>`
and whether deletion is paused, held, or running, where `paused` is the
ordinary pause alone; `--json` prints `{"paused": <bool>, "hold": <hold or
null>}`. It is read-only like `cleanup status`, and a daemon older than the
hold refuses it. The daemon logs every acquire, repeat, release, and refusal
with the same caller fields as `pause` and `resume`.

A hold prevents an accidental `resume`. It is not a security boundary: any
process running as the same user can release a hold whose ID it knows.

## Completion and recovery

| Reported outcome | Meaning |
| --- | --- |
| Claimed | The removal is proven and the job's lock holds the Git registration; the tree stays at its path until its deletion runs |
| Waiting for inactivity | A completed internal workspace remains at its original path while a session holds it |
| Physical deletion in progress | The daemon is deleting the tree where it sits, then its registration, in bounded slices |
| Paused | Preparation and deletion are held at the recorded phase |
| Pending | A `worktree rm` asked for during a pause; nothing has moved, and `resume` runs its preflight |
| Blocked | The job needs attention; its error and recorded phase explain where it stopped |
| Done | Physical deletion finished |

A job receipt proves acceptance, not completion. Normal removal succeeds only
after checking the original path and Git registration. `--wait` or
`cleanup wait <job-id>` confirms physical deletion; a queued or blocked receipt
does not substitute for that result.

The queue survives daemon restarts. It records each phase and checks the
captured worktree identity before continuing. Thirty seconds after it loads the
journal, and every six hours after that, the daemon retries every job blocked
for an operator, so a daemon restarted onto a fix moves the jobs an older one
gave up on. A job whose cause still holds blocks again with the same reason.

A path reused by another actor is not permission to delete the replacement. Identity mismatches and other
errors block the job with diagnostics. A legacy payload whose repository was
deleted has no registry left to claim it, so its deletion proceeds; a tree
still at its path needs its repository for the proof, so it blocks.

The daemon deletes every tree where it sits. Before it claims one, it checks
the captured inode, the registration and links, and the pinned `HEAD`. The
tree must also be clean unless `--force`, with no checked-out submodule, nested
registration, watcher above or inside it, or live process. It then writes `ccx cleanup <job-id>` to the registration's `locked`
file, which keeps `git worktree remove`, `prune`, and `move` off it, and
proves all of it again before the deletion starts. A deletion that resumes
after a pause, a crash, or a block is proven again too. That proof counts
files added or edited since the deletion began, and skips the ones it already
removed.

The daemon deletes the tree, then the admin directory under
`<common>/worktrees/` that its lock holds, and the job is done once both are
gone. The lock is checked once the tree is gone, before the admin directory's
deletion begins. A resumed admin deletion needs only the captured inode,
since the deletion itself may already have removed `locked`. A tree whose
`.git` and admin directory were
lost is proven against the job's saved `HEAD` through a throwaway index
instead; a file the tree no longer holds is one that commit restores, so it is
not dirt.

A directory counts as the job's when it has the captured inode on the device
its parent sits on at that moment, so a volume renumbered since capture still
matches and a mount root never does. A tree gone from every path the job names
finishes the job only once its registration is gone too or still carries the
job's lock. A registration Git keeps under no lock of the job's holds the job
on a retryable blockage, since the tree may only have moved away. Jobs an
older daemon relocated into its job folder drain from where they sit, with no
further moves.

Every Git command the daemon runs has a finite budget, one minute by
default. An expired budget blocks the job with the transient `timeout`
reason at its recorded phase; it retries with the existing backoff. A watcher or
quarantine probe that cannot answer blocks the same way. Captured
Git error output has a byte limit. Cancellation signals only the direct
child, never a process group.

The daemon deletes in bounded slices and yields to new logical removals. While
the governor is clear, 16 workers remove entries in parallel at up to 4,000
entries a second, and a slice ends after 1,600 entries or 800ms. The workers
remove entries through directory descriptors the deletion holds open. They
never follow a symlink or cross onto another volume, they restore write access
to a read-only directory before emptying it, and they remove a directory only
after a pass over it lists nothing. Each slice first proves that every held
directory still sits where the deletion found it, and lets go of one that
moved.

Every 10s of a running deletion, the daemon checks the job again without Git:
the captured inode is still where it sits, the registration still carries the
job's lock and no other claim, and no watcher or live process is in the tree.
When one fails, the deletion stops and the job blocks, with everything removed
so far kept. The full proof, Git included, runs again the next time the
deletion opens.

The daemon rests after each slice for the larger share it used of either
bound. A slice that spends its time on directories and removes little rests as
long as a full one. The `cleanup status` header names the worker count and the
entries deleted per second over the last sample, and the job being deleted
carries the same rate. The daemon samples `fseventsd` every 5s and learns its ambient
load from samples taken while deletion ran no faster than its floor. It does
not run idle repository or process scans.

The daemon drops to one worker and at most 100 entries a second, in slices of
100 entries or 50ms, in three cases:

| `cleanup status` governor | Cause | Lifts |
| --- | --- | --- |
| `throttled at N% cpu` | Deletion pushed `fseventsd` more than half a core over its ambient load | After three samples within a quarter core of that load |
| `throttled at N% cpu: slices stalled past twice their time budget` | A slice took twice its time budget or more, 1.6s with 16 workers and 100ms with one. The floor starts with that slice's own rest, not at the next sample | After three samples that each ran a slice, none of them stalled |
| `unavailable: <error>` | `fseventsd` could not be sampled | One sample after `fseventsd` reads again; deletion waits out that 5s interval |

A slice that ends at its time budget is the budget working, and does not throttle.
Both throttles can hold at once. The status then names the stalled slices, and
the full rate returns once each has lifted. At the floor, a slice that takes
its whole 50ms rests a full second whatever it removed, so deletion can run
below 100 entries a second.

The floor keeps the queue draining on a machine whose `fseventsd` never goes
quiet. A job a wait command is polling skips the queue only. It deletes at the
rate the governor allows.

Work on the daemon's own volume also waits for the Claude and Codex sessions
that share it. Any parallel deletion there slows every file create on that
volume roughly tenfold, background priority included. Every 15s while such
work is pending, the daemon counts the processes of yours whose `argv[0]` is
`claude` or `codex` and that used over a tenth of a core since the last
sample. It also times eight file creates in its `probe` directory after
deletion has rested for a second.

When more than three sessions are busy, or
the median create takes over 3ms, the header reads `governor held: <counts>`.
In that state no job on that volume deletes or advances, however long it
waits. Work resumes at full speed after two quiet samples in a row, and the
first sample after a start only sets the CPU baseline. A job whose tree sits
on another volume does not wait.

A completed internal conflict workspace can wait for inactivity while other
cleanup jobs proceed and stack completion succeeds. Interactive Claude,
Codex, Orca, editor, and terminal sessions remain alive. Leave the workspace
when finished so the daemon can remove it.

## Watcher inspection and retirement

`ccx vcs cleanup watchers --json` reports watcher roots, subscriptions,
queries, triggers, configuration origins, recrawls, and Git fsmonitor ownership.
It is an inspection command. Removing an existing watcher root requires
authorization even when a snapshot shows zero consumers.

Watcher retirement belongs to removal of an authorized unused tree. It checks
current consumers, processes, and terminal activity, removes only roots inside
that tree, and verifies removal. Git fsmonitor ownership comes from its socket
and metadata; a process working directory alone is insufficient evidence.
The running Watchman server's descriptors are never activity. It can keep a
directory open after its root is deleted, and that descriptor follows the tree
when it is moved, so every activity check discounts the server's descriptors,
before and after retirement and at every location the tree passes through.
When a process or Watchman probe times out, the failure is a warning: the tree
is removed only if it has no uncommitted changes and a remote-tracking ref holds
its head, and the activity guard still refuses any holder it finds. A process
whose arguments the kernel cannot produce is judged by its working directories
and open files alone.

Scope watcher configuration changes to the authorized unused trees too.
Applying settings to an existing root requires checking that it is idle before
recreating that exact watch. Keep active source, generated-input, and build-tool
watches intact. Do not disable FSEvents or watchers globally to reduce churn.
