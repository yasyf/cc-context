package vcs

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/yasyf/cc-context/internal/render"
)

// trunkCommitFormat lays a foreign commit out as "<short sha> <subject>". %s is
// the subject line alone, so a body cannot spill into a second record.
const trunkCommitFormat = "--format=%h %s"

// TrunkCommit is one commit the local trunk branch carries that the
// remote-tracking trunk does not.
type TrunkCommit struct {
	SHA     string `json:"sha"`
	Subject string `json:"subject,omitempty"`
}

// TrunkState is the local trunk branch's fitness to be rebased onto. A restack
// moves a stack onto the local ref, not the remote-tracking one, so whatever
// the local ref carries beyond the remote lands in every branch of that stack.
// Holder and Dirty are why such a ref stays stuck: git refuses to fetch into a
// branch checked out elsewhere, and dirt blocks the holder's own fast-forward.
type TrunkState struct {
	Trunk   string        `json:"trunk"`
	Remote  string        `json:"remote"`
	Holder  string        `json:"holder,omitempty"`
	Stale   bool          `json:"stale_holder,omitempty"`
	Dirty   int           `json:"dirty,omitempty"`
	Behind  int           `json:"behind,omitempty"`
	Foreign []TrunkCommit `json:"foreign,omitempty"`
}

// Contaminated reports whether local trunk carries commits the remote does not.
func (s TrunkState) Contaminated() bool { return len(s.Foreign) > 0 }

// Healthy reports whether local trunk is exactly the remote's, the only state
// in which a restack reproduces the stack its author wrote.
func (s TrunkState) Healthy() bool { return s.Behind == 0 && !s.Contaminated() }

// ReadTrunkState measures the local trunk branch against the remote-tracking
// one. dir is any working copy of the repository, siblings sharing one ref
// namespace; holder is the checkout holding trunk, which BranchHolders answers
// and which is empty when none does. Two git calls when trunk is healthy and
// unheld, one more per fault found.
//
// A repository with no local trunk branch reads healthy, that miss proven with
// show-ref rather than inferred from a rev-list failure.
func ReadTrunkState(ctx context.Context, dir render.Dir, trunk Trunk, holder string) (TrunkState, error) {
	s := TrunkState{Trunk: trunk.Name(), Remote: trunk.Remote()}
	local := LocalBranchRef(trunk.Name())
	exists, err := refExists(ctx, dir, local)
	if err != nil {
		return TrunkState{}, err
	}
	if !exists {
		return s, nil
	}

	s.Holder = holder
	behind, ahead, err := countTrunkSpan(ctx, dir, trunk.Ref(), local)
	if err != nil {
		return TrunkState{}, err
	}
	s.Behind = behind
	if holder != "" {
		if s.Dirty, s.Stale, err = trunkHolderDirt(ctx, holder); err != nil {
			return TrunkState{}, err
		}
	}
	if ahead > 0 {
		if s.Foreign, err = readTrunkCommits(ctx, dir, trunk.Ref(), local); err != nil {
			return TrunkState{}, err
		}
	}
	return s, nil
}

// TrunkHolders lists every working copy with trunk checked out. BranchHolders
// keeps one entry per branch, and `git worktree add --force` permits several —
// a caller about to move the ref has to see all of them.
func TrunkHolders(ctx context.Context, c Checkout, trunk Trunk) ([]string, error) {
	list, err := Worktrees(ctx, c)
	if err != nil {
		return nil, err
	}
	var holders []string
	for _, wt := range list {
		if wt.Branch == trunk.Name() {
			holders = append(holders, wt.Path)
		}
	}
	return holders, nil
}

// ResolveRef reads ref's object id, for a caller that needs the value it
// measured rather than the ref's name — an update-ref's expected old value.
func ResolveRef(ctx context.Context, dir render.Dir, ref GitRef) (string, error) {
	out, err := render.RunCLI(ctx, dir, "git", []string{"rev-parse", "--verify", "--end-of-options", string(ref)})
	if err != nil {
		return "", fmt.Errorf("git rev-parse %s: %w", ref, err)
	}
	return strings.TrimSpace(out), nil
}

// refExists separates git's clean miss at exit 1 from a git that could not
// answer at all.
func refExists(ctx context.Context, dir render.Dir, ref GitRef) (bool, error) {
	_, code, stderr, err := render.RunCLIExitCode(ctx, dir, "git", []string{"show-ref", "--verify", "--quiet", string(ref)})
	if err != nil {
		return false, fmt.Errorf("git show-ref %s: %w", ref, err)
	}
	switch code {
	case 0:
		return true, nil
	case 1:
		return false, nil
	default:
		return false, fmt.Errorf("git show-ref %s: exit %d: %s", ref, code, strings.TrimSpace(stderr))
	}
}

// countTrunkSpan counts both sides of the symmetric difference between the
// remote-tracking trunk and the local branch in one walk.
func countTrunkSpan(ctx context.Context, dir render.Dir, remote, local GitRef) (behind, ahead int, err error) {
	span := string(remote) + "..." + string(local)
	out, err := render.RunCLI(ctx, dir, "git", []string{"rev-list", "--left-right", "--count", "--end-of-options", span})
	if err != nil {
		return 0, 0, fmt.Errorf("git rev-list %s: %w", span, err)
	}
	fields := strings.Fields(out)
	if len(fields) != 2 {
		return 0, 0, fmt.Errorf("malformed rev-list count %q for %s", out, span)
	}
	if behind, err = strconv.Atoi(fields[0]); err != nil {
		return 0, 0, fmt.Errorf("malformed rev-list count %q for %s: %w", out, span, err)
	}
	if ahead, err = strconv.Atoi(fields[1]); err != nil {
		return 0, 0, fmt.Errorf("malformed rev-list count %q for %s: %w", out, span, err)
	}
	return behind, ahead, nil
}

// trunkHolderDirt counts the uncommitted paths in the working copy holding
// trunk, reporting a registration whose tree is gone as stale rather than
// failing — it still pins the ref. --no-optional-locks keeps the count
// read-only, and --untracked-files pins the mode against a
// status.showUntrackedFiles=no that reads untracked work as clean.
func trunkHolderDirt(ctx context.Context, holder string) (dirty int, stale bool, err error) {
	if _, err := os.Stat(holder); os.IsNotExist(err) {
		return 0, true, nil
	} else if err != nil {
		return 0, false, fmt.Errorf("stat %q: %w", holder, err)
	}
	entries, err := GitStatus(ctx, GitArgs{Dir: render.Dir(holder), Sub: []string{"--no-optional-locks", "status", "--untracked-files=normal"}})
	if err != nil {
		return 0, false, fmt.Errorf("status in %q: %w", holder, err)
	}
	return len(entries), false, nil
}

// readTrunkCommits lists what local trunk carries beyond the remote, newest
// first — the commits a restack would splice into the stack.
// --no-show-signature pins the output against a log.showSignature=true, whose
// verification lines this parse would read as commits.
func readTrunkCommits(ctx context.Context, dir render.Dir, remote, local GitRef) ([]TrunkCommit, error) {
	span := string(remote) + ".." + string(local)
	out, err := render.RunCLI(ctx, dir, "git", []string{"log", "--no-show-signature", trunkCommitFormat, "--end-of-options", span})
	if err != nil {
		return nil, fmt.Errorf("git log %s: %w", span, err)
	}
	var commits []TrunkCommit
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		sha, subject, _ := strings.Cut(line, " ")
		commits = append(commits, TrunkCommit{SHA: sha, Subject: subject})
	}
	return commits, nil
}
