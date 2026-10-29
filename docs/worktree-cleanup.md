# Worktree removal and cleanup

On macOS, `ccx vcs worktree rm` returns after logical removal: the original
tree is gone and Git no longer registers that worktree. A durable per-user
queue handles physical deletion afterward; `--wait` waits for that deletion
to finish. Linux keeps synchronous removal.

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
reach it. `ccx vcs worktree rm --path <absolute-path>` moves an orphan to
`~/.Trash/<name>-<timestamp>` and prints
`removed <name> · orphaned checkout · moved to Trash <path>`. It runs from any
directory, since no repository registers the tree.

Removal refuses an orphan whose resolved path is not exactly
`~/.claude/worktrees/<repo>/<name>`, or whose `.git` is not a regular file
with a missing `gitdir:` target. It also refuses one that a live process works
in, holds a file open in, or was started on. Linux has no such process check,
so there `--force` is required.

Git cannot tell whether an orphan held uncommitted or unpushed work, so
removal moves it to the Trash instead of deleting it. A tree on another
volume than the Trash is refused. `--dry-run` runs the path, `.git`, and
live-process checks and prints `would remove`. It does not attempt the move,
so it cannot report a Trash on another volume. On macOS, `--force` waives none
of these checks.

## Queue commands

The macOS daemon owns the deletion queue. It does not run stacks or change
their publication rules. The public command group is `ccx vcs cleanup`.

| Command | Result |
| --- | --- |
| `ccx vcs cleanup status [job-id] --json` | Report the queue, or one job, with progress and any blockage |
| `ccx vcs cleanup wait <job-id>` | Wait for one job to finish; report a blockage if it cannot proceed |
| `ccx vcs cleanup pause` | Stop relocation and deletion; unregister unmoved removals in place and queue new `worktree rm` removals as pending |
| `ccx vcs cleanup resume` | Resume the queue; refused while a maintenance hold stands |
| `ccx vcs cleanup hold acquire --id <id> --reason <text>` | Stop the queue as `pause` does, under a hold that `resume` cannot lift |
| `ccx vcs cleanup hold release --id <id>` | Lift the hold `<id>` names, leaving any ordinary pause in place |
| `ccx vcs cleanup hold status [--json]` | Report the hold, if any, and the ordinary pause apart from it |
| `ccx vcs cleanup retry <job-id>` | Retry a blocked job from its recorded phase after its cause is addressed |
| `ccx vcs cleanup watchers --json` | Inspect watcher roots and consumers without retiring them |

`pause` (`ccx vcs cleanup pause`) and holds (`ccx vcs cleanup hold acquire`)
stop tree relocation and physical deletion until `resume` runs and the hold is
released. Jobs already mid-move (`prepared`, `moved`, `detached`) and every
physical deletion rest at their phases, and pause and hold state survive daemon
restarts. While paused, the daemon rejects `cleanup adopt` and deferred
workspace releases before any preparation or Git work, naming
`ccx vcs cleanup resume`. A paused `worktree rm` runs the main-working-copy,
trunk, lock, submodule, nesting, volume, and dirty-tree refusals, skips the
live-process and watcher checks, and records a `pending` job whose receipt
reads
`queue paused: git unregisters it in place, and the tree stays until ccx vcs cleanup resume`.
`worktree rm --wait` and `cleanup wait` fail at once while paused, naming
`ccx vcs cleanup resume`.

While paused or held, the daemon unregisters in place every `queued` or
`waiting` workspace deferral and `pending` `worktree rm` removal. It first
proves that the tree's identity, registration, `.git` and admin links, `HEAD`,
and branch match what the job captured, and applies the lock, submodule,
nesting, and volume refusals. It then checks that no live process holds the
tree or its admin directory, discounting watchers without stopping them. A held
deferral returns to `waiting` and is rechecked, a held removal blocks on
`activity` for backoff retry, and a changed tree blocks on `identity`.

To unregister a checkout, the daemon journals the job as `retiring`, rewrites
the checkout's `.git` file to point at `<common dir>/retired-worktrees/<id>`,
and renames the admin directory `<common dir>/worktrees/<name>` to that path.
The new path sits at the same depth below `<common dir>`, so a relative
`commondir` entry still resolves. Only metadata moves; nothing is copied, no
ref is written, and the index, `HEAD`, reflogs, and any merge or rebase state
in the admin directory move with it unchanged. The checkout keeps working at
its path and `git worktree list` no longer shows it. Because `git gc`,
`git worktree prune`, and git's checked-out-branch check read only
`<common dir>/worktrees`, nothing protects objects reachable only from that
checkout's `HEAD`, index, or reflogs, and another worktree may check out its
branch. Branch refs still keep the commits they name.

