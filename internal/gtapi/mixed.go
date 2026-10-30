package gtapi

import (
	"context"
	"errors"
	"maps"
)

// WithBranchProviders selects each status record from actual stack ownership instead of the checkout lane.
func (c *Client) WithBranchProviders(providers map[string]string) *Client {
	selected := *c
	selected.branchProviders = maps.Clone(providers)
	legacy := *c
	legacy.native, legacy.queue, legacy.legacy, legacy.branchProviders = nil, nil, nil, nil
	legacy.queueProvider = ""
	selected.legacy = &legacy
	if selected.native == nil {
		selected.native = NewNative().native
	}
	return &selected
}

func (c *Client) nativeInfo(info PullRequestInfo) bool {
	return info.StackNumber != 0 || c.branchProviders[info.HeadRefName] != "graphite"
}

func (c *Client) mixedPullRequestInfo(ctx context.Context, req PullRequestInfoRequest) ([]PullRequestInfo, error) {
	preferred := *c
	preferred.queueProvider = ""
	infos, err := preferred.nativePullRequestInfo(ctx, req)
	if err != nil {
		return nil, err
	}
	legacyReq := req
	legacyReq.PRNumbers, legacyReq.PRHeadRefNames = nil, nil
	for _, info := range infos {
		if !c.nativeInfo(info) || c.queueProvider == "graphite" {
			legacyReq.PRNumbers = append(legacyReq.PRNumbers, info.PRNumber)
		}
	}
	legacyInfos := map[int]PullRequestInfo{}
	if len(legacyReq.PRNumbers) > 0 {
		records, err := c.legacy.PullRequestInfo(ctx, legacyReq)
		if err != nil {
			return nil, err
		}
		for _, info := range records {
			legacyInfos[info.PRNumber] = info
		}
	}
	for i, native := range infos {
		legacy := legacyInfos[native.PRNumber]
		if !c.nativeInfo(native) {
			if legacy.PRNumber == 0 {
				return nil, &ResultError{Route: "pull-request-info", Message: "Graphite returned no metadata for tracked branch " + native.HeadRefName}
			}
			infos[i] = legacy
		}
		switch c.queueProvider {
		case "github":
			infos[i].MergeQueueStatus = native.MergeQueueStatus
		case "graphite":
			infos[i].MergeQueueStatus = legacy.MergeQueueStatus
		case "trunk", "none":
			infos[i].MergeQueueStatus = nil
		}
	}
	return infos, nil
}

// MergeabilityFor uses the same immutable metadata snapshot to select each pull request's queue.
func (c *Client) MergeabilityFor(ctx context.Context, owner, repo string, infos []PullRequestInfo, numbers []int) (map[int]string, error) {
	if c.legacy == nil || c.queueProvider != "" {
		return c.MergeabilityStatuses(ctx, owner, repo, numbers)
	}
	legacyNumbers := map[int]bool{}
	for _, info := range infos {
		if !c.nativeInfo(info) {
			legacyNumbers[info.PRNumber] = true
		}
	}
	var native, legacy []int
	for _, number := range numbers {
		if legacyNumbers[number] {
			legacy = append(legacy, number)
		} else {
			native = append(native, number)
		}
	}
	result := make(map[int]string, len(numbers))
	var failures []error
	if len(native) > 0 {
		states, err := commonQueueStatuses(c.native.MergeabilityStatuses(ctx, owner, repo, native))
		if err != nil {
			failures = append(failures, err)
		}
		maps.Copy(result, states)
	}
	if len(legacy) > 0 {
		states, err := c.legacy.MergeabilityStatuses(ctx, owner, repo, legacy)
		if err != nil {
			failures = append(failures, err)
		}
		maps.Copy(result, states)
	}
	return result, errors.Join(failures...)
}

// QueueRequiresStatus reports whether admission cannot be established from metadata alone.
func (c *Client) QueueRequiresStatus(info PullRequestInfo) bool {
	if c.queueProvider != "" {
		return c.queueProvider != "graphite"
	}
	if c.legacy != nil {
		return c.nativeInfo(info)
	}
	return c.IsNative()
}
