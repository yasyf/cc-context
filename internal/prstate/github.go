package prstate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/yasyf/cc-context/internal/ghapi"
	"github.com/yasyf/cc-context/internal/gtapi"
)

const (
	chunkSize = 40

	prFields = "number state title createdAt author { login } baseRefName headRefName headRefOid mergeable mergeStateStatus " +
		"reviewDecision changedFiles mergeCommit { oid } labels(first: 50) { nodes { name } } " +
		"checks: commits(last: 1) { nodes { commit { status { state } statusCheckRollup { state contexts(first: 100) { nodes { __typename " +
		"... on CheckRun { name conclusion status } ... on StatusContext { context state } } } } } } }"
	activityFields = " comments(last: 100) { nodes { body } }"
	probeQuery     = "query { viewer { login } rateLimit { remaining resetAt } }"
)

// GitHub polls one repository through GitHub's GraphQL API, plus Graphite's
// record of its merge queue when gt is set.
type GitHub struct {
	gh    *ghapi.Client
	gt    *gtapi.Client
	owner string
	name  string
	warn  io.Writer
}

// NewGitHub builds the source for repo, an owner/name pair. Its GitHub client
// returns a rate-limited response at once, since the Store keeps the backoff.
func NewGitHub(gh *ghapi.Client, gt *gtapi.Client, repo string, warn io.Writer) (*GitHub, error) {
	owner, name, ok := strings.Cut(repo, "/")
	if !ok || owner == "" || name == "" {
		return nil, fmt.Errorf("prstate: malformed repository name %q", repo)
	}
	return &GitHub{gh: gh.Unwaiting(), gt: gt, owner: owner, name: name, warn: warn}, nil
}

type poll struct {
	trunk   Trunk
	lanes   map[string][]int
	prs     map[int]PR
	rate    Rate
	missing []int
}

type target struct {
	number   int
	activity bool
	squash   string
	base     string
}

type prNode struct {
	Number           int       `json:"number"`
	State            string    `json:"state"`
	Title            string    `json:"title"`
	CreatedAt        time.Time `json:"createdAt"`
	BaseRefName      string    `json:"baseRefName"`
	HeadRefName      string    `json:"headRefName"`
	HeadRefOid       string    `json:"headRefOid"`
	Mergeable        string    `json:"mergeable"`
	MergeStateStatus string    `json:"mergeStateStatus"`
	ReviewDecision   string    `json:"reviewDecision"`
	ChangedFiles     int       `json:"changedFiles"`
	Author           *struct {
		Login string `json:"login"`
	} `json:"author"`
	MergeCommit *struct {
		OID string `json:"oid"`
	} `json:"mergeCommit"`
	Labels struct {
		Nodes []struct {
			Name string `json:"name"`
		} `json:"nodes"`
	} `json:"labels"`
	Checks struct {
		Nodes []struct {
			Commit struct {
				Status *struct {
					State string `json:"state"`
				} `json:"status"`
				StatusCheckRollup *Rollup `json:"statusCheckRollup"`
			} `json:"commit"`
		} `json:"nodes"`
	} `json:"checks"`
	Comments *struct {
		Nodes []struct {
			Body string `json:"body"`
		} `json:"nodes"`
	} `json:"comments"`
}

type trunkNode struct {
	Name   string `json:"name"`
	Target struct {
		History struct {
			Nodes []struct {
				OID             string `json:"oid"`
				MessageHeadline string `json:"messageHeadline"`
			} `json:"nodes"`
		} `json:"history"`
	} `json:"target"`
}

type refsNode struct {
	TotalCount int `json:"totalCount"`
	Nodes      []struct {
		Name                   string `json:"name"`
		AssociatedPullRequests struct {
			Nodes []struct {
				Number int `json:"number"`
			} `json:"nodes"`
		} `json:"associatedPullRequests"`
	} `json:"nodes"`
}

type compareNode struct {
	Compare *struct {
		Status string `json:"status"`
	} `json:"compare"`
}

type response struct {
	RateLimit  Rate                       `json:"rateLimit"`
	Repository map[string]json.RawMessage `json:"repository"`
}

func (g *GitHub) probe(ctx context.Context) (Rate, error) {
	resp, err := ghapi.GraphQL[struct {
		RateLimit Rate `json:"rateLimit"`
	}](ctx, g.gh, probeQuery, nil)
	return resp.RateLimit, err
}

func (g *GitHub) poll(ctx context.Context, req Want, prev map[int]PR) (poll, error) {
	p := pass{GitHub: g, prev: prev, infos: g.queueRecords(ctx, req.PRs), out: poll{prs: make(map[int]PR, len(req.PRs)), lanes: map[string][]int{}}}
	targets := make([]target, 0, len(req.PRs))
	for _, n := range req.PRs {
		targets = append(targets, p.target(n))
	}
	chunks := slices.Collect(slices.Chunk(targets, chunkSize))
	if len(chunks) == 0 {
		chunks = [][]target{nil}
	}
	for i, chunk := range chunks {
		var prefixes []string
		if i == 0 {
			prefixes = req.Prefixes
		}
		if err := p.chunk(ctx, chunk, i == 0, prefixes); err != nil {
			return poll{}, err
		}
	}
	return p.out, nil
}

