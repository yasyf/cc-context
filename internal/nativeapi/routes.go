package nativeapi

import "strings"

// PullRequestInfoRequest identifies the repository and PR numbers or head branches to inspect.
type PullRequestInfoRequest struct {
	RepoOwner        string   `json:"repoOwner"`
	RepoName         string   `json:"repoName"`
	PRNumbers        []int    `json:"prNumbers"`
	PRHeadRefNames   []string `json:"prHeadRefNames,omitempty"`
	TrunkBranchNames []string `json:"trunkBranchNames,omitempty"`
	Consistent       bool     `json:"consistent"`
	Callsite         string   `json:"callsite,omitempty"`
}

// PRState preserves the lifecycle state GitHub reports for a pull request.
type PRState string

const (
	// PROpen identifies a pull request that remains available for review.
	PROpen PRState = "OPEN"
	// PRClosed identifies a pull request closed without a merge.
	PRClosed PRState = "CLOSED"
	// PRMerged identifies a pull request GitHub records as merged.
	PRMerged PRState = "MERGED"
)

// MergeQueueStatus records admission and the commit held by the selected queue.
type MergeQueueStatus struct {
	InQueue        bool   `json:"inQueue"`
	EnqueuedCommit string `json:"enqueuedCommit"`
}

const (
	// MergeabilityQueued identifies an active verification run.
	MergeabilityQueued = "testing"
	// MergeabilityWaiting identifies an admitted PR awaiting a testing slot.
	MergeabilityWaiting = "pending"
	// MergeabilityFailureHandling identifies a failed dependency awaiting verification.
	MergeabilityFailureHandling = "pendingFailure"
)

// MergeabilityRebasing identifies a parent replay that may move the published head.
const MergeabilityRebasing = "rebasing"

// QueuedMergeability recognizes active states, including newly introduced queued states.
func QueuedMergeability(status string) bool {
	switch status {
	case "notReady", MergeabilityQueued, MergeabilityWaiting, "testsPassed", MergeabilityFailureHandling:
		return true
	}
	return strings.HasPrefix(status, "queued:")
}

// PRVersion records the head and target observed in one GitHub metadata read.
type PRVersion struct {
	HeadSha            string `json:"headSha"`
	BaseSha            string `json:"baseSha"`
	BaseName           string `json:"baseName"`
	CreatedAt          string `json:"createdAt"`
	AuthorGithubHandle string `json:"authorGithubHandle"`
}

// PullRequestInfo combines review state, native stack membership, and queue evidence.
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

// SubmitAction distinguishes opening a PR from changing its existing metadata.
type SubmitAction string

const (
	// SubmitCreate opens a PR from an already published branch.
	SubmitCreate SubmitAction = "create"
	// SubmitUpdate changes an existing PR while preserving fields not restated.
	SubmitUpdate SubmitAction = "update"
)

// SubmitPR describes a published branch and the PR metadata its submission may change.
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

// SubmitRequest groups branch submissions under one repository and stack trunk.
type SubmitRequest struct {
	RepoOwner             string     `json:"repoOwner"`
	RepoName              string     `json:"repoName"`
	TrunkBranchName       string     `json:"trunkBranchName"`
	TargetTrunkBranchName string     `json:"targetTrunkBranchName,omitempty"`
	MergeWhenReady        bool       `json:"mergeWhenReady"`
	RerequestReview       bool       `json:"rerequestReview"`
	PRs                   []SubmitPR `json:"prs"`
}

// SubmittedPR records an actual PR identity and its native stack after submission.
type SubmittedPR struct {
	PRID        string   `json:"prId"`
	StackNumber int      `json:"stackNumber"`
	Head        string   `json:"head"`
	PRNumber    int      `json:"prNumber"`
	PRURL       string   `json:"prURL"`
	Status      string   `json:"status"`
	Warnings    []string `json:"warnings"`
}
