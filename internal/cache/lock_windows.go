//go:build windows

package cache

import (
	"context"
	"os"
	"time"

	"golang.org/x/sys/windows"
)

// flockExclusive acquires an exclusive byte-range lock on f via LockFileEx,
// retrying the non-blocking lock until it succeeds or ctx is done.
func flockExclusive(ctx context.Context, f *os.File) error {
	h := windows.Handle(f.Fd())
	for {
		var overlapped windows.Overlapped
		err := windows.LockFileEx(h, windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, &overlapped)
		if err == nil {
			return nil
		}
		if err != windows.ERROR_LOCK_VIOLATION {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// flockUnlock releases the byte-range lock held on f.
func flockUnlock(f *os.File) error {
	var overlapped windows.Overlapped
	return windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, &overlapped)
}
