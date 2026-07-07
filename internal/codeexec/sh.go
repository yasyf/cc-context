package codeexec

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
)

// Sh returns a host function that runs a shell command and returns its combined
// output. This is a host-trust escape hatch, NOT a sandbox boundary: the command
// runs with the caller's privileges, exactly like the Bash tool. The monty
// sandbox bounds only the Python orchestration around it. A non-zero exit is not
// an error — the output (including stderr) is returned so the script can inspect
// it, matching how the model reads Bash output.
func Sh() HostFunc {
	return func(ctx context.Context, call Call) (any, error) {
		cmd := parse(call).str("cmd", 0)
		if cmd == "" {
			return nil, fmt.Errorf("sh: empty command")
		}
		if err := shPolicy(cmd); err != nil {
			return nil, err
		}
		out, err := exec.CommandContext(ctx, "/bin/sh", "-c", cmd).CombinedOutput() //nolint:gosec // sh is a deliberate host-trust escape hatch: the script's command runs with the caller's privileges, exactly like the Bash tool
		if err != nil {
			var exit *exec.ExitError
			if errors.As(err, &exit) {
				return string(out), nil
			}
			return nil, fmt.Errorf("sh: %w", err)
		}
		return string(out), nil
	}
}

// shDenied blocks a small set of catastrophic commands. It is a token guard, not
// real isolation — sh runs with the caller's privileges.
var shDenied = regexp.MustCompile(`(?i)\brm\s+-rf?\s+/|\bmkfs\b|\bdd\s+if=|:\s*\(\s*\)\s*\{|>\s*/dev/sd`)

func shPolicy(cmd string) error {
	if shDenied.MatchString(cmd) {
		return fmt.Errorf("sh: command blocked by policy: %q", cmd)
	}
	return nil
}
