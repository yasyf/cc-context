package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
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
	Number         int        `json:"number"`
	HTMLURL        string     `json:"html_url"`
	Title          string     `json:"title"`
	Body           string     `json:"body"`
	State          string     `json:"state"`
	MergedAt       *time.Time `json:"merged_at"`
	Mergeable      *bool      `json:"mergeable"`
	MergeableState string     `json:"mergeable_state"`
	Base           struct {
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

// ghRateLimitWait is how long one secondary rate limit is waited out when
// GitHub sends no Retry-After, which it asks to be at least a minute.
var ghRateLimitWait = time.Minute

const ghRateLimitRetries = 3

// ghAPI runs gh api, waiting out a secondary rate limit rather than failing on
// it. The refused response's own X-RateLimit headers tell the limits apart:
// gh api rate_limit misreports the quota on some tokens. A Retry-After is
// honored as given, an exhausted quota, whose reset can be an hour out, fails
// at once, and a refusal with quota left is the request-rate limit.
func ghAPI(ctx context.Context, dir render.Dir, args ...string) (string, error) {
	return ghAPIWaiting(ctx, dir, ghRateLimitWait, args...)
}

func ghAPIWaiting(ctx context.Context, dir render.Dir, wait time.Duration, args ...string) (string, error) {
	for waits := 0; ; waits++ {
		out, code, stderr, err := render.RunCLIExitCodeEnv(ctx, dir, "gh", append([]string{"api"}, args...), []string{"GH_DEBUG=api"})
		if err != nil || code == 0 {
			return out, err
		}
		refusal := ghDebugRefusal(stderr)
		failure := fmt.Errorf("gh: exit status %d: %s", code, refusal)
		delay, ok := ghRateLimitDelay(ghDebugLastResponse(stderr), wait)
		if !ok || waits == ghRateLimitRetries || !strings.Contains(strings.ToLower(refusal), "rate limit") {
			return "", failure
		}
		select {
		case <-ctx.Done():
			return "", errors.Join(failure, ctx.Err())
		case <-time.After(delay):
		}
	}
}

func ghRateLimitDelay(header http.Header, wait time.Duration) (time.Duration, bool) {
	if seconds, err := strconv.Atoi(header.Get("Retry-After")); err == nil {
		return time.Duration(seconds) * time.Second, true
	}
	if header.Get("X-Ratelimit-Remaining") == "0" {
		return 0, false
	}
	return wait, true
}

// ghDebugLastResponse parses the headers of the last response GH_DEBUG=api
// logged, the refused one when gh api fails.
func ghDebugLastResponse(stderr string) http.Header {
	header := http.Header{}
	for _, line := range strings.Split(stderr, "\n") {
		rest, ok := strings.CutPrefix(strings.TrimRight(line, "\r"), "< ")
		if !ok {
			continue
		}
		if strings.HasPrefix(rest, "HTTP/") {
			header = http.Header{}
			continue
		}
		if key, value, ok := strings.Cut(rest, ": "); ok {
			header.Add(key, value)
		}
	}
	return header
}

// ghDebugRefusal is gh's own error, which it prints after GH_DEBUG=api's log
// of the last request closes. gh names the status only of an error body it
// could parse, so an empty one gets the status line the log recorded.
func ghDebugRefusal(stderr string) string {
	refusal := stderr
	if i := strings.LastIndex(stderr, "* Request took "); i >= 0 {
		if _, after, ok := strings.Cut(stderr[i:], "\n"); ok {
			refusal = after
		}
	}
	refusal = strings.TrimSpace(refusal)
	status := ghDebugLastStatus(stderr)
	code, _, _ := strings.Cut(status, " ")
	if status == "" || strings.Contains(refusal, "(HTTP "+code+")") {
		return refusal
	}
	return fmt.Sprintf("%s (HTTP %s)", refusal, status)
}

// ghDebugLastStatus is the status GH_DEBUG=api logged for the last request,
// "502 Bad Gateway" off its "< HTTP/2.0 502 Bad Gateway" line, and empty when
// that request got no response.
func ghDebugLastStatus(stderr string) string {
	status := ""
	for _, line := range strings.Split(stderr, "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.HasPrefix(line, "* Request at ") {
			status = ""
		}
		if rest, ok := strings.CutPrefix(line, "< HTTP/"); ok {
			_, status, _ = strings.Cut(rest, " ")
		}
	}
	return status
}

func ghNewestPull(ctx context.Context, dir render.Dir, branch string) (ghPull, bool, error) {
	endpoint := ghRepoPath + "/pulls?head={owner}%3A" + url.QueryEscape(branch) + "&state=all&sort=created&direction=desc&per_page=1"
	out, err := ghAPI(ctx, dir, endpoint)
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
	out, err := ghAPI(ctx, dir, fmt.Sprintf("%s/pulls/%d", ghRepoPath, number))
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
	out, err := ghAPI(ctx, dir, fmt.Sprintf("%s/issues/%d", ghRepoPath, p.Number), "--jq", `.closed_by.login // ""`)
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
