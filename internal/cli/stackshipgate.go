package cli

import (
	"context"
	"fmt"
	"slices"
	"strconv"
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

// stackRefuseStalePins refuses a run that would publish branches onto one kept
// at its published head while the run moves that branch's own parent: the kept
// head no longer holds its parent, so everything stacked on it lands on a
// stale base.
func stackRefuseStalePins(ctx context.Context, dir render.Dir, run *stackRebaseRun) error {
	if run.NoPush {
		return nil
	}
	heads := map[string]string{run.Trunk: run.Pin}
	moved := map[string]bool{}
	byName := map[string]*stackRebaseBranch{}
	for i := range run.Branches {
		b := &run.Branches[i]
		byName[b.Name] = b
		heads[b.Name] = b.Head
		if b.Landed == "" && b.Held == "" && !b.Kept && !b.Stays {
			moved[b.Name] = moved[b.Parent] || heads[b.Parent] != b.OldBase
		}
	}
	for _, b := range run.Branches {
		parent := byName[b.Parent]
		if !b.Pinned || parent == nil || parent.Landed != "" || parent.Held != "" || parent.Kept || parent.Stays {
			continue
		}
		holds := !moved[parent.Name]
		if holds {
			var err error
			if holds, err = gitIsAncestor(ctx, dir, stackRebasePrefix, parent.Head, b.Head); err != nil {
				return err
			}
		}
		if holds {
			continue
		}
		above := map[string]bool{b.Name: true}
		var names []string
		for _, up := range run.Branches {
			if above[up.Parent] {
				above[up.Name] = true
				names = append(names, up.Name)
			}
		}
		return refuse("%s: %s is another lane's, kept at its published head, and that head does not hold the head this run publishes for %s — publishing %s would stack them on a stale %s, so nothing was pushed; pass --include %s to restack it with the run, or --to %s to stop below it",
			stackRebasePrefix, b.Name, parent.Name, strings.Join(names, ", "), b.Name, b.Name, parent.Name)
	}
	return nil
}

// stackRefuseUnpublishedPins refuses a run that would publish a branch stacked
// on one kept at its published head while its own history carries work of that
// branch origin lacks: the run replays it onto the published head and drops
// that work from what it pushes. Work that only replays the published commits
// onto newer trunk is no loss.
func stackRefuseUnpublishedPins(ctx context.Context, dir render.Dir, run *stackRebaseRun) error {
	if run.NoPush {
		return nil
	}
	byName := map[string]*stackRebaseBranch{}
	for i := range run.Branches {
		byName[run.Branches[i].Name] = &run.Branches[i]
	}
	for _, b := range run.Branches {
		pin := byName[b.Parent]
		if b.Landed != "" || b.Held != "" || b.Kept || pin == nil || !pin.Pinned || pin.Local == pin.Remote {
			continue
		}
		carried, err := stackCarriedUnpublished(ctx, dir, run.Pin, pin, b.Local)
		if err != nil {
			return err
		}
		if carried == 0 {
			continue
		}
		if replays, err := stackRemoteReplays(ctx, dir, pin.Remote, pin.Local, run.Pin); err != nil {
			return err
		} else if replays {
			continue
		}
		above := map[string]bool{b.Name: true}
		names := []string{b.Name}
		for _, up := range run.Branches {
			if above[up.Parent] {
				above[up.Name] = true
				names = append(names, up.Name)
			}
		}
		return refuse("%s: %s is another lane's, kept at its published head, but %s carries %d commit(s) of %s that origin does not — publishing %s onto that head would drop them, so nothing was pushed; submit %s from the working copy holding it first, or pass --include %s to publish it with the run",
			stackRebasePrefix, pin.Name, b.Name, carried, pin.Name, strings.Join(names, ", "), pin.Name, pin.Name)
	}
	return nil
}

// stackCarriedUnpublished counts the commits of pin's local head that head
// also holds and neither pin's published head nor trunk does.
func stackCarriedUnpublished(ctx context.Context, dir render.Dir, trunk string, pin *stackRebaseBranch, head string) (int, error) {
	out, code, stderr, err := render.RunCLIExitCode(ctx, dir, "git", []string{"merge-base", "--all", pin.Local, head})
	if err != nil {
		return 0, fmt.Errorf("%s: git merge-base --all %.12s %.12s: %w", stackRebasePrefix, pin.Local, head, err)
	}
	switch code {
	case 0:
	case 1:
		return 0, nil
	default:
		return 0, fmt.Errorf("%s: git merge-base --all %.12s %.12s: exit %d: %s", stackRebasePrefix, pin.Local, head, code, strings.TrimSpace(stderr))
	}
	args := append([]string{"rev-list", "--count"}, strings.Fields(out)...)
	args = append(args, "^"+trunk)
	if pin.Remote != "" {
		args = append(args, "^"+pin.Remote)
	}
	count, err := render.RunCLI(ctx, dir, "git", args)
	if err != nil {
		return 0, fmt.Errorf("%s: git rev-list --count: %w", stackRebasePrefix, err)
	}
	return strconv.Atoi(strings.TrimSpace(count))
}
