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

## Queue commands

The macOS daemon owns the deletion queue. It does not run stacks or change
their publication rules. The public command group is `ccx vcs cleanup`.

| Command | Result |
| --- | --- |
| `ccx vcs cleanup status [job-id] --json` | Report the queue, or one job, with progress and any blockage |
| `ccx vcs cleanup wait <job-id>` | Wait for one job to finish; report a blockage if it cannot proceed |
| `ccx vcs cleanup pause` | Pause physical deletion across the queue |
| `ccx vcs cleanup resume` | Resume physical deletion across the queue |
| `ccx vcs cleanup retry <job-id>` | Retry a blocked job from its recorded phase after its cause is addressed |
| `ccx vcs cleanup watchers --json` | Inspect watcher roots and consumers without retiring them |

`ccx vcs cleanup status` and `ccx vcs cleanup wait` are read-only: they query
only the running daemon with one 5s hello and bounded requests, including
older versions that speak the same protocol. `cleanup wait` polls status
with a 10s limit per request and backs off up to 1s between polls; it reports
a finished job pruned from the queue as finished. `cleanup status`,
`cleanup wait`, and `cleanup retry` reject an empty job ID.
They never install a program copy, stop or replace the daemon, apply the
`LaunchAgent`, or take the start lock, and both fail if no daemon is running.
`launchd` starts the daemon at login; queue-changing commands that reach the
daemon (`ccx vcs worktree rm`, `ccx vcs cleanup pause`/`resume`/`retry`/`adopt`,
and deferred removals) install and start or replace it, with a 15s agent
timeout on `LaunchAgent` apply. If the daemon accepts and closes without
replying, never sends its hello, or holds its serve lock behind a refusing or
missing socket, those commands probe once more under the start lock and
report an error if it still does not answer, before any install or
`LaunchAgent` apply. During the v0.67.3 upgrade incident, the draining daemon
accepted each new connection and closed it at once without a reply, so status
saw an empty reply and the shutdown wait never saw a refusal.

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
| Paused | Deletion is held; the job remains durable |
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

The daemon limits physical deletion to bounded slices and a capped rate,
yields to new logical removals, and pauses deletion while `fseventsd` is busy.
It does not run idle repository or process scans.

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
When a process or Watchman probe times out, or the activity guard cannot read
a process's arguments, the failure is a warning: the tree is removed only if it
has no uncommitted changes and a remote-tracking ref holds its head, and the
activity guard still refuses any holder it finds.

Scope watcher configuration changes to the authorized unused trees too.
Applying settings to an existing root requires checking that it is idle before
recreating that exact watch. Keep active source, generated-input, and build-tool
watches intact. Do not disable FSEvents or watchers globally to reduce churn.
