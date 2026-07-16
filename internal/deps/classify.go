package deps

import (
	"os"
	"path/filepath"
	"strings"
)

// classify labels name for its family and returns the text the `## uses` row
// shows. Only a deterministic rule assigns a label; a name no rule resolves is
// "unresolved" rather than guessed.
func classify(fam family, name string, cc classCtx) (class, display string) {
	switch fam {
	case familyGo:
		return classifyGo(name, cc.mod)
	case familyPython:
		return classifyPython(name, cc.root), name
	case familyJS:
		return classifyJS(name), name
	case familyRust:
		return classifyRust(name), name
	default:
		return classUnresolved, name
	}
}

// classifyGo classifies a Go import path. A path under the module prefix whose
// directory exists is local, and its display is the repo-relative path; a path
// under the prefix whose directory is missing is unresolved (it is our module, not
// a third-party one, so it is never guessed external); a first segment with no dot
// is a standard-library package; a dotted first segment is a third-party module.
func classifyGo(name string, mod goModule) (class, display string) {
	if mod.path != "" && (name == mod.path || strings.HasPrefix(name, mod.path+"/")) {
		rel := "."
		if name != mod.path {
			rel = name[len(mod.path)+1:]
		}
		if isDir(filepath.Join(mod.root, filepath.FromSlash(rel))) {
			return classLocal, rel
		}
		return classUnresolved, name
	}
	first := name
	if i := strings.IndexByte(name, '/'); i >= 0 {
		first = name[:i]
	}
	if !strings.Contains(first, ".") {
		return classStd, name
	}
	return classExternal, name
}

// classifyPython classifies a Python module. A leading-dot (relative) import, or a
// top segment that resolves to a repo directory or <name>.py under root, is local;
// everything else is unresolved — distinguishing std from third-party needs a
// stdlib inventory the scan does not carry.
func classifyPython(name, root string) string {
	if strings.HasPrefix(name, ".") {
		return classLocal
	}
	top := name
	if i := strings.IndexByte(name, '.'); i >= 0 {
		top = name[:i]
	}
	if top != "" && (isDir(filepath.Join(root, top)) || isFile(filepath.Join(root, top+".py"))) {
		return classLocal
	}
	return classUnresolved
}

// classifyJS classifies a JS/TS module specifier: a relative specifier is local,
// a bare specifier is a third-party package.
func classifyJS(name string) string {
	if strings.HasPrefix(name, "./") || strings.HasPrefix(name, "../") {
		return classLocal
	}
	return classExternal
}

// classifyRust classifies a Rust use path by its leading segment: crate/super/self
// are local, std/core/alloc are standard, and any other crate root is third-party.
func classifyRust(name string) string {
	for _, p := range []string{"crate::", "super::", "self::"} {
		if strings.HasPrefix(name, p) {
			return classLocal
		}
	}
	for _, p := range []string{"std::", "core::", "alloc::"} {
		if strings.HasPrefix(name, p) {
			return classStd
		}
	}
	return classExternal
}

// isDir reports whether path is an existing directory.
func isDir(path string) bool {
	info, err := os.Stat(path) //nolint:gosec // existence probe on a repo-relative path built from import classification, not untrusted input
	return err == nil && info.IsDir()
}

// isFile reports whether path is an existing regular file.
func isFile(path string) bool {
	info, err := os.Stat(path) //nolint:gosec // existence probe on a repo-relative path built from import classification, not untrusted input
	return err == nil && info.Mode().IsRegular()
}
