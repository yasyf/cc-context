//go:build !windows

package cache

import (
	"context"
	"os"
	"time"

	"golang.org/x/sys/unix"
)

// flockExclusive acquires an exclusive advisory lock on f, retrying the
// non-blocking flock until it succeeds or ctx is done.
func flockExclusive(ctx context.Context, f *os.File) error {
	for {
		err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			return nil
		}
		if err != unix.EWOULDBLOCK {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// flockUnlock releases the advisory lock held on f.
func flockUnlock(f *os.File) error {
	return unix.Flock(int(f.Fd()), unix.LOCK_UN)
}
