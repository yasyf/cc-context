package native

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"unsafe"

	"github.com/ebitengine/purego"

	"github.com/yasyf/cc-context/internal/cleanup"
)

type probeCounters struct {
	pids  atomic.Int64
	cwd   atomic.Int64
	fds   atomic.Int64
	nodes atomic.Int64
}

type probeCounts struct {
	pids, cwd, fds, nodes int64
}

func (c *probeCounters) take() probeCounts {
	return probeCounts{c.pids.Swap(0), c.cwd.Swap(0), c.fds.Swap(0), c.nodes.Swap(0)}
}

func countedProcess(t *testing.T, pid int) (*libSystem, *probeCounters) {
	t.Helper()
	real, err := loadLibSystem()
	if err != nil {
		t.Fatal(err)
	}
	lib := *real
	counts := new(probeCounters)
	lib.procListPIDs = purego.NewCallback(func(_, _, buf, _ uintptr) uintptr {
		counts.pids.Add(1)
		*at[int32](buf) = int32(pid) //nolint:gosec // Darwin pids are C ints
		return unsafe.Sizeof(int32(0))
	})
	lib.procPIDInfo = purego.NewCallback(func(process, flavor, subject, buf, size uintptr) uintptr {
		if process == uintptr(pid) { //nolint:gosec // a positive pid fits uintptr
			switch flavor {
			case flavorVnodePathInfo:
				counts.cwd.Add(1)
			case flavorListFDs:
				counts.fds.Add(1)
			}
		}
		result, _, errno := purego.SyscallN(real.procPIDInfo, process, flavor, subject, buf, size)
		*real.errno() = int32(uint32(errno)) //nolint:gosec // errno is a C int in the low 32 bits
		return result
	})
	lib.procPIDFDInfo = purego.NewCallback(func(process, fd, flavor, buf, size uintptr) uintptr {
		if process == uintptr(pid) && flavor == fdFlavorVnodePathInfo { //nolint:gosec // a positive pid fits uintptr
			counts.nodes.Add(1)
		}
		result, _, errno := purego.SyscallN(real.procPIDFDInfo, process, fd, flavor, buf, size)
		*real.errno() = int32(uint32(errno)) //nolint:gosec // errno is a C int in the low 32 bits
		return result
	})
	return &lib, counts
}

func TestGuardRootsSharesOneProcessObservation(t *testing.T) {
	f := plant(t)
	pid, _ := grandchild(t, f.outside, reader, f.file)
	lib, counts := countedProcess(t, pid)
	base := t.TempDir()
	first := cleanup.GuardRoot{Path: filepath.Join(base, "first")}
	second := cleanup.GuardRoot{Path: filepath.Join(base, "second")}
	for _, root := range []cleanup.GuardRoot{first, second} {
		if err := os.Mkdir(root.Path, 0o700); err != nil {
			t.Fatal(err)
		}
		if _, err := guardRoots(t.Context(), lib, root); err != nil {
			t.Fatal(err)
		}
	}
	before := counts.take()
	if _, err := guardRoots(t.Context(), lib, first, second); err != nil {
		t.Fatal(err)
	}
	after := counts.take()
	if after.pids != 1 || after.cwd != 1 || after.fds == 0 || after.nodes == 0 {
		t.Fatalf("shared observations missing: %+v", after)
	}
	if before.pids != 2*after.pids || before.cwd != 2*after.cwd || before.fds != 2*after.fds || before.nodes != 2*after.nodes {
		t.Fatalf("sequential %+v, shared %+v; expected one kernel observation for both roots", before, after)
	}
	t.Logf("sequential=%+v shared=%+v", before, after)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := guardRoots(ctx, lib, first, second); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled guard = %v", err)
	}
	if got := counts.take(); got != (probeCounts{}) {
		t.Fatalf("cancelled guard performed process reads: %+v", got)
	}
}
