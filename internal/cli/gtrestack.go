package cli

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/yasyf/cc-context/internal/gtmeta"
	"github.com/yasyf/cc-context/internal/render"
	"github.com/yasyf/cc-context/internal/vcs"
)

// errRestackConflict is what a replay that could not apply a commit returns.
// git replay says nothing at all on a conflict — it exits 1 with both streams
// empty, applies nothing, and leaves no rebase to continue — so the branch it
// stopped on is ccx's to name.
type errRestackConflict struct {
	Branch string
	Onto   string
	Dir    string
}

// Error carries the way out as well as the fact, because nothing is left
// mid-rebase for gt continue to continue: a replay that conflicts applies
// nothing. The step is gt's own interactive restack of that one branch, driven
// from the checkout holding it.
func (e *errRestackConflict) Error() string {
	step := "gt restack --only --branch " + e.Branch
	if e.Dir == "" {
		return fmt.Sprintf("%s does not rebase onto %s cleanly — rebase it by hand with %s", e.Branch, e.Onto, step)
	}
	return fmt.Sprintf("%s does not rebase onto %s cleanly — rebase it by hand in %s with %s", e.Branch, e.Onto, e.Dir, step)
}

// errRestackMerged is a branch whose commits are already in its parent. No
// rebase moves it anywhere — replaying it would re-apply commits the parent
// carries — and the submit that would follow is one Graphite refuses.
type errRestackMerged struct{ Branch string }

func (e *errRestackMerged) Error() string {
	return e.Branch + " is already merged, so there is nothing to submit — drop it with gt untrack " + e.Branch
}

// gtRestackResult is what one restack did: the branches whose refs moved, the
// working copies reset onto them, and the branches gt is holding frozen, which
// are left exactly where they are.
type gtRestackResult struct {
	moved     []string
	realigned []string
	held      map[string]string
}

// gtRestackChain rebases every branch of chain that sits off its parent onto
// that parent, bottom-up.
//
// It never runs gt, and it never checks a branch out: git replay computes the
// new commits and moves the refs without touching a working tree or an index,
// so a branch a sibling checkout holds is not a special case at all. That is
// the whole reason this exists — gt restack rebases, and git refuses to rebase
// a branch another working copy has checked out, which gt answers by declining
// the branch on stdout at exit 0 for some of them and dying on git's exit 128
// for others. A sweep built on that guard stops mid-stack on the second kind.
//
// The price is that a moved ref leaves its holder's HEAD ahead of its index and
// working tree, reading as a whole-tree reverse diff until gtRestackAlign resets
// it. That holder's uncommitted work is snapshotted beforehand and applied
// after, which is what gt did for a lane it rebased in place.
func gtRestackChain(ctx context.Context, prefix string, c vcs.Checkout, dir render.Dir, state gtState, chain []string) (gtRestackResult, error) {
	movers, held := gtRestackPlan(state, chain)
	if len(movers) == 0 {
		return gtRestackResult{held: held}, nil
	}
	commonDir, err := gtCommonDir(ctx, dir, prefix)
	if err != nil {
		return gtRestackResult{}, err
	}
	holders, err := vcs.BranchHolders(ctx, c)
	if err != nil {
		return gtRestackResult{}, fmt.Errorf("%s: %w", prefix, err)
	}

	snapshots, snapErr := gtRestackSnapshots(ctx, prefix, movers, holders)
	if snapErr != nil {
		return gtRestackResult{held: held}, snapErr
	}

	moves, replayErr := gtReplayChain(ctx, prefix, dir, state, movers, holders)
	realigned, alignErr := gtRestackAlign(ctx, prefix, holders, snapshots, moves)
	recordErr := gtmeta.RecordRestacked(ctx, commonDir, gtRestackRevisions(moves))
	if recordErr != nil {
		recordErr = fmt.Errorf("%s: %w", prefix, recordErr)
	}
	result := gtRestackResult{moved: gtRestackBranches(moves), realigned: realigned, held: held}
	return result, errors.Join(replayErr, alignErr, recordErr)
}

// gtRestackPlan names the branches to move: every branch gt reads as sitting off
// its parent, and every branch above one of those, which the move puts off its
// own parent in turn. Everything else is left exactly where it is, keeping the
// shas its pull request was pushed as.
//
// A branch gt is holding — gt freeze, or a merge in progress — is left where it
// is whatever its parent did, and named in the second return so the caller can
// report it. gt declines such a branch too; nothing here may quietly rebase one
// gt was asked to leave alone. Its children are not dragged either, since the
// branch they sit on did not move.
//
// chain is walked in the order given, which every caller hands over parents
// first — gtBottomUp for a downstack, gtStackAll's breadth-first walk for a
// whole stack. That second one is a tree, not a line, so a branch inherits from
// its own parent rather than from whatever preceded it in the chain: a stale
// branch in one subtree says nothing about a sibling in another.
func gtRestackPlan(state gtState, chain []string) ([]string, map[string]string) {
	moving := make(map[string]bool, len(chain))
	held := make(map[string]string)
	var movers []string
	for _, branch := range chain {
		s := state[branch]
		if !s.NeedsRestack && !moving[s.Parents[0].Ref] {
			continue
		}
		if s.State != "" {
			held[branch] = s.State
			continue
		}
		moving[branch] = true
		movers = append(movers, branch)
	}
	return movers, held
}

