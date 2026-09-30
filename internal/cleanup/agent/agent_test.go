package agent

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/yasyf/daemonkit/durable"

	"github.com/yasyf/cc-context/internal/cleanup"
)

var (
	errRefused   = errors.New("connection refused")
	errBootstrap = errors.New("bootstrap failed")

	sourceBytes = []byte("#!/bin/sh\nexec true\n")
)

type fakeDaemon struct {
	mu       sync.Mutex
	serving  *cleanup.Info
	starts   *cleanup.Info
	applyErr error
	lingers  int
	exiting  int
	events   []string
	sockets  []string
	entered  chan struct{}
	enter    sync.Once
	gate     chan struct{}
	hellos   chan struct{}
}

type fakeControl struct {
	cleanup.Service
	d *fakeDaemon
}

func (c fakeControl) Hello(context.Context) (cleanup.Info, error) {
	c.d.mu.Lock()
	c.d.events = append(c.d.events, "hello")
	serving := c.d.serving
	c.d.mu.Unlock()
	if c.d.hellos != nil {
		c.d.hellos <- struct{}{}
	}
	if serving == nil {
		return cleanup.Info{}, errRefused
	}
	return *serving, nil
}

func (c fakeControl) Shutdown(context.Context) error {
	c.d.mu.Lock()
	defer c.d.mu.Unlock()
	c.d.events = append(c.d.events, "shutdown")
	c.d.serving = nil
	c.d.exiting = c.d.lingers
	return nil
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

func (d *fakeDaemon) apply(context.Context) error {
	d.mu.Lock()
	d.events = append(d.events, "apply")
	d.mu.Unlock()
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
	dir, err := os.MkdirTemp("/tmp", "ccxa")
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
		applyErr      error
		lingers       int
		wantEvents    []string
		wantInfo      cleanup.Info
		wantErr       error
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
			wantEvents:    []string{"hello", "hello", "apply", "hello"},
			wantInfo:      current,
			wantInstalled: true,
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
			wantEvents:    []string{"hello", "hello", "apply", "hello"},
			wantErr:       errOutdatedAfterStart,
			wantInstalled: true,
		},
		{
			name:          "apply failure is returned",
			applyErr:      errBootstrap,
			wantEvents:    []string{"hello", "hello", "apply"},
			wantErr:       errBootstrap,
			wantInstalled: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := &fakeDaemon{serving: tt.serving, starts: tt.starts, applyErr: tt.applyErr, lingers: tt.lingers}
			s := fixture(t, d)

			ctl, err := s.connect(context.Background())
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("connect() error = %v, want %v", err, tt.wantErr)
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
			case !tt.wantInstalled:
				if _, err := os.Stat(s.Layout.Root); !errors.Is(err, fs.ErrNotExist) {
					t.Errorf("layout root stat error = %v, want it never created", err)
				}
			}
		})
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
	s.Timeout = 100 * time.Millisecond

	ctl, err := s.connect(context.Background())
	if !errors.Is(err, context.DeadlineExceeded) || ctl != nil {
		t.Fatalf("connect() = %v, %v; want nil and an error wrapping %v", ctl, err, context.DeadlineExceeded)
	}
	if got := d.count("apply"); got != 0 {
		t.Errorf("apply ran %d times, want 0 while the outdated daemon still runs", got)
	}
	if got := d.count("alive 41"); got < 2 {
		t.Errorf("the outdated daemon's process was checked %d times, want repeated checks", got)
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
	if events, _ := d.recorded(); !reflect.DeepEqual(events, []string{"hello"}) {
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
