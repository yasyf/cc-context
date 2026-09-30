package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/yasyf/cc-context/internal/cleanup"
	"github.com/yasyf/cc-context/internal/cleanupwatch"
	"github.com/yasyf/cc-context/internal/render"
)

const (
	watchersBudget = 6000
	psStartLayout  = "Mon Jan 2 15:04:05 2006"
)

type cleanupWatchers struct {
	deps cleanupwatch.Deps
}

var _ cleanup.Watchers = cleanupWatchers{}

func newCleanupWatchers(guard cleanup.Guard) cleanupWatchers {
	return cleanupWatchers{deps: cleanupwatch.NewDeps(cleanupwatch.ActivityGuard(guard))}
}

func (w cleanupWatchers) Retiring(ctx context.Context, worktree string) ([]cleanup.ProcessID, error) {
	plan, err := cleanupwatch.Plan(ctx, w.deps, worktree)
	if err != nil {
		return nil, fmt.Errorf("plan the watcher retirement of %s: %w", worktree, err)
	}
	if plan.Refused() {
		return nil, fmt.Errorf("name the watchers of %s: %w: %s", worktree, cleanupwatch.ErrRefused, strings.Join(plan.Blockers, "; "))
	}
	var watchers []cleanupwatch.Process
	if len(plan.Roots) > 0 {
		server, err := w.watchmanServer(ctx)
		if err != nil {
			return nil, fmt.Errorf("name the watchers of %s: %w", worktree, err)
		}
		watchers = append(watchers, server)
	}
	if plan.FSMonitor != nil {
		owner, err := w.process(ctx, plan.FSMonitor.PID)
		if err != nil {
			return nil, fmt.Errorf("name the watchers of %s: %w", worktree, err)
		}
		if owner.Start != plan.FSMonitor.Start {
			return nil, fmt.Errorf("name the watchers of %s: %w: fsmonitor owner pid %d restarted since planning", worktree, cleanupwatch.ErrRefused, owner.PID)
		}
		watchers = append(watchers, owner)
	}
	ids := make([]cleanup.ProcessID, 0, len(watchers))
	for _, proc := range watchers {
		start, err := time.ParseInLocation(psStartLayout, proc.Start, time.Local)
		if err != nil {
			return nil, fmt.Errorf("name the watchers of %s: start of pid %d: %w", worktree, proc.PID, err)
		}
		ids = append(ids, cleanup.ProcessID{PID: proc.PID, Start: start.Unix()})
	}
	slices.SortFunc(ids, func(a, b cleanup.ProcessID) int { return a.PID - b.PID })
	return ids, nil
}

func (w cleanupWatchers) watchmanServer(ctx context.Context) (cleanupwatch.Process, error) {
	sock, err := w.watchman(ctx, "get-sockname")
	if err != nil {
		return cleanupwatch.Process{}, err
	}
	answered, err := w.watchman(ctx, "get-pid")
	if err != nil {
		return cleanupwatch.Process{}, err
	}
	server, err := w.process(ctx, answered.PID)
	if err != nil {
		return cleanupwatch.Process{}, err
	}
	if !w.watchmanServerCommand(server.Command) {
		return cleanupwatch.Process{}, fmt.Errorf("%w: pid %d answering for watchman runs %q, not a watchman server", cleanupwatch.ErrRefused, server.PID, server.Command)
	}
	held, err := w.holdsSocket(ctx, server.PID, sock.Sockname)
	if err != nil {
		return cleanupwatch.Process{}, err
	}
	if !held {
		return cleanupwatch.Process{}, fmt.Errorf("%w: watchman server pid %d does not hold its socket %s", cleanupwatch.ErrRefused, server.PID, sock.Sockname)
	}
	again, err := w.watchman(ctx, "get-pid")
	if err != nil {
		return cleanupwatch.Process{}, err
	}
	now, err := w.process(ctx, server.PID)
	if err != nil {
		return cleanupwatch.Process{}, err
	}
	if again.PID != server.PID || now.Start != server.Start || now.Command != server.Command {
		return cleanupwatch.Process{}, fmt.Errorf("%w: watchman server pid %d started %s changed while it was identified: pid %d answers, pid %d started %s", cleanupwatch.ErrRefused, server.PID, server.Start, again.PID, now.PID, now.Start)
	}
	return server, nil
}

func (w cleanupWatchers) watchmanServerCommand(command string) bool {
	args := strings.Fields(command)
	if len(args) == 0 || filepath.Base(args[0]) != "watchman" || !slices.Contains(args[1:], "--foreground") {
		return false
	}
	return !slices.ContainsFunc(w.deps.Protected, func(p string) bool { return strings.Contains(command, p) })
}

func (w cleanupWatchers) holdsSocket(ctx context.Context, pid int, socket string) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, w.deps.Timeout)
	defer cancel()
	out, err := w.deps.Run.Run(ctx, render.Ambient, "lsof", "-n", "-P", "-w", "-a", "-p", strconv.Itoa(pid), "-U", "-F", "ftn")
	var exitErr *cleanupwatch.ExitError
	if errors.As(err, &exitErr) && exitErr.Code == 1 {
		out.Stdout, err = exitErr.Stdout, nil
	}
	if err != nil {
		return false, fmt.Errorf("list the sockets of pid %d: %w", pid, err)
	}
	for line := range strings.Lines(string(out.Stdout)) {
		if strings.TrimSuffix(line, "\n") == "n"+socket {
			return true, nil
		}
	}
	return false, nil
}

