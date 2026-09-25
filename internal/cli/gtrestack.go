package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
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

// errRestackDuplicates is a branch whose replay would carry commits its parent
// already holds under other shas: the branch was rebased outside gt onto a copy
// of its parent that has since been rewritten again. Replaying them applies each
// one twice, and the pull request then proposes the parent's work as its own.
type errRestackDuplicates struct {
	Branch  string
	Parent  string
	Commits []string
}

func (e *errRestackDuplicates) Error() string {
	return fmt.Sprintf("%s carries %d commit(s) %s already holds under other shas (%s), so a replay would apply them twice — rebase %s onto %s by hand, then re-record it with gt track --force --parent %s %s",
		e.Branch, len(e.Commits), e.Parent, strings.Join(e.Commits, ", "), e.Branch, e.Parent, e.Parent, e.Branch)
}

// errTrunkDiverged is a local trunk holding commits its remote does not. Every
// lane of a restack lands on trunk's commit, so restacking onto a drifted local
// branch splices that drift into every branch of the stack, and the pull
// requests then propose to land another lane's unlanded work.
type errTrunkDiverged struct {
	Trunk  string
	Remote string
	Ahead  int
}

func (e *errTrunkDiverged) Error() string {
	return fmt.Sprintf("%s holds %d commit(s) %s does not, and restacking onto it would splice them into every branch of the stack — reconcile it first with gt sync, or with git branch -f %s %s if those commits are disposable",
		e.Trunk, e.Ahead, e.Remote, e.Trunk, e.Remote)
}

// gtRestackResult is what one restack did: the branches whose refs moved, the
// branches already on their parent whose recorded base alone moved, the working
// copies reset onto the moved ones, and the branches gt is holding frozen, which
// are left exactly where they are.
type gtRestackResult struct {
	moved      []string
	rerecorded []string
	empty      []string
	realigned  []string
	held       map[string]string
}

