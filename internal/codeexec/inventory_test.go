package codeexec

import (
	"os"
	"reflect"
	"testing"
	"time"
)

func sampleInventory() Inventory {
	return Inventory{
		Hash:    "inv1",
		Servers: []ServerSpec{{Name: "fake", Command: "fake-mcp", Argv: []string{"serve"}, Prefix: "fake"}},
		Notes:   []string{"probe note"},
	}
}

func TestInventoryStoreRoundtrip(t *testing.T) {
	t.Setenv("CLAUDE_PLUGIN_DATA", t.TempDir())
	disk, err := NewDiskInventoryStore()
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
			if _, _, ok := tt.store.Load(); ok {
				t.Fatal("Load on empty store = true, want miss")
			}
			inv := sampleInventory()
			probed := time.Now().UTC()
			if err := tt.store.Save(inv, probed); err != nil {
				t.Fatalf("Save: %v", err)
			}
			got, at, ok := tt.store.Load()
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
	store := newDiskInventoryStore(t.TempDir(), "/some/project")
	if err := store.Save(sampleInventory(), time.Now()); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := os.WriteFile(store.path, []byte("{not json"), 0o600); err != nil {
		t.Fatalf("corrupt: %v", err)
	}
	if _, _, ok := store.Load(); ok {
		t.Error("Load on corrupt file = true, want miss")
	}
}

func TestDiskInventoryStoreCWDKeyed(t *testing.T) {
	dir := t.TempDir()
	a := newDiskInventoryStore(dir, "/project/a")
	b := newDiskInventoryStore(dir, "/project/b")
	if a.path == b.path {
		t.Fatalf("distinct cwds share a path: %s", a.path)
	}
	if err := a.Save(Inventory{Hash: "a", Servers: []ServerSpec{{Name: "a", Prefix: "a"}}}, time.Now()); err != nil {
		t.Fatalf("Save a: %v", err)
	}
	if err := b.Save(Inventory{Hash: "b", Servers: []ServerSpec{{Name: "b", Prefix: "b"}}}, time.Now()); err != nil {
		t.Fatalf("Save b: %v", err)
	}
	if got, _, ok := a.Load(); !ok || got.Hash != "a" {
		t.Errorf("a.Load = %+v ok=%v, want hash a", got, ok)
	}
	if got, _, ok := b.Load(); !ok || got.Hash != "b" {
		t.Errorf("b.Load = %+v ok=%v, want hash b", got, ok)
	}
}

func TestInventoryStoreEnvFingerprint(t *testing.T) {
	t.Setenv("CLAUDE_PLUGIN_DATA", t.TempDir())
	disk, err := NewDiskInventoryStore()
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
			t.Setenv("CCX_EXEC_MCP_DENY", "railway")
			if err := tt.store.Save(sampleInventory(), time.Now()); err != nil {
				t.Fatalf("Save: %v", err)
			}
			if _, _, ok := tt.store.Load(); !ok {
				t.Error("Load under the same DENY = miss, want hit")
			}
			t.Setenv("CCX_EXEC_MCP_DENY", "auggie")
			if _, _, ok := tt.store.Load(); ok {
				t.Error("Load after flipping DENY = hit, want miss (a filter change must invalidate)")
			}
			t.Setenv("CCX_EXEC_MCP_DENY", "railway")
			if _, _, ok := tt.store.Load(); !ok {
				t.Error("Load after restoring DENY = miss, want hit")
			}
		})
	}
}

func TestDiskInventoryStoreInvalidEnvelope(t *testing.T) {
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
			store := newDiskInventoryStore(t.TempDir(), "/p")
			if err := os.WriteFile(store.path, []byte(tt.data), 0o600); err != nil {
				t.Fatalf("write: %v", err)
			}
			if _, _, ok := store.Load(); ok {
				t.Errorf("Load(%s) = hit, want miss", tt.data)
			}
		})
	}
}
