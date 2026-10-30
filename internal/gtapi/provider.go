package gtapi

import (
	"context"
	"errors"
	"fmt"

	"github.com/yasyf/cc-context/internal/nativeapi"
)

func NewNative() *Client {
	c := New(baseURLProd)
	c.native = nativeapi.Default()
	return c
}

func NewNativeWithToken(baseURL, token string) *Client {
	c := New(baseURLProd)
	c.native = nativeapi.NewWithToken(baseURL, token)
	return c
}

func (c *Client) WithProviderError(err error) *Client {
	selected := *c
	selected.providerErr = err
	return &selected
}

func (c *Client) IsNative() bool { return c.native != nil }

func (c *Client) QueueProvider() string {
	if c.providerErr != nil {
		return "invalid"
	}
	if c.queueProvider != "" {
		return c.queueProvider
	}
	if c.IsNative() {
		return "github"
	}
	return "graphite"
}

func (c *Client) CacheNamespace() string {
	if c.providerErr != nil {
		return "invalid"
	}
	queue := c.QueueProvider()
	if !c.IsNative() && queue == "graphite" {
		return ""
	}
	stack := "graphite-stack"
	if c.IsNative() {
		stack = "github-stack"
	}
	return stack + "--" + queue
}

func (c *Client) WithQueueProvider(provider string) *Client {
	selected := *c
	switch provider {
	case "", "graphite", "github", "trunk", "none":
	default:
		selected.providerErr = fmt.Errorf("unknown ccx.queue provider %q; expected graphite, github, trunk, or none", provider)
		return &selected
	}
	selected.queueProvider = provider
	if provider == "" || provider == "graphite" {
		selected.queue = nil
		return &selected
	}
	queue := c.native
	if queue == nil {
		queue = nativeapi.Default()
	}
	selected.queue = queue.WithQueueProvider(provider)
	return &selected
}

func (c *Client) nativePullRequestInfo(ctx context.Context, req PullRequestInfoRequest) ([]PullRequestInfo, error) {
	infos, err := c.native.PullRequestInfo(ctx, nativeapi.PullRequestInfoRequest(req))
	if err != nil {
		return nil, commonNativeError(err)
	}
	result := make([]PullRequestInfo, 0, len(infos))
	for _, info := range infos {
		p := PullRequestInfo{PRID: info.PRID, StackNumber: info.StackNumber, PRNumber: info.PRNumber, Title: info.Title, AuthorGithubHandle: info.AuthorGithubHandle, Body: info.Body, State: PRState(info.State), ReviewDecision: info.ReviewDecision, HeadRefName: info.HeadRefName, BaseRefName: info.BaseRefName, URL: info.URL, IsDraft: info.IsDraft, DependentPRNumber: info.DependentPRNumber, MergeCommitSha: info.MergeCommitSha, CliTrunk: info.CliTrunk}
		for _, v := range info.Versions {
			p.Versions = append(p.Versions, PRVersion{HeadSha: v.HeadSha, BaseSha: v.BaseSha, BaseName: v.BaseName, CreatedAt: v.CreatedAt, AuthorGithubHandle: v.AuthorGithubHandle})
		}
		if info.MergeQueueStatus != nil && (c.queueProvider == "" || c.queueProvider == "github") {
			p.MergeQueueStatus = &MergeQueueStatus{InQueue: info.MergeQueueStatus.InQueue, EnqueuedCommit: info.MergeQueueStatus.EnqueuedCommit}
		}
		result = append(result, p)
	}
	return result, nil
}

func (c *Client) nativeSubmitPullRequests(ctx context.Context, req SubmitRequest) ([]SubmittedPR, error) {
	request := nativeapi.SubmitRequest{RepoOwner: req.RepoOwner, RepoName: req.RepoName, TrunkBranchName: req.TrunkBranchName, TargetTrunkBranchName: req.TargetTrunkBranchName, MergeWhenReady: req.MergeWhenReady, RerequestReview: req.RerequestReview}
	for _, p := range req.PRs {
		request.PRs = append(request.PRs, nativeapi.SubmitPR{Action: nativeapi.SubmitAction(p.Action), Head: p.Head, HeadSha: p.HeadSha, Base: p.Base, BaseSha: p.BaseSha, PRNumber: p.PRNumber, Title: p.Title, Body: p.Body, Draft: p.Draft, Reviewers: p.Reviewers, ShouldRetarget: p.ShouldRetarget, RebaseOnlyChange: p.RebaseOnlyChange, MaintainRetarget: p.MaintainRetarget})
	}
	submitted, err := c.native.SubmitPullRequests(ctx, request)
	return commonSubmitted(submitted), commonNativeError(err)
}

func commonSubmitted(prs []nativeapi.SubmittedPR) []SubmittedPR {
	if prs == nil {
		return nil
	}
	result := make([]SubmittedPR, 0, len(prs))
	for _, p := range prs {
		result = append(result, SubmittedPR{PRID: p.PRID, StackNumber: p.StackNumber, Head: p.Head, PRNumber: p.PRNumber, PRURL: p.PRURL, Status: p.Status, Warnings: p.Warnings})
	}
	return result
}

func commonNativeError(err error) error {
	if err == nil {
		return nil
	}
	var submit *nativeapi.SubmitError
	if errors.As(err, &submit) {
		out := &SubmitError{Cause: commonNativeError(submit.Cause), Submitted: commonSubmitted(submit.Submitted)}
		for _, f := range submit.Failed {
			out.Failed = append(out.Failed, BranchSubmitError{Head: f.Head, Message: f.Message})
		}
		return out
	}
	var status *nativeapi.StatusError
	if errors.As(err, &status) {
		return &StatusError{Method: status.Method, URL: status.URL, Status: status.Status, Message: status.Message}
	}
	var result *nativeapi.ResultError
	if errors.As(err, &result) {
		return &ResultError{Route: result.Route, Message: result.Message}
	}
	if errors.Is(err, nativeapi.ErrUnauthorized) {
		return fmt.Errorf("%w: %w", ErrUnauthorized, err)
	}
	if errors.Is(err, nativeapi.ErrNoToken) {
		return fmt.Errorf("%w: %w", ErrNoToken, err)
	}
	return err
}

func commonQueueStatuses(states map[int]string, err error) (map[int]string, error) {
	if err != nil {
		return nil, commonNativeError(err)
	}
	result := make(map[int]string, len(states))
	for number, state := range states {
		switch state {
		case nativeapi.MergeabilityFailureHandling:
			state = MergeabilityFailureHandling
		case "notReady", nativeapi.MergeabilityWaiting:
			state = MergeabilityWaiting
		case nativeapi.MergeabilityRebasing:
			state = MergeabilityRebasing
		default:
			if nativeapi.QueuedMergeability(state) {
				state = MergeabilityQueued
			}
		}
		result[number] = state
	}
	return result, nil
}
