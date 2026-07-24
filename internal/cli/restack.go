package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"

	"github.com/spf13/cobra"

	"github.com/yasyf/cc-context/internal/render"
	"github.com/yasyf/cc-context/internal/vcs"
)

const (
	// gtSyncConflict through gtSyncAuthRequired2 are gt 1.8.6's own wording
	// for classifyGTRestack; version-dependent, kept as lone constants so an
	// upgrade is a one-line change (precedent: gtRestackNeeded1).
	gtSyncConflict      = "Hit conflict restacking"
	gtSyncAuthRequired1 = "Please authenticate your Graphite CLI"
	gtSyncAuthRequired2 = "Your Graphite auth token is invalid/expired"

	jjRestackAncestorRevset = "trunk() & ::@"
	jjRestackStackRevset    = "trunk()..@"
	jjRestackConflictRevset = "conflicts() & @::"
)

type restackOpts struct {
	noGT bool
}

func newRestackCmd() *cobra.Command {
	var o restackOpts
	cmd := &cobra.Command{
		Use:     "restack",
		Aliases: []string{"rebase"},
		Short:   "Fetch and restack the working-copy stack onto trunk",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runRestack(cmd, o)
		},
	}
	cmd.Flags().BoolVar(&o.noGT, "no-gt", false, "ignore a live graphite config and fall back to the jj/git detection")
	return cmd
}

func runRestack(cmd *cobra.Command, o restackOpts) error {
	ctx := cmd.Context()
	kind, root := vcs.DetectRoot(workingDir())
	if kind == vcs.None {
		return errors.New("restack: no git or jj repository in the working directory")
	}

	gtLane := !o.noGT && (kind == vcs.Git || kind == vcs.JJ) && vcs.GraphiteRepo(root)
	if gtLane {
		if _, err := exec.LookPath("gt"); err != nil {
			return errors.New("restack: graphite config found but gt not on PATH — install graphite (brew install graphite) or pass --no-gt")
		}
		summary, err := restackGT(ctx, cmd.ErrOrStderr())
		if err != nil {
			return err
		}
		cmd.Println(summary)
		return nil
	}

	var summary string
	var err error
	switch kind {
	case vcs.JJ:
		summary, err = restackJJ(ctx)
	case vcs.Git:
		summary, err = restackGit(ctx)
	default:
		panic(fmt.Sprintf("restack: unsupported vcs kind %d", kind))
	}
	if err != nil {
		return err
	}
	cmd.Println(summary)
	return nil
}

func restackGT(ctx context.Context, errW io.Writer) (string, error) {
	state, err := gtStateQuery(ctx)
	if err != nil {
		return "", fmt.Errorf("restack: graphite state: %w", err)
	}
	trunk, err := gtTrunkBranch(state)
	if err != nil {
		return "", fmt.Errorf("restack: graphite trunk: %w", err)
	}
	beforeHead, beforeTrunk, err := gtRestackShas(ctx, trunk)
	if err != nil {
		return "", err
	}

	argv := []string{"sync", "--no-interactive"}
	if shipStreamCI(errW) {
		var output strings.Builder
		if err := render.RunCLIStream(ctx, "gt", argv, io.MultiWriter(errW, &output)); err != nil {
			return "", classifyGTRestack(fmt.Errorf("%w: %s", err, strings.TrimSpace(output.String())))
		}
	} else if _, err := render.RunCLI(ctx, "gt", argv); err != nil {
		return "", classifyGTRestack(err)
	}

	afterHead, afterTrunk, err := gtRestackShas(ctx, trunk)
	if err != nil {
		return "", err
	}
	if beforeHead == afterHead && beforeTrunk == afterTrunk {
		return "already up to date · trunk " + trunk, nil
	}
	return "restacked · trunk " + trunk, nil
}

