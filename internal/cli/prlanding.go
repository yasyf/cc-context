package cli

import "time"

// graphiteQueueActor is the account Graphite's merge queue closes a pull request
// with. The queue squash-merges a whole stack into one trunk commit, so GitHub
// records the individual pull requests as closed rather than merged.
const graphiteQueueActor = "graphite-app"

// prLandingFields is the GraphQL selection prLanding decodes, so every batch
// asking whether a pull request landed asks it the same way.
const prLandingFields = "state mergedAt " +
	"timelineItems(last: 1, itemTypes: [CLOSED_EVENT]) { nodes { ... on ClosedEvent { actor { login } } } }"

// prLanding is how a pull request ended. State and mergedAt answer that outright
// for anything GitHub merged itself; the account that closed it is what
// separates a stack the Graphite merge queue landed — CLOSED, with a null
// mergedAt and no merge commit — from one a human abandoned.
type prLanding struct {
	State         string     `json:"state"`
	MergedAt      *time.Time `json:"mergedAt"`
	TimelineItems struct {
		Nodes []struct {
			Actor struct {
				Login string `json:"login"`
			} `json:"actor"`
		} `json:"nodes"`
	} `json:"timelineItems"`
}

// landed reports whether a pull request reached the trunk. gt admits the merge
// queue's signature, which is Graphite's own and only meaningful for a
// repository landing through it; off that lane a closed pull request is closed.
func (l prLanding) landed(gt bool) bool {
	if l.MergedAt != nil || l.State == "MERGED" {
		return true
	}
	return gt && l.State == "CLOSED" && l.closedBy() == graphiteQueueActor
}

// closedBy names the account that closed the pull request last, and is empty
// until one has. A merged pull request carries a close of its own, so it is read
// only after the merge itself comes back negative.
func (l prLanding) closedBy() string {
	nodes := l.TimelineItems.Nodes
	if len(nodes) == 0 {
		return ""
	}
	return nodes[0].Actor.Login
}
