package daemon

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/yasyf/daemonkit/durable"

	"github.com/yasyf/cc-context/internal/cleanup"
	"github.com/yasyf/cc-context/internal/version"
)

const (
	unknownJob = "0000000000000000-000000"
	unrecorded = "00000000000000ff-abcdef"
)

type daemonFixture struct {
	t      *testing.T
	h      *harness
	socket string
	client *Client
	cancel context.CancelFunc
	served chan error
	once   sync.Once
	result error
}

func serveFixture(t *testing.T, seed func(h *harness)) *daemonFixture {
	t.Helper()
	root, err := os.MkdirTemp("/tmp", "ccxc")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(root); err != nil {
			t.Errorf("remove %s: %v", root, err)
		}
	})
	h := newHarnessAt(t, root, DefaultTuning())
	if seed != nil {
		seed(h)
	}
	engine := h.build()
	ctx, cancel := context.WithCancel(context.Background())
	s, err := listen(ctx, engine, h.layout)
	if err != nil {
		cancel()
		t.Fatalf("listen() = %v", err)
	}
	socket, err := h.layout.Socket()
	if err != nil {
		t.Fatal(err)
	}
	f := &daemonFixture{t: t, h: h, socket: socket, client: Dial(socket), cancel: cancel, served: make(chan error, 1)}
	go func() { f.served <- s.serve(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := f.wait(); err != nil {
			t.Errorf("Serve() = %v", err)
		}
	})
	return f
}

func (f *daemonFixture) wait() error {
	f.once.Do(func() { f.result = <-f.served })
	return f.result
}

func (f *daemonFixture) raw(line string) response {
	f.t.Helper()
	conn, err := net.Dial("unix", f.socket)
	if err != nil {
		f.t.Fatalf("dial %s: %v", f.socket, err)
	}
	defer func() { _ = conn.Close() }()
	reply, err := exchange(conn, []byte(line+"\n"))
	if err != nil {
		f.t.Fatalf("exchange %q: %v", line, err)
	}
	decoded, err := durable.Unmarshal[response](reply)
	if err != nil {
		f.t.Fatalf("decode reply %q: %v", reply, err)
	}
	return decoded
}

func queuePaused(h *harness) {
	if err := h.journal.SaveSwitch(cleanup.Switch{Paused: true}); err != nil {
		h.t.Fatal(err)
	}
}

