package format

import "testing"

// TestLoadEngineRetriesAfterFailedInit proves loadEngine caches only success: a
// failed compile leaves the engine uninitialized so a later call retries, rather
// than pinning the error for the process lifetime the way a plain sync.Once
// would. The seam is the package-level wasmModule var — swapping in unloadable
// bytes forces initEngine to fail without a mock.
func TestLoadEngineRetriesAfterFailedInit(t *testing.T) {
	engineMu.Lock()
	savedInst, savedBytes := engineInst, wasmModule
	engineInst = nil
	engineMu.Unlock()
	t.Cleanup(func() {
		engineMu.Lock()
		engineInst, wasmModule = savedInst, savedBytes
		engineMu.Unlock()
	})

	wasmModule = []byte("\x00not a wasm module")
	if _, err := loadEngine(); err == nil {
		t.Fatal("loadEngine() with unloadable wasm: want error, got nil")
	}
	engineMu.Lock()
	cached := engineInst
	engineMu.Unlock()
	if cached != nil {
		t.Fatal("loadEngine() cached an engine after a failed init")
	}

	wasmModule = savedBytes
	eng, err := loadEngine()
	if err != nil {
		t.Fatalf("loadEngine() retry after failure: %v", err)
	}
	if eng == nil {
		t.Fatal("loadEngine() retry returned a nil engine")
	}
}
