package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/yasyf/cc-context/internal/render"
	"github.com/yasyf/cc-context/internal/vcs"
)

const (
	reviewsPollDefault = 30 * time.Second
	reviewsBodyBudget  = 500
	reviewsMaxFails    = 5

	envReviewsPollInterval = "CCX_REVIEWS_POLL_INTERVAL"
)

var errBadReviewsPollInterval = errors.New("invalid CCX_REVIEWS_POLL_INTERVAL")

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

type ghPRView struct {
	Number   int        `json:"number"`
	State    string     `json:"state"`
	URL      string     `json:"url"`
	MergedAt *time.Time `json:"mergedAt"`
}

type prTarget struct {
	Number    int
	URL       string
	watermark time.Time
	seen      map[string]time.Time
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
		Args:  cobra.ArbitraryArgs,
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

			var targets []*prTarget
			if stack {
				targets, err = resolveStackReviewTargets(cmd.Context(), cmd.OutOrStdout(), since)
			} else {
				targets, err = resolveReviewTargets(cmd.Context(), args, since)
			}
			if err != nil {
				return err
			}
			return watchReviews(cmd.Context(), cmd.OutOrStdout(), targets, o)
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

func resolveReviewTargets(ctx context.Context, operands []string, since time.Time) ([]*prTarget, error) {
	if _, err := exec.LookPath("gh"); err != nil {
		return nil, fmt.Errorf("reviews: gh: %w", err)
	}
	if len(operands) == 0 {
		operands = []string{""}
	}
	targets := make([]*prTarget, 0, len(operands))
	for _, operand := range operands {
		view, err := viewReviewTarget(ctx, operand)
		if err != nil {
			return nil, err
		}
		targets = append(targets, &prTarget{
			Number:    view.Number,
			URL:       view.URL,
			watermark: since,
			seen:      map[string]time.Time{},
		})
	}
	return targets, nil
}

func viewReviewTarget(ctx context.Context, operand string) (ghPRView, error) {
	argv := []string{"pr", "view"}
	label := "current branch"
	if operand != "" {
		argv = append(argv, operand)
		label = operand
	}
	argv = append(argv, "--json", "number,state,url,mergedAt")
	out, err := render.RunCLI(ctx, "gh", argv)
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "no pull requests found") {
			return ghPRView{}, fmt.Errorf("reviews: resolve %s: %w: %w", label, ErrNotFound, err)
		}
		return ghPRView{}, fmt.Errorf("reviews: resolve %s: %w", label, err)
	}
	var view ghPRView
	if err := json.Unmarshal([]byte(out), &view); err != nil {
		return ghPRView{}, fmt.Errorf("reviews: parse gh pr view %s: %w", label, err)
	}
	return view, nil
}

// resolveStackReviewTargets resolves review targets from the current
// graphite downstack, skipping (with a note to w) a branch with no open PR
// rather than failing the whole command.
func resolveStackReviewTargets(ctx context.Context, w io.Writer, since time.Time) ([]*prTarget, error) {
	kind, root := vcs.DetectRoot(workingDir())
	if (kind != vcs.Git && kind != vcs.JJ) || !vcs.GraphiteRepo(root) {
		return nil, errors.New("reviews: --stack requires a graphite repo")
	}
	branches, err := stackBranches(ctx)
	if err != nil {
		return nil, err
	}
	return resolveBranchTargets(ctx, w, branches, since)
}

// resolveBranchTargets resolves each branch's open PR into a prTarget,
// skipping (with a note to w) any branch with none — shared by ship's
// --reviews wiring and reviews --stack.
func resolveBranchTargets(ctx context.Context, w io.Writer, branches []string, since time.Time) ([]*prTarget, error) {
	var targets []*prTarget
	for _, branch := range branches {
		view, err := viewReviewTarget(ctx, branch)
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				if _, werr := fmt.Fprintf(w, "reviews: no open PR for %s\n", branch); werr != nil {
					return nil, werr
				}
				continue
			}
			return nil, err
		}
		targets = append(targets, &prTarget{
			Number:    view.Number,
			URL:       view.URL,
			watermark: since,
			seen:      map[string]time.Time{},
		})
	}
	return targets, nil
}

// shipWatchReviews resolves each branch's open PR and watches all that
// resolve, for ship's --reviews flag. A branch with no open PR is skipped
// with a note rather than failing the already-succeeded ship.
func shipWatchReviews(ctx context.Context, w io.Writer, branches []string) error {
	if _, err := exec.LookPath("gh"); err != nil {
		_, err := fmt.Fprintln(w, "reviews: gh not found")
		return err
	}
	targets, err := resolveBranchTargets(ctx, w, branches, time.Now())
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
	return watchReviews(ctx, w, targets, reviewsOpts{interval: interval, budget: reviewsBodyBudget})
}

