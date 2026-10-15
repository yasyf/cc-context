package prstate

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/yasyf/cc-context/internal/ghapi"
)

const restConcurrency = 8

type restPull struct {
	Number         int        `json:"number"`
	State          string     `json:"state"`
	Title          string     `json:"title"`
	CreatedAt      time.Time  `json:"created_at"`
	Draft          bool       `json:"draft"`
	ChangedFiles   int        `json:"changed_files"`
	MergedAt       *time.Time `json:"merged_at"`
	MergeCommitSHA string     `json:"merge_commit_sha"`
	Mergeable      *bool      `json:"mergeable"`
	MergeableState string     `json:"mergeable_state"`
	User           *struct {
		Login string `json:"login"`
	} `json:"user"`
	Base struct {
		Ref string `json:"ref"`
	} `json:"base"`
	Head struct {
		Ref string `json:"ref"`
		SHA string `json:"sha"`
	} `json:"head"`
	Labels []struct {
		Name string `json:"name"`
	} `json:"labels"`
}

type restReview struct {
	State string `json:"state"`
	User  *struct {
		Login string `json:"login"`
	} `json:"user"`
}

type restChecks struct {
	CheckRuns []struct {
		Name       string `json:"name"`
		Status     string `json:"status"`
		Conclusion string `json:"conclusion"`
	} `json:"check_runs"`
}

type restStatuses struct {
	State    string `json:"state"`
	Statuses []struct {
		Context string `json:"context"`
		State   string `json:"state"`
	} `json:"statuses"`
}

type restComment struct {
	Body string `json:"body"`
}

type restRepo struct {
	DefaultBranch string `json:"default_branch"`
}

type restCommit struct {
	SHA    string `json:"sha"`
	Commit struct {
		Message string `json:"message"`
	} `json:"commit"`
}

func (g *GitHub) repoPath(suffix string) string {
	return "repos/" + g.owner + "/" + g.name + suffix
}

// restChunk answers a chunk from the REST endpoints, which GitHub serves on a
// quota and a code path apart from GraphQL.
func (p *pass) restChunk(ctx context.Context, chunk []target, first bool, prefixes []string) error {
	ctx, cancel := context.WithTimeout(ctx, p.deadline)
	defer cancel()

	asked := make(map[int]bool, len(chunk))
	targets := slices.Clone(chunk)
	for _, t := range chunk {
		asked[t.number] = true
	}
	if first {
		if err := p.restRepoWide(ctx, prefixes); err != nil {
			return err
		}
		for _, prs := range p.out.lanes {
			for _, n := range prs {
				if !slices.ContainsFunc(targets, func(t target) bool { return t.number == n }) {
					targets = append(targets, p.target(n))
				}
			}
		}
	}

	var mu sync.Mutex
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(restConcurrency)
	for _, t := range targets {
		g.Go(func() error {
			pr, found, err := p.restPR(gctx, t, asked[t.number])
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err != nil:
				return err
			case !found:
				p.out.missing = append(p.out.missing, t.number)
			default:
				p.out.prs[t.number] = pr
			}
			return nil
		})
	}
	return g.Wait()
}

func (p *pass) restRepoWide(ctx context.Context, prefixes []string) error {
	var g errgroup.Group
	var repo restRepo
	var commits []restCommit
	var open []restPull
	g.Go(func() (err error) {
		repo, err = ghapi.Get[restRepo](ctx, p.gh, p.repoPath(""))
		return err
	})
	g.Go(func() (err error) {
		commits, err = ghapi.Get[[]restCommit](ctx, p.gh, p.repoPath("/commits?per_page=100"))
		return err
	})
	if len(prefixes) > 0 {
		g.Go(func() (err error) {
			open, err = ghapi.Paginate[restPull](ctx, p.gh, p.repoPath("/pulls?state=open&per_page=100"))
			return err
		})
	}
	if err := g.Wait(); err != nil {
		return err
	}
	p.out.trunk = Trunk{Name: repo.DefaultBranch}
	for _, c := range commits {
		headline, _, _ := strings.Cut(c.Commit.Message, "\n")
		p.out.trunk.History = append(p.out.trunk.History, Commit{OID: c.SHA, Headline: headline})
	}
	for _, prefix := range prefixes {
		prs := []int{}
		for _, pull := range open {
			if strings.HasPrefix(pull.Head.Ref, prefix) {
				prs = append(prs, pull.Number)
			}
		}
		p.out.lanes[prefix] = prs
	}
	return nil
}

