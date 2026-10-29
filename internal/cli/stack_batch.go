package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/yasyf/cc-context/internal/gtmeta"
	"github.com/yasyf/cc-context/internal/render"
	"github.com/yasyf/cc-context/internal/vcs"
)

type stackBatchResult struct {
	Path    string           `json:"path"`
	Listing *stackListReport `json:"listing"`
	Error   string           `json:"error,omitempty"`
}

type stackBatchStore struct {
	tracked    gtmeta.State
	orphans    []gtmeta.Orphan
	state      gtState
	children   map[string][]string
	holders    map[string]string
	unrecorded map[string]bool
}

type stackBatch struct {
	stores map[string]*stackBatchStore
}

func newStackListBatchCmd() *cobra.Command {
	return &cobra.Command{
		Use:    "list-batch <worktree>...",
		Short:  "Resolve worktree stack listings from shared repository state",
		Hidden: true,
		Args:   cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, paths []string) error {
			batch := stackBatch{stores: make(map[string]*stackBatchStore)}
			results := make([]stackBatchResult, 0, len(paths))
			for _, path := range paths {
				if err := cmd.Context().Err(); err != nil {
					return err
				}
				listing, err := batch.list(cmd.Context(), path)
				result := stackBatchResult{Path: path, Listing: listing}
				if err != nil {
					result.Error = err.Error()
				}
				results = append(results, result)
			}
			data, err := json.Marshal(results)
			if err != nil {
				return fmt.Errorf("stack list: marshal batch: %w", err)
			}
			cmd.Println(string(data))
			return nil
		},
	}
}

func (b *stackBatch) list(ctx context.Context, path string) (*stackListReport, error) {
	l, err := resolveLaneReport(ctx, "stack list", path, true, false)
	if err != nil {
		return nil, err
	}
	branch, err := gitCurrentBranch(ctx, l.dir(), "stack list")
	if err != nil {
		return nil, err
	}
	if branch == "" {
		return nil, fmt.Errorf("stack list: detached HEAD; no stack to resolve")
	}
	common, err := gtCommonDir(ctx, l.dir(), "stack list")
	if err != nil {
		return nil, err
	}
	store, err := b.store(ctx, common)
	if err != nil {
		return nil, err
	}
	if err := store.focus(ctx, common, branch); err != nil {
		return nil, err
	}
	stack, err := gtStackFromState(ctx, l.dir(), "stack list", store.state, branch, store.children)
	if err != nil {
		return nil, err
	}
	report := &stackListReport{Root: l.checkout.Root, Branches: make([]stackListEntry, 0, len(stack))}
	holders, err := store.holdersFor(ctx, common, l.checkout)
	if err != nil {
		return nil, err
	}
	for _, name := range stack {
		unrecorded, known := store.unrecorded[name]
		if !known {
			unrecorded, err = gtUnrecorded(common, name)
			if err != nil {
				return nil, fmt.Errorf("stack list: %w", err)
			}
			store.unrecorded[name] = unrecorded
		}
		report.Branches = append(report.Branches, stackListEntry{
			Branch: name, Path: holders[name], Current: holders[name] == l.checkout.Root,
			NeedsRestack: store.state[name].NeedsRestack, NeedsSubmit: unrecorded, State: store.state[name].State,
		})
	}
	return report, nil
}

func (b *stackBatch) store(ctx context.Context, common string) (*stackBatchStore, error) {
	if store := b.stores[common]; store != nil {
		return store, nil
	}
	store := &stackBatchStore{unrecorded: make(map[string]bool)}
	if err := store.reload(ctx, common); err != nil {
		return nil, err
	}
	b.stores[common] = store
	return store, nil
}

func (s *stackBatchStore) holdersFor(ctx context.Context, common string, checkout vcs.Checkout) (map[string]string, error) {
	if checkout.GitDir != checkout.CommonDir && filepath.Dir(checkout.GitDir) != filepath.Join(checkout.CommonDir, "worktrees") {
		holders, err := vcs.BranchHolders(ctx, checkout)
		if err != nil {
			return nil, fmt.Errorf("stack list: %w", err)
		}
		return holders, nil
	}
	if s.holders == nil {
		registered := checkout
		registered.GitDir = common
		var err error
		s.holders, err = vcs.BranchHolders(ctx, registered)
		if err != nil {
			return nil, fmt.Errorf("stack list: %w", err)
		}
	}
	return s.holders, nil
}

func (s *stackBatchStore) reload(ctx context.Context, common string) error {
	var err error
	s.tracked, s.orphans, err = gtmeta.ReadOriginOrphans(ctx, common)
	if err != nil {
		return fmt.Errorf("stack list: %w", err)
	}
	s.state = gtBranchStates(s.tracked)
	s.children = gtChildren(s.state)
	return nil
}

func (s *stackBatchStore) focus(ctx context.Context, common, branch string) error {
	if len(s.orphans) == 0 {
		return nil
	}
	orphans, err := gtRelatedOrphans(ctx, common, "stack list", s.tracked, s.orphans, branch, "")
	if err != nil {
		return err
	}
	adopted, err := gtAdoptOrphans(ctx, common, "stack list", s.tracked, orphans)
	if err != nil {
		return err
	}
	if adopted {
		return s.reload(ctx, common)
	}
	return nil
}