func ghPages[T any](ctx context.Context, path string) ([]T, error) {
	out, err := render.RunCLI(ctx, "gh", []string{"api", "--paginate", path})
	if err != nil {
		return nil, fmt.Errorf("reviews: gh api %s: %w", path, err)
	}
	decoder := json.NewDecoder(strings.NewReader(out))
	var items []T
	for {
		var page []T
		if err := decoder.Decode(&page); errors.Is(err, io.EOF) {
			return items, nil
		} else if err != nil {
			return nil, fmt.Errorf("reviews: parse gh api %s: %w", path, err)
		}
		items = append(items, page...)
	}
}

func watchReviews(ctx context.Context, w io.Writer, targets []*prTarget, o reviewsOpts) error {
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
	merged, closed := 0, 0
	failures := 0
	for len(open) > 0 {
		polls, err := pollReviewCycle(ctx, open, o)
		if ctx.Err() != nil {
			if werr := writeReviewsCancellation(w, open, merged, closed); werr != nil {
				return werr
			}
			return ctx.Err()
		}
		if err != nil {
			failures++
			slog.Warn("reviews: poll failed", "consecutive_failures", failures, "err", err)
			if failures >= reviewsMaxFails {
				return fmt.Errorf("reviews: poll failed %d consecutive cycles: %w", failures, err)
			}
		} else {
			failures = 0
			var events []reviewEvent
			for _, poll := range polls {
				events = append(events, poll.events...)
			}
			sort.SliceStable(events, func(i, j int) bool {
				return events[i].timestamp.Before(events[j].timestamp)
			})
			for _, event := range events {
				if err := writeReviewEvent(w, event, o.budget); err != nil {
					return err
				}
			}

			next := make([]*prTarget, 0, len(open))
			for _, poll := range polls {
				poll.target.watermark = poll.watermark
				poll.target.seen = poll.seen
				if poll.terminal == "" {
					next = append(next, poll.target)
					continue
				}
				if _, err := fmt.Fprintf(w, "◆ pr#%d %s%s%s\n\n",
					poll.target.Number, poll.terminal, shipSep, poll.target.URL); err != nil {
					return fmt.Errorf("reviews: write terminal line: %w", err)
				}
				if poll.terminal == "merged" {
					merged++
				} else {
					closed++
				}
			}
			open = next
		}

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
	if _, err := fmt.Fprintln(w, strings.Join([]string{
		fmt.Sprintf("watch done%s%d merged", shipSep, merged),
		fmt.Sprintf("%d closed", closed),
	}, shipSep)); err != nil {
		return fmt.Errorf("reviews: write completion line: %w", err)
	}
	return nil
}

func pollReviewCycle(ctx context.Context, targets []*prTarget, o reviewsOpts) ([]reviewsPoll, error) {
	polls := make([]reviewsPoll, 0, len(targets))
	for _, target := range targets {
		poll, err := pollReviewTarget(ctx, target, o)
		if err != nil {
			return nil, err
		}
		polls = append(polls, poll)
	}
	return polls, nil
}

func pollReviewTarget(ctx context.Context, target *prTarget, o reviewsOpts) (reviewsPoll, error) {
	base := fmt.Sprintf("repos/{owner}/{repo}/pulls/%d", target.Number)
	suffix := ""
	if !o.all {
		suffix = "&since=" + target.watermark.UTC().Format(time.RFC3339)
	}
	inline, err := ghPages[ghPRComment](ctx, base+"/comments?per_page=100"+suffix)
	if err != nil {
		return reviewsPoll{}, fmt.Errorf("reviews: pr#%d inline comments: %w", target.Number, err)
	}
	comments, err := ghPages[ghPRComment](ctx,
		fmt.Sprintf("repos/{owner}/{repo}/issues/%d/comments?per_page=100%s", target.Number, suffix))
	if err != nil {
		return reviewsPoll{}, fmt.Errorf("reviews: pr#%d issue comments: %w", target.Number, err)
	}
	reviews, err := ghPages[ghReview](ctx, base+"/reviews?per_page=100")
	if err != nil {
		return reviewsPoll{}, fmt.Errorf("reviews: pr#%d reviews: %w", target.Number, err)
	}
	view, err := viewReviewTarget(ctx, fmt.Sprintf("%d", target.Number))
	if err != nil {
		return reviewsPoll{}, err
	}
	terminal, err := reviewTerminalState(view)
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

func reviewTerminalState(view ghPRView) (string, error) {
	if view.MergedAt != nil || view.State == "MERGED" {
		return "merged", nil
	}
	switch view.State {
	case "OPEN":
		return "", nil
	case "CLOSED":
		return "closed", nil
	default:
		return "", fmt.Errorf("unexpected state %q", view.State)
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
	if body := strings.TrimRight(render.Cap(event.body, budget), "\n"); body != "" {
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
