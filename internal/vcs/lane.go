package vcs

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

const laneFile = "ccx-lane"

// Lane names c's lane: the id JoinLane recorded in c's admin dir, or c's own
// root while no working copy has joined it.
func Lane(c Checkout) (string, error) {
	id, err := recordedLane(c)
	if err != nil || id != "" {
		return id, err
	}
	return c.Root, nil
}

// JoinLane puts the linked working copy child into src's lane, minting the
// lane's id into src when src founded it. The id outlives no admin dir, so a
// working copy recreated at a founder's path founds a lane of its own.
func JoinLane(src, child Checkout) error {
	id, err := recordedLane(src)
	if err != nil {
		return err
	}
	if id == "" {
		id = rand.Text()
		if err := writeLane(src, id); err != nil {
			return err
		}
	}
	return writeLane(child, id)
}

func recordedLane(c Checkout) (string, error) {
	if c.Shape == ShapeJJWorkspace {
		return "", nil
	}
	data, err := os.ReadFile(filepath.Join(c.GitDir, laneFile)) //nolint:gosec // a file in the checkout's own admin dir, not untrusted input
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read the lane of %s: %w", c.Root, err)
	}
	return strings.TrimSpace(string(data)), nil
}

func writeLane(c Checkout, id string) error {
	if err := os.WriteFile(filepath.Join(c.GitDir, laneFile), []byte(id+"\n"), 0o600); err != nil {
		return fmt.Errorf("record the lane of %s: %w", c.Root, err)
	}
	return nil
}

// ForeignBranchHolders is BranchHolders less the working copies of c's lane,
// c included. A pruned working copy has no admin dir left to name its lane, so
// it stays another lane's.
func ForeignBranchHolders(ctx context.Context, c Checkout) (map[string]string, error) {
	lane, err := Lane(c)
	if err != nil {
		return nil, err
	}
	list, err := Worktrees(ctx, c)
	if err != nil {
		return nil, err
	}
	holders := make(map[string]string)
	for _, wt := range list {
		if wt.Branch == "" {
			continue
		}
		if wt.Prunable == "" {
			holder, err := ResolveCheckout(wt.Path)
			if err != nil {
				return nil, err
			}
			theirs, err := Lane(holder)
			if err != nil {
				return nil, err
			}
			if theirs == lane {
				continue
			}
		}
		holders[wt.Branch] = wt.Path
	}
	return holders, nil
}