func TestServeRoundTrip(t *testing.T) {
	ctx := context.Background()
	f := serveFixture(t, func(h *harness) {
		h.relocator.held["d"] = "zsh (pid 7) in the tree"
		h.relocator.script("d", stayWaiting)
	})
	inProcess := func(jobID string) cleanup.Report {
		t.Helper()
		report, err := f.h.engine.Status(ctx, cleanup.Query{JobID: jobID})
		if err != nil {
			t.Fatalf("in-process Status(%q) = %v", jobID, err)
		}
		return report
	}

	info, err := f.client.Hello(ctx)
	if want := (cleanup.Info{Version: "v1.2.3", Protocol: cleanup.Protocol, PID: os.Getpid()}); err != nil || info != want {
		t.Fatalf("Hello() = %+v, %v; want %+v", info, err, want)
	}

	deferred, err := f.client.Defer(ctx, f.h.deferral("d"))
	if err != nil {
		t.Fatalf("Defer() = %v", err)
	}
	if want := (cleanup.Receipt{JobID: deferred.JobID, State: cleanup.State(cleanup.PhaseWaiting), Original: f.h.tree("d"), Detail: "zsh (pid 7) in the tree"}); deferred != want {
		t.Errorf("Defer() = %+v, want %+v", deferred, want)
	}

	sampling := f.h.cpu.hold()
	removed, err := f.client.Remove(ctx, f.h.request("a"))
	if err != nil {
		t.Fatalf("Remove() = %v", err)
	}
	if want := (cleanup.Receipt{JobID: removed.JobID, State: cleanup.State(cleanup.PhaseUnregistered), Original: f.h.tree("a")}); removed != want || f.h.rec.name(removed.JobID) != "a" {
		t.Errorf("Remove() = %+v, want %+v", removed, want)
	}
	<-f.h.cpu.entered

	report, err := f.client.Status(ctx, cleanup.Query{})
	if err != nil {
		t.Fatalf("Status() = %v", err)
	}
	if want := inProcess(""); !reflect.DeepEqual(report, want) {
		t.Errorf("Status() over the socket = %+v, want the in-process report %+v", report, want)
	}
	if got, want := f.h.names(report.Jobs), []string{"d", "a"}; !slices.Equal(got, want) || report.Paused {
		t.Errorf("Status() = jobs %v, paused %t; want %v, false", got, report.Paused, want)
	}
	limited, err := f.client.Status(ctx, cleanup.Query{Limit: 1})
	if err != nil || len(limited.Jobs) != 1 || limited.Omitted != 1 {
		t.Errorf("Status(limit 1) = %d jobs, %d omitted, %v; want 1, 1", len(limited.Jobs), limited.Omitted, err)
	}
	f.h.cpu.mu.Lock()
	f.h.cpu.gate = nil
	f.h.cpu.mu.Unlock()
	sampling <- struct{}{}

	retried, err := f.client.Retry(ctx, deferred.JobID)
	if err != nil {
		t.Fatalf("Retry(d) = %v", err)
	}
	if want := inProcess(deferred.JobID).Jobs[0]; !reflect.DeepEqual(retried, want) || retried.Phase != cleanup.PhaseWaiting {
		t.Errorf("Retry(d) = %+v, want the waiting in-process job %+v", retried, want)
	}

	request := f.h.adoption("q")
	adopted, err := f.client.Adopt(ctx, request)
	if err != nil {
		t.Fatalf("Adopt() = %v", err)
	}
	if want := (cleanup.Receipt{JobID: adopted.JobID, State: cleanup.State(cleanup.PhaseUnregistered), Original: request.Original, RecoveryRef: request.RecoveryRef}); adopted != want || f.h.rec.name(adopted.JobID) != "q" {
		t.Errorf("Adopt() = %+v, want %+v", adopted, want)
	}
	if got := inProcess(adopted.JobID).Jobs[0]; !got.Adopted || got.Source != request.Source || got.Tree != request.Tree || got.Head != request.Head || got.Owner != request.Owner {
		t.Errorf("adopted job = %+v, want the tree request %+v names", got, request)
	}

	if _, err := f.h.engine.Remove(ctx, f.h.request("b")); err != nil {
		t.Fatalf("in-process Remove(b) = %v", err)
	}
	client := strconv.Itoa(os.Getpid())
	wantServed := []string{
		"intend:d=" + client,
		"accept:a=" + client, "advance:a=" + client,
		"adopt:q=" + client, "advance:q=" + client,
		"accept:b=none", "advance:b=none",
	}
	if got := f.h.relocator.takeServed(); !slices.Equal(got, wantServed) {
		t.Errorf("requesters = %q, want the connecting process on its own commands only: %q", got, wantServed)
	}

	if err := f.client.Resume(ctx); err != nil {
		t.Fatalf("Resume() = %v", err)
	}
	finished, err := f.client.Wait(ctx, removed.JobID)
	if err != nil || finished.Phase != cleanup.PhaseDone || finished.ID != removed.JobID {
		t.Errorf("Wait(a) = job %s at %s, %v; want %s done", finished.ID, finished.Phase, err, removed.JobID)
	}
	if sw, err := f.h.journal.LoadSwitch(); err != nil || sw.Paused {
		t.Errorf("LoadSwitch() after Resume = %+v, %v; want unpaused", sw, err)
	}
	if err := f.client.Pause(ctx); err != nil {
		t.Fatalf("Pause() = %v", err)
	}
	if sw, err := f.h.journal.LoadSwitch(); err != nil || !sw.Paused || !inProcess("").Paused {
		t.Errorf("LoadSwitch() after Pause = %+v, %v; want paused in the journal and the report", sw, err)
	}
}

func TestServeLogsWhoPausedAndResumed(t *testing.T) {
	var logged bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logged, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	ctx := context.Background()
	f := serveFixture(t, nil)

	if err := f.client.Pause(ctx); err != nil {
		t.Fatalf("Pause() = %v", err)
	}
	if err := f.client.Resume(ctx); err != nil {
		t.Fatalf("Resume() = %v", err)
	}

	if asked := f.h.parent.takeAsked(); !slices.Equal(asked, []int{os.Getpid(), os.Getpid()}) {
		t.Errorf("parent lookups = %v, want this process once per switch", asked)
	}
	var ops []string
	for line := range strings.Lines(logged.String()) {
		var record struct {
			Msg           string `json:"msg"`
			ClientPID     int    `json:"client_pid"`
			ClientVersion string `json:"client_version"`
			ParentPID     int    `json:"client_parent_pid"`
			Parent        string `json:"client_parent"`
		}
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("log line %q: %v", line, err)
		}
		op, found := strings.CutPrefix(record.Msg, "cleanup daemon: ")
		if !found || (op != opPause && op != opResume) {
			continue
		}
		ops = append(ops, op)
		if record.ClientPID != os.Getpid() || record.ClientVersion != version.String() || record.ParentPID != 1 || record.Parent != "zsh -c ccx vcs cleanup pause" {
			t.Errorf("%s record = %+v, want this process, its version, and its parent's command", op, record)
		}
	}
	if !slices.Equal(ops, []string{opPause, opResume}) {
		t.Errorf("logged switches = %v, want pause then resume", ops)
	}
}

