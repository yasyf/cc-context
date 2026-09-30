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
	"log/slog"
	"os"
	"path/filepath"
	"slices"
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
	// Rate is the payload entries deleted per second.
	Rate int
	// SliceEntries and SliceBudget bound one uninterruptible step.
	SliceEntries int
	SliceBudget  time.Duration
	// SampleEvery is the interval between fseventsd CPU samples while deletion
	// is pending.
	SampleEvery time.Duration
	// PauseAbove and ResumeBelow are percentages of one core.
	PauseAbove  float64
	ResumeBelow float64
	// ResumeSamples is how many consecutive samples under ResumeBelow lift a
	// throttle.
	ResumeSamples int
	// Recheck is the interval between activity checks of a waiting job.
	Recheck time.Duration
	// KeepDone is how many finished records the journal retains.
	KeepDone int
	// StatusLimit is the default cap on the jobs one report carries.
	StatusLimit int
}

// DefaultTuning is the production pace: 250 entries a second in slices of at
// most 100 entries or 50ms, throttled while fseventsd runs above half a core.
func DefaultTuning() Tuning {
	return Tuning{
		Rate:          250,
		SliceEntries:  100,
		SliceBudget:   50 * time.Millisecond,
		SampleEvery:   5 * time.Second,
		PauseAbove:    50,
		ResumeBelow:   25,
		ResumeSamples: 3,
		Recheck:       30 * time.Second,
		KeepDone:      100,
		StatusLimit:   50,
	}
}

func (t Tuning) validate() error {
	switch {
	case t.Rate <= 0, t.SliceEntries <= 0, t.SliceBudget <= 0:
		return fmt.Errorf("rate %d, slice of %d entries or %s: all must be positive", t.Rate, t.SliceEntries, t.SliceBudget)
	case t.SampleEvery <= 0, t.Recheck <= 0:
		return fmt.Errorf("sample interval %s and recheck interval %s must be positive", t.SampleEvery, t.Recheck)
	case t.ResumeBelow <= 0, t.PauseAbove < t.ResumeBelow, t.ResumeSamples <= 0:
		return fmt.Errorf("throttle pauses above %v and resumes after %d samples below %v", t.PauseAbove, t.ResumeSamples, t.ResumeBelow)
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
	Version   string
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
	kept     map[string]cleanup.Job
	damaged  []cleanup.Damaged
	paused   bool
	governed cleanup.Governor
	changed  chan struct{}

	seq       uint64
	gov       *governor
	checked   map[string]time.Time
	active    *activeDeletion
	paceUntil time.Time
}

var (
	_ cleanup.Service = (*Engine)(nil)
	_ cleanup.Adopter = (*Engine)(nil)
)

// New builds an Engine over cfg's journal and reads nothing from it: Run loads
// the records, so a daemon holds its serve lock before anything reads or
// repairs them.
func New(cfg Config) (*Engine, error) {
	if cfg.Journal == nil || cfg.Relocator == nil || cfg.Deleter == nil || cfg.CPU == nil {
		return nil, errors.New("cleanup daemon: config needs a journal, a relocator, a deleter, and a cpu sampler")
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
		version:   cfg.Version,
		clock:     clock,
		tuning:    tuning,
		commands:  make(chan command),
		quit:      make(chan struct{}),
		loaded:    make(chan struct{}),
		done:      make(chan struct{}),
		jobs:      make(map[string]cleanup.Job),
		waiters:   make(map[string]int),
		kept:      make(map[string]cleanup.Job),
		governed:  gov.report(),
		changed:   make(chan struct{}),
		gov:       gov,
		checked:   make(map[string]time.Time),
	}, nil
}

// Run is the single worker. It loads the journal, resumes every unfinished job
// from the phase its record holds, and returns nil once ctx is done or Stop is
// called, after journaling the active deletion's progress. It returns an error
// only when the journal could not be read or written: it never keeps mutating
// what it cannot record.
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
	return errors.Join(e.work(ctx), e.park())
}

// Stop lets the worker finish the one slice or ladder step it is in, flush,
// and return. A command the worker has not started by then is answered with
// ErrStopped. It never cancels the context that step runs under; only Run's
// own context does. It is idempotent, and stops an engine whose Run has not
// started yet: every call on that engine is answered with ErrStopped whether
// or not Run ever starts.
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
// but the requester it names. A request for a tree whose deferred removal
// still waits on its holders is a *cleanup.RefusedError naming that job, and
// a request without Force never rejoins a job journaled with it: it is
// preflighted afresh.
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
// source rejoins the job the first one journaled.
func (e *Engine) Adopt(ctx context.Context, r cleanup.AdoptRequest) (cleanup.Receipt, error) {
	if err := r.Validate(); err != nil {
		return cleanup.Receipt{}, fmt.Errorf("cleanup daemon: adopt request: %w", err)
	}
	return call(ctx, e, onBehalf(ctx, func(ctx context.Context) (cleanup.Receipt, error) { return e.adopt(ctx, r) }))
}

