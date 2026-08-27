package cli

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// mqLabel and mqLabelFast are the labels Graphite's merge queue watches, and
// mqAdded through mqMerged its wording in the "Merge activity" comment it edits
// in place. That comment's last recognized bullet is the queue's verdict.
const (
	mqLabel     = "merge"
	mqLabelFast = "merge-fast"

	mqAdded     = "added this pull request to the "
	mqDetected  = "was detected. This PR will be added"
	mqRemoved   = "removed this pull request due to "
	mqRunningCI = "CI is running for this pull request on a draft pull request"
	mqMerged    = "Merged by the "
)

// mqDraftRef matches the pull request number in an activity bullet, which
// Graphite links to its own app rather than to GitHub — so a draft never
// becomes a cross-reference and cannot be read off the timeline.
var mqDraftRef = regexp.MustCompile(`#(\d+)`)

// mqLink matches one markdown link, whose text is what a report quotes.
var mqLink = regexp.MustCompile(`\[([^\]]*)\]\([^)]*\)`)

// mqPhase is where a pull request stands with the Graphite merge queue. The
// queue consumes the merge label on admission and drops it on failure, so the
// label's absence alone cannot separate accepted from rejected — these can.
type mqPhase string

const (
	mqPhaseLabeled   mqPhase = "labeled"
	mqPhasePending   mqPhase = "pending"
	mqPhaseQueued    mqPhase = "queued"
	mqPhaseRejected  mqPhase = "rejected"
	mqPhaseMerged    mqPhase = "merged"
	mqPhaseWithdrawn mqPhase = "withdrawn"
)

// mqCommit is one commit on the branch. At is the committer date, which a
// restack rewrites to the replay — the closest GitHub carries to when the
// branch last moved.
type mqCommit struct {
	OID     string
	Subject string
	At      time.Time
}

// mqForcePush is one HeadRefForcePushedEvent. Before is the head the push
// replaced, and the only record of a commit the branch no longer carries.
type mqForcePush struct {
	At     time.Time
	Before string
}

// mqLabelEvent is one add or removal of a merge label. Actor separates the
// queue consuming the label from a human taking it back off.
type mqLabelEvent struct {
	At    time.Time
	Name  string
	Actor string
	Added bool
}

// mqInput is everything the reconstruction reads off one pull request. Drafts
// dates each merge-queue draft the activity comment names: a draft is cut from
// the branch as it stands, so its birth is the queue's latest snapshot.
type mqInput struct {
	Head     string
	Landed   bool
	Labels   []string
	Commits  []mqCommit
	Pushes   []mqForcePush
	Events   []mqLabelEvent
	Activity string
	Drafts   map[int]time.Time
}

// statusCommit is one commit named in a report.
type statusCommit struct {
	OID     string `json:"oid"`
	Subject string `json:"subject,omitempty"`
}

// statusQueue is what the Graphite merge queue is doing with one pull request,
// and which commit it snapshotted — the fact that decides whether landing the
// pull request lands the branch.
type statusQueue struct {
	Phase   mqPhase        `json:"phase"`
	Label   string         `json:"label,omitempty"`
	Detail  string         `json:"detail,omitempty"`
	DraftPR int            `json:"draft_pr,omitempty"`
	Held    string         `json:"held_sha,omitempty"`
	Unknown string         `json:"held_unknown,omitempty"`
	Dropped []statusCommit `json:"dropped,omitempty"`
}

// Drifted reports whether the queue holds something other than the branch head,
// which is the condition under which merging lands less than the branch.
func (q statusQueue) Drifted() bool { return q.Held != "" && len(q.Dropped) > 0 }

// mergeQueue reconstructs the queue's view of one pull request, and is nil when
// the queue was never involved. Graphite publishes no field naming the commit
// it snapshotted, but GitHub records when the queue last looked at the branch
// and how the head moved since.
func mergeQueue(in mqInput) *statusQueue {
	q := mqPhaseOf(in)
	if q == nil {
		return nil
	}
	if q.Phase == mqPhaseRejected || q.Phase == mqPhaseWithdrawn {
		return q
	}
	at, ok := mqSnapshotAt(in)
	if !ok {
		q.Unknown = "no merge label or draft pull request to date the snapshot from"
		return q
	}
	q.Held = mqHeldAt(in, at)
	for _, c := range in.Commits {
		if c.At.After(at) {
			q.Dropped = append(q.Dropped, statusCommit{OID: c.OID, Subject: c.Subject})
		}
	}
	return q
}

// mqPhaseOf reads the phase and the bullet supporting it. The activity comment
// outranks the labels: it is the queue speaking, where a label is only what
// someone asked for.
func mqPhaseOf(in mqInput) *statusQueue {
	q := &statusQueue{Label: mqCurrentLabel(in.Labels)}
	for _, bullet := range mqBullets(in.Activity) {
		switch {
		case strings.Contains(bullet, mqMerged):
			q.Phase, q.Detail = mqPhaseMerged, bullet
		case strings.Contains(bullet, mqRemoved):
			q.Phase, q.Detail, q.DraftPR = mqPhaseRejected, bullet, 0
		case strings.Contains(bullet, mqRunningCI):
			q.Phase, q.Detail = mqPhaseQueued, bullet
			if n, ok := mqDraftNumber(bullet); ok {
				q.DraftPR = n
			}
		case strings.Contains(bullet, mqAdded):
			q.Phase, q.Detail = mqPhaseQueued, bullet
		case strings.Contains(bullet, mqDetected):
			q.Phase, q.Detail = mqPhasePending, bullet
		}
	}
	if q.Phase != "" {
		return q
	}
	q = mqLabelPhase(q, in.Events)
	if q != nil && in.Landed {
		q.Phase = mqPhaseMerged
	}
	return q
}

