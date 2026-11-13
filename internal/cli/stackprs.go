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

const stackPRFields = "number url title body isCrossRepository baseRefName headRefOid mergeable labels(first: 20) { nodes { name } } stack { number } " +
	"retargets: timelineItems(last: 20, itemTypes: [BASE_REF_CHANGED_EVENT, AUTOMATIC_BASE_CHANGE_SUCCEEDED_EVENT, BASE_REF_FORCE_PUSHED_EVENT]) { nodes { __typename " +
	"... on AutomaticBaseChangeSucceededEvent { oldBase newBase } ... on BaseRefForcePushedEvent { beforeCommit { oid } afterCommit { oid } } } } " + prLandingFields

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
	Stack *struct {
		Number int `json:"number"`
	} `json:"stack"`
	Retargets struct {
		Nodes []stackRetarget `json:"nodes"`
	} `json:"retargets"`
	prLanding
}

type stackRetarget struct {
	Typename     string       `json:"__typename"`
	OldBase      string       `json:"oldBase"`
	NewBase      string       `json:"newBase"`
	BeforeCommit *ghCommitOID `json:"beforeCommit"`
	AfterCommit  *ghCommitOID `json:"afterCommit"`
}

type ghCommitOID struct {
	OID string `json:"oid"`
}

func stackLandedBase(base string, events []stackRetarget) (string, []string, []string) {
	var landed string
	var heads, held []string
	for _, e := range events {
		switch e.Typename {
		case "BaseRefForcePushedEvent":
			for _, c := range []*ghCommitOID{e.BeforeCommit, e.AfterCommit} {
				if c != nil {
					held = append(held, c.OID)
				}
			}
		case "AutomaticBaseChangeSucceededEvent":
			landed, heads, held = e.OldBase, held, nil
			if e.NewBase != base {
				landed, heads = "", nil
			}
		default:
			landed, heads, held = "", nil, nil
		}
	}
	return landed, heads, held
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
	kept := reads[:0]
	for _, read := range reads {
		reused, err := stackReusedName(ctx, dir, trunk, read.branch, read.pr)
		if err != nil {
			return nil, err
		}
		if !reused {
			kept = append(kept, read)
		}
	}
	return stackLandPRs(ctx, dir, trunk, kept), nil
}

func stackReusedName(ctx context.Context, dir render.Dir, trunk, branch string, pr *stackPR) (bool, error) {
	if pr.State == "OPEN" || pr.Head == "" {
		return false, nil
	}
	ref := "refs/heads/" + branch
	out, code, stderr, err := render.RunCLIExitCode(ctx, dir, "git", []string{"rev-parse", "--verify", "--quiet", ref})
	switch {
	case err != nil:
		return false, fmt.Errorf("%s: git rev-parse %s: %w", stackRebasePrefix, ref, err)
	case code == 1:
		return false, nil
	case code != 0:
		return false, fmt.Errorf("%s: git rev-parse %s: exit %d: %s", stackRebasePrefix, ref, code, strings.TrimSpace(stderr))
	}
	tip := strings.TrimSpace(out)
	if tip == pr.Head {
		return false, nil
	}
	_, code, stderr, err = render.RunCLIExitCodeEnv(ctx, dir, "git", []string{"cat-file", "-e", pr.Head}, gitRecordedHistoryEnv)
	switch {
	case err != nil:
		return false, fmt.Errorf("%s: git cat-file -e %s: %w", stackRebasePrefix, shortOID(pr.Head), err)
	case code == 1:
		return true, nil
	case code != 0:
		return false, fmt.Errorf("%s: git cat-file -e %s: exit %d: %s", stackRebasePrefix, shortOID(pr.Head), code, strings.TrimSpace(stderr))
	}
	for _, pair := range [][2]string{{pr.Head, tip}, {tip, pr.Head}} {
		if related, err := gitIsAncestor(ctx, dir, stackRebasePrefix, pair[0], pair[1]); err != nil || related {
			return false, err
		}
	}
	ours, err := stackAuthored(ctx, dir, trunk, tip)
	if err != nil {
		return false, err
	}
	theirs, err := stackAuthored(ctx, dir, trunk, pr.Head)
	if err != nil {
		return false, err
	}
	return !slices.ContainsFunc(ours, func(commit string) bool { return slices.Contains(theirs, commit) }), nil
}

