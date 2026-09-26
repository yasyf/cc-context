package format

import (
	"os"
	"path/filepath"
	"testing"
)

// TestMain points the wazero compilation cache at a fixed dir under os.TempDir
// so the suite never writes the developer's real one. It has to be
// process-scoped rather than per-test: the engine is a singleton and the first
// test to reach it pins the dir for the process lifetime, so a t.TempDir
// removed at its own test's end would leave every later compile writing into a
// deleted directory. The dir is reused and never removed, so consecutive runs
// skip the cold compile of formatcore; wazero keys entries by module content,
// CPU features, and its own version, so a stale entry is never wrongly reused.
func TestMain(m *testing.M) {
	if err := os.Setenv("CLAUDE_PLUGIN_DATA", filepath.Join(os.TempDir(), "cc-context-test-wasm-cache")); err != nil {
		panic(err)
	}
	os.Exit(m.Run())
}
