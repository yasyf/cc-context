// Package daemon is the cleanup daemon itself: the scheduler that interleaves
// logical removals with paced physical deletion, the unix-socket server in
// front of it, and the client that reaches it.
package daemon

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"iter"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/yasyf/cc-context/internal/cleanup"
)

const (
	progressEvery = time.Minute
	reasonWaiting = "waiting"
)

// ErrStopped reports a request the engine could not take because its worker
// has returned or is stopping.
var ErrStopped = errors.New("cleanup daemon: the engine has stopped")

// Clock is the engine's only source of time, so a test drives every wait.
type Clock interface {
	Now() time.Time
	After(d time.Duration) <-chan time.Time
}

type systemClock struct{}

func (systemClock) Now() time.Time                         { return time.Now() }
func (systemClock) After(d time.Duration) <-chan time.Time { return time.After(d) }

// Tuning is the pace of physical deletion and the bounds of what the engine
// retains and reports.
type Tuning struct {
	// Rate caps the entries deleted per second while fseventsd keeps near its
	// ambient load and no slice stalls past twice SliceBudget. FloorRate caps
	// it otherwise, an unreadable fseventsd included.
	Rate      int
	FloorRate int
	// SliceEntries and SliceBudget bound one uninterruptible step, which rests
	// for the larger share it used of either bound, a full slice's rest at most.
	SliceEntries int
	SliceBudget  time.Duration
	// SampleEvery is the interval between fseventsd CPU samples while deletion
	// is pending.
	SampleEvery time.Duration
	// PauseAbove and ResumeBelow are percentages of one core over fseventsd's
	// ambient load, the average of its samples while deletion ran at most at
	// FloorRate.
	PauseAbove  float64
	ResumeBelow float64
	// ResumeSamples is how many consecutive samples under ResumeBelow lift an
	// fseventsd throttle, and how many with no stalled slice lift a stall one.
	ResumeSamples int
	// Recheck is the interval between activity checks of a waiting job.
	Recheck time.Duration
	// RetryAfter is the first retry of a job blocked on a transient
	// inspection; each consecutive transient block doubles it up to
	// RetryCeiling. After RetryLimit consecutive transient blocks since the
	// daemon loaded its journal the job waits for Retry.
	RetryAfter   time.Duration
	RetryCeiling time.Duration
	RetryLimit   int
	// AwaitLease is how long one awaiting status read keeps its job ahead of
	// the queue.
	AwaitLease time.Duration
	// KeepDone is how many finished records the journal retains.
	KeepDone int
	// StatusLimit is the default cap on the jobs one report carries.
	StatusLimit int
}

// DefaultTuning is the production pace: 1000 entries a second in slices of at
// most 100 entries or 50ms, and at most 100 a second while the governor
// throttles.
func DefaultTuning() Tuning {
	return Tuning{
		Rate:          1000,
		FloorRate:     100,
		SliceEntries:  100,
		SliceBudget:   50 * time.Millisecond,
		SampleEvery:   5 * time.Second,
		PauseAbove:    50,
		ResumeBelow:   25,
		ResumeSamples: 3,
		Recheck:       30 * time.Second,
		RetryAfter:    30 * time.Second,
		RetryCeiling:  10 * time.Minute,
		RetryLimit:    6,
		AwaitLease:    10 * time.Second,
		KeepDone:      100,
		StatusLimit:   50,
	}
}

func (t Tuning) validate() error {
	switch {
	case t.Rate <= 0, t.SliceEntries <= 0, t.SliceBudget <= 0:
		return fmt.Errorf("rate %d, slice of %d entries or %s: all must be positive", t.Rate, t.SliceEntries, t.SliceBudget)
	case t.FloorRate <= 0, t.FloorRate > t.Rate:
		return fmt.Errorf("floor rate %d must be positive and at most the rate %d", t.FloorRate, t.Rate)
	case t.SampleEvery <= 0, t.Recheck <= 0, t.RetryAfter <= 0, t.RetryCeiling < t.RetryAfter, t.RetryLimit <= 0:
		return fmt.Errorf("sample interval %s, recheck interval %s, first retry %s and retry limit %d must be positive, and the retry ceiling %s at least the first retry", t.SampleEvery, t.Recheck, t.RetryAfter, t.RetryLimit, t.RetryCeiling)
	case t.ResumeBelow <= 0, t.PauseAbove < t.ResumeBelow, t.ResumeSamples <= 0:
		return fmt.Errorf("throttle pauses above %v and resumes after %d samples below %v", t.PauseAbove, t.ResumeSamples, t.ResumeBelow)
	case t.AwaitLease <= 0:
		return fmt.Errorf("await lease %s must be positive", t.AwaitLease)
	case t.KeepDone < 0, t.StatusLimit <= 0:
		return fmt.Errorf("keeps %d finished records and reports %d jobs", t.KeepDone, t.StatusLimit)
	}
	return nil
}

// Config is everything an Engine is built from. The two halves of a removal
// and the CPU sampler are injected, so the engine itself is portable.
type Config struct {
	Journal   *cleanup.Journal
	Relocator cleanup.Relocator
	Deleter   cleanup.Deleter
	CPU       cleanup.CPUSampler
	// Band runs commands and paused registration retirement in the default
	// band, and physical deletion in the background band.
	Band    cleanup.Band
	Version string
	// Parent names a client's parent process and its arguments, logged with
	// every pause and resume.
	Parent func(pid int) (int, []string, error)
	// Identify names the client process at pid with the second the kernel
	// started it, so a request's requester outlives the request only as that
	// exact process.
	Identify func(pid int) (cleanup.ProcessID, error)
	// Clock is the real clock when nil.
	Clock Clock
	// Tuning is DefaultTuning when zero.
	Tuning Tuning
}

type command struct {
	run    func(ctx context.Context) error
	refuse func()
}

type fatalError struct{ err error }

func (f *fatalError) Error() string { return f.err.Error() }
func (f *fatalError) Unwrap() error { return f.err }

type activeDeletion struct {
	id       string
	deletion cleanup.Deletion
	unsaved  bool
	savedAt  time.Time
}