func TestServePreservesTypedErrors(t *testing.T) {
	ctx := context.Background()
	f := serveFixture(t, nil)
	refusal := &cleanup.RefusedError{Worktree: f.h.tree("refused"), Reason: "dirty", Detail: "2 uncommitted paths"}
	active := &cleanup.ActiveError{Worktree: f.h.tree("active"), Holders: []cleanup.Holder{
		{PID: 7, Name: "zsh", TTY: true, Evidence: cleanup.EvidenceCwd, Path: f.h.tree("active")},
		{PID: 9, Name: "node", Evidence: cleanup.EvidenceFD, Path: f.h.tree("active") + "/src/index.ts"},
		{PID: 11, Name: "vim", Evidence: cleanup.EvidenceArgv, Path: f.h.tree("active") + "/README.md"},
	}}
	unpinned := &cleanup.RefusedError{Worktree: f.h.parked("unpinned"), Reason: "recovery", Detail: "the legacy ref resolves to another commit"}
	f.h.relocator.refuse["unpinned"] = unpinned
	f.h.relocator.refuse["held"] = &cleanup.ActiveError{Worktree: f.h.parked("held"), Holders: active.Holders[:1]}
	f.h.relocator.script("parked", blockWith(f.h.clock, "quarantine", "the job folder is watched"))
	f.h.relocator.refuse["refused"] = refusal
	f.h.relocator.refuse["active"] = active
	f.h.relocator.refuse["unclear"] = errors.New("could not verify activity: pid 9: operation not permitted")
	f.h.relocator.script("blocked", blockWith(f.h.clock, "git", "worktree move failed"))

	t.Run("refused", func(t *testing.T) {
		_, err := f.client.Remove(ctx, f.h.request("refused"))
		var got *cleanup.RefusedError
		if !errors.As(err, &got) || *got != *refusal {
			t.Errorf("Remove() = %v, want %+v", err, refusal)
		}
	})
	t.Run("active", func(t *testing.T) {
		_, err := f.client.Remove(ctx, f.h.request("active"))
		var got *cleanup.ActiveError
		if !errors.As(err, &got) || !reflect.DeepEqual(got, active) {
			t.Errorf("Remove() = %v, want %+v", err, active)
		}
	})
	t.Run("blocked", func(t *testing.T) {
		_, err := f.client.Remove(ctx, f.h.request("blocked"))
		var got *cleanup.BlockedError
		if !errors.As(err, &got) {
			t.Fatalf("Remove() = %v, want a *BlockedError", err)
		}
		report, statusErr := f.h.engine.Status(ctx, cleanup.Query{JobID: got.Job.ID})
		if statusErr != nil {
			t.Fatalf("Status(%s) = %v", got.Job.ID, statusErr)
		}
		if !reflect.DeepEqual(got.Job, report.Jobs[0]) || got.Job.Blocked.Reason != "git" || got.Job.Blocked.Detail != "worktree move failed" {
			t.Errorf("blocked job over the socket = %+v, want the in-process job %+v", got.Job, report.Jobs[0])
		}
		waited, err := f.client.Wait(ctx, got.Job.ID)
		var again *cleanup.BlockedError
		if !errors.As(err, &again) || !reflect.DeepEqual(waited, report.Jobs[0]) || !reflect.DeepEqual(again.Job, report.Jobs[0]) {
			t.Errorf("Wait(blocked) = %+v, %v; want the blocked job with a *BlockedError", waited, err)
		}
	})
	t.Run("adopt refused", func(t *testing.T) {
		_, err := f.client.Adopt(ctx, f.h.adoption("unpinned"))
		var got *cleanup.RefusedError
		if !errors.As(err, &got) || *got != *unpinned {
			t.Errorf("Adopt() = %v, want %+v", err, unpinned)
		}
	})
	t.Run("adopt active", func(t *testing.T) {
		_, err := f.client.Adopt(ctx, f.h.adoption("held"))
		var got *cleanup.ActiveError
		if want := f.h.relocator.refuse["held"]; !errors.As(err, &got) || !reflect.DeepEqual(got, want) {
			t.Errorf("Adopt() = %v, want %+v", err, want)
		}
	})
	t.Run("adopt blocked", func(t *testing.T) {
		request := f.h.adoption("parked")
		_, err := f.client.Adopt(ctx, request)
		var got *cleanup.BlockedError
		if !errors.As(err, &got) {
			t.Fatalf("Adopt() = %v, want a *BlockedError", err)
		}
		want := cleanup.Blockage{Reason: "quarantine", Detail: "the job folder is watched", At: f.h.clock.Now()}
		if *got.Job.Blocked != want || got.Job.Phase != cleanup.PhasePrepared || !got.Job.Adopted || got.Job.Source != request.Source {
			t.Errorf("blocked adoption = %+v, want %s prepared and blocked with %+v", got.Job, request.Source, want)
		}
	})
	t.Run("unknown job", func(t *testing.T) {
		if _, err := f.client.Retry(ctx, unknownJob); !errors.Is(err, cleanup.ErrUnknownJob) {
			t.Errorf("Retry(unknown) = %v, want ErrUnknownJob", err)
		}
		if _, err := f.client.Wait(ctx, unknownJob); !errors.Is(err, cleanup.ErrUnknownJob) {
			t.Errorf("Wait(unknown) = %v, want ErrUnknownJob", err)
		}
		if _, err := f.client.Status(ctx, cleanup.Query{JobID: unknownJob}); !errors.Is(err, cleanup.ErrUnknownJob) {
			t.Errorf("Status(unknown) = %v, want ErrUnknownJob", err)
		}
	})
	t.Run("untyped", func(t *testing.T) {
		_, err := f.client.Remove(ctx, f.h.request("unclear"))
		if err == nil || err.Error() != "could not verify activity: pid 9: operation not permitted" {
			t.Errorf("Remove() = %v, want the relocator's message", err)
		}
		var refused *cleanup.RefusedError
		if errors.As(err, &refused) || errors.Is(err, cleanup.ErrUnknownJob) {
			t.Errorf("Remove() = %v matched a typed error", err)
		}
	})
}