// restackMove is one branch the replay moved: where its ref now points, and the
// revision its parent stood at when it landed there — the pair gt's metadata
// compares to decide the branch is restacked.
type restackMove struct {
	branch string
	head   string
	parent string
}

// gtReplayChain replays each mover onto the commit its parent now stands at,
// feeding every new head forward to that parent's own children. The moves it
// made stand even when a later branch conflicts: the refs that moved stay
// moved, and gt's metadata must agree with them or the next command reads a
// stale stack.
func gtReplayChain(ctx context.Context, prefix string, dir render.Dir, state gtState, movers []string, holders map[string]string) ([]restackMove, error) {
	var moves []restackMove
	heads := make(map[string]string, len(movers))
	for _, branch := range movers {
		s := state[branch]
		parent := s.Parents[0]
		base, moved := heads[parent.Ref]
		if !moved {
			base = state[parent.Ref].Head
		}
		merged, err := gitIsAncestor(ctx, dir, prefix, gtRestackRef(branch), base)
		if err != nil {
			return moves, err
		}
		if merged {
			return moves, &errRestackMerged{Branch: branch}
		}
		if err := gtReplay(ctx, prefix, dir, base, parent.SHA+".."+gtRestackRef(branch)); err != nil {
			if errors.Is(err, errReplayConflict) {
				return moves, &errRestackConflict{Branch: branch, Onto: parent.Ref, Dir: holders[branch]}
			}
			return moves, err
		}
		head, err := gtRestackHead(ctx, prefix, dir, branch)
		if err != nil {
			return moves, err
		}
		heads[branch] = head
		moves = append(moves, restackMove{branch: branch, head: head, parent: base})
	}
	return moves, nil
}

func gtRestackBranches(moves []restackMove) []string {
	branches := make([]string, len(moves))
	for i, m := range moves {
		branches[i] = m.branch
	}
	return branches
}

func gtRestackRevisions(moves []restackMove) map[string]string {
	revisions := make(map[string]string, len(moves))
	for _, m := range moves {
		revisions[m.branch] = m.parent
	}
	return revisions
}

// errReplayConflict marks the one failure git replay reports by exit code
// alone: a commit that does not apply. It exits 1 with both streams empty,
// creates nothing, and moves no ref, so silence at a nonzero exit is the
// signal, and anything git did say is a different failure the caller surfaces
// verbatim — an unknown `replay` subcommand on a git too old for it, say.
var errReplayConflict = errors.New("replay: conflict")

// gtReplay rebases one range onto base without a working tree, and applies the
// ref updates itself when git only printed them: git replay updates refs in an
// atomic transaction from 2.55 and writes nothing, while every earlier version
// prints `update refs/heads/… <new> <old>` lines for git update-ref --stdin.
// Feeding whatever it printed back is both behaviours in one path — an empty
// stdout is an update-ref that does nothing.
//
// The caller's range must end at the branch's ref, never at the sha it stands
// at: git replay infers the refs to update from the range it is given, and a
// raw sha names none — it would compute the new commits and move nothing,
// reporting a restack it did not do.
func gtReplay(ctx context.Context, prefix string, dir render.Dir, base, span string) error {
	out, code, stderr, err := render.RunCLIExitCode(ctx, dir, "git", []string{"replay", "--onto", base, span})
	if err != nil {
		return fmt.Errorf("%s: git replay: %w", prefix, err)
	}
	if code != 0 {
		if strings.TrimSpace(stderr) == "" {
			return errReplayConflict
		}
		return fmt.Errorf("%s: git replay --onto %s %s: %s", prefix, base, span, strings.TrimSpace(stderr))
	}
	if strings.TrimSpace(out) == "" {
		return nil
	}
	if _, err := render.RunCLIStdin(ctx, dir, "git", []string{"update-ref", "--stdin"}, []byte(out)); err != nil {
		return fmt.Errorf("%s: git update-ref after replaying %s: %w", prefix, span, err)
	}
	return nil
}

