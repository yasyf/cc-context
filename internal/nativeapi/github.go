package nativeapi

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
)

type githubPull struct {
	MergeQueueEntry *struct {
		State      string
		HeadCommit *struct{ Oid string }
	}
	IsCrossRepository bool
	ID                string
	Number            int
	Title             string
	Body              string
	State             PRState
	ReviewDecision    string
	HeadRefName       string
	HeadRefOid        string
	BaseRefName       string
	BaseRefOid        string
	URL               string
	IsDraft           bool
	UpdatedAt         string
	Author            struct{ Login string }
	MergeCommit       *struct{ Oid string }
	StackEntry        *struct{ Position int }
	Stack             *struct {
		Number      int
		BaseRefName string
		Entries     struct {
			Nodes []struct {
				Position    int
				PullRequest *struct {
					Number      int
					State       PRState
					HeadRefName string
				}
			}
			PageInfo struct{ HasNextPage bool }
		}
	}
}

const pullFields = "id number isCrossRepository title body state reviewDecision headRefName headRefOid baseRefName baseRefOid url isDraft updatedAt author{login} mergeCommit{oid} mergeQueueEntry{state headCommit{oid}} stackEntry{position} stack{number baseRefName entries(first:100){nodes{position pullRequest{number state headRefName}} pageInfo{hasNextPage}}}"

func (p githubPull) info() (PullRequestInfo, error) {
	out := PullRequestInfo{PRID: p.ID, PRNumber: p.Number, Title: p.Title, Body: p.Body, State: p.State, ReviewDecision: p.ReviewDecision, HeadRefName: p.HeadRefName, BaseRefName: p.BaseRefName, URL: p.URL, IsDraft: p.IsDraft, AuthorGithubHandle: p.Author.Login, CliTrunk: p.BaseRefName, Versions: []PRVersion{{HeadSha: p.HeadRefOid, BaseSha: p.BaseRefOid, BaseName: p.BaseRefName, CreatedAt: p.UpdatedAt, AuthorGithubHandle: p.Author.Login}}}
	if p.MergeQueueEntry != nil {
		out.MergeQueueStatus = &MergeQueueStatus{InQueue: true}
		if p.MergeQueueEntry.HeadCommit != nil {
			out.MergeQueueStatus.EnqueuedCommit = p.MergeQueueEntry.HeadCommit.Oid
		}
	}
	if p.MergeCommit != nil {
		out.MergeCommitSha = p.MergeCommit.Oid
	}
	if p.Stack != nil {
		out.StackNumber = p.Stack.Number
		out.CliTrunk = p.Stack.BaseRefName
		if p.Stack.Entries.PageInfo.HasNextPage {
			return PullRequestInfo{}, fmt.Errorf("GitHub stack #%d exceeds the supported 100 pull requests", p.Stack.Number)
		}
		if p.StackEntry != nil {
			for _, e := range p.Stack.Entries.Nodes {
				if e.PullRequest == nil {
					continue
				}
				if e.Position == p.StackEntry.Position-1 {
					out.DependentPRNumber = e.PullRequest.Number
				}
				if e.Position > p.StackEntry.Position && e.PullRequest.State == PROpen {
					out.OpenAbove = append(out.OpenAbove, StackPull{Number: e.PullRequest.Number, Head: e.PullRequest.HeadRefName})
				}
			}
		}
	}
	return out, nil
}

