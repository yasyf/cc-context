package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"syscall"
	"time"

	"github.com/yasyf/daemonkit/launchd"

	"github.com/yasyf/cc-context/internal/cleanup"
	"github.com/yasyf/cc-context/internal/cleanup/native"
)

const servicePath = "/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin"

// Connect returns a control for a serving daemon at least as new as this
// client. When no daemon accepts connections, or the one answering is
// Outdated, it stops the outdated daemon and waits for its process to exit,
// then takes the serve lock and, holding it, refreshes the program copy and
// applies the LaunchAgent, all under the start lock. A daemon that takes the
// serve lock first is waited for and never installed or applied over; a daemon
// speaking a newer protocol is refused rather than downgraded.
// A daemon that does not answer, including one still holding the serve lock
// behind a socket that refuses, is probed once more under the start lock and
// reported, never installed or applied over.
func Connect(ctx context.Context, o Options) (cleanup.Control, error) {
	spec := Spec(o.Layout)
	s := starter{
		Options:  o,
		apply:    func(ctx context.Context) error { return launchd.Apply(ctx, o.Launchctl, spec) },
		alive:    processAlive,
		held:     serveLockHeld,
		identify: native.ProcessUniqueID,
	}
	return s.connect(ctx)
}

// Reach returns a control for the daemon serving layout's socket and never
// installs, stops, or starts one: a single hello bounded by timeout decides,
// and a socket that refuses or is missing reads as no daemon only while no
// process holds the serve lock. A daemon of any version on this client's
// protocol is returned as found.
func Reach(ctx context.Context, layout cleanup.Layout, dial func(socket string) cleanup.Control, timeout time.Duration) (cleanup.Control, error) {
	s := starter{Options: Options{Layout: layout, Dial: dial, Timeout: timeout}, held: serveLockHeld}
	return s.reach(ctx)
}

// Spec is the daemon's one LaunchAgent. launchd's RunAtLoad restores it at
// login, and it restarts only after a failed exit, so a daemon told to shut
// down stays down until the next Connect applies it again.
func Spec(layout cleanup.Layout) launchd.Agent {
	return launchd.Agent{
		Label:         cleanup.Label,
		Program:       layout.ProgramPath(),
		Args:          []string{"vcs", "cleanup", "serve"},
		LogPath:       layout.LogPath(),
		Env:           map[string]string{"PATH": servicePath},
		RestartPolicy: launchd.RestartOnFailure,
		ProcessType:   launchd.ProcessTypeStandard,
	}
}

func processAlive(pid int) bool {
	return syscall.Kill(pid, 0) == nil
}

func serveLockHeld(lock string) (bool, error) {
	f, err := os.OpenFile(lock, os.O_RDONLY|syscall.O_NOFOLLOW, 0) //nolint:gosec // lock is the layout's serve lock, opened read-only and never created
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("cleanup agent: open the serve lock: %w", err)
	}
	defer func() { _ = f.Close() }()
	// Darwin's F_GETLK also reports flock(2) locks, the kind durable.AcquireLock
	// takes, so the daemon's lock is observed without ever being contended.
	probe := syscall.Flock_t{Type: syscall.F_WRLCK, Whence: io.SeekStart}
	if err := syscall.FcntlFlock(f.Fd(), syscall.F_GETLK, &probe); err != nil {
		return false, fmt.Errorf("cleanup agent: query the serve lock %s: %w", lock, err)
	}
	return probe.Type != syscall.F_UNLCK, nil
}
