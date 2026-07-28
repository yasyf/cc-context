//go:build !windows

package render

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// The grandchild never execs: a process inside execve cannot act on a pending
// SIGKILL. Its death is observed as the kernel closing the FIFO write end at
// exit, which an unreaped zombie or a recycled PID cannot fake.
func TestRunCLIProbeDirKillsDescendants(t *testing.T) {
	dir := t.TempDir()
	pidPath := filepath.Join(dir, "grandchild.pid")
	livePath := filepath.Join(dir, "grandchild.live")
	blockPath := filepath.Join(dir, "grandchild.block")
	for _, p := range []string{livePath, blockPath} {
		if err := syscall.Mkfifo(p, 0o600); err != nil {
			t.Fatalf("mkfifo %s: %v", p, err)
		}
	}
	script := filepath.Join(dir, "fork.sh")
	body := "#!/bin/sh\n" +
		"( trap '' TERM; exec 3>\"" + livePath + "\"; read x < \"" + blockPath + "\" ) &\n" +
		"printf '%s\\n' \"$!\" > \"" + pidPath + "\"\n" +
		"trap '' TERM\n" +
		"wait\n"
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil { //nolint:gosec // fake executable must be owner-executable
		t.Fatalf("write %s: %v", script, err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	probe := make(chan error, 1)
	go func() {
		_, _, _, err := RunCLIProbeDir(ctx, dir, script, nil)
		probe <- err
	}()

	var pid int
	readDeadline := time.Now().Add(30 * time.Second)
	for {
		raw, err := os.ReadFile(pidPath) //nolint:gosec // path is the test's own temp dir
		if err == nil && strings.HasSuffix(string(raw), "\n") {
			pid, err = strconv.Atoi(strings.TrimSpace(string(raw)))
			if err != nil {
				t.Fatalf("parse grandchild PID %q: %v", raw, err)
			}
			break
		}
		if time.Now().After(readDeadline) {
			t.Fatalf("grandchild PID never published at %s: %v", pidPath, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })

	live, err := os.OpenFile(livePath, os.O_RDONLY, 0)
	if err != nil {
		t.Fatalf("open %s: %v", livePath, err)
	}
	defer func() { _ = live.Close() }()
	closed := make(chan struct{})
	go func() {
		var b [1]byte
		_, _ = live.Read(b[:])
		close(closed)
	}()

	cancel()
	select {
	case <-closed:
	case <-time.After(10 * time.Second):
		t.Errorf("grandchild PID %d survived cancellation", pid)
	}
	select {
	case <-probe:
	case <-time.After(10 * time.Second):
		t.Errorf("the probe never returned; a surviving grandchild still holds the output pipe")
	}
}