// PullRequestInfo batches current GitHub metadata and preserves the requested lookup order.
func (c *Client) PullRequestInfo(ctx context.Context, req PullRequestInfoRequest) ([]PullRequestInfo, error) {
	out := make([]PullRequestInfo, 0, len(req.PRNumbers)+len(req.PRHeadRefNames))
	type lookup struct {
		number int
		head   string
	}
	lookups := make([]lookup, 0, cap(out))
	for _, n := range req.PRNumbers {
		lookups = append(lookups, lookup{number: n})
	}
	for _, head := range req.PRHeadRefNames {
		lookups = append(lookups, lookup{head: head})
	}
	for batch := range slices.Chunk(lookups, 40) {
		var fields strings.Builder
		for i, item := range batch {
			if item.number != 0 {
				fmt.Fprintf(&fields, "p%d:pullRequest(number:%d){%s} ", i, item.number, pullFields)
				continue
			}
			fmt.Fprintf(&fields, "h%d:pullRequests(headRefName:%s,states:OPEN,first:1){totalCount nodes{%s}} c%d:pullRequests(headRefName:%s,states:[CLOSED,MERGED],first:1,orderBy:{field:CREATED_AT,direction:DESC}){nodes{%s}} ", i, strconv.Quote(item.head), pullFields, i, strconv.Quote(item.head), pullFields)
		}
		query := fmt.Sprintf("{repository(owner:%s,name:%s){%s}}", strconv.Quote(req.RepoOwner), strconv.Quote(req.RepoName), fields.String())
		var result struct{ Repository map[string]json.RawMessage }
		if err := c.readGraphQL(ctx, req.RepoOwner, req.RepoName, query, &result); err != nil {
			return nil, err
		}
		if result.Repository == nil {
			return nil, &ResultError{Route: "pull requests", Message: "repository is not accessible"}
		}
		for i, item := range batch {
			var pull githubPull
			if item.number != 0 {
				if err := json.Unmarshal(result.Repository[fmt.Sprintf("p%d", i)], &pull); err != nil {
					return nil, err
				}
			} else {
				var open, closed struct {
					TotalCount int
					Nodes      []githubPull
				}
				if err := json.Unmarshal(result.Repository[fmt.Sprintf("h%d", i)], &open); err != nil {
					return nil, err
				}
				if open.TotalCount > 1 {
					return nil, fmt.Errorf("head branch %s has %d open pull requests; select a pull request number to disambiguate", item.head, open.TotalCount)
				}
				if len(open.Nodes) > 0 {
					pull = open.Nodes[0]
				} else {
					if err := json.Unmarshal(result.Repository[fmt.Sprintf("c%d", i)], &closed); err != nil {
						return nil, err
					}
					if len(closed.Nodes) > 0 {
						pull = closed.Nodes[0]
					}
				}
			}
			if item.head != "" && pull.IsCrossRepository {
				return nil, fmt.Errorf("head branch %s matched cross-repository PR #%d; native stack branches must belong to this repository", item.head, pull.Number)
			}
			if pull.Number == 0 {
				continue
			}
			info, err := pull.info()
			if err != nil {
				return nil, err
			}
			out = append(out, info)
		}
	}
	return out, nil
}

const headPullFields = "number state headRefOid isCrossRepository"

// HeadPullRequests reads, for each head branch, the open pull request or else
// the newest closed or merged one, carrying only the number, state and head
// commit. A branch with none is left out. It is the cheap read behind a sweep of
// thousands of branches; PullRequestInfo's stack and body fields cost an order
// of magnitude more per branch, enough to trip GitHub's secondary limit.
func (c *Client) HeadPullRequests(ctx context.Context, owner, repo string, heads []string) ([]PullRequestInfo, error) {
	var out []PullRequestInfo
	for batch := range slices.Chunk(heads, headPullBatch) {
		var fields strings.Builder
		for i, head := range batch {
			fmt.Fprintf(&fields, "o%d:pullRequests(headRefName:%s,states:OPEN,first:1){nodes{%s}} c%d:pullRequests(headRefName:%s,states:[CLOSED,MERGED],first:1,orderBy:{field:CREATED_AT,direction:DESC}){nodes{%s}} ", i, strconv.Quote(head), headPullFields, i, strconv.Quote(head), headPullFields)
		}
		query := fmt.Sprintf("{repository(owner:%s,name:%s){%s}}", strconv.Quote(owner), strconv.Quote(repo), fields.String())
		var result struct{ Repository map[string]json.RawMessage }
		if err := c.readGraphQL(ctx, owner, repo, query, &result); err != nil {
			return nil, err
		}
		if result.Repository == nil {
			return nil, &ResultError{Route: "pull requests", Message: "repository is not accessible"}
		}
		for i, head := range batch {
			pull, err := firstHeadPull(result.Repository, fmt.Sprintf("o%d", i), fmt.Sprintf("c%d", i))
			if err != nil {
				return nil, err
			}
			if pull.Number == 0 {
				continue
			}
			if pull.IsCrossRepository {
				return nil, fmt.Errorf("head branch %s matched cross-repository PR #%d; native stack branches must belong to this repository", head, pull.Number)
			}
			out = append(out, PullRequestInfo{PRNumber: pull.Number, State: pull.State, HeadRefName: head, Versions: []PRVersion{{HeadSha: pull.HeadRefOid}}})
		}
	}
	return out, nil
}

