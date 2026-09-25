package ripgrep

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/yasyf/cc-context/internal/backend"
	"github.com/yasyf/cc-context/internal/render"
)

func searchPlans(eng engine, dir render.Dir, a backend.Args) ([]backend.Args, error) {
	if eng != engineRipgrep || len(a.Paths) > 0 {
		return []backend.Args{a}, nil
	}
	var roots, includes, excludes []string
	for _, glob := range a.Globs {
		if strings.HasPrefix(glob, "!") {
			excludes = append(excludes, glob)
			continue
		}
		if len(excludes) > 0 {
			return []backend.Args{a}, nil
		}
		root, recursive := strings.CutSuffix(glob, "/**")
		if !recursive || root == "" || strings.ContainsAny(root, "*?[]{}") {
			includes = append(includes, glob)
			continue
		}
		absolute := root
		if !filepath.IsAbs(absolute) {
			absolute = filepath.Join(string(dir), root)
		}
		info, err := os.Stat(absolute)
		if err != nil || !info.IsDir() {
			includes = append(includes, glob)
			continue
		}
		roots = append(roots, root)
	}
	if len(roots) == 0 {
		return []backend.Args{a}, nil
	}
	var kept []string
	for _, root := range roots {
		selected, err := backend.MatchGlobs(filepath.ToSlash(root)+"/", excludes)
		if err != nil {
			return nil, err
		}
		if selected {
			kept = append(kept, root)
		}
	}
	var plans []backend.Args
	if len(kept) > 0 {
		scoped := a
		scoped.Paths = kept
		scoped.Globs = excludes
		plans = append(plans, scoped)
	}
	if len(includes) > 0 {
		filtered := a
		filtered.Globs = append(includes, excludes...)
		plans = append(plans, filtered)
	}
	return plans, nil
}

func mergeGroups(dir render.Dir, parts [][]fileGroup) []fileGroup {
	var out []fileGroup
	byPath := map[string]int{}
	seen := map[string]map[grepLine]bool{}
	for _, groups := range parts {
		for _, g := range groups {
			key := g.path
			if !filepath.IsAbs(key) {
				key = filepath.Join(string(dir), key)
			}
			key = filepath.Clean(key)
			slot, found := byPath[key]
			if !found {
				slot = len(out)
				byPath[key] = slot
				out = append(out, fileGroup{path: g.path})
				seen[key] = map[grepLine]bool{}
			}
			for _, line := range g.lines {
				if !seen[key][line] {
					seen[key][line] = true
					out[slot].lines = append(out[slot].lines, line)
				}
			}
		}
	}
	return out
}
