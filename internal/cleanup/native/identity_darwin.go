package native

import "fmt"

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
