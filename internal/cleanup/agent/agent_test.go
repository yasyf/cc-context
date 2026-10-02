package agent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
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
)

var (
	errRefused   = fmt.Errorf("cleanup daemon: connect to sock: %w", syscall.ECONNREFUSED)
	errMissing   = fmt.Errorf("cleanup daemon: connect to sock: %w", syscall.ENOENT)
	errClosing   = fmt.Errorf("cleanup daemon: hello: %w", io.EOF)
	errDeparted  = fmt.Errorf("%w: sock: read LOCAL_PEERPID: %w", cleanup.ErrUnidentifiedPeer, syscall.ENOTCONN)
	errNameless  = fmt.Errorf("%w: sock: read LOCAL_PEERCRED: %w", cleanup.ErrUnidentifiedPeer, syscall.EINVAL)
	errDenied    = fmt.Errorf("cleanup daemon: connect to sock: %w", syscall.EACCES)
	errLockQuery = fmt.Errorf("cleanup agent: open the serve lock: %w", syscall.EACCES)
	errBootstrap = errors.New("bootstrap failed")

	sourceBytes = []byte("#!/bin/sh\nexec true\n")
)

type fakeDaemon struct {
	mu         sync.Mutex
	serving    *cleanup.Info
	starts     *cleanup.Info
	transient  []error
	helloErr   error
	hangs      bool
	lockHeld   bool
	lockErr    error
	applyErr   error
	applyHangs bool
	lingers    int
	exiting    int
	foreignUID bool
	kernelPID  int
	started    time.Time
	bornErr    error
	greeted    func()
	events     []string
	sockets    []string
	entered    chan struct{}
	enter      sync.Once
	gate       chan struct{}
	hellos     chan struct{}
}

type fakeControl struct {
	cleanup.Service
	d *fakeDaemon
}

func (c fakeControl) Hello(ctx context.Context) (cleanup.Info, error) {
	return c.hello(ctx, func(cleanup.Peer) {})
}

func (c fakeControl) Greet(ctx context.Context, observe func(cleanup.Peer)) (cleanup.Info, error) {
	info, err := c.hello(ctx, observe)
	if c.d.greeted != nil {
		c.d.greeted()
	}
	return info, err
}

func (c fakeControl) hello(ctx context.Context, observe func(cleanup.Peer)) (cleanup.Info, error) {
	c.d.mu.Lock()
	c.d.events = append(c.d.events, "hello")
	serving, helloErr, hangs := c.d.serving, c.d.helloErr, c.d.hangs
	var peer cleanup.Peer
	if serving != nil {
		peer = c.d.peer(serving)
	}
	var transient error
	if len(c.d.transient) > 0 {
		transient, c.d.transient = c.d.transient[0], c.d.transient[1:]
	}
	c.d.mu.Unlock()
	if c.d.hellos != nil {
		c.d.hellos <- struct{}{}
	}
	switch {
	case transient != nil:
		return cleanup.Info{}, transient
	case hangs:
		if serving != nil {
			observe(peer)
		}
		<-ctx.Done()
		return cleanup.Info{}, ctx.Err()
	case serving != nil:
		observe(peer)
		return *serving, nil
	case helloErr != nil:
		return cleanup.Info{}, helloErr
	}
	return cleanup.Info{}, errRefused
}

func (c fakeControl) Shutdown(_ context.Context, verify func(cleanup.Peer) error) error {
	c.d.mu.Lock()
	serving := c.d.serving
	var peer cleanup.Peer
	if serving != nil {
		peer = c.d.peer(serving)
	}
	c.d.mu.Unlock()
	if serving == nil {
		return errRefused
	}
	err := verify(peer)
	c.d.mu.Lock()
	defer c.d.mu.Unlock()
	if err != nil {
		c.d.events = append(c.d.events, "refused "+strconv.Itoa(serving.PID))
		return err
	}
	c.d.events = append(c.d.events, "shutdown")
	c.d.serving = nil
	c.d.exiting = c.d.lingers
	return nil
}

func birth(pid int) (time.Time, error) {
	return time.Unix(int64(pid), 0), nil
}

func (d *fakeDaemon) peer(serving *cleanup.Info) cleanup.Peer {
	pid, uid := serving.PID, os.Getuid()
	if d.kernelPID != 0 {
		pid = d.kernelPID
	}
	if d.foreignUID {
		uid++
	}
	return cleanup.Peer{PID: pid, UID: uid}
}

func (d *fakeDaemon) born(pid int) (time.Time, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	switch {
	case d.bornErr != nil:
		return time.Time{}, d.bornErr
	case d.serving != nil && pid == d.serving.PID && !d.started.IsZero():
		return d.started, nil
	}
	return birth(pid)
}

func (d *fakeDaemon) dial(socket string) cleanup.Control {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.sockets = append(d.sockets, socket)
	return fakeControl{d: d}
}

func (d *fakeDaemon) alive(pid int) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.events = append(d.events, "alive "+strconv.Itoa(pid))
	if d.exiting == 0 {
		return false
	}
	d.exiting--
	return true
}

func (d *fakeDaemon) held(lock string) (bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.events = append(d.events, "held "+filepath.Base(lock))
	return d.lockHeld, d.lockErr
}

