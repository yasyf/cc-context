package vcs

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/yasyf/cc-context/internal/render"
)

// ErrNoTrunk reports that the repository designates no default branch — git
// answered the question and the answer was "none". It is never a stand-in for a
// failed query: --quiet turns symbolic-ref's miss into exit 1 and leaves exit 128
// for a git that genuinely broke, so the two can be told apart and each caller
// picks its own policy (ship appends where it stands, worktree rm skips its
// guard, info reports an empty trunk, restack refuses).
var ErrNoTrunk = errors.New("no default branch configured")

// ResolveTrunk reads remote's designated default branch out of
// refs/remotes/<remote>/HEAD, returning ErrNoTrunk when the repository
// designates none. It asks for the full ref, never --short: a --short answer has
// to be re-lengthened by trimming "<remote>/" back off, which cannot tell
// origin's branch "main" from a tag "main" that origin/HEAD legally points at,
// and hands the caller a name it must re-qualify to use. A target outside
// refs/remotes/<remote>/ is an error rather than a miss — git named a ref, so
// reporting "no trunk" would discard an answer instead of surfacing a
// misconfiguration.
func ResolveTrunk(ctx context.Context, dir render.Dir, remote string) (Trunk, error) {
	head := "refs/remotes/" + remote + "/HEAD"
	out, code, stderr, err := render.RunCLIExitCode(ctx, dir, "git", []string{"symbolic-ref", "--quiet", head})
	if err != nil {
		return Trunk{}, fmt.Errorf("git symbolic-ref %s: %w", head, err)
	}
	switch code {
	case 0:
	case 1:
		return Trunk{}, fmt.Errorf("%s: %w — run git remote set-head %s -a", head, ErrNoTrunk, remote)
	default:
		return Trunk{}, fmt.Errorf("git symbolic-ref %s: exit %d: %s", head, code, strings.TrimSpace(stderr))
	}
	target := strings.TrimSpace(out)
	prefix := "refs/remotes/" + remote + "/"
	name, ok := strings.CutPrefix(target, prefix)
	if !ok || name == "" {
		return Trunk{}, fmt.Errorf("%s points at %q, which names no branch of %s — run git remote set-head %s -a", head, target, remote, remote)
	}
	return Trunk{name: name, remote: remote, ref: GitRef(target)}, nil
}

// TrunkFromName qualifies a trunk name that came from outside git — gt's stack
// state, a --branch flag — into the remote-tracking ref, verifying it exists.
// The verification is the point: the name arrives unqualified, and a caller that
// pasted it straight into git plumbing would be answered by a local branch or
// tag of the same name. A name with no remote-tracking ref is ErrNoTrunk.
func TrunkFromName(ctx context.Context, dir render.Dir, remote, name string) (Trunk, error) {
	ref := RemoteBranchRef(remote, name)
	_, code, stderr, err := render.RunCLIExitCode(ctx, dir, "git", []string{"show-ref", "--verify", "--quiet", string(ref)})
	if err != nil {
		return Trunk{}, fmt.Errorf("git show-ref %s: %w", ref, err)
	}
	switch code {
	case 0:
		return Trunk{name: name, remote: remote, ref: ref}, nil
	case 1:
		return Trunk{}, fmt.Errorf("%s: %w — run git fetch %s %s", ref, ErrNoTrunk, remote, name)
	default:
		return Trunk{}, fmt.Errorf("git show-ref %s: exit %d: %s", ref, code, strings.TrimSpace(stderr))
	}
}

// GitRemoteFor resolves the remote that branch.<branch>.remote configures, so a
// triangular or non-origin-only repository fetches, rebases, and pushes against
// the same remote. git config --get exits 1 when the key is unset; that and an
// empty value both mean origin. Any other exit is an error.
func GitRemoteFor(ctx context.Context, dir render.Dir, branch string) (string, error) {
	key := "branch." + branch + ".remote"
	out, code, stderr, err := render.RunCLIExitCode(ctx, dir, "git", []string{"config", "--get", key})
	if err != nil {
		return "", fmt.Errorf("git config %s: %w", key, err)
	}
	switch code {
	case 0:
		if remote := strings.TrimSpace(out); remote != "" {
			return remote, nil
		}
		return "origin", nil
	case 1:
		return "origin", nil
	default:
		return "", fmt.Errorf("git config %s: exit %d: %s", key, code, strings.TrimSpace(stderr))
	}
}
