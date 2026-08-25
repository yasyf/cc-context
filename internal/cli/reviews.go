package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/yasyf/cc-context/internal/ghapi"
	"github.com/yasyf/cc-context/internal/render"
	"github.com/yasyf/cc-context/internal/vcs"
)

const (
	reviewsPollDefault = 30 * time.Second
	reviewsBodyBudget  = 500
	reviewsMaxFails    = 5

	envReviewsPollInterval = "CCX_REVIEWS_POLL_INTERVAL"
)

var (
	errBadReviewsPollInterval     = errors.New("invalid CCX_REVIEWS_POLL_INTERVAL")
	errReviewsIntervalNotPositive = errors.New("reviews: poll interval must be positive")
)

type ghPRComment struct {
	ID   int64  `json:"id"`
	Body string `json:"body"`
	User struct {
		Login string `json:"login"`
	} `json:"user"`
	Path      string    `json:"path"`
	Line      *int      `json:"line"`
	HTMLURL   string    `json:"html_url"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

type ghReview struct {
	ID    int64  `json:"id"`
	State string `json:"state"`
	Body  string `json:"body"`
	User  struct {
		Login string `json:"login"`
	} `json:"user"`
	HTMLURL     string    `json:"html_url"`
	SubmittedAt time.Time `json:"submitted_at"`
}

// ghPullRequest is the pull request shape every batch selects: what a target
// needs to identify itself, and what reviewTerminalState reads.
type ghPullRequest struct {
	Number int    `json:"number"`
	URL    string `json:"url"`
	prLanding
}

type prTarget struct {
	Number    int
	URL       string
	watermark time.Time
	seen      map[string]time.Time
	fails     int // consecutive failed polls
}

type reviewsOpts struct {
	interval time.Duration
	budget   int
	all      bool
}

type reviewEvent struct {
	target    *prTarget
	key       string
	kind      string
	author    string
	locus     string
	body      string
	htmlURL   string
	id        int64
	timestamp time.Time
	edited    bool
	triage    bool
}

type reviewsPoll struct {
	target    *prTarget
	events    []reviewEvent
	watermark time.Time
	seen      map[string]time.Time
	terminal  string
}

func newReviewsCmd() *cobra.Command {
	var (
		o         reviewsOpts
		sinceText string
		stack     bool
	)
	cmd := &cobra.Command{
		Use:   "reviews [pr-or-branch...]",
		Short: "Stream new GitHub PR review events",
		Long: "Stream new GitHub PR review events. The watch is at-least-once: " +
			"a cancelled watch's resume command reuses the oldest open target's " +
			"watermark, so a resumed watch may re-print events already seen.",
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if stack && len(args) > 0 {
				return errors.New("reviews: --stack and positional targets cannot be combined")
			}
			interval, err := reviewsPollInterval(o.interval, cmd.Flags().Changed("interval"))
			if err != nil {
				return err
			}
			o.interval = interval

			var since time.Time
			if sinceText == "now" {
				since = time.Now()
			} else {
				since, o.all, err = parseSince(sinceText)
				if err != nil {
					return fmt.Errorf("reviews: --since %q: %w", sinceText, err)
				}
			}

			var (
				client  reviewsClient
				targets []*prTarget
			)
			if stack {
				client, targets, err = resolveStackReviewTargets(cmd.Context(), cmd.OutOrStdout(), since)
			} else {
				client, targets, err = resolveReviewTargets(cmd.Context(), args, since)
			}
			if err != nil {
				return err
			}
			if stack && len(targets) == 0 {
				return errors.New("reviews: --stack found no stacked branches — run it from a stacked branch, not trunk")
			}
			return watchReviews(cmd.Context(), cmd.OutOrStdout(), client, targets, o)
		},
	}
	cmd.Flags().StringVar(&sinceText, "since", "now", "events since RFC3339, duration ago, or all")
	cmd.Flags().DurationVar(&o.interval, "interval", 0, "poll interval")
	cmd.Flags().IntVar(&o.budget, "budget", reviewsBodyBudget, "token budget per event body (0 = uncapped)")
	cmd.Flags().BoolVar(&stack, "stack", false, "watch every PR in the current graphite downstack instead of positional targets")
	return cmd
}

func reviewsPollInterval(flag time.Duration, changed bool) (time.Duration, error) {
	if changed {
		if flag <= 0 {
			return 0, errReviewsIntervalNotPositive
		}
		return flag, nil
	}
	raw := os.Getenv(envReviewsPollInterval)
	if raw == "" {
		return reviewsPollDefault, nil
	}
	interval, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("%w %q: %w", errBadReviewsPollInterval, raw, err)
	}
	if interval <= 0 {
		return 0, errReviewsIntervalNotPositive
	}
	return interval, nil
}

func parseSince(s string) (t time.Time, all bool, err error) {
	if s == "all" {
		return time.Time{}, true, nil
	}
	if d, derr := time.ParseDuration(s); derr == nil {
		return time.Now().Add(-d), false, nil
	}
	if t, terr := time.Parse(time.RFC3339, s); terr == nil {
		return t, false, nil
	}
	return time.Time{}, false, fmt.Errorf("must be RFC3339, a duration, or all")
}

// reviewsClient is the GitHub endpoint one watch polls: the API client, the
// repository every request is scoped to, and whether this working copy lands its
// pull requests through Graphite — the lane whose merge queue closes what it
// merges.
type reviewsClient struct {
	api   *ghapi.Client
	owner string
	repo  string
	gt    bool
	dir   render.Dir
}

// reviewsAPI builds the client a watch polls through. A var so tests point the
// watch at an httptest server instead of api.github.com.
var reviewsAPI = ghapi.Default

func resolveReviewsClient(ctx context.Context) (reviewsClient, error) {
	l, err := resolveLane(ctx, "reviews", workingDir(), false)
	if err != nil {
		return reviewsClient{}, err
	}
	repo, err := vcs.LookupRepo(ctx, l.dir(), false)
	if err != nil {
		return reviewsClient{}, fmt.Errorf("reviews: %w", err)
	}
	owner, name, ok := strings.Cut(repo.NameWithOwner, "/")
	if !ok {
		return reviewsClient{}, fmt.Errorf("reviews: %q is not owner/name", repo.NameWithOwner)
	}
	return reviewsClient{api: reviewsAPI(), owner: owner, repo: name, gt: l.gt, dir: l.dir()}, nil
}

// reviewsAlias names one target's field in a batched query. A GraphQL alias
// takes neither "/" nor "." nor "-", which branch names do, so the target's
// position stands in for its name.
func reviewsAlias(i int) string { return fmt.Sprintf("p%d", i) }

func reviewsBatch(n int, decl string, field func(alias string) string) string {
	decls := make([]string, 0, n+2)
	decls = append(decls, "$owner: String!", "$repo: String!")
	var fields strings.Builder
	for i := range n {
		alias := reviewsAlias(i)
		decls = append(decls, "$"+alias+": "+decl)
		fields.WriteString("    " + field(alias) + "\n")
	}
	return fmt.Sprintf("query(%s) {\n  repository(owner: $owner, name: $repo) {\n%s  }\n}",
		strings.Join(decls, ", "), fields.String())
}

func reviewsNumberQuery(n int) string {
	return reviewsBatch(n, "Int!", func(alias string) string {
		return fmt.Sprintf("%s: pullRequest(number: $%s) { number url %s }", alias, alias, prLandingFields)
	})
}

// reviewsBranchQuery resolves each branch's pull request. It names no state
// filter, because the gh pr view it replaced had none either and resolves a
// merged pull request just as happily; and it orders descending because that is
// the one gh picks — a branch resubmitted after its first pull request closed
// carries two, and the watch belongs on the live one.
func reviewsBranchQuery(n int) string {
	return reviewsBatch(n, "String!", func(alias string) string {
		return fmt.Sprintf(
			"%s: pullRequests(headRefName: $%s, first: 1, orderBy: {field: CREATED_AT, direction: DESC}) { nodes { number url %s } }",
			alias, alias, prLandingFields)
	})
}

type reviewsNumberBatch struct {
	Repository map[string]ghPullRequest `json:"repository"`
}

type reviewsBranchBatch struct {
	Repository map[string]struct {
		Nodes []ghPullRequest `json:"nodes"`
	} `json:"repository"`
}

// pullRequestsByNumber resolves every number in one query — the single call a
// poll cycle makes for all its open targets' state.
func (c reviewsClient) pullRequestsByNumber(ctx context.Context, numbers []int) (map[int]ghPullRequest, error) {
	if len(numbers) == 0 {
		return nil, nil
	}
	vars := map[string]any{"owner": c.owner, "repo": c.repo}
	for i, number := range numbers {
		vars[reviewsAlias(i)] = number
	}
	batch, err := ghapi.GraphQL[reviewsNumberBatch](ctx, c.api, reviewsNumberQuery(len(numbers)), vars)
	if err != nil {
		return nil, err
	}
	byNumber := make(map[int]ghPullRequest, len(numbers))
	for i, number := range numbers {
		byNumber[number] = batch.Repository[reviewsAlias(i)]
	}
	return byNumber, nil
}

// pullRequestsByBranch resolves every branch in one query. A branch with no
// pull request is absent from the map rather than an error, which is what lets
// a stack skip it and a named operand refuse.
func (c reviewsClient) pullRequestsByBranch(ctx context.Context, branches []string) (map[string]ghPullRequest, error) {
	if len(branches) == 0 {
		return nil, nil
	}
	vars := map[string]any{"owner": c.owner, "repo": c.repo}
	for i, branch := range branches {
		vars[reviewsAlias(i)] = branch
	}
	batch, err := ghapi.GraphQL[reviewsBranchBatch](ctx, c.api, reviewsBranchQuery(len(branches)), vars)
	if err != nil {
		return nil, err
	}
	byBranch := make(map[string]ghPullRequest, len(branches))
	for i, branch := range branches {
		nodes := batch.Repository[reviewsAlias(i)].Nodes
		if len(nodes) == 0 {
			continue
		}
		byBranch[branch] = nodes[0]
	}
	return byBranch, nil
}

// reviewsResolveError maps GitHub's not-found onto the CLI's own sentinel, so a
// named pull request that does not exist exits 3.
func reviewsResolveError(err error) error {
	if errors.Is(err, ghapi.ErrNotFound) {
		return fmt.Errorf("reviews: resolve targets: %w: %w", ErrNotFound, err)
	}
	return fmt.Errorf("reviews: resolve targets: %w", err)
}

// reviewsOperand is one positional target: a pull request number, or a branch
// whose head pull request stands in for it.
type reviewsOperand struct {
	text   string
	number int
}

// reviewsOperands reads each operand as a pull request number or a branch name,
// standing the current branch in for an empty operand list.
func reviewsOperands(ctx context.Context, dir render.Dir, operands []string) ([]reviewsOperand, error) {
	if len(operands) == 0 {
		branch, err := gitCurrentBranch(ctx, dir, "reviews")
		if err != nil {
			return nil, err
		}
		if branch == "" {
			return nil, errors.New("reviews: detached HEAD; name a pull request number or branch")
		}
		operands = []string{branch}
	}
	ops := make([]reviewsOperand, 0, len(operands))
	for _, operand := range operands {
		number, err := strconv.Atoi(operand)
		if err != nil || number <= 0 {
			ops = append(ops, reviewsOperand{text: operand})
			continue
		}
		ops = append(ops, reviewsOperand{text: operand, number: number})
	}
	return ops, nil
}

func resolveReviewTargets(ctx context.Context, operands []string, since time.Time) (reviewsClient, []*prTarget, error) {
	client, err := resolveReviewsClient(ctx)
	if err != nil {
		return reviewsClient{}, nil, err
	}
	ops, err := reviewsOperands(ctx, client.dir, operands)
	if err != nil {
		return reviewsClient{}, nil, err
	}
	var (
		numbers  []int
		branches []string
	)
	for _, op := range ops {
		if op.number > 0 {
			numbers = append(numbers, op.number)
			continue
		}
		branches = append(branches, op.text)
	}
	byNumber, err := client.pullRequestsByNumber(ctx, numbers)
	if err != nil {
		return reviewsClient{}, nil, reviewsResolveError(err)
	}
	byBranch, err := client.pullRequestsByBranch(ctx, branches)
	if err != nil {
		return reviewsClient{}, nil, reviewsResolveError(err)
	}

	targets := make([]*prTarget, 0, len(ops))
	for _, op := range ops {
		if op.number > 0 {
			targets = append(targets, newReviewTarget(byNumber[op.number], since))
			continue
		}
		pr, ok := byBranch[op.text]
		if !ok {
			return reviewsClient{}, nil, fmt.Errorf("reviews: resolve %s: %w", op.text, ErrNotFound)
		}
		targets = append(targets, newReviewTarget(pr, since))
	}
	return client, targets, nil
}

func newReviewTarget(pr ghPullRequest, since time.Time) *prTarget {
	return &prTarget{Number: pr.Number, URL: pr.URL, watermark: since, seen: map[string]time.Time{}}
}

// resolveStackReviewTargets resolves review targets from the current
// graphite downstack, skipping (with a note to w) a branch with no open PR
// rather than failing the whole command.
func resolveStackReviewTargets(ctx context.Context, w io.Writer, since time.Time) (reviewsClient, []*prTarget, error) {
	l, err := resolveLane(ctx, "reviews", workingDir(), false)
	if err != nil {
		return reviewsClient{}, nil, err
	}
	if !l.gt {
		if l.note != "" {
			return reviewsClient{}, nil, fmt.Errorf("reviews: --stack declined the graphite lane: %s", l.note)
		}
		return reviewsClient{}, nil, errors.New("reviews: --stack requires a graphite repo")
	}
	branches, err := stackBranches(ctx, l.dir(), "reviews")
	if err != nil {
		return reviewsClient{}, nil, err
	}
	return resolveBranchTargets(ctx, w, branches, since)
}

// resolveBranchTargets resolves every branch's pull request in one batched
// query, skipping (with a note to w) any branch with none — shared by ship's
// --reviews wiring and reviews --stack.
func resolveBranchTargets(ctx context.Context, w io.Writer, branches []string, since time.Time) (reviewsClient, []*prTarget, error) {
	client, err := resolveReviewsClient(ctx)
	if err != nil {
		return reviewsClient{}, nil, err
	}
	byBranch, err := client.pullRequestsByBranch(ctx, branches)
	if err != nil {
		return reviewsClient{}, nil, reviewsResolveError(err)
	}
	var targets []*prTarget
	for _, branch := range branches {
		pr, ok := byBranch[branch]
		if !ok {
			if _, werr := fmt.Fprintf(w, "reviews: no open PR for %s\n", branch); werr != nil {
				return reviewsClient{}, nil, werr
			}
			continue
		}
		targets = append(targets, newReviewTarget(pr, since))
	}
	return client, targets, nil
}

// shipWatchReviews resolves each branch's open PR and watches all that
// resolve, for ship's --reviews flag. A branch with no open PR is skipped
// with a note rather than failing the already-succeeded ship.
func shipWatchReviews(ctx context.Context, w io.Writer, branches []string) error {
	client, targets, err := resolveBranchTargets(ctx, w, branches, time.Now())
	if err != nil {
		return err
	}
	if len(targets) == 0 {
		return nil
	}
	interval, err := reviewsPollInterval(0, false)
	if err != nil {
		return err
	}
	return watchReviews(ctx, w, client, targets, reviewsOpts{interval: interval, budget: reviewsBodyBudget})
}

func watchReviews(ctx context.Context, w io.Writer, client reviewsClient, targets []*prTarget, o reviewsOpts) error {
	for _, target := range targets {
		if _, err := fmt.Fprintln(w, strings.Join([]string{
			fmt.Sprintf("watching pr#%d", target.Number),
			target.URL,
			"poll " + o.interval.String(),
		}, shipSep)); err != nil {
			return fmt.Errorf("reviews: write watching line: %w", err)
		}
	}

	open := targets
	merged, closed, aborted := 0, 0, 0
	for len(open) > 0 {
		results := pollReviewCycle(ctx, client, open, o)
		if ctx.Err() != nil {
			if werr := writeReviewsCancellation(w, open, merged, closed); werr != nil {
				return werr
			}
			return ctx.Err()
		}

		var events []reviewEvent
		next := make([]*prTarget, 0, len(open))
		for _, res := range results {
			if res.err != nil {
				res.target.fails++
				slog.Warn("reviews: poll failed", "pr", res.target.Number, "consecutive_failures", res.target.fails, "err", res.err)
				if res.target.fails < reviewsMaxFails {
					next = append(next, res.target)
				}
				continue
			}
			res.target.fails = 0
			res.target.watermark = res.poll.watermark
			res.target.seen = res.poll.seen
			events = append(events, res.poll.events...)
			if res.poll.terminal == "" {
				next = append(next, res.target)
			}
		}
		sort.SliceStable(events, func(i, j int) bool {
			return events[i].timestamp.Before(events[j].timestamp)
		})
		for _, event := range events {
			if err := writeReviewEvent(w, event, o.budget); err != nil {
				return err
			}
		}

		for _, res := range results {
			switch {
			case res.err != nil:
				if res.target.fails < reviewsMaxFails {
					continue
				}
				if _, werr := fmt.Fprintf(w, "◆ pr#%d watch aborted%s%v\n\n", res.target.Number, shipSep, res.err); werr != nil {
					return fmt.Errorf("reviews: write aborted line: %w", werr)
				}
				aborted++
			case res.poll.terminal != "":
				if _, werr := fmt.Fprintf(w, "◆ pr#%d %s%s%s\n\n",
					res.target.Number, res.poll.terminal, shipSep, res.target.URL); werr != nil {
					return fmt.Errorf("reviews: write terminal line: %w", werr)
				}
				if res.poll.terminal == "merged" {
					merged++
				} else {
					closed++
				}
			}
		}
		open = next

		if len(open) == 0 {
			break
		}
		if err := sleepCtx(ctx, o.interval); err != nil {
			if ctx.Err() != nil {
				if werr := writeReviewsCancellation(w, open, merged, closed); werr != nil {
					return werr
				}
				return ctx.Err()
			}
			return fmt.Errorf("reviews: sleep: %w", err)
		}
	}
	summary := []string{
		fmt.Sprintf("watch done%s%d merged", shipSep, merged),
		fmt.Sprintf("%d closed", closed),
	}
	if aborted > 0 {
		summary = append(summary, fmt.Sprintf("%d aborted", aborted))
	}
	if _, err := fmt.Fprintln(w, strings.Join(summary, shipSep)); err != nil {
		return fmt.Errorf("reviews: write completion line: %w", err)
	}
	if aborted > 0 {
		return fmt.Errorf("reviews: %d of %d target(s) aborted after repeated failures", aborted, len(targets))
	}
	return nil
}

// pollReviewResult is one target's outcome from a single poll cycle.
type pollReviewResult struct {
	target *prTarget
	poll   reviewsPoll
	err    error
}

// pollReviewCycle reads every open target's state in one batched query, then
// polls each target's feeds, continuing past a per-target failure so one broken
// PR never blocks another target's cycle. A failed batch fails every target:
// none of them learned whether it is still open.
func pollReviewCycle(ctx context.Context, client reviewsClient, targets []*prTarget, o reviewsOpts) []pollReviewResult {
	results := make([]pollReviewResult, 0, len(targets))
	numbers := make([]int, 0, len(targets))
	for _, target := range targets {
		numbers = append(numbers, target.Number)
	}
	states, err := client.pullRequestsByNumber(ctx, numbers)
	if err != nil {
		for _, target := range targets {
			results = append(results, pollReviewResult{target: target, err: fmt.Errorf("reviews: pr states: %w", err)})
		}
		return results
	}
	for _, target := range targets {
		poll, err := pollReviewTarget(ctx, client, target, states[target.Number], o)
		results = append(results, pollReviewResult{target: target, poll: poll, err: err})
	}
	return results
}

func pollReviewTarget(ctx context.Context, client reviewsClient, target *prTarget, pr ghPullRequest, o reviewsOpts) (reviewsPoll, error) {
	base := fmt.Sprintf("/repos/%s/%s", client.owner, client.repo)
	suffix := ""
	if !o.all {
		suffix = "&since=" + target.watermark.UTC().Format(time.RFC3339)
	}
	inline, err := ghapi.Paginate[ghPRComment](ctx, client.api,
		fmt.Sprintf("%s/pulls/%d/comments?per_page=100%s", base, target.Number, suffix))
	if err != nil {
		return reviewsPoll{}, fmt.Errorf("reviews: pr#%d inline comments: %w", target.Number, err)
	}
	comments, err := ghapi.Paginate[ghPRComment](ctx, client.api,
		fmt.Sprintf("%s/issues/%d/comments?per_page=100%s", base, target.Number, suffix))
	if err != nil {
		return reviewsPoll{}, fmt.Errorf("reviews: pr#%d issue comments: %w", target.Number, err)
	}
	reviews, err := ghapi.Paginate[ghReview](ctx, client.api,
		fmt.Sprintf("%s/pulls/%d/reviews?per_page=100", base, target.Number))
	if err != nil {
		return reviewsPoll{}, fmt.Errorf("reviews: pr#%d reviews: %w", target.Number, err)
	}
	terminal, err := reviewTerminalState(pr, client.gt)
	if err != nil {
		return reviewsPoll{}, fmt.Errorf("reviews: pr#%d: %w", target.Number, err)
	}

	poll := reviewsPoll{
		target:    target,
		watermark: target.watermark,
		seen:      cloneReviewSeen(target.seen),
		terminal:  terminal,
	}
	for _, comment := range inline {
		poll.observe(comment.UpdatedAt)
		poll.addComment(target, comment, "i:", "inline", inlineLocus(comment))
	}
	for _, comment := range comments {
		poll.observe(comment.UpdatedAt)
		poll.addComment(target, comment, "c:", "comment", "")
	}
	for _, review := range reviews {
		poll.observe(review.SubmittedAt)
		state := strings.ToLower(review.State)
		if review.State == "PENDING" || (review.State == "COMMENTED" && review.Body == "") {
			continue
		}
		poll.addEvent(reviewEvent{
			target:    target,
			key:       fmt.Sprintf("r:%d", review.ID),
			kind:      "review",
			author:    review.User.Login,
			locus:     state,
			body:      review.Body,
			htmlURL:   review.HTMLURL,
			id:        review.ID,
			timestamp: review.SubmittedAt,
			triage:    review.State == "CHANGES_REQUESTED",
		})
	}
	return poll, nil
}

// reviewTerminalState names the end a pull request reached, and is empty while
// it is still open. A stack the Graphite merge queue landed reports CLOSED with
// a null mergedAt, so on the gt lane the closing account is what tells a landed
// pull request from an abandoned one.
func reviewTerminalState(pr ghPullRequest, gt bool) (string, error) {
	if pr.landed(gt) {
		return "merged", nil
	}
	switch pr.State {
	case "OPEN":
		return "", nil
	case "CLOSED":
		return "closed", nil
	default:
		return "", fmt.Errorf("unexpected state %q", pr.State)
	}
}

func cloneReviewSeen(seen map[string]time.Time) map[string]time.Time {
	cloned := make(map[string]time.Time, len(seen))
	for key, timestamp := range seen {
		cloned[key] = timestamp
	}
	return cloned
}

func (p *reviewsPoll) observe(timestamp time.Time) {
	if timestamp.After(p.watermark) {
		p.watermark = timestamp
	}
}

func (p *reviewsPoll) addComment(target *prTarget, comment ghPRComment, prefix, kind, locus string) {
	p.addEvent(reviewEvent{
		target:    target,
		key:       fmt.Sprintf("%s%d", prefix, comment.ID),
		kind:      kind,
		author:    comment.User.Login,
		locus:     locus,
		body:      comment.Body,
		htmlURL:   comment.HTMLURL,
		id:        comment.ID,
		timestamp: comment.UpdatedAt,
	})
}

func (p *reviewsPoll) addEvent(event reviewEvent) {
	if event.timestamp.Before(p.target.watermark) {
		return
	}
	previous, known := p.seen[event.key]
	if known && !event.timestamp.After(previous) {
		return
	}
	event.edited = known
	p.seen[event.key] = event.timestamp
	p.events = append(p.events, event)
}

func inlineLocus(comment ghPRComment) string {
	if comment.Line == nil {
		return comment.Path + " (outdated)"
	}
	return fmt.Sprintf("%s:%d", comment.Path, *comment.Line)
}

// sanitizeReviewBody strips ANSI escapes and C0 control characters (besides
// \n and \t) from a PR comment body before it reaches the terminal.
func sanitizeReviewBody(body string) string {
	body = ansiRE.ReplaceAllString(body, "")
	return strings.Map(func(r rune) rune {
		if r < 0x20 && r != '\n' && r != '\t' {
			return -1
		}
		return r
	}, body)
}

func writeReviewEvent(w io.Writer, event reviewEvent, budget int) error {
	parts := []string{event.kind, event.author, fmt.Sprintf("pr#%d", event.target.Number)}
	if event.locus != "" {
		parts = append(parts, event.locus)
	}
	if event.edited {
		parts = append(parts, "edited")
	}
	parts = append(parts, event.timestamp.UTC().Format(time.RFC3339))
	if _, err := fmt.Fprintf(w, "◆ %s\n", strings.Join(parts, shipSep)); err != nil {
		return fmt.Errorf("reviews: write event header: %w", err)
	}
	if body := strings.TrimRight(render.Cap(sanitizeReviewBody(event.body), budget), "\n"); body != "" {
		if _, err := fmt.Fprintf(w, "  %s\n", strings.ReplaceAll(body, "\n", "\n  ")); err != nil {
			return fmt.Errorf("reviews: write event body: %w", err)
		}
	}
	if _, err := fmt.Fprintf(w, "↳ %s%sid %d\n", event.htmlURL, shipSep, event.id); err != nil {
		return fmt.Errorf("reviews: write event footer: %w", err)
	}
	if event.triage {
		if _, err := fmt.Fprintf(w,
			"↳ triage: spawn the cc-context:pr-review-triage agent with pr#%d and review id %d\n",
			event.target.Number, event.id); err != nil {
			return fmt.Errorf("reviews: write triage footer: %w", err)
		}
	}
	if _, err := fmt.Fprintln(w); err != nil {
		return fmt.Errorf("reviews: write event separator: %w", err)
	}
	return nil
}

func writeReviewsCancellation(w io.Writer, open []*prTarget, merged, closed int) error {
	numbers := make([]string, 0, len(open))
	watermark := open[0].watermark
	for _, target := range open {
		numbers = append(numbers, fmt.Sprintf("%d", target.Number))
		if target.watermark.Before(watermark) {
			watermark = target.watermark
		}
	}
	line := strings.Join([]string{
		"watch cancelled",
		fmt.Sprintf("%d open", len(open)),
		fmt.Sprintf("%d merged", merged),
		fmt.Sprintf("%d closed", closed),
		fmt.Sprintf("resume: ccx vcs reviews %s --since %s",
			strings.Join(numbers, " "), watermark.UTC().Format(time.RFC3339)),
	}, shipSep)
	if _, err := fmt.Fprintln(w, line); err != nil {
		return fmt.Errorf("reviews: write cancellation line: %w", err)
	}
	return nil
}
