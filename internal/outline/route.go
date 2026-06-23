// Package outline is the front door for `ccx outline`: it selects the engine
// that serves a path. Both the CLI and the MCP handler route through it so the
// two surfaces behave identically.
package outline

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/yasyf/cc-context/internal/backend"
)

// astGrepExts are the file extensions ast-grep 0.44.0 outlines (Go, Python,
// TypeScript/TSX, JavaScript, Rust, Java, Kotlin). Verified against the vendored
// binary; languages it merely recognizes but has no outline rules for (Ruby, C,
// C++, C#, PHP, and the config/markup family) return empty and so route to tilth.
// Revisit on a vendor version bump.
var astGrepExts = map[string]struct{}{
	".go":   {},
	".py":   {},
	".pyi":  {},
	".ts":   {},
	".mts":  {},
	".cts":  {},
	".tsx":  {},
	".js":   {},
	".mjs":  {},
	".cjs":  {},
	".jsx":  {},
	".rs":   {},
	".java": {},
	".kt":   {},
	".kts":  {},
}

// astGrepLangs are the ast-grep --lang values whose outlines ast-grep serves,
// used when a.Lang forces a language instead of inferring it from the extension.
var astGrepLangs = map[string]struct{}{
	"go":         {},
	"python":     {},
	"typescript": {},
	"tsx":        {},
	"javascript": {},
	"js":         {},
	"ts":         {},
	"rust":       {},
	"java":       {},
	"kotlin":     {},
}

// Route selects the op that serves a.Path. A directory always routes to ast-grep
// (OpStructOutline) — tilth outlines a single file only. A file routes to
// ast-grep when its language is in ast-grep's outline catalog and to tilth
// signature mode (OpOutline) otherwise, so an unsupported language is served by
// the engine that can outline it rather than silently returning nothing.
func Route(a backend.Args) (backend.Op, error) {
	info, err := os.Stat(a.Path)
	if err != nil {
		return "", fmt.Errorf("outline: %w", err)
	}
	if info.IsDir() {
		return backend.OpStructOutline, nil
	}
	if a.Lang != "" {
		if _, ok := astGrepLangs[strings.ToLower(a.Lang)]; ok {
			return backend.OpStructOutline, nil
		}
		return backend.OpOutline, nil
	}
	if _, ok := astGrepExts[strings.ToLower(filepath.Ext(a.Path))]; ok {
		return backend.OpStructOutline, nil
	}
	return backend.OpOutline, nil
}
