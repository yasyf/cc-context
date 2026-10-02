// Package agent keeps one current cleanup daemon serving for the user: it
// installs the daemon's own copy of the executable and its LaunchAgent,
// replaces a daemon older than the client asking, and never downgrades one. It
// installs and applies only while holding the daemon's serve lock, so it never
// does either over a daemon that started first.
// Reach only observes: it greets whichever daemon serves the socket and never
// installs, stops, or starts one. A socket that refuses or is missing counts as
// no daemon only while no process holds the daemon's serve lock.
package agent

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"regexp"
	"slices"
	"strconv"
	"syscall"
	"time"

	"github.com/yasyf/daemonkit/durable"

	"github.com/yasyf/cc-context/internal/cleanup"
)

const (
	defaultTimeout = 15 * time.Second
	claimWait      = time.Second
	pollFloor      = 10 * time.Millisecond
	pollCeiling    = 250 * time.Millisecond
)

var (
	errNewerProtocol      = errors.New("cleanup agent: the daemon speaks a newer protocol; upgrade ccx")
	errOlderProtocol      = errors.New("cleanup agent: the daemon speaks an older protocol; the next queue-changing command replaces it")
	errOutdatedAfterStart = errors.New("cleanup agent: an outdated daemon answered after the restart")
	errNoDaemon           = errors.New("cleanup agent: no cleanup daemon accepts connections")
	errUnresponsive       = errors.New("cleanup agent: the cleanup daemon did not answer")
	errStartedElsewhere   = errors.New("cleanup agent: another daemon started first")
	errUnverified         = errors.New("cleanup agent: the outdated daemon's process could not be verified; it is left running")

	releasePattern = regexp.MustCompile(`^v?(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)
)

// Options is what Connect needs to reach, install, or replace the daemon.
type Options struct {
	Layout cleanup.Layout
	// Version is the client's own; a daemon Outdated against it is replaced.
	Version string
	// Source is the client's own executable with symlinks resolved: the bytes
	// the daemon's program copy is refreshed from.
	Source string
	// Dial opens a control connection to a daemon socket.
	Dial func(socket string) cleanup.Control
	// Launchctl runs /bin/launchctl on behalf of launchd.Apply.
	Launchctl func(ctx context.Context, path string, args ...string) (output string, code int, err error)
	// Timeout bounds each wait: a hello, the start lock, an outdated daemon's
	// shutdown and exit, applying the LaunchAgent, and readiness after a
	// start. Zero is 15s. It must stay under cleanup.ServeLockWait: the daemon
	// an apply launches waits that long for the serve lock this client holds
	// until the apply returns.
	Timeout time.Duration
}

type standing int

const (
	absent standing = iota
	current
	stale
)

type starter struct {
	Options
	apply func(ctx context.Context) error
	alive func(pid int) bool
	held  func(lock string) (bool, error)
	born  func(pid int) (time.Time, error)
}

// Outdated reports whether a client at version client replaces a daemon at
// version daemon: only when the client is the newer release if both are clean
// vMAJOR.MINOR.PATCH semver, and on any difference when either is a dev,
// dirty, or pseudo-version build.
func Outdated(daemon, client string) bool {
	d, dok := release(daemon)
	c, cok := release(client)
	if !dok || !cok {
		return daemon != client
	}
	return slices.Compare(c, d) > 0
}

func release(version string) ([]uint64, bool) {
	match := releasePattern.FindStringSubmatch(version)
	if match == nil {
		return nil, false
	}
	parts := make([]uint64, 0, len(match)-1)
	for _, digits := range match[1:] {
		n, err := strconv.ParseUint(digits, 10, 64)
		if err != nil {
			return nil, false
		}
		parts = append(parts, n)
	}
	return parts, true
}

// InstallProgram refreshes the daemon-owned copy of source at
// layout.ProgramPath, reporting whether it wrote. The copy is always a regular
// 0700 file with a single link, published by rename, so a running daemon keeps
// the inode it started from, only the next launch runs the new bytes, and no
// write through another pathname can reach it.
func InstallProgram(layout cleanup.Layout, source string) (changed bool, err error) {
	if err := layout.Ensure(); err != nil {
		return false, err
	}
	src, err := os.Open(source) //nolint:gosec // source is the caller's own resolved executable
	if err != nil {
		return false, fmt.Errorf("cleanup agent: open program source: %w", err)
	}
	defer func() { _ = src.Close() }()
	info, err := src.Stat()
	if err != nil {
		return false, fmt.Errorf("cleanup agent: inspect program source: %w", err)
	}
	if !info.Mode().IsRegular() {
		return false, fmt.Errorf("cleanup agent: program source %s is not a regular file", source)
	}
	want, err := digest(src)
	if err != nil {
		return false, fmt.Errorf("cleanup agent: hash program source: %w", err)
	}
	program := layout.ProgramPath()
	same, err := installed(program, want)
	if err != nil || same {
		return false, err
	}
	if _, err := src.Seek(0, io.SeekStart); err != nil {
		return false, fmt.Errorf("cleanup agent: rewind program source: %w", err)
	}
	w, err := durable.Create(program, 0o700)
	if err != nil {
		return false, fmt.Errorf("cleanup agent: stage program copy: %w", err)
	}
	defer func() { _ = w.Close() }()
	if _, err := io.Copy(w, src); err != nil {
		return false, fmt.Errorf("cleanup agent: copy program: %w", err)
	}
	if err := w.Commit(); err != nil {
		return false, fmt.Errorf("cleanup agent: publish program copy: %w", err)
	}
	slog.Info("cleanup agent: installed the daemon program", "path", program, "source", source)
	return true, nil
}

func installed(program string, want [sha256.Size]byte) (bool, error) {
	info, err := os.Lstat(program)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("cleanup agent: inspect program copy: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o700 || info.Sys().(*syscall.Stat_t).Nlink != 1 {
		return false, nil
	}
	f, err := os.Open(program) //nolint:gosec // program is the daemon-owned copy inside the private layout
	if err != nil {
		return false, fmt.Errorf("cleanup agent: open program copy: %w", err)
	}
	defer func() { _ = f.Close() }()
	got, err := digest(f)
	if err != nil {
		return false, fmt.Errorf("cleanup agent: hash program copy: %w", err)
	}
	return got == want, nil
}

func digest(r io.Reader) ([sha256.Size]byte, error) {
	h := sha256.New()
	if _, err := io.Copy(h, r); err != nil {
		return [sha256.Size]byte{}, err
	}
	return [sha256.Size]byte(h.Sum(nil)), nil
}

func (s starter) reach(ctx context.Context) (cleanup.Control, error) {
	socket, err := s.Layout.Socket()
	if err != nil {
		return nil, err
	}
	ctl, info, err := s.greet(ctx, socket)
	switch {
	case errors.Is(err, errNoDaemon):
		return nil, fmt.Errorf("%w; launchd starts one at login, and the next ccx command that queues or steers deletion, such as worktree rm or cleanup pause, installs and starts it", err)
	case err != nil:
		return nil, err
	case info.Protocol > cleanup.Protocol:
		return nil, fmt.Errorf("%w: daemon %s (pid %d) speaks protocol %d, this ccx %d", errNewerProtocol, info.Version, info.PID, info.Protocol, cleanup.Protocol)
	case info.Protocol < cleanup.Protocol:
		return nil, fmt.Errorf("%w: daemon %s (pid %d) speaks protocol %d, this ccx %d", errOlderProtocol, info.Version, info.PID, info.Protocol, cleanup.Protocol)
	}
	return ctl, nil
}

func (s starter) greet(ctx context.Context, socket string) (cleanup.Control, cleanup.Info, error) {
	bounded, cancel := context.WithTimeout(ctx, s.timeout())
	defer cancel()
	ctl := s.Dial(socket)
	info, err := ctl.Hello(bounded)
	switch {
	case err == nil:
		return ctl, info, nil
	case ctx.Err() != nil:
		return nil, cleanup.Info{}, fmt.Errorf("cleanup agent: hello to %s: %w", socket, ctx.Err())
	case errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ECONNREFUSED):
		return nil, cleanup.Info{}, s.vacant(socket, err)
	case bounded.Err() != nil:
		return nil, cleanup.Info{}, fmt.Errorf("%w: %s sent no hello within %s", errUnresponsive, socket, s.timeout())
	case errors.Is(err, io.EOF) || errors.Is(err, syscall.EPIPE) || errors.Is(err, syscall.ECONNRESET):
		return nil, cleanup.Info{}, fmt.Errorf("%w: %s closed the connection without a hello; a daemon told to shut down refuses requests until it exits: %w", errUnresponsive, socket, err)
	}
	return nil, cleanup.Info{}, fmt.Errorf("%w: hello to %s: %w", errUnresponsive, socket, err)
}

func (s starter) vacant(socket string, refused error) error {
	lock := s.Layout.ServeLockPath()
	held, err := s.held(lock)
	switch {
	case err != nil:
		return fmt.Errorf("cleanup agent: hello to %s: %w; %w", socket, refused, err)
	case held:
		return fmt.Errorf("%w: a daemon holds the serve lock %s but is not accepting connections on %s: %w", errUnresponsive, lock, socket, refused)
	}
	return fmt.Errorf("%w on %s: %w", errNoDaemon, socket, refused)
}

func (s starter) connect(ctx context.Context) (cleanup.Control, error) {
	socket, err := s.Layout.Socket()
	if err != nil {
		return nil, err
	}
	ctl, _, found, err := s.probe(ctx, socket)
	switch {
	case errors.Is(err, errUnresponsive):
		slog.Debug("cleanup agent: the daemon did not answer; probing again under the start lock", "socket", socket, "err", err)
	case err != nil || found == current:
		return ctl, err
	}
	if err := s.Layout.Ensure(); err != nil {
		return nil, err
	}
	lock, err := s.lock(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = lock.Close() }()
	observed := time.Now()
	ctl, info, found, err := s.probe(ctx, socket)
	if err != nil || found == current {
		return ctl, err
	}
	if found == stale {
		err = s.shutdown(ctx, ctl, info.PID, observed)
		switch {
		case errors.Is(err, errStartedElsewhere):
			return s.leave(ctx, socket, err)
		case err != nil:
			return nil, err
		}
	}
	err = s.launch(ctx)
	switch {
	case errors.Is(err, errStartedElsewhere):
		return s.leave(ctx, socket, err)
	case err != nil:
		return nil, err
	}
	return s.ready(ctx, socket)
}

func (s starter) leave(ctx context.Context, socket string, cause error) (cleanup.Control, error) {
	slog.Info("cleanup agent: leaving the daemon that started first to come up", "err", cause)
	ready, err := s.ready(ctx, socket)
	if err != nil {
		return nil, fmt.Errorf("%w; it is left running: %w", cause, err)
	}
	return ready, nil
}

func (s starter) launch(ctx context.Context) error {
	lock, err := s.claim(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = lock.Close() }()
	if _, err := InstallProgram(s.Layout, s.Source); err != nil {
		return err
	}
	return s.start(ctx)
}

func (s starter) claim(ctx context.Context) (*durable.Lock, error) {
	path := s.Layout.ServeLockPath()
	bounded, cancel := context.WithTimeout(ctx, claimWait)
	defer cancel()
	lock, err := durable.AcquireLock(bounded, path)
	switch {
	case err == nil:
		return lock, nil
	case ctx.Err() != nil:
		return nil, fmt.Errorf("cleanup agent: take the serve lock %s: %w", path, ctx.Err())
	case errors.Is(err, durable.ErrLockBusy):
		return nil, fmt.Errorf("%w: it holds the serve lock %s", errStartedElsewhere, path)
	}
	return nil, fmt.Errorf("cleanup agent: take the serve lock %s: %w", path, err)
}

func (s starter) probe(ctx context.Context, socket string) (cleanup.Control, cleanup.Info, standing, error) {
	ctl, info, err := s.greet(ctx, socket)
	switch {
	case errors.Is(err, errNoDaemon):
		slog.Debug("cleanup agent: no daemon answered", "socket", socket, "err", err)
		return nil, cleanup.Info{}, absent, nil
	case err != nil:
		return nil, cleanup.Info{}, absent, err
	}
	found, err := s.judge(info)
	if err != nil {
		return nil, cleanup.Info{}, absent, err
	}
	return ctl, info, found, nil
}

func (s starter) judge(info cleanup.Info) (standing, error) {
	switch {
	case info.Protocol > cleanup.Protocol:
		return absent, fmt.Errorf("%w: the daemon speaks protocol %d, this ccx %d", errNewerProtocol, info.Protocol, cleanup.Protocol)
	case info.Protocol < cleanup.Protocol || Outdated(info.Version, s.Version):
		return stale, nil
	}
	return current, nil
}

func (s starter) lock(ctx context.Context) (*durable.Lock, error) {
	ctx, cancel := context.WithTimeout(ctx, s.timeout())
	defer cancel()
	lock, err := durable.AcquireLock(ctx, s.Layout.StartLockPath())
	if err != nil {
		return nil, fmt.Errorf("cleanup agent: take the start lock: %w", err)
	}
	return lock, nil
}

func (s starter) shutdown(ctx context.Context, ctl cleanup.Control, pid int, observed time.Time) error {
	start, err := s.born(pid)
	switch {
	case err != nil:
		return fmt.Errorf("%w: pid %d: %w", errUnverified, pid, err)
	case !start.Before(observed):
		return fmt.Errorf("%w: pid %d started %s, after this client asked the outdated daemon for its hello at %s", errStartedElsewhere, pid, start.Format(time.RFC3339Nano), observed.Format(time.RFC3339Nano))
	}
	ctx, cancel := context.WithTimeout(ctx, s.timeout())
	defer cancel()
	slog.Info("cleanup agent: stopping an outdated daemon", "client", s.Version, "pid", pid)
	err = ctl.Shutdown(ctx, s.verifier(pid, start))
	switch {
	case errors.Is(err, cleanup.ErrUnidentifiedPeer):
		return fmt.Errorf("%w: pid %d: %w", errUnverified, pid, err)
	case err != nil:
		return fmt.Errorf("cleanup agent: stop the outdated daemon: %w", err)
	}
	for delay := pollFloor; s.alive(pid); delay = min(2*delay, pollCeiling) {
		select {
		case <-ctx.Done():
			return fmt.Errorf("cleanup agent: the outdated daemon (pid %d) did not exit within %s: %w", pid, s.timeout(), ctx.Err())
		case <-time.After(delay):
		}
	}
	return nil
}

func (s starter) verifier(pid int, start time.Time) func(cleanup.Peer) error {
	return func(peer cleanup.Peer) error {
		if peer.PID != pid || peer.UID != os.Getuid() {
			return fmt.Errorf("%w: pid %d (uid %d) serves the socket, not the outdated daemon (pid %d, uid %d)", errStartedElsewhere, peer.PID, peer.UID, pid, os.Getuid())
		}
		got, err := s.born(peer.PID)
		switch {
		case err != nil:
			return fmt.Errorf("%w: pid %d: %w", errUnverified, peer.PID, err)
		case !got.Equal(start):
			return fmt.Errorf("%w: pid %d started %s serves the socket, not the outdated daemon started %s", errStartedElsewhere, peer.PID, got.Format(time.RFC3339Nano), start.Format(time.RFC3339Nano))
		}
		return nil
	}
}

func (s starter) start(ctx context.Context) error {
	bounded, cancel := context.WithTimeout(ctx, s.timeout())
	defer cancel()
	err := s.apply(bounded)
	switch {
	case err == nil:
		return nil
	case ctx.Err() != nil:
		return fmt.Errorf("cleanup agent: start the daemon: %w", ctx.Err())
	case bounded.Err() != nil:
		return fmt.Errorf("cleanup agent: start the daemon: launchd did not finish within %s: %w", s.timeout(), err)
	}
	return fmt.Errorf("cleanup agent: start the daemon: %w", err)
}

func (s starter) ready(ctx context.Context, socket string) (cleanup.Control, error) {
	ctx, cancel := context.WithTimeout(ctx, s.timeout())
	defer cancel()
	for delay := pollFloor; ; delay = min(2*delay, pollCeiling) {
		ctl := s.Dial(socket)
		info, err := ctl.Hello(ctx)
		if err == nil {
			found, err := s.judge(info)
			switch {
			case err != nil:
				return nil, err
			case found == stale:
				return nil, fmt.Errorf("%w: version %s protocol %d, want %s protocol %d", errOutdatedAfterStart, info.Version, info.Protocol, s.Version, cleanup.Protocol)
			}
			return ctl, nil
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("cleanup agent: the daemon was not ready within %s: %w", s.timeout(), err)
		case <-time.After(delay):
		}
	}
}

func (s starter) timeout() time.Duration {
	if s.Timeout == 0 {
		return defaultTimeout
	}
	return s.Timeout
}
