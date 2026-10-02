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

func releaseOnCleanup(t *testing.T, live, block string) {
	t.Cleanup(func() {
		watch, err := syscall.Open(live, syscall.O_RDONLY|syscall.O_NONBLOCK, 0)
		if err != nil {
			t.Errorf("watch %s: %v", live, err)
			return
		}
		defer func() { _ = syscall.Close(watch) }()
		deadline := time.Now().Add(30 * time.Second)
		for {
			if release, err := os.OpenFile(block, os.O_WRONLY|syscall.O_NONBLOCK, 0); err == nil { //nolint:gosec // the fifo is the test's own
				_, _ = release.Write([]byte("x\n"))
				_ = release.Close()
			}
			if n, err := syscall.Read(watch, make([]byte, 1)); n == 0 && err == nil {
				return
			}
			if time.Now().After(deadline) {
				t.Errorf("%s still had a writer 30s into teardown: its holder never exited", live)
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
	})
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

func TestExecRunnerCancelKillsOnlyTheDirectChild(t *testing.T) {
	dir := t.TempDir()
	live, block := fifo(t, dir, "live"), fifo(t, dir, "block")
	releaseOnCleanup(t, live, block)
	child := &exec.Cmd{}
	render.BoundChild(child)
	limit := child.WaitDelay + 5*time.Second
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	result := make(chan error, 1)
	go func() {
		_, err := ExecRunner{}.Run(ctx, render.Ambient, "sh", "-c", fmt.Sprintf("( exec 3>%s; read x < %s ) & exec sleep 60", live, block))
		result <- err
	}()
	descendant := openFIFO(t, live, os.O_RDONLY, "the descendant never opened the live fifo")
	defer func() { _ = descendant.Close() }()
	cancel()

	var err error
	select {
	case err = <-result:
	case <-time.After(limit):
		t.Fatalf("Run did not return within %s of the cancel: the descendant holding the pipes was waited on past WaitDelay", limit)
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("Run error = %v, want context.Canceled", err)
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
