package native

import (
	"fmt"

	"github.com/yasyf/cc-context/internal/cleanup"
)

// Identify returns the process at pid with the second the kernel started it.
func Identify(pid int) (cleanup.ProcessID, error) {
	lib, err := loadLibSystem()
	if err != nil {
		return cleanup.ProcessID{}, err
	}
	bsd, err := pidInfo[procBSDInfo](lib, pid, flavorBSDInfo)
	if err != nil {
		return cleanup.ProcessID{}, fmt.Errorf("native: identify pid %d: %w", pid, err)
	}
	return cleanup.ProcessID{PID: pid, Start: int64(bsd.start)}, nil //nolint:gosec // seconds since the epoch fit int64
}

// ProcessUniqueID returns the kernel's per-boot unique id for the process at
// pid, which a later process reusing the pid never repeats, whatever start
// time a stepped clock gives it.
func ProcessUniqueID(pid int) (uint64, error) {
	lib, err := loadLibSystem()
	if err != nil {
		return 0, err
	}
	unique, err := pidInfo[procUniqIdentifierInfo](lib, pid, flavorUniqIdentifierInfo)
	if err != nil {
		return 0, fmt.Errorf("native: identify pid %d: %w", pid, err)
	}
	return unique.uniqueID, nil
}