// mqLabelPhase reads the phase off the label events alone, which is the whole
// record when the queue took a labelled pull request without commenting on it.
func mqLabelPhase(q *statusQueue, events []mqLabelEvent) *statusQueue {
	last, ok := mqLastLabelEvent(events)
	switch {
	case q.Label != "":
		q.Phase = mqPhaseLabeled
	case !ok:
		return nil
	case last.Added:
		q.Phase, q.Label = mqPhaseLabeled, last.Name
	case last.Actor == graphiteQueueActor:
		q.Phase, q.Label = mqPhaseQueued, last.Name
		q.Detail = "the queue consumed the " + last.Name + " label"
	default:
		q.Phase, q.Label = mqPhaseWithdrawn, last.Name
		q.Detail = last.Actor + " took the " + last.Name + " label back off"
	}
	return q
}

// mqSnapshotAt dates the queue's most recent look at the branch: the newest of
// the drafts it opened and the merge labels it was given. The draft counts
// because a re-formed one picks up commits pushed since the label went on.
func mqSnapshotAt(in mqInput) (time.Time, bool) {
	var at time.Time
	for _, born := range in.Drafts {
		if born.After(at) {
			at = born
		}
	}
	for _, e := range in.Events {
		if e.Added && e.At.After(at) {
			at = e.At
		}
	}
	return at, !at.IsZero()
}

// mqHeldAt resolves the branch head as of at. A force push is authoritative —
// its Before names a commit the branch no longer carries, so nothing else can —
// and otherwise the newest commit laid down by then is the head.
func mqHeldAt(in mqInput, at time.Time) string {
	for _, p := range in.Pushes {
		if p.At.After(at) {
			return p.Before
		}
	}
	held := in.Head
	var newest time.Time
	for _, c := range in.Commits {
		if c.At.After(at) {
			continue
		}
		if newest.IsZero() || c.At.After(newest) {
			held, newest = c.OID, c.At
		}
	}
	return held
}

// mqCurrentLabel names the merge label presently on the pull request.
func mqCurrentLabel(labels []string) string {
	for _, name := range labels {
		if name == mqLabel || name == mqLabelFast {
			return name
		}
	}
	return ""
}

// mqLastLabelEvent returns the newest add or removal of a merge label.
func mqLastLabelEvent(events []mqLabelEvent) (mqLabelEvent, bool) {
	var last mqLabelEvent
	found := false
	for _, e := range events {
		if e.Name != mqLabel && e.Name != mqLabelFast {
			continue
		}
		if !found || e.At.After(last.At) {
			last, found = e, true
		}
	}
	return last, found
}

// mqBullets splits the activity comment into its bullets, in the order Graphite
// appends them, each stripped of its list marker and bolded timestamp.
func mqBullets(activity string) []string {
	var bullets []string
	for _, line := range strings.Split(activity, "\n") {
		rest, ok := strings.CutPrefix(strings.TrimSpace(line), "* ")
		if !ok {
			continue
		}
		if stamped, ok := strings.CutPrefix(rest, "**"); ok {
			if _, after, ok := strings.Cut(stamped, "**: "); ok {
				rest = after
			}
		}
		bullets = append(bullets, mqPlain(rest))
	}
	return bullets
}

// mqPlain reduces one bullet to the sentence a report prints: links become
// their text, emphasis goes, and the trailing period stays off so the line
// joins others with shipSep.
func mqPlain(s string) string {
	s = mqLink.ReplaceAllString(s, "$1")
	s = strings.NewReplacer("**", "", "`", "").Replace(s)
	return strings.TrimRight(strings.TrimSpace(s), ".")
}

// mqDraftNumber reads the draft pull request a bullet names.
func mqDraftNumber(bullet string) (int, bool) {
	m := mqDraftRef.FindStringSubmatch(bullet)
	if m == nil {
		return 0, false
	}
	n, err := strconv.Atoi(m[1])
	return n, err == nil
}

// mqDraftNumbers lists every draft an activity comment names, so one batched
// query can date them all.
func mqDraftNumbers(activity string) []int {
	var nums []int
	seen := map[int]bool{}
	for _, bullet := range mqBullets(activity) {
		if !strings.Contains(bullet, mqRunningCI) && !strings.Contains(bullet, mqMerged) {
			continue
		}
		if n, ok := mqDraftNumber(bullet); ok && !seen[n] {
			seen[n], nums = true, append(nums, n)
		}
	}
	return nums
}

// mqValue renders one queue line: where the pull request stands, and whether
// landing it would land the branch.
func mqValue(q statusQueue, head string) string {
	segs := []string{string(q.Phase)}
	if q.Label != "" {
		segs[0] += " as " + q.Label
	}
	if q.DraftPR != 0 {
		segs = append(segs, fmt.Sprintf("draft #%d", q.DraftPR))
	}
	switch {
	case q.Unknown != "":
		segs = append(segs, "held sha unknown: "+q.Unknown)
	case q.Drifted():
		segs = append(segs,
			fmt.Sprintf("holding %s, head is %s", shortSHA(q.Held), shortSHA(head)),
			fmt.Sprintf("%d %s would not land", len(q.Dropped), plural(len(q.Dropped), "commit", "commits")))
	case q.Held != "":
		segs = append(segs, "holding the branch head")
	}
	if q.Detail != "" {
		segs = append(segs, q.Detail)
	}
	return strings.Join(segs, shipSep)
}

// plural picks one of the two spellings of a noun for n.
func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// shortSHA abbreviates a commit to the eight characters git logs it as.
func shortSHA(sha string) string {
	if len(sha) <= 8 {
		return sha
	}
	return sha[:8]
}
