package cleanup

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/yasyf/daemonkit/paths"
)

const (
	// AgentName names the daemon's state directory under ~/.daemonkit/a.
	AgentName = "ccx-cleanup"
	// Label is the daemon's launchd label.
	Label = "com.yasyf.ccx-cleanup"

	recordName     = "job.json"
	registeredName = "registered"
	// PayloadName is the entry of a job folder holding the relocated tree once
	// git no longer finds it at the registered path.
	PayloadName = "payload"

	sunPathBytes = 104
)

// Layout is the daemon's private state tree, rooted at one 0700 directory. The
// job folders under it are where relocated worktrees wait for deletion, so the
// root must sit on the volume those worktrees live on and outside every
// watched tree.
type Layout struct {
	Root string
}

// DefaultLayout is the per-user layout at ~/.daemonkit/a/ccx-cleanup, the home
// resolved through the passwd database rather than the caller's environment.
func DefaultLayout() Layout {
	return Layout{Root: paths.Agent(AgentName).StateDir()}
}

// JobsDir is the parent of every job folder.
func (l Layout) JobsDir() string { return filepath.Join(l.Root, "jobs") }

// JobDir is one job's private folder.
func (l Layout) JobDir(id string) string { return filepath.Join(l.JobsDir(), id) }

// RecordPath is one job's durable record.
func (l Layout) RecordPath(id string) string { return filepath.Join(l.JobDir(id), recordName) }

// Registered is the private path git registers a relocated worktree at.
func (l Layout) Registered(id string) string { return filepath.Join(l.JobDir(id), registeredName) }

// Payload is where a relocated worktree waits for deletion.
func (l Layout) Payload(id string) string { return filepath.Join(l.JobDir(id), PayloadName) }

// Socket is the daemon's control socket, refused when the kernel's sun_path
// would truncate it.
func (l Layout) Socket() (string, error) {
	socket := filepath.Join(l.Root, "daemon.sock")
	if len(socket) >= sunPathBytes {
		return "", fmt.Errorf("cleanup: socket path is %d bytes; sun_path fits %d: %q", len(socket), sunPathBytes-1, socket)
	}
	return socket, nil
}

// ServeLockPath is the lock one serving daemon holds for its whole life.
func (l Layout) ServeLockPath() string { return filepath.Join(l.Root, "locks", "serve.lock") }

// StartLockPath serializes the clients installing or restarting the daemon.
func (l Layout) StartLockPath() string { return filepath.Join(l.Root, "locks", "start.lock") }

// ProgramPath is the daemon-owned regular-file copy of the executable launchd
// runs, so the path a package manager installed and later removes is never
// the one a relaunch depends on.
func (l Layout) ProgramPath() string { return filepath.Join(l.Root, "bin", "ccx") }

// LogPath is where launchd points the daemon's output.
func (l Layout) LogPath() string { return filepath.Join(l.Root, "daemon.log") }

// SwitchPath is the durable queue-wide pause switch.
func (l Layout) SwitchPath() string { return filepath.Join(l.Root, "switch.json") }

// Ensure creates the layout's directories and makes the root private.
func (l Layout) Ensure() error {
	if err := l.checkRoot(); err != nil {
		return err
	}
	for _, dir := range []string{l.Root, l.JobsDir(), filepath.Dir(l.ServeLockPath()), filepath.Dir(l.ProgramPath())} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("cleanup: create %s: %w", dir, err)
		}
	}
	if err := os.Chmod(l.Root, 0o700); err != nil {
		return fmt.Errorf("cleanup: make %s private: %w", l.Root, err)
	}
	return nil
}

func (l Layout) checkRoot() error {
	if !filepath.IsAbs(l.Root) || filepath.Clean(l.Root) != l.Root {
		return fmt.Errorf("cleanup: layout root %q is not a clean absolute path", l.Root)
	}
	return nil
}
