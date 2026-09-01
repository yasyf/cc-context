package gtapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
)

// CheckAuthResponse answers whether the token authenticates and can submit
// PRs against the named repo.
type CheckAuthResponse struct {
	GithubLogin  string `json:"githubLogin"`
	CanSubmitPrs bool   `json:"canSubmitPrs"`
}

// CheckAuth verifies the token against /graphite/check-auth.
func (c *Client) CheckAuth(ctx context.Context, repoOwner, repoName string) (CheckAuthResponse, error) {
	params := struct {
		RepoOwner string `json:"repoOwner"`
		RepoName  string `json:"repoName"`
	}{repoOwner, repoName}
	payload, err := c.post(ctx, "/graphite/check-auth", params)
	if err != nil {
		return CheckAuthResponse{}, err
	}
	var resp CheckAuthResponse
	if err := json.Unmarshal(payload, &resp); err != nil {
		return CheckAuthResponse{}, fmt.Errorf("gtapi: decode check-auth: %w", err)
	}
	return resp, nil
}

// RepoSyncStatus is Graphite's answer to whether it mirrors a repo.
type RepoSyncStatus string

// The statuses Graphite reports for a repository it was asked about.
const (
	RepoSynced             RepoSyncStatus = "SYNCED"
	RepoNotSyncedAddable   RepoSyncStatus = "NOT_SYNCED_ADDABLE"
	RepoNotSyncedUnaddable RepoSyncStatus = "NOT_SYNCED_UNADDABLE"
)

// RepoSync carries the sync status; Message is set only on
// RepoNotSyncedUnaddable.
type RepoSync struct {
	Status  RepoSyncStatus `json:"status"`
	Message string         `json:"message"`
}

// IsRepoSynced asks whether Graphite mirrors repoOwner/repoName.
func (c *Client) IsRepoSynced(ctx context.Context, repoOwner, repoName string) (RepoSync, error) {
	query := url.Values{"repoOwner": {repoOwner}, "repoName": {repoName}}
	payload, err := c.get(ctx, "/graphite/cli/is-repo-synced", query)
	if err != nil {
		return RepoSync{}, err
	}
	return unwrapResult[RepoSync](payload, "is-repo-synced")
}

// PullRequestInfoRequest names the PRs and branches to look up.
type PullRequestInfoRequest struct {
	RepoOwner        string   `json:"repoOwner"`
	RepoName         string   `json:"repoName"`
	PRNumbers        []int    `json:"prNumbers"`
	PRHeadRefNames   []string `json:"prHeadRefNames,omitempty"`
	TrunkBranchNames []string `json:"trunkBranchNames,omitempty"`
	Consistent       bool     `json:"consistent"`
	Callsite         string   `json:"callsite,omitempty"`
}

// PRState is a pull request's lifecycle state.
type PRState string

// The lifecycle states a pull request is reported in.
const (
	PROpen   PRState = "OPEN"
	PRClosed PRState = "CLOSED"
	PRMerged PRState = "MERGED"
)

// MergeQueueStatus reports a PR's position in the Graphite merge queue.
type MergeQueueStatus struct {
	IsInGraphiteMq bool   `json:"isInGraphiteMq"`
	EnqueuedCommit string `json:"enqueuedCommit"`
}

// PRVersion is one submitted version of a pull request.
type PRVersion struct {
	HeadSha             string `json:"headSha"`
	BaseSha             string `json:"baseSha"`
	BaseName            string `json:"baseName"`
	CreatedAt           string `json:"createdAt"`
	AuthorGithubHandle  string `json:"authorGithubHandle"`
	IsGraphiteGenerated bool   `json:"isGraphiteGenerated"`
}

// PullRequestInfo is Graphite's view of one pull request.
type PullRequestInfo struct {
	PRNumber              int               `json:"prNumber"`
	Title                 string            `json:"title"`
	AuthorGithubHandle    string            `json:"authorGithubHandle"`
	Body                  string            `json:"body"`
	State                 PRState           `json:"state"`
	ReviewDecision        string            `json:"reviewDecision"`
	HeadRefName           string            `json:"headRefName"`
	BaseRefName           string            `json:"baseRefName"`
	IsBaseRefGraphiteBase bool              `json:"isBaseRefGraphiteBase"`
	MergeQueueStatus      *MergeQueueStatus `json:"mergeQueueStatus"`
	URL                   string            `json:"url"`
	IsDraft               bool              `json:"isDraft"`
	Versions              []PRVersion       `json:"versions"`
	DependentPRNumber     int               `json:"dependentPrNumber"`
	MergeCommitSha        string            `json:"mergeCommitSha"`
	CliTrunk              string            `json:"cliTrunk"`
}

