package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
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

func (e *errRestackConflict) Error() string {
	return fmt.Sprintf("%s does not rebase onto %s cleanly — run ccx vcs stack rebase to resolve it in an isolated workspace", e.Branch, e.Onto)
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
	return fmt.Sprintf("%s would replay %d commits but owns %d — the rest are already in %s, so the restack would copy them onto the branch and its pull request would propose %d files rather than the %d it changed; re-record its base with gt track --parent %s",
		e.Branch, e.Span, e.Own, e.Trunk, e.SpanFiles, e.OwnFiles, e.Parent)
}

// gtRestackResult is what one restack did: the branches whose refs moved, the
// working copies reset onto them, and the branches gt is holding frozen, which
// are left exactly where they are.
type gtRestackResult struct {
	moved     []string
	realigned []string
	held      map[string]string
}

func gtRestackChain(ctx context.Context, prefix string, c vcs.Checkout, dir render.Dir, commonDir string, state gtState, chain []string, elsewhere bool) (gtRestackResult, error) {
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

	check := stackCheckHolders(ctx, c.Root, movers, holders)
	if elsewhere {
		check = stackCheckClean(ctx, movers, holders)
	}
	if check != nil {
		return gtRestackResult{held: held}, check
	}

	pin := gtTrunkPinned{name: trunk, sha: state[trunk].Head}
	moves, replayErr := gtReplayChain(ctx, prefix, dir, state, pin, movers, holders)
	if replayErr != nil {
		return gtRestackResult{held: held}, fmt.Errorf("%w; no branches moved", replayErr)
	}
	if err := gtRestackRefuseClobbers(ctx, prefix, holders, moves); err != nil {
		return gtRestackResult{held: held}, err
	}
	if err := gtRestackPublish(ctx, prefix, dir, state, moves); err != nil {
		return gtRestackResult{held: held}, err
	}
	realigned, alignErr := gtRestackAlign(ctx, prefix, holders, moves)
	recordErr := errors.Join(gtmeta.Reparent(ctx, commonDir, gtRestackReparents(moves)), gtmeta.RecordRestacked(ctx, commonDir, gtRestackRevisions(moves)))
	if recordErr != nil {
		recordErr = fmt.Errorf("%s: %w", prefix, recordErr)
	}
	result := gtRestackResult{moved: gtRestackBranches(moves), realigned: realigned, held: held}
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

type restackMove struct {
	branch   string
	head     string
	parent   string
	previous string
	onto     string
}

func gtReplayChain(ctx context.Context, prefix string, dir render.Dir, state gtState, pin gtTrunkPinned, movers []string, holders map[string]string) ([]restackMove, error) {
	var moves []restackMove
	heads := map[string]string{pin.name: pin.sha}
	for i, branch := range movers {
		s := state[branch]
		parent := s.Parents[0]
		base, moved := heads[parent.Ref]
		if !moved {
			base = state[parent.Ref].Head
		}
		onto := ""
		if !moved && parent.Ref != pin.name {
			spent, err := gitIsAncestor(ctx, dir, prefix, base, pin.sha)
			if err != nil {
				return moves, err
			}
			if spent {
				onto, base, parent.Ref = pin.name, pin.sha, pin.name
			}
		}
		merged, err := gitIsAncestor(ctx, dir, prefix, gtRestackRef(branch), base)
		if err != nil {
			return moves, err
		}
		if merged {
			if parent.Ref == pin.name && slices.ContainsFunc(movers[i+1:], func(m string) bool { return state[m].Parents[0].Ref == branch }) {
				continue
			}
			return moves, &errRestackMerged{Branch: branch}
		}
		from, err := gtRestackFrom(ctx, prefix, dir, branch, parent, state[parent.Ref].Head)
		if err != nil {
			return moves, err
		}
		if err := gtRestackOwnWork(ctx, prefix, dir, pin, branch, parent.Ref, from); err != nil {
			return moves, err
		}
		head, err := gtReplay(ctx, prefix, dir, base, from, branch, s)
		if err != nil {
			if errors.Is(err, errReplayConflict) {
				return moves, &errRestackConflict{Branch: branch, Onto: parent.Ref, Dir: holders[branch]}
			}
			return moves, err
		}
		heads[branch] = head
		moves = append(moves, restackMove{branch: branch, head: head, parent: base, previous: s.Head, onto: onto})
	}
	return moves, nil
}

// gtRestackFrom is where a branch's own commits start: the fork point from the
// parent as it stands, which is where git rebase itself would start, unless the
// parent revision gt recorded lies past it — a parent rewritten under a branch
// still sitting on its old head.
func gtRestackFrom(ctx context.Context, prefix string, dir render.Dir, branch string, parent gtRef, parentHead string) (string, error) {
	out, err := render.RunCLI(ctx, dir, "git", []string{"merge-base", gtRestackRef(branch), parentHead})
	if err != nil {
		return "", fmt.Errorf("%s: git merge-base %s %s: %w", prefix, branch, parent.Ref, err)
	}
	fork := strings.TrimSpace(out)
	recorded, err := gitIsAncestor(ctx, dir, prefix, parent.SHA, gtRestackRef(branch))
	if err != nil || !recorded {
		return fork, err
	}
	behind, err := gitIsAncestor(ctx, dir, prefix, parent.SHA, fork)
	if err != nil || behind {
		return fork, err
	}
	return parent.SHA, nil
}

// gtTrunkPinned is the commit one restack lands every lane on: trunk's branch
// name, the sha it is held at for the whole operation, and how many commits the
// local branch holds that the remote does not — drift the local ref could not be
// fast-forwarded past.
type gtTrunkPinned struct {
	name string
	sha  string
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

func gtSubmitWidth(ctx context.Context, prefix string, dir render.Dir, trunk string, heads []string) (int, int, error) {
	files := map[string]bool{}
	var live []string
	for _, head := range heads {
		contained, err := gitIsAncestor(ctx, dir, prefix, head, trunk)
		if err != nil {
			return 0, 0, err
		}
		if contained {
			continue
		}
		live = append(live, head)
		changed, err := gtRestackFiles(ctx, prefix, dir, trunk+"..."+head)
		if err != nil {
			return 0, 0, err
		}
		for _, file := range changed {
			files[file] = true
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
		fmt.Fprintf(&updates, "update %s %s %s\n", gtRestackRef(m.branch), m.head, state[m.branch].Head)
	}
	updates.WriteString("prepare\ncommit\n")
	if _, err := render.RunCLIStdin(ctx, dir, "git", []string{"update-ref", "--stdin"}, []byte(updates.String())); err != nil {
		return fmt.Errorf("%s: publish restacked branches: %w", prefix, err)
	}
	return nil
}

func gtRestackBranches(moves []restackMove) []string {
	branches := make([]string, len(moves))
	for i, m := range moves {
		branches[i] = m.branch
	}
	return branches
}

func gtRestackReparents(moves []restackMove) map[string]string {
	reparents := map[string]string{}
	for _, m := range moves {
		if m.onto != "" {
			reparents[m.branch] = m.onto
		}
	}
	return reparents
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

func gtReplay(ctx context.Context, prefix string, dir render.Dir, base, from, branch string, state gtBranchState) (string, error) {
	ref := gtRestackRef(branch)
	span := from + ".." + ref
	out, code, stderr, err := render.RunCLIExitCode(ctx, dir, "git", []string{"-c", "replay.refAction=print", "replay", "--onto", base, span})
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

// gtRestackRefuseClobbers refuses, before any ref moves, a move whose new head
// adds a path the holding working copy already has a file at. The holder check
// clears untracked files but never lists ignored ones, and read-tree -u
// replaces an ignored file without a word.
func gtRestackRefuseClobbers(ctx context.Context, prefix string, holders map[string]string, moves []restackMove) error {
	for _, m := range moves {
		holder := holders[m.branch]
		if holder == "" {
			continue
		}
		out, err := render.RunCLI(ctx, render.Dir(holder), "git", []string{"diff", "--name-only", "-z", "--no-renames", "--diff-filter=A", m.previous, m.head})
		if err != nil {
			return fmt.Errorf("%s: git diff %s %s: %w", prefix, shortSHA(m.previous), shortSHA(m.head), err)
		}
		var clobbered []string
		for _, path := range strings.Split(out, "\x00") {
			if path == "" {
				continue
			}
			if _, err := os.Lstat(filepath.Join(holder, path)); err == nil {
				clobbered = append(clobbered, path)
			}
		}
		if len(clobbered) > 0 {
			return fmt.Errorf("%s: moving %s would overwrite %s in %s, which git ignores there but the new head tracks; no branches moved — move them aside, then retry",
				prefix, m.branch, strings.Join(clobbered, ", "), holder)
		}
	}
	return nil
}

func gtRestackAlign(ctx context.Context, prefix string, holders map[string]string, moves []restackMove) ([]string, error) {
	var aligned []string
	for _, m := range moves {
		holder := holders[m.branch]
		if holder == "" {
			continue
		}
		if _, err := render.RunCLI(ctx, render.Dir(holder), "git", []string{"read-tree", "-m", "-u", m.previous, m.head}); err != nil {
			return aligned, fmt.Errorf("%s: branches moved but %s could not be aligned without overwriting local changes; preserve those changes and run ccx vcs stack continue: %w", prefix, holder, err)
		}
		aligned = append(aligned, holder)
	}
	return aligned, nil
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
