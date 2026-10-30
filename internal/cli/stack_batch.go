package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/spf13/cobra"

	"github.com/yasyf/cc-context/internal/gtmeta"
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
	holders    *vcs.HoldingCensus
	unrecorded map[string]bool
	branches   []string
}

type stackBatch struct {
	stores    map[string]*stackBatchStore
	requested map[string][]string
}

type stackBatchQuery struct {
	lane   lane
	branch string
	common string
	err    error
}

func newStackListBatchCmd() *cobra.Command {
	return &cobra.Command{
		Use:    "list-batch <worktree>...",
		Short:  "Resolve worktree stack listings from shared repository state",
		Hidden: true,
		Args:   cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, paths []string) error {
			batch := stackBatch{stores: make(map[string]*stackBatchStore), requested: make(map[string][]string)}
			queries := make([]stackBatchQuery, len(paths))
			for i, path := range paths {
				if err := cmd.Context().Err(); err != nil {
					return err
				}
				queries[i] = resolveStackBatchQuery(cmd.Context(), path)
				if query := queries[i]; query.err == nil {
					batch.requested[query.common] = append(batch.requested[query.common], query.branch)
				}
			}
			results := make([]stackBatchResult, 0, len(paths))
			for i, path := range paths {
				if err := cmd.Context().Err(); err != nil {
					return err
				}
				listing, err := batch.listQuery(cmd.Context(), queries[i])
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

func resolveStackBatchQuery(ctx context.Context, path string) stackBatchQuery {
	l, err := resolveLaneReport(ctx, "stack list", path, true, false)
	if err != nil {
		return stackBatchQuery{err: err}
	}
	branch, err := gitCurrentBranch(ctx, l.dir(), "stack list")
	if err != nil {
		return stackBatchQuery{err: err}
	}
	if branch == "" {
		return stackBatchQuery{err: fmt.Errorf("stack list: detached HEAD; no stack to resolve")}
	}
	common, err := gtCommonDir(ctx, l.dir(), "stack list")
	return stackBatchQuery{lane: l, branch: branch, common: common, err: err}
}

func (b *stackBatch) list(ctx context.Context, path string) (*stackListReport, error) {
	query := resolveStackBatchQuery(ctx, path)
	if b.requested == nil {
		b.requested = make(map[string][]string)
	}
	b.requested[query.common] = append(b.requested[query.common], query.branch)
	return b.listQuery(ctx, query)
}

func (b *stackBatch) listQuery(ctx context.Context, query stackBatchQuery) (*stackListReport, error) {
	if query.err != nil {
		return nil, query.err
	}
	l, branch, common := query.lane, query.branch, query.common
	store, err := b.store(ctx, common)
	if err != nil {
		return nil, err
	}
	_, tracked := store.state[branch]
	orphan := slices.ContainsFunc(store.orphans, func(o gtmeta.Orphan) bool { return o.Branch == branch })
	if !tracked && !orphan {
		store.tracked, store.orphans, err = gtmeta.ReadOriginOrphans(ctx, common)
		if err != nil {
			return nil, fmt.Errorf("stack list: %w", err)
		}
		store.state = gtBranchStates(store.tracked)
		store.children = gtChildren(store.state)
	}
	if err := store.focus(ctx, common, branch); err != nil {
		return nil, err
	}
	trunk, err := gtTrunkBranch("stack list", store.state)
	if err != nil {
		return nil, err
	}
	_, err = gtDownstack("stack list", store.state, branch, trunk)
	if untracked := (*errGTUntracked)(nil); errors.As(err, &untracked) {
		tracked, _, err := gtmeta.ReadOriginOrphans(ctx, common)
		if err != nil {
			return nil, fmt.Errorf("stack list: %w", err)
		}
		return nil, gtUntrackedRefusal(ctx, l.dir(), "stack list", gtBranchStates(tracked), trunk, branch)
	}
	if err != nil {
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
	store := &stackBatchStore{unrecorded: make(map[string]bool), branches: b.requested[common]}
	if err := store.reload(ctx, common); err != nil {
		return nil, err
	}
	b.stores[common] = store
	return store, nil
}

func (s *stackBatchStore) holdersFor(ctx context.Context, common string, checkout vcs.Checkout) (map[string]string, error) {
	if s.holders == nil {
		registered := checkout
		registered.GitDir = common
		var err error
		s.holders, err = vcs.CaptureHoldings(ctx, registered)
		if err != nil {
			return nil, fmt.Errorf("stack list: %w", err)
		}
	}
	holders, err := s.holders.BranchHolders(ctx, checkout)
	if err != nil {
		return nil, fmt.Errorf("stack list: %w", err)
	}
	return holders, nil
}

func (s *stackBatchStore) reload(ctx context.Context, common string) error {
	var err error
	s.tracked, s.orphans, err = gtmeta.ReadOriginOrphansForBranches(ctx, common, s.branches)
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
