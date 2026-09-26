// Package execstub writes the fake executables tests put on PATH.
package execstub

import (
	"os"
	"syscall"
	"testing"
)

// Write creates path as an owner-executable file holding script.
//
// It holds syscall.ForkLock for the whole write so no fork runs while the
// descriptor is open. A child that inherits it keeps the file open for writing
// until its own exec, and the kernel refuses to exec a file any process holds
// open for writing, so the stub's first run fails with ETXTBSY (Go issue
// #22315). Parallel tests that each write an executable hit that constantly.
func Write(t *testing.T, path, script string) {
	t.Helper()
	syscall.ForkLock.Lock()
	defer syscall.ForkLock.Unlock()
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil { //nolint:gosec // a fake executable must be owner-executable
		t.Fatalf("write executable %s: %v", path, err)
	}
}
