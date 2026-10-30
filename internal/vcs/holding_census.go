package vcs

import (
	"context"
	"maps"
	"path/filepath"
)

// HoldingCensus shares one repository's complete holder discovery within a command invocation.
type HoldingCensus struct {
	holders        map[string]string
	roots          map[string]bool
	canonicalRoots map[string]bool
}

// CaptureHoldings preserves discovered holder spellings and their branch precedence.
func CaptureHoldings(ctx context.Context, c Checkout) (*HoldingCensus, error) {
	list, err := registeredHoldings(ctx, c)
	if err != nil {
		return nil, err
	}
	census := &HoldingCensus{holders: make(map[string]string), roots: make(map[string]bool)}
	for _, holding := range list {
		census.roots[holding.Path] = true
		if holding.Branch != "" {
			census.holders[holding.Branch] = holding.Path
		}
	}
	return census, nil
}

// BranchHolders includes a checkout outside the discoverable admin directories without changing the shared census.
func (c *HoldingCensus) BranchHolders(ctx context.Context, checkout Checkout) (map[string]string, error) {
	if checkout.GitDir == checkout.CommonDir || filepath.Dir(checkout.GitDir) == filepath.Join(checkout.CommonDir, "worktrees") || c.contains(checkout.Root) {
		return c.holders, nil
	}
	branch, err := unlistedBranch(ctx, checkout)
	if err != nil {
		return nil, err
	}
	if branch == "" {
		return c.holders, nil
	}
	holders := maps.Clone(c.holders)
	holders[branch] = checkout.Root
	return holders, nil
}

func (c *HoldingCensus) contains(root string) bool {
	if c.roots[root] {
		return true
	}
	if c.canonicalRoots == nil {
		c.canonicalRoots = make(map[string]bool, len(c.roots))
		for path := range c.roots {
			c.canonicalRoots[canonical(path)] = true
		}
	}
	return c.canonicalRoots[root]
}
