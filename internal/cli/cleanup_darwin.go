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
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("find the home directory: %w", err)
	}
	out, code, stderr, err := render.RunCLIExitCode(ctx, render.Dir(home), "git", []string{"config", "--global", "--get-all", cleanupRateCapKey})
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", cleanupRateCapKey, err)
	}
	if code == 1 {
		return nil, nil
	}
	if code != 0 {
		return nil, fmt.Errorf("read %s: exit %d: %s", cleanupRateCapKey, code, strings.TrimSpace(stderr))
	}
	return parseCleanupRateCaps(out)
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
	layout := cleanup.DefaultLayout()
	journal, err := cleanup.OpenJournal(layout)
	if err != nil {
		return fmt.Errorf("cleanup serve: %w", err)
	}
	engine, err := daemon.New(daemon.Config{
		Journal: journal,
		Caps:    caps,
		Relocator: relocate.New(relocate.Config{
			Journal:  journal,
			Guard:    native.GuardRoots,
			Watchers: newCleanupWatchers(native.Guard),
			GitEnv:   os.Environ(),
			LFSClean: lfsclean.Command(executable),
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
