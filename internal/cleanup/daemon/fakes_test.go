package daemon

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/yasyf/cc-context/internal/cleanup"
)

const (
	legacyDate = "20260928"
	legacyHead = "0123456789abcdef0123456789abcdef01234567"
)

var errUnsampled = errors.New("no sampler in this test")

type recorder struct {
	mu     sync.Mutex
	events []string
	names  map[string]string
}

func (r *recorder) add(event string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, event)
}

func (r *recorder) take() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	events := r.events
	r.events = nil
	return events
}

func (r *recorder) register(id, name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.names[id] = name
}

func (r *recorder) name(id string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.names[id]
}

type manualTimer struct {
	at time.Time
	ch chan time.Time
}

type manualClock struct {
	mu     sync.Mutex
	now    time.Time
	timers []manualTimer
	armed  []time.Duration
}

func (c *manualClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *manualClock) After(d time.Duration) <-chan time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	ch := make(chan time.Time, 1)
	c.armed = append(c.armed, d)
	c.timers = append(c.timers, manualTimer{at: c.now.Add(d), ch: ch})
	c.fire()
	return ch
}

func (c *manualClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
	c.fire()
}

func (c *manualClock) fire() {
	c.timers = slices.DeleteFunc(c.timers, func(timer manualTimer) bool {
		if timer.at.After(c.now) {
			return false
		}
		timer.ch <- c.now
		return true
	})
}

func (c *manualClock) takeArmed() []time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	armed := c.armed
	c.armed = nil
	return armed
}

type fakeRelocator struct {
	h        *harness
	mu       sync.Mutex
	refuse   map[string]error
	held     map[string]string
	scripts  map[string]func(job *cleanup.Job)
	gates    map[string]chan struct{}
	entered  chan string
	served   []string
	admitted []string
}

func requesterOf(ctx context.Context) string {
	if pid, named := cleanup.RequesterFrom(ctx); named {
		return strconv.Itoa(pid)
	}
	return "none"
}

func (f *fakeRelocator) serve(ctx context.Context, step, name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.served = append(f.served, step+":"+name+"="+requesterOf(ctx))
}

func (f *fakeRelocator) takeAdmitted() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	admitted := f.admitted
	f.admitted = nil
	return admitted
}

func (f *fakeRelocator) takeServed() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	served := f.served
	f.served = nil
	return served
}

func (f *fakeRelocator) script(name string, step func(job *cleanup.Job)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.scripts[name] = step
}

func (f *fakeRelocator) Accept(ctx context.Context, seq uint64, r cleanup.Request) (cleanup.Job, error) {
	name := filepath.Base(r.Worktree)
	f.h.rec.add("accept:" + name)
	f.serve(ctx, "accept", name)
	f.mu.Lock()
	refusal := f.refuse[name]
	f.mu.Unlock()
	if refusal != nil {
		return cleanup.Job{}, refusal
	}
	job := f.h.newJob(name, seq, cleanup.PhasePrepared)
	job.Force = r.Force
	return job, f.h.journal.Create(job)
}

func (f *fakeRelocator) Intend(ctx context.Context, seq uint64, r cleanup.DeferRequest) (cleanup.Job, error) {
	name := filepath.Base(r.Worktree)
	f.h.rec.add("intend:" + name)
	f.serve(ctx, "intend", name)
	f.mu.Lock()
	refusal, holders := f.refuse[name], f.held[name]
	f.mu.Unlock()
	if refusal != nil {
		return cleanup.Job{}, refusal
	}
	job := f.h.newJob(name, seq, cleanup.PhaseQueued)
	job.Owner, job.Force = r.Owner, r.Force
	if holders != "" {
		job.Phase = cleanup.PhaseWaiting
		job.Note(f.h.clock.Now(), holders)
	}
	return job, f.h.journal.Create(job)
}

func (f *fakeRelocator) Adopt(ctx context.Context, seq uint64, r cleanup.AdoptRequest) (cleanup.Job, error) {
	name := parkedName(r.Source)
	f.h.rec.add("adopt:" + name)
	f.serve(ctx, "adopt", name)
	f.mu.Lock()
	refusal := f.refuse[name]
	f.mu.Unlock()
	if refusal != nil {
		return cleanup.Job{}, refusal
	}
	now := f.h.clock.Now()
	id := f.h.mint(name, seq)
	job := cleanup.Job{
		Schema:      cleanup.Schema,
		ID:          id,
		Seq:         seq,
		Phase:       cleanup.PhasePrepared,
		Adopted:     true,
		Source:      r.Source,
		Owner:       r.Owner,
		Repo:        r.CommonDir,
		Original:    r.Original,
		Registered:  f.h.layout.Registered(id),
		Payload:     f.h.layout.Payload(id),
		Tree:        r.Tree,
		Head:        r.Head,
		RecoveryRef: r.RecoveryRef,
		Git:         r.Git,
		Created:     now,
		Updated:     now,
	}
	return job, f.h.journal.Create(job)
}