func (d *fakeDaemon) apply(ctx context.Context) error {
	d.mu.Lock()
	d.events = append(d.events, "apply")
	hangs := d.applyHangs
	d.mu.Unlock()
	if hangs {
		<-ctx.Done()
		return ctx.Err()
	}
	if d.entered != nil {
		d.enter.Do(func() { close(d.entered) })
	}
	if d.gate != nil {
		<-d.gate
	}
	if d.applyErr != nil {
		return d.applyErr
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.exiting == 0 {
		d.serving = d.starts
	}
	return nil
}

func (d *fakeDaemon) recorded() ([]string, []string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return slices.Clone(d.events), slices.Clone(d.sockets)
}

func (d *fakeDaemon) count(event string) int {
	events, _ := d.recorded()
	n := 0
	for _, e := range events {
		if e == event {
			n++
		}
	}
	return n
}

func receive[T any](t *testing.T, ch <-chan T, what string) T {
	t.Helper()
	var zero T
	select {
	case v := <-ch:
		return v
	case <-time.After(10 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
	return zero
}

func fixture(t *testing.T, d *fakeDaemon) starter {
	t.Helper()
	dir, err := os.MkdirTemp(os.TempDir(), "ccxa")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	source := filepath.Join(dir, "ccx")
	if err := os.WriteFile(source, sourceBytes, 0o755); err != nil { //nolint:gosec // the fixture stands in for an installed executable
		t.Fatal(err)
	}
	return starter{
		Options: Options{
			Layout:  cleanup.Layout{Root: filepath.Join(dir, "state")},
			Version: "v1.2.3",
			Source:  source,
			Dial:    d.dial,
			Timeout: 5 * time.Second,
		},
		apply: d.apply,
		alive: d.alive,
		held:  d.held,
		born:  d.born,
	}
}

func TestOutdated(t *testing.T) {
	tests := []struct {
		name           string
		daemon, client string
		want           bool
	}{
		{"newer patch", "v1.2.3", "v1.2.4", true},
		{"newer minor compares numerically", "v1.9.0", "v1.10.0", true},
		{"newer major without a v", "1.9.9", "v2.0.0", true},
		{"same release", "v1.2.3", "v1.2.3", false},
		{"same release, v prefix differs", "1.2.3", "v1.2.3", false},
		{"older client never downgrades", "v1.3.0", "v1.2.9", false},
		{"older client never downgrades a major", "v2.0.0", "v1.99.99", false},
		{"identical dev builds", "dev", "dev", false},
		{"dev client replaces a release", "v1.2.3", "dev", true},
		{"release client replaces a dev daemon", "dev", "v1.2.3", true},
		{"dirty daemon replaced by its clean release", "v1.2.3+dirty", "v1.2.3", true},
		{"pseudo-version daemon replaced by an older release", "v1.2.4-0.20260930063442-3008bab62b90", "v1.2.3", true},
		{"identical prerelease builds", "v1.2.3-rc.1", "v1.2.3-rc.1", false},
		{"leading zero is not clean semver", "v1.02.3", "v1.2.3", true},
		{"two-part version is not clean semver", "v1.2", "v1.2.0", true},
		{"unreported daemon version", "", "v1.2.3", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Outdated(tt.daemon, tt.client); got != tt.want {
				t.Errorf("Outdated(%q, %q) = %v, want %v", tt.daemon, tt.client, got, tt.want)
			}
		})
	}
}

func TestInstallProgram(t *testing.T) {
	write := func(data []byte, mode os.FileMode) func(*testing.T, string, string) {
		return func(t *testing.T, path, _ string) {
			if err := os.WriteFile(path, data, mode); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(path, mode); err != nil {
				t.Fatal(err)
			}
		}
	}
	tests := []struct {
		name          string
		prepare       func(t *testing.T, path, source string)
		linkSource    bool
		wantChanged   bool
		wantSameInode bool
	}{
		{name: "fresh install", wantChanged: true},
		{name: "equal copy is left alone", prepare: write(sourceBytes, 0o700), wantSameInode: true},
		{name: "different copy is replaced by a new inode", prepare: write([]byte("old build"), 0o700), wantChanged: true},
		{name: "copy with the wrong mode is republished", prepare: write(sourceBytes, 0o755), wantChanged: true},
		{
			name: "symlink at the program path is replaced by a regular file",
			prepare: func(t *testing.T, path, source string) {
				if err := os.Symlink(source, path); err != nil {
					t.Fatal(err)
				}
			},
			wantChanged: true,
		},
		{name: "symlinked source is copied as a regular file", linkSource: true, wantChanged: true},
		{
			name: "equal hardlink to the source is replaced by an independent copy",
			prepare: func(t *testing.T, path, source string) {
				if err := os.Chmod(source, 0o700); err != nil { //nolint:gosec // an executable, given the mode of the installed copy
					t.Fatal(err)
				}
				if err := os.Link(source, path); err != nil {
					t.Fatal(err)
				}
			},
			wantChanged: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			layout := cleanup.Layout{Root: filepath.Join(dir, "state")}
			source := filepath.Join(dir, "ccx")
			if err := os.WriteFile(source, sourceBytes, 0o755); err != nil { //nolint:gosec // the fixture stands in for an installed executable
				t.Fatal(err)
			}
			if tt.linkSource {
				link := filepath.Join(dir, "ccx-link")
				if err := os.Symlink(source, link); err != nil {
					t.Fatal(err)
				}
				source = link
			}
			path := layout.ProgramPath()
			var before cleanup.FileID
			if tt.prepare != nil {
				if err := layout.Ensure(); err != nil {
					t.Fatal(err)
				}
				tt.prepare(t, path, source)
				id, _, err := cleanup.LstatID(path)
				if err != nil {
					t.Fatal(err)
				}
				before = id
			}

			changed, err := InstallProgram(layout, source)
			if err != nil {
				t.Fatalf("InstallProgram() error = %v", err)
			}
			if changed != tt.wantChanged {
				t.Errorf("InstallProgram() changed = %v, want %v", changed, tt.wantChanged)
			}
			after, info, err := cleanup.LstatID(path)
			if err != nil {
				t.Fatal(err)
			}
			if !info.Mode().IsRegular() || info.Mode().Perm() != 0o700 {
				t.Errorf("program mode = %v, want a regular 0700 file", info.Mode())
			}
			if links := info.Sys().(*syscall.Stat_t).Nlink; links != 1 {
				t.Errorf("program has %d links, want 1: an independent copy", links)
			}
			got, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, sourceBytes) {
				t.Errorf("program = %q, want %q", got, sourceBytes)
			}
			if !before.Zero() && (after == before) != tt.wantSameInode {
				t.Errorf("program identity before %v, after %v; want same = %v", before, after, tt.wantSameInode)
			}

			again, err := InstallProgram(layout, source)
			if err != nil || again {
				t.Errorf("repeat InstallProgram() = %v, %v; want false, nil", again, err)
			}
			if id, _, err := cleanup.LstatID(path); err != nil || id != after {
				t.Errorf("repeat InstallProgram() moved the program to %v (%v); want %v", id, err, after)
			}
		})
	}
}