Right before rewriting `.git`, the daemon re-checks the tree's identity, the
`.git` regular file and its captured contents, the admin directory's identity
and `gitdir` back-link, its `commondir` resolving to the repository, and the
absent retired path. Right before the rename, it re-checks the tree, the new
pointer in `.git`, the admin directory, and the absent retired path. A failed
check before the rename restores the old `.git` contents, though only while the
captured admin directory remains at its path. Otherwise, `.git` keeps naming the
empty retired path, which binds no directory, and the job blocks on `reconcile`.
If the rename carried a different directory that had taken the admin directory's
place, the daemon moves it back to the admin path, or into `<common
dir>/retired-worktrees/.<id>.foreign-*` when that path is taken, and blocks on
`reconcile`. After the rename, git must resolve the checkout through the retired
path and the repository, `HEAD` must match, and no registration may reach the
tree before the job enters `retired`.

A daemon that stops mid-retirement resumes from what the file system shows.
With only `.git` rewritten, it restores the old contents under the same checks,
proves the tree unchanged and idle again, and starts over. With the admin
directory already moved, it verifies the result and finishes. Any other layout
blocks on `reconcile`. A halted or resumed queue never retries a `reconcile`
or `identity` block on its own; `ccx vcs cleanup retry` does. An `activity`
block backs off and retries as usual.

For compatibility, `cleanup status` reports a `retiring` or `retired` job as
`pending`, or `queued` for a deferral, including in its error history, so every
client of the daemon protocol keeps decoding the report. The protocol version
is unchanged.

When the queue resumes with no hold standing, the daemon proves each retired
tree afresh. It checks the tree's identity, the `.git` pointer to the retired
path, and the retired admin directory's identity, `gitdir` back-link, and
`commondir`. No registration may reach the path, `HEAD` and branch must match,
and the lock, submodule, nesting, and volume refusals apply again.

Once those hold, the daemon stops the tree's watchers, checks the job folder's
quarantine, live processes, and uncommitted changes, and records
`refs/ccx/cleanup/<id>` at `HEAD` as an ordinary removal does. It then moves the
tree into its job folder, removes the retired admin directory as `git worktree
remove` does on the ordinary path, and deletes the tree at the queue's pace. A
tree found already in its job folder finishes the same way. A tree gone from its
path whose retired admin directory remains blocks on `identity`, so whatever
moved the tree keeps its git dir. Any other change blocks the job.

A stack rebase, `stack continue`, or `stack abort` that meets a paused queue
still finishes and leaves its conflict workspace in place, naming the
`ccx vcs worktree rm` that queues its removal until resume.

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
off up to 1s between polls. A poll that finds the socket missing or refusing,
as it is while another ccx replaces an outdated daemon, polls again for up to
a minute before it fails; `worktree rm` following a blocked job's automatic
retry does the same. They succeed only when a poll shows the job
`done`, after its payload is gone; `worktree rm --wait` then reports
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

`launchd` starts the daemon at login; queue-changing commands that reach the
daemon (`ccx vcs worktree rm`, `ccx vcs cleanup pause`/`resume`/`retry`/`adopt`,
`ccx vcs cleanup hold acquire`/`release`,
and deferred removals) install and start or replace it. Under the start
lock, they read and keep the kernel peer process and user IDs and the
process's kernel unique ID on the hello connection, before sending hello.
On macOS, this is `p_uniqueid`, read through the existing `proc_pidinfo`
helper. The reply proves the saved identity belongs to the process that
answered.

They re-check the identity on the shutdown connection before sending the
stop. The kernel peer must match the saved process ID and the client's user
ID, with the same kernel unique ID.

The kernel never reuses that unique ID within a boot. Neither a reused process
ID nor a clock step, even one that repeats the old start time, can make a
different process match. No start time or clock is compared. They send
nothing on other connections and never retry the stop on a new one. This
works with older daemons without a wire protocol change.

After a verified stop, the socket wait also ends if a different process
answers; they still confirm the outdated daemon's exit before replacing
its program copy. They take the serve lock, which every daemon version
holds for its whole life, before installing the program or applying the
`LaunchAgent`, and hold it until apply returns. Apply has a 15s agent
timeout; a starting daemon waits up to 30s for the serve lock, so its wait
outlasts the apply that launches it.

