package cli

import (
	"fmt"
	"regexp"
	"strings"
	"time"
)

// ghPull is one pull request as GitHub's REST API reports it. The pull request
// verbs read and write through gh api's REST routes rather than gh pr, which
// spends the GraphQL budget: a rate-limited GraphQL budget must not fail a
// step whose push already landed.
type ghPull struct {
	Number   int        `json:"number"`
	HTMLURL  string     `json:"html_url"`
	State    string     `json:"state"`
	MergedAt *time.Time `json:"merged_at"`
	Base     struct {
		Ref string `json:"ref"`
	} `json:"base"`
}

// graphQLState is the state gh pr reports, which REST splits into a closed
// state and a merge time.
func (p ghPull) graphQLState() string {
	if p.MergedAt != nil {
		return "MERGED"
	}
	return strings.ToUpper(p.State)
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
