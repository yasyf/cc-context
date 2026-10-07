package native

import (
	"fmt"

	"golang.org/x/sys/unix"

	"github.com/yasyf/cc-context/internal/cleanup"
)

const (
	prioDarwinProcess = 4
	prioDarwinBG      = 0x1000

	iopolTypeDisk     = 0
	iopolScopeProcess = 0
	iopolDefault      = 0
	iopolThrottle     = 3
)

// Band switches the calling process between the darwin background band, with
// throttled disk I/O, and the default band, in either direction.
type Band struct{}

var _ cleanup.Band = Band{}

// Background enters the darwin background band and throttles disk I/O, so
// deletion never competes with the work a user is waiting on.
func (Band) Background() error {
	return band(prioDarwinBG, iopolThrottle)
}

// Foreground leaves the background band and restores the default disk I/O
// policy, for work a client is waiting on.
func (Band) Foreground() error {
	return band(0, iopolDefault)
}

func band(prio int, iopolicy uintptr) error {
	lib, err := loadLibSystem()
	if err != nil {
		return err
	}
	if err := unix.Setpriority(prioDarwinProcess, 0, prio); err != nil {
		return fmt.Errorf("native: set the darwin band to %#x: %w", prio, err)
	}
	if rc, errno := call(lib.setIOPolicy, iopolTypeDisk, iopolScopeProcess, iopolicy); rc != 0 {
		return fmt.Errorf("native: set the disk io policy to %d: %w", iopolicy, errno)
	}
	return nil
}
