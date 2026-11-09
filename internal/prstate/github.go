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

	"golang.org/x/sync/errgroup"

	"github.com/yasyf/cc-context/internal/ghapi"
	"github.com/yasyf/cc-context/internal/gtapi"
)

var (
	queueReadDeadline = 30 * time.Second
	queueReadBackoff  = []time.Duration{time.Second, 3 * time.Second}
)

const (
	pageSize         = 20
	pollWorkers      = 4
	graphqlDeadline  = 20 * time.Second
	graphiteDeadline = 10 * time.Second

	prFields = "number state title createdAt author { login } isDraft baseRefName headRefName headRefOid mergeable mergeStateStatus " +
		"reviewDecision latestOpinionatedReviews(first: 20) { nodes { state author { login } } } changedFiles mergeCommit { oid } labels(first: 50) { nodes { name } } " +
		"checks: commits(last: 1) { nodes { commit { oid status { state } statusCheckRollup { state contexts(last: 100) { totalCount nodes { __typename " +
		"... on CheckRun { name title conclusion status startedAt databaseId checkSuite { databaseId workflowRun { workflow { databaseId } } } } ... on StatusContext { context state targetUrl } } } } " +
		"checkSuites(first: 50, filterBy: {appId: " + actionsAppID + "}) { nodes { status } } } } } " +
		"baseRef { branchProtectionRule { requiredStatusCheckContexts } rules(first: 50) { nodes { type parameters { " +
		"... on RequiredStatusChecksParameters { requiredStatusChecks { context } } } } } }"
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

func (g *GitHub) legacyOwned(branch string) bool {
	return g.gt != nil && g.gt.LegacyOwned(branch)
}

func (g *GitHub) stampOwners(prs map[int]PR) {
	for n, pr := range prs {
		pr.LegacyOwned = g.legacyOwned(pr.HeadRefName)
		prs[n] = pr
	}
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

type page struct {
	trunk    bool
	prefixes []string
	targets  []target
	resp     response
	gone     []int
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
	IsDraft          bool      `json:"isDraft"`
	ChangedFiles     int       `json:"changedFiles"`
	Author           *struct {
		Login string `json:"login"`
	} `json:"author"`
	LatestOpinionatedReviews struct {
		Nodes []struct {
			State  string `json:"state"`
			Author *struct {
				Login string `json:"login"`
			} `json:"author"`
		} `json:"nodes"`
	} `json:"latestOpinionatedReviews"`
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
				OID    string `json:"oid"`
				Status *struct {
					State string `json:"state"`
				} `json:"status"`
				StatusCheckRollup *Rollup `json:"statusCheckRollup"`
				CheckSuites       struct {
					Nodes []checkSuite `json:"nodes"`
				} `json:"checkSuites"`
			} `json:"commit"`
		} `json:"nodes"`
	} `json:"checks"`
	Comments *struct {
		Nodes []struct {
			Body string `json:"body"`
		} `json:"nodes"`
	} `json:"comments"`
	BaseRef *baseRefNode `json:"baseRef"`
}

type baseRefNode struct {
	BranchProtectionRule *struct {
		RequiredStatusCheckContexts []string `json:"requiredStatusCheckContexts"`
	} `json:"branchProtectionRule"`
	Rules struct {
		Nodes []struct {
			Type       string `json:"type"`
			Parameters *struct {
				RequiredStatusChecks []struct {
					Context string `json:"context"`
				} `json:"requiredStatusChecks"`
			} `json:"parameters"`
		} `json:"nodes"`
	} `json:"rules"`
}

