package nativeapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/yasyf/cc-context/internal/ghapi"
	"github.com/yasyf/cc-context/internal/render"
)

const (
	trunkLimitRetries  = 5
	trunkLimitPatience = 2 * time.Minute
)

type trunkPage[T any] struct {
	Data       []T     `json:"data"`
	NextCursor *string `json:"nextCursor"`
	HasMore    bool    `json:"hasMore"`
}
type trunkPull struct {
	Number             int  `json:"number"`
	CurrentlySubmitted bool `json:"isCurrentlySubmittedToQueue"`
	QueueEntry         *struct {
		State string `json:"state"`
	} `json:"queueEntry"`
}

func (c *Client) queueGet(ctx context.Context, path string, query url.Values, out any) error {
	token, err := c.trunkToken(ctx)
	if err != nil {
		return err
	}
	var waited time.Duration
	for attempt := 0; ; attempt++ {
		payload, err := c.request(ctx, c.queueBase, token, ghapi.IdentityUser, http.MethodGet, path+"?"+query.Encode(), nil)
		if err == nil {
			return json.Unmarshal(payload, out)
		}
		var refused *StatusError
		if !errors.As(err, &refused) || refused.Status != http.StatusTooManyRequests {
			return err
		}
		named, _ := ghapi.LimitWait(refused.Status, refused.Header, time.Now())
		wait := max(named, time.Second<<attempt)
		pause := wait + rand.N(wait/4+1) //nolint:gosec // jitter only spreads retries; nothing relies on it being unguessable
		waited += pause
		if attempt == trunkLimitRetries || waited > trunkLimitPatience {
			return err
		}
		render.Announce(ctx, "Trunk rate-limited GET %s, retrying in %s (wait %d of %d)", path, pause.Round(time.Second), attempt+1, trunkLimitRetries)
		select {
		case <-ctx.Done():
			return fmt.Errorf("%w: %w", ctx.Err(), err)
		case <-time.After(pause):
		}
	}
}

func (c *Client) queueRepository(ctx context.Context, owner, repo string) (string, error) {
	query := url.Values{"providerUrl": {"https://github.com/" + owner + "/" + repo}, "limit": {"100"}}
	var ids []string
	for {
		var page trunkPage[struct {
			ID string `json:"id"`
		}]
		if err := c.queueGet(ctx, "/v2/repositories", query, &page); err != nil {
			return "", err
		}
		for _, r := range page.Data {
			ids = append(ids, r.ID)
		}
		if !page.HasMore {
			break
		}
		if page.NextCursor == nil || *page.NextCursor == "" {
			return "", fmt.Errorf("trunk repository response advertises another page without a cursor")
		}
		query.Set("cursor", *page.NextCursor)
	}
	if len(ids) != 1 {
		return "", fmt.Errorf("trunk resolved %s/%s to %d repositories, expected one", owner, repo, len(ids))
	}
	return ids[0], nil
}

// MergeabilityStatuses reads the selected queue and leaves unindexed PRs absent.
func (c *Client) MergeabilityStatuses(ctx context.Context, owner, repo string, numbers []int) (map[int]string, error) {
	switch c.queueProvider {
	case "", "github":
		return c.githubQueueStatuses(ctx, owner, repo, numbers)
	case "none":
		states := make(map[int]string, len(numbers))
		for _, number := range numbers {
			states[number] = "notQueued"
		}
		return states, nil
	case "trunk":
		return c.trunkQueueStatuses(ctx, owner, repo, numbers)
	default:
		return nil, fmt.Errorf("unknown ccx.queue provider %q; expected github, trunk, or none", c.queueProvider)
	}
}

func (c *Client) githubQueueStatuses(ctx context.Context, owner, repo string, numbers []int) (map[int]string, error) {
	states := make(map[int]string, len(numbers))
	for batch := range slices.Chunk(numbers, 40) {
		var fields strings.Builder
		for i, n := range batch {
			fmt.Fprintf(&fields, "p%d:pullRequest(number:%d){mergeQueueEntry{state}} ", i, n)
		}
		query := fmt.Sprintf("{repository(owner:%s,name:%s){%s}}", strconv.Quote(owner), strconv.Quote(repo), fields.String())
		var response struct {
			Repository map[string]*struct{ MergeQueueEntry *struct{ State string } }
		}
		if err := c.readGraphQL(ctx, owner, repo, query, &response); err != nil {
			return nil, err
		}
		if response.Repository == nil {
			return nil, fmt.Errorf("GitHub repository %s/%s is not accessible", owner, repo)
		}
		for i, n := range batch {
			pr := response.Repository[fmt.Sprintf("p%d", i)]
			if pr == nil {
				continue
			}
			states[n] = "notQueued"
			if pr.MergeQueueEntry != nil {
				states[n] = "queued:github:" + pr.MergeQueueEntry.State
			}
		}
	}
	return states, nil
}

func (c *Client) trunkQueueStatuses(ctx context.Context, owner, repo string, numbers []int) (map[int]string, error) {
	states := make(map[int]string, len(numbers))
	if len(numbers) == 0 {
		return states, nil
	}
	repository, err := c.queueRepository(ctx, owner, repo)
	if err != nil {
		return nil, err
	}
	for batch := range slices.Chunk(numbers, 50) {
		nums := make([]string, len(batch))
		for i, n := range batch {
			nums[i] = strconv.Itoa(n)
		}
		query := url.Values{"repositoryId": {repository}, "number": {strings.Join(nums, ",")}, "limit": {"100"}}
		for {
			var page trunkPage[trunkPull]
			if err := c.queueGet(ctx, "/v2/pull-requests", query, &page); err != nil {
				return nil, err
			}
			for _, pr := range page.Data {
				if !slices.Contains(batch, pr.Number) {
					return nil, fmt.Errorf("trunk returned unrequested pull request #%d", pr.Number)
				}
				if pr.QueueEntry == nil {
					if pr.CurrentlySubmitted {
						return nil, fmt.Errorf("trunk reports PR #%d submitted without a queue entry", pr.Number)
					}
					states[pr.Number] = "notQueued"
					continue
				}
				state := pr.QueueEntry.State
				if pr.CurrentlySubmitted && !QueuedMergeability(state) {
					state = "queued:" + state
				}
				if !pr.CurrentlySubmitted && QueuedMergeability(state) {
					return nil, fmt.Errorf("trunk reports PR #%d outside the queue with active state %q", pr.Number, state)
				}
				states[pr.Number] = state
			}
			if !page.HasMore {
				break
			}
			if page.NextCursor == nil || *page.NextCursor == "" {
				return nil, fmt.Errorf("trunk pull request response advertises another page without a cursor")
			}
			query.Set("cursor", *page.NextCursor)
		}
	}
	return states, nil
}