func stackAuthored(ctx context.Context, dir render.Dir, trunk, rev string) ([]string, error) {
	argv := []string{"log", "--no-merges", "-z", "--format=%an <%ae> %at", rev, "--not", "--remotes=*/" + trunk}
	out, err := render.RunCLI(ctx, dir, "git", argv)
	if err != nil {
		return nil, fmt.Errorf("%s: git %s: %w", stackRebasePrefix, strings.Join(argv, " "), err)
	}
	return slices.DeleteFunc(strings.Split(out, "\x00"), func(commit string) bool { return commit == "" }), nil
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
		Base: n.BaseRefName, Head: n.HeadRefOid, Mergeable: n.Mergeable, Segment: n.BaseRefName == gtSegmentBase(branch),
	}
	for _, label := range n.Labels.Nodes {
		pr.Labels = append(pr.Labels, label.Name)
	}
	if n.Stack != nil {
		pr.NativeStack = n.Stack.Number
	}
	pr.LandedBase, pr.LandedHeads, pr.BaseHeads = stackLandedBase(n.BaseRefName, n.Retargets.Nodes)
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
			Base: p.Base.Ref, Head: p.Head.SHA, Mergeable: p.mergeable(), Labels: p.labelNames(), Segment: p.Base.Ref == gtSegmentBase(branch),
		}
		if p.Stack != nil {
			pr.NativeStack = p.Stack.Number
		}
		reads = append(reads, stackPRRead{branch: branch, pr: pr, landing: landing})
	}
	return reads, nil
}

// stackLandPRs marks the pull requests that reached trunk. One GitHub still
// reads as open landed when its queue squash is on trunk and postdates its head:
// the queue lands it first and closes it after, and a GitHub outage between the
// two leaves it open.
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
	for _, number := range stackSquashedAfterHead(ctx, dir, byNumber, prSquashesOnBase(ctx, dir, trunk, open)) {
		byNumber[number].Landed = true
	}
	for number, landed := range resolveQueueLandings(ctx, dir, closes) {
		byNumber[number].Landed = landed
	}
	return prs
}

// stackSquashedAfterHead keeps the open pull requests whose squash was committed
// no earlier than their head. A head committed after its squash carries work
// pushed since the landing, which the squash does not hold.
func stackSquashedAfterHead(ctx context.Context, dir render.Dir, byNumber map[int]*stackPR, squashes map[int]string) []int {
	var shas []string
	for number, squash := range squashes {
		if head := byNumber[number].Head; head != "" {
			shas = append(shas, squash, head)
		}
	}
	if len(shas) == 0 {
		return nil
	}
	committed := map[string]int64{}
	if err := gitEachLogLine(ctx, dir, stackRebasePrefix, append([]string{"log", "--no-walk=unsorted", "--format=%H%x00%ct%x00%s"}, shas...), func(sha string, at int64, _ string) {
		committed[sha] = at
	}); err != nil {
		return nil
	}
	var landed []int
	for number, squash := range squashes {
		if head := byNumber[number].Head; head != "" && committed[head] <= committed[squash] {
			landed = append(landed, number)
		}
	}
	return landed
}

// stackLandedParent reads parent's pull request before a branch is stacked on
// it. A landed one is returned so the caller stacks on trunk in its place: a
// landing deletes the branch a pull request would otherwise base on. One closed
// without landing refuses, since trunk holds none of the commits it carried.
func stackLandedParent(ctx context.Context, dir render.Dir, prefix, trunk, parent string) (*stackPR, error) {
	prs, err := stackPRs(ctx, dir, trunk, []string{parent})
	if err != nil {
		return nil, fmt.Errorf("%s: read %s's pull request: %w", prefix, parent, err)
	}
	switch pr := prs[parent]; {
	case pr != nil && pr.Landed:
		return pr, nil
	case pr != nil && pr.State == "CLOSED":
		return nil, fmt.Errorf("%s: %s's pull request #%d was closed without merging, so trunk holds none of its commits to stack on — reopen it with gh pr reopen %d, or name another parent with --parent", prefix, parent, pr.Number, pr.Number)
	}
	return nil, nil
}
