package cli

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/yasyf/cc-context/internal/gtapi"
	"github.com/yasyf/cc-context/internal/render"
)

const stackStrandedRemedy = "close and reopen each pull request on GitHub so Graphite rebuilds its stack record, then rerun ccx vcs stack submit"

var (
	stackTrackingWait  = 20 * time.Second
	stackTrackingRetry = 2 * time.Second
)

type stackUntracked struct {
	Branch string
	PR     int
	Parent string
	Stale  string
	Closed []int
}

func (u stackUntracked) String() string {
	fields := []string{fmt.Sprintf("#%d %s", u.PR, u.Branch), "parent " + u.Parent, "Graphite tracks no stack for it"}
	if u.Stale != "" {
		fields = append(fields, "its server-side parent is still "+u.Stale)
	}
	if len(u.Closed) > 0 {
		closed := make([]string, len(u.Closed))
		for i, n := range u.Closed {
			closed[i] = fmt.Sprintf("#%d", n)
		}
		fields = append(fields, "its server-side stack still holds "+strings.Join(closed, ", ")+", which already landed or closed")
	}
	return strings.Join(fields, shipSep)
}

func (u stackUntracked) repairable() bool { return len(u.Closed) == 0 }

type stackTracking struct {
	untracked []stackUntracked
	err       error
}

type stackTrackingKey struct{}

func withStackTracking(ctx context.Context, t *stackTracking) context.Context {
	return context.WithValue(ctx, stackTrackingKey{}, t)
}

func recordStackTracking(ctx context.Context, untracked []stackUntracked, err error) {
	if t, ok := ctx.Value(stackTrackingKey{}).(*stackTracking); ok {
		t.untracked, t.err = untracked, err
	}
}

func (t *stackTracking) repairable() []stackUntracked {
	return slices.DeleteFunc(slices.Clone(t.untracked), func(u stackUntracked) bool { return !u.repairable() })
}

func (t *stackTracking) stranded() []stackUntracked {
	return slices.DeleteFunc(slices.Clone(t.untracked), stackUntracked.repairable)
}

func stackUntrackedBranches(untracked []stackUntracked) []string {
	names := make([]string, len(untracked))
	for i, u := range untracked {
		names[i] = u.Branch
	}
	return names
}

func stackUntrackedLines(untracked []stackUntracked) string {
	lines := make([]string, len(untracked))
	for i, u := range untracked {
		lines[i] = u.String()
	}
	return strings.Join(lines, "\n")
}

func (t *stackTracking) unread() error {
	return fmt.Errorf("%s: pushed, but Graphite's tracking could not be read, so no pull request is reported mergeable: %w", stackRebasePrefix, t.err)
}

func stackTrackingStuck(untracked []stackUntracked) error {
	return fmt.Errorf("%s: Graphite still tracks no stack for these pull requests after republishing them with fresh heads, so the merge queue cannot enqueue them:\n%s", stackRebasePrefix, stackUntrackedLines(untracked))
}

func stackTrackingStranded(stranded []stackUntracked) error {
	return fmt.Errorf("%s: pushed, but Graphite tracks no stack for these pull requests because its stack record still holds pull requests that already landed or closed, so the merge queue cannot enqueue them; a fresh head does not clear that record, so ccx does not republish them — %s:\n%s", stackRebasePrefix, stackStrandedRemedy, stackUntrackedLines(stranded))
}

func (t *stackTracking) settled() error {
	switch stranded := t.stranded(); {
	case t.err != nil:
		return t.unread()
	case len(stranded) > 0:
		return stackTrackingStranded(stranded)
	case len(t.untracked) > 0:
		return fmt.Errorf("%s: pushed, but Graphite tracks no stack for these pull requests, so the merge queue cannot enqueue them — ccx vcs stack submit republishes them with fresh heads:\n%s", stackRebasePrefix, stackUntrackedLines(t.untracked))
	}
	return nil
}

func stackSettleTracking(cmd *cobra.Command, run func() error) error {
	tracking := &stackTracking{}
	cmd.SetContext(withStackTracking(cmd.Context(), tracking))
	if err := run(); err != nil {
		return err
	}
	return tracking.settled()
}

