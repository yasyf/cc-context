package cleanup

import (
	"context"
	"time"
)

// GuardRoot names a directory and the retiring watchers whose descriptors are excluded.
type GuardRoot struct {
	Path     string
	Retiring []ProcessID
}

// Guard authorizes mutation only when every root has no live holder. The first
// blocking root and its activity or inspection error are returned in input order.
// Watcher allowances belong to each root; requester allowances remain in ctx.
type Guard func(ctx context.Context, first GuardRoot, additional ...GuardRoot) (failedPath string, err error)

// WatcherProcesses separates the server from all watchers retiring with a tree.
type WatcherProcesses struct {
	Retiring []ProcessID
	Server   []ProcessID
}

// Watchers retires the filesystem watchers of an exact worktree before it is
// relocated, and vouches that a job folder is unwatched. Any error blocks the
// job; nothing is retried.
type Watchers interface {
	// Retiring names the server separately from all watchers whose descriptors
	// retirement discounts. Both sets come from the same read-only census.
	Retiring(ctx context.Context, worktree string) (WatcherProcesses, error)
	// Server names, changing nothing, the running Watchman server, or nothing
	// when none runs. It keeps a tree's directories open after their roots are
	// deleted, and the open directory follows the tree wherever it is moved,
	// so every guard discounts it.
	Server(ctx context.Context) ([]ProcessID, error)
	// Retire stops the watchers rooted in worktree.
	Retire(ctx context.Context, worktree string) error
	// CheckQuarantine reports whether jobDir sits outside every watched tree.
	CheckQuarantine(ctx context.Context, jobDir string) error
}

// CPUSampler reads fseventsd's cumulative CPU time. Two samples a known
// interval apart give the load deletion is throttled on.
type CPUSampler interface {
	Sample(ctx context.Context) (time.Duration, error)
}

// Band moves the daemon into the default band while a client waits on it and
// back to the background band; the processes it starts inherit the band.
type Band interface {
	Foreground() error
	Background() error
}

// Relocator performs the logical half of a removal against the journal. The
// daemon calls it from one goroutine, so no two of its methods ever run
// together.
type Relocator interface {
	// Accept preflights a removal and journals it at PhasePrepared under seq. A
	// refusal is a *RefusedError or an *ActiveError, with nothing journaled and
	// nothing mutated.
	Accept(ctx context.Context, seq uint64, r Request) (Job, error)
	// Pend journals a removal asked for while the queue is paused at
	// PhasePending under seq. It captures the registration without consulting
	// watchers or activity; later Retire checks inactivity before hiding it.
	// Pend changes only the journal.
	Pend(ctx context.Context, seq uint64, r Request) (Job, error)
	// Intend journals a deferred removal under seq, capturing the tree's
	// identity and mutating nothing else: at PhaseQueued when the guard finds
	// the tree idle, at PhaseWaiting with the holders noted when it reports the
	// tree held, and at PhaseWaiting with the failure noted when it could not
	// inspect the tree. A tree that is not a registered linked worktree of
	// r.CommonDir, or whose registration is not r.Expected, is a *RefusedError
	// with nothing journaled.
	Intend(ctx context.Context, seq uint64, r DeferRequest) (Job, error)
	// Adopt verifies a tree the retired janitor parked at r.Source and journals
	// it at PhasePrepared under seq, moving nothing. A refusal is a
	// *RefusedError or an *ActiveError with nothing journaled. Advance then
	// takes an adopted job from Source straight to Payload.
	Adopt(ctx context.Context, seq uint64, r AdoptRequest) (Job, error)
	// Advance moves job toward PhaseUnregistered from whatever phase it holds,
	// journaling each phase before the step that leaves it. It first reconciles
	// the phase against the filesystem, so it is equally the resume of a job a
	// crash interrupted and the retry of one an operator unblocked.
	//
	// From PhasePending it runs Accept's whole preflight again, and blocks the
	// job unless that passes for the tree the job captured.
	//
	// It returns nil with job at PhaseUnregistered or PhaseDeletingAdmin on success, nil with job at
	// PhaseWaiting when a deferred job's tree is still held, and nil with
	// job.Blocked set when the job needs an operator. The error is the journal's
	// when it could not be written, and ctx's when ctx was cancelled, leaving
	// the record unblocked at its last journaled phase for the next Advance. A
	// step rewriting git's state runs on past the cancellation until it ends or
	// expires its budget; a git step that expired its own budget journals a
	// timeout blockage and returns nil, cancelled or not.
	Advance(ctx context.Context, job *Job) error
	// Retire hides an inactive worktree's registration while retaining its checkout in place.
	// Registration-rebinding maintenance must not overlap retirement; a maintenance hold excludes it.
	Retire(ctx context.Context, job *Job) error
	// Admit supplies the captured root to open after proving its ownership,
	// registration, watcher and activity constraints afresh. External admin
	// completion is journaled before its payload can be admitted. A failed
	// proof blocks the job; errors report journal failure or cancellation.
	Admit(ctx context.Context, job *Job) (DeletionRoot, error)
}

// DeletionRoot names one captured directory admitted for descriptor-safe deletion.
type DeletionRoot struct {
	Dir  string
	Name string
	ID   FileID
}

// Deleter opens the physical deletion of one captured root.
type Deleter interface {
	// Open starts deleting the directory name inside dir, following no symlink
	// on the way. The error wraps fs.ErrNotExist when nothing is there and
	// ErrIdentity when what is there is not the tree want names.
	Open(dir, name string, want FileID) (Deletion, error)
}

// Deletion is one captured root's deletion in progress. It holds its place in the
// tree between steps, so a step never rescans what an earlier one covered.
type Deletion interface {
	// Step removes at most limit entries, stopping early once budget has
	// elapsed. done reports that the payload directory itself is gone.
	Step(limit int, budget time.Duration) (removed int, done bool, err error)
	// Close releases the deletion's place in the tree without removing more.
	Close() error
}
