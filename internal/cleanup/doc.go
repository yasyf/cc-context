// Package cleanup is the contract of ccx's worktree removal daemon: the durable
// job record, the private state layout, and the seams its halves meet at.
//
// A removal is logical first and physical afterwards. The logical half
// relocates a linked worktree into a private job folder and drops its
// registration, so the path the user named is free the moment the command
// returns. The physical half is the per-user daemon deleting the relocated
// payload at a bounded pace, yielding to every new logical removal.
package cleanup