// Engine is the daemon's scheduler. One worker goroutine, Run, owns every
// mutation and every journal read and write; the Service methods only post to
// it and read snapshots.
type Engine struct {
	journal   *cleanup.Journal
	layout    cleanup.Layout
	relocator cleanup.Relocator
	deleter   cleanup.Deleter
	cpu       cleanup.CPUSampler
	band      cleanup.Band
	parent    func(pid int) (int, []string, error)
	identify  func(pid int) (cleanup.ProcessID, error)
	version   string
	clock     Clock
	tuning    Tuning

	commands chan command
	quit     chan struct{}
	quitOnce sync.Once
	loaded   chan struct{}
	done     chan struct{}
	started  atomic.Bool

	mu       sync.Mutex
	jobs     map[string]cleanup.Job
	waiters  map[string]int
	awaits   map[string]time.Time
	kept     map[string]cleanup.Job
	damaged  []cleanup.Damaged
	paused   bool
	hold     *cleanup.Hold
	booted   time.Time
	governed cleanup.Governor
	changed  chan struct{}

	seq             uint64
	order           []seqKey
	logicalOrder    []seqKey
	retirementOrder []seqKey
	wakeOrder       []seqKey
	gov             *governor
	checked         map[string]time.Time
	active          *activeDeletion
	paceUntil       time.Time
	loud            bool
}

var (
	_ cleanup.Service = (*Engine)(nil)
	_ cleanup.Adopter = (*Engine)(nil)
)

// New builds an Engine over cfg's journal and reads nothing from it: Run loads
// the records, so a daemon holds its serve lock before anything reads or
// repairs them.
func New(cfg Config) (*Engine, error) {
	if cfg.Journal == nil || cfg.Relocator == nil || cfg.Deleter == nil || cfg.CPU == nil || cfg.Band == nil || cfg.Parent == nil || cfg.Identify == nil {
		return nil, errors.New("cleanup daemon: config needs a journal, a relocator, a deleter, a cpu sampler, a band, a parent lookup, and a process identity lookup")
	}
	tuning := cfg.Tuning
	if tuning == (Tuning{}) {
		tuning = DefaultTuning()
	}
	if err := tuning.validate(); err != nil {
		return nil, fmt.Errorf("cleanup daemon: tuning: %w", err)
	}
	clock := cfg.Clock
	if clock == nil {
		clock = systemClock{}
	}
	gov := newGovernor(tuning)
	return &Engine{
		journal:   cfg.Journal,
		layout:    cfg.Journal.Layout(),
		relocator: cfg.Relocator,
		deleter:   cfg.Deleter,
		cpu:       cfg.CPU,
		band:      cfg.Band,
		parent:    cfg.Parent,
		identify:  cfg.Identify,
		version:   cfg.Version,
		clock:     clock,
		tuning:    tuning,
		commands:  make(chan command),
		quit:      make(chan struct{}),
		loaded:    make(chan struct{}),
		done:      make(chan struct{}),
		jobs:      make(map[string]cleanup.Job),
		waiters:   make(map[string]int),
		awaits:    make(map[string]time.Time),
		kept:      make(map[string]cleanup.Job),
		governed:  gov.report(),
		changed:   make(chan struct{}),
		gov:       gov,
		checked:   make(map[string]time.Time),
	}, nil
}

// Run is the single worker. It loads the journal, resumes every unfinished job
// from its recorded phase, and returns nil once ctx is done or Stop is called,
// after journaling the active deletion's progress. Stop cancels the running
// step's context too, so a stop is seen at that step's next check. It returns
// an error only when the journal could not be read or written.
func (e *Engine) Run(ctx context.Context) error {
	if !e.started.CompareAndSwap(false, true) {
		return errors.New("cleanup daemon: the engine is already running")
	}
	defer close(e.done)
	if e.stopping(ctx) {
		return nil
	}
	if err := e.load(); err != nil {
		return err
	}
	steps, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		select {
		case <-e.quit:
			cancel()
		case <-steps.Done():
		}
	}()
	return errors.Join(e.work(steps), e.park())
}

