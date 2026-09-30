// Package cleanupwatch censuses the file watchers pinned to worktrees — Watchman
// roots and Git's builtin fsmonitor daemons — and retires exactly the ones an
// authorized worktree removal owns. It runs on demand only: every census and
// every retirement gate is taken fresh, and nothing here scans in the background.
package cleanupwatch

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/yasyf/cc-context/internal/lookpath"
)

var (
	// ErrRefused marks a retirement or recreation the fresh ownership gate did
	// not permit; the wrapped detail names every blocker.
	ErrRefused = errors.New("watcher retirement refused")
	// ErrNoGuard marks a mutation attempted without the caller's activity
	// guard. A missing guard never authorizes a mutation.
	ErrNoGuard = errors.New("no activity guard: watcher mutation not authorized")
	// ErrWatched marks a quarantine directory a live watcher still observes.
	ErrWatched = errors.New("directory is watched")
)

// DefaultProtected names the consumers whose presence always blocks a
// retirement or recreation, matched as substrings of a client's command line.
var DefaultProtected = []string{"relay", "watchman-make", "tilt"}

// Runner runs one external command to completion and returns its stdout and
// pid. A nonzero exit is an [*ExitError]; a missing binary wraps
// [exec.ErrNotFound].
type Runner interface {
	Run(ctx context.Context, dir, name string, args ...string) (Output, error)
}

// Output is a finished command's stdout and the pid it ran as.
type Output struct {
	Stdout []byte
	PID    int
}

// ExitError is a command that ran and exited nonzero.
type ExitError struct {
	Command string
	Code    int
	Stdout  []byte
	Stderr  string
}

func (e *ExitError) Error() string {
	return fmt.Sprintf("%s: exit %d: %s", e.Command, e.Code, strings.TrimSpace(e.Stderr))
}

// Process is one live process as the census sees it. PID, Start, and Command
// together identify one process instance across pid reuse.
type Process struct {
	PID     int      `json:"pid"`
	Start   string   `json:"start"`
	Command string   `json:"command"`
	CWD     string   `json:"cwd,omitempty"`
	Sockets []string `json:"sockets,omitempty"`
	// Unexamined reports a daemon past the process table's bound, listed
	// without its working directory or sockets.
	Unexamined bool `json:"unexamined,omitempty"`
}

// Processes is the process-table boundary: the builtin fsmonitor daemons with
// their bound unix sockets, and the identity of a batch of pids — a pid
// missing from the result has exited.
type Processes interface {
	FSMonitorDaemons(ctx context.Context) ([]Process, error)
	Processes(ctx context.Context, pids []int) (map[int]Process, error)
}

// ActivityGuard is the caller's fresh process, TTY, and service check for one
// exact path; a nil error means inactive. Retire and Recreate call it
// immediately before every mutation.
type ActivityGuard func(ctx context.Context, path string) error

// Deps carries the boundaries and bounds every census, retirement, and
// recreation runs under.
type Deps struct {
	Run       Runner
	Procs     Processes
	Guard     ActivityGuard
	Protected []string
	// MaxRoots bounds how many Watchman roots one census inspects in detail;
	// the rest are listed by path in [Watchman.Unexamined].
	MaxRoots int
	// MaxClients bounds how many distinct Watchman client pids one census
	// looks up; the rest are reported unexamined and block every mutation.
	MaxClients int
	// Timeout bounds each external command.
	Timeout time.Duration
	// SettleTries and SettleInterval bound the wait for a stopped fsmonitor
	// daemon to exit.
	SettleTries    int
	SettleInterval time.Duration
}

// NewDeps returns the production boundaries: exec for commands, ps and lsof
// for the process table, and guard as the activity check.
func NewDeps(guard ActivityGuard) Deps {
	run := ExecRunner{}
	return Deps{
		Run:            run,
		Procs:          ProcTable{Run: run, Timeout: 5 * time.Second, MaxDaemons: 64},
		Guard:          guard,
		Protected:      DefaultProtected,
		MaxRoots:       256,
		MaxClients:     64,
		Timeout:        10 * time.Second,
		SettleTries:    50,
		SettleInterval: 100 * time.Millisecond,
	}
}

// ExecRunner is the production [Runner].
type ExecRunner struct{}

// Run executes name with args in dir under the C locale, resolving name
// against the process PATH.
func (ExecRunner) Run(ctx context.Context, dir, name string, args ...string) (Output, error) {
	cmd := exec.CommandContext(ctx, lookpath.For(os.Environ()).Bin(name), args...) //nolint:gosec // name is one of watchman, git, ps, lsof and args are built by this package
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "LC_ALL=C")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return Output{}, fmt.Errorf("%s: %w", name, err)
	}
	out := Output{PID: cmd.Process.Pid}
	err := cmd.Wait()
	if ctxErr := ctx.Err(); ctxErr != nil {
		return Output{}, fmt.Errorf("%s: %w", name, ctxErr)
	}
	out.Stdout = stdout.Bytes()
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return out, &ExitError{
			Command: name + " " + strings.Join(args, " "),
			Code:    exitErr.ExitCode(),
			Stdout:  stdout.Bytes(),
			Stderr:  stderr.String(),
		}
	}
	if err != nil {
		return Output{}, fmt.Errorf("%s: %w", name, err)
	}
	return out, nil
}

func (d Deps) run(ctx context.Context, name string, args ...string) ([]byte, error) {
	out, err := d.runPID(ctx, name, args...)
	return out.Stdout, err
}

func (d Deps) runPID(ctx context.Context, name string, args ...string) (Output, error) {
	ctx, cancel := context.WithTimeout(ctx, d.Timeout)
	defer cancel()
	return d.Run.Run(ctx, "", name, args...)
}

func (d Deps) guard(ctx context.Context, path string) error {
	if err := d.Guard(ctx, path); err != nil {
		return fmt.Errorf("%w: activity guard for %s: %w", ErrRefused, path, err)
	}
	return nil
}

func (d Deps) protected(command string) bool {
	for _, p := range d.Protected {
		if strings.Contains(command, p) {
			return true
		}
	}
	return false
}

func refused(blockers []string) error {
	return fmt.Errorf("%w: %s", ErrRefused, strings.Join(blockers, "; "))
}
