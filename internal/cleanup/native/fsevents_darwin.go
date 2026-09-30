package native

import (
	"context"
	"errors"
	"fmt"
	"math/bits"
	"runtime"
	"strings"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

const fseventsdSuffix = "/FSEvents.framework/Versions/A/Support/fseventsd"

var errNoFSEvents = errors.New("native: fseventsd is not running")

type fseventsd struct {
	pid       int
	uniqueID  uint64
	coalition uint64
}

// FSEventsSampler reads fseventsd's cumulative CPU time from its resource
// coalition, the one account of a root process an unprivileged caller may
// read. It remembers which process fseventsd is and scans the process table
// again only when that process is replaced.
type FSEventsSampler struct {
	mu     sync.Mutex
	daemon *fseventsd
}

// NewFSEventsSampler returns a sampler that finds fseventsd on its first
// Sample.
func NewFSEventsSampler() *FSEventsSampler { return &FSEventsSampler{} }

// Sample returns the cumulative CPU time of fseventsd's coalition, which the
// kernel bills in its own time units and Sample converts. It is an error when
// fseventsd is not running or the kernel refuses the read.
func (s *FSEventsSampler) Sample(ctx context.Context) (time.Duration, error) {
	lib, err := loadLibSystem()
	if err != nil {
		return 0, err
	}
	timebase, err := loadTimebase()
	if err != nil {
		return 0, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.identify(ctx, lib); err != nil {
		return 0, err
	}
	usage := new(coalitionResourceUsage)
	var pinner runtime.Pinner
	pinner.Pin(usage)
	defer pinner.Unpin()
	if rc, errno := call(lib.coalitionUsage, uintptr(s.daemon.coalition), uintptr(unsafe.Pointer(usage)), unsafe.Sizeof(*usage)); rc != 0 { //nolint:gosec // FFI takes the pinned pointer
		return 0, fmt.Errorf("native: read the resource usage of coalition %d: %w", s.daemon.coalition, errno)
	}
	hi, lo := bits.Mul64(usage.cpuTime, uint64(timebase.numer))
	nanos, _ := bits.Div64(hi, lo, uint64(timebase.denom))
	return time.Duration(nanos), nil //nolint:gosec // cumulative CPU time stays far below 292 years
}

func (s *FSEventsSampler) identify(ctx context.Context, lib *libSystem) error {
	if s.daemon != nil {
		unique, err := pidInfo[procUniqIdentifierInfo](lib, s.daemon.pid, flavorUniqIdentifierInfo)
		if err == nil && unique.uniqueID == s.daemon.uniqueID {
			return nil
		}
		if err != nil && !errors.Is(err, unix.ESRCH) {
			return fmt.Errorf("native: re-identify fseventsd at pid %d: %w", s.daemon.pid, err)
		}
		s.daemon = nil
	}
	found, err := findFSEvents(ctx, lib)
	if err != nil {
		return err
	}
	s.daemon = found
	return nil
}

func findFSEvents(ctx context.Context, lib *libSystem) (*fseventsd, error) {
	pids, err := lib.listPIDs(procAllPIDs, 0)
	if err != nil {
		return nil, err
	}
	for _, pid := range pids {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		path, err := lib.pidPath(int(pid))
		if err != nil || !strings.HasSuffix(path, fseventsdSuffix) {
			continue
		}
		unique, err := pidInfo[procUniqIdentifierInfo](lib, int(pid), flavorUniqIdentifierInfo)
		if err != nil {
			return nil, fmt.Errorf("native: identify fseventsd at pid %d: %w", pid, err)
		}
		coalitions, err := pidInfo[procPIDCoalitionInfo](lib, int(pid), flavorCoalitionInfo)
		if err != nil {
			return nil, fmt.Errorf("native: read the coalition of fseventsd at pid %d: %w", pid, err)
		}
		return &fseventsd{pid: int(pid), uniqueID: unique.uniqueID, coalition: coalitions.ids[0]}, nil
	}
	return nil, errNoFSEvents
}

var loadTimebase = sync.OnceValues(func() (machTimebase, error) {
	lib, err := loadLibSystem()
	if err != nil {
		return machTimebase{}, err
	}
	timebase := new(machTimebase)
	var pinner runtime.Pinner
	pinner.Pin(timebase)
	defer pinner.Unpin()
	if rc, _ := call(lib.machTimebaseInfo, uintptr(unsafe.Pointer(timebase))); rc != 0 { //nolint:gosec // FFI takes the pinned pointer
		return machTimebase{}, fmt.Errorf("native: mach_timebase_info: kern_return %d", rc)
	}
	return *timebase, nil
})
