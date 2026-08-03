gt restack with the stack's branch checked out in a second worktree.

Recorded offline, no token. gt declines the branch, says so on stdout, and still
exits 0 — the exit-0-that-did-nothing case gtZeroSurfaces exists for. Pins
gtSyncSkippedPrefix / gtSyncSkippedReason / gtSyncSkippedWorktree, which
gtSyncSkipped cuts a branch and its reason out of, and pins that the line
carries no ERROR: prefix, so reportedError stays false.

The recorded path is the recorder's work root (CCX_GT_RECORD_ROOT, default
/tmp/ccx-gt-record) as git resolves it, so it differs between a macOS and a
Linux recording. Nothing reads the path itself; the tests read the shape.