func TestServeGatesRequestsOnTheProtocol(t *testing.T) {
	f := serveFixture(t, nil)
	remove := fmt.Sprintf(`"op":"remove","remove":{"worktree":%q,"git":"/usr/bin/git"}`, f.h.tree("a"))
	unbound := f.h.adoption("q")
	unbound.RecoveryRef = cleanup.LegacyRecoveryPrefix + legacyDate + "/" + legacyID("other")
	adopt := fmt.Sprintf(
		`"op":"adopt","adopt":{"source":%q,"tree":{"dev":%d,"ino":%d},"common_dir":%q,"head":%q,"recovery_ref":%q,"original":%q,"owner":%q,"git":%q}`,
		unbound.Source, unbound.Tree.Dev, unbound.Tree.Ino, unbound.CommonDir, unbound.Head, unbound.RecoveryRef, unbound.Original, unbound.Owner, unbound.Git,
	)
	tests := []struct {
		name        string
		line        string
		wantKind    string
		wantMessage string
	}{
		{"another protocol", `{"protocol":5,"version":"v9",` + remove + `}`, kindIncompatible, "the daemon speaks protocol 4, the client sent 5"},
		{"no protocol", `{"version":"v9",` + remove + `}`, kindIncompatible, "the daemon speaks protocol 4, the client sent 0"},
		{"an unknown field", `{"protocol":4,"version":"v9","op":"status","query":{},"extra":1}`, kindInternal, ""},
		{"an unknown op", `{"protocol":4,"version":"v9","op":"purge"}`, kindInternal, ""},
		{"a remove with no request", `{"protocol":4,"version":"v9","op":"remove"}`, kindInternal, ""},
		{"an adopt with no request", `{"protocol":4,"version":"v9","op":"adopt"}`, kindInternal, ""},
		{"an adopt of a tree its ref does not pin", `{"protocol":4,"version":"v9",` + adopt + `}`, kindInternal, ""},
		{"a pause naming a job", `{"protocol":4,"version":"v9","op":"pause","job_id":"` + unknownJob + `"}`, kindInternal, ""},
		{"not json", `remove everything`, kindInternal, ""},
		{"a shutdown behind a duplicate op", `{"protocol":4,"version":"v9","op":"hello","op":"shutdown"}`, kindInternal, ""},
		{"a shutdown with an unknown field", `{"protocol":4,"version":"v9","op":"shutdown","force":true}`, kindInternal, ""},
		{"a shutdown naming a job", `{"protocol":4,"version":"v9","op":"shutdown","job_id":"` + unknownJob + `"}`, kindInternal, ""},
		{"a hello carrying a query", `{"protocol":4,"version":"v9","op":"hello","query":{}}`, kindInternal, ""},
		{"a hello with trailing data", `{"protocol":4,"version":"v9","op":"hello"} {"op":"shutdown"}`, kindInternal, ""},
		{"a duplicate-op shutdown from another protocol", `{"protocol":5,"version":"v9","op":"hello","op":"shutdown"}`, kindIncompatible, "the daemon speaks protocol 4, the client sent 5"},
		{"an unknown shape from another protocol", `{"protocol":5,"version":"v9","op":"purge","everything":true}`, kindIncompatible, "the daemon speaks protocol 4, the client sent 5"},
		{"a hello with no version", `{"protocol":4,"op":"hello"}`, kindInternal, ""},
		{"a remove with no version", `{"protocol":4,` + remove + `}`, kindInternal, ""},
		{"a pause with an empty version", `{"protocol":4,"version":"","op":"pause"}`, kindInternal, ""},
		{"a shutdown with no version", `{"protocol":4,"op":"shutdown"}`, kindInternal, ""},
		{"a shutdown with no version from another protocol", `{"protocol":5,"op":"shutdown"}`, kindIncompatible, "the daemon speaks protocol 4, the client sent 5"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reply := f.raw(tt.line)
			if reply.Error == nil || reply.Error.Kind != tt.wantKind {
				t.Fatalf("reply = %+v, want a %s error", reply, tt.wantKind)
			}
			if tt.wantMessage != "" && reply.Error.Message != tt.wantMessage {
				t.Errorf("message = %q, want %q", reply.Error.Message, tt.wantMessage)
			}
			if got := errors.Is(reply.Error.rebuild(), ErrIncompatible); got != (tt.wantKind == kindIncompatible) {
				t.Errorf("rebuilt error is ErrIncompatible = %t", got)
			}
		})
	}
	if events := f.h.rec.take(); len(events) != 0 {
		t.Errorf("refused requests executed %q, want nothing", events)
	}
	hello := f.raw(`{"protocol":5,"version":"v9","op":"hello"}`)
	if want := (cleanup.Info{Version: "v1.2.3", Protocol: cleanup.Protocol, PID: os.Getpid()}); hello.Error != nil || hello.Info == nil || *hello.Info != want {
		t.Errorf("hello from another protocol = %+v, want info %+v", hello, want)
	}
	if reply := f.raw(`{"protocol":5,"version":"v9","op":"shutdown"}`); reply != (response{}) {
		t.Errorf("a well-formed shutdown from another protocol = %+v, want it acknowledged", reply)
	}
	if err := f.wait(); err != nil {
		t.Errorf("Serve() = %v, want nil after a shutdown from another protocol", err)
	}
}