type pass struct {
	*GitHub
	prev  map[int]PR
	infos map[int]*gtapi.PullRequestInfo
	out   poll
}

func (p *pass) target(n int) target {
	last, known := p.prev[n]
	info, fresh := p.infos[n]
	if !fresh {
		info = last.Graphite
	}
	t := target{number: n}
	var open bool
	if info != nil {
		open = info.State == gtapi.PROpen
		t.squash, t.base = info.MergeCommitSha, info.BaseRefName
	} else {
		open = !known || last.State == "OPEN"
	}
	t.activity = open && (inQueue(info) || !known || inQueue(last.Graphite) || last.QueueLabelled())
	return t
}

func (p *pass) chunk(ctx context.Context, chunk []target, first bool, prefixes []string) error {
	resp, err := p.query(ctx, chunk, first, prefixes)
	var gql *ghapi.GraphQLError
	if errors.As(err, &gql) {
		retry, gone, ok := dropNotFound(chunk, gql)
		if !ok {
			return err
		}
		p.out.missing = append(p.out.missing, gone...)
		chunk = retry
		resp, err = p.query(ctx, chunk, first, prefixes)
	}
	if err != nil {
		return err
	}
	p.out.rate = resp.RateLimit
	if first {
		if err := p.repoWide(resp, prefixes); err != nil {
			return err
		}
	}
	for i, t := range chunk {
		var node *prNode
		if err := decode(resp.Repository["p"+strconv.Itoa(i)], &node); err != nil {
			return err
		}
		if node == nil {
			p.out.missing = append(p.out.missing, t.number)
			continue
		}
		pr, err := p.record(*node, t, i, resp)
		if err != nil {
			return err
		}
		p.out.prs[t.number] = pr
	}
	return nil
}

func (p *pass) repoWide(resp response, prefixes []string) error {
	var trunk trunkNode
	if err := decode(resp.Repository["trunk"], &trunk); err != nil {
		return err
	}
	p.out.trunk = Trunk{Name: trunk.Name}
	for _, c := range trunk.Target.History.Nodes {
		p.out.trunk.History = append(p.out.trunk.History, Commit{OID: c.OID, Headline: c.MessageHeadline})
	}
	for k, prefix := range prefixes {
		var refs refsNode
		if err := decode(resp.Repository["l"+strconv.Itoa(k)], &refs); err != nil {
			return err
		}
		if refs.TotalCount > len(refs.Nodes) {
			_, _ = fmt.Fprintf(p.warn, "prstate: %d branches match %s; only the first %d are read\n", refs.TotalCount, prefix, len(refs.Nodes))
		}
		prs := []int{}
		for _, ref := range refs.Nodes {
			if !strings.HasPrefix(ref.Name, prefix) {
				continue
			}
			for _, pr := range ref.AssociatedPullRequests.Nodes {
				prs = append(prs, pr.Number)
			}
		}
		p.out.lanes[prefix] = prs
	}
	return nil
}

func (g *GitHub) query(ctx context.Context, chunk []target, first bool, prefixes []string) (response, error) {
	decls := []string{"$owner: String!", "$repo: String!"}
	vars := map[string]any{"owner": g.owner, "repo": g.name}
	var fields strings.Builder
	if first {
		fields.WriteString("    trunk: defaultBranchRef { name target { ... on Commit { history(first: 100) { nodes { oid messageHeadline } } } } }\n")
		for k, prefix := range prefixes {
			decls = append(decls, fmt.Sprintf("$l%d: String!", k))
			vars[fmt.Sprintf("l%d", k)] = prefix
			fmt.Fprintf(&fields, "    l%d: refs(refPrefix: \"refs/heads/\", query: $l%d, first: 100) { totalCount nodes { name associatedPullRequests(states: [OPEN], first: 5) { nodes { number } } } }\n", k, k)
		}
	}
	for i, t := range chunk {
		decls = append(decls, fmt.Sprintf("$p%d: Int!", i))
		vars[fmt.Sprintf("p%d", i)] = t.number
		selection := prFields
		if t.activity {
			selection += activityFields
		}
		fmt.Fprintf(&fields, "    p%d: pullRequest(number: $p%d) { %s }\n", i, i, selection)
		if t.squash == "" {
			continue
		}
		decls = append(decls, fmt.Sprintf("$m%d: String!", i))
		vars[fmt.Sprintf("m%d", i)] = t.squash
		fmt.Fprintf(&fields, "    t%d: defaultBranchRef { compare(headRef: $m%d) { status } }\n", i, i)
		if t.base != "" {
			decls = append(decls, fmt.Sprintf("$b%d: String!", i))
			vars[fmt.Sprintf("b%d", i)] = "refs/heads/" + t.base
			fmt.Fprintf(&fields, "    b%d: ref(qualifiedName: $b%d) { compare(headRef: $m%d) { status } }\n", i, i, i)
		}
	}
	query := fmt.Sprintf("query(%s) {\n  rateLimit { remaining resetAt }\n  repository(owner: $owner, name: $repo) {\n%s  }\n}",
		strings.Join(decls, ", "), fields.String())
	return ghapi.GraphQL[response](ctx, g.gh, query, vars)
}

