//go:build !windows

package codeexec

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/yasyf/cc-context/internal/render"
)

// The grandchild never execs: a process inside execve cannot act on a pending
// SIGKILL. Its death is observed as the kernel closing the FIFO write end at
// exit, which an unreaped zombie or a recycled PID cannot fake.
func TestDiscoverCancelKillsDescendants(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	pidPath := filepath.Join(dir, "grandchild.pid")
	livePath := filepath.Join(dir, "grandchild.live")
	blockPath := filepath.Join(dir, "grandchild.block")
	for _, p := range []string{livePath, blockPath} {
		if err := syscall.Mkfifo(p, 0o600); err != nil {
			t.Fatalf("mkfifo %s: %v", p, err)
		}
	}
	probeCtx := fakeClaudeCtx(render.WithEnv(t.Context(),
		"CCX_TEST_GRANDCHILD_PID="+pidPath,
		"CCX_TEST_GRANDCHILD_LIVE="+livePath,
		"CCX_TEST_GRANDCHILD_BLOCK="+blockPath,
		"CCX_EXEC_MCP_TIMEOUT=5m",
	), t, "#!/bin/sh\n"+
		"( trap '' TERM; exec 3>\"$CCX_TEST_GRANDCHILD_LIVE\"; read x < \"$CCX_TEST_GRANDCHILD_BLOCK\" ) &\n"+
		"printf '%s\\n' \"$!\" > \"$CCX_TEST_GRANDCHILD_PID\"\n"+
		"trap '' TERM\n"+
		"wait\n")

	ctx, cancel := context.WithCancel(probeCtx)
	defer cancel()
	probe := make(chan error, 1)
	go func() {
		_, err := Discover(ctx)
		probe <- err
	}()

	var pid int
	readDeadline := time.Now().Add(30 * time.Second)
	for {
		raw, err := os.ReadFile(pidPath)
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
	if err := <-probe; err == nil {
		t.Fatal("Discover on a cancelled probe = nil, want error")
	}

	select {
	case <-closed:
	case <-time.After(30 * time.Second):
		t.Fatalf("grandchild PID %d survived Discover cancellation", pid)
	}
}
