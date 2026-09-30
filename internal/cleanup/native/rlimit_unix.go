//go:build unix

package native

import (
	"fmt"

	"golang.org/x/sys/unix"
)

const fileLimit = 8192

// RaiseFileLimit lifts the soft open-file limit to the hard limit or 8192,
// whichever is lower, so a deletion holding one descriptor per directory level
// does not run out. It never lowers a limit that is already higher.
func RaiseFileLimit() error {
	var limit unix.Rlimit
	if err := unix.Getrlimit(unix.RLIMIT_NOFILE, &limit); err != nil {
		return fmt.Errorf("native: read the open-file limit: %w", err)
	}
	want := min(limit.Max, fileLimit)
	if limit.Cur >= want {
		return nil
	}
	limit.Cur = want
	if err := unix.Setrlimit(unix.RLIMIT_NOFILE, &limit); err != nil {
		return fmt.Errorf("native: raise the open-file limit to %d: %w", want, err)
	}
	return nil
}
