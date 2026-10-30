//go:build windows

package nativemeta

import (
	"context"
	"os"
	"time"

	"golang.org/x/sys/windows"
)

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
