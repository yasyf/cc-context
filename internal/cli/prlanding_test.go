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

// TestPRLandingVerdict pins how far a pull request's own fields settle its fate.
// The Graphite merge queue squash-merges a whole stack into one trunk commit, so
// a pull request it landed is CLOSED with a null mergedAt and the queue's own
// account as its closer — and so is one the queue dropped, which is why that
// close is a question (prQueueClosed) rather than a verdict. Off the gt lane the
// signature reads as nothing.
func TestPRLandingVerdict(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		body string
		gt   bool
		want prLandingVerdict
	}{
		{"github merged it", landingMerged, true, prLanded},
		{"github merged it off the graphite lane", landingMerged, false, prLanded},
		{"the graphite queue closed it", landingQueueClosed, true, prQueueClosed},
		{"the graphite queue closed it off the graphite lane", landingQueueClosed, false, prAbandoned},
		{"a human closed it", landingHumanClosed, true, prAbandoned},
		{"nobody closed it", landingOpen, true, prStillOpen},
		{"closed with no actor recorded", `{"state":"CLOSED","mergedAt":null,"timelineItems":{"nodes":[]}}`, true, prAbandoned},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := decodeLanding(t, tt.body).verdict(tt.gt); got != tt.want {
				t.Errorf("verdict(%t) = %d, want %d", tt.gt, got, tt.want)
			}
		})
	}
}

// TestQueueMergedLine pins the bullet that separates the queue's landing from
// its drop, both of which close the pull request under the same account. The
// merged body is Forge-AI/monorepo#20486's activity comment; the dropped one is
// #20260, closed when its base branch was deleted and never landed.
func TestQueueMergedLine(t *testing.T) {
	t.Parallel()
	const merged = "### Merge activity\n\n* **Sep 15, 5:40 PM UTC**: `yasyf` added this pull request to the " +
		"[Graphite merge queue](https://app.graphite.com/merges?org=Forge-AI&repo=monorepo).\n" +
		"* **Sep 15, 5:44 PM UTC**: Merged by the [Graphite merge queue]" +
		"(https://app.graphite.com/merges?org=Forge-AI&repo=monorepo) via draft PR: [#20499](https://app.graphite.com)."
	const dropped = "### Merge activity\n\n* **Sep 15, 1:40 PM UTC**: `yasyf` added this pull request to the " +
		"[Graphite merge queue](https://app.graphite.com/merges?org=Forge-AI&repo=monorepo).\n" +
		"* **Sep 15, 1:43 PM UTC**: Removed this pull request from the Graphite merge queue."
	tests := []struct {
		name string
		body string
		want bool
	}{
		{"the queue merged it", merged, true},
		{"the queue dropped it", dropped, false},
		{"no activity comment at all", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := queueMergedLine.MatchString(tt.body); got != tt.want {
				t.Errorf("queueMergedLine.MatchString = %t, want %t", got, tt.want)
			}
		})
	}
}

// TestPRSquashSubject pins the anchor, without which the pattern also matches a
// longer pull request number and a cross-reference in another commit's message.
func TestPRSquashSubject(t *testing.T) {
	t.Parallel()
	pattern := prSquashSubject(20486)
	subjects := map[string]bool{
		"ci: 🔧 keep the bake pinned (#20486)":  true,
		"ci: 🔧 keep the bake pinned (#204861)": false,
		"ci: 🔧 revert of (#20486) on a branch": false,
		"ci: 🔧 nothing to do with it (#20438)": false,
	}
	for subject, want := range subjects {
		if got := pattern.MatchString(subject); got != want {
			t.Errorf("prSquashSubject(20486) on %q = %t, want %t", subject, got, want)
		}
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
