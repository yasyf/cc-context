//go:build !windows

package codeexec

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

func configureProbeCommand(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// Mirrors the web drivers' process hygiene (internal/web): SIGKILL the
	// whole group at cancel, while the unreaped leader still pins the pgid —
	// a TERM-then-timer grace can outlive the CLI process, leak TERM-ignoring
	// descendants, or SIGKILL a recycled pgid after Wait reaps the leader.
	cmd.Cancel = func() error {
		if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
			if errors.Is(err, syscall.ESRCH) {
				return os.ErrProcessDone
			}
			return err
		}
		return nil
	}
}
