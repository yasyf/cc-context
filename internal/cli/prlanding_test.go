package cli

import (
	"encoding/json"
	"testing"
)

// The four shapes GitHub answers a landing question with. The merged one is
// cli/cli#13084 as testdata/gh/api/reviews-graphql-numbers.json recorded it —
// note that it carries a close of its own, which is why the merge has to be read
// before the closing account — and the open one is that corpus's #13982.
const (
	landingMerged      = `{"state":"MERGED","mergedAt":"2026-04-15T13:12:28Z","timelineItems":{"nodes":[{"actor":{"login":"babakks"}}]}}`
	landingQueueClosed = `{"state":"CLOSED","mergedAt":null,"timelineItems":{"nodes":[{"actor":{"login":"graphite-app"}}]}}`
	landingHumanClosed = `{"state":"CLOSED","mergedAt":null,"timelineItems":{"nodes":[{"actor":{"login":"octocat"}}]}}`
	landingOpen        = `{"state":"OPEN","mergedAt":null,"timelineItems":{"nodes":[]}}`
)

func decodeLanding(t *testing.T, body string) prLanding {
	t.Helper()
	var landing prLanding
	if err := json.Unmarshal([]byte(body), &landing); err != nil {
		t.Fatalf("unmarshal %s: %v", body, err)
	}
	return landing
}

// TestPRLandingLanded pins the merged-versus-abandoned verdict for every way a
// pull request ends. The Graphite merge queue squash-merges a whole stack into
// one trunk commit, so a pull request it landed is CLOSED with a null mergedAt
// and the queue's own account as its closer — by state alone indistinguishable
// from the pull request a human gave up on. That signature is Graphite's, so off
// the gt lane it reads as nothing.
func TestPRLandingLanded(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		body string
		gt   bool
		want bool
	}{
		{"github merged it", landingMerged, true, true},
		{"github merged it off the graphite lane", landingMerged, false, true},
		{"the graphite queue closed it", landingQueueClosed, true, true},
		{"the graphite queue closed it off the graphite lane", landingQueueClosed, false, false},
		{"a human closed it", landingHumanClosed, true, false},
		{"nobody closed it", landingOpen, true, false},
		{"closed with no actor recorded", `{"state":"CLOSED","mergedAt":null,"timelineItems":{"nodes":[]}}`, true, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := decodeLanding(t, tt.body).landed(tt.gt); got != tt.want {
				t.Errorf("landed(%t) = %t, want %t", tt.gt, got, tt.want)
			}
		})
	}
}

func TestPRLandingClosedBy(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		body string
		want string
	}{
		{"the graphite queue", landingQueueClosed, graphiteQueueActor},
		{"a human", landingHumanClosed, "octocat"},
		{"the merger", landingMerged, "babakks"},
		{"nobody", landingOpen, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := decodeLanding(t, tt.body).closedBy(); got != tt.want {
				t.Errorf("closedBy() = %q, want %q", got, tt.want)
			}
		})
	}
}
