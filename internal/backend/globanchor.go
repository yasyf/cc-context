package backend

import "strings"

const globMeta = "*?[]{}"

// SplitGlobAnchor peels glob's longest leading run of metacharacter-free segments
// off as dir, leaving the remaining glob as rest. dir is empty (no anchor) for
// exclusion globs ("!…"), slash-less globs, and globs whose first segment already
// carries a metacharacter (e.g. a leading "**"). Absolute prefixes are preserved.
func SplitGlobAnchor(glob string) (dir, rest string) {
	if strings.HasPrefix(glob, "!") || !strings.Contains(glob, "/") {
		return "", glob
	}
	segs := strings.Split(glob, "/")
	split := len(segs)
	for i, s := range segs {
		if strings.ContainsAny(s, globMeta) {
			split = i
			break
		}
	}
	if split == 0 {
		return "", glob
	}
	return strings.Join(segs[:split], "/"), strings.Join(segs[split:], "/")
}
