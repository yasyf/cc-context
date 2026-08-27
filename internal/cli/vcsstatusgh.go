package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/yasyf/cc-context/internal/render"
)

// statusPRFields is the selection one branch's pull request is read through: it
// carries prLandingFields, so status tells a queue merge from an abandonment
// the same way info and reviews do, plus everything a landing verdict needs.
const statusPRFields = "number url body isDraft baseRefName headRefOid mergeable mergeStateStatus reviewDecision " +
	prLandingFields + " " +
	"labels(first: 20) { nodes { name } } " +
	"latestOpinionatedReviews(first: 20) { nodes { state author { login __typename } commit { oid } } } " +
	"history: commits(last: 50) { nodes { commit { oid committedDate messageHeadline } } } " +
	"checks: commits(last: 1) { nodes { commit { statusCheckRollup { state contexts(first: 100) { nodes { __typename " +
	"... on CheckRun { name conclusion status } ... on StatusContext { context state } } } } } } } " +
	"events: timelineItems(last: 100, itemTypes: [LABELED_EVENT, UNLABELED_EVENT, HEAD_REF_FORCE_PUSHED_EVENT]) " +
	"{ nodes { __typename ... on LabeledEvent { createdAt label { name } actor { login } } " +
	"... on UnlabeledEvent { createdAt label { name } actor { login } } " +
	"... on HeadRefForcePushedEvent { createdAt beforeCommit { oid } } } } " +
	"comments(last: 40) { nodes { id author { login } } }"

// statusProtectionFields asks for the base branch's required checks. A viewer
// without admin is answered with an empty list rather than an error, so it
// costs nothing to ask and its emptiness proves nothing.
const statusProtectionFields = "branchProtectionRules(first: 20) { nodes { pattern requiredStatusChecks { context } } }"

// statusProtectionRule is one branch protection rule and the contexts it makes
// required.
type statusProtectionRule struct {
	Pattern              string `json:"pattern"`
	RequiredStatusChecks []struct {
		Context string `json:"context"`
	} `json:"requiredStatusChecks"`
}

// statusActor is a GraphQL Actor, whose __typename separates a Bot from a User.
type statusActor struct {
	Login    string `json:"login"`
	Typename string `json:"__typename"`
}

// statusContextNode is one entry of a check rollup, which is a CheckRun or a
// commit status and spells its name and verdict differently in each case.
type statusContextNode struct {
	Typename   string `json:"__typename"`
	Name       string `json:"name"`
	Conclusion string `json:"conclusion"`
	Status     string `json:"status"`
	Context    string `json:"context"`
	State      string `json:"state"`
}

// statusEventNode is one timeline event: a merge label going on or off, or a
// force push whose beforeCommit is the head it replaced.
type statusEventNode struct {
	Typename     string    `json:"__typename"`
	CreatedAt    time.Time `json:"createdAt"`
	Label        *struct{ Name string } `json:"label"`
	Actor        *statusActor           `json:"actor"`
	BeforeCommit *struct {
		OID string `json:"oid"`
	} `json:"beforeCommit"`
}

// statusRollup is the head commit's aggregate check state and the contexts
// behind it.
type statusRollup struct {
	State    string `json:"state"`
	Contexts struct {
		Nodes []statusContextNode `json:"nodes"`
	} `json:"contexts"`
}