// headPullBatch bounds the branches one HeadPullRequests query names.
const headPullBatch = 100

func firstHeadPull(repository map[string]json.RawMessage, aliases ...string) (githubPull, error) {
	for _, alias := range aliases {
		var connection struct{ Nodes []githubPull }
		if err := json.Unmarshal(repository[alias], &connection); err != nil {
			return githubPull{}, err
		}
		if len(connection.Nodes) > 0 {
			return connection.Nodes[0], nil
		}
	}
	return githubPull{}, nil
}

type restPull struct {
	Title          string `json:"title"`
	Body           string `json:"body"`
	Number         int    `json:"number"`
	NodeID         string `json:"node_id"`
	HTMLURL        string `json:"html_url"`
	State          string `json:"state"`
	Draft          bool   `json:"draft"`
	Merged         bool   `json:"merged"`
	MergeCommitSHA string `json:"merge_commit_sha"`
	Head           struct {
		Repo struct {
			FullName string `json:"full_name"`
		} `json:"repo"`
		Ref string `json:"ref"`
		Sha string `json:"sha"`
	} `json:"head"`
	Base struct {
		Ref string `json:"ref"`
		Sha string `json:"sha"`
	} `json:"base"`
	Stack *struct {
		Number   int `json:"number"`
		Position int `json:"position"`
		Size     int `json:"size"`
		Base     struct {
			Ref string `json:"ref"`
		} `json:"base"`
	} `json:"stack"`
}

func (c *Client) readPull(ctx context.Context, owner, repo string, n int) (restPull, error) {
	payload, err := c.get(ctx, fmt.Sprintf("/repos/%s/%s/pulls/%d", url.PathEscape(owner), url.PathEscape(repo), n), nil)
	if err != nil {
		return restPull{}, err
	}
	var pull restPull
	err = json.Unmarshal(payload, &pull)
	return pull, err
}

// SubmitPullRequests publishes PR metadata and registers actual native stack membership.
func (c *Client) SubmitPullRequests(ctx context.Context, req SubmitRequest) ([]SubmittedPR, error) {
	if req.MergeWhenReady {
		return nil, fmt.Errorf("GitHub stack submission does not enqueue: submit the reviewed stack through Trunk")
	}
	result := make([]SubmittedPR, 0, len(req.PRs))
	for _, entry := range req.PRs {
		pull, err := c.submitPull(ctx, req.RepoOwner, req.RepoName, entry)
		if pull.Number != 0 {
			posted := SubmittedPR{Head: entry.Head, PRNumber: pull.Number, PRID: pull.NodeID, PRURL: pull.HTMLURL, Status: "updated"}
			if entry.Action == SubmitCreate {
				posted.Status = "created"
			}
			if entry.Action == SubmitUpdate && pull.Number != entry.PRNumber {
				posted.Status = "moved"
			}
			if err == nil && entry.Base != "" && pull.Base.Ref != entry.Base && pull.Stack != nil {
				posted.Warnings = append(posted.Warnings, fmt.Sprintf("kept PR #%d on %s, not %s: native GitHub stack #%d still holds another open pull request and sets its base; ccx vcs stack submit --move %s opens a new pull request for it on %s and closes #%d", pull.Number, pull.Base.Ref, entry.Base, pull.Stack.Number, entry.Head, entry.Base, pull.Number))
			}
			result = append(result, posted)
		}
		if err != nil {
			return nil, &SubmitError{Cause: err, Submitted: result, Failed: []BranchSubmitError{{Head: entry.Head, Message: err.Error()}}}
		}
		stackNumber, err := c.linkPull(ctx, req.RepoOwner, req.RepoName, req.TrunkBranchName, pull)
		if err != nil {
			return nil, &SubmitError{Cause: err, Submitted: result, Failed: []BranchSubmitError{{Head: entry.Head, Message: err.Error()}}}
		}
		result[len(result)-1].StackNumber = stackNumber
		if err := c.requirePublishedHead(ctx, req.RepoOwner, req.RepoName, entry); err != nil {
			return nil, &SubmitError{Cause: err, Submitted: result, Failed: []BranchSubmitError{{Head: entry.Head, Message: err.Error()}}}
		}
	}
	return result, nil
}

