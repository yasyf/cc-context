//go:build !windows

package cleanupwatch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/yasyf/cc-context/internal/render"
)

func fifo(t *testing.T, dir, name string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Fatalf("mkfifo %s: %v", path, err)
	}
	return path
}

func openFIFO(t *testing.T, path string, flag int, stuck string) *os.File {
	t.Helper()
	opened := make(chan *os.File, 1)
	failed := make(chan error, 1)
	go func() {
		file, err := os.OpenFile(path, flag, 0) //nolint:gosec // the fifo is the test's own
		if err != nil {
			failed <- err
			return
		}
		opened <- file
	}()
	select {
	case file := <-opened:
		return file
	case err := <-failed:
		t.Fatalf("open %s: %v", path, err)
	case <-time.After(30 * time.Second):
		t.Fatal(stuck)
	}
	return nil
}

func TestExecRunnerDeadlineKillsOnlyTheDirectChild(t *testing.T) {
	dir := t.TempDir()
	live, block := fifo(t, dir, "live"), fifo(t, dir, "block")
	const deadline = 200 * time.Millisecond
	child := &exec.Cmd{}
	render.BoundChild(child)
	limit := deadline + child.WaitDelay + 5*time.Second
	ctx, cancel := context.WithTimeout(context.Background(), deadline)
	defer cancel()

	result := make(chan error, 1)
	go func() {
		_, err := ExecRunner{}.Run(ctx, render.Ambient, "sh", "-c", fmt.Sprintf("( exec 3>%s; read x < %s ) & exec sleep 60", live, block))
		result <- err
	}()
	descendant := openFIFO(t, live, os.O_RDONLY, "the descendant never opened the live fifo")
	defer func() { _ = descendant.Close() }()

	var err error
	select {
	case err = <-result:
	case <-time.After(limit):
		t.Fatalf("Run did not return within %s: the descendant holding the pipes was waited on past WaitDelay", limit)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Run error = %v, want context.DeadlineExceeded", err)
	}

	release := openFIFO(t, block, os.O_WRONLY, "the descendant was signalled: nothing reads the block fifo")
	if _, err := release.Write([]byte("x\n")); err != nil {
		t.Fatalf("release the descendant: %v", err)
	}
	if err := release.Close(); err != nil {
		t.Fatalf("close the block fifo: %v", err)
	}
	exited := make(chan error, 1)
	go func() {
		_, err := descendant.Read(make([]byte, 1))
		exited <- err
	}()
	select {
	case err := <-exited:
		if !errors.Is(err, io.EOF) {
			t.Errorf("live fifo read = %v, want EOF once the released descendant exits", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the released descendant never exited")
	}
}