// statusPRNode is one pull request as GitHub answers statusPRFields.
type statusPRNode struct {
	Number           int    `json:"number"`
	URL              string `json:"url"`
	Body             string `json:"body"`
	IsDraft          bool   `json:"isDraft"`
	BaseRefName      string `json:"baseRefName"`
	HeadRefOid       string `json:"headRefOid"`
	Mergeable        string `json:"mergeable"`
	MergeStateStatus string `json:"mergeStateStatus"`
	ReviewDecision   string `json:"reviewDecision"`
	Labels           struct {
		Nodes []struct {
			Name string `json:"name"`
		} `json:"nodes"`
	} `json:"labels"`
	LatestOpinionatedReviews struct {
		Nodes []struct {
			State  string       `json:"state"`
			Author *statusActor `json:"author"`
			Commit *struct {
				OID string `json:"oid"`
			} `json:"commit"`
		} `json:"nodes"`
	} `json:"latestOpinionatedReviews"`
	History struct {
		Nodes []struct {
			Commit struct {
				OID             string    `json:"oid"`
				CommittedDate   time.Time `json:"committedDate"`
				MessageHeadline string    `json:"messageHeadline"`
			} `json:"commit"`
		} `json:"nodes"`
	} `json:"history"`
	Checks struct {
		Nodes []struct {
			Commit struct {
				StatusCheckRollup *statusRollup `json:"statusCheckRollup"`
			} `json:"commit"`
		} `json:"nodes"`
	} `json:"checks"`
	Events struct {
		Nodes []statusEventNode `json:"nodes"`
	} `json:"events"`
	Comments struct {
		Nodes []struct {
			ID     string       `json:"id"`
			Author *statusActor `json:"author"`
		} `json:"nodes"`
	} `json:"comments"`
	prLanding
}

// statusResolvePRs fills every branch's pull request from GitHub. It swallows
// nothing silently: a repository nobody can ask about, a gh that is not there,
// and a query GitHub refused each land in PRError, because the local half of
// the report is still worth printing.
func statusResolvePRs(ctx context.Context, l lane, st *vcsStatus) {
	if st.Repo == "" || st.PRError != "" {
		return
	}
	if !statusGH() {
		st.PRError = "gh is not on PATH — install it to report pull requests"
		return
	}
	branches := make([]string, 0, len(st.Branches))
	for _, b := range st.Branches {
		branches = append(branches, b.Name)
	}
	resp, err := statusQueryPRs(ctx, l, branches)
	if err != nil {
		st.PRError = err.Error()
		return
	}
	if statusAnyUnknown(resp, branches) {
		select {
		case <-ctx.Done():
			return
		case <-time.After(statusMergeableRetry):
		}
		if second, err := statusQueryPRs(ctx, l, branches); err == nil {
			resp = second
		}
	}
	nodes := statusNodes(resp, branches)
	activity := statusActivity(ctx, l, nodes)
	drafts := statusDrafts(ctx, l, activity)
	rules := resp.rules()
	for i, node := range nodes {
		if node == nil {
			continue
		}
		required := statusRequired(rules, node.BaseRefName)
		pr := statusBuildPR(*node, l.gt, activity[node.Number], drafts)
		for j, check := range pr.Checks {
			pr.Checks[j].Required = slices.Contains(required, check.Name)
		}
		st.Branches[i].PR = pr
		st.Required = statusMerge(st.Required, required)
	}
}

// statusPRResponse is the batched query's payload: one aliased pull-request
// connection per branch, plus the base branch's protection rules.
type statusPRResponse struct {
	Data struct {
		Repository map[string]json.RawMessage `json:"repository"`
	} `json:"data"`
}

