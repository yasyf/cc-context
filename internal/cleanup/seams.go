package cleanup

import (
	"context"
	"time"
)

// Guard is the fresh activity check that authorizes a mutation of worktree. It
// returns nil only when no live process holds the tree, an *ActiveError when
// one does, and any other error when it could not tell — which authorizes
// nothing. Holding is a working directory, an open file, or an argument at or
// below the tree. The only discounts are the ones ctx names: the requester,
// for naming the tree in its own arguments, and the watchers about to be
// retired or that never let go, for the descriptors they hold.
type Guard func(ctx context.Context, worktree string) error

// Watchers retires the filesystem watchers of an exact worktree before it is
// relocated, and vouches that a job folder is unwatched. Any error blocks the
// job; nothing is retried.
type Watchers interface {
	// Retiring names, changing nothing, the running Watchman server and the
	// fsmonitor daemon Retire would stop. Both hold descriptors in worktree,
	// so a guard asked before retirement discounts exactly these.
	Retiring(ctx context.Context, worktree string) ([]ProcessID, error)
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

// Relocator performs the logical half of a removal against the journal. The
// daemon calls it from one goroutine, so no two of its methods ever run
// together.
type Relocator interface {
	// Accept preflights a removal and journals it at PhasePrepared under seq. A
	// refusal is a *RefusedError or an *ActiveError, with nothing journaled and
	// nothing mutated.
	Accept(ctx context.Context, seq uint64, r Request) (Job, error)
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
	// It returns nil with job at PhaseUnregistered on success, nil with job at
	// PhaseWaiting when a deferred job's tree is still held, and nil with
	// job.Blocked set when the job needs an operator. The error is the journal's
	// when it could not be written, and ctx's when ctx was cancelled, leaving
	// the record unblocked at its last journaled phase for the next Advance. A
	// step rewriting git's state runs on past the cancellation until it ends or
	// expires its budget; a git step that expired its own budget journals a
	// timeout blockage and returns nil, cancelled or not.
	Advance(ctx context.Context, job *Job) error
	// Admit is the gate every physical deletion passes each time its payload is
	// opened: first, after a restart, and after any pause. It proves afresh
	// that the payload is the captured tree, that no git registration names
	// the payload or anything inside the job folder, that the job folder is
	// unwatched, and that no live process holds the payload. A failed proof
	// blocks the job and returns nil; the error is non-nil only when the
	// journal could not be written or ctx was cancelled.
	Admit(ctx context.Context, job *Job) error
}

// Deleter opens the physical deletion of one relocated payload.
type Deleter interface {
	// Open starts deleting the directory name inside dir, following no symlink
	// on the way. The error wraps fs.ErrNotExist when nothing is there and
	// ErrIdentity when what is there is not the tree want names.
	Open(dir, name string, want FileID) (Deletion, error)
}

// Deletion is one payload's deletion in progress. It holds its place in the
// tree between steps, so a step never rescans what an earlier one covered.
type Deletion interface {
	// Step removes at most limit entries, stopping early once budget has
	// elapsed. done reports that the payload directory itself is gone.
	Step(limit int, budget time.Duration) (removed int, done bool, err error)
	// Close releases the deletion's place in the tree without removing more.
	Close() error
}
