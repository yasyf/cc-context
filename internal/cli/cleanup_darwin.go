//go:build darwin

package cli

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/yasyf/cc-context/internal/cleanup"
	"github.com/yasyf/cc-context/internal/cleanup/agent"
	"github.com/yasyf/cc-context/internal/cleanup/daemon"
	"github.com/yasyf/cc-context/internal/cleanup/lfsclean"
	"github.com/yasyf/cc-context/internal/cleanup/native"
	"github.com/yasyf/cc-context/internal/cleanup/relocate"
	"github.com/yasyf/cc-context/internal/cleanup/rmtree"
	"github.com/yasyf/cc-context/internal/render"
	"github.com/yasyf/cc-context/internal/version"
)

const (
	cleanupDaemonized   = true
	cleanupReachTimeout = 5 * time.Second
	cleanupServeProcs   = 2
)

func reachCleanup(ctx context.Context) (cleanup.Service, error) {
	control, err := agent.Reach(ctx, cleanup.DefaultLayout(), func(socket string) cleanup.Control { return daemon.Dial(socket) }, cleanupReachTimeout)
	if err != nil {
		return nil, fmt.Errorf("cleanup: read the deletion daemon: %w", err)
	}
	return control, nil
}

func previewCleanup(ctx context.Context, r cleanup.Request) (cleanup.Job, error) {
	journal, err := cleanup.ViewJournal(cleanup.DefaultLayout())
	if err != nil {
		return cleanup.Job{}, err
	}
	return relocate.New(relocate.Config{
		Journal:  journal,
		Guard:    native.GuardRoots,
		Watchers: newCleanupWatchers(native.Guard),
		GitEnv:   slices.Concat(os.Environ(), render.EnvFrom(ctx)),
		Now:      time.Now,
	}).Preview(ctx, r)
}

func cleanupRateCaps(ctx context.Context) ([]cleanup.RateCap, error) {
	out, err := cleanupGlobalConfig(ctx, cleanupRateCapKey)
	if err != nil {
		return nil, err
	}
	return parseCleanupRateCaps(out)
}

func cleanupBusy(ctx context.Context) (cleanup.BusyDeletes, error) {
	rate, err := cleanupGlobalConfig(ctx, cleanupBusyRateKey)
	if err != nil {
		return cleanup.BusyDeletes{}, err
	}
	workers, err := cleanupGlobalConfig(ctx, cleanupBusyWorkersKey)
	if err != nil {
		return cleanup.BusyDeletes{}, err
	}
	quiet, err := cleanupGlobalConfig(ctx, cleanupQuietRootKey)
	if err != nil {
		return cleanup.BusyDeletes{}, err
	}
	move, err := cleanupGlobalConfig(ctx, cleanupMoveRootKey)
	if err != nil {
		return cleanup.BusyDeletes{}, err
	}
	every, err := cleanupGlobalConfig(ctx, cleanupMoveEveryKey)
	if err != nil {
		return cleanup.BusyDeletes{}, err
	}
	return parseCleanupBusyDeletes(rate, workers, quiet, move, every)
}

func cleanupGlobalConfig(ctx context.Context, key string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("find the home directory: %w", err)
	}
	out, code, stderr, err := render.RunCLIExitCode(ctx, render.Dir(home), "git", []string{"config", "--global", "--get-all", key})
	if err != nil {
		return "", fmt.Errorf("read %s: %w", key, err)
	}
	if code == 1 {
		return "", nil
	}
	if code != 0 {
		return "", fmt.Errorf("read %s: exit %d: %s", key, code, strings.TrimSpace(stderr))
	}
	return out, nil
}

func runCleanupServe(ctx context.Context) error {
	runtime.GOMAXPROCS(cleanupServeProcs)
	band := native.Band{}
	if err := band.Background(); err != nil {
		return fmt.Errorf("cleanup serve: demote to background priority: %w", err)
	}
	if err := native.RaiseFileLimit(); err != nil {
		return fmt.Errorf("cleanup serve: raise the open-file limit: %w", err)
	}
	executable, err := os.Executable()
	if err != nil {
		return fmt.Errorf("cleanup serve: find this binary: %w", err)
	}
	caps, err := cleanupRateCaps(ctx)
	if err != nil {
		return fmt.Errorf("cleanup serve: %w", err)
	}
	busy, err := cleanupBusy(ctx)
	if err != nil {
		return fmt.Errorf("cleanup serve: %w", err)
	}
	layout := cleanup.DefaultLayout()
	journal, err := cleanup.OpenJournal(layout)
	if err != nil {
		return fmt.Errorf("cleanup serve: %w", err)
	}
	engine, err := daemon.New(daemon.Config{
		Journal: journal,
		Caps:    caps,
		Busy:    busy,
		Relocator: relocate.New(relocate.Config{
			Journal:  journal,
			Guard:    native.GuardRoots,
			Watchers: newCleanupWatchers(native.Guard),
			GitEnv:   os.Environ(),
			LFSClean: lfsclean.Command(executable),
			Move:     busy.Move,
			Quiet:    time.Hour,
			Now:      time.Now,
		}),
		Deleter:  rmtree.Deleter{},
		CPU:      native.NewFSEventsSampler(),
		Activity: native.NewActivitySampler(layout.ProbeDir()),
		Band:     band,
		Parent:   native.ParentCommand,
		Identify: native.Identify,
		Version:  version.String(),
	})
	if err != nil {
		return fmt.Errorf("cleanup serve: %w", err)
	}
	if err := daemon.Serve(ctx, engine, layout); err != nil {
		return fmt.Errorf("cleanup serve: %w", err)
	}
	return nil
}