func TestSecondServeFailsOnTheLock(t *testing.T) {
	f := serveFixture(t, nil)
	if _, err := f.client.Status(context.Background(), cleanup.Query{}); err != nil {
		t.Fatalf("Status() = %v", err)
	}
	if err := os.Mkdir(f.h.layout.JobDir(unrecorded), 0o700); err != nil {
		t.Fatal(err)
	}
	bound, cancelBound := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancelBound()
	for {
		second, err := New(Config{Journal: f.h.journal, Relocator: f.h.relocator, Deleter: f.h.deleter, CPU: f.h.cpu, Band: f.h.band, Parent: f.h.parent.lookup, Identify: identify, Clock: f.h.clock})
		if err != nil {
			t.Fatalf("New() = %v", err)
		}
		round, cancel := context.WithTimeout(bound, 50*time.Millisecond)
		err = Serve(round, second, f.h.layout)
		cancel()
		if errors.Is(err, durable.ErrLockBusy) {
			break
		}
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("second Serve() = %v, want ErrLockBusy", err)
		}
		if bound.Err() != nil {
			t.Fatalf("no second Serve observed the held serve lock within 30s: %v", err)
		}
	}
	if _, err := os.Lstat(f.h.layout.JobDir(unrecorded)); err != nil {
		t.Errorf("Lstat(a job folder the serving daemon has yet to record) = %v, want it left alone by the refused daemon", err)
	}
	if _, err := f.client.Hello(context.Background()); err != nil {
		t.Errorf("Hello() after the refused second daemon = %v, want the first still serving", err)
	}
}

func TestSocketIsPrivate(t *testing.T) {
	f := serveFixture(t, nil)
	info, err := os.Lstat(f.socket)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Type() != fs.ModeSocket || info.Mode().Perm() != 0o600 {
		t.Errorf("socket mode = %s, want a 0600 socket", info.Mode())
	}
}

func TestShutdownFinishesTheSliceInFlight(t *testing.T) {
	ctx := context.Background()
	var job cleanup.Job
	gated := &payload{entries: 300, gate: make(chan struct{}), entered: make(chan struct{}, 16)}
	f := serveFixture(t, func(h *harness) {
		job = h.seed("a", 1, cleanup.PhaseUnregistered)
		h.deleter.put("a", gated)
	})
	<-gated.entered

	if _, err := f.client.roundTrip(ctx, request{Op: opShutdown}); err != nil {
		t.Fatalf("shutdown request = %v", err)
	}
	conn, err := net.Dial("unix", f.socket)
	if err != nil {
		t.Fatalf("dial during the in-flight slice = %v, want the socket held until the engine stops", err)
	}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}

	gated.gate <- struct{}{}
	if err := f.wait(); err != nil {
		t.Fatalf("Serve() = %v, want nil after shutdown", err)
	}
	if got, want := f.h.rec.take(), []string{"sample", "admit:a", "open:a", "step:a", "close:a"}; !slices.Equal(got, want) {
		t.Errorf("events = %q, want %q", got, want)
	}
	if got := f.h.journaled(job.ID); got.Phase != cleanup.PhaseDeleting || got.Removed != 100 {
		t.Errorf("journal after shutdown = phase %s, removed %d; want deleting, 100", got.Phase, got.Removed)
	}
	if _, err := os.Lstat(f.socket); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("Lstat(socket) after shutdown = %v, want not exist", err)
	}
	lockCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	lock, err := durable.AcquireLock(lockCtx, f.h.layout.ServeLockPath())
	if err != nil {
		t.Fatalf("AcquireLock() after shutdown = %v, want the serve lock released", err)
	}
	if err := lock.Close(); err != nil {
		t.Error(err)
	}
}

func anyPeer(cleanup.Peer) error { return nil }