// rules decodes the protection rules out of the repository payload they share
// with the aliased branches.
func (r statusPRResponse) rules() []statusProtectionRule {
	raw, ok := r.Data.Repository["branchProtectionRules"]
	if !ok {
		return nil
	}
	var decoded struct {
		Nodes []statusProtectionRule `json:"nodes"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return nil
	}
	return decoded.Nodes
}

// statusNodes lines the answer's aliased connections back up with the branches
// that named them, leaving a branch with no pull request nil.
func statusNodes(r statusPRResponse, branches []string) []*statusPRNode {
	nodes := make([]*statusPRNode, len(branches))
	for i := range branches {
		raw, ok := r.Data.Repository[downstackPRAlias(i)]
		if !ok {
			continue
		}
		var decoded struct {
			Nodes []statusPRNode `json:"nodes"`
		}
		if err := json.Unmarshal(raw, &decoded); err != nil || len(decoded.Nodes) == 0 {
			continue
		}
		nodes[i] = &decoded.Nodes[0]
	}
	return nodes
}

// statusAnyUnknown reports whether an open pull request came back without a
// computed mergeability, which is the one answer a second ask can improve.
func statusAnyUnknown(r statusPRResponse, branches []string) bool {
	for _, node := range statusNodes(r, branches) {
		if node != nil && node.State == "OPEN" && node.Mergeable == statusUnknown {
			return true
		}
	}
	return false
}

// statusQueryPRs runs the batched query, one round trip for the whole stack.
func statusQueryPRs(ctx context.Context, l lane, branches []string) (statusPRResponse, error) {
	argv := make([]string, 0, 8+2*len(branches))
	argv = append(argv, "api", "graphql", "-F", "owner={owner}", "-F", "repo={repo}")
	for i, branch := range branches {
		argv = append(argv, "-f", downstackPRAlias(i)+"="+branch)
	}
	argv = append(argv, "-f", "query="+statusPRQuery(len(branches)))
	out, err := render.RunCLI(ctx, l.dir(), "gh", argv)
	if err != nil {
		return statusPRResponse{}, fmt.Errorf("status: gh api graphql: %w", err)
	}
	var resp statusPRResponse
	if err := json.Unmarshal([]byte(out), &resp); err != nil {
		return statusPRResponse{}, fmt.Errorf("status: parse gh api graphql: %w", err)
	}
	return resp, nil
}

// statusPRQuery renders one aliased pullRequests field per branch, sharing one
// fragment so the wire text stays flat in the stack's size. It orders and
// filters exactly as downstackPRQuery does, so both resolve the same pull
// request for a branch that carries more than one.
func statusPRQuery(n int) string {
	decls := make([]string, 0, n+2)
	decls = append(decls, "$owner: String!", "$repo: String!")
	var fields strings.Builder
	for i := range n {
		alias := downstackPRAlias(i)
		decls = append(decls, "$"+alias+": String!")
		fmt.Fprintf(&fields, "    %s: pullRequests(headRefName: $%s, first: 1, orderBy: {field: CREATED_AT, direction: DESC})"+
			" { nodes { ...prStatus } }\n", alias, alias)
	}
	return fmt.Sprintf("query(%s) {\n  repository(owner: $owner, name: $repo) {\n    %s\n%s  }\n}\nfragment prStatus on PullRequest { %s }",
		strings.Join(decls, ", "), statusProtectionFields, fields.String(), statusPRFields)
}

// statusActivity fetches the Graphite merge queue's activity comment for every
// pull request that has one, keyed by pull request number. The first pass asks
// only for comment ids and authors, so a pull request's other bot comments —
// which run to kilobytes each — never cross the wire.
func statusActivity(ctx context.Context, l lane, nodes []*statusPRNode) map[int]string {
	ids := map[string]int{}
	var order []string
	for _, node := range nodes {
		if node == nil {
			continue
		}
		for _, c := range node.Comments.Nodes {
			if c.Author != nil && c.Author.Login == graphiteQueueActor {
				ids[c.ID], order = node.Number, append(order, c.ID)
			}
		}
	}
	if len(order) == 0 {
		return nil
	}
	argv := []string{"api", "graphql"}
	for i, id := range order {
		argv = append(argv, "-f", statusCommentAlias(i)+"="+id)
	}
	argv = append(argv, "-f", "query="+statusCommentQuery(len(order)))
	out, err := render.RunCLI(ctx, l.dir(), "gh", argv)
	if err != nil {
		return nil
	}
	var resp struct {
		Data struct {
			Nodes []struct {
				ID   string `json:"id"`
				Body string `json:"body"`
			} `json:"nodes"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &resp); err != nil {
		return nil
	}
	activity := map[int]string{}
	for _, node := range resp.Data.Nodes {
		if number, ok := ids[node.ID]; ok {
			activity[number] = node.Body
		}
	}
	return activity
}

// statusCommentAlias names one comment id's variable in the batched fetch.
func statusCommentAlias(i int) string { return fmt.Sprintf("c%d", i) }

