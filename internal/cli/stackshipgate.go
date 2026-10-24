package cli

import (
	"context"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"

	"github.com/yasyf/cc-context/internal/render"
	"github.com/yasyf/cc-context/internal/vcs"
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
	return refuse("ship: shipping %s would restack %s onto a new base, restarting CI and dismissing approvals they already have — rerun with --tip-only to ship %s alone, or with --restack to move them anyway",
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

// stackRefusePins refuses a run that would publish onto a branch kept at its
// published head when that head no longer serves the branches above it, naming
// every such branch and the --include set that takes them all into the run.
func stackRefusePins(ctx context.Context, w io.Writer, dir render.Dir, run *stackRebaseRun) error {
	if run.NoPush {
		return nil
	}
	if err := stackRefuseOffOwnedPins(ctx, dir, run); err != nil {
		return err
	}
	stale, reasons, err := stackStalePins(ctx, dir, run)
	if err != nil {
		return err
	}
	remote, err := vcs.GitRemoteFor(ctx, dir, "HEAD")
	if err != nil {
		return fmt.Errorf("%s: %w", stackRebasePrefix, err)
	}
	include := stale
	for {
		pins, more, err := stackUnpublishedPins(ctx, dir, run, remote, include)
		if err != nil {
			return err
		}
		if len(pins) == 0 {
			break
		}
		include, reasons = append(include, pins...), append(reasons, more...)
	}
	if run.Tip == "" {
		pins, more, warnings, err := stackConflictingPins(ctx, dir, run, include)
		if err != nil {
			return err
		}
		for _, warning := range warnings {
			if _, err := fmt.Fprintf(w, "%s: warning: %s\n", stackRebasePrefix, warning); err != nil {
				return err
			}
		}
		include, reasons = append(include, pins...), append(reasons, more...)
	}
	if len(include) == 0 {
		return nil
	}
	flags := make([]string, len(include))
	for i, name := range include {
		flags[i] = "--include " + name
	}
	them := "it"
	if len(include) > 1 {
		them = "them"
	}
	return refuse("%s: %s, so nothing was pushed; pass %s to take %s into the run",
		stackRebasePrefix, strings.Join(reasons, "; "), strings.Join(flags, " "), them)
}

func stackRefuseOffOwnedPins(ctx context.Context, dir render.Dir, run *stackRebaseRun) error {
	for _, b := range run.Branches {
		pin, owner := run.branch(b.Parent), run.owners[b.Parent]
		if b.Landed != "" || b.Held != "" || b.Kept || pin == nil || !pin.Pinned || owner == "" || b.OldBase == pin.Head {
			continue
		}
		_, code, stderr, err := render.RunCLIExitCode(ctx, dir, "git", []string{"--attr-source=" + pin.Head, "merge-tree", "--write-tree", "--merge-base=" + b.OldBase, pin.Head, b.Head})
		if err != nil {
			return fmt.Errorf("stack submit: git merge-tree %.12s %.12s: %w", pin.Head, b.Head, err)
		}
		switch code {
		case 0:
		case 1:
			return refuse("stack submit: %s no longer applies on %s's published head %.12s, which %s owns, so nothing was pushed; publish %s alone with ccx vcs ship --tip-only from its working copy",
				b.Name, pin.Name, pin.Head, stackOwnerName(owner), b.Name)
		default:
			return fmt.Errorf("stack submit: git merge-tree %.12s %.12s: exit %d: %s", pin.Head, b.Head, code, strings.TrimSpace(stderr))
		}
	}
	return nil
}

// stackStalePins names each branch kept at its published head while the run
// moves its own parent: the kept head no longer holds that parent, so
// everything stacked on it lands on a stale base. A pin the run would take in
// moves with it, so a pin above it is stale too.
func stackStalePins(ctx context.Context, dir render.Dir, run *stackRebaseRun) (stale, reasons []string, err error) {
	heads := map[string]string{run.Trunk: run.Pin}
	moved := map[string]bool{}
	byName := map[string]*stackRebaseBranch{}
	for i := range run.Branches {
		b := &run.Branches[i]
		byName[b.Name] = b
		heads[b.Name] = b.Head
		parent := byName[b.Parent]
		if b.Pinned && parent != nil && parent.Landed == "" && parent.Held == "" && (!parent.Kept || moved[parent.Name]) && !parent.Stays {
			holds := !moved[parent.Name]
			if holds {
				if holds, err = gitIsAncestor(ctx, dir, stackRebasePrefix, parent.Head, b.Head); err != nil {
					return nil, nil, err
				}
			}
			if !holds {
				stale = append(stale, b.Name)
				moved[b.Name] = true
				reasons = append(reasons, fmt.Sprintf("%s is another lane's, kept at its published head, and that head does not hold the head this run publishes for %s — publishing %s would stack them on a stale %s unless --to %s stops below it",
					b.Name, parent.Name, strings.Join(stackAbove(run, b.Name), ", "), b.Name, parent.Name))
				continue
			}
		}
		if b.Landed == "" && b.Held == "" && !b.Kept && !b.Stays {
			moved[b.Name] = moved[b.Parent] || heads[b.Parent] != b.OldBase
		}
	}
	return stale, reasons, nil
}

// stackConflictingPins names each pin under a published branch that conflicts
// with trunk, which GitHub grades mergeable against the pin's older trunk.
func stackConflictingPins(ctx context.Context, dir render.Dir, run *stackRebaseRun, taken []string) (pins, reasons, warnings []string, err error) {
	byName := map[string]*stackRebaseBranch{}
	for i := range run.Branches {
		byName[run.Branches[i].Name] = &run.Branches[i]
	}
	publishes := func(b *stackRebaseBranch) bool { return b.Landed == "" && b.Held == "" && !b.Kept }
	for _, b := range run.Branches {
		pin := byName[b.Parent]
		if !publishes(&b) || pin == nil || !pin.Pinned || slices.Contains(taken, pin.Name) || slices.Contains(pins, pin.Name) {
			continue
		}
		var conflicted, files []string
		for _, name := range append([]string{b.Name}, stackAbove(run, b.Name)...) {
			up := byName[name]
			if !publishes(up) {
				continue
			}
			merges, conflicts, err := stackTrunkConflicts(ctx, dir, run.Pin, up.Head)
			if err != nil {
				return nil, nil, nil, err
			}
			if merges {
				continue
			}
			conflicted = append(conflicted, name)
			for _, file := range conflicts {
				if !slices.Contains(files, file) {
					files = append(files, file)
				}
			}
		}
		if len(conflicted) == 0 {
			continue
		}
		if slices.Contains(run.Reparented, b.Name) {
			warnings = append(warnings, fmt.Sprintf("%s follows %s's published head, and %s on it conflicts with %s@%.12s in %s — the merge queue ejects it until %s moves onto %s by ccx vcs stack submit from the working copy holding it",
				b.Name, pin.Name, strings.Join(conflicted, ", "), run.Trunk, run.Pin, strings.Join(files, ", "), pin.Name, run.Trunk))
			continue
		}
		pins = append(pins, pin.Name)
		reasons = append(reasons, fmt.Sprintf("%s is another lane's, kept at its published head, and %s on it conflicts with %s@%.12s in %s — GitHub grades it mergeable against %s, but the merge queue ejects it until %s moves onto %s, here or by ccx vcs stack submit from the working copy holding it",
			pin.Name, strings.Join(conflicted, ", "), run.Trunk, run.Pin, strings.Join(files, ", "), pin.Name, pin.Name, run.Trunk))
	}
	return pins, reasons, warnings, nil
}

// stackUnpublishedPins names each pin a run branch sits on whose local head
// holds work origin lacks that the branch carries, or that rewrote the
// published head outside a --tip-only run, which publishes onto that head. A
// pure replay onto newer trunk is no loss, and neither is a head origin once
// published and has since rewritten; a pin in taken is no pin, and a kept
// branch in taken publishes with the run.
func stackUnpublishedPins(ctx context.Context, dir render.Dir, run *stackRebaseRun, remote string, taken []string) (pins, reasons []string, err error) {
	byName := map[string]*stackRebaseBranch{}
	for i := range run.Branches {
		byName[run.Branches[i].Name] = &run.Branches[i]
	}
	for _, b := range run.Branches {
		pin := byName[b.Parent]
		if b.Landed != "" || b.Held != "" || (b.Kept && !slices.Contains(taken, b.Name)) || pin == nil || !pin.Pinned || pin.Local == pin.Remote || slices.Contains(taken, pin.Name) || slices.Contains(pins, pin.Name) {
			continue
		}
		carried, err := stackCarriedUnpublished(ctx, dir, remote, run.Pin, pin, b.Local)
		if err != nil {
			return nil, nil, err
		}
		rewritten := false
		if carried == 0 {
			if run.TipOnly {
				continue
			}
			if rewritten, err = stackRewrotePublished(ctx, dir, remote, run.Pin, pin); err != nil {
				return nil, nil, err
			}
			if !rewritten {
				continue
			}
		}
		if replays, err := stackRemoteReplays(ctx, dir, pin.Remote, pin.Local, run.Pin); err != nil {
			return nil, nil, err
		} else if replays {
			continue
		}
		pins = append(pins, pin.Name)
		published := strings.Join(append([]string{b.Name}, stackAbove(run, b.Name)...), ", ")
		if rewritten {
			reasons = append(reasons, fmt.Sprintf("%s is another lane's, kept at its published head %.12s, but its local head %.12s rewrote that head — publishing %s onto it would stack them on commits %s no longer holds unless %s is submitted from the working copy holding it first",
				pin.Name, pin.Remote, pin.Local, published, pin.Name, pin.Name))
			continue
		}
		reasons = append(reasons, fmt.Sprintf("%s is another lane's, kept at its published head, but %s carries %d commit(s) of %s that origin does not — publishing %s onto that head would drop them unless %s is submitted from the working copy holding it first",
			pin.Name, b.Name, carried, pin.Name, published, pin.Name))
	}
	return pins, reasons, nil
}

func stackRewrotePublished(ctx context.Context, dir render.Dir, remote, trunk string, pin *stackRebaseBranch) (bool, error) {
	kept, err := gitIsAncestor(ctx, dir, stackRebasePrefix, pin.Remote, pin.Local)
	if err != nil || kept {
		return false, err
	}
	own, err := stackCarriedUnpublished(ctx, dir, remote, trunk, pin, pin.Local)
	return own > 0, err
}

// stackAbove names every branch of the run stacked above name, in run order.
func stackAbove(run *stackRebaseRun, name string) []string {
	above := map[string]bool{name: true}
	var names []string
	for _, up := range run.Branches {
		if above[up.Parent] {
			above[up.Name] = true
			names = append(names, up.Name)
		}
	}
	return names
}

// stackCarriedUnpublished counts the commits of pin's local head that head
// also holds and neither trunk nor any head the remote-tracking reflog records
// for pin does: a copy origin published before a rewrite superseded it is no
// unpublished work.
func stackCarriedUnpublished(ctx context.Context, dir render.Dir, remote, trunk string, pin *stackRebaseBranch, head string) (int, error) {
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
		ref := "refs/remotes/" + remote + "/" + pin.Name
		published, err := render.RunCLI(ctx, dir, "git", []string{"reflog", "show", "--format=^%H", ref, "--"})
		if err != nil {
			return 0, fmt.Errorf("%s: git reflog show %s: %w", stackRebasePrefix, ref, err)
		}
		args = append(append(args, "^"+pin.Remote), strings.Fields(published)...)
	}
	count, err := render.RunCLI(ctx, dir, "git", args)
	if err != nil {
		return 0, fmt.Errorf("%s: git rev-list --count: %w", stackRebasePrefix, err)
	}
	return strconv.Atoi(strings.TrimSpace(count))
}
