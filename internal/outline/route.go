// Package outline is the front door for `ccx code outline`: it selects the engine
// that serves a path. Both the CLI and the MCP handler route through it so the
// two surfaces behave identically.
package outline

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/yasyf/cc-context/anchor"
	"github.com/yasyf/cc-context/internal/backend"
)

// astGrepLangByExt maps the file extensions ast-grep 0.44.0 outlines to the
// --lang value that outlines them (Go, Python, TypeScript/TSX, JavaScript, Rust,
// Java, Kotlin). Verified against the vendored binary; languages it merely
// recognizes but has no outline rules for (Ruby, C, C++, C#, PHP, and the
// config/markup family) are absent and so route to the native fallback
// (internal/outline/fallback.go). Revisit on a vendor version bump. The ast-grep
// anchor rewrites in internal/astgrep/outline.go are keyed to its output grammar;
// TestContentAnchorsSurviveEngineGrammar (internal/cli) is the drift canary.
var astGrepLangByExt = map[string]string{
	".go":   "go",
	".py":   "python",
	".pyi":  "python",
	".ts":   "typescript",
	".mts":  "typescript",
	".cts":  "typescript",
	".tsx":  "tsx",
	".js":   "javascript",
	".mjs":  "javascript",
	".cjs":  "javascript",
	".jsx":  "javascript",
	".rs":   "rust",
	".java": "java",
	".kt":   "kotlin",
	".kts":  "kotlin",
}

// LangForExt returns the ast-grep --lang value that outlines path's extension,
// with ok=false when ast-grep has no outline rules for it. It is the single ext→
// lang source both outline.Route and the native diff's blob outlining consult.
func LangForExt(path string) (lang string, ok bool) {
	lang, ok = astGrepLangByExt[strings.ToLower(filepath.Ext(path))]
	return lang, ok
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

// Route resolves a.Path and selects the op that serves it. A directory always
// routes to ast-grep (OpStructOutline). A file routes to ast-grep when its
// language is in ast-grep's outline catalog and to the native fallback
// (OpOutline) otherwise.
func Route(a *backend.Args) (backend.Op, string, error) {
	resolved, note, err := backend.ResolvePath(backend.OpStructural, backend.Args{Paths: []string{a.Path}})
	if err != nil {
		return "", "", fmt.Errorf("outline: %w", err)
	}
	a.Path = resolved.Paths[0]
	info, err := os.Stat(a.Path)
	if errors.Is(err, fs.ErrNotExist) {
		return "", "", fmt.Errorf("outline %q: %w: %w", a.Path, backend.ErrPathNotFound, err)
	}
	if err != nil {
		return "", "", fmt.Errorf("outline: %w", err)
	}
	if info.IsDir() {
		return backend.OpStructOutline, note, nil
	}
	if a.Lang != "" {
		if _, ok := astGrepLangs[strings.ToLower(a.Lang)]; ok {
			return backend.OpStructOutline, note, nil
		}
		return backend.OpOutline, note, nil
	}
	if _, ok := LangForExt(a.Path); ok {
		return backend.OpStructOutline, note, nil
	}
	return backend.OpOutline, note, nil
}

// ValidateSection guards `ccx code outline --section` and returns the 1-indexed
// inclusive [start, end] window it resolves to. A line window applies only to a
// single-file structural (ast-grep) outline: it returns (0, 0, nil) when no
// section is set, otherwise a precise error — naming the range grammar, rejecting
// a reversed or directory window, or pointing a fallback-outlined file at
// ccx code read — so no caller runs an outline that cannot honor the window. op
// is the engine outline.Route picked for a. It is the single validation point for
// every surface (CLI, MCP, exec, and the struct-outline runner).
func ValidateSection(a backend.Args, op backend.Op) (start, end int, err error) {
	if a.Section == "" {
		return 0, 0, nil
	}
	start, end, ok, rangeErr := anchor.ParseNumericRange(a.Section)
	if rangeErr != nil {
		return 0, 0, fmt.Errorf("outline --section: %w; window a single file with ccx code read %s --section", rangeErr, a.Path)
	}
	if !ok {
		return 0, 0, fmt.Errorf("outline --section %q is not a line range (%q or %q); for a heading or anchor read the file with ccx code read %s --section", a.Section, "A-B", "A,B", a.Path)
	}
	info, statErr := os.Stat(a.Path)
	if statErr != nil {
		return 0, 0, fmt.Errorf("outline: %w", statErr)
	}
	if info.IsDir() {
		return 0, 0, fmt.Errorf("outline --section windows a single file, not the directory %s; pass a file path", a.Path)
	}
	if op == backend.OpOutline {
		return 0, 0, fmt.Errorf("outline --section windows structured (ast-grep) outlines; %s outlines via the native fallback — read a line window with ccx code read %s --section %s", a.Path, a.Path, a.Section)
	}
	return start, end, nil
}