// gtRestackRef qualifies a branch name, so every lookup names the branch rather
// than whatever else answers to that name — a tag sharing it would otherwise
// decide an ancestry check, or hand a reset the wrong commit.
func gtRestackRef(branch string) string { return "refs/heads/" + branch }

func gtRestackHead(ctx context.Context, prefix string, dir render.Dir, branch string) (string, error) {
	out, err := render.RunCLI(ctx, dir, "git", []string{"rev-parse", "--verify", gtRestackRef(branch)})
	if err != nil {
		return "", fmt.Errorf("%s: git rev-parse %s: %w", prefix, branch, err)
	}
	return strings.TrimSpace(out), nil
}

// gtRestackSnapshots records the uncommitted work of every working copy holding
// a branch about to move, before any ref moves. After it moves, that working
// copy's own diff against its new HEAD is the whole restack in reverse, so a
// snapshot taken then cannot tell the two apart — this one is taken while the
// answer still means something.
//
// git stash create, never git stash push: refs/stash is shared by every working
// copy of a repository, so two holders pushing onto it build one stack whose
// order says nothing about which entry belongs to whom, and popping it back by
// position hands each holder the other's work. create writes the entry as a
// plain commit, touches no ref and no stack, and leaves the working copy alone —
// so a holder that never moves needs nothing restored, and each snapshot is
// addressed by the sha it actually is. Untracked files are outside it and stay
// outside it: git reset --hard does not remove them.
func gtRestackSnapshots(ctx context.Context, prefix string, movers []string, holders map[string]string) (map[string]string, error) {
	snapshots := make(map[string]string)
	for _, branch := range movers {
		holder := holders[branch]
		if holder == "" {
			continue
		}
		out, err := render.RunCLI(ctx, render.Dir(holder), "git", []string{"stash", "create", "ccx restack"})
		if err != nil {
			return nil, fmt.Errorf("%s: snapshot the uncommitted work in %s before restacking %s: %w", prefix, holder, branch, err)
		}
		if sha := strings.TrimSpace(out); sha != "" {
			snapshots[holder] = sha
		}
	}
	return snapshots, nil
}

// gtRestackAlign resets each moved branch's working copy onto its new head and
// applies the snapshot taken from it. The reset is hard by design: the tree it
// throws away is the pre-restack checkout, and the work worth keeping was
// snapshotted before the ref moved. A working copy whose branch never moved is
// left untouched — its tree was never disturbed, so there is nothing to put
// back.
//
// --index restores what was staged as staged, since a snapshot that comes back
// entirely unstaged has quietly rewritten somebody's in-progress commit. An
// apply that conflicts leaves the work in the tree with markers and names the
// commit it came from, which is the one address that survives this call.
func gtRestackAlign(ctx context.Context, prefix string, holders map[string]string, snapshots map[string]string, moves []restackMove) ([]string, error) {
	var realigned []string
	var failures []error
	for _, m := range moves {
		holder := holders[m.branch]
		if holder == "" {
			continue
		}
		if _, err := render.RunCLI(ctx, render.Dir(holder), "git", []string{"reset", "--hard", m.head}); err != nil {
			failures = append(failures, fmt.Errorf("%s: reset %s to the restacked %s: %w", prefix, holder, m.branch, err))
			continue
		}
		realigned = append(realigned, holder)
		snapshot := snapshots[holder]
		if snapshot == "" {
			continue
		}
		if _, err := render.RunCLI(ctx, render.Dir(holder), "git", []string{"stash", "apply", "--index", snapshot}); err != nil {
			failures = append(failures, fmt.Errorf("%s: the uncommitted work from %s does not apply to the restacked %s — resolve it there, or recover it with git stash apply %s: %w", prefix, holder, m.branch, snapshot, err))
		}
	}
	return realigned, errors.Join(failures...)
}

// gtRestackSegment reports a restack in the words of what it did: the branches
// moved, and the working copies reset onto them when more than this one had to
// be. A chain already on its parents moved nothing, and says so rather than
// claiming a restack.
func gtRestackSegment(r gtRestackResult) string {
	if len(r.moved) == 0 {
		return "already restacked"
	}
	segment := fmt.Sprintf("restacked %d branches", len(r.moved))
	if len(r.moved) == 1 {
		segment = "restacked 1 branch"
	}
	if len(r.realigned) > 1 {
		segment += fmt.Sprintf(" across %d working copies", len(r.realigned))
	}
	return segment
}

// gtBottomUp reverses a gtDownstack chain, which runs branch-first, into the
// trunk-adjacent-first order a restack is driven in: rebasing a branch leaves
// everything above it off its parent again, so a top-down pass converges only
// by accident.
func gtBottomUp(chain []string) []string {
	bottomUp := slices.Clone(chain)
	slices.Reverse(bottomUp)
	return bottomUp
}
