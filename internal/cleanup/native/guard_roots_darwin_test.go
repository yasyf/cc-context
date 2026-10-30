package native

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/yasyf/cc-context/internal/cleanup"
)

func TestGuardRootsMatchesSequentialScopes(t *testing.T) {
	for _, name := range []string{
		"empty", "cwd first before unknown fd", "cwd second after unknown fd",
		"fd second", "watcher first only", "both watchers", "bystander", "bystander vnode second", "unknown", "exited",
	} {
		t.Run(name, func(t *testing.T) {
			f := plant(t)
			outside := realPath(t, t.TempDir())
			secondFile := filepath.Join(f.outside, "held")
			if err := os.WriteFile(secondFile, []byte("held"), 0o600); err != nil {
				t.Fatal(err)
			}
			makeRoots := func() []rootWalk {
				kernel := staged{parent: 1, starts: []uint64{100}, cwd: vnodeRecord(t, outside, outside)}
				first, second := cleanup.GuardRoot{Path: f.tree}, cleanup.GuardRoot{Path: f.outside}
				switch name {
				case "cwd first before unknown fd":
					kernel.cwd, kernel.denied = vnodeRecord(t, f.sub, f.real+"/sub"), unix.EPERM
				case "cwd second after unknown fd":
					kernel.cwd, kernel.denied = vnodeRecord(t, f.outside, realPath(t, f.outside)), unix.EPERM
				case "fd second":
					kernel.open = vnodeRecord(t, secondFile, realPath(t, secondFile))
				case "watcher first only", "both watchers":
					second.Path = f.tree + "/."
					first.Retiring = []cleanup.ProcessID{{PID: 4242, Start: 100}}
					kernel.open = vnodeRecord(t, f.file, f.real+"/file")
					if name == "both watchers" {
						second.Retiring = slices.Clone(first.Retiring)
					}
				case "bystander", "bystander vnode second":
					kernel = agent("/usr/libexec/lsd", 100)
					kernel.cwd = vnodeRecord(t, outside, outside)
					if name == "bystander vnode second" {
						kernel.opaque = 0
						kernel.vnode = vnodeRecord(t, secondFile, realPath(t, secondFile))
					}
				case "unknown":
					kernel.denied = unix.EIO
				case "exited":
					kernel.identities = []unix.Errno{unix.ESRCH}
				}
				lib := kernel.lib(t)
				var roots []rootWalk
				for _, target := range []cleanup.GuardRoot{first, second} {
					roots = append(roots, rootWalk{path: target.Path, scan: &scan{
						lib: lib, root: realPath(t, target.Path), given: filepath.Clean(target.Path), retiring: target.Retiring,
					}})
				}
				return roots
			}
			sequentialCtx, sequentialIgnored := cleanup.WithIgnored(t.Context())
			sequential := makeRoots()
			var wantPath string
			var wantErr error
			for i := range sequential {
				walkProcess(sequential[i:i+1], 4242)
				wantPath, wantErr = guardOutcome(sequentialCtx, sequential[i:i+1])
				if wantErr != nil {
					break
				}
			}
			ctx, ignored := cleanup.WithIgnored(t.Context())
			combined := makeRoots()
			walkProcess(combined, 4242)
			path, err := guardOutcome(ctx, combined)
			if path != wantPath || errorText(err) != errorText(wantErr) {
				t.Fatalf("combined = %q, %v; sequential = %q, %v", path, err, wantPath, wantErr)
			}
			var active, wantActive *cleanup.ActiveError
			errors.As(err, &active)
			errors.As(wantErr, &wantActive)
			if !reflect.DeepEqual(active, wantActive) || !slices.Equal(*ignored, *sequentialIgnored) {
				t.Fatalf("combined holders/ignored = %+v/%+v, sequential %+v/%+v", active, *ignored, wantActive, *sequentialIgnored)
			}
			switch name {
			case "empty", "both watchers", "bystander", "exited":
				if err != nil {
					t.Fatal(err)
				}
			case "cwd first before unknown fd":
				if active == nil || active.Worktree != f.tree || active.Holders[0].Evidence != cleanup.EvidenceCwd {
					t.Fatalf("expected first cwd refusal, got %v", err)
				}
			case "cwd second after unknown fd":
				if !errors.Is(err, unix.EPERM) || path != f.tree {
					t.Fatalf("expected first unknown refusal before later holder, got %q/%v", path, err)
				}
			case "fd second", "watcher first only", "bystander vnode second":
				if active == nil || active.Worktree != combined[1].path || active.Holders[0].Evidence != cleanup.EvidenceFD {
					t.Fatalf("strict second scope omitted or widened: %q/%v", path, err)
				}
			case "unknown":
				if !errors.Is(err, unix.EIO) {
					t.Fatalf("unknown descriptor was admitted: %v", err)
				}
			}
		})
	}
}

func errorText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func TestGuardRootsRetainsEveryRawSafetyRecheck(t *testing.T) {
	for _, drift := range []bool{false, true} {
		name := "stable"
		if drift {
			name = "second final birth changed"
		}
		t.Run(name, func(t *testing.T) {
			kernel := agent("/usr/libexec/lsd", 100)
			kernel.micros = []uint64{1, 1, 1, 1, 1}
			if drift {
				kernel.micros[4] = 2
			}
			lib := kernel.lib(t)
			roots := []rootWalk{
				{path: "/first", scan: &scan{lib: lib, root: "/first", given: "/first"}},
				{path: "/second", scan: &scan{lib: lib, root: "/second", given: "/second"}},
			}
			ctx, ignored := cleanup.WithIgnored(context.Background())
			walkProcess(roots, 4242)
			path, err := guardOutcome(ctx, roots)
			if !drift {
				if err != nil || kernel.reads != 5 || len(*ignored) != 2 {
					t.Fatalf("stable raw checks = %d, ignored %+v, error %v", kernel.reads, *ignored, err)
				}
			} else if err == nil || path != "/second" || !strings.Contains(err.Error(), "100.000002") || len(*ignored) != 1 {
				t.Fatalf("second final recheck skipped: %q/%v, ignored %+v", path, err, *ignored)
			}
		})
	}
}

func TestGuardRootsRetiringIdentityRechecksAreNotShared(t *testing.T) {
	kernel := staged{parent: 1, starts: []uint64{100, 100, 101}}
	lib := kernel.lib(t)
	watcher := []cleanup.ProcessID{{PID: 4242, Start: 100}}
	roots := []rootWalk{
		{path: "/first", scan: &scan{lib: lib, root: "/first", given: "/first", retiring: watcher}},
		{path: "/second", scan: &scan{lib: lib, root: "/second", given: "/second", retiring: watcher}},
	}
	walkProcess(roots, 4242)
	path, err := guardOutcome(t.Context(), roots)
	if err == nil || path != "/second" || !strings.Contains(err.Error(), "started at 101") {
		t.Fatalf("retiring watcher recheck skipped: %q/%v", path, err)
	}
}
