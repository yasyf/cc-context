//go:build windows

package codeexec

import (
	"os/exec"
	"syscall"
)

func configureProbeCommand(cmd *exec.Cmd) {
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
}