If another daemon takes over the socket before the stop or takes the serve
lock first, the command installs and applies nothing and stops nothing.
It waits up to the agent timeout for that daemon to answer, uses it if it
is current, and otherwise reports an error saying it was left running. When
the unique ID or peer credentials cannot be read, including on hosts other
than macOS, or the hello's process ID differs from the kernel's, the command
reports that the daemon could not be verified. It leaves the daemon running
and stops, installs, and applies nothing. If a daemon accepts and closes
without replying, never sends its hello, or holds its serve lock behind a
refusing or missing socket, the command probes again under the start lock.
If it still does not answer, the command reports an error before any install
or apply.

Shutdown cancels the worker's current step. Git reads stop; an `update-ref`,
`worktree move`, or `worktree remove` already running finishes or reaches its
budget before the step yields. One that reaches its budget is recorded as a
timeout, and the restart retries it with the usual backoff. The relocation
ladder stops at the next durably saved phase, and a restart resumes from that
phase. Shutdown waits for at most one active Git command's budget, plus up to
five seconds if a descendant keeps its output pipes open.

During the v0.67.3 upgrade incident, the draining daemon accepted each new
connection and closed it at once without a reply, so status saw an empty
reply and the shutdown wait never saw a refusal.

Inspect queue progress as JSON:

```sh
ccx vcs cleanup status --json
```

Status bounds the number of jobs it returns and reports omissions. It does not
scan the remaining files to invent a total or an estimated completion time.
Query the job ID from a removal report to inspect a job omitted from the queue
view.

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
| Logical removal succeeded | The original tree is gone and its Git registration is removed; queued data can still occupy disk space |
| Waiting for inactivity | A completed internal workspace remains at its original path while a session holds it |
| Physical deletion in progress | The daemon is deleting the relocated data in bounded slices |
| Paused | Preparation, relocation, and deletion are held at the recorded phase |
| Pending | A `worktree rm` asked for during a pause; nothing has moved, and `resume` runs its preflight |
| Blocked | The job needs attention; its error and recorded phase explain where it stopped |
| Done | Physical deletion finished |

A job receipt proves acceptance, not completion. Normal removal succeeds only
after checking the original path and Git registration. `--wait` or
`cleanup wait <job-id>` confirms physical deletion; a queued or blocked receipt
does not substitute for that result.

The queue survives daemon restarts. It records each phase and checks the
captured worktree identity before continuing. A path reused by another actor
is not permission to delete the replacement. Identity mismatches and other
errors block the job with diagnostics.

Every Git command used for relocation has a finite budget, one minute by
default. An expired budget blocks the job with the transient `timeout`
reason at its recorded phase; it retries with the existing backoff. Captured
Git error output has a byte limit. Cancellation signals only the direct
child, never a process group.

The daemon deletes in bounded slices at up to 1,000 entries a second and
yields to new logical removals. A slice ends after 100 entries or 50ms, and
the daemon rests after each one for the larger share it used of either bound.
A slice that spends its 50ms on directories and removes little rests as long
as a full one. The daemon samples `fseventsd` every 5s and learns its ambient
load from samples taken while deletion ran no faster than its floor. It does
not run idle repository or process scans.

The daemon slows to at most 100 entries a second in three cases:

| `cleanup status` governor | Cause | Lifts |
| --- | --- | --- |
| `throttled at N% cpu` | Deletion pushed `fseventsd` more than half a core over its ambient load | After three samples within a quarter core of that load |
| `throttled at N% cpu: slices stalled past twice their time budget` | A slice took 100ms or more, which takes one operation on the volume stalling for a whole 50ms. The floor starts with that slice's own rest, not at the next sample | After three samples that each ran a slice, none of them stalled |
| `unavailable: <error>` | `fseventsd` could not be sampled | One sample after `fseventsd` reads again; deletion waits out that 5s interval |

A slice that ends at its 50ms is the budget working, and does not throttle.
Both throttles can hold at once. The status then names the stalled slices, and
the full rate returns once each has lifted. At the floor, a slice that takes
its whole 50ms rests a full second whatever it removed, so deletion can run
below 100 entries a second.

The floor keeps the queue draining on a machine whose `fseventsd` never goes
quiet. A job a wait command is polling skips the queue only. It deletes at the
rate the governor allows.

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