func TestConnect(t *testing.T) {
	current := cleanup.Info{Version: "v1.2.3", Protocol: cleanup.Protocol, PID: 42}
	older := cleanup.Info{Version: "v1.2.2", Protocol: cleanup.Protocol, PID: 41}
	newer := cleanup.Info{Version: "v1.3.0", Protocol: cleanup.Protocol, PID: 43}
	olderProtocol := cleanup.Info{Version: "v1.2.3", Protocol: cleanup.Protocol - 1, PID: 40}
	newerProtocol := cleanup.Info{Version: "v1.2.3", Protocol: cleanup.Protocol + 1, PID: 44}
	tests := []struct {
		name          string
		serving       *cleanup.Info
		starts        *cleanup.Info
		transient     []error
		helloErr      error
		lockHeld      bool
		applyErr      error
		applyHangs    bool
		timeout       time.Duration
		lingers       int
		wantEvents    []string
		wantInfo      cleanup.Info
		wantErr       error
		wantText      string
		wantInstalled bool
	}{
		{
			name:       "current daemon takes the fast path",
			serving:    &current,
			wantEvents: []string{"hello"},
			wantInfo:   current,
		},
		{
			name:       "newer daemon is kept",
			serving:    &newer,
			wantEvents: []string{"hello"},
			wantInfo:   newer,
		},
		{
			name:          "unreachable daemon is applied then ready",
			starts:        &current,
			wantEvents:    []string{"hello", "held serve.lock", "hello", "held serve.lock", "apply", "hello"},
			wantInfo:      current,
			wantInstalled: true,
		},
		{
			name:          "missing socket is applied then ready",
			starts:        &current,
			helloErr:      errMissing,
			wantEvents:    []string{"hello", "held serve.lock", "hello", "held serve.lock", "apply", "hello"},
			wantInfo:      current,
			wantInstalled: true,
		},
		{
			name:       "closing daemon replaced meanwhile is found by the probe under the start lock",
			serving:    &current,
			transient:  []error{errClosing},
			wantEvents: []string{"hello", "hello"},
			wantInfo:   current,
		},
		{
			name:       "refusing daemon still holding the serve lock is replaced meanwhile",
			serving:    &current,
			transient:  []error{errRefused},
			lockHeld:   true,
			wantEvents: []string{"hello", "held serve.lock", "hello"},
			wantInfo:   current,
		},
		{
			name:       "closing daemon that disconnects before its peer is read under the start lock did not answer",
			transient:  []error{errClosing},
			helloErr:   errDeparted,
			wantEvents: []string{"hello", "hello"},
			wantErr:    errUnresponsive,
		},
		{
			name:       "daemon whose peer the hello connection cannot identify under the start lock is left running",
			transient:  []error{errClosing},
			helloErr:   errNameless,
			wantEvents: []string{"hello", "hello"},
			wantErr:    errUnverified,
		},
		{
			name:          "outdated daemon is shut down before apply",
			serving:       &older,
			starts:        &current,
			wantEvents:    []string{"hello", "hello", "shutdown", "alive 41", "apply", "hello"},
			wantInfo:      current,
			wantInstalled: true,
		},
		{
			name:          "outdated daemon is applied only after its process exits",
			serving:       &older,
			starts:        &current,
			lingers:       2,
			wantEvents:    []string{"hello", "hello", "shutdown", "alive 41", "alive 41", "alive 41", "apply", "hello"},
			wantInfo:      current,
			wantInstalled: true,
		},
		{
			name:          "older protocol daemon is shut down before apply",
			serving:       &olderProtocol,
			starts:        &current,
			wantEvents:    []string{"hello", "hello", "shutdown", "alive 40", "apply", "hello"},
			wantInfo:      current,
			wantInstalled: true,
		},
		{
			name:       "newer protocol daemon is refused",
			serving:    &newerProtocol,
			wantEvents: []string{"hello"},
			wantErr:    errNewerProtocol,
		},
		{
			name:          "outdated daemon answering after the start is refused",
			starts:        &older,
			wantEvents:    []string{"hello", "held serve.lock", "hello", "held serve.lock", "apply", "hello"},
			wantErr:       errOutdatedAfterStart,
			wantInstalled: true,
		},
		{
			name:          "apply failure is returned",
			applyErr:      errBootstrap,
			wantEvents:    []string{"hello", "held serve.lock", "hello", "held serve.lock", "apply"},
			wantErr:       errBootstrap,
			wantInstalled: true,
		},
		{
			name:          "apply that never returns is bounded by the timeout",
			applyHangs:    true,
			timeout:       50 * time.Millisecond,
			wantEvents:    []string{"hello", "held serve.lock", "hello", "held serve.lock", "apply"},
			wantErr:       context.DeadlineExceeded,
			wantText:      "cleanup agent: start the daemon: launchd did not finish within 50ms: context deadline exceeded",
			wantInstalled: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := &fakeDaemon{
				serving:    tt.serving,
				starts:     tt.starts,
				transient:  tt.transient,
				helloErr:   tt.helloErr,
				lockHeld:   tt.lockHeld,
				applyErr:   tt.applyErr,
				applyHangs: tt.applyHangs,
				lingers:    tt.lingers,
			}
			s := fixture(t, d)
			if tt.timeout != 0 {
				s.Timeout = tt.timeout
			}

			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			ctl, err := s.connect(ctx)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("connect() error = %v, want %v", err, tt.wantErr)
			}
			if tt.wantText != "" && err.Error() != tt.wantText {
				t.Errorf("connect() error = %q, want %q", err, tt.wantText)
			}
			events, sockets := d.recorded()
			if !reflect.DeepEqual(events, tt.wantEvents) {
				t.Errorf("events = %q, want %q", events, tt.wantEvents)
			}
			socket, err := s.Layout.Socket()
			if err != nil {
				t.Fatal(err)
			}
			for _, got := range sockets {
				if got != socket {
					t.Errorf("dialed %q, want %q", got, socket)
				}
			}
			if tt.wantErr == nil {
				info, err := ctl.Hello(context.Background())
				if err != nil || info != tt.wantInfo {
					t.Errorf("returned control answers %+v, %v; want %+v", info, err, tt.wantInfo)
				}
			} else if ctl != nil {
				t.Errorf("connect() control = %v, want nil beside an error", ctl)
			}
			got, err := os.ReadFile(s.Layout.ProgramPath())
			switch {
			case tt.wantInstalled && (err != nil || !bytes.Equal(got, sourceBytes)):
				t.Errorf("program copy = %q, %v; want the source bytes", got, err)
			case !tt.wantInstalled && !errors.Is(err, fs.ErrNotExist):
				t.Errorf("program copy read error = %v, want nothing installed", err)
			}
			if slices.Equal(tt.wantEvents, []string{"hello"}) {
				if _, err := os.Stat(s.Layout.Root); !errors.Is(err, fs.ErrNotExist) {
					t.Errorf("layout root stat error = %v, want a first probe that decides alone to create nothing", err)
				}
			}
		})
	}
}

