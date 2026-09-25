package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/yasyf/cc-context/internal/gtapi"
	"github.com/yasyf/cc-context/internal/render"
	"github.com/yasyf/cc-context/internal/vcs"
)

// prQueueState is where a pull request stands against the Graphite merge queue.
type prQueueState string

const (
	prQueueQueued    prQueueState = "queued"
	prQueueNotQueued prQueueState = "not queued"
	prQueueLanded    prQueueState = "landed"
)

// prQueueReport is one pull request's queue state, with the commits that
// decided it: the one the queue admitted, and the squash it wrote on the base.
type prQueueReport struct {
	Number   int          `json:"number"`
	Queue    prQueueState `json:"queue"`
	State    string       `json:"state"`
	Base     string       `json:"base"`
	Enqueued string       `json:"enqueued,omitempty"`
	Squash   string       `json:"squash,omitempty"`
}

type prCommitCandidate struct {
	number int
	base   string
	sha    string
}

type prComparisonGroup struct {
	base       string
	candidates []prCommitCandidate
}

var prCommitsOnBase = func(ctx context.Context, repo string, candidates []prCommitCandidate) (map[int]bool, error) {
	owner, name, _ := strings.Cut(repo, "/")
	groups := make([]prComparisonGroup, 0)
	groupIndex := make(map[string]int)
	for _, candidate := range candidates {
		index, ok := groupIndex[candidate.base]
		if !ok {
			index = len(groups)
			groupIndex[candidate.base] = index
			groups = append(groups, prComparisonGroup{base: candidate.base})
		}
		groups[index].candidates = append(groups[index].candidates, candidate)
	}
	argv := []string{"api", "graphql", "-f", "owner=" + owner, "-f", "repo=" + name}
	for i, group := range groups {
		argv = append(argv, "-f", fmt.Sprintf("b%d=refs/heads/%s", i, group.base))
		for j, candidate := range group.candidates {
			argv = append(argv, "-f", fmt.Sprintf("s%d_%d=%s", i, j, candidate.sha))
		}
	}
	argv = append(argv, "-f", "query="+prCompareQuery(groups))
	out, err := render.RunCLI(ctx, render.Ambient, "gh", argv)
	if err != nil {
		return nil, fmt.Errorf("pr status: gh api graphql: %w", err)
	}
	var resp struct {
		Data struct {
			Repository map[string]map[string]*struct {
				Status string `json:"status"`
			} `json:"repository"`
		} `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal([]byte(out), &resp); err != nil {
		return nil, fmt.Errorf("pr status: parse gh api graphql: %w", err)
	}
	if len(resp.Errors) > 0 {
		return nil, fmt.Errorf("pr status: gh api graphql: %s", resp.Errors[0].Message)
	}
	onBase := make(map[int]bool, len(candidates))
	for i, group := range groups {
		ref := resp.Data.Repository[fmt.Sprintf("b%d", i)]
		if ref == nil {
			return nil, fmt.Errorf("pr status: base branch %q not found", group.base)
		}
		for j, candidate := range group.candidates {
			comparison := ref[fmt.Sprintf("c%d", j)]
			if comparison == nil {
				return nil, fmt.Errorf("pr status: compare %s with %s: no result", candidate.sha, group.base)
			}
			switch comparison.Status {
			case "BEHIND", "IDENTICAL":
				onBase[candidate.number] = true
			case "AHEAD", "DIVERGED":
				onBase[candidate.number] = false
			default:
				return nil, fmt.Errorf("pr status: compare %s with %s: unknown status %q", candidate.sha, group.base, comparison.Status)
			}
		}
	}
	return onBase, nil
}

func prCompareQuery(groups []prComparisonGroup) string {
	decls := []string{"$owner: String!", "$repo: String!"}
	var fields strings.Builder
	for i, group := range groups {
		decls = append(decls, fmt.Sprintf("$b%d: String!", i))
		fmt.Fprintf(&fields, "    b%d: ref(qualifiedName: $b%d) {\n", i, i)
		for j := range group.candidates {
			decls = append(decls, fmt.Sprintf("$s%d_%d: String!", i, j))
			fmt.Fprintf(&fields, "      c%d: compare(headRef: $s%d_%d) { status }\n", j, i, j)
		}
		fields.WriteString("    }\n")
	}
	return fmt.Sprintf("query(%s) {\n  repository(owner: $owner, name: $repo) {\n%s  }\n}", strings.Join(decls, ", "), fields.String())
}

type vcsPRStatusOpts struct {
	json bool
	repo string
}

func newVcsPRCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "pr",
		Short: "Answer questions about pull requests by number",
		Args:  cobra.NoArgs,
		RunE:  groupHelp,
	}
	cmd.AddCommand(newVcsPRStatusCmd())
	return cmd
}

func newVcsPRStatusCmd() *cobra.Command {
	var o vcsPRStatusOpts
	cmd := &cobra.Command{
		Use:   "status <number>...",
		Short: "Report whether each pull request is queued, not queued, or landed",
		Long: `Report whether each pull request is queued, not queued, or landed.

Pass all pull request numbers in one invocation. The command fetches their
statuses together and prints one result per pull request in input order.

The answer comes from Graphite's own record of the pull request, the one gt
reads, so a pull request enqueued from the Graphite web UI reads queued even
though it carries no merge label. A merge label is not the answer either way:
the queue consumes it on admission, and a label left on a pull request the
queue dropped means nothing.

Landed means the squash commit Graphite recorded is reachable from the base
branch on GitHub. The queue closes what it lands, so a landed pull request
reads CLOSED with a null mergedAt on GitHub; the squash is what settles it.

A queued pull request names the commit the queue admitted. A push after
admission does not move it: the queue lands that commit and drops the rest.`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runVcsPRStatus(cmd, args, o)
		},
	}
	cmd.Flags().BoolVar(&o.json, "json", false, "emit the report as JSON")
	cmd.Flags().StringVarP(&o.repo, "repo", "R", "", "the owner/name repository (default: the current checkout's)")
	return cmd
}

func runVcsPRStatus(cmd *cobra.Command, args []string, o vcsPRStatusOpts) error {
	ctx := cmd.Context()
	numbers := make([]int, 0, len(args))
	for _, arg := range args {
		n, err := strconv.Atoi(strings.TrimPrefix(arg, "#"))
		if err != nil || n <= 0 {
			return fmt.Errorf("pr status: %q is not a pull request number", arg)
		}
		numbers = append(numbers, n)
	}
	repo := o.repo
	if repo == "" {
		looked, err := vcs.LookupRepo(ctx, render.Dir(workingDir(ctx)), false)
		if err != nil {
			return fmt.Errorf("pr status: name the repository with --repo: %w", err)
		}
		repo = looked.NameWithOwner
	}
	reports, err := collectPRQueue(ctx, repo, numbers)
	if err != nil {
		return err
	}
	if o.json {
		data, err := json.MarshalIndent(reports, "", "  ")
		if err != nil {
			return fmt.Errorf("pr status: marshal report: %w", err)
		}
		cmd.Println(string(data))
		return nil
	}
	cmd.Print(renderPRQueue(reports))
	return nil
}

func collectPRQueue(ctx context.Context, repo string, numbers []int) ([]prQueueReport, error) {
	owner, name, ok := strings.Cut(repo, "/")
	if !ok {
		return nil, fmt.Errorf("pr status: malformed repository name %q", repo)
	}
	infos, err := gtAPIClient().PullRequestInfo(ctx, gtapi.PullRequestInfoRequest{
		RepoOwner:  owner,
		RepoName:   name,
		PRNumbers:  numbers,
		Consistent: true,
		Callsite:   "ccx",
	})
	if err != nil {
		return nil, fmt.Errorf("pr status: graphite: %w", err)
	}
	byNumber := make(map[int]gtapi.PullRequestInfo, len(infos))
	for _, info := range infos {
		byNumber[info.PRNumber] = info
	}
	candidates := make([]prCommitCandidate, 0, len(numbers))
	for _, number := range numbers {
		info, ok := byNumber[number]
		if !ok {
			return nil, fmt.Errorf("pr status: graphite has no record of %s#%d", repo, number)
		}
		if info.MergeCommitSha != "" {
			candidates = append(candidates, prCommitCandidate{number: number, base: info.BaseRefName, sha: info.MergeCommitSha})
		}
	}
	onBase := map[int]bool{}
	if len(candidates) > 0 {
		onBase, err = prCommitsOnBase(ctx, repo, candidates)
		if err != nil {
			return nil, err
		}
	}
	reports := make([]prQueueReport, 0, len(numbers))
	for _, number := range numbers {
		info := byNumber[number]
		reports = append(reports, classifyPRQueue(info, onBase[number]))
	}
	return reports, nil
}

// classifyPRQueue settles one pull request's queue state. Landed is checked
// first because Graphite keeps isInGraphiteMq set on a pull request it has
// already merged; queued needs the pull request still open for the same reason.
func classifyPRQueue(info gtapi.PullRequestInfo, squashOnBase bool) prQueueReport {
	r := prQueueReport{Number: info.PRNumber, Queue: prQueueNotQueued, State: string(info.State), Base: info.BaseRefName}
	switch {
	case squashOnBase:
		r.Queue = prQueueLanded
		r.Squash = info.MergeCommitSha
	case info.State == gtapi.PROpen && info.MergeQueueStatus != nil && info.MergeQueueStatus.IsInGraphiteMq:
		r.Queue = prQueueQueued
		r.Enqueued = info.MergeQueueStatus.EnqueuedCommit
	}
	return r
}

func renderPRQueue(reports []prQueueReport) string {
	var b strings.Builder
	for _, r := range reports {
		fmt.Fprintf(&b, "#%d  %s", r.Number, r.Queue)
		switch r.Queue {
		case prQueueLanded:
			fmt.Fprintf(&b, " · squash %s on %s", shortSHA(r.Squash), r.Base)
		case prQueueQueued:
			fmt.Fprintf(&b, " · enqueued %s into %s", shortSHA(r.Enqueued), r.Base)
		case prQueueNotQueued:
			fmt.Fprintf(&b, " · %s", strings.ToLower(r.State))
		}
		b.WriteString("\n")
	}
	return b.String()
}