func (c *Client) requirePublishedHead(ctx context.Context, owner, repo string, entry SubmitPR) error {
	path := fmt.Sprintf("/repos/%s/%s/git/ref/heads/%s", url.PathEscape(owner), url.PathEscape(repo), url.PathEscape(entry.Head))
	payload, err := c.get(ctx, path, nil)
	if err != nil {
		return err
	}
	var ref struct {
		Object struct {
			Sha string `json:"sha"`
		} `json:"object"`
	}
	if err := json.Unmarshal(payload, &ref); err != nil {
		return err
	}
	if ref.Object.Sha != entry.HeadSha {
		return fmt.Errorf("git ref %s reports commit %s instead of published %s", entry.Head, ref.Object.Sha, entry.HeadSha)
	}
	return nil
}

func (c *Client) submitPull(ctx context.Context, owner, repo string, entry SubmitPR) (restPull, error) {
	path := fmt.Sprintf("/repos/%s/%s/pulls", url.PathEscape(owner), url.PathEscape(repo))
	values := map[string]any{}
	var pull restPull
	method := http.MethodPost
	switch entry.Action {
	case SubmitCreate:
		values["base"] = entry.Base
		values["head"], values["title"], values["body"] = entry.Head, entry.Title, entry.Body
		if entry.Draft != nil {
			values["draft"] = *entry.Draft
		}
	case SubmitUpdate:
		current, err := c.readPull(ctx, owner, repo, entry.PRNumber)
		if err != nil {
			return restPull{}, err
		}
		if current.Head.Ref != entry.Head || !strings.EqualFold(current.Head.Repo.FullName, owner+"/"+repo) {
			return restPull{}, fmt.Errorf("PR #%d uses head %s:%s instead of %s/%s:%s", entry.PRNumber, current.Head.Repo.FullName, current.Head.Ref, owner, repo, entry.Head)
		}
		if current.State != "open" {
			return restPull{}, fmt.Errorf("PR #%d is %s", entry.PRNumber, current.State)
		}
		pull = current
		if entry.Base != "" && entry.Base != current.Base.Ref {
			values["base"] = entry.Base
		}
		path += "/" + strconv.Itoa(entry.PRNumber)
		method = http.MethodPatch
		if entry.Title != "" && entry.Title != current.Title {
			values["title"] = entry.Title
		}
		if entry.Body != "" && entry.Body != current.Body {
			values["body"] = entry.Body
		}
	default:
		return restPull{}, fmt.Errorf("unknown submit action %q", entry.Action)
	}
	if err := c.requirePublishedHead(ctx, owner, repo, entry); err != nil {
		return restPull{}, err
	}
	var above []int
	if _, retarget := values["base"]; retarget && entry.Action == SubmitUpdate && pull.Stack != nil {
		left, upper, unstacked, err := c.leaveDeadStack(ctx, owner, repo, pull, entry.Base)
		if err != nil {
			return restPull{}, err
		}
		pull, above = left, upper
		if !unstacked && entry.Move {
			moved, err := c.movePull(ctx, owner, repo, pull, entry)
			if err != nil {
				return moved, err
			}
			pull, values = moved, nil
		} else if !unstacked {
			delete(values, "base")
		}
	}
	if len(values) > 0 {
		payload, err := c.send(ctx, method, path, values)
		if err != nil {
			return restPull{}, err
		}
		if err := json.Unmarshal(payload, &pull); err != nil {
			return restPull{}, err
		}
	}
	if len(above) > 0 {
		relinked, err := c.restackAbove(ctx, owner, repo, pull, above)
		if err != nil {
			return pull, err
		}
		pull = relinked
	}
	if pull.Head.Ref != entry.Head || !strings.EqualFold(pull.Head.Repo.FullName, owner+"/"+repo) {
		return pull, fmt.Errorf("PR #%d uses head %s:%s instead of %s/%s:%s", pull.Number, pull.Head.Repo.FullName, pull.Head.Ref, owner, repo, entry.Head)
	}
	if pull.State != "open" {
		return pull, fmt.Errorf("PR #%d is %s after metadata update", pull.Number, pull.State)
	}
	if entry.Draft != nil && pull.Draft != *entry.Draft {
		mutation := "markPullRequestReadyForReview"
		if *entry.Draft {
			mutation = "convertPullRequestToDraft"
		}
		query := fmt.Sprintf("mutation{%s(input:{pullRequestId:%s}){pullRequest{id}}}", mutation, strconv.Quote(pull.NodeID))
		if err := c.graphql(ctx, query, &struct{ Data any }{}); err != nil {
			return pull, err
		}
	}
	if len(entry.Reviewers) > 0 {
		if _, err := c.post(ctx, fmt.Sprintf("/repos/%s/%s/pulls/%d/requested_reviewers", url.PathEscape(owner), url.PathEscape(repo), pull.Number), map[string]any{"reviewers": entry.Reviewers}); err != nil {
			return pull, err
		}
	}
	return pull, nil
}