// gtRestackShas resolves HEAD's and trunk's current commit SHAs, so restackGT
// can tell a no-op gt sync from one that actually moved something.
func gtRestackShas(ctx context.Context, trunk string) (head, trunkSHA string, err error) {
	head, err = gitRevParseSHA(ctx, "HEAD")
	if err != nil {
		return "", "", err
	}
	trunkSHA, err = gitRevParseSHA(ctx, trunk)
	if err != nil {
		return "", "", err
	}
	return head, trunkSHA, nil
}

// gitRevParseSHA resolves ref to its commit SHA.
func gitRevParseSHA(ctx context.Context, ref string) (string, error) {
	out, err := render.RunCLI(ctx, "git", []string{"rev-parse", ref})
	if err != nil {
		return "", fmt.Errorf("restack: git rev-parse %s: %w", ref, err)
	}
	return strings.TrimSpace(out), nil
}

func classifyGTRestack(err error) error {
	msg := err.Error()
	switch {
	case strings.Contains(msg, gtSyncConflict):
		return errors.New("restack: conflict — resolve the listed files, then gt continue (or gt abort); see the output above")
	case strings.Contains(msg, gtSyncAuthRequired1) || strings.Contains(msg, gtSyncAuthRequired2):
		return errors.New("restack: graphite auth required — run gt auth")
	default:
		return fmt.Errorf("restack: gt sync: %w", err)
	}
}

func restackJJ(ctx context.Context) (string, error) {
	trunkNames, err := jjTrunkBookmarkNames(ctx)
	if err != nil {
		return "", fmt.Errorf("restack: resolve jj trunk bookmark: %w", err)
	}
	if len(trunkNames) != 1 {
		return "", fmt.Errorf("restack: cannot resolve the trunk bookmark from %q — configure trunk() to resolve one tracked bookmark", trunkNames)
	}
	trunk := trunkNames[0]

	if _, err := render.RunCLI(ctx, "jj", []string{"git", "fetch"}); err != nil {
		return "", fmt.Errorf("restack: jj git fetch: %w", err)
	}
	ancestors, err := jjLogLines(ctx, jjRestackAncestorRevset)
	if err != nil {
		return "", fmt.Errorf("restack: inspect jj trunk ancestry: %w", err)
	}
	if len(ancestors) > 0 {
		return "fetched · already up to date", nil
	}

	rebased, err := jjRestackOntoTrunk(ctx, trunk)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("fetched · rebased %d commit(s) onto %s", rebased, trunk), nil
}

func jjRestackOntoTrunk(ctx context.Context, trunk string) (int, error) {
	stack, err := jjLogLines(ctx, jjRestackStackRevset)
	if err != nil {
		return 0, fmt.Errorf("restack: inspect jj stack: %w", err)
	}
	if len(stack) == 0 {
		return 0, fmt.Errorf("restack: trunk %q is not an ancestor of @ but trunk()..@ is empty", trunk)
	}

	if _, err := render.RunCLI(ctx, "jj", []string{"rebase", "-b", "@", "--destination", "trunk()"}); err != nil {
		return 0, fmt.Errorf("restack: jj rebase onto trunk %q: %w — retry manually: jj rebase -b @ --destination 'trunk()'", trunk, err)
	}
	rebaseOp, err := jjOpID(ctx)
	if err != nil {
		return 0, fmt.Errorf("restack: read jj rebase operation: %w", err)
	}

	conflicts, err := jjLogLines(ctx, jjRestackConflictRevset)
	cleanup := context.WithoutCancel(ctx)
	if err != nil {
		_, revertErr := render.RunCLI(cleanup, "jj", []string{"op", "revert", rebaseOp})
		if revertErr == nil {
			return 0, fmt.Errorf("restack: conflict check after rebase onto %q failed (rebase rolled back): %w", trunk, err)
		}
		return 0, fmt.Errorf("restack: conflict check after rebase onto %q failed: %w; rollback also failed: %w — run: jj op revert %s", trunk, err, revertErr, rebaseOp)
	}
	if len(conflicts) > 0 {
		if _, revertErr := render.RunCLI(cleanup, "jj", []string{"op", "revert", rebaseOp}); revertErr != nil {
			return 0, fmt.Errorf("restack: rebase onto %q conflicted and rollback failed: %w — run: jj op revert %s, then resolve manually", trunk, revertErr, rebaseOp)
		}
		return 0, fmt.Errorf("restack: rebase onto %q conflicts in %d commit(s); rolled back to the pre-rebase state\nconflicted:\n  %s\nresolve manually: jj rebase -b @ --destination 'trunk()', then fix the conflicts (jj status)", trunk, len(conflicts), strings.Join(conflicts, "\n  "))
	}
	return len(stack), nil
}