// Stop cancels the worker's current step and waits for it to return: a git
// read is killed and its job rests unblocked at the phase on record, a git
// mutation already running finishes or expires at its budget (journaled as a
// timeout) before the step yields, and a slice in flight completes. A command
// not started by then is answered with ErrStopped. Idempotent; works before Run.
func (e *Engine) Stop(ctx context.Context) error {
	e.quitOnce.Do(func() { close(e.quit) })
	if !e.started.Load() {
		return nil
	}
	select {
	case <-e.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Remove relocates and unregisters a worktree on the worker. Once the worker
// has taken the request it runs to the end of its ladder step even when ctx is
// cancelled; ctx bounds only the caller's wait, and lends the worker nothing
// but the requester it names, which the job keeps for its later passes. A
// request for a tree whose deferred removal still waits on its holders is a
// *cleanup.RefusedError naming that job, and a request without Force never
// rejoins a job journaled with it: it is preflighted afresh. While the queue is
// paused or held a new request is journaled pending and a repeat rejoins its
// job, and either returns with nothing moved.
func (e *Engine) Remove(ctx context.Context, r cleanup.Request) (cleanup.Receipt, error) {
	if err := r.Validate(); err != nil {
		return cleanup.Receipt{}, fmt.Errorf("cleanup daemon: remove request: %w", err)
	}
	return call(ctx, e, onBehalf(ctx, func(ctx context.Context) (cleanup.Receipt, error) { return e.remove(ctx, r) }))
}

// Defer journals a removal that waits for inactivity. A workspace still held
// comes back waiting, its receipt naming the holders. Only this first look
// runs on behalf of ctx's requester; the rechecks that follow name nobody. A
// repeat rejoins a job only when that job journaled the registration the
// request expects, whatever its path holds by then, and a request without
// Force never rejoins a job journaled with it.
func (e *Engine) Defer(ctx context.Context, r cleanup.DeferRequest) (cleanup.Receipt, error) {
	if err := r.Validate(); err != nil {
		return cleanup.Receipt{}, fmt.Errorf("cleanup daemon: defer request: %w", err)
	}
	return call(ctx, e, onBehalf(ctx, func(ctx context.Context) (cleanup.Receipt, error) { return e.intend(ctx, r) }))
}

// Adopt journals a tree the retired janitor parked and relocates it into its
// job folder on the worker. A repeat of a request whose tree still sits at its
// source rejoins the job the first one journaled. While the queue is paused or
// held it is cleanup.ErrPaused, before any preflight or record.
func (e *Engine) Adopt(ctx context.Context, r cleanup.AdoptRequest) (cleanup.Receipt, error) {
	if err := r.Validate(); err != nil {
		return cleanup.Receipt{}, fmt.Errorf("cleanup daemon: adopt request: %w", err)
	}
	return call(ctx, e, onBehalf(ctx, func(ctx context.Context) (cleanup.Receipt, error) { return e.adopt(ctx, r) }))
}

// Status reads a snapshot and never waits behind the worker once Run has
// loaded the journal: unfinished jobs in queue order, then finished jobs
// newest first, capped at the limit. A query that awaits an unfinished job
// renews that job's lease ahead of the queue.
func (e *Engine) Status(ctx context.Context, q cleanup.Query) (cleanup.Report, error) {
	if err := e.ready(ctx); err != nil {
		return cleanup.Report{}, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	report := cleanup.Report{
		Version:  e.version,
		PID:      os.Getpid(),
		Paused:   e.halted(),
		Governor: e.governed,
		Jobs:     []cleanup.Job{},
		Damaged:  slices.Clone(e.damaged),
	}
	if q.JobID != "" {
		job, ok := e.jobs[q.JobID]
		if !ok {
			return cleanup.Report{}, cleanup.ErrUnknownJob
		}
		if q.Await && job.Phase != cleanup.PhaseDone {
			e.awaits[q.JobID] = e.clock.Now()
		}
		report.Jobs = append(report.Jobs, e.reported(job))
		return report, nil
	}
	limit := q.Limit
	if limit <= 0 {
		limit = e.tuning.StatusLimit
	}
	var finished []cleanup.Job
	for job := range e.ordered() {
		if job.Phase == cleanup.PhaseDone {
			finished = append(finished, job)
			continue
		}
		report.Jobs = append(report.Jobs, e.reported(job))
		if len(report.Jobs) == limit {
			report.Omitted = len(e.jobs) - len(report.Jobs)
			return report, nil
		}
	}
	slices.SortFunc(finished, func(a, b cleanup.Job) int { return byFinish(b, a) })
	for _, job := range finished[:min(len(finished), limit-len(report.Jobs))] {
		report.Jobs = append(report.Jobs, clone(job))
	}
	report.Omitted = len(e.jobs) - len(report.Jobs)
	return report, nil
}

// Wait returns the job once it is done, and the job with a *BlockedError once
// it blocks for an operator; a transient blockage the daemon retries on its own
// keeps the caller waiting, as does a queue-wide pause. A caller already
// waiting when the job finishes gets the finished job even when its record is
// pruned in the same breath.
func (e *Engine) Wait(ctx context.Context, jobID string) (cleanup.Job, error) {
	if err := e.ready(ctx); err != nil {
		return cleanup.Job{}, err
	}
	e.mu.Lock()
	_, known := e.jobs[jobID]
	if known {
		e.waiters[jobID]++
	}
	e.mu.Unlock()
	if !known {
		return cleanup.Job{}, cleanup.ErrUnknownJob
	}
	defer e.leave(jobID)
	stopped := false
	for {
		e.mu.Lock()
		job, live := e.jobs[jobID]
		if !live {
			job, live = e.kept[jobID]
		}
		job, changed := e.reported(job), e.changed
		e.mu.Unlock()
		switch {
		case !live:
			return cleanup.Job{}, cleanup.ErrUnknownJob
		case job.Phase == cleanup.PhaseDone:
			return job, nil
		case job.Blocked != nil && job.Blocked.RetryAt == nil:
			return job, &cleanup.BlockedError{Job: job}
		case stopped:
			return cleanup.Job{}, ErrStopped
		}
		select {
		case <-changed:
		case <-e.done:
			stopped = true
		case <-ctx.Done():
			return cleanup.Job{}, ctx.Err()
		}
	}
}

// Pause stops physical deletion, closes its active payload, and journals progress
// before returning. Inactive linked worktrees may still have their registrations
// retired in place. Hold excludes that logical maintenance as well. Remove and
// Defer journal retained workspaces; Adopt is refused. The pause survives restart.
func (e *Engine) Pause(ctx context.Context) error {
	_, err := call(ctx, e, func(context.Context) (struct{}, error) { return struct{}{}, e.pause(true) })
	return err
}

// Resume lets the queue run again: rested jobs go on from their phase, pending
// removals are preflighted afresh in queue order, new removals are taken, and
// a payload is admitted and reopened, and so re-verified, before another entry
// goes. While a maintenance hold stands it is a *cleanup.HeldError and changes
// nothing.
func (e *Engine) Resume(ctx context.Context) error {
	_, err := call(ctx, e, func(context.Context) (struct{}, error) { return struct{}{}, e.pause(false) })
	return err
}

// Hold takes the maintenance hold and, as Pause does, returns only once the
// deletion it interrupts is closed and its progress journaled. The hold is
// journaled in the pause switch, so a restarted daemon stays held.
func (e *Engine) Hold(ctx context.Context, r cleanup.HoldRequest) (cleanup.HoldReceipt, error) {
	if err := r.Validate(); err != nil {
		return cleanup.HoldReceipt{}, fmt.Errorf("cleanup daemon: hold request: %w", err)
	}
	return call(ctx, e, func(context.Context) (cleanup.HoldReceipt, error) { return e.take(r) })
}

// Release lifts the maintenance hold id names and journals the switch with the
// ordinary pause as it stands, so a queue paused before or during the hold
// stays paused.
func (e *Engine) Release(ctx context.Context, id string) error {
	_, err := call(ctx, e, func(context.Context) (struct{}, error) { return struct{}{}, e.release(id) })
	return err
}

// HoldState reads a snapshot of the pause switch, the ordinary pause and the
// maintenance hold apart, and never waits behind the worker once Run has loaded
// the journal.
func (e *Engine) HoldState(ctx context.Context) (cleanup.HoldState, error) {
	if err := e.ready(ctx); err != nil {
		return cleanup.HoldState{}, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	state := cleanup.HoldState{Paused: e.paused}
	if e.hold != nil {
		hold := *e.hold
		state.Hold = &hold
	}
	return state, nil
}

// Retry clears a job's blockage and returns the job as journaled; the worker's
// next pass resumes it from the phase it stopped in.
func (e *Engine) Retry(ctx context.Context, jobID string) (cleanup.Job, error) {
	return call(ctx, e, func(context.Context) (cleanup.Job, error) { return e.retry(jobID) })
}

// Cancel undoes a removal whose tree has not begun to move and drops its job,
// leaving the worktree registered in place and its recovery ref kept. A
// maintenance hold refuses it as a *HeldError.
func (e *Engine) Cancel(ctx context.Context, jobID string) (cleanup.Job, error) {
	return call(ctx, e, func(ctx context.Context) (cleanup.Job, error) { return e.cancel(ctx, jobID) })
}

type result[T any] struct {
	value T
	err   error
}

func onBehalf[T any](caller context.Context, fn func(context.Context) (T, error)) func(context.Context) (T, error) {
	pid, named := cleanup.RequesterFrom(caller)
	if !named {
		return fn
	}
	return func(ctx context.Context) (T, error) { return fn(cleanup.WithRequester(ctx, pid)) }
}

func isFatal(err error) bool {
	var fatal *fatalError
	return errors.As(err, &fatal)
}

func call[T any](ctx context.Context, e *Engine, fn func(context.Context) (T, error)) (T, error) {
	var zero T
	reply := make(chan result[T], 1)
	cmd := command{
		run: func(ctx context.Context) error {
			value, err := fn(ctx)
			reply <- result[T]{value, err}
			if isFatal(err) {
				return err
			}
			return nil
		},
		refuse: func() { reply <- result[T]{zero, ErrStopped} },
	}
	select {
	case e.commands <- cmd:
	case <-e.done:
		return zero, ErrStopped
	case <-e.quit:
		return zero, ErrStopped
	case <-ctx.Done():
		return zero, ctx.Err()
	}
	select {
	case r := <-reply:
		return r.value, r.err
	case <-ctx.Done():
		return zero, ctx.Err()
	}
}

func (e *Engine) ready(ctx context.Context) error {
	select {
	case <-e.loaded:
		return nil
	default:
	}
	select {
	case <-e.loaded:
		return nil
	case <-e.done:
	case <-e.quit:
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case <-e.loaded:
		return nil
	default:
		return ErrStopped
	}
}

func (e *Engine) leave(jobID string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.waiters[jobID]--
	if e.waiters[jobID] > 0 {
		return
	}
	delete(e.waiters, jobID)
	delete(e.kept, jobID)
}

func (e *Engine) load() error {
	jobs, damaged, err := e.journal.Load()
	if err != nil {
		return err
	}
	sw, err := e.journal.LoadSwitch()
	if err != nil {
		return fmt.Errorf("cleanup daemon: load the pause switch: %w", err)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, job := range jobs {
		e.jobs[job.ID] = job
		e.order = append(e.order, seqKey{seq: job.Seq, id: job.ID})
		e.seq = max(e.seq, job.Seq)
	}
	slices.SortFunc(e.order, func(a, b seqKey) int { return cmp.Compare(a.seq, b.seq) })
	e.damaged, e.paused, e.hold, e.booted = damaged, sw.Paused, sw.Hold, e.clock.Now()
	for _, key := range e.order {
		logical, retirement, wake := e.scheduled(e.jobs[key.id])
		if logical {
			e.logicalOrder = append(e.logicalOrder, key)
		}
		if retirement {
			e.retirementOrder = append(e.retirementOrder, key)
		}
		if wake {
			e.wakeOrder = append(e.wakeOrder, key)
		}
	}
	close(e.loaded)
	return nil
}

func (e *Engine) stopping(ctx context.Context) bool {
	select {
	case <-ctx.Done():
		return true
	case <-e.quit:
		return true
	default:
		return false
	}
}

func (e *Engine) execute(ctx context.Context, cmd command) error {
	if e.stopping(ctx) {
		cmd.refuse()
		return nil
	}
	if err := e.band.Foreground(); err != nil {
		cmd.refuse()
		return fmt.Errorf("cleanup daemon: leave the background band: %w", err)
	}
	err := cmd.run(ctx)
	if bandErr := e.band.Background(); bandErr != nil {
		return errors.Join(err, fmt.Errorf("cleanup daemon: return to the background band: %w", bandErr))
	}
	return err
}

func (e *Engine) work(ctx context.Context) error {
	for {
		if e.stopping(ctx) {
			return nil
		}
		select {
		case cmd := <-e.commands:
			if err := e.execute(ctx, cmd); err != nil {
				return err
			}
			continue
		default:
		}
		now := e.clock.Now()
		if job, ok := e.logicalDue(now); ok {
			if job.Blocked != nil {
				var err error
				if job, err = e.unblock(job); err != nil {
					return err
				}
			}
			if _, err := e.advance(ctx, job); isFatal(err) {
				return err
			}
			continue
		}
		job, physical := e.physicalDue(now)
		if physical && job.Blocked != nil {
			var err error
			if job, err = e.unblock(job); err != nil {
				return err
			}
		}
		if e.active != nil && (!physical || e.active.id != job.ID) {
			if err := e.park(); err != nil {
				return err
			}
		}
		if !physical {
			e.gov.reset()
			e.publishGovernor()
		}
		if physical {
			if e.gov.due(now) {
				cpu, err := e.cpu.Sample(ctx)
				if e.stopping(ctx) {
					return nil
				}
				e.gov.observe(now, cpu, err, !e.loud)
				e.loud = false
				e.publishGovernor()
			}
			switch {
			case e.gov.rate() == 0:
				if err := e.park(); err != nil {
					return err
				}
			case !now.Before(e.paceUntil):
				e.loud = e.loud || e.gov.rate() > e.tuning.FloorRate
				if err := e.slice(ctx, job); err != nil {
					return err
				}
				continue
			}
		}
		var timer <-chan time.Time
		if wake, timed := e.wake(physical); timed {
			timer = e.clock.After(wake.Sub(e.clock.Now()))
		}
		select {
		case <-ctx.Done():
			return nil
		case <-e.quit:
			return nil
		case cmd := <-e.commands:
			if err := e.execute(ctx, cmd); err != nil {
				return err
			}
		case <-timer:
		}
	}
}

func (e *Engine) wake(physical bool) (time.Time, bool) {
	var (
		wake  time.Time
		timed bool
	)
	consider := func(at time.Time) {
		if !timed || at.Before(wake) {
			wake, timed = at, true
		}
	}
	order := e.wakeOrder
	if e.paused {
		order = e.retirementOrder
	}
	for job := range e.orderedKeys(order) {
		if e.hold != nil {
			break
		}
		if e.paused && !e.retirementDue(job) {
			continue
		}
		if job.Phase == cleanup.PhaseWaiting && job.Blocked == nil {
			consider(e.checked[job.ID].Add(e.tuning.Recheck))
		}
		if e.paused && job.Blocked == nil && !job.RetireAfter.IsZero() {
			consider(job.RetireAfter)
		}
		if at, ok := e.retryAt(job); ok {
			consider(at)
		}
	}
	if physical {
		consider(e.gov.next())
		if e.gov.rate() > 0 {
			consider(e.paceUntil)
		}
	}
	return wake, timed
}

func (e *Engine) retirementDue(job cleanup.Job) bool {
	return job.Phase.Logical() && !job.Adopted && job.Retirement != cleanup.RetirementComplete
}

func (e *Engine) logicalDue(now time.Time) (cleanup.Job, bool) {
	if e.hold != nil {
		return cleanup.Job{}, false
	}
	order := e.logicalOrder
	if e.paused {
		order = e.retirementOrder
	}
	for job := range e.orderedKeys(order) {
		if !job.Phase.Logical() || (e.paused && !e.retirementDue(job)) {
			continue
		}
		if job.Blocked != nil {
			if at, ok := e.retryAt(job); ok && !now.Before(at) {
				return clone(job), true
			}
			continue
		}
		if at, ok := e.checked[job.ID]; ok && job.Phase == cleanup.PhaseWaiting && now.Before(at.Add(e.tuning.Recheck)) {
			continue
		}
		if e.paused && now.Before(job.RetireAfter) {
			continue
		}
		return clone(job), true
	}
	return cleanup.Job{}, false
}

// retryAt is when a job blocked on a transient inspection — a process census
// or watcher read that failed, a git read that timed out, or a holder that may
// leave — retries on its own: RetryAfter past the first block, doubling per
// consecutive transient block up to RetryCeiling, for RetryLimit blocks in a
// row since the daemon loaded its journal, so a restarted or upgraded daemon
// inspects again a job an earlier one gave up on. Any other blockage, and the
// one after the limit, waits for Retry.
func (e *Engine) retryAt(job cleanup.Job) (time.Time, bool) {
	if job.Blocked == nil || !transient(job.Blocked.Reason) {
		return time.Time{}, false
	}
	streak := 0
	for i := len(job.Errors) - 1; i >= 0; i-- {
		reason, _, _ := strings.Cut(job.Errors[i].Message, ": ")
		if !transient(reason) || job.Errors[i].At.Before(e.booted) {
			break
		}
		streak++
	}
	if streak > e.tuning.RetryLimit {
		return time.Time{}, false
	}
	delay := e.tuning.RetryAfter
	for range max(streak-1, 0) {
		if delay >= e.tuning.RetryCeiling {
			break
		}
		delay *= 2
	}
	return job.Blocked.At.Add(min(delay, e.tuning.RetryCeiling)), true
}

// reported is the copy of a job the daemon hands out, its blockage carrying
// when the daemon retries it on its own.
func (e *Engine) reported(job cleanup.Job) cleanup.Job {
	job = clone(job)
	if at, ok := e.retryAt(job); ok {
		job.Blocked.RetryAt = &at
	}
	return job
}

func transient(reason string) bool {
	return reason == "activity" || reason == "watchers" || reason == "timeout"
}

func (e *Engine) physicalDue(now time.Time) (cleanup.Job, bool) {
	if e.halted() {
		return cleanup.Job{}, false
	}
	var (
		next  cleanup.Job
		found bool
	)
	for job := range e.ordered() {
		if !job.Phase.Physical() {
			continue
		}
		if job.Blocked != nil {
			if at, ok := e.retryAt(job); !ok || now.Before(at) {
				continue
			}
		}
		if e.awaited(job.ID, now) {
			return clone(job), true
		}
		if !found {
			next, found = job, true
		}
	}
	return clone(next), found
}

func (e *Engine) awaited(jobID string, now time.Time) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return now.Before(e.awaits[jobID].Add(e.tuning.AwaitLease))
}

func cancelled(ctx context.Context, err error) bool {
	return ctx.Err() != nil && errors.Is(err, ctx.Err())
}

func (e *Engine) advance(ctx context.Context, job cleanup.Job) (cleanup.Job, error) {
	if _, named := cleanup.RequesterFrom(ctx); !named && job.Requester != (cleanup.ProcessID{}) {
		ctx = cleanup.WithRequester(ctx, job.Requester)
	}
	step := e.relocator.Advance
	if e.paused {
		step = e.retire
	}
	err := step(ctx, &job)
	if err != nil && (isFatal(err) || !cancelled(ctx, err)) {
		return job, &fatalError{fmt.Errorf("cleanup daemon: advance job %s: %w", job.ID, err)}
	}
	e.put(job)
	switch {
	case err != nil:
		return job, fmt.Errorf("%w: job %s rests at %s: %w", ErrStopped, job.ID, job.Phase, err)
	case job.Phase == cleanup.PhaseDone:
		delete(e.checked, job.ID)
		return job, e.finish(job)
	case job.Blocked != nil, !job.Phase.Logical(), e.paused && (job.Retirement == cleanup.RetirementComplete || !job.RetireAfter.IsZero()):
		delete(e.checked, job.ID)
	case job.Phase == cleanup.PhaseWaiting:
		e.checked[job.ID] = e.clock.Now()
	default:
		return job, &fatalError{fmt.Errorf("cleanup daemon: the relocator left job %s runnable at %s", job.ID, job.Phase)}
	}
	return job, nil
}

func (e *Engine) retire(ctx context.Context, job *cleanup.Job) error {
	if err := e.band.Foreground(); err != nil {
		return fmt.Errorf("cleanup daemon: leave the background band for retirement: %w", err)
	}
	err := e.relocator.Retire(ctx, job)
	if bandErr := e.band.Background(); bandErr != nil {
		return &fatalError{errors.Join(err, fmt.Errorf("cleanup daemon: restore the background band after retirement: %w", bandErr))}
	}
	return err
}

func (e *Engine) slice(ctx context.Context, job cleanup.Job) error {
	if e.active == nil {
		if err := e.open(ctx, &job); err != nil || e.active == nil {
			return err
		}
	}
	start := e.clock.Now()
	removed, done, err := e.active.deletion.Step(e.tuning.SliceEntries, e.tuning.SliceBudget)
	end := e.clock.Now()
	if job.Phase != cleanup.PhaseDeletingAdmin {
		job.Removed += uint64(removed) //nolint:gosec // a step never removes a negative count
	}
	e.gov.sliced(end.Sub(start))
	e.publishGovernor()
	e.paceUntil = start.Add(e.rest(removed, end.Sub(start)))
	switch {
	case err != nil:
		e.active = nil
		return e.block(job, err)
	case done:
		e.active = nil
		if job.Phase == cleanup.PhaseDeletingAdmin {
			job.Phase, job.Updated = cleanup.PhaseDeletingRetiredPayload, e.clock.Now()
			return e.save(job)
		}
		return e.finish(job)
	case end.Sub(e.active.savedAt) >= progressEvery:
		job.Updated = end
		return e.save(job)
	}
	e.active.unsaved = true
	e.put(job)
	return nil
}

func (e *Engine) rest(removed int, took time.Duration) time.Duration {
	rate := time.Duration(e.gov.rate())
	whole := time.Duration(e.tuning.SliceEntries) * time.Second / rate
	byEntries := time.Duration(removed) * time.Second / rate
	byTime := time.Duration(min(float64(took)/float64(e.tuning.SliceBudget), 1) * float64(whole))
	return max(byEntries, byTime)
}

func (e *Engine) open(ctx context.Context, job *cleanup.Job) error {
	if job.Phase == cleanup.PhaseUnregistered {
		job.Phase, job.Updated = cleanup.PhaseDeleting, e.clock.Now()
		if err := e.save(*job); err != nil {
			return err
		}
	}
	root, err := e.relocator.Admit(ctx, job)
	if err != nil {
		if cancelled(ctx, err) {
			return nil
		}
		return &fatalError{fmt.Errorf("cleanup daemon: admit job %s: %w", job.ID, err)}
	}
	e.put(*job)
	if job.Blocked != nil {
		return nil
	}
	deletion, err := e.deleter.Open(root.Dir, root.Name, root.ID)
	if errors.Is(err, fs.ErrNotExist) && job.Phase != cleanup.PhaseDeletingAdmin {
		return e.finish(*job)
	}
	if err != nil {
		return e.block(*job, err)
	}
	e.active = &activeDeletion{id: job.ID, deletion: deletion, savedAt: e.clock.Now()}
	return nil
}

func (e *Engine) block(job cleanup.Job, cause error) error {
	job.Block(e.clock.Now(), "delete", cause.Error())
	return e.save(job)
}

func (e *Engine) finish(job cleanup.Job) error {
	job.Phase, job.Updated = cleanup.PhaseDone, e.clock.Now()
	if err := e.save(job); err != nil {
		return err
	}
	e.mu.Lock()
	delete(e.awaits, job.ID)
	e.mu.Unlock()
	var finished []cleanup.Job
	for job := range e.ordered() {
		if job.Phase == cleanup.PhaseDone {
			finished = append(finished, job)
		}
	}
	slices.SortFunc(finished, byFinish)
	for _, old := range finished[:max(len(finished)-e.tuning.KeepDone, 0)] {
		if err := e.journal.Discard(old.ID); err != nil {
			return &fatalError{fmt.Errorf("cleanup daemon: prune finished job %s: %w", old.ID, err)}
		}
		e.mu.Lock()
		if e.waiters[old.ID] > 0 {
			e.kept[old.ID] = e.jobs[old.ID]
		}
		delete(e.jobs, old.ID)
		e.dequeue(old)
		e.broadcast()
		e.mu.Unlock()
	}
	return nil
}

func (e *Engine) park() error {
	if e.active == nil {
		return nil
	}
	active := e.active
	e.active = nil
	if err := active.deletion.Close(); err != nil {
		slog.Warn("cleanup daemon: close a paused deletion", "job", active.id, "error", err)
	}
	if !active.unsaved {
		return nil
	}
	job := e.jobs[active.id]
	job.Updated = e.clock.Now()
	return e.save(job)
}

func (e *Engine) remove(ctx context.Context, r cleanup.Request) (cleanup.Receipt, error) {
	job, found, err := e.revive(sittingAt(r.Worktree, r.Force))
	if err != nil {
		return cleanup.Receipt{}, err
	}
	if !found {
		accept := e.relocator.Accept
		if e.halted() {
			accept = e.relocator.Pend
		}
		e.seq++
		if job, err = e.created(accept(ctx, e.seq, r)); err != nil {
			return cleanup.Receipt{}, err
		}
	}
	job = requested(ctx, job)
	if e.halted() {
		if err := e.save(job); err != nil {
			return cleanup.Receipt{}, err
		}
		return receiptOf(job), nil
	}
	return e.relocate(ctx, job)
}

func (e *Engine) adopt(ctx context.Context, r cleanup.AdoptRequest) (cleanup.Receipt, error) {
	if e.halted() {
		return cleanup.Receipt{}, cleanup.ErrPaused
	}
	job, found, err := e.revive(parkedAt(r))
	if err != nil {
		return cleanup.Receipt{}, err
	}
	if !found {
		e.seq++
		if job, err = e.created(e.relocator.Adopt(ctx, e.seq, r)); err != nil {
			return cleanup.Receipt{}, err
		}
	}
	return e.relocate(ctx, requested(ctx, job))
}

func requested(ctx context.Context, job cleanup.Job) cleanup.Job {
	if requester, named := cleanup.RequesterFrom(ctx); named {
		job.Requester = requester
	}
	return job
}

func (e *Engine) relocate(ctx context.Context, job cleanup.Job) (cleanup.Receipt, error) {
	job, err := e.advance(ctx, job)
	switch {
	case err != nil:
		return cleanup.Receipt{}, err
	case job.Blocked != nil:
		return cleanup.Receipt{}, &cleanup.BlockedError{Job: e.reported(job)}
	case job.Phase == cleanup.PhaseWaiting:
		return cleanup.Receipt{}, &cleanup.RefusedError{
			Worktree: job.Original,
			Reason:   reasonWaiting,
			Detail:   fmt.Sprintf("deferred removal %s still waits for the tree: %s", job.ID, receiptOf(job).Detail),
		}
	}
	return receiptOf(job), nil
}

func (e *Engine) intend(ctx context.Context, r cleanup.DeferRequest) (cleanup.Receipt, error) {
	if e.hold != nil {
		return cleanup.Receipt{}, cleanup.ErrPaused
	}
	job, found, err := e.revive(boundTo(r))
	if err != nil {
		return cleanup.Receipt{}, err
	}
	if found {
		return receiptOf(job), nil
	}
	e.seq++
	if job, err = e.created(e.relocator.Intend(ctx, e.seq, r)); err != nil {
		return cleanup.Receipt{}, err
	}
	if job.Phase == cleanup.PhaseWaiting {
		e.checked[job.ID] = e.clock.Now()
	}
	return receiptOf(job), nil
}

func (e *Engine) created(job cleanup.Job, err error) (cleanup.Job, error) {
	if err != nil && e.journalFailed(err) {
		return cleanup.Job{}, &fatalError{fmt.Errorf("cleanup daemon: journal a new job: %w", err)}
	}
	if err != nil {
		return cleanup.Job{}, err
	}
	e.put(job)
	return job, nil
}

// TODO: replace the path test with errors.Is on a journal error once the
// cleanup contract types one; until then a failed operation on a path inside
// the jobs directory is the one mark a journal write failure carries.
func (e *Engine) journalFailed(err error) bool {
	var (
		path *fs.PathError
		link *os.LinkError
	)
	switch {
	case errors.As(err, &path):
		return e.records(path.Path)
	case errors.As(err, &link):
		return e.records(link.Old) || e.records(link.New)
	}
	return false
}

func (e *Engine) records(path string) bool {
	rel, err := filepath.Rel(e.layout.JobsDir(), path)
	return err == nil && filepath.IsLocal(rel)
}

type rejoinable func(job cleanup.Job) bool

func registeredAt(worktree string, force bool) rejoinable {
	resolved, _ := filepath.EvalSymlinks(worktree)
	return func(job cleanup.Job) bool {
		untouched := job.Phase == cleanup.PhaseQueued || job.Phase == cleanup.PhaseWaiting || job.Phase == cleanup.PhasePending || job.Phase == cleanup.PhasePrepared
		authorized := force || !job.Force
		return untouched && authorized && !job.Adopted && (job.Original == worktree || job.Original == resolved)
	}
}

func sittingAt(worktree string, force bool) rejoinable {
	registered := registeredAt(worktree, force)
	return func(job cleanup.Job) bool {
		if job.Blocked != nil && job.Blocked.Reason == "identity" && job.Retirement == "" {
			return false
		}
		return registered(job) && holds(job.Original, job.Tree)
	}
}

func boundTo(r cleanup.DeferRequest) rejoinable {
	registered := registeredAt(r.Worktree, r.Force)
	return func(job cleanup.Job) bool {
		return registered(job) && cleanup.Registration{Tree: job.Tree, AdminDir: job.AdminDir, Admin: job.Admin} == r.Expected
	}
}

func parkedAt(r cleanup.AdoptRequest) rejoinable {
	return func(job cleanup.Job) bool {
		same := job.Source == r.Source && job.Tree == r.Tree && job.Head == r.Head
		return job.Adopted && job.Phase == cleanup.PhasePrepared && same && holds(job.Source, job.Tree)
	}
}

func holds(path string, tree cleanup.FileID) bool {
	id, _, err := cleanup.LstatID(path)
	return err == nil && id == tree
}

func (e *Engine) revive(rejoins rejoinable) (cleanup.Job, bool, error) {
	for job := range e.ordered() {
		if !rejoins(job) {
			continue
		}
		if job.Blocked == nil {
			return clone(job), true, nil
		}
		revived, err := e.unblock(clone(job))
		return revived, true, err
	}
	return cleanup.Job{}, false, nil
}

func (e *Engine) unblock(job cleanup.Job) (cleanup.Job, error) {
	delete(e.checked, job.ID)
	job.Blocked, job.Updated = nil, e.clock.Now()
	return job, e.save(job)
}

func (e *Engine) retry(jobID string) (cleanup.Job, error) {
	job, ok := e.jobs[jobID]
	if !ok {
		return cleanup.Job{}, cleanup.ErrUnknownJob
	}
	if job.Blocked == nil {
		return clone(job), nil
	}
	return e.unblock(clone(job))
}

func (e *Engine) cancel(ctx context.Context, jobID string) (cleanup.Job, error) {
	job, ok := e.jobs[jobID]
	switch {
	case !ok:
		return cleanup.Job{}, cleanup.ErrUnknownJob
	case e.hold != nil:
		return cleanup.Job{}, &cleanup.HeldError{Hold: *e.hold}
	}
	job = clone(job)
	if err := e.relocator.Cancel(ctx, &job); err != nil {
		return cleanup.Job{}, err
	}
	if err := e.journal.Discard(job.ID); err != nil {
		return cleanup.Job{}, &fatalError{fmt.Errorf("cleanup daemon: discard cancelled job %s: %w", job.ID, err)}
	}
	delete(e.checked, job.ID)
	e.mu.Lock()
	delete(e.jobs, job.ID)
	delete(e.awaits, job.ID)
	e.dequeue(job)
	e.broadcast()
	e.mu.Unlock()
	return job, nil
}

func (e *Engine) announce(event string, pid int, version string, details ...any) {
	attrs := append([]any{"client_pid", pid, "client_version", version}, details...)
	parent, command, err := e.parent(pid)
	if err != nil {
		attrs = append(attrs, "client_parent_error", err)
	} else {
		attrs = append(attrs, "client_parent_pid", parent, "client_parent", strings.Join(command, " "))
	}
	slog.Info("cleanup daemon: "+event, attrs...)
}

func (e *Engine) halted() bool { return e.paused || e.hold != nil }

func (e *Engine) pause(paused bool) error {
	switch {
	case !paused && e.hold != nil:
		return &cleanup.HeldError{Hold: *e.hold}
	case e.paused == paused:
		return nil
	}
	return e.publish(cleanup.Switch{Paused: paused, Hold: e.hold})
}

func (e *Engine) take(r cleanup.HoldRequest) (cleanup.HoldReceipt, error) {
	switch {
	case e.hold == nil:
	case e.hold.ID == r.ID:
		return cleanup.HoldReceipt{Hold: *e.hold, Already: true}, nil
	default:
		return cleanup.HoldReceipt{}, &cleanup.HeldError{Hold: *e.hold}
	}
	hold := cleanup.Hold{ID: r.ID, Reason: r.Reason, Since: e.clock.Now()}
	if err := e.publish(cleanup.Switch{Paused: e.paused, Hold: &hold}); err != nil {
		return cleanup.HoldReceipt{}, err
	}
	return cleanup.HoldReceipt{Hold: hold}, nil
}

func (e *Engine) release(id string) error {
	switch {
	case e.hold == nil:
		return cleanup.ErrNotHeld
	case e.hold.ID != id:
		return &cleanup.HeldError{Hold: *e.hold}
	}
	return e.publish(cleanup.Switch{Paused: e.paused})
}

func (e *Engine) publish(sw cleanup.Switch) error {
	if err := e.journal.SaveSwitch(sw); err != nil {
		return &fatalError{fmt.Errorf("cleanup daemon: journal the pause switch: %w", err)}
	}
	e.mu.Lock()
	e.paused, e.hold = sw.Paused, sw.Hold
	e.broadcast()
	e.mu.Unlock()
	return e.park()
}

func (e *Engine) save(job cleanup.Job) error {
	if err := e.journal.Save(job); err != nil {
		return &fatalError{fmt.Errorf("cleanup daemon: journal job %s: %w", job.ID, err)}
	}
	if e.active != nil && e.active.id == job.ID {
		e.active.unsaved, e.active.savedAt = false, e.clock.Now()
	}
	e.put(job)
	return nil
}

func (e *Engine) put(job cleanup.Job) {
	e.mu.Lock()
	if _, known := e.jobs[job.ID]; !known {
		at, _ := slices.BinarySearchFunc(e.order, job.Seq, bySeqKey)
		e.order = slices.Insert(e.order, at, seqKey{seq: job.Seq, id: job.ID})
	}
	e.jobs[job.ID] = clone(job)
	e.schedule(job)
	e.broadcast()
	e.mu.Unlock()
}

func (e *Engine) dequeue(job cleanup.Job) {
	at, found := slices.BinarySearchFunc(e.order, job.Seq, bySeqKey)
	if !found || e.order[at].id != job.ID {
		panic(fmt.Sprintf("cleanup daemon: job %s at seq %d is missing from the queue order", job.ID, job.Seq))
	}
	e.order = slices.Delete(e.order, at, at+1)
	e.logicalOrder = scheduleKey(e.logicalOrder, e.order, job, false)
	e.retirementOrder = scheduleKey(e.retirementOrder, e.order, job, false)
	e.wakeOrder = scheduleKey(e.wakeOrder, e.order, job, false)
}

func (e *Engine) broadcast() {
	close(e.changed)
	e.changed = make(chan struct{})
}

func (e *Engine) publishGovernor() {
	report := e.gov.report()
	if report == e.governed {
		return
	}
	e.mu.Lock()
	e.governed = report
	e.broadcast()
	e.mu.Unlock()
}

func (e *Engine) scheduled(job cleanup.Job) (bool, bool, bool) {
	_, retries := e.retryAt(job)
	logical := job.Phase.Logical() && (job.Blocked == nil || retries)
	return logical, logical && e.retirementDue(job), (job.Phase == cleanup.PhaseWaiting && job.Blocked == nil) || retries
}

func (e *Engine) schedule(job cleanup.Job) {
	logical, retirement, wake := e.scheduled(job)
	e.logicalOrder = scheduleKey(e.logicalOrder, e.order, job, logical)
	e.retirementOrder = scheduleKey(e.retirementOrder, e.order, job, retirement)
	e.wakeOrder = scheduleKey(e.wakeOrder, e.order, job, wake)
}

func scheduleKey(order, all []seqKey, job cleanup.Job, eligible bool) []seqKey {
	first, _ := slices.BinarySearchFunc(order, job.Seq, bySeqKey)
	for at := first; at < len(order) && order[at].seq == job.Seq; at++ {
		if order[at].id == job.ID {
			if !eligible {
				return slices.Delete(order, at, at+1)
			}
			return order
		}
	}
	if !eligible {
		return order
	}
	at := first
	start, _ := slices.BinarySearchFunc(all, job.Seq, bySeqKey)
	for _, key := range all[start:] {
		if key.id == job.ID {
			break
		}
		if at < len(order) && order[at] == key {
			at++
		}
	}
	return slices.Insert(order, at, seqKey{seq: job.Seq, id: job.ID})
}

func (e *Engine) ordered() iter.Seq[cleanup.Job] {
	return e.orderedKeys(e.order)
}

func (e *Engine) orderedKeys(order []seqKey) iter.Seq[cleanup.Job] {
	return func(yield func(cleanup.Job) bool) {
		for _, key := range order {
			if !yield(e.jobs[key.id]) {
				return
			}
		}
	}
}

func (e *Engine) info() cleanup.Info {
	return cleanup.Info{Version: e.version, Protocol: cleanup.Protocol, PID: os.Getpid()}
}

func bySeq(a, b cleanup.Job) int { return cmp.Compare(a.Seq, b.Seq) }

type seqKey struct {
	seq uint64
	id  string
}

func bySeqKey(key seqKey, seq uint64) int { return cmp.Compare(key.seq, seq) }

func byFinish(a, b cleanup.Job) int {
	if order := a.Updated.Compare(b.Updated); order != 0 {
		return order
	}
	return bySeq(a, b)
}

func receiptOf(job cleanup.Job) cleanup.Receipt {
	receipt := cleanup.ReceiptOf(job)
	if job.Phase == cleanup.PhaseWaiting && len(job.Errors) > 0 {
		receipt.Detail = job.Errors[len(job.Errors)-1].Message
	}
	return receipt
}

func clone(job cleanup.Job) cleanup.Job {
	job.Errors = slices.Clone(job.Errors)
	job.Ignored = slices.Clone(job.Ignored)
	if job.Blocked != nil {
		blocked := *job.Blocked
		job.Blocked = &blocked
	}
	return job
}
