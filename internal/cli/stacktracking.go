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

var (
	stackTrackingWait  = 20 * time.Second
	stackTrackingRetry = 2 * time.Second
)

type stackUntracked struct {
	Branch string
	PR     int
	Parent string
	Stale  string
}

func (u stackUntracked) String() string {
	fields := []string{fmt.Sprintf("#%d %s", u.PR, u.Branch), "parent " + u.Parent, "Graphite tracks no stack for it"}
	if u.Stale != "" {
		fields = append(fields, "its server-side parent is still "+u.Stale)
	}
	return strings.Join(fields, shipSep)
}

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

func (t *stackTracking) branches() []string {
	names := make([]string, len(t.untracked))
	for i, u := range t.untracked {
		names[i] = u.Branch
	}
	return names
}

func (t *stackTracking) lines() string {
	lines := make([]string, len(t.untracked))
	for i, u := range t.untracked {
		lines[i] = u.String()
	}
	return strings.Join(lines, "\n")
}

func (t *stackTracking) unread() error {
	return fmt.Errorf("%s: pushed, but Graphite's tracking could not be read, so no pull request is reported mergeable: %w", stackRebasePrefix, t.err)
}

func (t *stackTracking) stuck() error {
	return fmt.Errorf("%s: Graphite still tracks no stack for these pull requests after republishing them with fresh heads, so the merge queue cannot enqueue them:\n%s", stackRebasePrefix, t.lines())
}

func (t *stackTracking) settled() error {
	switch {
	case t.err != nil:
		return t.unread()
	case len(t.untracked) > 0:
		return fmt.Errorf("%s: pushed, but Graphite tracks no stack for these pull requests, so the merge queue cannot enqueue them — ccx vcs stack submit republishes them with fresh heads:\n%s", stackRebasePrefix, t.lines())
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
	infos, err := client.PullRequestInfo(ctx, gtapi.PullRequestInfoRequest{RepoOwner: owner, RepoName: repo, PRNumbers: missing, Callsite: "ccx"})
	if err != nil {
		return nil, err
	}
	recorded := map[int]string{}
	for _, info := range infos {
		recorded[info.PRNumber] = info.Newest().BaseName
	}
	untracked := make([]stackUntracked, 0, len(missing))
	for _, number := range missing {
		u := stackUntracked{Branch: branchOf[number], PR: number}
		u.Parent = run.branch(u.Branch).Parent
		if base := recorded[number]; base != u.Parent {
			u.Stale = base
		}
		untracked = append(untracked, u)
	}
	return untracked, nil
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