// statusCommentQuery fetches comment bodies by global id, which is the only
// way to read one comment without paging every comment on the pull request.
func statusCommentQuery(n int) string {
	decls := make([]string, 0, n)
	vars := make([]string, 0, n)
	for i := range n {
		decls = append(decls, "$"+statusCommentAlias(i)+": ID!")
		vars = append(vars, "$"+statusCommentAlias(i))
	}
	return fmt.Sprintf("query(%s) { nodes(ids: [%s]) { ... on IssueComment { id body } } }",
		strings.Join(decls, ", "), strings.Join(vars, ", "))
}

// statusDrafts dates every merge-queue draft the activity comments name. The
// queue cuts a draft from the branch as it stands, so a draft's birth is the
// most recent moment the queue looked — later than the label, when a re-formed
// group picked up a push.
func statusDrafts(ctx context.Context, l lane, activity map[int]string) map[int]time.Time {
	numbers := map[int]bool{}
	var order []int
	for _, body := range activity {
		for _, n := range mqDraftNumbers(body) {
			if !numbers[n] {
				numbers[n], order = true, append(order, n)
			}
		}
	}
	if len(order) == 0 {
		return nil
	}
	argv := []string{"api", "graphql", "-F", "owner={owner}", "-F", "repo={repo}"}
	for i, n := range order {
		argv = append(argv, "-F", fmt.Sprintf("%s=%d", statusDraftAlias(i), n))
	}
	argv = append(argv, "-f", "query="+statusDraftQuery(len(order)))
	out, err := render.RunCLI(ctx, l.dir(), "gh", argv)
	if err != nil {
		return nil
	}
	var resp struct {
		Data struct {
			Repository map[string]*struct {
				Number    int       `json:"number"`
				CreatedAt time.Time `json:"createdAt"`
			} `json:"repository"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &resp); err != nil {
		return nil
	}
	drafts := map[int]time.Time{}
	for _, draft := range resp.Data.Repository {
		if draft != nil {
			drafts[draft.Number] = draft.CreatedAt
		}
	}
	return drafts
}

// statusDraftAlias names one draft's field in the batched fetch.
func statusDraftAlias(i int) string { return fmt.Sprintf("d%d", i) }

func statusDraftQuery(n int) string {
	decls := make([]string, 0, n+2)
	decls = append(decls, "$owner: String!", "$repo: String!")
	var fields strings.Builder
	for i := range n {
		alias := statusDraftAlias(i)
		decls = append(decls, "$"+alias+": Int!")
		fmt.Fprintf(&fields, "    %s: pullRequest(number: $%s) { number createdAt }\n", alias, alias)
	}
	return fmt.Sprintf("query(%s) {\n  repository(owner: $owner, name: $repo) {\n%s  }\n}", strings.Join(decls, ", "), fields.String())
}

// statusBuildPR turns one answered pull request into the report's view of it.
func statusBuildPR(node statusPRNode, gt bool, activity string, drafts map[int]time.Time) *statusPR {
	pr := &statusPR{
		Number:         node.Number,
		URL:            node.URL,
		State:          node.State,
		Merged:         node.landed(gt),
		Draft:          node.IsDraft,
		HasBody:        strings.TrimSpace(node.Body) != "",
		Head:           node.HeadRefOid,
		Base:           node.BaseRefName,
		Mergeable:      node.Mergeable,
		MergeState:     node.MergeStateStatus,
		ReviewDecision: node.ReviewDecision,
		Labels:         statusLabels(node),
		Checks:         statusChecks(statusRollupOf(node)),
		Reviews:        statusReviews(node),
	}
	if rollup := statusRollupOf(node); rollup != nil {
		pr.ChecksState = rollup.State
	}
	pr.Queue = mergeQueue(mqInput{
		Head:     node.HeadRefOid,
		Landed:   pr.Merged,
		Labels:   pr.Labels,
		Commits:  statusCommits(node),
		Pushes:   statusPushes(node),
		Events:   statusLabelEvents(node),
		Activity: activity,
		Drafts:   statusDraftsFor(activity, drafts),
	})
	return pr
}

// statusDraftsFor narrows the dated drafts to the ones this pull request's own
// activity comment names, so one branch's queue history cannot date another's.
func statusDraftsFor(activity string, drafts map[int]time.Time) map[int]time.Time {
	mine := map[int]time.Time{}
	for _, n := range mqDraftNumbers(activity) {
		if born, ok := drafts[n]; ok {
			mine[n] = born
		}
	}
	return mine
}

// statusRollupOf reads the head commit's rollup, which is absent until a check
// has reported on it.
func statusRollupOf(node statusPRNode) *statusRollup {
	if len(node.Checks.Nodes) == 0 {
		return nil
	}
	return node.Checks.Nodes[0].Commit.StatusCheckRollup
}

func statusLabels(node statusPRNode) []string {
	labels := make([]string, 0, len(node.Labels.Nodes))
	for _, l := range node.Labels.Nodes {
		labels = append(labels, l.Name)
	}
	return labels
}

// statusChecks flattens the head commit's rollup, reading a CheckRun's verdict
// off its conclusion and an unfinished one off its status. A re-run is a fresh
// check run under the same name, so the newest entry of each name wins — the
// rollup carries every attempt and only the last one is the verdict.
func statusChecks(rollup *statusRollup) []statusCheck {
	if rollup == nil {
		return nil
	}
	var checks []statusCheck
	at := map[string]int{}
	for _, c := range rollup.Contexts.Nodes {
		check := statusCheck{Name: c.Context, State: c.State}
		if c.Typename == "CheckRun" {
			check = statusCheck{Name: c.Name, State: c.Conclusion}
			if c.Conclusion == "" {
				check.State = c.Status
			}
		}
		if i, seen := at[check.Name]; seen {
			checks[i] = check
			continue
		}
		at[check.Name], checks = len(checks), append(checks, check)
	}
	return checks
}

// statusReviews reads each reviewer's standing verdict and whether it still
// speaks for the head.
func statusReviews(node statusPRNode) []statusReview {
	nodes := node.LatestOpinionatedReviews.Nodes
	reviews := make([]statusReview, 0, len(nodes))
	for _, r := range nodes {
		review := statusReview{State: r.State}
		if r.Author != nil {
			review.Author, review.Bot = r.Author.Login, r.Author.Typename == "Bot"
		}
		if r.Commit != nil {
			review.Commit = r.Commit.OID
			review.Stale = r.Commit.OID != node.HeadRefOid
		}
		reviews = append(reviews, review)
	}
	return reviews
}

func statusCommits(node statusPRNode) []mqCommit {
	commits := make([]mqCommit, 0, len(node.History.Nodes))
	for _, c := range node.History.Nodes {
		commits = append(commits, mqCommit{OID: c.Commit.OID, Subject: c.Commit.MessageHeadline, At: c.Commit.CommittedDate})
	}
	return commits
}

// statusPushes lists the force pushes in the order they happened, which is the
// order the timeline returns them.
func statusPushes(node statusPRNode) []mqForcePush {
	var pushes []mqForcePush
	for _, e := range node.Events.Nodes {
		if e.Typename == "HeadRefForcePushedEvent" && e.BeforeCommit != nil {
			pushes = append(pushes, mqForcePush{At: e.CreatedAt, Before: e.BeforeCommit.OID})
		}
	}
	return pushes
}

func statusLabelEvents(node statusPRNode) []mqLabelEvent {
	var events []mqLabelEvent
	for _, e := range node.Events.Nodes {
		if e.Label == nil {
			continue
		}
		event := mqLabelEvent{At: e.CreatedAt, Name: e.Label.Name, Added: e.Typename == "LabeledEvent"}
		if e.Actor != nil {
			event.Actor = e.Actor.Login
		}
		events = append(events, event)
	}
	return events
}

// statusMerge appends the names into lacks, keeping the report's required-check
// list free of the duplicates a stack sharing one base produces.
func statusMerge(into, add []string) []string {
	for _, name := range add {
		if !slices.Contains(into, name) {
			into = append(into, name)
		}
	}
	return into
}
