package nativeapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
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

// MergedPR is one pull request a merge landed or handed to the merge queue, with its merge commit once landed.
type MergedPR struct {
	Number int    `json:"number"`
	Head   string `json:"head"`
	Status string `json:"status"`
	SHA    string `json:"sha,omitempty"`
}

type asyncMerge struct {
	Status  string `json:"status"`
	Details struct {
		Message string `json:"message"`
		UUID    string `json:"uuid"`
		SHA     string `json:"sha"`
	} `json:"details"`
}

// MergePullRequest merges through GitHub's asynchronous merge API, the one a
// native stacked pull request requires. A stacked pull request merges with
// every open pull request below it, reported bottom-up; Admin merges directly,
// bypassing the rules the caller may bypass, where the default lets GitHub's
// merge queue take it when the base branch has one.
func (c *Client) MergePullRequest(ctx context.Context, req MergeRequest) ([]MergedPR, error) {
	repo := fmt.Sprintf("/repos/%s/%s", url.PathEscape(req.RepoOwner), url.PathEscape(req.RepoName))
	number, err := c.mergeTarget(ctx, repo, req)
	if err != nil {
		return nil, err
	}
	pull, err := c.readPull(ctx, req.RepoOwner, req.RepoName, number)
	if err != nil {
		return nil, err
	}
	if pull.Merged {
		return []MergedPR{{Number: pull.Number, Head: pull.Head.Ref, Status: "merged", SHA: pull.MergeCommitSHA}}, nil
	}
	if pull.State != "open" {
		return nil, fmt.Errorf("PR #%d is %s", pull.Number, pull.State)
	}
	members := []int{pull.Number}
	if pull.Stack != nil {
		if members, err = c.downstack(ctx, repo, pull); err != nil {
			return nil, err
		}
	}
	action := "default"
	if req.Admin {
		action = "direct_merge"
	}
	path := fmt.Sprintf("%s/pulls/%d/merge-async", repo, pull.Number)
	result, err := c.requestMerge(ctx, path, map[string]any{"sha": pull.Head.Sha, "merge_method": req.Method, "merge_action": action, "bypass_rules": req.Admin})
	if err != nil {
		return nil, fmt.Errorf("merge PR #%d: %w", pull.Number, err)
	}
	if result, err = c.settleMerge(ctx, path, pull.Number, result); err != nil {
		return nil, err
	}
	switch result.Status {
	case "merged":
	case "enqueued":
		queued := make([]MergedPR, 0, len(members))
		for _, n := range members {
			queued = append(queued, MergedPR{Number: n, Status: "enqueued"})
		}
		queued[len(queued)-1].Head = pull.Head.Ref
		return queued, nil
	case "failed":
		return nil, fmt.Errorf("merge PR #%d failed: %s", pull.Number, result.Details.Message)
	default:
		return nil, fmt.Errorf("merge PR #%d: GitHub answered status %q: %s", pull.Number, result.Status, result.Details.Message)
	}
	if len(members) == 1 {
		return []MergedPR{{Number: pull.Number, Head: pull.Head.Ref, Status: "merged", SHA: result.Details.SHA}}, nil
	}
	merged := make([]MergedPR, 0, len(members))
	for _, n := range members {
		landed, err := c.readPull(ctx, req.RepoOwner, req.RepoName, n)
		if err != nil {
			return merged, err
		}
		if !landed.Merged {
			return merged, fmt.Errorf("GitHub merged PR #%d but reports #%d below it %s and unmerged", pull.Number, n, landed.State)
		}
		merged = append(merged, MergedPR{Number: n, Head: landed.Head.Ref, Status: "merged", SHA: landed.MergeCommitSHA})
	}
	return merged, nil
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

func (c *Client) downstack(ctx context.Context, repo string, pull restPull) ([]int, error) {
	payload, err := c.get(ctx, fmt.Sprintf("%s/stacks/%d", repo, pull.Stack.Number), nil)
	if err != nil {
		return nil, err
	}
	var stack nativeStack
	if err := json.Unmarshal(payload, &stack); err != nil {
		return nil, err
	}
	at := slices.Index(stack.numbers(), pull.Number)
	if at < 0 {
		return nil, fmt.Errorf("native GitHub stack #%d does not list PR #%d, which names it as its stack", pull.Stack.Number, pull.Number)
	}
	var open []int
	for _, pr := range stack.PullRequests[:at+1] {
		if pr.State != "closed" {
			open = append(open, pr.Number)
		}
	}
	return open, nil
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
