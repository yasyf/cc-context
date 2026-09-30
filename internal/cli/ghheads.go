package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/yasyf/cc-context/internal/render"
)

// headPR is the newest pull request of one branch, as one batched GraphQL read
// returns it.
type headPR struct {
	Number      int    `json:"number"`
	URL         string `json:"url"`
	Title       string `json:"title"`
	Body        string `json:"body"`
	BaseRefName string `json:"baseRefName"`
	HeadRefOid  string `json:"headRefOid"`
	Mergeable   string `json:"mergeable"`
	Labels      struct {
		Nodes []struct {
			Name string `json:"name"`
		} `json:"nodes"`
	} `json:"labels"`
	Commits struct {
		Nodes []struct {
			Commit struct {
				StatusCheckRollup struct {
					State string `json:"state"`
				} `json:"statusCheckRollup"`
			} `json:"commit"`
		} `json:"nodes"`
	} `json:"commits"`
	prLanding
}

func (p headPR) checks() string {
	if len(p.Commits.Nodes) == 0 {
		return ""
	}
	return p.Commits.Nodes[0].Commit.StatusCheckRollup.State
}

func (p headPR) labelNames() []string {
	names := make([]string, 0, len(p.Labels.Nodes))
	for _, label := range p.Labels.Nodes {
		names = append(names, label.Name)
	}
	return names
}

// ghHeadPRs reads the newest pull request of every branch in one gh api graphql
// call, the one batch gh exposes: a REST read costs a request per branch, and a
// stack of lanes sharing one token spends its REST quota on them. {owner} and
// {repo} are gh's own placeholders for the working directory's repository. A
// branch with no pull request is absent from the result.
func ghHeadPRs(ctx context.Context, dir render.Dir, branches []string) (map[string]headPR, error) {
	heads := map[string]headPR{}
	if len(branches) == 0 {
		return heads, nil
	}
	out, err := ghAPI(ctx, dir, ghHeadPRsArgv(branches)...)
	if err != nil {
		return nil, fmt.Errorf("gh api graphql: read the pull requests of %s: %w", strings.Join(branches, ", "), err)
	}
	var resp struct {
		Data struct {
			Repository map[string]struct {
				Nodes []headPR `json:"nodes"`
			} `json:"repository"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &resp); err != nil {
		return nil, fmt.Errorf("gh api graphql: parse the pull requests of %s: %w", strings.Join(branches, ", "), err)
	}
	for i, branch := range branches {
		if nodes := resp.Data.Repository[headPRAlias(i)].Nodes; len(nodes) > 0 {
			heads[branch] = nodes[0]
		}
	}
	return heads, nil
}

func ghHeadPRsArgv(branches []string) []string {
	argv := make([]string, 0, 6+2*len(branches))
	argv = append(argv, "graphql", "-F", "owner={owner}", "-F", "repo={repo}")
	for i, branch := range branches {
		argv = append(argv, "-f", headPRAlias(i)+"="+branch)
	}
	return append(argv, "-f", "query="+headPRQuery(len(branches)))
}

// headPRAlias names one branch's field in the batched query. A GraphQL alias
// takes neither "/" nor "." nor "-", which branch names do, so the branch's
// position stands in for its name.
func headPRAlias(i int) string {
	return fmt.Sprintf("b%d", i)
}

// headPRQuery renders one aliased pullRequests field per branch. It names no
// state filter, so a merged or closed pull request resolves as an open one does,
// and it orders descending: a branch resubmitted after its first pull request
// closed carries two, and the newest is the one that speaks for it.
func headPRQuery(n int) string {
	decls := make([]string, 0, n+2)
	decls = append(decls, "$owner: String!", "$repo: String!")
	var fields strings.Builder
	for i := range n {
		alias := headPRAlias(i)
		decls = append(decls, "$"+alias+": String!")
		fmt.Fprintf(&fields, "    %s: pullRequests(headRefName: $%s, first: 1, orderBy: {field: CREATED_AT, direction: DESC})"+
			" { nodes { number url title body baseRefName headRefOid mergeable labels(first: 50) { nodes { name } } %s"+
			" commits(last: 1) { nodes { commit { statusCheckRollup { state } } } } } }\n",
			alias, alias, prLandingFields)
	}
	return fmt.Sprintf("query(%s) {\n  repository(owner: $owner, name: $repo) {\n%s  }\n}", strings.Join(decls, ", "), fields.String())
}
