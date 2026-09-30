// Package native asks the darwin kernel what a worktree removal depends on:
// which live processes work inside a tree, and how much CPU fseventsd has
// burned. It binds libSystem without cgo, so a release cross-compiled from
// Linux still carries it. Every other platform reports cleanup.ErrUnsupported.
package native

import "github.com/yasyf/cc-context/internal/cleanup"

var (
	_ cleanup.Guard      = Guard
	_ cleanup.CPUSampler = (*FSEventsSampler)(nil)
)
