package backend

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/bmatcuk/doublestar/v4"
)

type globVerdict int

const (
	globUnmatched globVerdict = iota
	globIncluded
	globExcluded
)

type globRule struct {
	pattern  string
	negated  bool
	dirOnly  bool
	basename bool
}

func compileGlobRule(g string) globRule {
	pattern, negated := strings.CutPrefix(g, "!")
	pattern, dirOnly := strings.CutSuffix(pattern, "/")
	// doublestar's trailing "/**" also matches the bare prefix; rg's does not.
	if strings.HasSuffix(pattern, "/**") {
		pattern += "/*"
	}
	return globRule{
		pattern:  pattern,
		negated:  negated,
		dirOnly:  dirOnly,
		basename: !strings.Contains(pattern, "/"),
	}
}

func (r globRule) match(target string) (bool, error) {
	if r.basename {
		target = path.Base(target)
	}
	ok, err := doublestar.Match(r.pattern, target)
	if err != nil {
		return false, fmt.Errorf("glob %q: %w", r.pattern, err)
	}
	return ok, nil
}

func lastVerdict(rules []globRule, target string, isDir bool) (globVerdict, error) {
	for i := len(rules) - 1; i >= 0; i-- {
		r := rules[i]
		if r.dirOnly && !isDir {
			continue
		}
		ok, err := r.match(target)
		if err != nil {
			return globUnmatched, err
		}
		if ok {
			if r.negated {
				return globExcluded, nil
			}
			return globIncluded, nil
		}
	}
	return globUnmatched, nil
}

// MatchGlobs reports whether repo-relative p is selected by an ordered ripgrep
// -g list, textually — no stat, so a deleted path still decides. Last match
// wins; any include turns the list into a whitelist, an exclusion-only list
// selects what it does not exclude, an empty list everything. A slash-less glob
// matches the basename at any depth, a slashed one the whole path.
//
// An exclusion matching an ancestor directory prunes that subtree; a later
// include cannot undo it, though each ancestor votes last-match-wins itself.
func MatchGlobs(p string, globs []string) (bool, error) {
	if len(globs) == 0 {
		return true, nil
	}
	rules := make([]globRule, len(globs))
	hasInclude := false
	for i, g := range globs {
		rules[i] = compileGlobRule(g)
		hasInclude = hasInclude || !rules[i].negated
	}
	p = filepath.ToSlash(p)
	for i, c := range p {
		if c != '/' {
			continue
		}
		v, err := lastVerdict(rules, p[:i], true)
		if err != nil {
			return false, err
		}
		if v == globExcluded {
			return false, nil
		}
	}
	v, err := lastVerdict(rules, p, false)
	if err != nil {
		return false, err
	}
	switch v {
	case globIncluded:
		return true, nil
	case globExcluded:
		return false, nil
	default:
		return !hasInclude, nil
	}
}

// SharedGlobAnchor returns the literal directory prefix every include glob
// carries, or "" when the set holds no include, one carries no anchor, or two
// anchor differently — an engine applies every glob under every operand, so
// disjoint anchors have no single directory to peel into one. Exclusions never
// anchor and are skipped.
func SharedGlobAnchor(globs []string) string {
	var anchor string
	for _, g := range globs {
		if strings.HasPrefix(g, "!") {
			continue
		}
		dir, _ := SplitGlobAnchor(g)
		if dir == "" || (anchor != "" && dir != anchor) {
			return ""
		}
		anchor = dir
	}
	return anchor
}

// FilterGlobPaths keeps the explicit operands globs select, for engines whose
// glob flag ignores explicit operands (rg's -g and ast-grep's --globs both do).
// A directory passes through for the engine's own recursion; a regular file
// survives only when MatchGlobs keeps it. Filtering every operand away is a loud
// clean no-match.
func FilterGlobPaths(paths, globs []string) ([]string, error) {
	var kept []string
	for _, p := range paths {
		info, err := os.Stat(p)
		if err != nil || info.IsDir() {
			kept = append(kept, p)
			continue
		}
		ok, err := MatchGlobs(p, globs)
		if err != nil {
			return nil, err
		}
		if ok {
			kept = append(kept, p)
		}
	}
	if len(kept) == 0 {
		return nil, fmt.Errorf("no paths match --glob %q", globs)
	}
	return kept, nil
}

// SingleGlob wraps the single-valued glob the MCP and exec wire formats still
// carry into the ordered list Args.Globs takes; an empty string yields none.
func SingleGlob(g string) []string {
	if g == "" {
		return nil
	}
	return []string{g}
}