func (f *fakeRelocator) Advance(ctx context.Context, job *cleanup.Job) error {
	name := f.h.rec.name(job.ID)
	f.h.rec.add(fmt.Sprintf("advance:%s@%s", name, job.Phase))
	f.serve(ctx, "advance", name)
	f.mu.Lock()
	step, gate := f.scripts[name], f.gates[name]
	f.mu.Unlock()
	if gate != nil {
		f.entered <- name
		<-gate
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("check activity in %s: %w", name, err)
	}
	if step == nil {
		step = func(job *cleanup.Job) { job.Phase = cleanup.PhaseUnregistered }
	}
	step(job)
	job.Updated = f.h.clock.Now()
	return f.h.journal.Save(*job)
}

func (f *fakeRelocator) Admit(ctx context.Context, job *cleanup.Job) error {
	name := f.h.rec.name(job.ID)
	f.h.rec.add("admit:" + name)
	f.mu.Lock()
	f.admitted = append(f.admitted, name+"="+requesterOf(ctx))
	verdict, gate := f.scripts["admit:"+name], f.gates["admit:"+name]
	f.mu.Unlock()
	if gate != nil {
		f.entered <- "admit:" + name
		<-gate
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("check activity in the payload of %s: %w", name, err)
	}
	if verdict == nil {
		return nil
	}
	verdict(job)
	job.Updated = f.h.clock.Now()
	return f.h.journal.Save(*job)
}

func blockWith(clock *manualClock, reason, detail string) func(job *cleanup.Job) {
	return func(job *cleanup.Job) { job.Block(clock.Now(), reason, detail) }
}

func stayWaiting(job *cleanup.Job) { job.Phase = cleanup.PhaseWaiting }

type payload struct {
	entries  int
	perStep  int
	stepTime time.Duration
	openErr  error
	stepErr  error
	gate     chan struct{}
	entered  chan struct{}
	wants    []cleanup.FileID
}

type fakeDeleter struct {
	h        *harness
	mu       sync.Mutex
	payloads map[string]*payload
}

func (f *fakeDeleter) put(name string, p *payload) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.payloads[name] = p
}

func (f *fakeDeleter) wants(name string) []cleanup.FileID {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.payloads[name].wants)
}

func (f *fakeDeleter) Open(dir, name string, want cleanup.FileID) (cleanup.Deletion, error) {
	job := f.h.rec.name(filepath.Base(dir))
	f.h.rec.add("open:" + job)
	f.mu.Lock()
	defer f.mu.Unlock()
	p := f.payloads[job]
	if name != cleanup.PayloadName || p == nil || p.entries == 0 {
		return nil, fmt.Errorf("open %s: %w", filepath.Join(dir, name), fs.ErrNotExist)
	}
	p.wants = append(p.wants, want)
	if p.openErr != nil {
		return nil, p.openErr
	}
	return &fakeDeletion{deleter: f, job: job, payload: p}, nil
}

type fakeDeletion struct {
	deleter *fakeDeleter
	job     string
	payload *payload
}

func (d *fakeDeletion) Step(limit int, _ time.Duration) (int, bool, error) {
	d.deleter.h.rec.add("step:" + d.job)
	d.deleter.mu.Lock()
	gate := d.payload.gate
	d.deleter.mu.Unlock()
	if gate != nil {
		d.payload.entered <- struct{}{}
		<-gate
	}
	d.deleter.mu.Lock()
	defer d.deleter.mu.Unlock()
	d.deleter.h.clock.Advance(d.payload.stepTime)
	if d.payload.stepErr != nil {
		return 0, false, d.payload.stepErr
	}
	if d.payload.perStep > 0 {
		limit = min(limit, d.payload.perStep)
	}
	removed := min(limit, d.payload.entries)
	d.payload.entries -= removed
	return removed, d.payload.entries == 0, nil
}

func (d *fakeDeletion) Close() error {
	d.deleter.h.rec.add("close:" + d.job)
	return nil
}

type fakeCPU struct {
	h       *harness
	mu      sync.Mutex
	cpu     time.Duration
	err     error
	gate    chan struct{}
	entered chan struct{}
}

func (f *fakeCPU) hold() chan struct{} {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gate = make(chan struct{})
	return f.gate
}

func (f *fakeCPU) set(cpu time.Duration, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cpu, f.err = cpu, err
}

func (f *fakeCPU) burn(cpu time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cpu += cpu
}

