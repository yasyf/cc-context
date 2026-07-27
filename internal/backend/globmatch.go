package backend

import (
	"fmt"
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
