package nativeapi

import (
	"context"
	"encoding/json"
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
				PullRequest *struct{ Number int }
			}
			PageInfo struct{ HasNextPage bool }
		}
	}
}

const pullFields = "id number isCrossRepository title body state reviewDecision headRefName headRefOid baseRefName baseRefOid url isDraft updatedAt author{login} mergeCommit{oid} mergeQueueEntry{state headCommit{oid}} stackEntry{position} stack{number baseRefName entries(first:100){nodes{position pullRequest{number}} pageInfo{hasNextPage}}}"

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
				if e.Position == p.StackEntry.Position-1 && e.PullRequest != nil {
					out.DependentPRNumber = e.PullRequest.Number
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
		if err := c.graphql(ctx, query, &result); err != nil {
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

type restPull struct {
	Title   string `json:"title"`
	Body    string `json:"body"`
	Number  int    `json:"number"`
	NodeID  string `json:"node_id"`
	HTMLURL string `json:"html_url"`
	State   string `json:"state"`
	Draft   bool   `json:"draft"`
	Head    struct {
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
	if len(values) > 0 {
		payload, err := c.send(ctx, method, path, values)
		if err != nil {
			return restPull{}, err
		}
		if err := json.Unmarshal(payload, &pull); err != nil {
			return restPull{}, err
		}
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

func (c *Client) linkPull(ctx context.Context, owner, repo, trunk string, pull restPull) (int, error) {
	if pull.Stack != nil && pull.Stack.Base.Ref != trunk {
		return 0, fmt.Errorf("native GitHub stack #%d targets %s instead of %s", pull.Stack.Number, pull.Stack.Base.Ref, trunk)
	}
	if pull.Base.Ref == trunk {
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
	if len(parents) != 1 {
		return 0, fmt.Errorf("PR #%d parent branch %s has %d open pull requests; native stack registration requires one", pull.Number, pull.Base.Ref, len(parents))
	}
	parent := parents[0]
	if parent.Stack != nil && parent.Stack.Base.Ref != trunk {
		return 0, fmt.Errorf("parent PR #%d belongs to native GitHub stack #%d targeting %s instead of %s", parent.Number, parent.Stack.Number, parent.Stack.Base.Ref, trunk)
	}
	if pull.Stack != nil {
		if parent.Stack == nil || parent.Stack.Number != pull.Stack.Number || pull.Stack.Position != parent.Stack.Position+1 {
			return 0, fmt.Errorf("PR #%d and parent #%d belong to different native GitHub stacks", pull.Number, parent.Number)
		}
		return pull.Stack.Number, nil
	}
	if parent.Stack == nil && parent.Base.Ref != trunk {
		if _, err := c.linkPull(ctx, owner, repo, trunk, parent); err != nil {
			return 0, err
		}
		parent, err = c.readPull(ctx, owner, repo, parent.Number)
		if err != nil {
			return 0, err
		}
		if parent.Stack == nil {
			return 0, fmt.Errorf("GitHub did not register parent PR #%d in a native stack", parent.Number)
		}
	}
	if parent.Stack != nil {
		_, err = c.post(ctx, fmt.Sprintf("%s/stacks/%d/add", path, parent.Stack.Number), map[string]any{"pull_requests": []int{pull.Number}})
		return parent.Stack.Number, err
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