func (w cleanupWatchers) process(ctx context.Context, pid int) (cleanupwatch.Process, error) {
	procs, err := w.deps.Procs.Processes(ctx, []int{pid})
	if err != nil {
		return cleanupwatch.Process{}, err
	}
	proc, ok := procs[pid]
	if !ok {
		return cleanupwatch.Process{}, fmt.Errorf("%w: watcher pid %d exited", cleanupwatch.ErrRefused, pid)
	}
	return proc, nil
}

type watchmanReply struct {
	PID      int    `json:"pid"`
	Sockname string `json:"sockname"`
	Error    string `json:"error"`
}

func (w cleanupWatchers) watchman(ctx context.Context, command string) (watchmanReply, error) {
	ctx, cancel := context.WithTimeout(ctx, w.deps.Timeout)
	defer cancel()
	out, err := w.deps.Run.Run(ctx, render.Ambient, "watchman", "--no-spawn", "--no-pretty", command)
	if err != nil {
		return watchmanReply{}, fmt.Errorf("watchman %s: %w", command, err)
	}
	var reply watchmanReply
	if err := json.Unmarshal(out.Stdout, &reply); err != nil {
		return watchmanReply{}, fmt.Errorf("watchman %s: decode: %w", command, err)
	}
	if reply.Error != "" {
		return watchmanReply{}, fmt.Errorf("watchman %s: %s", command, reply.Error)
	}
	return reply, nil
}

func (w cleanupWatchers) Retire(ctx context.Context, worktree string) error {
	plan, err := cleanupwatch.Plan(ctx, w.deps, worktree)
	if err != nil {
		return fmt.Errorf("plan the watcher retirement of %s: %w", worktree, err)
	}
	out, err := cleanupwatch.Retire(ctx, w.deps, plan)
	done := retired(out)
	switch {
	case err != nil && done != "":
		return fmt.Errorf("retire the watchers of %s after retiring %s: %w", worktree, done, err)
	case err != nil:
		return fmt.Errorf("retire the watchers of %s: %w", worktree, err)
	case done != "":
		slog.Info("cleanup watchers: retired", "worktree", worktree, "retired", done)
	}
	return nil
}

func (w cleanupWatchers) CheckQuarantine(ctx context.Context, jobDir string) error {
	return cleanupwatch.CheckQuarantine(ctx, w.deps, jobDir)
}

func retired(out cleanupwatch.Outcome) string {
	var done []string
	if len(out.Retired) > 0 {
		done = append(done, "watchman roots "+strings.Join(out.Retired, ", "))
	}
	if out.Stopped != nil {
		done = append(done, fmt.Sprintf("fsmonitor daemon %d", out.Stopped.PID))
	}
	return strings.Join(done, " and ")
}

type cleanupWatchersOpts struct {
	json   bool
	budget int
}

func newCleanupWatchersCmd() *cobra.Command {
	return cleanupWatchersCmd(cleanupwatch.NewDeps(nil))
}

func cleanupWatchersCmd(deps cleanupwatch.Deps) *cobra.Command {
	var o cleanupWatchersOpts
	cmd := &cobra.Command{
		Use:   "watchers",
		Short: "Census the Watchman roots and fsmonitor daemons pinned to worktrees",
		Long: `Census the file watchers a worktree removal would have to retire: every
Watchman root with its subscriptions, triggers, in-flight queries, recrawls, and
loaded-versus-disk .watchmanconfig, every connected Watchman client, and every
builtin Git fsmonitor daemon with the worktree its IPC socket names and the
core.fsmonitor setting that starts it.

The census is read-only and bounded. Watchman is asked only through --no-spawn,
so a stopped server stays stopped; at most 256 roots and 64 client pids are
inspected, at most 64 fsmonitor daemons are examined, and each Watchman or Git
command gets 10s and each ps or lsof 5s. What falls past a bound is listed as
not examined rather than dropped.
It never contacts or starts the cleanup daemon, and it retires nothing.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runCleanupWatchers(cmd, deps, o)
		},
	}
	cmd.Flags().BoolVar(&o.json, "json", false, "emit the census as JSON")
	cmd.Flags().IntVar(&o.budget, "budget", watchersBudget, "token budget for the human report (0 = uncapped)")
	return cmd
}

func runCleanupWatchers(cmd *cobra.Command, deps cleanupwatch.Deps, o cleanupWatchersOpts) error {
	report, err := cleanupwatch.Take(cmd.Context(), deps)
	if err != nil {
		return fmt.Errorf("watchers: census: %w", err)
	}
	if o.json {
		data, err := json.MarshalIndent(report, "", "  ")
		if err != nil {
			return fmt.Errorf("watchers: marshal census: %w", err)
		}
		cmd.Println(string(data))
		return nil
	}
	cmd.Print(render.Cap(report.Render(), o.budget))
	return nil
}
