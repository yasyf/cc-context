package nativeapi

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"time"
)

var (
	mergePoll = 2 * time.Second
	mergeWait = 10 * time.Minute
)

// MergeRequest names one pull request, by number or by head branch, and how to merge it.
type MergeRequest struct {
	RepoOwner string
	RepoName  string
	PRNumber  int
	Head      string
	Method    string
	Admin     bool
}

// MergeCandidate is one pull request a merge lands, with its head commit's checks.
type MergeCandidate struct {
	Number  int
	Head    string
	HeadSHA string
	// Rollup is the head commit's statusCheckRollup in GitHub's GraphQL
	// shape, null when nothing reported on it.
	Rollup json.RawMessage
}

// MergePlan is what one read of a pull request says merging it would land.
type MergePlan struct {
	Pull          MergeCandidate
	State         PRState
	MergeCommit   string
	DefaultBranch string
	Stacked       bool
	// Below is each open pull request under Pull in its native stack,
	// bottom-up, all of which the asynchronous merge lands with it.
	Below []MergeCandidate
}

// MergedPR is one pull request a merge landed or handed to the merge queue, with its merge commit once landed.
type MergedPR struct {
	Number int    `json:"number"`
	Head   string `json:"head"`
	Status string `json:"status"`
	SHA    string `json:"sha,omitempty"`
}

// ActionsRun is one GitHub Actions workflow run: the branch and event it ran
// for, its workflow, and the jobs of its latest attempt.
type ActionsRun struct {
	Branch     string
	Event      string
	WorkflowID int64
	Jobs       []ActionsJob
}