func TestClientShutdownReturnsOnceTheSocketRefuses(t *testing.T) {
	ctx := context.Background()
	f := serveFixture(t, nil)
	if err := f.client.Shutdown(ctx, anyPeer); err != nil {
		t.Fatalf("Shutdown() = %v", err)
	}
	if conn, err := net.Dial("unix", f.socket); err == nil {
		_ = conn.Close()
		t.Error("the socket still accepts after Shutdown returned")
	}
	if err := f.wait(); err != nil {
		t.Errorf("Serve() = %v, want nil after shutdown", err)
	}
	if _, err := f.client.Hello(ctx); err == nil {
		t.Error("Hello() after shutdown = nil, want a connect error")
	}
	if err := f.h.engine.Pause(ctx); !errors.Is(err, ErrStopped) {
		t.Errorf("Pause() after shutdown = %v, want ErrStopped", err)
	}
}

func TestClientShutdownStopsOnlyAPeerItsVerifierAccepts(t *testing.T) {
	ctx := context.Background()
	f := serveFixture(t, nil)
	self := cleanup.Peer{PID: os.Getpid(), UID: os.Getuid()}
	errForeign := errors.New("not the observed daemon")

	var refused []cleanup.Peer
	err := f.client.Shutdown(ctx, func(peer cleanup.Peer) error {
		refused = append(refused, peer)
		return errForeign
	})
	if !errors.Is(err, errForeign) {
		t.Fatalf("Shutdown() refused by its verifier = %v, want %v", err, errForeign)
	}
	if !slices.Equal(refused, []cleanup.Peer{self}) {
		t.Errorf("verifier saw %+v, want the serving process %+v once", refused, self)
	}
	if _, err := f.client.Hello(ctx); err != nil {
		t.Fatalf("Hello() after a refused Shutdown = %v, want the daemon still serving", err)
	}

	var accepted []cleanup.Peer
	if err := f.client.Shutdown(ctx, func(peer cleanup.Peer) error {
		accepted = append(accepted, peer)
		return nil
	}); err != nil {
		t.Fatalf("Shutdown() accepted by its verifier = %v", err)
	}
	if !slices.Equal(accepted, []cleanup.Peer{self}) {
		t.Errorf("verifier saw %+v, want the serving process %+v once", accepted, self)
	}
	if err := f.wait(); err != nil {
		t.Errorf("Serve() = %v, want nil after shutdown", err)
	}
}

func TestClientShutdownSendsNothingToARefusedPeer(t *testing.T) {
	root, err := os.MkdirTemp("/tmp", "ccxc")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(root); err != nil {
			t.Errorf("remove %s: %v", root, err)
		}
	})
	socket := filepath.Join(root, "s")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()
	type delivery struct {
		data []byte
		err  error
	}
	received := make(chan delivery, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			received <- delivery{err: err}
			return
		}
		defer func() { _ = conn.Close() }()
		data, err := io.ReadAll(conn)
		received <- delivery{data: data, err: err}
	}()
	errForeign := errors.New("not the observed daemon")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := Dial(socket).Shutdown(ctx, func(cleanup.Peer) error { return errForeign }); !errors.Is(err, errForeign) {
		t.Errorf("Shutdown() refused by its verifier = %v, want %v", err, errForeign)
	}
	if got := <-received; got.err != nil || len(got.data) != 0 {
		t.Errorf("the refused peer received %q, %v; want nothing before the connection closed", got.data, got.err)
	}
}

func TestClientGreetObservesTheServingPeer(t *testing.T) {
	ctx := context.Background()
	f := serveFixture(t, nil)
	var observed []cleanup.Peer
	info, err := f.client.Greet(ctx, func(peer cleanup.Peer) { observed = append(observed, peer) })
	if err != nil {
		t.Fatalf("Greet() = %v", err)
	}
	if want := []cleanup.Peer{{PID: os.Getpid(), UID: os.Getuid()}}; !slices.Equal(observed, want) {
		t.Errorf("observe saw %+v, want the serving process %+v once", observed, want)
	}
	hello, err := f.client.Hello(ctx)
	if err != nil || hello != info {
		t.Errorf("Hello() = %+v, %v; want Greet's %+v", hello, err, info)
	}
}

func TestClientGreetSendsNothingBeforeObserve(t *testing.T) {
	root, err := os.MkdirTemp("/tmp", "ccxc")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(root); err != nil {
			t.Errorf("remove %s: %v", root, err)
		}
	})
	socket := filepath.Join(root, "s")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()
	type accepted struct {
		conn net.Conn
		err  error
	}
	conns := make(chan accepted, 1)
	go func() {
		conn, err := listener.Accept()
		conns <- accepted{conn: conn, err: err}
	}()
	hello := fmt.Sprintf(`{"info":{"version":"v0.0.1","protocol":%d,"pid":7}}`, cleanup.Protocol)
	served := make(chan error, 1)
	var (
		early    int
		earlyErr error
	)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	info, err := Dial(socket).Greet(ctx, func(cleanup.Peer) {
		got := <-conns
		if got.err != nil {
			t.Fatalf("Accept() = %v", got.err)
		}
		raw, err := got.conn.(*net.UnixConn).SyscallConn()
		if err != nil {
			t.Fatal(err)
		}
		if err := raw.Read(func(fd uintptr) bool {
			early, earlyErr = syscall.Read(int(fd), make([]byte, 1))
			return true
		}); err != nil {
			t.Fatal(err)
		}
		go func() {
			defer func() { _ = got.conn.Close() }()
			if _, err := bufio.NewReader(got.conn).ReadBytes('\n'); err != nil {
				served <- err
				return
			}
			_, err := got.conn.Write([]byte(hello + "\n"))
			served <- err
		}()
	})
	if err != nil {
		t.Fatalf("Greet() = %v", err)
	}
	if want := (cleanup.Info{Version: "v0.0.1", Protocol: cleanup.Protocol, PID: 7}); info != want {
		t.Errorf("Greet() = %+v, want %+v", info, want)
	}
	if early > 0 || !errors.Is(earlyErr, syscall.EAGAIN) {
		t.Errorf("the daemon had %d bytes, %v buffered when observe ran; want none and %v: nothing is sent before observe", early, earlyErr, syscall.EAGAIN)
	}
	if err := <-served; err != nil {
		t.Errorf("the stand-in daemon = %v", err)
	}
}

