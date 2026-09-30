package agent

import (
	"context"
	"syscall"

	"github.com/yasyf/daemonkit/launchd"

	"github.com/yasyf/cc-context/internal/cleanup"
)

const servicePath = "/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin"

// Connect returns a control for a serving daemon at least as new as this
// client. When none answers, or the one answering is Outdated, it refreshes
// the program copy, stops the outdated daemon and waits for its process to
// exit, and applies the LaunchAgent, all under the start lock; a daemon
// speaking a newer protocol is refused rather than downgraded.
func Connect(ctx context.Context, o Options) (cleanup.Control, error) {
	spec := Spec(o.Layout)
	s := starter{
		Options: o,
		apply:   func(ctx context.Context) error { return launchd.Apply(ctx, o.Launchctl, spec) },
		alive:   processAlive,
	}
	return s.connect(ctx)
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
		ProcessType:   launchd.ProcessTypeBackground,
	}
}

func processAlive(pid int) bool {
	return syscall.Kill(pid, 0) == nil
}
