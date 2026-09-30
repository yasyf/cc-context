//go:build darwin

package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"time"

	"github.com/yasyf/cc-context/internal/cleanup"
	"github.com/yasyf/cc-context/internal/cleanup/agent"
	"github.com/yasyf/cc-context/internal/cleanup/daemon"
	"github.com/yasyf/cc-context/internal/cleanup/native"
	"github.com/yasyf/cc-context/internal/cleanup/relocate"
	"github.com/yasyf/cc-context/internal/cleanup/rmtree"
	"github.com/yasyf/cc-context/internal/render"
	"github.com/yasyf/cc-context/internal/version"
)

const cleanupDaemonized = true

func connectCleanup(ctx context.Context) (cleanup.Service, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("cleanup: locate the ccx executable: %w", err)
	}
	if exe, err = filepath.EvalSymlinks(exe); err != nil {
		return nil, fmt.Errorf("cleanup: resolve the ccx executable: %w", err)
	}
	control, err := agent.Connect(ctx, agent.Options{
		Layout:    cleanup.DefaultLayout(),
		Version:   version.String(),
		Source:    exe,
		Dial:      func(socket string) cleanup.Control { return daemon.Dial(socket) },
		Launchctl: runLaunchctl,
	})
	if err != nil {
		return nil, fmt.Errorf("cleanup: reach the deletion daemon: %w", err)
	}
	return control, nil
}

func runLaunchctl(ctx context.Context, path string, args ...string) (string, int, error) {
	out, err := exec.CommandContext(ctx, path, args...).CombinedOutput() //nolint:gosec // path is daemonkit's fixed launchctl binary and args are the verbs it authors
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() > 0 {
		return string(out), exitErr.ExitCode(), nil
	}
	return string(out), 0, err
}

func previewCleanup(ctx context.Context, r cleanup.Request) (cleanup.Job, error) {
	journal, err := cleanup.ViewJournal(cleanup.DefaultLayout())
	if err != nil {
		return cleanup.Job{}, err
	}
	return relocate.New(relocate.Config{
		Journal:  journal,
		Guard:    native.Guard,
		Watchers: newCleanupWatchers(native.Guard),
		GitEnv:   slices.Concat(os.Environ(), render.EnvFrom(ctx)),
		Now:      time.Now,
	}).Preview(ctx, r)
}

func runCleanupServe(ctx context.Context) error {
	if err := native.Background(); err != nil {
		return fmt.Errorf("cleanup serve: demote to background priority: %w", err)
	}
	if err := native.RaiseFileLimit(); err != nil {
		return fmt.Errorf("cleanup serve: raise the open-file limit: %w", err)
	}
	layout := cleanup.DefaultLayout()
	journal, err := cleanup.OpenJournal(layout)
	if err != nil {
		return fmt.Errorf("cleanup serve: %w", err)
	}
	engine, err := daemon.New(daemon.Config{
		Journal: journal,
		Relocator: relocate.New(relocate.Config{
			Journal:  journal,
			Guard:    native.Guard,
			Watchers: newCleanupWatchers(native.Guard),
			GitEnv:   os.Environ(),
			Now:      time.Now,
		}),
		Deleter: rmtree.Deleter{},
		CPU:     native.NewFSEventsSampler(),
		Version: version.String(),
	})
	if err != nil {
		return fmt.Errorf("cleanup serve: %w", err)
	}
	if err := daemon.Serve(ctx, engine, layout); err != nil {
		return fmt.Errorf("cleanup serve: %w", err)
	}
	return nil
}