func TestConnectReportsACancelledCallerDuringStart(t *testing.T) {
	d := &fakeDaemon{applyHangs: true}
	s := fixture(t, d)
	s.Timeout = 5 * time.Second
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	stop := time.AfterFunc(100*time.Millisecond, cancel)
	defer stop.Stop()

	ctl, err := s.connect(ctx)
	if !errors.Is(err, context.Canceled) || ctl != nil {
		t.Fatalf("connect() = %v, %v; want nil and the caller's cancellation", ctl, err)
	}
	if want := "cleanup agent: start the daemon: context canceled"; err.Error() != want {
		t.Errorf("connect() error = %q, want %q", err, want)
	}
	if got := d.count("apply"); got != 1 {
		t.Errorf("apply calls = %d, want 1", got)
	}
}

func TestConnectNeverReadyTimesOut(t *testing.T) {
	d := &fakeDaemon{}
	s := fixture(t, d)
	s.Timeout = 100 * time.Millisecond

	ctl, err := s.connect(context.Background())
	if !errors.Is(err, errRefused) || ctl != nil {
		t.Fatalf("connect() = %v, %v; want nil and an error wrapping %v", ctl, err, errRefused)
	}
	if got := d.count("apply"); got != 1 {
		t.Errorf("apply ran %d times, want 1", got)
	}
	if got := d.count("hello"); got < 3 {
		t.Errorf("hello ran %d times, want the two probes and at least one readiness poll", got)
	}
}

func TestConnectOutdatedNeverExits(t *testing.T) {
	d := &fakeDaemon{
		serving: &cleanup.Info{Version: "v1.2.2", Protocol: cleanup.Protocol, PID: 41},
		starts:  &cleanup.Info{Version: "v1.2.3", Protocol: cleanup.Protocol, PID: 42},
		lingers: 1 << 30,
	}
	s := fixture(t, d)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	alive := s.alive
	s.alive = func(pid int) bool {
		running := alive(pid)
		if d.count("alive 41") == 2 {
			cancel()
		}
		return running
	}

	ctl, err := s.connect(ctx)
	if !errors.Is(err, context.Canceled) || ctl != nil {
		t.Fatalf("connect() = %v, %v; want nil and an error wrapping %v", ctl, err, context.Canceled)
	}
	if got := d.count("apply"); got != 0 {
		t.Errorf("apply ran %d times, want 0 while the outdated daemon still runs", got)
	}
	if got := d.count("alive 41"); got < 2 {
		t.Errorf("the outdated daemon's process was checked %d times, want repeated checks", got)
	}
	if _, err := os.Lstat(s.Layout.ProgramPath()); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("program copy stat error = %v, want nothing installed while the outdated daemon still runs", err)
	}
}

