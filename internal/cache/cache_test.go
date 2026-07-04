package cache

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestDir(t *testing.T) {
	tests := []struct {
		name string
		sub  []string
	}{
		{"no sub", nil},
		{"single sub", []string{"bin"}},
		{"nested sub", []string{"idx", "v1"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			t.Setenv("CLAUDE_PLUGIN_DATA", root)
			got, err := Dir(tt.sub...)
			if err != nil {
				t.Fatalf("Dir(%v): %v", tt.sub, err)
			}
			want := filepath.Join(append([]string{root}, tt.sub...)...)
			if got != want {
				t.Errorf("Dir(%v) = %q, want %q", tt.sub, got, want)
			}
			info, err := os.Stat(got)
			if err != nil {
				t.Fatalf("stat %q: %v", got, err)
			}
			if !info.IsDir() {
				t.Errorf("Dir(%v) = %q, not a directory", tt.sub, got)
			}
		})
	}
}

func TestDirUserCacheFallback(t *testing.T) {
	t.Setenv("CLAUDE_PLUGIN_DATA", "")
	base, err := os.UserCacheDir()
	if err != nil {
		t.Fatalf("UserCacheDir: %v", err)
	}
	got, err := Dir("bin")
	if err != nil {
		t.Fatalf("Dir(bin): %v", err)
	}
	if want := filepath.Join(base, "cc-context", "bin"); got != want {
		t.Errorf("Dir(bin) = %q, want %q", got, want)
	}
}

func TestStore(t *testing.T) {
	tests := []struct {
		name string
		pre  []byte
		data []byte
		perm os.FileMode
	}{
		{"executable", nil, []byte("#!/bin/sh\nexit 0\n"), 0o700},
		{"read-only", nil, []byte("payload"), 0o400},
		{"overwrite", []byte("old contents"), []byte("new contents"), 0o600},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "artifact")
			if tt.pre != nil {
				if err := os.WriteFile(path, tt.pre, 0o600); err != nil {
					t.Fatalf("seed %q: %v", path, err)
				}
			}
			if err := Store(path, tt.data, tt.perm); err != nil {
				t.Fatalf("Store: %v", err)
			}
			got, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read %q: %v", path, err)
			}
			if !bytes.Equal(got, tt.data) {
				t.Errorf("stored content = %q, want %q", got, tt.data)
			}
			info, err := os.Stat(path)
			if err != nil {
				t.Fatalf("stat %q: %v", path, err)
			}
			if info.Mode().Perm() != tt.perm {
				t.Errorf("stored perm = %v, want %v", info.Mode().Perm(), tt.perm)
			}
			entries, err := os.ReadDir(dir)
			if err != nil {
				t.Fatalf("read dir: %v", err)
			}
			if len(entries) != 1 || entries[0].Name() != "artifact" {
				names := make([]string, len(entries))
				for i, e := range entries {
					names[i] = e.Name()
				}
				t.Errorf("dir entries = %v, want [artifact] only (no temp file)", names)
			}
		})
	}
}

func TestWithLockMutualExclusion(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()

	// The flock is a kernel lock the race detector cannot see, so the shared
	// event log needs its own mutex.
	var mu sync.Mutex
	var events []string
	record := func(e string) {
		mu.Lock()
		defer mu.Unlock()
		events = append(events, e)
	}

	inside := make(chan struct{})
	release := make(chan struct{})
	first := make(chan error, 1)
	go func() {
		first <- WithLock(ctx, dir, "tool", func() error {
			record("first-enter")
			close(inside)
			<-release
			record("first-exit")
			return nil
		})
	}()
	<-inside

	second := make(chan error, 1)
	go func() {
		second <- WithLock(ctx, dir, "tool", func() error {
			record("second-enter")
			return nil
		})
	}()

	close(release)
	if err := <-first; err != nil {
		t.Fatalf("first WithLock: %v", err)
	}
	if err := <-second; err != nil {
		t.Fatalf("second WithLock: %v", err)
	}
	want := []string{"first-enter", "first-exit", "second-enter"}
	if len(events) != len(want) {
		t.Fatalf("events = %v, want %v", events, want)
	}
	for i := range want {
		if events[i] != want[i] {
			t.Fatalf("events = %v, want %v", events, want)
		}
	}
}

func TestWithLockContextCanceled(t *testing.T) {
	dir := t.TempDir()

	acquired := make(chan struct{})
	release := make(chan struct{})
	holder := make(chan error, 1)
	go func() {
		holder <- WithLock(context.Background(), dir, "tool", func() error {
			close(acquired)
			<-release
			return nil
		})
	}()
	<-acquired

	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	err := WithLock(ctx, dir, "tool", func() error {
		t.Error("fn ran despite expired context")
		return nil
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("WithLock err = %v, want context.DeadlineExceeded", err)
	}

	close(release)
	if err := <-holder; err != nil {
		t.Fatalf("holder WithLock: %v", err)
	}
}
