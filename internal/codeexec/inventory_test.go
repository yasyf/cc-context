package codeexec

import (
	"context"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/yasyf/cc-context/internal/render"
	"github.com/yasyf/cc-context/internal/workspace"
)

func sampleInventory() Inventory {
	return Inventory{
		Hash:    "inv1",
		Servers: []ServerSpec{{Name: "fake", Command: "fake-mcp", Argv: []string{"serve"}, Prefix: "fake"}},
		Notes:   []string{"probe note"},
	}
}

func mustPath(ctx context.Context, t *testing.T, store *diskInventoryStore) string {
	t.Helper()
	path, err := store.path(ctx)
	if err != nil {
		t.Fatalf("path: %v", err)
	}
	return path
}

func TestInventoryStoreRoundtrip(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	ctx := render.WithEnv(t.Context(), "CLAUDE_PLUGIN_DATA="+dir)
	disk, err := NewDiskInventoryStore(ctx)
	if err != nil {
		t.Fatalf("NewDiskInventoryStore: %v", err)
	}
	if got := mustPath(ctx, t, disk.(*diskInventoryStore)); !strings.HasPrefix(got, dir) {
		t.Fatalf("record path = %q, want it under the cache root ctx carries (%q)", got, dir)
	}
	tests := []struct {
		name  string
		store InventoryStore
	}{
		{"memory", NewMemoryInventoryStore()},
		{"disk", disk},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if _, _, ok := tt.store.Load(ctx); ok {
				t.Fatal("Load on empty store = true, want miss")
			}
			inv := sampleInventory()
			probed := time.Now().UTC()
			if err := tt.store.Save(ctx, inv, probed); err != nil {
				t.Fatalf("Save: %v", err)
			}
			got, at, ok := tt.store.Load(ctx)
			if !ok {
				t.Fatal("Load after Save = miss")
			}
			if !at.Equal(probed) {
				t.Errorf("Load probed = %v, want %v", at, probed)
			}
			if !reflect.DeepEqual(got, inv) {
				t.Errorf("Load inventory = %+v, want %+v", got, inv)
			}
		})
	}
}

func TestDiskInventoryStoreCorruptMiss(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	store := newDiskInventoryStore(t.TempDir(), "/some/project")
	if err := store.Save(ctx, sampleInventory(), time.Now()); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := os.WriteFile(mustPath(ctx, t, store), []byte("{not json"), 0o600); err != nil {
		t.Fatalf("corrupt: %v", err)
	}
	if _, _, ok := store.Load(ctx); ok {
		t.Error("Load on corrupt file = true, want miss")
	}
}

func TestDiskInventoryStoreRootKeyed(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	dir := t.TempDir()
	a := newDiskInventoryStore(dir, "/project/a")
	b := newDiskInventoryStore(dir, "/project/b")
	if mustPath(ctx, t, a) == mustPath(ctx, t, b) {
		t.Fatalf("distinct roots share a path: %s", mustPath(ctx, t, a))
	}
	if err := a.Save(ctx, Inventory{Hash: "a", Servers: []ServerSpec{{Name: "a", Prefix: "a"}}}, time.Now()); err != nil {
		t.Fatalf("Save a: %v", err)
	}
	if err := b.Save(ctx, Inventory{Hash: "b", Servers: []ServerSpec{{Name: "b", Prefix: "b"}}}, time.Now()); err != nil {
		t.Fatalf("Save b: %v", err)
	}
	if got, _, ok := a.Load(ctx); !ok || got.Hash != "a" {
		t.Errorf("a.Load = %+v ok=%v, want hash a", got, ok)
	}
	if got, _, ok := b.Load(ctx); !ok || got.Hash != "b" {
		t.Errorf("b.Load = %+v ok=%v, want hash b", got, ok)
	}
}

func TestInventoryStoreEnvFingerprint(t *testing.T) {
	t.Parallel()
	ctx := render.WithEnv(t.Context(), "CLAUDE_PLUGIN_DATA="+t.TempDir())
	railway := filterCtx(t, "", "railway")
	auggie := filterCtx(t, "", "auggie")
	disk, err := NewDiskInventoryStore(ctx)
	if err != nil {
		t.Fatalf("NewDiskInventoryStore: %v", err)
	}
	tests := []struct {
		name  string
		store InventoryStore
	}{
		{"memory", NewMemoryInventoryStore()},
		{"disk", disk},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if err := tt.store.Save(railway, sampleInventory(), time.Now()); err != nil {
				t.Fatalf("Save: %v", err)
			}
			if _, _, ok := tt.store.Load(railway); !ok {
				t.Error("Load under the same DENY = miss, want hit")
			}
			if _, _, ok := tt.store.Load(auggie); ok {
				t.Error("Load after flipping DENY = hit, want miss (a filter change must invalidate)")
			}
			if _, _, ok := tt.store.Load(railway); !ok {
				t.Error("Load after restoring DENY = miss, want hit")
			}
		})
	}
}

func TestDiskInventoryStoreInvalidEnvelope(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	tests := []struct {
		name string
		data string
	}{
		{"json null", "null"},
		{"empty object", "{}"},
		{"zero probed time", `{"probed":"0001-01-01T00:00:00Z","inventory":{}}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			store := newDiskInventoryStore(t.TempDir(), "/p")
			if err := os.WriteFile(mustPath(ctx, t, store), []byte(tt.data), 0o600); err != nil {
				t.Fatalf("write: %v", err)
			}
			if _, _, ok := store.Load(ctx); ok {
				t.Errorf("Load(%s) = hit, want miss", tt.data)
			}
		})
	}
}

func TestDiskInventoryStoreFollowsRootChange(t *testing.T) {
	t.Parallel()
	ctx := render.WithEnv(t.Context(), "CLAUDE_PLUGIN_DATA="+t.TempDir())
	store, err := NewDiskInventoryStore(ctx)
	if err != nil {
		t.Fatalf("NewDiskInventoryStore: %v", err)
	}
	a := workspace.WithRoot(ctx, "/project/a")
	b := workspace.WithRoot(ctx, "/project/b")
	if err := store.Save(a, sampleInventory(), time.Now()); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if _, _, ok := store.Load(a); !ok {
		t.Fatal("Load under the probing root = miss, want hit")
	}
	if _, _, ok := store.Load(b); ok {
		t.Error("Load after a root change = hit, want miss (a warm catalog must not outlive the switch)")
	}
	if _, _, ok := store.Load(a); !ok {
		t.Error("Load after switching back = miss, want hit")
	}
}
