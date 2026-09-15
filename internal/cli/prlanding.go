package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/yasyf/cc-context/internal/render"
)

// graphiteQueueActor is the account Graphite's merge queue closes a pull request
// with. The queue squash-merges a whole stack into one trunk commit, so GitHub
// records the individual pull requests as closed rather than merged.
const graphiteQueueActor = "graphite-app"

// prLandingFields is the GraphQL selection prLanding decodes, so every batch
// asking whether a pull request landed asks it the same way.
const prLandingFields = "state mergedAt " +
	"timelineItems(last: 1, itemTypes: [CLOSED_EVENT]) { nodes { ... on ClosedEvent { actor { login } } } }"

// queueMergedLine matches the bullet Graphite's merge-activity comment gains the
// moment its queue lands the pull request.
var queueMergedLine = regexp.MustCompile(`(?i)merged by the \[?graphite merge queue`)

// prSquashCandidates bounds the commits whose subjects are read back. The queue
// writes one squash per landing, so the number this cites is in the newest few
// of them; a landing older than this many commits is left to the queue's own
// comment to answer.
const prSquashCandidates = 200

// prLanding is how a pull request ended. State and mergedAt answer that outright
// for anything GitHub merged itself; the account that closed it narrows the rest
// to the Graphite merge queue's own closes, which land some pull requests and
// drop others.
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

// prLandingVerdict is how far a pull request's own fields settle its fate.
type prLandingVerdict int

const (
	// prStillOpen is a pull request GitHub still reports as open.
	prStillOpen prLandingVerdict = iota
	// prLanded reached the trunk.
	prLanded
	// prAbandoned closed without reaching it.
	prAbandoned
	// prQueueClosed is the Graphite merge queue's close, which the pull
	// request's own fields cannot tell from an abandonment.
	prQueueClosed
)

// verdict reads the end a pull request's own fields record. GitHub's merge is
// decisive; the queue's close is not, because it closes the pull requests it
// drops, the children an unlanded base branch deletes, and its own merge-queue
// draft pull requests with the account it closes landings with. gt admits that
// signature at all, being Graphite's and meaningless off its lane.
func (l prLanding) verdict(gt bool) prLandingVerdict {
	if l.MergedAt != nil || l.State == "MERGED" {
		return prLanded
	}
	if l.State == "OPEN" {
		return prStillOpen
	}
	if gt && l.State == "CLOSED" && l.closedBy() == graphiteQueueActor {
		return prQueueClosed
	}
	return prAbandoned
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

// prSquashSubject matches the subject the queue leaves on the squash it writes.
// The anchor earns its keep: unanchored, the number also matches a longer pull
// request number and a cross-reference somewhere in another commit's message.
func prSquashSubject(number int) *regexp.Regexp {
	return regexp.MustCompile(fmt.Sprintf(`\(#%d\)$`, number))
}

// prSquashOnBase returns the squash the queue wrote for this pull request, and
// is empty when this checkout's copy of the base branch carries none. A checkout
// that has not fetched since the landing answers the same way, so emptiness is
// only ever the absence of evidence here, never proof of an abandonment.
//
// git's --grep reads the whole message, where the number also appears in bodies
// citing the pull request, so it only narrows the candidates: the subject of
// each is what decides.
func prSquashOnBase(ctx context.Context, dir render.Dir, base string, number int) string {
	if base == "" {
		return ""
	}
	out, err := render.RunCLI(ctx, dir, "git", []string{
		"log", "--format=%H%x09%s", "--extended-regexp",
		fmt.Sprintf("--grep=\\(#%d\\)", number), "-" + strconv.Itoa(prSquashCandidates), "origin/" + base,
	})
	if err != nil {
		return ""
	}
	subject := prSquashSubject(number)
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		sha, message, found := strings.Cut(line, "\t")
		if found && subject.MatchString(message) {
			return sha
		}
	}
	return ""
}

// prQueueClose is one pull request the Graphite merge queue closed, and the base
// branch its squash would be on.
type prQueueClose struct {
	Number int
	Base   string
}

// resolveQueueLandings answers the queue's ambiguous closes. The squash goes
// first because the checkout can answer it without a round trip, and whatever it
// leaves unanswered costs one query for the queue's own account of itself.
//
// Evidence that cannot be fetched reads as not landed, which is the safe
// direction for the callers that cannot report the difference: prune leaves the
// branch alone, and ship attempts a submit gt refuses anyway. The reviews watch,
// which has a retry of its own, propagates the failure instead.
func resolveQueueLandings(ctx context.Context, dir render.Dir, closes []prQueueClose) map[int]bool {
	landed := make(map[int]bool, len(closes))
	ask := make([]int, 0, len(closes))
	for _, closed := range closes {
		if prSquashOnBase(ctx, dir, closed.Base, closed.Number) != "" {
			landed[closed.Number] = true
			continue
		}
		ask = append(ask, closed.Number)
	}
	for number, body := range prQueueActivity(ctx, dir, ask) {
		landed[number] = queueMergedLine.MatchString(body)
	}
	return landed
}

// prQueueActivity fetches the Graphite merge-activity comment of every pull
// request named, in one round trip, and answers nothing for the ones that have
// none. Only a queue close reaches here, so the kilobytes of bot commentary this
// selection also carries are spent on the few pull requests that need them.
func prQueueActivity(ctx context.Context, dir render.Dir, numbers []int) map[int]string {
	if len(numbers) == 0 {
		return nil
	}
	argv := []string{"api", "graphql", "-F", "owner={owner}", "-F", "repo={repo}"}
	for i, number := range numbers {
		argv = append(argv, "-F", fmt.Sprintf("%s=%d", prActivityAlias(i), number))
	}
	argv = append(argv, "-f", "query="+prActivityQuery(len(numbers)))
	out, err := render.RunCLI(ctx, dir, "gh", argv)
	if err != nil {
		return nil
	}
	var resp struct {
		Data struct {
			Repository map[string]struct {
				Comments struct {
					Nodes []struct {
						Author *struct {
							Login string `json:"login"`
						} `json:"author"`
						Body string `json:"body"`
					} `json:"nodes"`
				} `json:"comments"`
			} `json:"repository"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &resp); err != nil {
		return nil
	}
	activity := make(map[int]string, len(numbers))
	for i, number := range numbers {
		for _, comment := range resp.Data.Repository[prActivityAlias(i)].Comments.Nodes {
			if comment.Author != nil && comment.Author.Login == graphiteQueueActor {
				activity[number] = comment.Body
			}
		}
	}
	return activity
}

// prActivityAlias names one pull request's field in the batched activity query.
func prActivityAlias(i int) string {
	return fmt.Sprintf("a%d", i)
}

// prActivityQuery renders one aliased pullRequest field per number, selecting
// the tail of its comments — Graphite edits its one merge-activity comment in
// place, so the landing bullet arrives on a comment as old as the enqueue.
func prActivityQuery(n int) string {
	decls := make([]string, 0, n+2)
	decls = append(decls, "$owner: String!", "$repo: String!")
	var fields strings.Builder
	for i := range n {
		alias := prActivityAlias(i)
		decls = append(decls, "$"+alias+": Int!")
		fmt.Fprintf(&fields, "    %s: pullRequest(number: $%s) { comments(last: 100) { nodes { author { login } body } } }\n",
			alias, alias)
	}
	return fmt.Sprintf("query(%s) {\n  repository(owner: $owner, name: $repo) {\n%s  }\n}",
		strings.Join(decls, ", "), fields.String())
}
