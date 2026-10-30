package cli

import (
	"context"
	"testing"

	"github.com/yasyf/cc-context/internal/gtapi"
)

func TestStackQueueOfReadsNoQueueForAStackWithNoPullRequest(t *testing.T) {
	ctx := context.WithValue(context.Background(), gtAPIKey{}, gtapi.NewWithToken("http://invalid.local", "graphite-token").WithQueueProvider("unread"))
	if states, err := stackQueueOf(ctx, "org", "repo", nil, nil); err != nil || len(states) != 0 {
		t.Fatalf("stackQueueOf = %v, %v; want no queue read", states, err)
	}
}
