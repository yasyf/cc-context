package format

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/yasyf/cc-context/internal/render"
)

// TestEngineLoaderRetriesAfterFailedInit proves a loader caches only success: a
// failed compile leaves its engine uninitialized so a later call retries, rather
// than pinning the error for the process lifetime the way a plain sync.Once
// would. Unloadable module bytes force the failure without a mock.
func TestEngineLoaderRetriesAfterFailedInit(t *testing.T) {
	t.Parallel()
	loader := &engineLoader{module: []byte("\x00not a wasm module")}

	if _, err := loader.load(t.Context()); err == nil {
		t.Fatal("load with unloadable wasm: want error, got nil")
	}
	if loader.engine != nil {
		t.Fatal("load cached an engine after a failed init")
	}

	loader.module = wasmModule
	eng, err := loader.load(t.Context())
	if err != nil {
		t.Fatalf("load retry after failure: %v", err)
	}
	if eng == nil {
		t.Fatal("load retry returned a nil engine")
	}
	t.Cleanup(func() { _ = eng.runtime.Close(context.WithoutCancel(t.Context())) })
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

	eng, err := initEngine(render.WithEnv(t.Context(), "CLAUDE_PLUGIN_DATA="+root), wasmModule)
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