// Status reads a snapshot and never waits behind the worker once Run has
// loaded the journal: unfinished jobs in queue order, then finished jobs
// newest first, capped at the limit.
func (e *Engine) Status(ctx context.Context, q cleanup.Query) (cleanup.Report, error) {
	if err := e.ready(ctx); err != nil {
		return cleanup.Report{}, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	report := cleanup.Report{
		Version:  e.version,
		PID:      os.Getpid(),
		Paused:   e.paused,
		Governor: e.governed,
		Jobs:     []cleanup.Job{},
		Damaged:  slices.Clone(e.damaged),
	}
	if q.JobID != "" {
		job, ok := e.jobs[q.JobID]
		if !ok {
			return cleanup.Report{}, cleanup.ErrUnknownJob
		}
		report.Jobs = append(report.Jobs, clone(job))
		return report, nil
	}
	var unfinished, finished []cleanup.Job
	for _, job := range e.jobs {
		if job.Phase == cleanup.PhaseDone {
			finished = append(finished, clone(job))
			continue
		}
		unfinished = append(unfinished, clone(job))
	}
	slices.SortFunc(unfinished, bySeq)
	slices.SortFunc(finished, func(a, b cleanup.Job) int { return byFinish(b, a) })
	report.Jobs = append(append(report.Jobs, unfinished...), finished...)
	limit := q.Limit
	if limit <= 0 {
		limit = e.tuning.StatusLimit
	}
	if over := len(report.Jobs) - limit; over > 0 {
		report.Jobs, report.Omitted = report.Jobs[:limit], over
	}
	return report, nil
}

// Wait returns the job once it is done, and the job with a *BlockedError once
// it blocks. A queue-wide pause keeps the caller waiting. A caller already
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
			job = e.kept[jobID]
		}
		job, changed := clone(job), e.changed
		e.mu.Unlock()
		switch {
		case job.Phase == cleanup.PhaseDone:
			return job, nil
		case job.Blocked != nil:
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

// Pause stops physical deletion queue-wide. The deletion it interrupts is
// closed and its progress journaled before Pause returns, so no entry goes
// after it until a Resume has the payload admitted afresh. Logical removals
// keep running.
func (e *Engine) Pause(ctx context.Context) error {
	_, err := call(ctx, e, func(context.Context) (struct{}, error) { return struct{}{}, e.pause(true) })
	return err
}

// Resume lets physical deletion continue. The payload is admitted and
// reopened, and so re-verified, before another entry goes.
func (e *Engine) Resume(ctx context.Context) error {
	_, err := call(ctx, e, func(context.Context) (struct{}, error) { return struct{}{}, e.pause(false) })
	return err
}

// Retry clears a job's blockage and returns the job as journaled; the worker's
// next pass resumes it from the phase it stopped in.
func (e *Engine) Retry(ctx context.Context, jobID string) (cleanup.Job, error) {
	return call(ctx, e, func(context.Context) (cleanup.Job, error) { return e.retry(jobID) })
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
		e.seq = max(e.seq, job.Seq)
	}
	e.damaged, e.paused = damaged, sw.Paused
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
	return cmd.run(ctx)
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
			if _, err := e.advance(ctx, job); isFatal(err) {
				return err
			}
			continue
		}
		job, physical := e.physicalDue()
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
				e.gov.observe(now, cpu, err)
				e.publishGovernor()
			}
			if !e.gov.allows() {
				if err := e.park(); err != nil {
					return err
				}
			} else if !now.Before(e.paceUntil) {
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
	for _, job := range e.ordered() {
		if job.Phase == cleanup.PhaseWaiting && job.Blocked == nil {
			consider(e.checked[job.ID].Add(e.tuning.Recheck))
		}
	}
	if physical {
		consider(e.gov.next())
		if e.gov.allows() {
			consider(e.paceUntil)
		}
	}
	return wake, timed
}

func (e *Engine) logicalDue(now time.Time) (cleanup.Job, bool) {
	for _, job := range e.ordered() {
		if !job.Phase.Logical() || job.Blocked != nil {
			continue
		}
		if at, ok := e.checked[job.ID]; ok && job.Phase == cleanup.PhaseWaiting && now.Before(at.Add(e.tuning.Recheck)) {
			continue
		}
		return job, true
	}
	return cleanup.Job{}, false
}

func (e *Engine) physicalDue() (cleanup.Job, bool) {
	if e.paused {
		return cleanup.Job{}, false
	}
	for _, job := range e.ordered() {
		if job.Phase.Physical() && job.Blocked == nil {
			return job, true
		}
	}
	return cleanup.Job{}, false
}

func cancelled(ctx context.Context, err error) bool {
	return ctx.Err() != nil && errors.Is(err, ctx.Err())
}

