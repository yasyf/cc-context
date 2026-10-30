package nativeapi

import (
	"encoding/json"
	"strings"
)

type PullRequestInfoRequest struct {
	RepoOwner        string   `json:"repoOwner"`
	RepoName         string   `json:"repoName"`
	PRNumbers        []int    `json:"prNumbers"`
	PRHeadRefNames   []string `json:"prHeadRefNames,omitempty"`
	TrunkBranchNames []string `json:"trunkBranchNames,omitempty"`
	Consistent       bool     `json:"consistent"`
	Callsite         string   `json:"callsite,omitempty"`
}

func (r PullRequestInfoRequest) MarshalJSON() ([]byte, error) {
	type wire PullRequestInfoRequest
	w := wire(r)
	if w.PRNumbers == nil {
		w.PRNumbers = []int{}
	}
	return json.Marshal(w)
}

type PRState string

const (
	PROpen   PRState = "OPEN"
	PRClosed PRState = "CLOSED"
	PRMerged PRState = "MERGED"
)

type MergeQueueStatus struct {
	InQueue        bool   `json:"inQueue"`
	EnqueuedCommit string `json:"enqueuedCommit"`
}

const (
	MergeabilityQueued          = "testing"
	MergeabilityWaiting         = "pending"
	MergeabilityFailureHandling = "pendingFailure"
)

const MergeabilityRebasing = "rebasing"

func QueuedMergeability(status string) bool {
	switch status {
	case "notReady", MergeabilityQueued, MergeabilityWaiting, "testsPassed", MergeabilityFailureHandling:
		return true
	}
	return strings.HasPrefix(status, "queued:")
}

type PRVersion struct {
	HeadSha            string `json:"headSha"`
	BaseSha            string `json:"baseSha"`
	BaseName           string `json:"baseName"`
	CreatedAt          string `json:"createdAt"`
	AuthorGithubHandle string `json:"authorGithubHandle"`
}

type PullRequestInfo struct {
	PRID               string            `json:"prId,omitempty"`
	StackNumber        int               `json:"stackNumber"`
	PRNumber           int               `json:"prNumber"`
	Title              string            `json:"title"`
	AuthorGithubHandle string            `json:"authorGithubHandle"`
	Body               string            `json:"body"`
	State              PRState           `json:"state"`
	ReviewDecision     string            `json:"reviewDecision"`
	HeadRefName        string            `json:"headRefName"`
	BaseRefName        string            `json:"baseRefName"`
	MergeQueueStatus   *MergeQueueStatus `json:"mergeQueueStatus"`
	URL                string            `json:"url"`
	IsDraft            bool              `json:"isDraft"`
	Versions           []PRVersion       `json:"versions"`
	DependentPRNumber  int               `json:"dependentPrNumber"`
	MergeCommitSha     string            `json:"mergeCommitSha"`
	CliTrunk           string            `json:"cliTrunk"`
}

func (p PullRequestInfo) Newest() PRVersion {
	newest := PRVersion{}
	for _, v := range p.Versions {
		if v.CreatedAt >= newest.CreatedAt {
			newest = v
		}
	}
	return newest
}

type SubmitAction string

const (
	SubmitCreate SubmitAction = "create"
	SubmitUpdate SubmitAction = "update"
)

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

type SubmitRequest struct {
	RepoOwner             string     `json:"repoOwner"`
	RepoName              string     `json:"repoName"`
	TrunkBranchName       string     `json:"trunkBranchName"`
	TargetTrunkBranchName string     `json:"targetTrunkBranchName,omitempty"`
	MergeWhenReady        bool       `json:"mergeWhenReady"`
	RerequestReview       bool       `json:"rerequestReview"`
	PRs                   []SubmitPR `json:"prs"`
}

type SubmittedPR struct {
	PRID        string   `json:"prId"`
	StackNumber int      `json:"stackNumber"`
	Head        string   `json:"head"`
	PRNumber    int      `json:"prNumber"`
	PRURL       string   `json:"prURL"`
	Status      string   `json:"status"`
	Warnings    []string `json:"warnings"`
}