func stackReadTracking(ctx context.Context, l lane, run *stackRebaseRun, live []string, prs map[string]*stackPR) ([]stackUntracked, error) {
	var numbers []int
	branchOf := map[int]string{}
	for _, name := range live {
		if pr := prs[name]; pr != nil && pr.State == "OPEN" {
			numbers = append(numbers, pr.Number)
			branchOf[pr.Number] = name
		}
	}
	if len(numbers) == 0 {
		return nil, nil
	}
	owner, repo, err := gtRepoOwnerName(ctx, l, stackRebasePrefix)
	if err != nil {
		return nil, err
	}
	client := gtAPI(ctx)
	missing, err := stackAwaitTracking(ctx, client, owner, repo, numbers)
	if err != nil || len(missing) == 0 {
		return nil, err
	}
	untracked := make([]stackUntracked, 0, len(missing))
	for _, number := range missing {
		u, err := stackReadUntracked(ctx, client, owner, repo, number, branchOf[number], run)
		if err != nil {
			return nil, err
		}
		untracked = append(untracked, u)
	}
	return untracked, nil
}

func stackReadUntracked(ctx context.Context, client *gtapi.Client, owner, repo string, number int, branch string, run *stackRebaseRun) (stackUntracked, error) {
	infos, err := client.PullRequestInfo(ctx, gtapi.PullRequestInfoRequest{RepoOwner: owner, RepoName: repo, PRNumbers: []int{number}, Callsite: "ccx"})
	if err != nil {
		return stackUntracked{}, err
	}
	u := stackUntracked{Branch: branch, PR: number, Parent: run.branch(branch).Parent}
	var recorded string
	for _, info := range infos {
		switch {
		case info.PRNumber == number:
			recorded = info.Newest().BaseName
		case info.State != gtapi.PROpen:
			u.Closed = append(u.Closed, info.PRNumber)
		}
	}
	if recorded != u.Parent {
		u.Stale = recorded
	}
	slices.Sort(u.Closed)
	return u, nil
}

func stackAwaitTracking(ctx context.Context, client *gtapi.Client, owner, repo string, numbers []int) ([]int, error) {
	deadline := time.Now().Add(stackTrackingWait)
	for {
		statuses, err := client.MergeabilityStatuses(ctx, owner, repo, numbers)
		if err != nil {
			return nil, err
		}
		missing := slices.DeleteFunc(slices.Clone(numbers), func(n int) bool { _, tracked := statuses[n]; return tracked })
		if len(missing) == 0 || !time.Now().Add(stackTrackingRetry).Before(deadline) {
			return missing, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(stackTrackingRetry):
		}
	}
}

func stackBumpHead(ctx context.Context, dir render.Dir, run *stackRebaseRun, b *stackRebaseBranch) (string, error) {
	out, err := render.RunCLI(ctx, dir, "git", []string{"log", "-1", "--date=raw", "--format=%an%x00%ae%x00%ad%x00%cd%x00%P%x00%B", b.Head})
	if err != nil {
		return "", fmt.Errorf("%s: read %s's head: %w", stackRebasePrefix, b.Name, err)
	}
	fields := strings.SplitN(out, "\x00", 6)
	if len(fields) != 6 {
		return "", fmt.Errorf("%s: read %s's head %.12s: got %d fields, want 6", stackRebasePrefix, b.Name, b.Head, len(fields))
	}
	committed, _, _ := strings.Cut(fields[3], " ")
	was, err := strconv.ParseInt(committed, 10, 64)
	if err != nil {
		return "", fmt.Errorf("%s: read the committer date of %.12s: %w", stackRebasePrefix, b.Head, err)
	}
	argv := []string{"commit-tree", b.Head + "^{tree}"}
	for _, parent := range strings.Fields(fields[4]) {
		argv = append(argv, "-p", parent)
	}
	argv = append(argv, "-m", strings.TrimRight(fields[5], "\n"))
	head, err := render.RunCLIEnv(ctx, dir, "git", argv, []string{
		"GIT_AUTHOR_NAME=" + fields[0],
		"GIT_AUTHOR_EMAIL=" + fields[1],
		"GIT_AUTHOR_DATE=" + fields[2],
		"GIT_COMMITTER_DATE=" + strconv.FormatInt(max(time.Now().Unix(), was+1), 10) + " +0000",
	})
	if err != nil {
		return "", fmt.Errorf("%s: commit-tree a fresh head for %s: %w", stackRebasePrefix, b.Name, err)
	}
	head = strings.TrimSpace(head)
	pin := stackPublicationPin(run, b.Name)
	pinned, err := stackPinned(ctx, dir, pin, len(head))
	if err != nil {
		return "", err
	}
	return head, stackPinHead(ctx, dir, pin, head, pinned)
}
