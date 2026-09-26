package cli

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/yasyf/cc-context/internal/gtmeta"
	"github.com/yasyf/cc-context/internal/render"
	"github.com/yasyf/cc-context/internal/vcs"
)

type vcsPushOpts struct {
	noVerify bool
}

func newVcsPushCmd() *cobra.Command {
	var o vcsPushOpts
	cmd := &cobra.Command{
		Use:   "push",
		Short: "Push the current branch, force-pushing under a lease when its history was rewritten",
		Long: `Push the current branch, force-pushing under a lease when its history was rewritten.

A branch rebased onto a new base, amended, or reordered no longer descends from
the head its remote holds, and git refuses a plain push of it as non-fast-forward.
Ship answers that by rebasing the local branch onto the remote, which for a
deliberate rewrite is backwards — it replays the new commits onto the history
they replaced. Push is the other answer: it moves the remote to the local head.

Push fetches first and grades what it finds. A remote head that is an ancestor of
HEAD fast-forwards, pushed with no force at all. A remote head this branch itself
once held — one its reflog still carries, as an entry or as an ancestor of one —
is a rewrite, and the push carries --force-with-lease pinned to the head this run
observed, so it lands only while the remote still sits where the grading found
it. A refused lease is reported rather than retried: something advanced the branch
mid-run, and reconciling that is a person's call. A remote head no reflog entry
of this branch reaches is work this branch has never held — a divergence, not a
rewrite — and push refuses it, naming the commits the force would drop.

Push moves one branch and nothing else. A graphite stack's bases live in
Graphite's own record, which only ccx vcs stack submit writes. A branch a stack
submit published carries a publication receipt, and push rewrites it to the head
it pushed, as ship does, so the next ship or stack rebase reads that head as the
branch's published version rather than as someone else's push.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runVcsPush(cmd, o)
		},
	}
	cmd.Flags().BoolVar(&o.noVerify, "no-verify", false, "skip the repository's pre-push hook")
	return cmd
}

func runVcsPush(cmd *cobra.Command, o vcsPushOpts) error {
	ctx := cmd.Context()
	ck, err := vcs.ResolveCheckout(workingDir(ctx))
	if err != nil {
		return fmt.Errorf("push: %w", err)
	}
	switch ck.Kind {
	case vcs.None:
		return errors.New("push: no git repository in the working directory")
	case vcs.JJ:
		return errors.New("push: jj git push already moves a rewritten bookmark under a lease of its own — run ccx vcs ship")
	}
	dir := render.Dir(ck.Root)
	branch, err := gitCurrentBranch(ctx, dir, "push")
	if err != nil {
		return err
	}
	if branch == "" {
		return errors.New("push: HEAD is detached — check out the branch you mean to move")
	}
	remote, err := gitRemoteFor(ctx, dir, "push", branch)
	if err != nil {
		return err
	}
	head, err := gitRevParse(ctx, dir, "push", "HEAD")
	if err != nil {
		return err
	}
	receipt, prior, err := vcsPushPublication(ctx, dir, remote, branch, head)
	if err != nil {
		return err
	}
	summary, err := vcsPushGit(ctx, dir, remote, branch, head, o.noVerify)
	if err != nil {
		return err
	}
	if receipt != nil {
		var tx strings.Builder
		tx.WriteString("start\n")
		if err := stackReceiptTx(ctx, dir, &tx, *receipt, prior); err != nil {
			return err
		}
		tx.WriteString("commit\n")
		if _, err := render.RunCLIStdin(ctx, dir, "git", []string{"update-ref", "--stdin"}, []byte(tx.String())); err != nil {
			return fmt.Errorf("push: record %s's publication at %s: %w", branch, shortOID(head), err)
		}
		if err := vcsPushSubmitted(ctx, dir, *receipt); err != nil {
			return err
		}
	}
	cmd.Println(summary)
	return nil
}

// vcsPushSubmitted moves Graphite's last submitted version to the receipt push
// wrote, when there is one, so Graphite and the receipt agree on what the remote
// holds.
func vcsPushSubmitted(ctx context.Context, dir render.Dir, receipt stackPublication) error {
	commonDir, err := gtCommonDir(ctx, dir, "push")
	if err != nil {
		return err
	}
	submitted, err := gtmeta.LastSubmitted(ctx, commonDir)
	if err != nil {
		return fmt.Errorf("push: %w", err)
	}
	if submitted[receipt.Branch] == (gtmeta.Version{}) {
		return nil
	}
	version := gtmeta.Version{HeadSha: receipt.Head, BaseSha: receipt.Base, BaseName: receipt.Parent}
	if err := gtmeta.RecordSubmitted(ctx, commonDir, map[string]gtmeta.Version{receipt.Branch: version}); err != nil {
		return fmt.Errorf("push: record %s's submitted version at %s: %w", receipt.Branch, shortOID(receipt.Head), err)
	}
	return nil
}

func vcsPushPublication(ctx context.Context, dir render.Dir, remote, branch, head string) (*stackPublication, string, error) {
	prior, err := stackReadPublication(ctx, dir, branch)
	if err != nil || prior == nil {
		return nil, "", err
	}
	state, err := gtStateQuery(ctx, dir, "push")
	if err != nil {
		return nil, "", err
	}
	parents := state[branch].Parents
	if len(parents) == 0 {
		return nil, "", nil
	}
	parent := parents[0]
	candidates := []string{prior.Base, parent.SHA}
	for _, tip := range []string{"refs/heads/" + parent.Ref, "refs/remotes/" + remote + "/" + parent.Ref} {
		present, err := gitRefExists(ctx, dir, "push", tip)
		if err != nil {
			return nil, "", err
		}
		if !present {
			continue
		}
		fork, err := render.RunCLI(ctx, dir, "git", []string{"merge-base", head, tip})
		if err != nil {
			return nil, "", fmt.Errorf("push: git merge-base %s %s: %w", shortOID(head), tip, err)
		}
		candidates = append(candidates, strings.TrimSpace(fork))
	}
	base := ""
	for _, candidate := range candidates {
		held, err := gitIsAncestor(ctx, dir, "push", candidate, head)
		if err != nil {
			return nil, "", err
		}
		if !held {
			continue
		}
		if base == "" {
			base = candidate
			continue
		}
		further, err := gitIsAncestor(ctx, dir, "push", base, candidate)
		if err != nil {
			return nil, "", err
		}
		if further {
			base = candidate
		}
	}
	if base == "" {
		return nil, "", fmt.Errorf("push: %s's published base %s and its parent %s at %s are both off HEAD, so no base marks its own commits; publish it with ccx vcs stack submit instead", branch, shortOID(prior.Base), parent.Ref, shortOID(parent.SHA))
	}
	return &stackPublication{Branch: branch, Source: head, SourceBase: base, Head: head, Base: base, Parent: parent.Ref}, prior.OID, nil
}

func vcsPushGit(ctx context.Context, dir render.Dir, remote, branch, head string, noVerify bool) (string, error) {
	if _, err := render.RunCLI(ctx, dir, "git", []string{"fetch", remote}); err != nil {
		return "", fmt.Errorf("push: git fetch %s: %w", remote, err)
	}
	ref := "refs/heads/" + branch
	refspec := head + ":" + ref
	remoteRef := "refs/remotes/" + remote + "/" + branch
	present, err := gitRefExists(ctx, dir, "push", remoteRef)
	if err != nil {
		return "", err
	}
	if !present {
		if _, err := render.RunCLI(ctx, dir, "git", gitPushArgv(noVerify, remote, refspec)); err != nil {
			return "", fmt.Errorf("push: git push: %w", err)
		}
		return fmt.Sprintf("pushed %s → %s · created %s/%s at %s", branch, remote, remote, branch, shortOID(head)), nil
	}
	tip, err := gitRevParse(ctx, dir, "push", remoteRef)
	if err != nil {
		return "", err
	}
	if tip == head {
		return fmt.Sprintf("%s/%s already at %s — nothing to push", remote, branch, shortOID(head)), nil
	}
	ancestor, err := gitIsAncestor(ctx, dir, "push", tip, head)
	if err != nil {
		return "", err
	}
	if ancestor {
		if _, err := render.RunCLI(ctx, dir, "git", gitPushArgv(noVerify, remote, refspec)); err != nil {
			return "", fmt.Errorf("push: git push: %w", err)
		}
		return fmt.Sprintf("pushed %s → %s · %s..%s", branch, remote, shortOID(tip), shortOID(head)), nil
	}
	rewrite, err := gitReflogHolds(ctx, dir, "push", branch, tip)
	if err != nil {
		return "", err
	}
	if !rewrite {
		unheld, err := gitCommitsNotIn(ctx, dir, "push", tip, head)
		if err != nil {
			return "", err
		}
		return "", fmt.Errorf("push: %s/%s carries %d commit(s) %s has never held, so this is a divergence, not a rewrite, and the force would drop them:\n%s\nfetch and reconcile first: git fetch %s && git rebase %s/%s",
			remote, branch, len(unheld), branch, strings.Join(unheld, "\n"), remote, remote, branch)
	}
	lease := fmt.Sprintf("--force-with-lease=%s:%s", ref, tip)
	if _, err := render.RunCLI(ctx, dir, "git", gitPushArgv(noVerify, remote, lease, refspec)); err != nil {
		if gitPushStaleLease(err) || gitPushRejected(err) {
			return "", fmt.Errorf("push: %s/%s moved off %s after this run graded it — someone pushed mid-run; fetch and reconcile before moving it again: %w", remote, branch, shortOID(tip), err)
		}
		return "", fmt.Errorf("push: git push: %w", err)
	}
	return fmt.Sprintf("force-pushed %s → %s · replaced %s with %s", branch, remote, shortOID(tip), shortOID(head)), nil
}

// gitReflogHolds reports whether branch's reflog reaches sha, as an entry or an
// ancestor of one — a rebase or amend leaves the head it replaced there, so a
// reachable remote head is this branch's own earlier history. Patch identity
// cannot answer it: a rebase onto a new base rewrites the context of every hunk
// it replays, giving the same work different patch ids on either side.
func gitReflogHolds(ctx context.Context, dir render.Dir, prefix, branch, sha string) (bool, error) {
	out, err := render.RunCLI(ctx, dir, "git", []string{"reflog", "show", "--format=%H", "refs/heads/" + branch})
	if err != nil {
		return false, fmt.Errorf("%s: git reflog show %s: %w", prefix, branch, err)
	}
	seen := map[string]bool{}
	for _, entry := range strings.Fields(out) {
		if seen[entry] {
			continue
		}
		seen[entry] = true
		held, err := gitIsAncestor(ctx, dir, prefix, sha, entry)
		if err != nil {
			return false, err
		}
		if held {
			return true, nil
		}
	}
	return false, nil
}

func gitCommitsNotIn(ctx context.Context, dir render.Dir, prefix, rev, exclude string) ([]string, error) {
	out, err := render.RunCLI(ctx, dir, "git", []string{"log", "--format=  %h %s", rev, "^" + exclude})
	if err != nil {
		return nil, fmt.Errorf("%s: git log %s ^%s: %w", prefix, rev, exclude, err)
	}
	return strings.Split(strings.TrimRight(out, "\n"), "\n"), nil
}

func gitRevParse(ctx context.Context, dir render.Dir, prefix, rev string) (string, error) {
	out, err := render.RunCLI(ctx, dir, "git", []string{"rev-parse", rev})
	if err != nil {
		return "", fmt.Errorf("%s: git rev-parse %s: %w", prefix, rev, err)
	}
	return strings.TrimSpace(out), nil
}

func shortOID(sha string) string { return sha[:12] }
