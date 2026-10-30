package vcs

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"

	"github.com/yasyf/cc-context/internal/cache"
	"github.com/yasyf/cc-context/internal/render"
	"github.com/yasyf/cc-context/internal/vcstest"
)

type packLockWaitContext struct {
	context.Context
	waiting chan struct{}
	once    sync.Once
}

func (c *packLockWaitContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.waiting) })
	return c.Context.Done()
}

func writePackPair(t *testing.T, objectDir, name string) {
	t.Helper()
	for _, ext := range []string{".pack", ".idx"} {
		writePackFile(t, objectDir, name+ext)
	}
}

func writePackFile(t *testing.T, objectDir, name string) {
	t.Helper()
	dir := filepath.Join(objectDir, "pack")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), nil, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestPackIndexSnapshotNewCompletePacks(t *testing.T) {
	objectDir := t.TempDir()
	writePackPair(t, objectDir, "pack-before")
	before, err := packIndexNames(t.Context(), objectDir)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := PackIndexSnapshot{objectDir: objectDir, indexes: before}
	calls := 0
	publish := func(gotDir string, indexes []string) error {
		calls++
		if gotDir != objectDir || !slices.Equal(indexes, []string{"pack-after.idx", "pack-before.idx", "pack-concurrent.idx"}) {
			t.Fatalf("publication = %q %q", gotDir, indexes)
		}
		return nil
	}
	if err := snapshot.WithNewPacks(t.Context(), publish); err != nil {
		t.Fatal(err)
	}
	writePackFile(t, objectDir, "pack-after.idx")
	writePackFile(t, objectDir, "pack-unindexed.pack")
	if err := snapshot.WithNewPacks(t.Context(), publish); err != nil {
		t.Fatal(err)
	}
	if calls != 0 {
		t.Fatalf("publication calls before complete pack = %d", calls)
	}
	writePackFile(t, objectDir, "pack-after.pack")
	writePackPair(t, objectDir, "pack-concurrent")
	if err := snapshot.WithNewPacks(t.Context(), publish); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("publication calls = %d", calls)
	}
}

func TestCapturePackIndexesObjectStoreAuthority(t *testing.T) {
	f := vcstest.Repo(t, vcstest.Worktree("lane"))
	main, err := CapturePackIndexes(f.Context(), render.Dir(f.Dir))
	if err != nil {
		t.Fatal(err)
	}
	lane := f.WorktreePath("lane")
	linked, err := CapturePackIndexes(f.ContextIn(lane), render.Dir(lane))
	if err != nil {
		t.Fatal(err)
	}
	if main.objectDir != linked.objectDir {
		t.Fatalf("main %q != linked %q", main.objectDir, linked.objectDir)
	}
	other := t.TempDir()
	writePackPair(t, other, "pack-other")
	f.Setenv("GIT_OBJECT_DIRECTORY", other)
	overridden, err := CapturePackIndexes(f.ContextIn(lane), render.Dir(lane))
	if err != nil {
		t.Fatal(err)
	}
	if overridden.objectDir != other || !slices.Equal(overridden.indexes, []string{"pack-other.idx"}) {
		t.Fatalf("overridden = %+v", overridden)
	}
}

func TestPackIndexPublicationReadsAfterSharedLock(t *testing.T) {
	objectDir := t.TempDir()
	snapshot := PackIndexSnapshot{objectDir: objectDir}
	held := make(chan struct{})
	release := make(chan struct{})
	holder := make(chan error, 1)
	go func() {
		holder <- cache.WithLock(t.Context(), objectDir, "cc-pack-index", func() error {
			close(held)
			<-release
			return nil
		})
	}()
	<-held
	ctx := &packLockWaitContext{Context: t.Context(), waiting: make(chan struct{})}
	result := make(chan error, 1)
	published := make(chan struct{}, 1)
	go func() {
		result <- snapshot.WithNewPacks(ctx, func(gotDir string, indexes []string) error {
			if gotDir != objectDir || !slices.Equal(indexes, []string{"pack-during-lock.idx"}) {
				return errors.New("publication did not capture packs after acquiring the shared lock")
			}
			published <- struct{}{}
			return nil
		})
	}()
	<-ctx.waiting
	writePackPair(t, objectDir, "pack-during-lock")
	close(release)
	if err := <-holder; err != nil {
		t.Fatal(err)
	}
	if err := <-result; err != nil {
		t.Fatal(err)
	}
	if len(published) != 1 {
		t.Fatal("new pack was not published")
	}
}

func TestPackIndexPublicationCancellationAndFailure(t *testing.T) {
	objectDir := t.TempDir()
	snapshot := PackIndexSnapshot{objectDir: objectDir}
	writePackPair(t, objectDir, "pack-new")
	want := errors.New("publication failed")
	if err := snapshot.WithNewPacks(t.Context(), func(string, []string) error { return want }); !errors.Is(err, want) {
		t.Fatalf("publication error = %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	err := snapshot.WithNewPacks(ctx, func(string, []string) error {
		t.Fatal("publication ran after cancellation")
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation error = %v", err)
	}
}