func (p *pass) record(node prNode, t target, i int, resp response) (PR, error) {
	last := p.prev[t.number]
	pr := PR{
		Number:           node.Number,
		State:            node.State,
		Title:            node.Title,
		CreatedAt:        node.CreatedAt,
		BaseRefName:      node.BaseRefName,
		HeadRefName:      node.HeadRefName,
		HeadRefOid:       node.HeadRefOid,
		Mergeable:        node.Mergeable,
		MergeStateStatus: node.MergeStateStatus,
		ReviewDecision:   node.ReviewDecision,
		ChangedFiles:     node.ChangedFiles,
		Activity:         last.Activity,
		Graphite:         last.Graphite,
	}
	if node.Author != nil {
		pr.Author = node.Author.Login
	}
	if node.MergeCommit != nil {
		pr.MergeCommit = node.MergeCommit.OID
	}
	for _, label := range node.Labels.Nodes {
		pr.Labels = append(pr.Labels, label.Name)
	}
	if len(node.Checks.Nodes) > 0 {
		commit := node.Checks.Nodes[0].Commit
		pr.Rollup = commit.StatusCheckRollup
		if commit.Status != nil {
			pr.Status = commit.Status.State
		}
	}
	if node.Comments != nil {
		pr.Activity = ""
		for _, comment := range node.Comments.Nodes {
			if strings.HasPrefix(comment.Body, ActivityHeading) {
				pr.Activity = comment.Body
			}
		}
	}
	if info, ok := p.infos[t.number]; ok {
		pr.Graphite = info
	}
	if t.squash == "" {
		return pr, nil
	}
	for _, check := range []struct{ alias, branch string }{{"b", t.base}, {"t", p.out.trunk.Name}} {
		var compared *compareNode
		if err := decode(resp.Repository[check.alias+strconv.Itoa(i)], &compared); err != nil {
			return PR{}, err
		}
		if compared != nil && compared.Compare != nil && (compared.Compare.Status == "BEHIND" || compared.Compare.Status == "IDENTICAL") && !slices.Contains(pr.SquashOn, check.branch) {
			pr.SquashOn = append(pr.SquashOn, check.branch)
		}
	}
	return pr, nil
}

func (g *GitHub) queueRecords(ctx context.Context, numbers []int) map[int]*gtapi.PullRequestInfo {
	if len(numbers) == 0 || g.gt == nil {
		return nil
	}
	infos, err := g.gt.PullRequestInfo(ctx, gtapi.PullRequestInfoRequest{
		RepoOwner:  g.owner,
		RepoName:   g.name,
		PRNumbers:  numbers,
		Consistent: true,
		Callsite:   "ccx",
	})
	if err != nil {
		_, _ = fmt.Fprintf(g.warn, "prstate: graphite: %v; queue state held from the last poll\n", err)
		return nil
	}
	byNumber := make(map[int]*gtapi.PullRequestInfo, len(infos))
	for _, info := range infos {
		info.Body, info.Versions = "", nil
		byNumber[info.PRNumber] = &info
	}
	return byNumber
}

func inQueue(info *gtapi.PullRequestInfo) bool {
	return info != nil && info.MergeQueueStatus != nil && info.MergeQueueStatus.IsInGraphiteMq
}

func dropNotFound(chunk []target, gql *ghapi.GraphQLError) ([]target, []int, bool) {
	gonePR := map[int]bool{}
	noCompare := map[int]bool{}
	for _, m := range gql.Messages {
		if m.Type != "NOT_FOUND" || len(m.Path) < 2 || m.Path[0] != "repository" {
			return nil, nil, false
		}
		alias, _ := m.Path[1].(string)
		if alias == "" {
			return nil, nil, false
		}
		i, err := strconv.Atoi(alias[1:])
		if err != nil || i >= len(chunk) {
			return nil, nil, false
		}
		switch alias[0] {
		case 'p':
			gonePR[i] = true
		case 't', 'b':
			noCompare[i] = true
		default:
			return nil, nil, false
		}
	}
	var kept []target
	var gone []int
	for i, t := range chunk {
		if gonePR[i] {
			gone = append(gone, t.number)
			continue
		}
		if noCompare[i] {
			t.squash = ""
		}
		kept = append(kept, t)
	}
	return kept, gone, true
}

func decode(raw json.RawMessage, into any) error {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	if err := json.Unmarshal(raw, into); err != nil {
		return fmt.Errorf("prstate: decode graphql response: %w", err)
	}
	return nil
}
