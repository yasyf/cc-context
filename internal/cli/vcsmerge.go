package cli

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/spf13/cobra"

	"github.com/yasyf/cc-context/internal/nativeapi"
	"github.com/yasyf/cc-context/internal/prstate"
	"github.com/yasyf/cc-context/internal/render"
	"github.com/yasyf/cc-context/internal/vcs"
)

type vcsMergeOpts struct {
	repo                  string
	head                  string
	redOnMain             int64
	downstack             bool
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
		Use:   "merge [<pr|branch>] --match-head-commit <sha>",
		Short: "Merge a pull request at the head you name, refusing a red check, and report each merge commit",
		Long: `Merge one pull request, named by number or by its head branch, or the current
branch's when neither is given, at the head commit --match-head-commit names.

One read takes the pull request's head, the open pull requests below it in its
native GitHub stack, and the checks on each one's head. The merge refuses when
the head is not the one named, and when a finished check failed on any pull
request it lands, unless --red-on-main names a GitHub Actions run on the default
branch whose latest attempt fails it the same way in the same workflow. A red
job whose log names failing pytest tests, read as ccx vcs job-log --failed-tests
reads it, fails the same way when the run's failed jobs fail every one of those
tests, in any job, since shards rebalance as the test set changes. A red job
whose log names no test, or one whose tests the run does not all fail while a
failed job of the run names none, fails the same way when the run fails the job
of the same name. A job log it cannot read refuses the merge with the read
error. A check still running never refuses.

A native stack member goes through GitHub's asynchronous merge API, the one
gh pr merge cannot use. That API lands every open pull request below it in the
same operation, so the command refuses a stack member with open pull requests
below it unless --with-downstack says to land them together. Any other pull
request goes through the plain merge API. The report reads each landed pull
request back from GitHub and names it, bottom-up, with its merge commit.

--admin works only on a repository your own account owns, and refuses anywhere
else. On a stack member it merges directly, past GitHub's merge queue and the
repository rules the account may bypass; without it GitHub's merge queue takes
the stack member when the base branch has one, and the report reads enqueued.

Without --admin the command reads the merge queue a checkout of the repository
configures, in git config ccx.queue or the [vcs] queue of .claude/ccx.toml, and
refuses when that queue is Trunk or Graphite: those queues land a pull request
themselves, and a merge from here would skip them. Graphite stack metadata in
the checkout configures no queue. Run it from a checkout of the repository so it
can tell.

The merge method is squash unless --merge or --rebase says otherwise. An
asynchronous merge still pending after ten minutes stops waiting; rerunning the
command resumes the same request.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runVcsMerge(cmd, args, o)
		},
	}
	cmd.Flags().StringVarP(&o.repo, "repo", "R", "", "the owner/name repository (default: the current checkout's)")
	cmd.Flags().StringVar(&o.head, "match-head-commit", "", "the full sha the pull request's head must be at, the one its checks graded")
	cmd.Flags().Int64Var(&o.redOnMain, "red-on-main", 0, "a GitHub Actions run on the default branch that fails the same checks, so their failure is inherited")
	cmd.Flags().BoolVar(&o.downstack, "with-downstack", false, "land the open pull requests below a native stack member with it")
	cmd.Flags().BoolVar(&o.admin, "admin", false, "on a repository your own account owns, merge past GitHub's merge queue and the rules the account may bypass")
	cmd.Flags().BoolVar(&o.squash, "squash", false, "squash the pull request into one commit (the default)")
	cmd.Flags().BoolVar(&o.merge, "merge", false, "merge with a merge commit")
	cmd.Flags().BoolVar(&o.rebase, "rebase", false, "rebase the pull request's commits onto the base")
	cmd.MarkFlagsMutuallyExclusive("squash", "merge", "rebase")
	_ = cmd.MarkFlagRequired("match-head-commit")
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
	if err := vcsMergeRefusal(ctx, repo, owner, o.admin); err != nil {
		return err
	}
	req.RepoOwner, req.RepoName = owner, name
	client := mergeClient(ctx)
	plan, err := client.ReadMerge(ctx, req)
	if err != nil {
		return fmt.Errorf("merge: %w", err)
	}
	if plan.Pull.HeadSHA != o.head {
		return fmt.Errorf("merge: PR #%d's head is %s, not %s; merge the head its checks graded, named in full", plan.Pull.Number, plan.Pull.HeadSHA, o.head)
	}
	merged := []nativeapi.MergedPR{{Number: plan.Pull.Number, Head: plan.Pull.Head, Status: "merged", SHA: plan.MergeCommit}}
	if plan.State != nativeapi.PRMerged {
		if err := vcsMergeGate(ctx, client, req, plan, o); err != nil {
			return fmt.Errorf("merge: %w", err)
		}
		merged, err = client.MergePullRequest(ctx, req, plan)
	}
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

func vcsMergeRefusal(ctx context.Context, repo, owner string, admin bool) error {
	if admin {
		login, err := mergeClient(ctx).ViewerLogin(ctx)
		if err != nil {
			return fmt.Errorf("merge: read the signed-in account, since --admin works only on a repository it owns: %w", err)
		}
		if !strings.EqualFold(login, owner) {
			return fmt.Errorf("merge: --admin works only on a repository your own account owns, and %s belongs to %s, not %s", repo, owner, login)
		}
		return nil
	}
	client := gtAPIForRepo(ctx, repo)
	if err := client.ProviderError(); err != nil {
		return fmt.Errorf("merge: %w", err)
	}
	if queue := client.ConfiguredQueue(); queue == "trunk" || queue == "graphite" {
		return fmt.Errorf("merge: %s lands through the %s queue, which merges its pull requests itself; enqueue it there", repo, queue)
	}
	return nil
}

func vcsMergeGate(ctx context.Context, client *nativeapi.Client, req nativeapi.MergeRequest, plan nativeapi.MergePlan, o vcsMergeOpts) error {
	pull := plan.Pull
	if plan.State != nativeapi.PROpen {
		return fmt.Errorf("PR #%d is %s", pull.Number, strings.ToLower(string(plan.State)))
	}
	if len(plan.Below) > 0 && !o.downstack {
		return fmt.Errorf("PR #%d's merge would also land %s below it in its native stack; merge those first, or pass --with-downstack to land them together", pull.Number, mergeNumbers(plan.Below))
	}
	var reds []mergeRed
	for _, pr := range append(slices.Clone(plan.Below), pull) {
		red, err := mergeReds(pr)
		if err != nil {
			return err
		}
		reds = append(reds, red...)
	}
	if len(reds) == 0 {
		return nil
	}
	if o.redOnMain == 0 {
		return fmt.Errorf("%s failed; fix it and merge the new head, or pass --red-on-main <run> naming a %s run that fails the same way", mergeRedList(reds), plan.DefaultBranch)
	}
	run, err := client.ReadActionsRun(ctx, req.RepoOwner, req.RepoName, o.redOnMain)
	if err != nil {
		return err
	}
	if run.Branch != plan.DefaultBranch || strings.HasPrefix(run.Event, "pull_request") {
		return fmt.Errorf("--red-on-main %d names a %s run of %s; it takes a run on %s itself", o.redOnMain, run.Event, run.Branch, plan.DefaultBranch)
	}
	failedOnMain := map[string]bool{}
	var failedJobs []int64
	for _, job := range run.Jobs {
		if statusClassify(strings.ToUpper(cmp.Or(job.Conclusion, job.Status))) == statusFailed {
			failedOnMain[job.Name] = true
			failedJobs = append(failedJobs, job.ID)
		}
	}
	var compared []int64
	for _, r := range reds {
		if r.job && r.workflow == run.WorkflowID {
			compared = append(compared, r.id)
		}
	}
	tests := map[int64][]string{}
	if err := mergeFailedTests(ctx, client, req, compared, tests); err != nil {
		return err
	}
	if slices.ContainsFunc(compared, func(id int64) bool { return len(tests[id]) > 0 }) {
		if err := mergeFailedTests(ctx, client, req, failedJobs, tests); err != nil {
			return err
		}
	}
	var own []mergeRed
	for _, r := range reds {
		if r.job && r.workflow == run.WorkflowID {
			mine, read := ownTests(tests[r.id], failedJobs, tests)
			if read && len(mine) == 0 || !read && failedOnMain[r.name] {
				continue
			}
			r.tests = mine
		}
		own = append(own, r)
	}
	if len(own) > 0 {
		return fmt.Errorf("%s failed, and run %d on %s does not fail the same tests, or the same job where a log names no test, of the same workflow, so the failure is this change's own; fix it and merge the new head", mergeRedList(own), o.redOnMain, plan.DefaultBranch)
	}
	return nil
}

func mergeFailedTests(ctx context.Context, client *nativeapi.Client, req nativeapi.MergeRequest, jobs []int64, into map[int64][]string) error {
	jobs = slices.DeleteFunc(slices.Clone(jobs), func(id int64) bool {
		_, done := into[id]
		return done
	})
	found := make([][]string, len(jobs))
	failures := make([]error, len(jobs))
	var reads sync.WaitGroup
	for i, id := range jobs {
		reads.Go(func() {
			var log string
			log, failures[i] = client.ReadJobLog(ctx, req.RepoOwner, req.RepoName, id)
			found[i] = failedTests(log)
		})
	}
	reads.Wait()
	for i, id := range jobs {
		into[id] = found[i]
	}
	return errors.Join(failures...)
}

func ownTests(failing []string, onMain []int64, tests map[int64][]string) ([]string, bool) {
	if len(failing) == 0 {
		return nil, false
	}
	inherited := map[string]bool{}
	named := true
	for _, id := range onMain {
		named = named && len(tests[id]) > 0
		for _, test := range tests[id] {
			inherited[test] = true
		}
	}
	mine := slices.DeleteFunc(slices.Clone(failing), func(test string) bool { return inherited[test] })
	return mine, named || len(mine) == 0
}

type mergeRed struct {
	pr       int
	name     string
	job      bool
	id       int64
	workflow int64
	tests    []string
}

func mergeReds(pr nativeapi.MergeCandidate) ([]mergeRed, error) {
	var rollup *prstate.Rollup
	if err := json.Unmarshal(pr.Rollup, &rollup); err != nil {
		return nil, fmt.Errorf("parse PR #%d's checks: %w", pr.Number, err)
	}
	if rollup == nil {
		return nil, nil
	}
	if len(rollup.Contexts.Nodes) < rollup.Contexts.TotalCount {
		return nil, fmt.Errorf("PR #%d's head carries %d checks, more than the %d one read covers", pr.Number, rollup.Contexts.TotalCount, len(rollup.Contexts.Nodes))
	}
	rollup = rollup.Latest()
	var reds []mergeRed
	for _, c := range rollup.Contexts.Nodes {
		red, state := mergeRed{pr: pr.Number, name: c.Context}, c.State
		if c.Typename == "CheckRun" {
			red, state = mergeRed{pr: pr.Number, name: c.Name, job: true, id: c.DatabaseID, workflow: c.CheckSuite.WorkflowRun.Workflow.DatabaseID}, cmp.Or(c.Conclusion, c.Status)
		}
		if red.name != gtMergeabilityCheck && statusClassify(state) == statusFailed {
			reds = append(reds, red)
		}
	}
	if len(reds) == 0 && statusClassify(rollup.State) == statusFailed {
		reds = append(reds, mergeRed{pr: pr.Number, name: "rollup " + strings.ToLower(rollup.State)})
	}
	return reds, nil
}

func mergeRedList(reds []mergeRed) string {
	names := make([]string, len(reds))
	for i, r := range reds {
		names[i] = fmt.Sprintf("#%d %s", r.pr, r.name)
		if len(r.tests) > 0 {
			names[i] += fmt.Sprintf(" (%s)", strings.Join(r.tests, ", "))
		}
	}
	return strings.Join(names, ", ")
}

func mergeNumbers(prs []nativeapi.MergeCandidate) string {
	numbers := make([]string, len(prs))
	for i, pr := range prs {
		numbers[i] = fmt.Sprintf("#%d", pr.Number)
	}
	return strings.Join(numbers, ", ")
}
