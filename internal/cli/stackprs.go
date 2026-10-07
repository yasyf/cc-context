package cli

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/yasyf/cc-context/internal/ghapi"
	"github.com/yasyf/cc-context/internal/render"
	"github.com/yasyf/cc-context/internal/vcs"
)

// stackPRCandidates bounds the pull requests read per head name, which GraphQL
// cannot narrow to this repository's own branches the way REST's owner:branch
// filter does.
const stackPRCandidates = 10

const stackPRChunk = 40

const stackPRFields = "number url title body isCrossRepository baseRefName headRefOid mergeable labels(first: 20) { nodes { name } } " + prLandingFields

type stackPRNode struct {
	Number            int    `json:"number"`
	URL               string `json:"url"`
	Title             string `json:"title"`
	Body              string `json:"body"`
	IsCrossRepository bool   `json:"isCrossRepository"`
	BaseRefName       string `json:"baseRefName"`
	HeadRefOid        string `json:"headRefOid"`
	Mergeable         string `json:"mergeable"`
	Labels            struct {
		Nodes []struct {
			Name string `json:"name"`
		} `json:"nodes"`
	} `json:"labels"`
	prLanding
}

type stackPRBatch struct {
	Repository map[string]struct {
		TotalCount int           `json:"totalCount"`
		Nodes      []stackPRNode `json:"nodes"`
	} `json:"repository"`
}

type stackPRRead struct {
	branch  string
	pr      *stackPR
	landing prLanding
}

// stackQueryPRs reads the stack's pull requests over GraphQL, and over REST
// when GitHub refuses that query for rate or the repository cannot be named:
// the two budgets are metered apart, so either one spent leaves the other.
func stackQueryPRs(ctx context.Context, dir render.Dir, trunk string, branches []string) (map[string]*stackPR, error) {
	reads, err := stackReadPRsGraphQL(ctx, dir, branches)
	if _, limited := ghapi.RateLimited(err); limited || errors.Is(err, vcs.ErrNoGitHub) {
		reads, err = stackReadPRsREST(ctx, dir, branches)
	}
	if err != nil {
		return nil, err
	}
	return stackLandPRs(ctx, dir, trunk, reads), nil
}

func stackPRGraphQL(n int) string {
	return reviewsBatch(n, "String!", func(alias string) string {
		return fmt.Sprintf("%s: pullRequests(headRefName: $%s, first: %d, orderBy: {field: CREATED_AT, direction: DESC}) { totalCount nodes { %s } }",
			alias, alias, stackPRCandidates, stackPRFields)
	})
}

func stackReadPRsGraphQL(ctx context.Context, dir render.Dir, branches []string) ([]stackPRRead, error) {
	if len(branches) == 0 {
		return nil, nil
	}
	repo, err := vcs.LookupRepo(ctx, dir, false)
	if err != nil {
		return nil, err
	}
	owner, name, ok := strings.Cut(repo.NameWithOwner, "/")
	if !ok {
		return nil, fmt.Errorf("%q is not owner/name", repo.NameWithOwner)
	}
	var reads []stackPRRead
	var crowded []string
	for chunk := range slices.Chunk(branches, stackPRChunk) {
		vars := map[string]any{"owner": owner, "repo": name}
		for i, branch := range chunk {
			vars[reviewsAlias(i)] = branch
		}
		batch, err := ghapi.GraphQL[stackPRBatch](ctx, reviewsAPI().ForRepo(repo.NameWithOwner).Unwaiting(), stackPRGraphQL(len(chunk)), vars)
		if err != nil {
			return nil, err
		}
		for i, branch := range chunk {
			found := batch.Repository[reviewsAlias(i)]
			own := slices.IndexFunc(found.Nodes, func(n stackPRNode) bool { return !n.IsCrossRepository })
			switch {
			case own >= 0:
				reads = append(reads, stackGraphQLRead(branch, found.Nodes[own]))
			case found.TotalCount > len(found.Nodes):
				crowded = append(crowded, branch)
			}
		}
	}
	rest, err := stackReadPRsREST(ctx, dir, crowded)
	if err != nil {
		return nil, err
	}
	return append(reads, rest...), nil
}

func stackGraphQLRead(branch string, n stackPRNode) stackPRRead {
	pr := &stackPR{
		Number: n.Number, URL: n.URL, Title: n.Title, Body: n.Body, State: n.State,
		Base: n.BaseRefName, Head: n.HeadRefOid, Mergeable: n.Mergeable,
	}
	for _, label := range n.Labels.Nodes {
		pr.Labels = append(pr.Labels, label.Name)
	}
	return stackPRRead{branch: branch, pr: pr, landing: n.prLanding}
}

func stackReadPRsREST(ctx context.Context, dir render.Dir, branches []string) ([]stackPRRead, error) {
	var reads []stackPRRead
	for _, branch := range branches {
		p, found, err := ghNewestPull(ctx, dir, branch)
		if err != nil {
			return nil, err
		}
		if !found {
			continue
		}
		if p.State == "open" {
			if p, err = ghPullAt(ctx, dir, p.Number); err != nil {
				return nil, err
			}
		}
		landing, err := ghLanding(ctx, dir, p, true)
		if err != nil {
			return nil, err
		}
		pr := &stackPR{
			Number: p.Number, URL: p.HTMLURL, Title: p.Title, Body: p.Body, State: landing.State,
			Base: p.Base.Ref, Head: p.Head.SHA, Mergeable: p.mergeable(), Labels: p.labelNames(),
		}
		reads = append(reads, stackPRRead{branch: branch, pr: pr, landing: landing})
	}
	return reads, nil
}

// stackLandPRs marks the pull requests that reached trunk. One GitHub still
// reads as open landed when its queue squash is on trunk: the queue lands it
// first and closes it after, and a GitHub outage between the two leaves it open.
func stackLandPRs(ctx context.Context, dir render.Dir, trunk string, reads []stackPRRead) map[string]*stackPR {
	prs := make(map[string]*stackPR, len(reads))
	var closes []prQueueClose
	var open []int
	byNumber := map[int]*stackPR{}
	for _, read := range reads {
		switch read.landing.verdict(true) {
		case prLanded:
			read.pr.Landed = true
		case prStillOpen:
			open = append(open, read.pr.Number)
			byNumber[read.pr.Number] = read.pr
		case prQueueClosed:
			closes = append(closes, prQueueClose{Number: read.pr.Number, Base: trunk})
			byNumber[read.pr.Number] = read.pr
		}
		prs[read.branch] = read.pr
	}
	for number := range prSquashesOnBase(ctx, dir, trunk, open) {
		byNumber[number].Landed = true
	}
	for number, landed := range resolveQueueLandings(ctx, dir, closes) {
		byNumber[number].Landed = landed
	}
	return prs
}