func TestConnectLeavesADaemonThatTakesTheServeLockFirst(t *testing.T) {
	current := cleanup.Info{Version: "v1.2.3", Protocol: cleanup.Protocol, PID: 42}
	older := cleanup.Info{Version: "v1.2.2", Protocol: cleanup.Protocol, PID: 41}
	tests := []struct {
		name          string
		serving       *cleanup.Info
		barrier       string
		answers       *cleanup.Info
		wantProbes    []string
		wantShutdowns int
		wantErr       error
	}{
		{
			name:       "no daemon, then one that never answers starts after the probe under the start lock",
			barrier:    "held",
			wantProbes: []string{"hello", "held serve.lock", "hello", "held serve.lock"},
			wantErr:    errStartedElsewhere,
		},
		{
			name:       "no daemon, then a current one starts after the probe under the start lock",
			barrier:    "held",
			answers:    &current,
			wantProbes: []string{"hello", "held serve.lock", "hello", "held serve.lock"},
		},
		{
			name:          "outdated daemon exits, then one that never answers starts before this client",
			serving:       &older,
			barrier:       "alive",
			wantProbes:    []string{"hello", "hello", "shutdown", "alive 41"},
			wantShutdowns: 1,
			wantErr:       errStartedElsewhere,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := &fakeDaemon{serving: tt.serving, starts: &current}
			s := fixture(t, d)
			s.Timeout = 100 * time.Millisecond
			admit := func() {
				ctx, cancel := context.WithTimeout(t.Context(), time.Second)
				defer cancel()
				lock, err := durable.AcquireLock(ctx, s.Layout.ServeLockPath())
				if err != nil {
					t.Fatalf("the starting daemon's AcquireLock() = %v", err)
				}
				t.Cleanup(func() { _ = lock.Close() })
				d.mu.Lock()
				defer d.mu.Unlock()
				d.serving = tt.answers
			}
			held, alive := s.held, s.alive
			s.held = func(lock string) (bool, error) {
				observed, err := held(lock)
				if tt.barrier == "held" && d.count("held serve.lock") == 2 {
					admit()
				}
				return observed, err
			}
			s.alive = func(pid int) bool {
				running := alive(pid)
				if tt.barrier == "alive" && !running {
					admit()
				}
				return running
			}

			ctl, err := s.connect(t.Context())
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("connect() error = %v, want %v", err, tt.wantErr)
			}
			if tt.wantErr != nil {
				if ctl != nil {
					t.Errorf("connect() control = %v, want nil beside an error", ctl)
				}
				if want := "; it is left running: cleanup agent: the daemon was not ready within 100ms"; !strings.Contains(err.Error(), want) {
					t.Errorf("connect() error = %q, want it to contain %q", err, want)
				}
			} else if info, err := ctl.Hello(context.Background()); err != nil || info != current {
				t.Errorf("returned control answers %+v, %v; want the daemon that started first, %+v", info, err, current)
			}
			events, _ := d.recorded()
			if len(events) <= len(tt.wantProbes) || !slices.Equal(events[:len(tt.wantProbes)], tt.wantProbes) {
				t.Fatalf("events = %q, want %q and then readiness hellos", events, tt.wantProbes)
			}
			for _, e := range events[len(tt.wantProbes):] {
				if e != "hello" {
					t.Errorf("event %q after the daemon took the serve lock, want only readiness hellos", e)
				}
			}
			if got := d.count("apply"); got != 0 {
				t.Errorf("apply ran %d times, want 0: the LaunchAgent must not be applied over the daemon that started first", got)
			}
			if got := d.count("shutdown"); got != tt.wantShutdowns {
				t.Errorf("shutdown ran %d times, want %d", got, tt.wantShutdowns)
			}
			if _, err := os.Lstat(s.Layout.ProgramPath()); !errors.Is(err, fs.ErrNotExist) {
				t.Errorf("program copy stat error = %v, want nothing installed under the daemon that started first", err)
			}
		})
	}
}