func TestServeStopsWhenItsContextEnds(t *testing.T) {
	f := serveFixture(t, nil)
	if _, err := f.client.Hello(context.Background()); err != nil {
		t.Fatalf("Hello() = %v", err)
	}
	f.cancel()
	if err := f.wait(); err != nil {
		t.Errorf("Serve() = %v, want nil once its context ends", err)
	}
	if _, err := os.Lstat(f.socket); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("Lstat(socket) = %v, want not exist", err)
	}
}

func TestServeReplacesAStaleSocket(t *testing.T) {
	root, err := os.MkdirTemp("/tmp", "ccxc")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(root); err != nil {
			t.Errorf("remove %s: %v", root, err)
		}
	})
	h := newHarnessAt(t, root, DefaultTuning())
	socket, err := h.layout.Socket()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(socket, []byte("left by a crashed daemon"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(h.layout.JobDir(unrecorded), 0o700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	s, err := listen(ctx, h.build(), h.layout)
	if err != nil {
		cancel()
		t.Fatalf("listen() over a stale socket = %v", err)
	}
	go func() { served <- s.serve(ctx) }()
	if _, err := Dial(socket).Hello(ctx); err != nil {
		t.Errorf("Hello() = %v", err)
	}
	if report, err := Dial(socket).Status(ctx, cleanup.Query{}); err != nil || len(report.Jobs) != 0 || len(report.Damaged) != 0 {
		t.Errorf("Status() = %+v, %v; want an empty queue", report, err)
	}
	if _, err := os.Lstat(h.layout.JobDir(unrecorded)); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("Lstat(a job folder a crashed daemon never recorded) = %v, want it discarded once the lock was held", err)
	}
	cancel()
	if err := <-served; err != nil {
		t.Errorf("Serve() = %v", err)
	}
}

func TestRemoveSurvivesADisconnectedClient(t *testing.T) {
	gated := &payload{entries: 300, gate: make(chan struct{}), entered: make(chan struct{}, 16)}
	f := serveFixture(t, func(h *harness) { h.deleter.put("a", gated) })
	gate := make(chan struct{})
	f.h.relocator.gates["a"] = gate
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	abandoned := make(chan error, 1)
	go func() {
		_, err := f.client.Remove(ctx, f.h.request("a"))
		abandoned <- err
	}()
	if got := <-f.h.relocator.entered; got != "a" {
		t.Fatalf("the relocator entered %q, want a", got)
	}
	cancel()
	if err := <-abandoned; !errors.Is(err, context.Canceled) {
		t.Fatalf("abandoned Remove() = %v, want context.Canceled", err)
	}
	gate <- struct{}{}
	<-gated.entered
	report, err := f.client.Status(context.Background(), cleanup.Query{})
	if err != nil {
		t.Fatalf("Status() = %v", err)
	}
	if len(report.Jobs) != 1 || report.Jobs[0].Phase != cleanup.PhaseDeleting || report.Jobs[0].Blocked != nil {
		t.Errorf("jobs after the client left = %+v, want the one job relocated and into its deletion", report.Jobs)
	}
	if got, want := f.h.rec.take(), []string{"accept:a", "advance:a@prepared", "sample", "admit:a", "open:a", "step:a"}; !slices.Equal(got, want) {
		t.Errorf("events = %q, want %q", got, want)
	}
	gated.gate <- struct{}{}
}

func TestServeRefusesNewRemovalsWhilePaused(t *testing.T) {
	ctx := context.Background()
	f := serveFixture(t, queuePaused)
	tests := []struct {
		name string
		ask  func() error
	}{
		{"remove", func() error { _, err := f.client.Remove(ctx, f.h.request("a")); return err }},
		{"defer", func() error { _, err := f.client.Defer(ctx, f.h.deferral("d")); return err }},
		{"adopt", func() error { _, err := f.client.Adopt(ctx, f.h.adoption("q")); return err }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.ask()
			if !errors.Is(err, cleanup.ErrPaused) || err.Error() != cleanup.ErrPaused.Error() {
				t.Errorf("%s over the socket = %v, want ErrPaused rebuilt as is", tt.name, err)
			}
		})
	}
	raw := f.raw(fmt.Sprintf(`{"protocol":4,"version":"v9","op":"remove","remove":{"worktree":%q,"git":"/usr/bin/git"}}`, f.h.tree("a")))
	if raw.Error == nil || raw.Error.Kind != kindPaused || raw.Error.Message != cleanup.ErrPaused.Error() {
		t.Errorf("raw remove reply = %+v, want a %s error carrying ErrPaused's message", raw, kindPaused)
	}
	if events := f.h.rec.take(); len(events) != 0 {
		t.Errorf("paused requests executed %q, want nothing", events)
	}
	if served := f.h.relocator.takeServed(); len(served) != 0 {
		t.Errorf("paused requests reached the relocator as %q, want never", served)
	}
	jobs, damaged, err := f.h.journal.Load()
	if err != nil || len(damaged) != 0 || len(jobs) != 0 {
		t.Errorf("Load() = %d jobs, damaged %v, %v; want an empty journal", len(jobs), damaged, err)
	}
	report, err := f.client.Status(ctx, cleanup.Query{})
	if err != nil || !report.Paused || len(report.Jobs) != 0 {
		t.Errorf("Status() = paused %t, %d jobs, %v; want paused and empty", report.Paused, len(report.Jobs), err)
	}

	if err := f.client.Resume(ctx); err != nil {
		t.Fatalf("Resume() = %v", err)
	}
	removed, err := f.client.Remove(ctx, f.h.request("a"))
	if err != nil || removed.State != cleanup.State(cleanup.PhaseUnregistered) {
		t.Fatalf("Remove() after the resume = %+v, %v; want it unregistered", removed, err)
	}
	if finished, err := f.client.Wait(ctx, removed.JobID); err != nil || finished.Phase != cleanup.PhaseDone {
		t.Errorf("Wait() = phase %s, %v; want done", finished.Phase, err)
	}
	client := strconv.Itoa(os.Getpid())
	if got, want := f.h.relocator.takeServed(), []string{"accept:a=" + client, "advance:a=" + client}; !slices.Equal(got, want) {
		t.Errorf("requesters after the resume = %q, want %q", got, want)
	}
}