func restackGit(ctx context.Context) (string, error) {
	out, err := render.RunCLI(ctx, "git", []string{"branch", "--show-current"})
	if err != nil {
		return "", fmt.Errorf("restack: git branch --show-current: %w", err)
	}
	branch := strings.TrimSpace(out)
	if branch == "" {
		return "", errors.New("restack: detached HEAD — check out a branch before restacking")
	}
	remote, err := gitRemoteFor(ctx, branch)
	if err != nil {
		return "", fmt.Errorf("restack: resolve remote for %s: %w", branch, err)
	}
	if _, err := render.RunCLI(ctx, "git", []string{"fetch", remote}); err != nil {
		return "", fmt.Errorf("restack: git fetch %s: %w", remote, err)
	}

	trunk, err := gitRemoteTrunk(ctx, remote)
	if err != nil {
		return "", err
	}
	remoteTrunk := remote + "/" + trunk
	upToDate, err := gitIsAncestor(ctx, remoteTrunk, "HEAD")
	if err != nil {
		return "", fmt.Errorf("restack: compare HEAD with %s: %w", remoteTrunk, err)
	}
	if upToDate {
		return "fetched · already up to date", nil
	}

	if branch == trunk {
		if _, err := render.RunCLI(ctx, "git", []string{"merge", "--ff-only", remoteTrunk}); err != nil {
			return "", fmt.Errorf("restack: fast-forward %s to %s: %w — resolve manually: git fetch %s && git merge --ff-only %s", branch, remoteTrunk, err, remote, remoteTrunk)
		}
		return "fetched · fast-forwarded " + trunk, nil
	}

	if _, err := gitRebaseOnto(ctx, remote, trunk); err != nil {
		return "", err
	}
	return "fetched · rebased onto " + remoteTrunk, nil
}

func gitRemoteTrunk(ctx context.Context, remote string) (string, error) {
	ref := "refs/remotes/" + remote + "/HEAD"
	out, code, _, err := render.RunCLIExitCode(ctx, "git", []string{"symbolic-ref", "--short", ref})
	if err != nil {
		return "", fmt.Errorf("restack: git symbolic-ref %s: %w", ref, err)
	}
	if code == 0 {
		name := strings.TrimSpace(out)
		prefix := remote + "/"
		if trunk, ok := strings.CutPrefix(name, prefix); ok && trunk != "" {
			return trunk, nil
		}
		return "", fmt.Errorf("restack: cannot resolve %s's default branch — run git remote set-head %s -a", remote, remote)
	}

	for _, trunk := range []string{"main", "master"} {
		candidate := "refs/remotes/" + remote + "/" + trunk
		_, code, stderr, err := render.RunCLIExitCode(ctx, "git", []string{"show-ref", "--verify", candidate})
		if err != nil {
			return "", fmt.Errorf("restack: git show-ref %s: %w", candidate, err)
		}
		switch code {
		case 0:
			return trunk, nil
		case 1:
		default:
			return "", fmt.Errorf("restack: git show-ref %s: exit %d: %s", candidate, code, strings.TrimSpace(stderr))
		}
	}
	return "", fmt.Errorf("restack: cannot resolve %s's default branch — run git remote set-head %s -a", remote, remote)
}