// PullRequestInfo fetches Graphite's record of the named pull requests.
func (c *Client) PullRequestInfo(ctx context.Context, req PullRequestInfoRequest) ([]PullRequestInfo, error) {
	payload, err := c.post(ctx, "/graphite/cli/pull-request-info", req)
	if err != nil {
		return nil, err
	}
	result, err := unwrapResult[struct {
		Status  string            `json:"status"`
		Prs     []PullRequestInfo `json:"prs"`
		Message string            `json:"message"`
	}](payload, "pull-request-info")
	if err != nil {
		return nil, err
	}
	if result.Status != "ok" {
		return nil, &ResultError{Route: "pull-request-info", Message: result.Message}
	}
	return result.Prs, nil
}

// PreSubmitBranch names one branch of an upcoming submit; PRNumber is zero for
// a branch with no PR yet.
type PreSubmitBranch struct {
	HeadRefName string `json:"headRefName"`
	PRNumber    int    `json:"prNumber,omitempty"`
}

// PreSubmitPullRequests announces an upcoming submit and returns the PR
// numbers Graphite retargeted in response.
func (c *Client) PreSubmitPullRequests(ctx context.Context, repoOwner, repoName string, branches []PreSubmitBranch) ([]int, error) {
	params := struct {
		RepoOwner string            `json:"repoOwner"`
		RepoName  string            `json:"repoName"`
		Branches  []PreSubmitBranch `json:"branches"`
	}{repoOwner, repoName, branches}
	payload, err := c.post(ctx, "/graphite/cli/submit/pre-submit-pull-requests", params)
	if err != nil {
		return nil, err
	}
	result, err := unwrapResult[struct {
		RetargetedPrs []int  `json:"retargetedPrs"`
		Error         string `json:"error"`
	}](payload, "pre-submit-pull-requests")
	if err != nil {
		return nil, err
	}
	if result.Error != "" {
		return nil, &ResultError{Route: "pre-submit-pull-requests", Message: result.Error}
	}
	return result.RetargetedPrs, nil
}

// SubmitAction distinguishes creating a PR from updating one.
type SubmitAction string

// The two actions a submit entry carries.
const (
	SubmitCreate SubmitAction = "create"
	SubmitUpdate SubmitAction = "update"
)

// SubmitPR is one branch of a submit. Create takes Head, HeadSha, Base,
// BaseSha, and Title; update takes PRNumber as well and its Title and Body are
// optional. HeadSha must already be pushed: gt force-pushes every branch
// before this call. Draft is a pointer because an explicit false publishes a
// draft PR on update, which an omitted field leaves alone.
type SubmitPR struct {
	Action           SubmitAction `json:"action"`
	Head             string       `json:"head"`
	HeadSha          string       `json:"headSha"`
	Base             string       `json:"base"`
	BaseSha          string       `json:"baseSha"`
	PRNumber         int          `json:"prNumber,omitempty"`
	Title            string       `json:"title,omitempty"`
	Body             string       `json:"body,omitempty"`
	Draft            *bool        `json:"draft,omitempty"`
	Reviewers        []string     `json:"reviewers,omitempty"`
	ShouldRetarget   bool         `json:"shouldRetarget,omitempty"`
	RebaseOnlyChange bool         `json:"rebaseOnlyChange,omitempty"`
	MaintainRetarget bool         `json:"maintainRetarget,omitempty"`
}

// SubmitRequest opens and updates the named PRs in one call.
type SubmitRequest struct {
	RepoOwner             string     `json:"repoOwner"`
	RepoName              string     `json:"repoName"`
	TrunkBranchName       string     `json:"trunkBranchName"`
	TargetTrunkBranchName string     `json:"targetTrunkBranchName,omitempty"`
	MergeWhenReady        bool       `json:"mergeWhenReady"`
	RerequestReview       bool       `json:"rerequestReview"`
	PRs                   []SubmitPR `json:"prs"`
}

// SubmittedPR is one branch a submit landed.
type SubmittedPR struct {
	Head     string   `json:"head"`
	PRNumber int      `json:"prNumber"`
	PRURL    string   `json:"prURL"`
	Status   string   `json:"status"`
	Warnings []string `json:"warnings"`
}

// SubmitPullRequests posts the submit. Failure is per branch: when any entry
// comes back status error the call returns *SubmitError carrying both the
// branches that landed and the ones that did not.
func (c *Client) SubmitPullRequests(ctx context.Context, req SubmitRequest) ([]SubmittedPR, error) {
	payload, err := c.post(ctx, "/graphite/submit/pull-requests", req)
	if err != nil {
		return nil, err
	}
	var resp struct {
		Prs []struct {
			SubmittedPR
			Error string `json:"error"`
		} `json:"prs"`
	}
	if err := json.Unmarshal(payload, &resp); err != nil {
		return nil, fmt.Errorf("gtapi: decode submit/pull-requests: %w", err)
	}
	submitted := make([]SubmittedPR, 0, len(resp.Prs))
	var failed []BranchSubmitError
	for _, pr := range resp.Prs {
		if pr.Status == "error" {
			failed = append(failed, BranchSubmitError{Head: pr.Head, Message: pr.Error})
			continue
		}
		submitted = append(submitted, pr.SubmittedPR)
	}
	if len(failed) > 0 {
		return nil, &SubmitError{Submitted: submitted, Failed: failed}
	}
	return submitted, nil
}