func (f *fakeCPU) Sample(ctx context.Context) (time.Duration, error) {
	f.h.rec.add("sample")
	f.mu.Lock()
	gate := f.gate
	f.mu.Unlock()
	if gate != nil {
		f.entered <- struct{}{}
		<-gate
	}
	if err := ctx.Err(); err != nil {
		return 0, fmt.Errorf("find fseventsd: %w", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.cpu, f.err
}

type harness struct {
	t         *testing.T
	root      string
	layout    cleanup.Layout
	journal   *cleanup.Journal
	tuning    Tuning
	clock     *manualClock
	rec       *recorder
	relocator *fakeRelocator
	deleter   *fakeDeleter
	cpu       *fakeCPU
	parent    *fakeParent
	engine    *Engine
	running   chan error
}

func newHarness(t *testing.T, tuning Tuning) *harness {
	t.Helper()
	return newHarnessAt(t, t.TempDir(), tuning)
}

func newHarnessAt(t *testing.T, root string, tuning Tuning) *harness {
	t.Helper()
	layout := cleanup.Layout{Root: filepath.Join(root, "s")}
	journal, err := cleanup.OpenJournal(layout)
	if err != nil {
		t.Fatalf("OpenJournal() = %v", err)
	}
	h := &harness{
		t:       t,
		root:    root,
		layout:  layout,
		journal: journal,
		tuning:  tuning,
		clock:   &manualClock{now: time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)},
		rec:     &recorder{names: make(map[string]string)},
	}
	h.relocator = &fakeRelocator{
		h:       h,
		refuse:  make(map[string]error),
		held:    make(map[string]string),
		scripts: make(map[string]func(job *cleanup.Job)),
		gates:   make(map[string]chan struct{}),
		entered: make(chan string, 16),
	}
	h.deleter = &fakeDeleter{h: h, payloads: make(map[string]*payload)}
	h.cpu = &fakeCPU{h: h, err: errUnsampled, entered: make(chan struct{}, 16)}
	h.parent = &fakeParent{}
	return h
}

type fakeParent struct {
	mu    sync.Mutex
	asked []int
}

func (p *fakeParent) lookup(pid int) (int, []string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.asked = append(p.asked, pid)
	return 1, []string{"zsh", "-c", "ccx vcs cleanup pause"}, nil
}

func (p *fakeParent) takeAsked() []int {
	p.mu.Lock()
	defer p.mu.Unlock()
	asked := p.asked
	p.asked = nil
	return asked
}

func (h *harness) tree(name string) string {
	h.t.Helper()
	path := filepath.Join(h.root, "trees", name)
	if err := os.MkdirAll(path, 0o700); err != nil {
		h.t.Fatalf("create tree %s: %v", name, err)
	}
	return path
}

func (h *harness) registration(name string) cleanup.Registration {
	h.t.Helper()
	original := h.tree(name)
	tree, _, err := cleanup.LstatID(original)
	if err != nil {
		h.t.Fatalf("LstatID(%s) = %v", original, err)
	}
	return cleanup.Registration{
		Tree:     tree,
		AdminDir: filepath.Join(h.root, "repo.git", "worktrees", name),
		Admin:    cleanup.FileID{Dev: 1, Ino: 1},
	}
}

func (h *harness) replace(name string) {
	h.t.Helper()
	original := h.registration(name)
	if err := os.Rename(h.tree(name), filepath.Join(h.root, "trees", name+".kept")); err != nil {
		h.t.Fatalf("move tree %s aside: %v", name, err)
	}
	if replacement := h.registration(name); replacement.Tree == original.Tree {
		h.t.Fatalf("the replacement of tree %s kept identity %v", name, original.Tree)
	}
}

func (h *harness) newJob(name string, seq uint64, phase cleanup.Phase) cleanup.Job {
	h.t.Helper()
	original := h.tree(name)
	registered := h.registration(name)
	now := h.clock.Now()
	id := h.mint(name, seq)
	return cleanup.Job{
		Schema:     cleanup.Schema,
		ID:         id,
		Seq:        seq,
		Phase:      phase,
		Deferred:   phase == cleanup.PhaseQueued || phase == cleanup.PhaseWaiting,
		Repo:       filepath.Join(h.root, "repo.git"),
		AdminDir:   registered.AdminDir,
		Admin:      registered.Admin,
		Original:   original,
		Registered: h.layout.Registered(id),
		Payload:    h.layout.Payload(id),
		Tree:       registered.Tree,
		Git:        "/usr/bin/git",
		Links:      cleanup.Links{DotGit: "gitdir: " + registered.AdminDir + "\n", AdminGitdir: original + "/.git\n"},
		Created:    now,
		Updated:    now,
	}
}

func (h *harness) mint(name string, seq uint64) string {
	id := fmt.Sprintf("%016x-%06x", h.clock.Now().UnixNano(), seq)
	h.rec.register(id, name)
	return id
}

func legacyID(name string) string {
	hexed := hex.EncodeToString([]byte(name))
	return strings.Repeat("0", 20-len(hexed)) + hexed
}

func parkedName(source string) string {
	return filepath.Base(source)[len(legacyID(""))+1:]
}

func (h *harness) parked(name string) string {
	h.t.Helper()
	path := filepath.Join(h.root, cleanup.LegacyQuarantinePrefix+legacyDate, legacyID(name)+"-"+name)
	if err := os.MkdirAll(path, 0o700); err != nil {
		h.t.Fatalf("park tree %s: %v", name, err)
	}
	return path
}

func (h *harness) adoption(name string) cleanup.AdoptRequest {
	h.t.Helper()
	source := h.parked(name)
	tree, _, err := cleanup.LstatID(source)
	if err != nil {
		h.t.Fatalf("LstatID(%s) = %v", source, err)
	}
	return cleanup.AdoptRequest{
		Source:      source,
		Tree:        tree,
		CommonDir:   filepath.Join(h.root, "repo.git"),
		Head:        legacyHead,
		RecoveryRef: cleanup.LegacyRecoveryPrefix + legacyDate + "/" + legacyID(name),
		Original:    filepath.Join(h.root, "trees", name),
		Owner:       "legacy quarantine import",
		Git:         "/usr/bin/git",
	}
}

func (h *harness) seed(name string, seq uint64, phase cleanup.Phase, adjust ...func(job *cleanup.Job)) cleanup.Job {
	h.t.Helper()
	job := h.newJob(name, seq, phase)
	for _, fn := range adjust {
		fn(&job)
	}
	if err := h.journal.Create(job); err != nil {
		h.t.Fatalf("Journal.Create(%s) = %v", name, err)
	}
	return job
}

func (h *harness) request(name string) cleanup.Request {
	return cleanup.Request{Worktree: h.tree(name), Git: "/usr/bin/git"}
}

func (h *harness) deferral(name string) cleanup.DeferRequest {
	h.t.Helper()
	return cleanup.DeferRequest{
		Worktree:  h.tree(name),
		CommonDir: filepath.Join(h.root, "repo.git"),
		Owner:     "stack",
		Expected:  h.registration(name),
		Git:       "/usr/bin/git",
	}
}

func (h *harness) build() *Engine {
	h.t.Helper()
	engine, err := New(Config{
		Journal:   h.journal,
		Relocator: h.relocator,
		Deleter:   h.deleter,
		CPU:       h.cpu,
		Parent:    h.parent.lookup,
		Version:   "v1.2.3",
		Clock:     h.clock,
		Tuning:    h.tuning,
	})
	if err != nil {
		h.t.Fatalf("New() = %v", err)
	}
	h.engine = engine
	return engine
}

func (h *harness) start() {
	h.t.Helper()
	engine := h.build()
	running := make(chan error, 1)
	h.running = running
	go func() { running <- engine.Run(context.Background()) }()
	h.t.Cleanup(func() {
		if h.engine == engine {
			h.stop()
		}
	})
}

func (h *harness) stop() {
	h.t.Helper()
	if err := h.engine.Stop(context.Background()); err != nil {
		h.t.Errorf("Stop() = %v", err)
	}
	if err := <-h.running; err != nil {
		h.t.Errorf("Run() = %v", err)
	}
	h.engine = nil
}

func (h *harness) expectEvents(want ...string) {
	h.t.Helper()
	synctest.Wait()
	if got := h.rec.take(); !slices.Equal(got, want) {
		h.t.Fatalf("events = %q, want %q", got, want)
	}
}

func (h *harness) expectTimers(want ...time.Duration) {
	h.t.Helper()
	synctest.Wait()
	if got := h.clock.takeArmed(); !slices.Equal(got, want) {
		h.t.Fatalf("armed timers = %v, want %v", got, want)
	}
}

func (h *harness) status(jobID string) cleanup.Job {
	h.t.Helper()
	report, err := h.engine.Status(context.Background(), cleanup.Query{JobID: jobID})
	if err != nil {
		h.t.Fatalf("Status(%s) = %v", jobID, err)
	}
	return report.Jobs[0]
}

func (h *harness) journaled(jobID string) cleanup.Job {
	h.t.Helper()
	jobs, damaged, err := h.journal.Load()
	if err != nil || len(damaged) != 0 {
		h.t.Fatalf("Journal.Load() = damaged %v, err %v", damaged, err)
	}
	for _, job := range jobs {
		if job.ID == jobID {
			return job
		}
	}
	h.t.Fatalf("the journal holds no job %s", jobID)
	return cleanup.Job{}
}

func (h *harness) names(jobs []cleanup.Job) []string {
	names := make([]string, 0, len(jobs))
	for _, job := range jobs {
		names = append(names, h.rec.name(job.ID))
	}
	return names
}