func (c *Client) movePull(ctx context.Context, owner, repo string, pull restPull, entry SubmitPR) (restPull, error) {
	path := fmt.Sprintf("/repos/%s/%s", url.PathEscape(owner), url.PathEscape(repo))
	title, body := cmp.Or(entry.Title, pull.Title), cmp.Or(entry.Body, pull.Body)
	payload, err := c.post(ctx, path+"/pulls", map[string]any{"head": entry.Head, "base": entry.Base, "title": title, "body": body, "draft": pull.Draft})
	if err != nil {
		return restPull{}, fmt.Errorf("open a pull request for %s on %s to move PR #%d out of native GitHub stack #%d: %w", entry.Head, entry.Base, pull.Number, pull.Stack.Number, err)
	}
	var moved restPull
	if err := json.Unmarshal(payload, &moved); err != nil {
		return restPull{}, err
	}
	note := fmt.Sprintf("Moved to #%d: native GitHub stack #%d holds this pull request on %s, and %s now sits on %s.", moved.Number, pull.Stack.Number, pull.Base.Ref, entry.Head, entry.Base)
	if _, err := c.post(ctx, fmt.Sprintf("%s/issues/%d/comments", path, pull.Number), map[string]any{"body": note}); err != nil {
		return moved, fmt.Errorf("opened PR #%d for %s on %s, but commenting on PR #%d failed, so it stays open: %w", moved.Number, entry.Head, entry.Base, pull.Number, err)
	}
	if _, err := c.send(ctx, http.MethodPatch, fmt.Sprintf("%s/pulls/%d", path, pull.Number), map[string]any{"state": "closed"}); err != nil {
		return moved, fmt.Errorf("opened PR #%d for %s on %s, but closing PR #%d failed: %w", moved.Number, entry.Head, entry.Base, pull.Number, err)
	}
	return moved, nil
}

