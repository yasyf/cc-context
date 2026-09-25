package format

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/yasyf/cc-context/internal/render"
)

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
	if _, err := loadEngine(t.Context()); err == nil {
		t.Fatal("loadEngine(t.Context()) with unloadable wasm: want error, got nil")
	}
	engineMu.Lock()
	cached := engineInst
	engineMu.Unlock()
	if cached != nil {
		t.Fatal("loadEngine(t.Context()) cached an engine after a failed init")
	}

	wasmModule = savedBytes
	eng, err := loadEngine(t.Context())
	if err != nil {
		t.Fatalf("loadEngine(t.Context()) retry after failure: %v", err)
	}
	if eng == nil {
		t.Fatal("loadEngine(t.Context()) retry returned a nil engine")
	}
}

// TestInitEngineResolvesTheCacheDirOffTheContext proves the wasm compilation
// cache lands under the $CLAUDE_PLUGIN_DATA the context carries rather than the
// process's: the two name different directories here, and only the context's
// ends up holding wazero's artifacts.
func TestInitEngineResolvesTheCacheDirOffTheContext(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	if root == os.Getenv("CLAUDE_PLUGIN_DATA") {
		t.Fatalf("fixture root %q is the process value; the test cannot discriminate", root)
	}

	eng, err := initEngine(render.WithEnv(t.Context(), "CLAUDE_PLUGIN_DATA="+root))
	if err != nil {
		t.Fatalf("initEngine: %v", err)
	}
	defer func() { _ = eng.runtime.Close(context.WithoutCancel(t.Context())) }()

	entries, err := os.ReadDir(filepath.Join(root, "wasm"))
	if err != nil {
		t.Fatalf("read the context's wasm cache dir: %v", err)
	}
	if len(entries) == 0 {
		t.Error("the context's wasm cache dir is empty; the compile resolved some other root")
	}
}
