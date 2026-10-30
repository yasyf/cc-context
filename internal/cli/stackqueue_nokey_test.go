package cli

import (
	"context"
	"testing"

	"github.com/yasyf/cc-context/internal/gtapi"
)

// TestStackQueueOfSkipsATrunkQueueThisMachineHasNoKeyFor is the 6:37 AM Oct 30
// refusal of every ship and stack submit on the monorepo (drive records
// #211122, #211173, #211323): ccx.queue named trunk and the Mac held no
// TRUNK_MERGE_API_KEY, so the queue read before the push failed the run.
func TestStackQueueOfSkipsATrunkQueueThisMachineHasNoKeyFor(t *testing.T) {
	t.Setenv("TRUNK_MERGE_API_KEY", "")
	ctx := context.WithValue(context.Background(), gtAPIKey{}, gtapi.NewWithToken("http://invalid.local", "graphite-token").WithQueueProvider("trunk"))
	states, err := stackQueueOf(ctx, "org", "repo", []gtapi.PullRequestInfo{{PRNumber: 7, HeadRefName: "a"}}, nil)
	if err != nil {
		t.Fatalf("stackQueueOf = %v, want the queue read skipped", err)
	}
	if _, ok := states["a"]; !ok || len(states) != 1 {
		t.Fatalf("states = %v, want a's state read with no queue entry", states)
	}
}

// TestStackQueueOfReadsNoQueueForAStackWithNoPullRequest is root #211345: a
// plain ship of a branch that has no pull request yet never needs the queue.
func TestStackQueueOfReadsNoQueueForAStackWithNoPullRequest(t *testing.T) {
	ctx := context.WithValue(context.Background(), gtAPIKey{}, gtapi.NewWithToken("http://invalid.local", "graphite-token").WithQueueProvider("unread"))
	if states, err := stackQueueOf(ctx, "org", "repo", nil, nil); err != nil || len(states) != 0 {
		t.Fatalf("stackQueueOf = %v, %v; want no queue read", states, err)
	}
}
