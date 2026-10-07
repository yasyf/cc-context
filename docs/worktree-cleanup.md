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
| `ccx vcs cleanup pause` | Stop preparation, relocation, and deletion; refuse new removals |
| `ccx vcs cleanup resume` | Resume the queue |
| `ccx vcs cleanup retry <job-id>` | Retry a blocked job from its recorded phase after its cause is addressed |
| `ccx vcs cleanup watchers --json` | Inspect watcher roots and consumers without retiring them |

`pause` holds queued jobs at their recorded phases until `resume`, with no
retries or inactivity checks. The pause survives daemon restarts. While
paused, the daemon rejects `worktree rm`, `cleanup adopt`, and deferred
workspace releases before any preparation or Git work. The error names
`ccx vcs cleanup resume`. A stack rebase, `stack continue`, or `stack abort`
that meets a paused queue still finishes and leaves its conflict workspace in
place, naming the `ccx vcs worktree rm` that removes it after resume.

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
awaited jobs, in queue order, ahead of every job nobody awaits, without the rate
cap or the `fseventsd` throttle. A wait then lasts about as long as its own
tree's deletion, plus any awaited trees queued ahead of it. Logical removals
still go first, and a paused queue or a blocked job still holds.

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

## Completion and recovery

| Reported outcome | Meaning |
| --- | --- |
| Logical removal succeeded | The original tree is gone and its Git registration is removed; queued data can still occupy disk space |
| Waiting for inactivity | A completed internal workspace remains at its original path while a session holds it |
| Physical deletion in progress | The daemon is deleting the relocated data in bounded slices |
| Paused | Preparation, relocation, and deletion are held at the recorded phase |
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
yields to new logical removals. It samples `fseventsd` every 5s and learns its
ambient load from samples taken while deletion ran no faster than its floor.
It does not run idle repository or process scans.

When deletion pushes `fseventsd` more than half a core over that load, the
daemon slows to 100 entries a second. It returns to the full rate after three
samples within a quarter core of it. The floor keeps the queue draining on a
machine whose `fseventsd` never goes quiet. A job a wait command is polling
skips the queue, the rate cap, and the floor.

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
When a process or Watchman probe times out, or the activity guard cannot read
a process's arguments, the failure is a warning: the tree is removed only if it
has no uncommitted changes and a remote-tracking ref holds its head, and the
activity guard still refuses any holder it finds.

Scope watcher configuration changes to the authorized unused trees too.
Applying settings to an existing root requires checking that it is idle before
recreating that exact watch. Keep active source, generated-input, and build-tool
watches intact. Do not disable FSEvents or watchers globally to reduce churn.
