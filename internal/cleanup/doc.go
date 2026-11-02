// Package cleanup is the contract of ccx's worktree removal daemon: the durable
// job record, the private state layout, and the seams its halves meet at.
//
// A removal is logical first and physical afterwards. The logical half proves
// a linked worktree safe to delete where it sits and locks its registration
// for the job, so no git command can move, prune or remove it once the command
// returns. The physical half is the per-user daemon deleting the tree, then
// its registration, in place at a bounded pace, yielding to every new logical
// removal.
package cleanup
