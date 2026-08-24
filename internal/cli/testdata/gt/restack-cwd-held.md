gt restack driven with --cwd from the second worktree that holds the branch a
plain restack declines.

Recorded offline, no token. The counterpart to restack-worktree-held: the same
repository, the same held branch, and gt restacks it because the run happens
inside the working copy holding it. Pins that --cwd reaches a branch the
invoking checkout cannot move, which is what gtLaneRestack sweeps a stack with,
and that the run still declines whatever this lane does not hold — one gt run
never restacks a whole stack spread across working copies.

The recorded paths are the recorder's work root (CCX_GT_RECORD_ROOT, default
/tmp/ccx-gt-record) as git resolves it, and --cwd puts one in the argv as well.
Nothing reads the paths themselves; the tests read the shape.