func (e *Engine) advance(ctx context.Context, job cleanup.Job) (cleanup.Job, error) {
	err := e.relocator.Advance(ctx, &job)
	if err != nil && !cancelled(ctx, err) {
		return job, &fatalError{fmt.Errorf("cleanup daemon: advance job %s: %w", job.ID, err)}
	}
	e.put(job)
	switch {
	case err != nil:
		return job, fmt.Errorf("%w: job %s rests at %s: %w", ErrStopped, job.ID, job.Phase, err)
	case job.Blocked != nil, !job.Phase.Logical():
		delete(e.checked, job.ID)
	case job.Phase == cleanup.PhaseWaiting:
		e.checked[job.ID] = e.clock.Now()
	default:
		return job, &fatalError{fmt.Errorf("cleanup daemon: the relocator left job %s runnable at %s", job.ID, job.Phase)}
	}
	return job, nil
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
	job.Removed += uint64(removed) //nolint:gosec // a step never removes a negative count
	e.paceUntil = start.Add(time.Duration(removed) * time.Second / time.Duration(e.tuning.Rate))
	switch {
	case err != nil:
		e.active = nil
		return e.block(job, err)
	case done:
		e.active = nil
		return e.finish(job)
	case end.Sub(e.active.savedAt) >= progressEvery:
		job.Updated = end
		return e.save(job)
	}
	e.active.unsaved = true
	e.put(job)
	return nil
}

func (e *Engine) open(ctx context.Context, job *cleanup.Job) error {
	if job.Phase == cleanup.PhaseUnregistered {
		job.Phase, job.Updated = cleanup.PhaseDeleting, e.clock.Now()
		if err := e.save(*job); err != nil {
			return err
		}
	}
	if err := e.relocator.Admit(ctx, job); err != nil {
		if cancelled(ctx, err) {
			return nil
		}
		return &fatalError{fmt.Errorf("cleanup daemon: admit job %s: %w", job.ID, err)}
	}
	e.put(*job)
	if job.Blocked != nil {
		return nil
	}
	deletion, err := e.deleter.Open(e.layout.JobDir(job.ID), cleanup.PayloadName, job.Tree)
	if errors.Is(err, fs.ErrNotExist) {
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
	finished := slices.DeleteFunc(e.ordered(), func(job cleanup.Job) bool { return job.Phase != cleanup.PhaseDone })
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
		e.seq++
		if job, err = e.created(e.relocator.Accept(ctx, e.seq, r)); err != nil {
			return cleanup.Receipt{}, err
		}
	}
	return e.relocate(ctx, job)
}

func (e *Engine) adopt(ctx context.Context, r cleanup.AdoptRequest) (cleanup.Receipt, error) {
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
	return e.relocate(ctx, job)
}

func (e *Engine) relocate(ctx context.Context, job cleanup.Job) (cleanup.Receipt, error) {
	job, err := e.advance(ctx, job)
	switch {
	case err != nil:
		return cleanup.Receipt{}, err
	case job.Blocked != nil:
		return cleanup.Receipt{}, &cleanup.BlockedError{Job: job}
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
		untouched := job.Phase == cleanup.PhaseQueued || job.Phase == cleanup.PhaseWaiting || job.Phase == cleanup.PhasePrepared
		authorized := force || !job.Force
		return untouched && authorized && !job.Adopted && (job.Original == worktree || job.Original == resolved)
	}
}

func sittingAt(worktree string, force bool) rejoinable {
	registered := registeredAt(worktree, force)
	return func(job cleanup.Job) bool { return registered(job) && holds(job.Original, job.Tree) }
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
	for _, job := range e.ordered() {
		if !rejoins(job) {
			continue
		}
		if job.Blocked == nil {
			return job, true, nil
		}
		revived, err := e.unblock(job)
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

func (e *Engine) pause(paused bool) error {
	if e.paused == paused {
		return nil
	}
	if err := e.journal.SaveSwitch(cleanup.Switch{Paused: paused}); err != nil {
		return &fatalError{fmt.Errorf("cleanup daemon: journal the pause switch: %w", err)}
	}
	e.mu.Lock()
	e.paused = paused
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
	e.jobs[job.ID] = clone(job)
	e.broadcast()
	e.mu.Unlock()
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

func (e *Engine) ordered() []cleanup.Job {
	jobs := make([]cleanup.Job, 0, len(e.jobs))
	for _, job := range e.jobs {
		jobs = append(jobs, clone(job))
	}
	slices.SortFunc(jobs, bySeq)
	return jobs
}

func (e *Engine) info() cleanup.Info {
	return cleanup.Info{Version: e.version, Protocol: cleanup.Protocol, PID: os.Getpid()}
}

func bySeq(a, b cleanup.Job) int { return cmp.Compare(a.Seq, b.Seq) }

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
	if job.Blocked != nil {
		blocked := *job.Blocked
		job.Blocked = &blocked
	}
	return job
}
