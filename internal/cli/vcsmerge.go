package cli

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/yasyf/cc-context/internal/nativeapi"
	"github.com/yasyf/cc-context/internal/render"
	"github.com/yasyf/cc-context/internal/vcs"
)

type vcsMergeOpts struct {
	repo                  string
	admin                 bool
	squash, merge, rebase bool
}

type mergeClientKey struct{}

func withMergeClient(ctx context.Context, client *nativeapi.Client) context.Context {
	return context.WithValue(ctx, mergeClientKey{}, client)
}

func mergeClient(ctx context.Context) *nativeapi.Client {
	if client, ok := ctx.Value(mergeClientKey{}).(*nativeapi.Client); ok {
		return client
	}
	return nativeapi.Default()
}

func newVcsMergeCmd() *cobra.Command {
	var o vcsMergeOpts
	cmd := &cobra.Command{
		Use:   "merge [<pr|branch>]",
		Short: "Merge a pull request, and every open pull request below it in its native stack, and report each merge commit",
		Long: `Merge one pull request, named by number or by its head branch, or the current
branch's when neither is given, through GitHub's asynchronous merge API.

That API is the one a native stacked pull request requires: gh pr merge refuses
one. A stacked pull request merges together with every open pull request below
it in its native GitHub stack, and the report names each one bottom-up with its
merge commit. A pull request outside any stack merges on its own the same way.

The merge requires the head GitHub holds when the command reads the pull
request, so a push in between cancels it. Without --admin GitHub's merge queue
takes the pull request when the base branch has one, and the report reads
enqueued. --admin merges directly and bypasses the repository rules the caller
may bypass.

The command reads the repository's merge queue from a checkout of it, and
refuses when that queue is Trunk or Graphite, or when legacy Graphite branches
leave the repository's queue mixed: those queues land a pull request themselves,
and a merge from here would skip them. Run it from a checkout of the repository
so it can tell. On a repository your own account owns, --admin merges past that
queue, from any checkout; on any other repository --admin never does.

The merge method is squash unless --merge or --rebase says otherwise. A merge
still pending after ten minutes stops waiting; rerunning the command resumes
the same request.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runVcsMerge(cmd, args, o)
		},
	}
	cmd.Flags().StringVarP(&o.repo, "repo", "R", "", "the owner/name repository (default: the current checkout's)")
	cmd.Flags().BoolVar(&o.admin, "admin", false, "merge directly, past GitHub's merge queue and the repository rules the caller may bypass")
	cmd.Flags().BoolVar(&o.squash, "squash", false, "squash the pull request into one commit (the default)")
	cmd.Flags().BoolVar(&o.merge, "merge", false, "merge with a merge commit")
	cmd.Flags().BoolVar(&o.rebase, "rebase", false, "rebase the pull request's commits onto the base")
	cmd.MarkFlagsMutuallyExclusive("squash", "merge", "rebase")
	return cmd
}

func runVcsMerge(cmd *cobra.Command, args []string, o vcsMergeOpts) error {
	ctx := cmd.Context()
	dir := render.Dir(workingDir(ctx))
	req := nativeapi.MergeRequest{Method: "squash", Admin: o.admin}
	switch {
	case o.merge:
		req.Method = "merge"
	case o.rebase:
		req.Method = "rebase"
	}
	if len(args) == 1 {
		if n, err := strconv.Atoi(strings.TrimPrefix(args[0], "#")); err == nil && n > 0 {
			req.PRNumber = n
		} else {
			req.Head = args[0]
		}
	} else {
		branch, err := gitCurrentBranch(ctx, dir, "merge")
		if err != nil {
			return err
		}
		if branch == "" {
			return fmt.Errorf("merge: HEAD is detached; name the pull request or branch to merge")
		}
		req.Head = branch
	}
	repo := o.repo
	if repo == "" {
		looked, err := vcs.LookupRepo(ctx, dir, false)
		if err != nil {
			return fmt.Errorf("merge: name the repository with --repo: %w", err)
		}
		repo = looked.NameWithOwner
	}
	owner, name, ok := strings.Cut(repo, "/")
	if !ok || owner == "" || name == "" {
		return fmt.Errorf("merge: %q is not an owner/name repository", repo)
	}
	if err := vcsMergeQueueRefusal(ctx, repo, owner, o.admin); err != nil {
		return err
	}
	req.RepoOwner, req.RepoName = owner, name
	merged, err := mergeClient(ctx).MergePullRequest(ctx, req)
	for _, pr := range merged {
		line := fmt.Sprintf("%s #%d", pr.Status, pr.Number)
		if pr.Head != "" {
			line += " " + pr.Head
		}
		if pr.SHA != "" {
			line += " " + pr.SHA
		}
		if _, err := fmt.Fprintln(cmd.OutOrStdout(), line); err != nil {
			return err
		}
	}
	if err != nil {
		return fmt.Errorf("merge: %w", err)
	}
	return nil
}

func vcsMergeQueueRefusal(ctx context.Context, repo, owner string, admin bool) error {
	client := gtAPIForRepo(ctx, repo)
	var refusal error
	if err := client.ProviderError(); err != nil {
		refusal = fmt.Errorf("merge: %w", err)
	} else if queue := client.QueueProvider(); queue == "trunk" || queue == "graphite" || queue == "mixed" {
		refusal = fmt.Errorf("merge: %s lands through the %s queue, which merges its pull requests itself; enqueue it there, since --admin merges past a queue only on a repository your own account owns", repo, queue)
	}
	if refusal == nil || !admin {
		return refusal
	}
	login, err := mergeClient(ctx).ViewerLogin(ctx)
	if err != nil {
		return errors.Join(refusal, fmt.Errorf("merge: read the signed-in account to tell whether %s is its own: %w", repo, err))
	}
	if strings.EqualFold(login, owner) {
		return nil
	}
	return refusal
}
