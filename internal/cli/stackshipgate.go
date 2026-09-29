package cli

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/yasyf/cc-context/internal/render"
)

// stackStanding is what a pull request has already earned on its published
// head, which a force-push throws away: a green check rollup and an approving
// review that still speaks for that head.
type stackStanding struct {
	Green    bool
	Approved bool
}

func (s stackStanding) String() string {
	var earned []string
	if s.Green {
		earned = append(earned, "green")
	}
	if s.Approved {
		earned = append(earned, "approved")
	}
	return strings.Join(earned, ", ")
}

type stackStandingQuery func(ctx context.Context, l lane, branches []string) (map[string]stackStanding, error)

type stackStandingKey struct{}

func withStackStanding(ctx context.Context, query stackStandingQuery) context.Context {
	return context.WithValue(ctx, stackStandingKey{}, query)
}

func stackStandings(ctx context.Context, l lane, branches []string) (map[string]stackStanding, error) {
	if query, ok := ctx.Value(stackStandingKey{}).(stackStandingQuery); ok {
		return query(ctx, l, branches)
	}
	return stackQueryStanding(ctx, l, branches)
}

func stackQueryStanding(ctx context.Context, l lane, branches []string) (map[string]stackStanding, error) {
	resp, err := statusQueryPRs(ctx, l, branches)
	if err != nil {
		return nil, err
	}
	standings := map[string]stackStanding{}
	for i, node := range statusNodes(resp, branches) {
		if node == nil || node.State != "OPEN" || node.Mergeable == "CONFLICTING" {
			continue
		}
		rollup := statusRollupOf(*node)
		standings[branches[i]] = stackStanding{
			Green: rollup != nil && rollup.State == "SUCCESS",
			Approved: slices.ContainsFunc(statusReviews(*node), func(r statusReview) bool {
				return r.State == "APPROVED" && !r.Stale
			}),
		}
	}
	return standings, nil
}

// stackRefuseGreenRestack refuses a ship whose plan would force-push a pull
// request below the tip that is green or approved only to move it onto a new
// base, which restarts its CI and dismisses its approvals for no change of its
// own. A queued one never reaches here: the plan already keeps it at its
// published head.
func stackRefuseGreenRestack(ctx context.Context, l lane, run *stackRebaseRun) error {
	restacks, err := stackRestacks(ctx, l.dir(), run)
	if err != nil || len(restacks) == 0 {
		return err
	}
	names := make([]string, 0, len(restacks))
	for _, b := range restacks {
		names = append(names, b.Name)
	}
	standings, err := stackStandings(ctx, l, names)
	if err != nil {
		return fmt.Errorf("ship: read the standing of the pull requests a restack would move: %w", err)
	}
	var earned []string
	for _, b := range restacks {
		if s := standings[b.Name]; s.Green || s.Approved {
			earned = append(earned, fmt.Sprintf("%s #%d (%s)", b.Name, b.PR.Number, s))
		}
	}
	if len(earned) == 0 {
		return nil
	}
	return refuse("ship: shipping %s would restack %s onto a new base, restarting CI and dismissing approvals they already have — rerun with --tip-only to ship %s alone onto its parent's published head, or with --restack to move them anyway",
		run.Tip, strings.Join(earned, ", "), run.Tip)
}

// stackRestacks names the published branches below the tip that the run would
// move onto a new base when nothing of their own changed: trunk moved under
// them, a parent landed, or a parent moved for either reason. A branch
// carrying local commits its published head lacks is the author's own change,
// and so is everything the run moves above it; one GitHub already reports as
// conflicting must move to merge at all.
func stackRestacks(ctx context.Context, dir render.Dir, run *stackRebaseRun) ([]*stackRebaseBranch, error) {
	heads := map[string]string{run.Trunk: run.Pin}
	moved, fresh := map[string]bool{}, map[string]bool{}
	var restacks []*stackRebaseBranch
	for i := range run.Branches {
		b := &run.Branches[i]
		if b.Landed != "" {
			continue
		}
		heads[b.Name] = b.Head
		if b.Kept {
			continue
		}
		own, err := stackCarriesOwnWork(ctx, dir, run.Pin, b)
		if err != nil {
			return nil, err
		}
		moves := b.Held == "" && (moved[b.Parent] || heads[b.Parent] != b.OldBase)
		moved[b.Name], fresh[b.Name] = moves, own || (moves && fresh[b.Parent])
		if moves && !fresh[b.Name] && b.Name != run.Tip && !b.LocalOnly && b.PR != nil && b.PR.State == "OPEN" && b.PR.Mergeable != "CONFLICTING" {
			restacks = append(restacks, b)
		}
	}
	return restacks, nil
}

// stackCarriesOwnWork reports whether a branch's local head holds work its
// published head does not; a head that only replays the published commits
// onto newer trunk, as a local restack leaves it, holds none.
func stackCarriesOwnWork(ctx context.Context, dir render.Dir, pin string, b *stackRebaseBranch) (bool, error) {
	if b.Remote == "" {
		return true, nil
	}
	if b.Head == b.Remote {
		return false, nil
	}
	replays, err := stackRemoteReplays(ctx, dir, b.Head, b.Remote, pin)
	return !replays, err
}
