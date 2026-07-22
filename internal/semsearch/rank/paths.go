package rank

import "strings"

// POSIX path helpers matching Python pathlib.PurePosixPath for the repo-relative,
// forward-slash paths semble stores (index/create.py sets chunk.file_path via
// Path.relative_to; index/sparse.py and ranking/boosting.py read .stem/.parent).
// Backslash-separated paths are not handled here (penalties.go normalises them
// before its own regex checks, matching semble).

// posixParts splits a path into its non-empty, non-"." components, dropping the
// root anchor — the components pathlib exposes via .parts minus "." and "/".
func posixParts(p string) []string {
	var out []string
	for _, s := range strings.Split(p, "/") {
		if s == "" || s == "." {
			continue
		}
		out = append(out, s)
	}
	return out
}

// pathBase returns the final path component (pathlib Path.name); "" when the
// path has no real component.
func pathBase(p string) string {
	parts := posixParts(p)
	if len(parts) == 0 {
		return ""
	}
	return parts[len(parts)-1]
}

// pathStem returns the filename without its final suffix (pathlib Path.stem):
// the suffix is only stripped when its dot is neither the first nor the last
// character of the name ("foo.d.ts" → "foo.d", ".gitignore" → ".gitignore").
func pathStem(p string) string {
	name := pathBase(p)
	i := strings.LastIndexByte(name, '.')
	if i > 0 && i < len(name)-1 {
		return name[:i]
	}
	return name
}

// pathParentName returns the name of the parent directory (pathlib
// Path.parent.name); "" when the path has fewer than two components.
func pathParentName(p string) string {
	parts := posixParts(p)
	if len(parts) < 2 {
		return ""
	}
	return parts[len(parts)-2]
}

// pathParentDirs returns the directory components of a path (pathlib
// Path.parent.parts filtered of "." and "/").
func pathParentDirs(p string) []string {
	parts := posixParts(p)
	if len(parts) == 0 {
		return nil
	}
	return parts[:len(parts)-1]
}