func (c *Client) linkPull(ctx context.Context, owner, repo, trunk string, pull restPull) (int, error) {
	if pull.Base.Ref == trunk || pull.Stack != nil && pull.Stack.Position == 1 {
		if pull.Stack != nil {
			return pull.Stack.Number, nil
		}
		return 0, nil
	}
	path := fmt.Sprintf("/repos/%s/%s", url.PathEscape(owner), url.PathEscape(repo))
	payload, err := c.get(ctx, path+"/pulls", url.Values{"head": {owner + ":" + pull.Base.Ref}, "state": {"open"}, "per_page": {"100"}})
	if err != nil {
		return 0, err
	}
	var parents []restPull
	if err := json.Unmarshal(payload, &parents); err != nil {
		return 0, err
	}
	if len(parents) == 0 {
		payload, err := c.get(ctx, path, nil)
		if err != nil {
			return 0, err
		}
		var repository struct {
			DefaultBranch string `json:"default_branch"`
		}
		if err := json.Unmarshal(payload, &repository); err != nil {
			return 0, err
		}
		if repository.DefaultBranch == pull.Base.Ref {
			return 0, nil
		}
		remedy := fmt.Sprintf("ccx vcs stack submit --parent %s=%s", pull.Head.Ref, trunk)
		if pull.Stack != nil {
			remedy += " --move " + pull.Head.Ref
		}
		return 0, fmt.Errorf("PR #%d parent branch %s has no open pull request, so it cannot join a native stack; move it onto trunk with %s", pull.Number, pull.Base.Ref, remedy)
	}
	if len(parents) != 1 {
		return 0, fmt.Errorf("PR #%d parent branch %s has %d open pull requests; native stack registration requires one", pull.Number, pull.Base.Ref, len(parents))
	}
	parent := parents[0]
	if pull.Stack != nil {
		if parent.Stack == nil || parent.Stack.Number != pull.Stack.Number {
			return 0, fmt.Errorf("PR #%d and parent #%d belong to different native GitHub stacks", pull.Number, parent.Number)
		}
		if pull.Stack.Position == parent.Stack.Position+1 {
			return pull.Stack.Number, nil
		}
		number, relinked, err := c.relinkPastClosed(ctx, owner, repo, pull.Stack.Number, parent.Number, pull.Number)
		if err == nil && !relinked {
			err = fmt.Errorf("PR #%d sits in native GitHub stack #%d with open pull requests between it and parent #%d", pull.Number, pull.Stack.Number, parent.Number)
		}
		return number, err
	}
	if parent.Stack == nil && parent.Base.Ref != trunk {
		if _, err := c.linkPull(ctx, owner, repo, trunk, parent); err != nil {
			return 0, err
		}
		parent, err = c.readPull(ctx, owner, repo, parent.Number)
		if err != nil {
			return 0, err
		}
	}
	if parent.Stack != nil && parent.Stack.Position == parent.Stack.Size {
		_, err = c.post(ctx, fmt.Sprintf("%s/stacks/%d/add", path, parent.Stack.Number), map[string]any{"pull_requests": []int{pull.Number}})
		return parent.Stack.Number, err
	}
	if parent.Stack != nil {
		number, _, err := c.relinkPastClosed(ctx, owner, repo, parent.Stack.Number, parent.Number, pull.Number)
		return number, err
	}
	payload, err = c.post(ctx, path+"/stacks", map[string]any{"pull_requests": []int{parent.Number, pull.Number}})
	if err != nil {
		return 0, err
	}
	var created struct {
		Number int `json:"number"`
	}
	if err := json.Unmarshal(payload, &created); err != nil {
		return 0, err
	}
	if created.Number == 0 {
		return 0, fmt.Errorf("GitHub created a native stack without its stack number")
	}
	return created.Number, nil
}

type nativeStack struct {
	Number       int               `json:"number"`
	PullRequests []nativeStackPull `json:"pull_requests"`
}

type nativeStackPull struct {
	Number   int    `json:"number"`
	State    string `json:"state"`
	MergedAt string `json:"merged_at"`
}

func (s nativeStack) numbers() []int {
	numbers := make([]int, len(s.PullRequests))
	for i, pr := range s.PullRequests {
		numbers[i] = pr.Number
	}
	return numbers
}

func (c *Client) leaveDeadStack(ctx context.Context, owner, repo string, pull restPull, base string) (restPull, []int, bool, error) {
	path := fmt.Sprintf("/repos/%s/%s/stacks/%d", url.PathEscape(owner), url.PathEscape(repo), pull.Stack.Number)
	payload, err := c.get(ctx, path, nil)
	if err != nil {
		return restPull{}, nil, false, err
	}
	var stack nativeStack
	if err := json.Unmarshal(payload, &stack); err != nil {
		return restPull{}, nil, false, err
	}
	at := slices.Index(stack.numbers(), pull.Number)
	if at < 0 {
		return restPull{}, nil, false, fmt.Errorf("native GitHub stack #%d does not list PR #%d, which names it as its stack", pull.Stack.Number, pull.Number)
	}
	var above []int
	for _, pr := range stack.PullRequests[at+1:] {
		if pr.State != "closed" {
			above = append(above, pr.Number)
		}
	}
	if slices.ContainsFunc(stack.PullRequests[:at], func(pr nativeStackPull) bool { return pr.State != "closed" }) || len(above) > 0 && base != pull.Stack.Base.Ref {
		return pull, nil, false, nil
	}
	if _, err := c.do(ctx, http.MethodPost, path+"/unstack", nil); err != nil {
		return restPull{}, nil, false, fmt.Errorf("unstack native GitHub stack #%d, whose pull requests below PR #%d are all merged or closed, to retarget #%d: %w", pull.Stack.Number, pull.Number, pull.Number, err)
	}
	left, err := c.readPull(ctx, owner, repo, pull.Number)
	if err != nil {
		return restPull{}, nil, false, err
	}
	if left.Stack != nil {
		return restPull{}, nil, false, fmt.Errorf("PR #%d is still in native GitHub stack #%d after unstacking #%d", pull.Number, left.Stack.Number, pull.Stack.Number)
	}
	return left, above, true, nil
}