// ActionsJob is one job of a workflow run, with its conclusion once finished
// and its status before.
type ActionsJob struct {
	Name       string `json:"name"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
}

const mergeCandidateFields = "number state headRefName headRefOid commits(last:1){nodes{commit{statusCheckRollup{state contexts(last:100){totalCount nodes{__typename " +
	"...on CheckRun{name conclusion status startedAt databaseId checkSuite{databaseId workflowRun{workflow{databaseId}}}} ...on StatusContext{context state}}}}}}}"

type mergePull struct {
	Number      int
	State       PRState
	HeadRefName string
	HeadRefOid  string
	MergeCommit *struct{ Oid string }
	Commits     struct {
		Nodes []struct {
			Commit struct{ StatusCheckRollup json.RawMessage }
		}
	}
	StackEntry *struct{ Position int }
	Stack      *struct {
		Entries struct {
			Nodes    []mergeEntry
			PageInfo struct{ HasNextPage bool }
		}
	}
}

type mergeEntry struct {
	Position    int
	PullRequest *mergePull
}

func (p mergePull) candidate() MergeCandidate {
	out := MergeCandidate{Number: p.Number, Head: p.HeadRefName, HeadSHA: p.HeadRefOid}
	for _, node := range p.Commits.Nodes {
		out.Rollup = node.Commit.StatusCheckRollup
	}
	return out
}

type asyncMerge struct {
	Status  string `json:"status"`
	Details struct {
		Message string `json:"message"`
		UUID    string `json:"uuid"`
		SHA     string `json:"sha"`
	} `json:"details"`
}

// ViewerLogin reads the login of the account the client's token signs in as.
func (c *Client) ViewerLogin(ctx context.Context) (string, error) {
	payload, err := c.get(ctx, "/user", nil)
	if err != nil {
		return "", err
	}
	var user struct {
		Login string `json:"login"`
	}
	if err := json.Unmarshal(payload, &user); err != nil {
		return "", fmt.Errorf("parse the signed-in account: %w", err)
	}
	return user.Login, nil
}

// ReadMerge reads, in one GraphQL call, the pull request's state and head,
// the open pull requests below it in its native stack, and the checks on each
// one's head commit.
func (c *Client) ReadMerge(ctx context.Context, req MergeRequest) (MergePlan, error) {
	number, err := c.mergeTarget(ctx, fmt.Sprintf("/repos/%s/%s", url.PathEscape(req.RepoOwner), url.PathEscape(req.RepoName)), req)
	if err != nil {
		return MergePlan{}, err
	}
	query := fmt.Sprintf("{repository(owner:%s,name:%s){defaultBranchRef{name} pullRequest(number:%d){%s mergeCommit{oid} stackEntry{position} stack{entries(first:100){nodes{position pullRequest{%s}} pageInfo{hasNextPage}}}}}}",
		strconv.Quote(req.RepoOwner), strconv.Quote(req.RepoName), number, mergeCandidateFields, mergeCandidateFields)
	var result struct {
		Repository *struct {
			DefaultBranchRef struct{ Name string }
			PullRequest      *mergePull
		}
	}
	if err := c.graphql(ctx, query, &result); err != nil {
		return MergePlan{}, err
	}
	if result.Repository == nil || result.Repository.PullRequest == nil {
		return MergePlan{}, fmt.Errorf("%s/%s has no PR #%d this account can read", req.RepoOwner, req.RepoName, number)
	}
	pull := result.Repository.PullRequest
	plan := MergePlan{Pull: pull.candidate(), State: pull.State, DefaultBranch: result.Repository.DefaultBranchRef.Name, Stacked: pull.Stack != nil}
	if pull.MergeCommit != nil {
		plan.MergeCommit = pull.MergeCommit.Oid
	}
	if pull.Stack == nil {
		return plan, nil
	}
	if pull.Stack.Entries.PageInfo.HasNextPage {
		return MergePlan{}, fmt.Errorf("PR #%d's native stack exceeds the supported 100 pull requests", pull.Number)
	}
	below := slices.DeleteFunc(slices.Clone(pull.Stack.Entries.Nodes), func(e mergeEntry) bool {
		return e.PullRequest == nil || e.Position >= pull.StackEntry.Position || e.PullRequest.State != PROpen
	})
	slices.SortFunc(below, func(a, b mergeEntry) int { return cmp.Compare(a.Position, b.Position) })
	for _, e := range below {
		plan.Below = append(plan.Below, e.PullRequest.candidate())
	}
	return plan, nil
}

// ReadActionsRun reads one workflow run and the jobs of its latest attempt.
func (c *Client) ReadActionsRun(ctx context.Context, owner, repo string, id int64) (ActionsRun, error) {
	path := fmt.Sprintf("/repos/%s/%s/actions/runs/%d", url.PathEscape(owner), url.PathEscape(repo), id)
	payload, err := c.get(ctx, path, nil)
	if err != nil {
		return ActionsRun{}, fmt.Errorf("read run %d: %w", id, err)
	}
	var run struct {
		HeadBranch string `json:"head_branch"`
		Event      string `json:"event"`
		WorkflowID int64  `json:"workflow_id"`
	}
	if err := json.Unmarshal(payload, &run); err != nil {
		return ActionsRun{}, fmt.Errorf("parse run %d: %w", id, err)
	}
	if payload, err = c.get(ctx, path+"/jobs", url.Values{"filter": {"latest"}, "per_page": {"100"}}); err != nil {
		return ActionsRun{}, fmt.Errorf("read run %d's jobs: %w", id, err)
	}
	var jobs struct {
		Jobs []ActionsJob `json:"jobs"`
	}
	if err := json.Unmarshal(payload, &jobs); err != nil {
		return ActionsRun{}, fmt.Errorf("parse run %d's jobs: %w", id, err)
	}
	return ActionsRun{Branch: run.HeadBranch, Event: run.Event, WorkflowID: run.WorkflowID, Jobs: jobs.Jobs}, nil
}

// MergePullRequest merges the plan's pull request at the head the plan read
// and reads back each pull request it landed, bottom-up. A native stack member
// goes through the asynchronous merge API, which lands the open ones below it
// too, and Admin there bypasses the rules the caller may bypass; any other
// pull request goes through the plain merge API.
func (c *Client) MergePullRequest(ctx context.Context, req MergeRequest, plan MergePlan) ([]MergedPR, error) {
	repo := fmt.Sprintf("/repos/%s/%s", url.PathEscape(req.RepoOwner), url.PathEscape(req.RepoName))
	landing := append(slices.Clone(plan.Below), plan.Pull)
	if !plan.Stacked {
		if err := c.mergePlain(ctx, repo, plan.Pull, req.Method); err != nil {
			return nil, err
		}
		return c.confirmMerged(ctx, req, plan.Pull.Number, landing)
	}
	action := "default"
	if req.Admin {
		action = "direct_merge"
	}
	path := fmt.Sprintf("%s/pulls/%d/merge-async", repo, plan.Pull.Number)
	result, err := c.requestMerge(ctx, path, map[string]any{"sha": plan.Pull.HeadSHA, "merge_method": req.Method, "merge_action": action, "bypass_rules": req.Admin})
	if err != nil {
		return nil, fmt.Errorf("merge PR #%d: %w", plan.Pull.Number, err)
	}
	if result, err = c.settleMerge(ctx, path, plan.Pull.Number, result); err != nil {
		return nil, err
	}
	switch result.Status {
	case "merged":
		return c.confirmMerged(ctx, req, plan.Pull.Number, landing)
	case "enqueued":
		queued := make([]MergedPR, 0, len(landing))
		for _, pr := range landing {
			queued = append(queued, MergedPR{Number: pr.Number, Head: pr.Head, Status: "enqueued"})
		}
		return queued, nil
	case "failed":
		return nil, fmt.Errorf("merge PR #%d failed: %s", plan.Pull.Number, result.Details.Message)
	default:
		return nil, fmt.Errorf("merge PR #%d: GitHub answered status %q: %s", plan.Pull.Number, result.Status, result.Details.Message)
	}
}

func (c *Client) mergeTarget(ctx context.Context, repo string, req MergeRequest) (int, error) {
	if req.PRNumber != 0 {
		return req.PRNumber, nil
	}
	payload, err := c.get(ctx, repo+"/pulls", url.Values{"head": {req.RepoOwner + ":" + req.Head}, "state": {"all"}, "per_page": {"100"}})
	if err != nil {
		return 0, err
	}
	var pulls []restPull
	if err := json.Unmarshal(payload, &pulls); err != nil {
		return 0, err
	}
	open := slices.DeleteFunc(slices.Clone(pulls), func(p restPull) bool { return p.State != "open" })
	switch {
	case len(open) == 1:
		return open[0].Number, nil
	case len(open) > 1:
		return 0, fmt.Errorf("%s heads %d open pull requests; name one by number", req.Head, len(open))
	case len(pulls) > 0:
		return pulls[0].Number, nil
	}
	return 0, fmt.Errorf("%s heads no pull request in %s/%s", req.Head, req.RepoOwner, req.RepoName)
}

func (c *Client) mergePlain(ctx context.Context, repo string, pull MergeCandidate, method string) error {
	payload, err := c.send(ctx, http.MethodPut, fmt.Sprintf("%s/pulls/%d/merge", repo, pull.Number), map[string]any{"sha": pull.HeadSHA, "merge_method": method})
	if err != nil {
		return fmt.Errorf("merge PR #%d: %w", pull.Number, err)
	}
	var result struct {
		Merged  bool   `json:"merged"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(payload, &result); err != nil {
		return fmt.Errorf("merge PR #%d: %w", pull.Number, err)
	}
	if !result.Merged {
		return fmt.Errorf("merge PR #%d: %s", pull.Number, result.Message)
	}
	return nil
}

