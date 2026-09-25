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

// errRestackDuplicates is a branch whose replay span reaches back past commits
// trunk already carries. Replaying it copies those commits onto the branch, and
// the pull request then proposes every file they touch rather than the ones the
// branch changed — the hundred-file diff behind a one-file change.
type errRestackDuplicates struct {
	Branch    string
	Parent    string
	Trunk     string
	Span      int
	Own       int
	SpanFiles int
	OwnFiles  int
}

func (e *errRestackDuplicates) Error() string {
	return fmt.Sprintf("%s would replay %d commits but owns %d — the rest are already in %s, so the restack would copy them onto the branch and its pull request would propose %d files rather than the %d it changed; re-record its base with gt track --force --parent %s",
		e.Branch, e.Span, e.Own, e.Trunk, e.SpanFiles, e.OwnFiles, e.Parent)
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
//
// Every lane lands on the one commit state's trunk row stands at, and a chain
// that stops partway moves nothing: gtRestackUnwind puts back the refs the
// replay had already moved, so the stack the caller is left with is the one it
// handed over rather than a bottom on the new trunk under branches still on the
// old one.
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
	moves, replayErr := gtReplayChain(ctx, prefix, dir, state, pin, movers, holders)
	if replayErr != nil {
		return gtRestackResult{held: held}, gtRestackUnwind(ctx, dir, state, moves, replayErr)
	}
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
// feeding every new head forward to that parent's own children. It reports the
// moves it made along with the failure that stopped it, so the caller can put
// them back: a chain left half-moved is a stack split across two bases, which
// nothing but commit archaeology can name afterwards.
//
// The trunk row's head is the pin: every lane whose parent is trunk lands on
// that one commit, whatever the local branch does while the chain runs.
func gtReplayChain(ctx context.Context, prefix string, dir render.Dir, state gtState, pin gtTrunkPinned, movers []string, holders map[string]string) ([]restackMove, error) {
	var moves []restackMove
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
			return moves, err
		}
		if merged {
			return moves, &errRestackMerged{Branch: branch}
		}
		from, err := gtRestackFrom(ctx, prefix, dir, state, branch)
		if err != nil {
			return moves, err
		}
		if err := gtRestackOwnWork(ctx, prefix, dir, pin, branch, parent.Ref, from); err != nil {
			return moves, err
		}
		if err := gtReplay(ctx, prefix, dir, base, from+".."+gtRestackRef(branch)); err != nil {
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

// gtRestackOwnWork refuses a replay whose span reaches back past commits trunk
// already carries. The span is what gt recorded as the branch's parent revision
// through to its head, and a branch rebased outside gt leaves that revision
// behind its real base — so the span picks up the trunk commits in between and
// the replay copies every one of them onto the branch. The pull request that
// follows proposes those commits' files as the branch's own, which is the
// hundred-file diff over a one-file change.
func gtRestackOwnWork(ctx context.Context, prefix string, dir render.Dir, pin gtTrunkPinned, branch, parent, parentSHA string) error {
	span := parentSHA + ".." + gtRestackRef(branch)
	replayed, err := gtRevCount(ctx, prefix, dir, span)
	if err != nil {
		return err
	}
	own, err := gtRevCount(ctx, prefix, dir, span, "--not", pin.sha)
	if err != nil {
		return err
	}
	if replayed == own {
		return nil
	}
	spanFiles, err := gtRestackFileCount(ctx, prefix, dir, span)
	if err != nil {
		return err
	}
	ownFiles, err := gtRestackFileCount(ctx, prefix, dir, pin.sha+"..."+gtRestackRef(branch))
	if err != nil {
		return err
	}
	return &errRestackDuplicates{
		Branch: branch, Parent: parent, Trunk: pin.name,
		Span: replayed, Own: own, SpanFiles: spanFiles, OwnFiles: ownFiles,
	}
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

// gtRestackFileCount counts the files a range changes, which is the measure a
// pull request is read by: a diff wider than the work is the tell.
func gtRestackFileCount(ctx context.Context, prefix string, dir render.Dir, span string) (int, error) {
	files, err := gtRestackFiles(ctx, prefix, dir, span)
	if err != nil {
		return 0, err
	}
	return len(files), nil
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

// gtRestackUnwind puts every ref the replay moved back where it stood. A chain
// that stopped partway otherwise leaves the bottom on the trunk it had just
// reached with everything above it on the old one — two bases in one stack,
// which reads as a healthy branch and a stale copy until somebody rebuilds it
// from commit archaeology. Nothing else has to come back: the align pass has
// not run, so no working copy was reset, and gt's metadata is written only for
// a chain that finished.
//
// A ref that will not go back is the one case left to finish by hand, so the
// refusal names each branch that moved and the sha it moved from.
func gtRestackUnwind(ctx context.Context, dir render.Dir, state gtState, moves []restackMove, cause error) error {
	if len(moves) == 0 {
		return cause
	}
	cleanup := context.WithoutCancel(ctx)
	var stuck []string
	var put []string
	for _, m := range slices.Backward(moves) {
		was := state[m.branch].Head
		if _, err := render.RunCLI(cleanup, dir, "git", []string{"update-ref", gtRestackRef(m.branch), was, m.head}); err != nil {
			stuck = append(stuck, fmt.Sprintf("%s is at %.12s and belongs at %.12s", m.branch, m.head, was))
			continue
		}
		put = append(put, m.branch)
	}
	if len(stuck) > 0 {
		return fmt.Errorf("%w; the restack could not be rolled back, so reset these by hand: %s", cause, strings.Join(stuck, ", "))
	}
	slices.Reverse(put)
	return fmt.Errorf("%w; the restack rolled back, so nothing moved — put back: %s", cause, strings.Join(put, ", "))
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