func TestConnectStopsOnlyTheOutdatedDaemonItObserved(t *testing.T) {
	current := cleanup.Info{Version: "v1.2.3", Protocol: cleanup.Protocol, PID: 42}
	older := cleanup.Info{Version: "v1.2.2", Protocol: cleanup.Protocol, PID: 41}
	oldest := cleanup.Info{Version: "v1.2.1", Protocol: cleanup.Protocol, PID: 39}
	reused := cleanup.Info{Version: "v1.2.3", Protocol: cleanup.Protocol, PID: 41}
	errStart := errors.New("sysctl kern.proc.pid: input/output error")
	olderBorn, _ := birth(older.PID)
	tests := []struct {
		name          string
		observedStart time.Time
		kernelPID     int
		foreignUID    bool
		recordErr     error
		successor     *cleanup.Info
		reborn        time.Time
		verifyErr     error
		wantEvents    []string
		wantReady     bool
		wantStopped   bool
		wantInfo      cleanup.Info
		wantErrs      []error
		wantText      string
	}{
		{
			name:       "a current daemon that replaces the outdated one after its hello is kept",
			successor:  &current,
			wantEvents: []string{"hello", "hello", "refused 42"},
			wantReady:  true,
			wantInfo:   current,
		},
		{
			name:       "an outdated daemon that replaces the outdated one after its hello is left running",
			successor:  &oldest,
			wantEvents: []string{"hello", "hello", "refused 39"},
			wantReady:  true,
			wantErrs:   []error{errStartedElsewhere, errOutdatedAfterStart},
		},
		{
			name:       "a daemon that reuses the pid after the hello with a later start is refused",
			successor:  &reused,
			reborn:     olderBorn.Add(time.Microsecond),
			wantEvents: []string{"hello", "hello", "refused 41"},
			wantReady:  true,
			wantInfo:   reused,
		},
		{
			name:       "a daemon that reuses the pid after the hello with an earlier start, the clock rolled back, is refused",
			successor:  &reused,
			reborn:     olderBorn.Add(-time.Hour),
			wantEvents: []string{"hello", "hello", "refused 41"},
			wantReady:  true,
			wantInfo:   reused,
		},
		{
			name:       "a socket served under another uid is left running",
			foreignUID: true,
			wantEvents: []string{"hello", "hello", "refused 41"},
			wantReady:  true,
			wantErrs:   []error{errStartedElsewhere, errOutdatedAfterStart},
		},
		{
			name:       "an outdated daemon whose start cannot be read on its hello connection is never asked to stop",
			recordErr:  errStart,
			wantEvents: []string{"hello", "hello"},
			wantErrs:   []error{errUnverified, errStart},
		},
		{
			name:       "a peer whose start cannot be read is sent nothing",
			verifyErr:  errStart,
			wantEvents: []string{"hello", "hello", "refused 41"},
			wantErrs:   []error{errUnverified, errStart},
		},
		{
			name:       "a hello naming a pid other than the kernel's peer is never asked to stop",
			kernelPID:  77,
			wantEvents: []string{"hello", "hello"},
			wantErrs:   []error{errUnverified},
			wantText:   "the hello names pid 41 but the kernel names pid 77",
		},
		{
			name:          "an outdated daemon born in the future, the clock since rolled back, is stopped and replaced",
			observedStart: time.Now().Add(time.Hour),
			wantEvents:    []string{"hello", "hello", "shutdown", "alive 41", "apply"},
			wantReady:     true,
			wantStopped:   true,
			wantInfo:      current,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := &fakeDaemon{
				serving:    &older,
				starts:     &current,
				started:    tt.observedStart,
				kernelPID:  tt.kernelPID,
				foreignUID: tt.foreignUID,
				bornErr:    tt.recordErr,
			}
			d.greeted = func() {
				d.mu.Lock()
				defer d.mu.Unlock()
				if tt.successor != nil {
					d.serving, d.started = tt.successor, tt.reborn
				}
				if tt.verifyErr != nil {
					d.bornErr = tt.verifyErr
				}
			}
			s := fixture(t, d)
			s.Timeout = 100 * time.Millisecond

			ctl, err := s.connect(t.Context())
			for _, want := range tt.wantErrs {
				if !errors.Is(err, want) {
					t.Errorf("connect() error = %v, want it to wrap %v", err, want)
				}
			}
			switch {
			case len(tt.wantErrs) == 0 && err != nil:
				t.Fatalf("connect() error = %v, want the daemon serving the socket", err)
			case len(tt.wantErrs) == 0:
				if info, err := ctl.Hello(context.Background()); err != nil || info != tt.wantInfo {
					t.Errorf("returned control answers %+v, %v; want the daemon serving the socket, %+v", info, err, tt.wantInfo)
				}
			case ctl != nil:
				t.Errorf("connect() control = %v, want nil beside an error", ctl)
			case !strings.Contains(err.Error(), "it is left running"):
				t.Errorf("connect() error = %q, want it to say the daemon is left running", err)
			case !strings.Contains(err.Error(), tt.wantText):
				t.Errorf("connect() error = %q, want it to contain %q", err, tt.wantText)
			}
			events, _ := d.recorded()
			if len(events) < len(tt.wantEvents) || !slices.Equal(events[:len(tt.wantEvents)], tt.wantEvents) {
				t.Fatalf("events = %q, want %q first", events, tt.wantEvents)
			}
			readiness := events[len(tt.wantEvents):]
			if got := len(readiness) > 0; got != tt.wantReady {
				t.Errorf("events after %q = %q, want readiness polled: %v", tt.wantEvents, readiness, tt.wantReady)
			}
			for _, e := range readiness {
				if e != "hello" {
					t.Errorf("event %q after %q, want only readiness hellos", e, tt.wantEvents)
				}
			}
			stops := 0
			if tt.wantStopped {
				stops = 1
			}
			if got := d.count("shutdown"); got != stops {
				t.Errorf("shutdown reached a daemon %d times, want %d: only the observed outdated daemon may be stopped", got, stops)
			}
			if got := d.count("apply"); got != stops {
				t.Errorf("apply ran %d times, want %d: the LaunchAgent is applied only over a daemon this client stopped", got, stops)
			}
			got, err := os.ReadFile(s.Layout.ProgramPath())
			switch {
			case tt.wantStopped && (err != nil || !bytes.Equal(got, sourceBytes)):
				t.Errorf("program copy = %q, %v; want the source bytes installed over the daemon this client stopped", got, err)
			case !tt.wantStopped && !errors.Is(err, fs.ErrNotExist):
				t.Errorf("program copy read error = %v, want nothing installed over a daemon this client did not stop", err)
			}
		})
	}
}