func TestClientWaitFollowsAJobToDone(t *testing.T) {
	var job cleanup.Job
	gated := &payload{entries: 100, gate: make(chan struct{}), entered: make(chan struct{}, 16)}
	f := serveFixture(t, func(h *harness) {
		job = h.seed("a", 1, cleanup.PhaseUnregistered)
		h.deleter.put("a", gated)
	})
	<-gated.entered
	type outcome struct {
		job cleanup.Job
		err error
	}
	waited := make(chan outcome, 1)
	go func() {
		got, err := f.client.Wait(context.Background(), job.ID)
		waited <- outcome{got, err}
	}()
	gated.gate <- struct{}{}
	got := <-waited
	if got.err != nil || got.job.ID != job.ID || got.job.Phase != cleanup.PhaseDone || got.job.Removed != 100 {
		t.Errorf("Wait() = job %s at %s, removed %d, %v; want %s done, 100", got.job.ID, got.job.Phase, got.job.Removed, got.err, job.ID)
	}
}

func TestOversizedRequestIsDropped(t *testing.T) {
	f := serveFixture(t, nil)
	conn, err := net.Dial("unix", f.socket)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	_, _ = conn.Write(make([]byte, maxRequestBytes+1))
	if line, err := bufio.NewReader(conn).ReadBytes('\n'); err == nil {
		t.Errorf("reply to an oversized request = %q, want the connection closed", line)
	}
	if _, err := f.client.Hello(context.Background()); err != nil {
		t.Errorf("Hello() after an oversized request = %v", err)
	}
}

func TestShutdownReportsADaemonItCannotReach(t *testing.T) {
	root, err := os.MkdirTemp("/tmp", "ccxc")
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "d")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := errors.Join(os.Chmod(dir, 0o700), os.RemoveAll(root)); err != nil { //nolint:gosec // restores a directory's search bit
			t.Errorf("remove %s: %v", root, err)
		}
	})
	socket := filepath.Join(dir, "s")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()
	acknowledged := make(chan error, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			acknowledged <- err
			return
		}
		defer func() { _ = conn.Close() }()
		if _, err := bufio.NewReader(conn).ReadBytes('\n'); err != nil {
			acknowledged <- err
			return
		}
		if err := os.Chmod(dir, 0); err != nil {
			acknowledged <- err
			return
		}
		_, err = conn.Write([]byte("{}\n"))
		acknowledged <- err
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := Dial(socket).Shutdown(ctx, anyPeer); !errors.Is(err, fs.ErrPermission) {
		t.Errorf("Shutdown() of a daemon still listening behind an unreachable socket = %v, want the connect failure", err)
	}
	if err := <-acknowledged; err != nil {
		t.Errorf("the stand-in daemon = %v", err)
	}
}