// required names the status checks the base branch's classic protection and
// rulesets require; GitHub answers no base when the branch is gone.
func (b *baseRefNode) required() []string {
	if b == nil {
		return nil
	}
	var out []string
	if b.BranchProtectionRule != nil {
		out = append(out, b.BranchProtectionRule.RequiredStatusCheckContexts...)
	}
	for _, rule := range b.Rules.Nodes {
		if rule.Type != "REQUIRED_STATUS_CHECKS" || rule.Parameters == nil {
			continue
		}
		for _, check := range rule.Parameters.RequiredStatusChecks {
			out = append(out, check.Context)
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
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
	infos, mergeability := g.queueRecords(ctx, req.PRs)
	p := pass{GitHub: g, prev: prev, infos: infos, mergeability: mergeability, out: poll{prs: make(map[int]PR, len(req.PRs)), lanes: map[string][]int{}}}
	targets := make([]target, 0, len(req.PRs))
	for _, n := range req.PRs {
		targets = append(targets, p.target(n))
	}
	pages := paginate(req.Prefixes, targets)
	gctx, stop := context.WithCancel(ctx)
	defer stop()
	var group errgroup.Group
	group.SetLimit(pollWorkers)
	failed := make([]error, len(pages))
	for i := range pages {
		group.Go(func() error {
			failed[i] = g.send(gctx, &pages[i])
			if _, limited := ghapi.RateLimited(failed[i]); limited {
				stop()
			}
			return nil
		})
	}
	_ = group.Wait()
	for i, pg := range pages {
		if failed[i] != nil {
			continue
		}
		if err := p.collect(pg); err != nil {
			return poll{}, err
		}
	}
	if err := errors.Join(failed...); err != nil {
		return p.out, err
	}
	if len(p.out.missing) == 0 {
		return p.out, nil
	}
	user := *g
	user.gh = g.gh.AsUser()
	read, err := user.restPass(ctx, p.out.missing, prev, p.out.trunk.Name)
	if err != nil {
		return poll{}, fmt.Errorf("prstate: GitHub answered no pull request %s, and reading them as the user failed: %w", prNumbers(p.out.missing), err)
	}
	for n, pr := range read.prs {
		pr.Source = SourceAppRefused
		p.out.prs[n] = pr
	}
	p.out.missing = read.missing
	return p.out, nil
}

func prNumbers(numbers []int) string {
	refs := make([]string, 0, len(numbers))
	for _, n := range numbers {
		refs = append(refs, fmt.Sprintf("#%d", n))
	}
	return strings.Join(refs, " ")
}

type pass struct {
	*GitHub
	prev         map[int]PR
	infos        map[int]*gtapi.PullRequestInfo
	mergeability map[int]string
	out          poll
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
	t.activity = open && (gtapi.InMergeQueue(info, p.mergeability[n]) || !known || gtapi.InMergeQueue(last.Graphite, last.Mergeability) || last.QueueLabelled())
	return t
}

func paginate(prefixes []string, targets []target) []page {
	pages := []page{{trunk: true}}
	for {
		pg := &pages[len(pages)-1]
		n := min(len(prefixes), pageSize)
		pg.prefixes, prefixes = prefixes[:n], prefixes[n:]
		m := min(len(targets), pageSize-n)
		pg.targets, targets = targets[:m], targets[m:]
		if len(prefixes)+len(targets) == 0 {
			return pages
		}
		pages = append(pages, page{})
	}
}

func (g *GitHub) send(ctx context.Context, pg *page) error {
	ctx, cancel := context.WithTimeout(ctx, graphqlDeadline)
	defer cancel()
	resp, err := g.query(ctx, *pg)
	var gql *ghapi.GraphQLError
	if errors.As(err, &gql) {
		retry, gone, ok := dropNotFound(pg.targets, gql)
		if !ok {
			return err
		}
		pg.targets, pg.gone = retry, gone
		resp, err = g.query(ctx, *pg)
	}
	pg.resp = resp
	return err
}

func (p *pass) collect(pg page) error {
	p.out.missing = append(p.out.missing, pg.gone...)
	if pg.trunk || pg.resp.RateLimit.Remaining < p.out.rate.Remaining {
		p.out.rate = pg.resp.RateLimit
	}
	if err := p.repoWide(pg); err != nil {
		return err
	}
	for i, t := range pg.targets {
		var node *prNode
		if err := decode(pg.resp.Repository["p"+strconv.Itoa(i)], &node); err != nil {
			return err
		}
		if node == nil {
			p.out.missing = append(p.out.missing, t.number)
			continue
		}
		pr, err := p.record(*node, t, i, pg.resp)
		if err != nil {
			return err
		}
		p.out.prs[t.number] = pr
	}
	return nil
}

func (p *pass) repoWide(pg page) error {
	if pg.trunk {
		var trunk trunkNode
		if err := decode(pg.resp.Repository["trunk"], &trunk); err != nil {
			return err
		}
		p.out.trunk = Trunk{Name: trunk.Name}
		for _, c := range trunk.Target.History.Nodes {
			p.out.trunk.History = append(p.out.trunk.History, Commit{OID: c.OID, Headline: c.MessageHeadline})
		}
	}
	for k, prefix := range pg.prefixes {
		var refs refsNode
		if err := decode(pg.resp.Repository["l"+strconv.Itoa(k)], &refs); err != nil {
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

func (g *GitHub) query(ctx context.Context, pg page) (response, error) {
	decls := []string{"$owner: String!", "$repo: String!"}
	vars := map[string]any{"owner": g.owner, "repo": g.name}
	var fields strings.Builder
	if pg.trunk {
		fields.WriteString("    trunk: defaultBranchRef { name target { ... on Commit { history(first: 100) { nodes { oid messageHeadline } } } } }\n")
	}
	for k, prefix := range pg.prefixes {
		decls = append(decls, fmt.Sprintf("$l%d: String!", k))
		vars[fmt.Sprintf("l%d", k)] = prefix
		fmt.Fprintf(&fields, "    l%d: refs(refPrefix: \"refs/heads/\", query: $l%d, first: 100) { totalCount nodes { name associatedPullRequests(states: [OPEN], first: 5) { nodes { number } } } }\n", k, k)
	}
	for i, t := range pg.targets {
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
		Draft:            node.IsDraft,
		ChangedFiles:     node.ChangedFiles,
		Required:         node.BaseRef.required(),
		Activity:         last.Activity,
		Graphite:         p.infos[t.number],
		Mergeability:     p.mergeability[t.number],
		QueueUnknown:     p.queueUnknown(t.number),
	}
	if node.Author != nil {
		pr.Author = node.Author.Login
	}
	if node.MergeCommit != nil {
		pr.MergeCommit = node.MergeCommit.OID
		if node.State == "MERGED" {
			pr.SquashOn = []string{node.BaseRefName}
		}
	}
	for _, label := range node.Labels.Nodes {
		pr.Labels = append(pr.Labels, label.Name)
	}
	for _, r := range node.LatestOpinionatedReviews.Nodes {
		review := Review{State: r.State}
		if r.Author != nil {
			review.Author = r.Author.Login
		}
		pr.Reviews = append(pr.Reviews, review)
	}
	if checks := node.Checks.Nodes; len(checks) > 0 && checks[0].Commit.OID == node.HeadRefOid {
		commit := checks[0].Commit
		pr.Rollup = commit.StatusCheckRollup.Latest()
		pr.Rollup.countUnfinishedActions(commit.CheckSuites.Nodes)
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

func (p *pass) queueUnknown(number int) bool {
	return p.gt != nil && (p.infos == nil || p.infos[number] == nil && p.gt.QueueRequiresStatus(gtapi.PullRequestInfo{}))
}

func (g *GitHub) queueInfos(ctx context.Context, numbers []int) ([]gtapi.PullRequestInfo, error) {
	for attempt := 0; ; attempt++ {
		infoCtx, cancel := context.WithTimeout(ctx, queueReadDeadline)
		infos, err := g.gt.PullRequestInfo(infoCtx, gtapi.PullRequestInfoRequest{
			RepoOwner:  g.owner,
			RepoName:   g.name,
			PRNumbers:  numbers,
			Consistent: true,
			Callsite:   "ccx",
		})
		cancel()
		if !errors.Is(err, context.DeadlineExceeded) || attempt == len(queueReadBackoff) {
			return infos, err
		}
		select {
		case <-ctx.Done():
			return nil, err
		case <-time.After(queueReadBackoff[attempt]):
		}
	}
}

func (g *GitHub) queueRecords(ctx context.Context, numbers []int) (map[int]*gtapi.PullRequestInfo, map[int]string) {
	if len(numbers) == 0 || g.gt == nil || g.gt.ProviderError() != nil {
		return nil, nil
	}
	infos, err := g.queueInfos(ctx, numbers)
	if err != nil {
		_, _ = fmt.Fprintf(g.warn, "prstate: %s: %v; this poll carries no queue state\n", g.gt.QueueProvider(), err)
		return nil, nil
	}
	byNumber := make(map[int]*gtapi.PullRequestInfo, len(infos))
	for _, info := range infos {
		info.Body, info.Versions = "", nil
		byNumber[info.PRNumber] = &info
	}
	mergeCtx, cancelMerge := context.WithTimeout(ctx, graphiteDeadline)
	defer cancelMerge()
	mergeability, err := g.gt.MergeabilityFor(mergeCtx, g.owner, g.name, infos, numbers)
	if err != nil {
		_, _ = fmt.Fprintf(g.warn, "prstate: %s: %v; this poll carries no mergeability\n", g.gt.QueueProvider(), err)
	}
	var unread []int
	for _, number := range numbers {
		if info := byNumber[number]; info != nil && g.gt.QueueRequiresStatus(*info) {
			if _, known := mergeability[number]; !known {
				unread = append(unread, number)
			}
		}
	}
	if len(unread) == 0 {
		return byNumber, mergeability
	}
	reread := map[int]string{}
	if err == nil {
		reread, err = g.gt.MergeabilityFor(mergeCtx, g.owner, g.name, infos, unread)
	}
	if err != nil {
		_, _ = fmt.Fprintf(g.warn, "prstate: %s: %v; this poll carries no queue state for %v\n", g.gt.QueueProvider(), err, unread)
		for _, number := range unread {
			delete(byNumber, number)
		}
		return byNumber, mergeability
	}
	if mergeability == nil {
		mergeability = make(map[int]string, len(unread))
	}
	for _, number := range unread {
		state, known := settledQueue(g.gt.QueueProvider(), reread, number)
		if !known {
			_, _ = fmt.Fprintf(g.warn, "prstate: %s: queue state of #%d is unknown after a second read; this poll carries no queue state\n", g.gt.QueueProvider(), number)
			delete(byNumber, number)
			continue
		}
		if _, read := reread[number]; !read {
			_, _ = fmt.Fprintf(g.warn, "prstate: trunk holds no record of #%d after a second read, so its queue never took it; read as not queued\n", number)
		}
		mergeability[number] = state
	}
	return byNumber, mergeability
}

func settledQueue(provider string, reread map[int]string, number int) (string, bool) {
	if state, read := reread[number]; read {
		return state, true
	}
	if provider == "trunk" {
		return "notQueued", true
	}
	return "", false
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