func (p *pass) restPR(ctx context.Context, t target, asked bool) (PR, bool, error) {
	pull, err := ghapi.Get[restPull](ctx, p.gh, p.repoPath(fmt.Sprintf("/pulls/%d", t.number)))
	if errors.Is(err, ghapi.ErrNotFound) {
		return PR{}, false, nil
	}
	if err != nil {
		return PR{}, false, err
	}

	var reviews []restReview
	var checks restChecks
	var statuses restStatuses
	var comments []restComment
	var g errgroup.Group
	g.Go(func() (err error) {
		reviews, err = ghapi.Paginate[restReview](ctx, p.gh, p.repoPath(fmt.Sprintf("/pulls/%d/reviews?per_page=100", t.number)))
		return err
	})
	g.Go(func() (err error) {
		checks, err = ghapi.Get[restChecks](ctx, p.gh, p.repoPath("/commits/"+pull.Head.SHA+"/check-runs?per_page=100"))
		return err
	})
	g.Go(func() (err error) {
		statuses, err = ghapi.Get[restStatuses](ctx, p.gh, p.repoPath("/commits/"+pull.Head.SHA+"/status?per_page=100"))
		return err
	})
	if t.activity {
		g.Go(func() (err error) {
			comments, err = ghapi.Paginate[restComment](ctx, p.gh, p.repoPath(fmt.Sprintf("/issues/%d/comments?per_page=100", t.number)))
			return err
		})
	}
	if err := g.Wait(); err != nil {
		return PR{}, false, err
	}

	pr := PR{
		Number:           pull.Number,
		State:            strings.ToUpper(pull.State),
		Title:            pull.Title,
		CreatedAt:        pull.CreatedAt,
		BaseRefName:      pull.Base.Ref,
		HeadRefName:      pull.Head.Ref,
		HeadRefOid:       pull.Head.SHA,
		Mergeable:        restMergeable(pull.Mergeable),
		MergeStateStatus: strings.ToUpper(pull.MergeableState),
		Reviews:          restReviews(reviews),
		Draft:            pull.Draft,
		ChangedFiles:     pull.ChangedFiles,
		Activity:         p.prev[t.number].Activity,
		Graphite:         p.infos[t.number],
		Mergeability:     p.mergeability[t.number],
		Via:              ViaREST,
	}
	if asked {
		pr.Via = p.via(ViaREST)
	}
	if pull.User != nil {
		pr.Author = pull.User.Login
	}
	for _, label := range pull.Labels {
		pr.Labels = append(pr.Labels, label.Name)
	}
	if pull.MergedAt != nil {
		pr.State, pr.MergeCommit, pr.SquashOn = "MERGED", pull.MergeCommitSHA, []string{pull.Base.Ref}
	}
	if len(statuses.Statuses) > 0 {
		pr.Status = strings.ToUpper(statuses.State)
	}
	pr.Rollup = restRollup(checks, statuses)
	if t.activity {
		pr.Activity = ""
		for _, comment := range comments {
			if strings.HasPrefix(comment.Body, ActivityHeading) {
				pr.Activity = comment.Body
			}
		}
	}
	if t.squash == "" {
		return pr, true, nil
	}
	for _, branch := range []string{t.base, p.out.trunk.Name} {
		if branch == "" || slices.Contains(pr.SquashOn, branch) {
			continue
		}
		reached, err := p.restReaches(ctx, branch, t.squash)
		if err != nil {
			return PR{}, false, err
		}
		if reached {
			pr.SquashOn = append(pr.SquashOn, branch)
		}
	}
	return pr, true, nil
}

func (g *GitHub) restReaches(ctx context.Context, branch, sha string) (bool, error) {
	compared, err := ghapi.Get[struct {
		Status string `json:"status"`
	}](ctx, g.gh, g.repoPath("/compare/"+(&url.URL{Path: branch}).EscapedPath()+"..."+sha+"?per_page=1"))
	if errors.Is(err, ghapi.ErrNotFound) {
		return false, nil
	}
	return compared.Status == "behind" || compared.Status == "identical", err
}

func restMergeable(mergeable *bool) string {
	switch {
	case mergeable == nil:
		return "UNKNOWN"
	case *mergeable:
		return "MERGEABLE"
	}
	return "CONFLICTING"
}

// restReviews keeps each reviewer's latest approving or change-requesting
// review, the set GraphQL's latestOpinionatedReviews holds: a comment leaves a
// standing verdict alone, and a dismissal retires it.
func restReviews(reviews []restReview) []Review {
	var latest []Review
	for _, r := range reviews {
		if r.User == nil || r.State != "APPROVED" && r.State != "CHANGES_REQUESTED" && r.State != "DISMISSED" {
			continue
		}
		latest = slices.DeleteFunc(latest, func(l Review) bool { return l.Author == r.User.Login })
		if r.State != "DISMISSED" {
			latest = append(latest, Review{Author: r.User.Login, State: r.State})
		}
	}
	return latest
}

// restRollup rebuilds the head commit's rollup from its check runs and commit
// statuses: failure when any context failed, pending while any has not
// finished, success once something passed, and none when nothing reported.
func restRollup(checks restChecks, statuses restStatuses) *Rollup {
	var rollup Rollup
	for _, c := range checks.CheckRuns {
		rollup.Contexts.Nodes = append(rollup.Contexts.Nodes, Context{
			Typename: "CheckRun", Name: c.Name, Conclusion: strings.ToUpper(c.Conclusion), Status: strings.ToUpper(c.Status),
		})
	}
	for _, s := range statuses.Statuses {
		rollup.Contexts.Nodes = append(rollup.Contexts.Nodes, Context{Typename: "StatusContext", Context: s.Context, State: strings.ToUpper(s.State)})
	}
	if len(rollup.Contexts.Nodes) == 0 {
		return nil
	}
	rollup.State = "SUCCESS"
	for _, c := range rollup.Contexts.Nodes {
		state := c.State
		if c.Typename == "CheckRun" {
			state = cmp.Or(c.Conclusion, c.Status)
		}
		switch state {
		case "SUCCESS", "SKIPPED", "NEUTRAL":
		case "QUEUED", "IN_PROGRESS", "PENDING", "WAITING", "REQUESTED", "EXPECTED":
			if rollup.State == "SUCCESS" {
				rollup.State = "PENDING"
			}
		default:
			rollup.State = "FAILURE"
		}
	}
	return &rollup
}
