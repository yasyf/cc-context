package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/yasyf/cc-context/internal/render"
)

// gh substitutes {owner} and {repo} only inside the endpoint, never in -f
// fields, so an owner-qualified filter rides in the endpoint's query string.
// Its colon goes escaped: gh also expands a bare :owner, :repo or :branch.
const ghRepoPath = "repos/{owner}/{repo}"

// ghPull is one pull request as GitHub's REST API reports it. The pull request
// verbs read and write through gh api's REST routes rather than gh pr, which
// spends the GraphQL budget: a rate-limited GraphQL budget must not fail a
// step whose push already landed.
type ghPull struct {
	Number    int        `json:"number"`
	HTMLURL   string     `json:"html_url"`
	Title     string     `json:"title"`
	Body      string     `json:"body"`
	State     string     `json:"state"`
	MergedAt  *time.Time `json:"merged_at"`
	Mergeable *bool      `json:"mergeable"`
	Base      struct {
		Ref string `json:"ref"`
	} `json:"base"`
	Head struct {
		SHA string `json:"sha"`
	} `json:"head"`
	Labels []struct {
		Name string `json:"name"`
	} `json:"labels"`
}

// graphQLState is the state gh pr reports, which REST splits into a closed
// state and a merge time.
func (p ghPull) graphQLState() string {
	if p.MergedAt != nil {
		return "MERGED"
	}
	return strings.ToUpper(p.State)
}

func (p ghPull) mergeable() string {
	switch {
	case p.Mergeable == nil:
		return statusUnknown
	case *p.Mergeable:
		return "MERGEABLE"
	default:
		return "CONFLICTING"
	}
}

func (p ghPull) labelNames() []string {
	names := make([]string, 0, len(p.Labels))
	for _, label := range p.Labels {
		names = append(names, label.Name)
	}
	return names
}

func ghNewestPull(ctx context.Context, dir render.Dir, branch string) (ghPull, bool, error) {
	endpoint := ghRepoPath + "/pulls?head={owner}%3A" + url.QueryEscape(branch) + "&state=all&sort=created&direction=desc&per_page=1"
	out, err := render.RunCLI(ctx, dir, "gh", []string{"api", endpoint})
	if err != nil {
		return ghPull{}, false, fmt.Errorf("gh api: list the pull requests of %s: %w", branch, err)
	}
	var prs []ghPull
	if err := json.Unmarshal([]byte(out), &prs); err != nil {
		return ghPull{}, false, fmt.Errorf("gh api: parse the pull requests of %s: %w", branch, err)
	}
	if len(prs) == 0 {
		return ghPull{}, false, nil
	}
	return prs[0], true, nil
}

func ghPullAt(ctx context.Context, dir render.Dir, number int) (ghPull, error) {
	out, err := render.RunCLI(ctx, dir, "gh", []string{"api", fmt.Sprintf("%s/pulls/%d", ghRepoPath, number)})
	if err != nil {
		return ghPull{}, fmt.Errorf("gh api: read PR #%d: %w", number, err)
	}
	var pr ghPull
	if err := json.Unmarshal([]byte(out), &pr); err != nil {
		return ghPull{}, fmt.Errorf("gh api: parse PR #%d: %w", number, err)
	}
	return pr, nil
}

func ghLanding(ctx context.Context, dir render.Dir, p ghPull, gt bool) (prLanding, error) {
	landing := prLanding{State: p.graphQLState(), MergedAt: p.MergedAt}
	if !gt || landing.State != "CLOSED" {
		return landing, nil
	}
	out, err := render.RunCLI(ctx, dir, "gh", []string{"api", fmt.Sprintf("%s/issues/%d", ghRepoPath, p.Number), "--jq", `.closed_by.login // ""`})
	if err != nil {
		return prLanding{}, fmt.Errorf("gh api: read who closed PR #%d: %w", p.Number, err)
	}
	// REST names an app's account with the [bot] suffix GraphQL leaves off.
	if login := strings.TrimSuffix(strings.TrimSpace(out), "[bot]"); login != "" {
		var closed prCloseEvent
		closed.Actor.Login = login
		landing.TimelineItems.Nodes = []prCloseEvent{closed}
	}
	return landing, nil
}

func ghPullPath(nwo string, number int) string {
	return fmt.Sprintf("repos/%s/pulls/%d", nwo, number)
}

func ghPatchPullArgv(nwo string, number int, fields ...string) []string {
	return append([]string{"api", "-X", "PATCH", ghPullPath(nwo, number), "--silent"}, fields...)
}

// ghPullsByHeadArgv lists branch's pull requests newest first, the one gh pr
// list --limit 1 resolves to. REST filters a head by owner:branch, and the
// owner is the repository's own because a stacked branch lives there.
func ghPullsByHeadArgv(nwo, branch, state string) []string {
	owner, _, _ := strings.Cut(nwo, "/")
	return []string{
		"api", "-X", "GET", "repos/" + nwo + "/pulls",
		"-f", "head=" + owner + ":" + branch, "-f", "state=" + state,
		"-f", "sort=created", "-f", "direction=desc", "-f", "per_page=1",
	}
}

var ghPlainArg = regexp.MustCompile(`^[A-Za-z0-9_./:=@+-]+$`)

// ghCommand renders a gh argv as the command a person pastes to retry it.
func ghCommand(argv []string) string {
	parts := make([]string, 0, len(argv)+1)
	parts = append(parts, "gh")
	for _, arg := range argv {
		if ghPlainArg.MatchString(arg) {
			parts = append(parts, arg)
			continue
		}
		parts = append(parts, shellSingleQuote(arg))
	}
	return strings.Join(parts, " ")
}