func (c *Client) confirmMerged(ctx context.Context, req MergeRequest, number int, landing []MergeCandidate) ([]MergedPR, error) {
	merged := make([]MergedPR, 0, len(landing))
	for _, pr := range landing {
		landed, err := c.readPull(ctx, req.RepoOwner, req.RepoName, pr.Number)
		if err != nil {
			return merged, err
		}
		if !landed.Merged {
			return merged, fmt.Errorf("GitHub merged PR #%d but reports #%d %s and unmerged", number, pr.Number, landed.State)
		}
		merged = append(merged, MergedPR{Number: pr.Number, Head: landed.Head.Ref, Status: "merged", SHA: landed.MergeCommitSHA})
	}
	return merged, nil
}

func (c *Client) requestMerge(ctx context.Context, path string, body map[string]any) (asyncMerge, error) {
	payload, err := c.send(ctx, http.MethodPut, path, body)
	var status *StatusError
	if errors.As(err, &status) && status.Status == http.StatusConflict {
		payload, err = status.Payload, nil
	}
	if err != nil {
		return asyncMerge{}, err
	}
	var result asyncMerge
	err = json.Unmarshal(payload, &result)
	return result, err
}

func (c *Client) settleMerge(ctx context.Context, path string, number int, result asyncMerge) (asyncMerge, error) {
	deadline := time.Now().Add(mergeWait)
	for result.Status == "pending" {
		if time.Now().After(deadline) {
			return result, fmt.Errorf("merge PR #%d is still pending after %s; GitHub keeps request %s for 24 hours, and rerunning the merge resumes it", number, mergeWait, result.Details.UUID)
		}
		select {
		case <-ctx.Done():
			return result, ctx.Err()
		case <-time.After(mergePoll):
		}
		payload, err := c.get(ctx, path+"/"+url.PathEscape(result.Details.UUID), nil)
		if err != nil {
			return result, fmt.Errorf("read merge request %s for PR #%d: %w", result.Details.UUID, number, err)
		}
		uuid := result.Details.UUID
		result = asyncMerge{}
		if err := json.Unmarshal(payload, &result); err != nil {
			return result, err
		}
		if result.Details.UUID == "" {
			result.Details.UUID = uuid
		}
	}
	return result, nil
}