func (c *Client) restackAbove(ctx context.Context, owner, repo string, pull restPull, above []int) (restPull, error) {
	desired := append([]int{pull.Number}, above...)
	if _, err := c.post(ctx, fmt.Sprintf("/repos/%s/%s/stacks", url.PathEscape(owner), url.PathEscape(repo)), map[string]any{"pull_requests": desired}); err != nil {
		return pull, fmt.Errorf("moved PR #%d onto %s, but relinking %v as one native GitHub stack failed, so the pull requests above it are unstacked until ccx vcs stack submit links them again: %w", pull.Number, pull.Base.Ref, desired, err)
	}
	return c.readPull(ctx, owner, repo, pull.Number)
}

func (c *Client) relinkPastClosed(ctx context.Context, owner, repo string, stackNumber, parent, pull int) (int, bool, error) {
	stacks := fmt.Sprintf("/repos/%s/%s/stacks", url.PathEscape(owner), url.PathEscape(repo))
	path := fmt.Sprintf("%s/%d", stacks, stackNumber)
	payload, err := c.get(ctx, path, nil)
	if err != nil {
		return 0, false, err
	}
	var stack nativeStack
	if err := json.Unmarshal(payload, &stack); err != nil {
		return 0, false, err
	}
	numbers := stack.numbers()
	parentAt, pullAt := slices.Index(numbers, parent), slices.Index(numbers, pull)
	if pullAt < 0 {
		pullAt = len(numbers)
	}
	if parentAt < 0 || pullAt <= parentAt+1 {
		return 0, false, nil
	}
	closed := func(i int) bool {
		return stack.PullRequests[i].State == "closed"
	}
	for i := parentAt + 1; i < pullAt; i++ {
		if !closed(i) {
			return 0, false, nil
		}
	}
	var desired []int
	for i, number := range numbers {
		if !closed(i) {
			desired = append(desired, number)
		}
		if i == parentAt && pullAt == len(numbers) {
			desired = append(desired, pull)
		}
	}
	if _, err := c.do(ctx, http.MethodPost, path+"/unstack", nil); err != nil {
		return 0, false, fmt.Errorf("unstack native GitHub stack #%d to drop its merged and closed pull requests: %w", stackNumber, err)
	}
	payload, err = c.post(ctx, stacks, map[string]any{"pull_requests": desired})
	if err != nil {
		return 0, false, fmt.Errorf("relink %v as one native GitHub stack after dropping its merged and closed pull requests: %w", desired, err)
	}
	var relinked nativeStack
	if err := json.Unmarshal(payload, &relinked); err != nil {
		return 0, false, err
	}
	if relinked.Number == 0 {
		return 0, false, fmt.Errorf("GitHub relinked %v without its stack number", desired)
	}
	return relinked.Number, true, nil
}

// BranchHistory reports whether branch exists on GitHub and whether any pull request ever used it as its head; it reads the repository first, so a 404 from a token without access is an error rather than a missing branch.
func (c *Client) BranchHistory(ctx context.Context, owner, repo, branch string) (onOrigin, headed bool, err error) {
	path := fmt.Sprintf("/repos/%s/%s", url.PathEscape(owner), url.PathEscape(repo))
	if _, err := c.get(ctx, path, nil); err != nil {
		return false, false, err
	}
	if _, err := c.get(ctx, path+"/branches/"+url.PathEscape(branch), nil); err != nil {
		var status *StatusError
		if errors.As(err, &status) && status.Status == http.StatusNotFound {
			return false, false, nil
		}
		return false, false, err
	}
	payload, err := c.get(ctx, path+"/pulls", url.Values{"head": {owner + ":" + branch}, "state": {"all"}, "per_page": {"1"}})
	if err != nil {
		return true, false, err
	}
	var pulls []json.RawMessage
	if err := json.Unmarshal(payload, &pulls); err != nil {
		return true, false, err
	}
	return true, len(pulls) > 0, nil
}
