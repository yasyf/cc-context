package native

import (
	"context"
	"errors"
	"fmt"
	"math/bits"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"sync"
	"time"

	"golang.org/x/sys/unix"

	"github.com/yasyf/cc-context/internal/cleanup"
)

const (
	flavorTaskAllInfo = 2
	createProbes      = 8
)

var (
	sessionNames = map[string]bool{"claude": true, "codex": true}

	errPriming = errors.New("native: the first activity sample only sets the CPU baseline")
)

type procTaskAllInfo struct {
	_      procBSDInfo
	_      [16]byte
	user   uint64
	system uint64
	_      [64]byte
}

// ActivitySampler reads how busy the Claude and Codex sessions sharing the
// daemon's volume are: the CPU each session process spent since the previous
// sample, and how long creating a file in a probe directory on that volume
// takes. A session is a process of this user whose argv[0] names claude or
// codex.
type ActivitySampler struct {
	probe string

	mu    sync.Mutex
	at    time.Time
	spent map[int]time.Duration
}

// NewActivitySampler returns a sampler that times creates in probe, which it
// makes on its first Sample.
func NewActivitySampler(probe string) *ActivitySampler { return &ActivitySampler{probe: probe} }

// Sample times file creates first, so its own census does not slow them, then
// reads every session's cumulative CPU time. The first Sample is errPriming.
func (s *ActivitySampler) Sample(ctx context.Context) (cleanup.Activity, error) {
	lib, err := loadLibSystem()
	if err != nil {
		return cleanup.Activity{}, err
	}
	timebase, err := loadTimebase()
	if err != nil {
		return cleanup.Activity{}, err
	}
	create, err := timeCreates(s.probe)
	if err != nil {
		return cleanup.Activity{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	spent, err := sessionCPU(ctx, lib, timebase)
	if err != nil {
		return cleanup.Activity{}, err
	}
	now := time.Now()
	previous, since := s.spent, now.Sub(s.at)
	s.spent, s.at = spent, now
	if previous == nil {
		return cleanup.Activity{}, errPriming
	}
	activity := cleanup.Activity{Sessions: make([]float64, 0, len(spent)), Create: create}
	for pid, cpu := range spent {
		if before := previous[pid]; before <= cpu {
			cpu -= before
		}
		activity.Sessions = append(activity.Sessions, float64(cpu)/float64(since)*100)
	}
	return activity, nil
}

func timeCreates(dir string) (time.Duration, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return 0, fmt.Errorf("native: make the create probe directory: %w", err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return 0, fmt.Errorf("native: open the create probe directory: %w", err)
	}
	defer func() { _ = root.Close() }()
	took := make([]time.Duration, createProbes)
	for i := range took {
		name := "probe-" + strconv.Itoa(i)
		start := time.Now()
		f, err := root.OpenFile(name, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
		if err != nil {
			return 0, fmt.Errorf("native: create probe file: %w", err)
		}
		if err := f.Close(); err != nil {
			return 0, fmt.Errorf("native: close probe file: %w", err)
		}
		if err := root.Remove(name); err != nil {
			return 0, fmt.Errorf("native: remove probe file: %w", err)
		}
		took[i] = time.Since(start)
	}
	slices.Sort(took)
	return took[len(took)/2], nil
}

func sessionCPU(ctx context.Context, lib *libSystem, timebase machTimebase) (map[int]time.Duration, error) {
	pids, err := lib.listPIDs(procUIDOnly, os.Getuid())
	if err != nil {
		return nil, err
	}
	spent := make(map[int]time.Duration)
	for _, pid := range pids {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		args, err := processArguments(int(pid))
		if err != nil || len(args) == 0 || !sessionNames[filepath.Base(args[0])] {
			continue
		}
		task, err := pidInfo[procTaskAllInfo](lib, int(pid), flavorTaskAllInfo)
		if errors.Is(err, unix.ESRCH) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("native: read the cpu time of session %d: %w", pid, err)
		}
		hi, lo := bits.Mul64(task.user+task.system, uint64(timebase.numer))
		nanos, _ := bits.Div64(hi, lo, uint64(timebase.denom))
		spent[int(pid)] = time.Duration(nanos) //nolint:gosec // cumulative CPU time stays far below 292 years
	}
	return spent, nil
}