func gtRestackChain(ctx context.Context, prefix string, c vcs.Checkout, dir render.Dir, commonDir string, state gtState, chain []string) (gtRestackResult, error) {
	movers, held := gtRestackPlan(state, chain)
	if len(movers) == 0 {
		return gtRestackResult{held: held}, nil
	}
	trunk, err := gtTrunkBranch(prefix, state)
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

	pin := gtTrunkPinned{name: trunk, sha: state[trunk].Head}
	moves, empty, replayErr := gtReplayChain(ctx, prefix, dir, state, pin, movers, holders)
	for _, branch := range empty {
		held[branch] = gtHoldEmpty
	}
	if replayErr != nil {
		return gtRestackResult{held: held}, fmt.Errorf("%w; no branches moved", replayErr)
	}
	if err := gtRestackPublish(ctx, prefix, dir, state, moves); err != nil {
		return gtRestackResult{held: held}, err
	}
	realigned, alignErr := gtRestackAlign(ctx, prefix, holders, snapshots, moves)
	recordErr := gtmeta.RecordRestacked(ctx, commonDir, gtRestackRevisions(moves))
	if recordErr != nil {
		recordErr = fmt.Errorf("%s: %w", prefix, recordErr)
	}
	moved, rerecorded := gtRestackBranches(moves)
	result := gtRestackResult{moved: moved, rerecorded: rerecorded, empty: empty, realigned: realigned, held: held}
	return result, errors.Join(alignErr, recordErr)
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

// gtHoldEmpty is why a tracked branch with no commit of its own is left where it
// is: it is a lane nobody has committed to yet, and it has nothing to replay.
const gtHoldEmpty = "empty, with no commit of its own yet"

// restackMove is one branch the replay settled: where its ref now points, and
// the revision its parent stood at when it landed there — the pair gt's metadata
// compares to decide the branch is restacked. stayed marks a branch rebased
// outside gt that already sat on that revision: its record moves, its ref never.
type restackMove struct {
	branch string
	head   string
	parent string
	stayed bool
}

func gtReplayChain(ctx context.Context, prefix string, dir render.Dir, state gtState, pin gtTrunkPinned, movers []string, holders map[string]string) ([]restackMove, []string, error) {
	var moves []restackMove
	var empty []string
	heads := map[string]string{pin.name: pin.sha}
	for _, branch := range movers {
		s := state[branch]
		parent := s.Parents[0]
		base, moved := heads[parent.Ref]
		if !moved {
			base = state[parent.Ref].Head
		}
		merged, err := gitIsAncestor(ctx, dir, prefix, gtRestackRef(branch), base)
		if err != nil {
			return moves, empty, err
		}
		from, err := gtRestackFrom(ctx, prefix, dir, state, branch)
		if err != nil {
			return moves, empty, err
		}
		if merged {
			if from != s.Head {
				return moves, empty, &errRestackMerged{Branch: branch}
			}
			empty = append(empty, branch)
			continue
		}
		recorded := gtRef{Ref: parent.Ref, SHA: from}
		on, err := gtRestackAlreadyOn(ctx, prefix, dir, branch, recorded, base)
		if err != nil {
			return moves, empty, err
		}
		if on {
			heads[branch] = s.Head
			moves = append(moves, restackMove{branch: branch, head: s.Head, parent: base, stayed: true})
			continue
		}
		span, err := gtRestackSpan(ctx, prefix, dir, pin, branch, recorded, base)
		if err != nil {
			return moves, empty, err
		}
		head, err := gtReplay(ctx, prefix, dir, base, branch, s, span...)
		if err != nil {
			if errors.Is(err, errReplayConflict) {
				return moves, empty, &errRestackConflict{Branch: branch, Onto: parent.Ref, Dir: holders[branch]}
			}
			return moves, empty, err
		}
		heads[branch] = head
		moves = append(moves, restackMove{branch: branch, head: head, parent: base})
	}
	return moves, empty, nil
}

// gtRestackFrom is where a branch's own commits start: the parent revision gt
// recorded, or — when a rebase outside gt took that revision out of the
// branch's history — the fork point from the parent as it stands, which is
// where git rebase itself would start.
func gtRestackFrom(ctx context.Context, prefix string, dir render.Dir, state gtState, branch string) (string, error) {
	parent := state[branch].Parents[0]
	recorded, err := gitIsAncestor(ctx, dir, prefix, parent.SHA, gtRestackRef(branch))
	if err != nil {
		return "", err
	}
	if recorded {
		return parent.SHA, nil
	}
	out, err := render.RunCLI(ctx, dir, "git", []string{"merge-base", gtRestackRef(branch), state[parent.Ref].Head})
	if err != nil {
		return "", fmt.Errorf("%s: git merge-base %s %s: %w", prefix, branch, parent.Ref, err)
	}
	return strings.TrimSpace(out), nil
}

// gtTrunkPinned is the commit one restack lands every lane on: trunk's branch
// name, the sha it is held at for the whole operation, and how many commits the
// local branch holds that the remote does not — drift the local ref could not be
// fast-forwarded past.
type gtTrunkPinned struct {
	name     string
	sha      string
	diverged int
}

func (p gtTrunkPinned) String() string { return fmt.Sprintf("%s@%.12s", p.name, p.sha) }

// gtTrunkPin fixes the commit every lane of a restack lands on — the remote
// trunk, which is the only ref a pull request is measured against — and puts
// the local trunk branch on it. Both halves are required: gt records a restack
// against the local ref, since gtmeta reads refs/heads/<trunk>, so a pin the
// local branch does not carry is one the next command reads as a branch still
// needing a restack and rebases back onto whatever the local ref holds.
//
// A local trunk the remote cannot fast-forward is left where it is and reported
// as drift: gtTrunkDrift decides what that costs.
func gtTrunkPin(ctx context.Context, prefix string, c vcs.Checkout, dir render.Dir, tr vcs.Trunk, local string) (gtTrunkPinned, error) {
	pinned, err := gtTrunkHead(ctx, dir, prefix, tr)
	if err != nil {
		return gtTrunkPinned{}, err
	}
	pin := gtTrunkPinned{name: tr.Name(), sha: pinned}
	if pinned == local {
		return pin, nil
	}
	behind, err := gitIsAncestor(ctx, dir, prefix, local, pinned)
	if err != nil {
		return gtTrunkPinned{}, fmt.Errorf("%s: compare %s with %s: %w", prefix, tr.Name(), tr.Ref(), err)
	}
	if !behind {
		ahead, err := gtRevCount(ctx, prefix, dir, string(tr.Ref())+".."+gtRestackRef(tr.Name()))
		if err != nil {
			return gtTrunkPinned{}, err
		}
		return gtTrunkPinned{name: tr.Name(), sha: local, diverged: ahead}, nil
	}
	return pin, gtTrunkFastForward(ctx, prefix, c, dir, tr, local, pinned)
}

// gtTrunkDrift refuses a restack that would land a branch on a drifted trunk,
// and reports the drift otherwise: a branch whose parent is trunk inherits
// every commit trunk holds, while a branch stacked on a branch inherits none of
// them.
func gtTrunkDrift(errW io.Writer, prefix string, state gtState, chain []string, pin gtTrunkPinned, remote string) error {
	if pin.diverged == 0 {
		return nil
	}
	drift := &errTrunkDiverged{Trunk: pin.name, Remote: remote, Ahead: pin.diverged}
	movers, _ := gtRestackPlan(state, chain)
	for _, branch := range movers {
		if state[branch].Parents[0].Ref == pin.name {
			return fmt.Errorf("%s: %w", prefix, drift)
		}
	}
	_, err := fmt.Fprintf(errW, "%s: warning: %s holds %d commit(s) %s does not; no branch of this stack lands on it, so none inherited them — reconcile it with gt sync\n",
		prefix, pin.name, pin.diverged, remote)
	return err
}

// gtTrunkFastForward moves the local trunk branch onto the pin, realigning the
// working copy holding it exactly as a moved stack branch is realigned: the ref
// moves without a checkout, so its holder's tree has to be reset onto it and
// the uncommitted work it was snapshotted with applied back.
func gtTrunkFastForward(ctx context.Context, prefix string, c vcs.Checkout, dir render.Dir, tr vcs.Trunk, local, pinned string) error {
	holders, err := vcs.BranchHolders(ctx, c)
	if err != nil {
		return fmt.Errorf("%s: %w", prefix, err)
	}
	snapshots, err := gtRestackSnapshots(ctx, prefix, []string{tr.Name()}, holders)
	if err != nil {
		return err
	}
	if _, err := render.RunCLI(ctx, dir, "git", []string{"update-ref", gtRestackRef(tr.Name()), pinned, local}); err != nil {
		return fmt.Errorf("%s: fast-forward %s to %s: %w", prefix, tr.Name(), tr.Ref(), err)
	}
	_, err = gtRestackAlign(ctx, prefix, holders, snapshots, []restackMove{{branch: tr.Name(), head: pinned, parent: local}})
	return err
}

// gtRestackAlreadyOn reports a branch rebased outside gt onto base: base is in
// its history and every commit above base is its own, counted from gt's recorded
// parent revision. A branch that merely contains base still carries whatever
// sits between them — a dropped or landed parent's commits — and must replay.
func gtRestackAlreadyOn(ctx context.Context, prefix string, dir render.Dir, branch string, parent gtRef, base string) (bool, error) {
	contains, err := gitIsAncestor(ctx, dir, prefix, base, gtRestackRef(branch))
	if err != nil || !contains {
		return false, err
	}
	above, err := gtRevCount(ctx, prefix, dir, base+".."+gtRestackRef(branch))
	if err != nil {
		return false, err
	}
	own, err := gtRevCount(ctx, prefix, dir, parent.SHA+".."+gtRestackRef(branch), "--not", base)
	if err != nil {
		return false, err
	}
	return above == own, nil
}

// gtRestackSpan names the commits a replay of branch onto base carries: gt's
// recorded parent revision through to the head. A branch rebased outside gt
// leaves that revision behind, so the span reaches trunk commits the pin already
// holds, and those are excluded rather than copied. A commit the parent holds
// under another sha is refused: no exclusion by ancestry can see it.
func gtRestackSpan(ctx context.Context, prefix string, dir render.Dir, pin gtTrunkPinned, branch string, parent gtRef, base string) ([]string, error) {
	span := parent.SHA + ".." + gtRestackRef(branch)
	replayed, err := gtRevCount(ctx, prefix, dir, span)
	if err != nil {
		return nil, err
	}
	own, err := gtRevCount(ctx, prefix, dir, span, "--not", pin.sha)
	if err != nil {
		return nil, err
	}
	if replayed == own {
		return []string{span}, nil
	}
	revs := []string{span, "^" + pin.sha}
	dups, err := gtRestackCopies(ctx, prefix, dir, base, branch, revs)
	if err != nil {
		return nil, err
	}
	if len(dups) > 0 {
		return nil, &errRestackDuplicates{Branch: branch, Parent: parent.Ref, Commits: dups}
	}
	return revs, nil
}

// gtRestackCopies lists the commits of revs that are patch-equivalent to a
// commit base carries and branch does not, short shas oldest first.
func gtRestackCopies(ctx context.Context, prefix string, dir render.Dir, base, branch string, revs []string) ([]string, error) {
	marks, err := gtCherryMarks(ctx, prefix, dir, base, gtRestackRef(branch))
	if err != nil {
		return nil, err
	}
	out, err := render.RunCLI(ctx, dir, "git", append([]string{"rev-list", "--reverse"}, revs...))
	if err != nil {
		return nil, fmt.Errorf("%s: git rev-list %s: %w", prefix, strings.Join(revs, " "), err)
	}
	copies := map[string]bool{}
	for _, m := range marks {
		if !m.own {
			copies[m.sha] = true
		}
	}
	var dups []string
	for _, sha := range strings.Fields(out) {
		if copies[sha] {
			dups = append(dups, shortSHA(sha))
		}
	}
	return dups, nil
}

// gtCherryMark is one commit on the branch side of a comparison against its
// parent, and whether it is the branch's own — no commit on the parent side
// carries the same patch.
type gtCherryMark struct {
	sha string
	own bool
}

// gtCherryMarks walks the commits head holds and parent does not, oldest first,
// marking each by patch identity against the commits parent holds and head does
// not: the one comparison that recognizes a parent's commit after a rewrite gave
// it a new sha.
func gtCherryMarks(ctx context.Context, prefix string, dir render.Dir, parent, head string) ([]gtCherryMark, error) {
	span := parent + "..." + head
	out, err := render.RunCLI(ctx, dir, "git", []string{"rev-list", "--cherry-mark", "--right-only", "--no-merges", "--reverse", "--topo-order", span})
	if err != nil {
		return nil, fmt.Errorf("%s: git rev-list --cherry-mark %s: %w", prefix, span, err)
	}
	var marks []gtCherryMark
	for _, line := range strings.Fields(out) {
		marks = append(marks, gtCherryMark{sha: line[1:], own: line[0] == '+'})
	}
	return marks, nil
}

func gtRevCount(ctx context.Context, prefix string, dir render.Dir, span string, args ...string) (int, error) {
	argv := append([]string{"rev-list", "--count", span}, args...)
	out, err := render.RunCLI(ctx, dir, "git", argv)
	if err != nil {
		return 0, fmt.Errorf("%s: git rev-list --count %s: %w", prefix, span, err)
	}
	count, err := strconv.Atoi(strings.TrimSpace(out))
	if err != nil {
		return 0, fmt.Errorf("%s: read a commit count from %q: %w", prefix, out, err)
	}
	return count, nil
}

// gtSubmitWidth measures what a stack proposes against the remote trunk: the
// commits and the changed files a reviewer sees. A branch trunk already
// contains is left out, since the submit drops it. The number is the tell a
// hundred-file pull request over a one-file change shows up as, which is why
// the report carries it rather than leaving it to be found on GitHub.
func gtSubmitWidth(ctx context.Context, prefix string, dir render.Dir, tr vcs.Trunk, branches []string) (int, int, error) {
	trunk := string(tr.Ref())
	files := map[string]bool{}
	var live []string
	for _, branch := range branches {
		contained, err := gitIsAncestor(ctx, dir, prefix, gtRestackRef(branch), trunk)
		if err != nil {
			return 0, 0, err
		}
		if contained {
			continue
		}
		live = append(live, gtRestackRef(branch))
		changed, err := gtRestackFiles(ctx, prefix, dir, trunk+"..."+gtRestackRef(branch))
		if err != nil {
			return 0, 0, err
		}
		for _, f := range changed {
			files[f] = true
		}
	}
	if len(live) == 0 {
		return 0, 0, nil
	}
	commits, err := gtRevCount(ctx, prefix, dir, live[0], append(live[1:], "--not", trunk)...)
	if err != nil {
		return 0, 0, err
	}
	return commits, len(files), nil
}

func gtRestackFiles(ctx context.Context, prefix string, dir render.Dir, span string) ([]string, error) {
	out, err := render.RunCLI(ctx, dir, "git", []string{"diff", "--name-only", span})
	if err != nil {
		return nil, fmt.Errorf("%s: git diff --name-only %s: %w", prefix, span, err)
	}
	return strings.Fields(out), nil
}

func gtRestackPublish(ctx context.Context, prefix string, dir render.Dir, state gtState, moves []restackMove) error {
	var updates strings.Builder
	updates.WriteString("start\n")
	for _, m := range moves {
		if m.stayed {
			continue
		}
		fmt.Fprintf(&updates, "update %s %s %s\n", gtRestackRef(m.branch), m.head, state[m.branch].Head)
	}
	updates.WriteString("prepare\ncommit\n")
	if _, err := render.RunCLIStdin(ctx, dir, "git", []string{"update-ref", "--stdin"}, []byte(updates.String())); err != nil {
		return fmt.Errorf("%s: publish restacked branches: %w", prefix, err)
	}
	return nil
}

func gtRestackBranches(moves []restackMove) (moved, rerecorded []string) {
	for _, m := range moves {
		if m.stayed {
			rerecorded = append(rerecorded, m.branch)
			continue
		}
		moved = append(moved, m.branch)
	}
	return moved, rerecorded
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

func gtReplay(ctx context.Context, prefix string, dir render.Dir, base, branch string, state gtBranchState, revs ...string) (string, error) {
	ref := gtRestackRef(branch)
	span := strings.Join(revs, " ")
	out, code, stderr, err := render.RunCLIExitCode(ctx, dir, "git", append([]string{"-c", "replay.refAction=print", "replay", "--onto", base}, revs...))
	if err != nil {
		return "", fmt.Errorf("%s: git replay: %w", prefix, err)
	}
	if code != 0 {
		if strings.TrimSpace(stderr) == "" {
			return "", errReplayConflict
		}
		return "", fmt.Errorf("%s: git replay --onto %s %s: %s", prefix, base, span, strings.TrimSpace(stderr))
	}
	update := strings.Fields(out)
	if len(update) != 4 || update[0] != "update" || update[1] != ref || update[3] != state.Head {
		return "", fmt.Errorf("%s: git replay for %s returned an unexpected ref update: %q", prefix, branch, strings.TrimSpace(out))
	}
	return update[2], nil
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
		dirty, err := render.RunCLI(ctx, render.Dir(holder), "git", []string{"status", "--porcelain", "--untracked-files=no"})
		if err != nil {
			return nil, fmt.Errorf("%s: read the uncommitted work in %s before restacking %s: git status --porcelain --untracked-files=no: %w", prefix, holder, branch, err)
		}
		if strings.TrimSpace(dirty) == "" {
			continue
		}
		out, err := render.RunCLI(ctx, render.Dir(holder), "git", []string{"stash", "create", "ccx restack"})
		if err != nil {
			return nil, fmt.Errorf("%s: snapshot the uncommitted work in %s before restacking %s: git stash create: %w", prefix, holder, branch, err)
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
		if holder == "" || m.stayed {
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
	var notes []string
	if len(r.rerecorded) > 0 {
		notes = append(notes, "re-recorded the base of "+strings.Join(r.rerecorded, ", "))
	}
	if len(r.empty) > 0 {
		notes = append(notes, "skipped empty "+strings.Join(r.empty, ", "))
	}
	if len(r.moved) == 0 {
		if len(notes) > 0 {
			return strings.Join(notes, shipSep)
		}
		return "already restacked"
	}
	segment := fmt.Sprintf("restacked %d branches", len(r.moved))
	if len(r.moved) == 1 {
		segment = "restacked 1 branch"
	}
	if len(r.realigned) > 1 {
		segment += fmt.Sprintf(" across %d working copies", len(r.realigned))
	}
	return strings.Join(append([]string{segment}, notes...), shipSep)
}

// gtUpstack lists the branches above branch, breadth-first, so a parent always
// precedes its children — the order gtRestackChain has to be driven in. branch
// itself is not in it. It is gt state's parent map read backwards, which is the
// only way to reach the branches above this one when each sits in a working copy
// of its own.
func gtUpstack(prefix string, state gtState, branch string) ([]string, error) {
	children := make(map[string][]string, len(state))
	for name, s := range state {
		if len(s.Parents) > 0 {
			children[s.Parents[0].Ref] = append(children[s.Parents[0].Ref], name)
		}
	}
	for _, kids := range children {
		slices.Sort(kids)
	}
	var up []string
	seen := map[string]bool{branch: true}
	for queue := []string{branch}; len(queue) > 0; {
		cur := queue[0]
		queue = queue[1:]
		for _, kid := range children[cur] {
			if seen[kid] {
				return nil, fmt.Errorf("%s: gt state parent chain cycles at %s", prefix, kid)
			}
			seen[kid] = true
			up = append(up, kid)
			queue = append(queue, kid)
		}
	}
	return up, nil
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