func TestConnectStartLockBusy(t *testing.T) {
	d := &fakeDaemon{starts: &cleanup.Info{Version: "v1.2.3", Protocol: cleanup.Protocol}}
	s := fixture(t, d)
	s.Timeout = 50 * time.Millisecond
	if err := s.Layout.Ensure(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	held, err := durable.AcquireLock(ctx, s.Layout.StartLockPath())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = held.Close() }()

	_, err = s.connect(context.Background())
	if !errors.Is(err, durable.ErrLockBusy) {
		t.Fatalf("connect() error = %v, want %v", err, durable.ErrLockBusy)
	}
	if events, _ := d.recorded(); !reflect.DeepEqual(events, []string{"hello", "held serve.lock"}) {
		t.Errorf("events = %q, want only the first probe", events)
	}
	if _, err := os.Lstat(s.Layout.ProgramPath()); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("program copy stat error = %v, want nothing installed without the lock", err)
	}
}

func TestConnectSerializesOnStartLock(t *testing.T) {
	current := cleanup.Info{Version: "v1.2.3", Protocol: cleanup.Protocol, PID: 42}
	d := &fakeDaemon{
		starts:  &current,
		entered: make(chan struct{}),
		gate:    make(chan struct{}),
		hellos:  make(chan struct{}, 16),
	}
	s := fixture(t, d)
	type result struct {
		ctl cleanup.Control
		err error
	}
	connect := func(out chan<- result) {
		ctl, err := s.connect(context.Background())
		out <- result{ctl, err}
	}

	first := make(chan result, 1)
	go connect(first)
	receive(t, d.entered, "the first Connect to reach apply")
	for len(d.hellos) > 0 {
		<-d.hellos
	}
	second := make(chan result, 1)
	go connect(second)
	receive(t, d.hellos, "the second Connect to find no daemon")
	close(d.gate)

	for name, ch := range map[string]chan result{"first": first, "second": second} {
		r := receive(t, ch, "the "+name+" Connect to return")
		if r.err != nil {
			t.Fatalf("%s connect() error = %v", name, r.err)
		}
		if info, err := r.ctl.Hello(context.Background()); err != nil || info != current {
			t.Errorf("%s control answers %+v, %v; want %+v", name, info, err, current)
		}
	}
	if got := d.count("apply"); got != 1 {
		t.Errorf("apply ran %d times, want 1: the second Connect must find the daemon the first started", got)
	}
	if got := d.count("shutdown"); got != 0 {
		t.Errorf("shutdown ran %d times, want 0", got)
	}
}

func TestConnectRefusesAnUnresponsiveDaemon(t *testing.T) {
	tests := []struct {
		name       string
		helloErr   error
		hangs      bool
		lockHeld   bool
		wantEvents []string
		wantText   string
	}{
		{name: "silent daemon", hangs: true, wantEvents: []string{"hello", "hello"}, wantText: "sent no hello within 50ms"},
		{name: "closing daemon", helloErr: errClosing, wantEvents: []string{"hello", "hello"}, wantText: "closed the connection without a hello"},
		{name: "denied socket", helloErr: errDenied, wantEvents: []string{"hello", "hello"}, wantText: "permission denied"},
		{
			name:       "refused socket while a daemon holds the serve lock",
			lockHeld:   true,
			wantEvents: []string{"hello", "held serve.lock", "hello", "held serve.lock"},
			wantText:   "but is not accepting connections on",
		},
		{
			name:       "missing socket while a daemon holds the serve lock",
			helloErr:   errMissing,
			lockHeld:   true,
			wantEvents: []string{"hello", "held serve.lock", "hello", "held serve.lock"},
			wantText:   "but is not accepting connections on",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := &fakeDaemon{
				starts:   &cleanup.Info{Version: "v1.2.3", Protocol: cleanup.Protocol, PID: 42},
				helloErr: tt.helloErr,
				hangs:    tt.hangs,
				lockHeld: tt.lockHeld,
			}
			s := fixture(t, d)
			s.Timeout = 50 * time.Millisecond

			ctl, err := s.connect(context.Background())
			if !errors.Is(err, errUnresponsive) || ctl != nil {
				t.Fatalf("connect() = %v, %v; want nil and an error wrapping %v", ctl, err, errUnresponsive)
			}
			if !strings.Contains(err.Error(), tt.wantText) {
				t.Errorf("connect() error = %q, want it to contain %q", err, tt.wantText)
			}
			if events, _ := d.recorded(); !reflect.DeepEqual(events, tt.wantEvents) {
				t.Errorf("events = %q, want %q: a second probe under the start lock, then no shutdown and no apply", events, tt.wantEvents)
			}
			if _, err := os.Lstat(s.Layout.ProgramPath()); !errors.Is(err, fs.ErrNotExist) {
				t.Errorf("program copy stat error = %v, want nothing installed", err)
			}
		})
	}
}

