package native

import (
	"fmt"

	"golang.org/x/sys/unix"
)

const (
	prioDarwinProcess = 4
	prioDarwinBG      = 0x1000
	niceLowest        = 20

	iopolTypeDisk     = 0
	iopolScopeProcess = 0
	iopolThrottle     = 3
)

// Background demotes the calling process for good: the darwin background
// band, throttled disk I/O, and the lowest nice value, so deletion never
// competes with the work a user is waiting on.
func Background() error {
	lib, err := loadLibSystem()
	if err != nil {
		return err
	}
	if err := unix.Setpriority(prioDarwinProcess, 0, prioDarwinBG); err != nil {
		return fmt.Errorf("native: enter the background band: %w", err)
	}
	if rc, errno := call(lib.setIOPolicy, iopolTypeDisk, iopolScopeProcess, iopolThrottle); rc != 0 {
		return fmt.Errorf("native: throttle disk io: %w", errno)
	}
	if err := unix.Setpriority(unix.PRIO_PROCESS, 0, niceLowest); err != nil {
		return fmt.Errorf("native: lower the nice value: %w", err)
	}
	return nil
}