func TestReach(t *testing.T) {
	current := cleanup.Info{Version: "v1.2.3", Protocol: cleanup.Protocol, PID: 42}
	older := cleanup.Info{Version: "v1.2.2", Protocol: cleanup.Protocol, PID: 41}
	newer := cleanup.Info{Version: "v1.3.0", Protocol: cleanup.Protocol, PID: 43}
	olderProtocol := cleanup.Info{Version: "v1.2.3", Protocol: cleanup.Protocol - 1, PID: 40}
	newerProtocol := cleanup.Info{Version: "v1.2.3", Protocol: cleanup.Protocol + 1, PID: 44}
	sentinels := []error{errNoDaemon, errUnresponsive, errOlderProtocol, errNewerProtocol}
	greeted := []string{"hello"}
	tests := []struct {
		name       string
		serving    *cleanup.Info
		helloErr   error
		hangs      bool
		lockHeld   bool
		lockErr    error
		cancel     bool
		wantInfo   cleanup.Info
		wantErr    error
		wantText   string
		wantEvents []string
	}{
		{name: "current daemon is returned", serving: &current, wantInfo: current, wantEvents: greeted},
		{name: "older release on this protocol is returned as found", serving: &older, wantInfo: older, wantEvents: greeted},
		{name: "newer release on this protocol is returned as found", serving: &newer, wantInfo: newer, wantEvents: greeted},
		{
			name:       "older protocol is refused naming both",
			serving:    &olderProtocol,
			wantErr:    errOlderProtocol,
			wantText:   fmt.Sprintf("daemon v1.2.3 (pid 40) speaks protocol %d, this ccx %d", cleanup.Protocol-1, cleanup.Protocol),
			wantEvents: greeted,
		},
		{
			name:       "newer protocol is refused naming both",
			serving:    &newerProtocol,
			wantErr:    errNewerProtocol,
			wantText:   fmt.Sprintf("daemon v1.2.3 (pid 44) speaks protocol %d, this ccx %d", cleanup.Protocol+1, cleanup.Protocol),
			wantEvents: greeted,
		},
		{
			name:       "missing socket names what starts the daemon",
			helloErr:   errMissing,
			wantErr:    errNoDaemon,
			wantText:   "such as worktree rm or cleanup pause, installs and starts it",
			wantEvents: []string{"hello", "held serve.lock"},
		},
		{
			name:       "refused socket names what starts the daemon",
			wantErr:    errNoDaemon,
			wantText:   "launchd starts one at login",
			wantEvents: []string{"hello", "held serve.lock"},
		},
		{
			name:       "refused socket while a daemon holds the serve lock is reported",
			lockHeld:   true,
			wantErr:    errUnresponsive,
			wantText:   "but is not accepting connections on",
			wantEvents: []string{"hello", "held serve.lock"},
		},
		{
			name:       "missing socket while a daemon holds the serve lock is reported",
			helloErr:   errMissing,
			lockHeld:   true,
			wantErr:    errUnresponsive,
			wantText:   "but is not accepting connections on",
			wantEvents: []string{"hello", "held serve.lock"},
		},
		{
			name:       "unreadable serve lock is no verdict on the daemon",
			lockErr:    errLockQuery,
			wantErr:    errLockQuery,
			wantText:   "connect to sock: connection refused; cleanup agent: open the serve lock: permission denied",
			wantEvents: []string{"hello", "held serve.lock"},
		},
		{name: "silent daemon is reported with the bound", hangs: true, wantErr: errUnresponsive, wantText: "sent no hello within 50ms", wantEvents: greeted},
		{name: "closing daemon is reported", helloErr: errClosing, wantErr: errUnresponsive, wantText: "closed the connection without a hello", wantEvents: greeted},
		{name: "denied socket is reported", helloErr: errDenied, wantErr: errUnresponsive, wantText: "permission denied", wantEvents: greeted},
		{name: "caller cancellation is no verdict on the daemon", hangs: true, cancel: true, wantErr: context.Canceled, wantText: "context canceled", wantEvents: greeted},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := &fakeDaemon{serving: tt.serving, helloErr: tt.helloErr, hangs: tt.hangs, lockHeld: tt.lockHeld, lockErr: tt.lockErr}
			s := fixture(t, d)
			s.Timeout = 50 * time.Millisecond
			socket, err := s.Layout.Socket()
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if tt.cancel {
				cancel()
			}

			ctl, err := s.reach(ctx)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("reach() error = %v, want %v", err, tt.wantErr)
			}
			for _, sentinel := range sentinels {
				if got, want := errors.Is(err, sentinel), errors.Is(tt.wantErr, sentinel); got != want {
					t.Errorf("errors.Is(%v, %v) = %v, want %v", err, sentinel, got, want)
				}
			}
			events, sockets := d.recorded()
			if !reflect.DeepEqual(events, tt.wantEvents) {
				t.Errorf("events = %q, want %q and nothing else", events, tt.wantEvents)
			}
			if !reflect.DeepEqual(sockets, []string{socket}) {
				t.Errorf("dialed %q, want %q once", sockets, socket)
			}
			if tt.wantErr == nil {
				info, err := ctl.Hello(context.Background())
				if err != nil || info != tt.wantInfo {
					t.Errorf("returned control answers %+v, %v; want %+v", info, err, tt.wantInfo)
				}
			} else {
				if ctl != nil {
					t.Errorf("reach() control = %v, want nil beside an error", ctl)
				}
				if !strings.Contains(err.Error(), tt.wantText) {
					t.Errorf("reach() error = %q, want it to contain %q", err, tt.wantText)
				}
			}
			if (errors.Is(err, errNoDaemon) || errors.Is(err, errUnresponsive)) && !strings.Contains(err.Error(), socket) {
				t.Errorf("reach() error = %q, want it to name the socket %s", err, socket)
			}
			if lock := s.Layout.ServeLockPath(); tt.lockHeld && !strings.Contains(err.Error(), "a daemon holds the serve lock "+lock) {
				t.Errorf("reach() error = %q, want it to name the held serve lock %s", err, lock)
			}
			if _, err := os.Stat(s.Layout.Root); !errors.Is(err, fs.ErrNotExist) {
				t.Errorf("layout root stat error = %v, want it never created", err)
			}
		})
	}
}
