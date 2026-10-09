package cli

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

	"github.com/spf13/cobra"

	"github.com/yasyf/cc-context/internal/ghapi"
	"github.com/yasyf/cc-context/internal/gtapi"
	"github.com/yasyf/cc-context/internal/prstate"
	"github.com/yasyf/cc-context/internal/render"
	"github.com/yasyf/cc-context/internal/vcs"
)

// prQueueState is where a pull request stands against the Graphite merge queue.
type prQueueState string

const (
	prQueueQueued    prQueueState = "queued"
	prQueueNotQueued prQueueState = "not queued"
	prQueueLanded    prQueueState = "landed"
	prQueueEvicted   prQueueState = "evicted"
)

type prCIState string

const (
	prCIGreen   prCIState = "green"
	prCIRed     prCIState = "red"
	prCIPending prCIState = "pending"
	prCINone    prCIState = "none"
)

type prApprovalState string

const (
	prApproved         prApprovalState = "approved"
	prChangesRequested prApprovalState = "changes-requested"
	prReviewRequired   prApprovalState = "review-required"
	prNoReview         prApprovalState = "none"
)

const prVerdictLandable = "landable"

const (
	gtMergeabilityCheck  = "Graphite / mergeability_check"
	gtRestackPrefix      = "NEEDS_RESTACK"
	gtParentLanded       = "NEEDS_RESTACK__BASE_BRANCH_MERGED"
	gtWaitingOnDownstack = "WAITING_ON_DOWNSTACK"
)

const prNamedFailures = 3

var prStatusRateLimitWait = 10 * time.Minute

// prQueueReport is one pull request's queue state, with the evidence that
// decided it: the commit the queue admitted, the squash it wrote on the base,
// or the reason and time it evicted the pull request.
type prQueueReport struct {
	Number      int          `json:"number"`
	Queue       prQueueState `json:"queue"`
	State       string       `json:"state"`
	Base        string       `json:"base"`
	Enqueued    string       `json:"enqueued,omitempty"`
	Squash      string       `json:"squash,omitempty"`
	Evicted     string       `json:"evicted,omitempty"`
	EvictedAt   string       `json:"evicted_at,omitempty"`
	Conflicting bool         `json:"conflicting,omitempty"`
	Stale       *prStale     `json:"stale,omitempty"`
}

type prStatusReport struct {
	prQueueReport
	CI           *prCIReport      `json:"ci,omitempty"`
	Approval     prApprovalReport `json:"approval"`
	Mergeability string           `json:"mergeability,omitempty"`
	Downstack    int              `json:"downstack,omitempty"`
	Verdict      string           `json:"verdict"`
}

type prCIReport struct {
	State   prCIState `json:"state"`
	Failing []string  `json:"failing,omitempty"`
	Running int       `json:"running,omitempty"`
	Rollup  string    `json:"rollup,omitempty"`
}

type prApprovalReport struct {
	State            prApprovalState `json:"state"`
	Approvers        []string        `json:"approvers,omitempty"`
	ChangesRequested []string        `json:"changes_requested_by,omitempty"`
}

// prStale marks a record the cache served while GitHub's rate limit refused a
// poll: it is as old as PolledAt, and nothing polls again before ProbeAt.
type prStale struct {
	PolledAt time.Time `json:"polledAt"`
	ProbeAt  time.Time `json:"probeAt"`
	Reason   string    `json:"reason"`
}

func (s *prStale) String() string {
	return "stale, polled " + s.PolledAt.UTC().Format(time.RFC3339)
}

func staleAt(limited *prstate.Backoff, polledAt time.Time) *prStale {
	if limited == nil {
		return nil
	}
	return &prStale{PolledAt: polledAt, ProbeAt: limited.ProbeAt, Reason: limited.Reason}
}

// prStateRoot is where every ccx process keeps the shared pull request cache;
// TestMain points it at a scratch directory.
var prStateRoot = prstate.DefaultRoot

func openPRState(ctx context.Context, repo string, warn io.Writer) (*prstate.Store, error) {
	src, err := prstate.NewGitHub(reviewsAPI().ForRepo(repo), gtAPI(ctx), repo, warn)
	if err != nil {
		return nil, err
	}
	root, err := prStateRoot()
	if err != nil {
		return nil, err
	}
	return prstate.Open(root, src)
}

type vcsPRStatusOpts struct {
	json bool
	repo string
}

func newVcsPRCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "pr",
		Short: "Answer questions about pull requests by number",
		Args:  cobra.NoArgs,
		RunE:  groupHelp,
	}
	cmd.AddCommand(newVcsPRStatusCmd(), newVcsPRWatchCmd(), newVcsPRStateCmd())
	return cmd
}

func newVcsPRStatusCmd() *cobra.Command {
	var o vcsPRStatusOpts
	cmd := &cobra.Command{
		Use:   "status <number>...",
		Short: "Report each pull request's queue state, CI, approval, and whether it can land",
		Long: `Report whether each pull request is queued, evicted, not queued, or landed,
along with the checks on its head, its approval, and a one-word verdict.

Pass all pull request numbers in one invocation. The command fetches their
statuses together and prints one line per pull request in input order.

CI is red when any check on the head failed, naming the first few, pending
while any has not finished, green once something passed and nothing failed or
runs, and none when nothing graded the head. Graphite's mergeability check is
not counted: Graphite holds it in progress while the pull request waits on its
stack. A landed pull request reports no CI: the queue graded it before landing,
and checks that finish or cancel after the queue closes it grade nothing.
Approval is GitHub's reviewDecision, which counts the reviews the base
branch's protection counts, bot approvals included, followed by who approved or
requested changes. The verdict is landed, queued, landable for an open pull
request that is green, approved, not a draft, and not conflicting, and
otherwise blocked: followed by every cause, such as blocked:conflict,ci-pending.
Conflicting is GitHub's mergeable answer. While the cache holds it UNKNOWN, or
serves it stale under a GraphQL rate limit, the command asks GitHub's REST pull
endpoint instead, and one still uncomputed reads blocked:mergeable-unknown.
Graphite's own merge state adds needs-restack (parent landed), needs-restack,
or waiting-on-downstack #N, naming the pull request below.

The answer comes from Graphite's own record of the pull request, the one gt
reads, and its mergeability status, so a pull request enqueued from the
Graphite web UI reads queued even though it carries no merge label. A status
of QUEUED_TO_MERGE, WAITING_TO_MERGE, or FAILURE_HANDLING reads queued
outright. During failure handling, while the queue retests a pull request whose
batch failed CI, Graphite's record drops its queue flag; the line reads
"queued (failure handling)". A merge label is not the answer either way:
the queue consumes it on admission, and a label left on a pull request the
queue dropped means nothing.

Graphite's record lags an eviction, so queued also needs the pull request's
merge activity comment to agree: an eviction or dequeue after its last
admission settles it. An evicted pull request names the queue's reason and
time, and reads conflicting when GitHub reports its branch dirty. The comment
counts only for a pull request Graphite holds in the queue or one carrying a
merge label.

Landed means the squash commit Graphite recorded is reachable from the base
branch on GitHub, or from the default branch, as a stacked pull request's is
once its parent lands.

Every read goes through the machine-wide pull request cache ccx vcs pr watch
shares, polled at most once per 30 seconds per repository. While GitHub
rate-limits the machine the command waits for the next probe, up to ten
minutes, then answers from the cache with each line marked "stale, polled
<time>"; a pull request the cache never polled fails naming the next probe.
The queue closes what it lands, so a landed pull request reads CLOSED with a
null mergedAt on GitHub; the squash is what settles it.

A queued pull request names the commit the queue admitted. A push after
admission does not move it: the queue lands that commit and drops the rest.`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runVcsPRStatus(cmd, args, o)
		},
	}
	cmd.Flags().BoolVar(&o.json, "json", false, "emit the report as JSON")
	cmd.Flags().StringVarP(&o.repo, "repo", "R", "", "the owner/name repository (default: the current checkout's)")
	return cmd
}

type prStateReport struct {
	Repo     string             `json:"repo"`
	PolledAt time.Time          `json:"polledAt"`
	Trunk    string             `json:"trunk"`
	Lanes    map[string][]int   `json:"lanes"`
	PRs      map[int]prstate.PR `json:"prs"`
	Stale    *prStale           `json:"stale,omitempty"`
}

func newVcsPRStateCmd() *cobra.Command {
	var repo string
	var prefixes []string
	var wait time.Duration
	cmd := &cobra.Command{
		Use:   "state [<number>...]",
		Short: "Print the shared cache's record of each pull request and lane prefix as JSON",
		Long: `Print the machine-wide pull request cache's record of each named pull request,
and of each open pull request on a branch under a --lane-prefix, as one JSON
object. Records are at most 30 seconds old; a read of anything older polls
GitHub once for every pull request any process on the machine watches in the
repository, the same poll ccx vcs pr watch and ccx vcs pr status share.

While GitHub rate-limits the machine the command sits it out for as long as
--wait allows, then serves the cached records with a top-level "stale" object
naming when they were polled and when the next probe is; records the cache
never held fail naming the next probe.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runVcsPRState(cmd, args, repo, prefixes, wait)
		},
	}
	cmd.Flags().StringVarP(&repo, "repo", "R", "", "the owner/name repository (default: the current checkout's)")
	cmd.Flags().StringArrayVar(&prefixes, "lane-prefix", nil, "also list the open pull requests on branches under this prefix; repeatable")
	cmd.Flags().DurationVar(&wait, "wait", 0, "how long to sit out a GitHub rate limit before failing")
	return cmd
}

func runVcsPRState(cmd *cobra.Command, args []string, repo string, prefixes []string, wait time.Duration) error {
	ctx := cmd.Context()
	numbers, err := prNumbers(args)
	if err != nil {
		return fmt.Errorf("pr state: %w", err)
	}
	if len(numbers) == 0 && len(prefixes) == 0 {
		return errors.New("pr state: name pull requests or --lane-prefix")
	}
	if repo == "" {
		looked, err := vcs.LookupRepo(ctx, render.Dir(workingDir(ctx)), false)
		if err != nil {
			return fmt.Errorf("pr state: name the repository with --repo: %w", err)
		}
		repo = looked.NameWithOwner
	}
	store, err := openPRState(ctx, repo, cmd.ErrOrStderr())
	if err != nil {
		return fmt.Errorf("pr state: %w", err)
	}
	st, limited, err := readPRStateWaiting(ctx, store, prstate.Want{PRs: numbers, Prefixes: prefixes}, wait)
	if err != nil {
		return fmt.Errorf("pr state: %w", err)
	}
	report := prStateReport{Repo: repo, PolledAt: st.PolledAt, Trunk: st.Trunk.Name, Lanes: map[string][]int{}, PRs: map[int]prstate.PR{}, Stale: staleAt(limited, st.PolledAt)}
	for _, prefix := range prefixes {
		report.Lanes[prefix] = st.Lanes[prefix].PRs
		numbers = append(numbers, st.Lanes[prefix].PRs...)
	}
	for _, number := range numbers {
		report.PRs[number] = st.PRs[number]
	}
	data, err := json.Marshal(report)
	if err != nil {
		return fmt.Errorf("pr state: encode: %w", err)
	}
	cmd.Println(string(data))
	return nil
}

func runVcsPRStatus(cmd *cobra.Command, args []string, o vcsPRStatusOpts) error {
	ctx := cmd.Context()
	numbers := make([]int, 0, len(args))
	for _, arg := range args {
		n, err := strconv.Atoi(strings.TrimPrefix(arg, "#"))
		if err != nil || n <= 0 {
			return fmt.Errorf("pr status: %q is not a pull request number", arg)
		}
		numbers = append(numbers, n)
	}
	repo := o.repo
	if repo == "" {
		looked, err := vcs.LookupRepo(ctx, render.Dir(workingDir(ctx)), false)
		if err != nil {
			return fmt.Errorf("pr status: name the repository with --repo: %w", err)
		}
		repo = looked.NameWithOwner
	}
	reports, err := collectPRQueue(ctx, repo, numbers, cmd.ErrOrStderr())
	if err != nil {
		return err
	}
	if o.json {
		data, err := json.MarshalIndent(reports, "", "  ")
		if err != nil {
			return fmt.Errorf("pr status: marshal report: %w", err)
		}
		cmd.Println(string(data))
		return nil
	}
	cmd.Print(renderPRQueue(reports))
	return nil
}

func collectPRQueue(ctx context.Context, repo string, numbers []int, warn io.Writer) ([]prStatusReport, error) {
	st, limited, err := readPRQueue(ctx, repo, numbers, warn, prStatusRateLimitWait)
	if err != nil {
		return nil, fmt.Errorf("pr status: %w", err)
	}
	reports := make([]prStatusReport, 0, len(numbers))
	for _, number := range numbers {
		pr := st.PRs[number]
		if pr.Graphite == nil {
			if limited != nil {
				return nil, fmt.Errorf("pr status: the cache has never polled %s#%d, and github %s; next probe at %s", repo, number, limited.Reason, limited.ProbeAt.UTC().Format(time.RFC3339))
			}
			return nil, fmt.Errorf("pr status: graphite has no record of %s#%d", repo, number)
		}
		info := *pr.Graphite
		landedOn := ""
		switch {
		case slices.Contains(pr.SquashOn, info.BaseRefName):
			landedOn = info.BaseRefName
		case len(pr.SquashOn) > 0:
			landedOn = pr.SquashOn[0]
		}
		r := prQueueOf(info, pr, landedOn)
		if r.State == string(gtapi.PROpen) && (r.Queue == prQueueNotQueued || r.Queue == prQueueEvicted) && (limited != nil || pr.Mergeable == statusUnknown) {
			mergeable, state, err := prReadMergeable(ctx, repo, number)
			if err != nil {
				_, _ = fmt.Fprintf(warn, "pr status: #%d: %v\n", number, err)
			} else {
				pr.Mergeable, pr.MergeStateStatus = mergeable, state
			}
		}
		r.Conflicting = r.Queue == prQueueEvicted && pr.MergeStateStatus == "DIRTY"
		r.Stale = staleAt(limited, pr.PolledAt)
		report := prStatusReport{prQueueReport: r, Approval: prApprovalOf(pr), Mergeability: pr.Mergeability, Downstack: info.DependentPRNumber}
		if r.Queue != prQueueLanded {
			ci := prCIOf(pr.Rollup)
			report.CI = &ci
		}
		report.Verdict = prVerdict(report, pr)
		reports = append(reports, report)
	}
	return reports, nil
}

// prReadMergeable asks GitHub's REST pull endpoint for a pull request's
// mergeability, on the REST quota a rate-limited GraphQL budget leaves alone.
// GitHub answers null until it computes the merge commit the first ask
// schedules, so an unknown answer is asked once more.
func prReadMergeable(ctx context.Context, repo string, number int) (string, string, error) {
	client := reviewsAPI().ForRepo(repo)
	for asked := 1; ; asked++ {
		pull, err := ghapi.Get[ghPull](ctx, client, ghPullPath(repo, number))
		if err != nil {
			return "", "", fmt.Errorf("read the mergeability from REST: %w", err)
		}
		if pull.Mergeable != nil || asked == 2 {
			return pull.mergeable(), strings.ToUpper(pull.MergeableState), nil
		}
		if err := sleepCtx(ctx, statusMergeableRetry); err != nil {
			return "", "", err
		}
	}
}

func readPRQueue(ctx context.Context, repo string, numbers []int, warn io.Writer, wait time.Duration) (prstate.State, *prstate.Backoff, error) {
	store, err := openPRState(ctx, repo, warn)
	if err != nil {
		return prstate.State{}, nil, err
	}
	return readPRStateWaiting(ctx, store, prstate.Want{PRs: numbers}, wait)
}

func prQueueOf(info gtapi.PullRequestInfo, pr prstate.PR, landedOn string) prQueueReport {
	var activity string
	if landedOn == "" && info.State == gtapi.PROpen && (gtapi.InMergeQueue(&info, pr.Mergeability) || pr.QueueLabelled()) {
		activity = pr.Activity
	}
	return classifyPRQueue(info, pr.Mergeability, landedOn, activity)
}

func prCIOf(rollup *prstate.Rollup) prCIReport {
	checks := statusChecks(rollup)
	r := prCIReport{State: prCINone}
	for _, c := range checks {
		switch statusClassify(c.State) {
		case statusFailed:
			r.Failing = append(r.Failing, c.Name)
		case statusRunning:
			r.Running++
		}
	}
	switch {
	case len(r.Failing) > 0:
		r.State = prCIRed
	case r.Running > 0:
		r.State = prCIPending
	case rollup != nil && statusClassify(rollup.State) == statusFailed:
		r.State, r.Rollup = prCIRed, rollup.State
	case statusGraded(checks) > 0:
		r.State = prCIGreen
	}
	return r
}

func prApprovalOf(pr prstate.PR) prApprovalReport {
	var r prApprovalReport
	for _, review := range pr.Reviews {
		switch review.State {
		case "APPROVED":
			r.Approvers = append(r.Approvers, review.Author)
		case "CHANGES_REQUESTED":
			r.ChangesRequested = append(r.ChangesRequested, review.Author)
		}
	}
	switch {
	case pr.ReviewDecision == "APPROVED":
		r.State = prApproved
	case pr.ReviewDecision == "CHANGES_REQUESTED":
		r.State = prChangesRequested
	case pr.ReviewDecision == "REVIEW_REQUIRED":
		r.State = prReviewRequired
	case len(r.ChangesRequested) > 0:
		r.State = prChangesRequested
	case len(r.Approvers) > 0:
		r.State = prApproved
	default:
		r.State = prNoReview
	}
	return r
}

func prVerdict(r prStatusReport, pr prstate.PR) string {
	switch {
	case r.Queue == prQueueLanded:
		return string(prQueueLanded)
	case r.Queue == prQueueQueued:
		return string(prQueueQueued)
	case r.State != "OPEN":
		return "blocked:" + strings.ToLower(r.State)
	}
	var causes []string
	if pr.Draft {
		causes = append(causes, "draft")
	}
	switch {
	case pr.Mergeable == "CONFLICTING" || pr.MergeStateStatus == "DIRTY" || r.Conflicting:
		causes = append(causes, "conflict")
	case pr.Mergeable == statusUnknown:
		causes = append(causes, "mergeable-unknown")
	}
	if cause := prGraphiteCause(r.Mergeability, r.Downstack); cause != "" {
		causes = append(causes, cause)
	}
	switch r.CI.State {
	case prCIRed:
		causes = append(causes, "ci-red")
	case prCIPending:
		causes = append(causes, "ci-pending")
	case prCINone:
		causes = append(causes, "no-ci")
	}
	switch r.Approval.State {
	case prChangesRequested:
		causes = append(causes, "changes-requested")
	case prReviewRequired, prNoReview:
		causes = append(causes, "unapproved")
	}
	if len(causes) == 0 {
		return prVerdictLandable
	}
	return "blocked:" + strings.Join(causes, ",")
}

func prGraphiteCause(mergeability string, downstack int) string {
	switch {
	case mergeability == gtParentLanded:
		return "needs-restack (parent landed)"
	case strings.HasPrefix(mergeability, gtRestackPrefix):
		return "needs-restack"
	case mergeability == gtWaitingOnDownstack && downstack != 0:
		return fmt.Sprintf("waiting-on-downstack #%d", downstack)
	case mergeability == gtWaitingOnDownstack:
		return "waiting-on-downstack"
	}
	return ""
}

// readPRStateWaiting reads through store, sitting out GitHub's rate limit to
// each next probe for as long as wait allows. A refusal past that returns the
// cached state with the backoff that refused it when the cache covers want,
// and the refusal itself when it does not.
func readPRStateWaiting(ctx context.Context, store *prstate.Store, want prstate.Want, wait time.Duration) (prstate.State, *prstate.Backoff, error) {
	deadline := time.Now().Add(wait)
	for {
		st, err := store.Read(ctx, want)
		var limited *prstate.LimitedError
		switch {
		case !errors.As(err, &limited):
			return st, nil, err
		case limited.ProbeAt.After(deadline) && st.Covers(want):
			return st, &limited.Backoff, nil
		case limited.ProbeAt.After(deadline):
			return st, nil, err
		}
		if err := sleepCtx(ctx, time.Until(limited.ProbeAt)); err != nil {
			return st, nil, err
		}
	}
}

// classifyPRQueue settles one pull request's queue state, where mergeability is
// its Graphite mergeability status, landedOn the branch its squash is on, and
// activity its merge activity comment. Landed is checked first because
// Graphite keeps isInGraphiteMq set on a pull request it has already merged;
// queued needs the pull request still open for the same reason. A queued
// mergeability status settles queued outright; the flag alone also needs no
// exit in the activity since its last admission, since it outlives an
// eviction too.
func classifyPRQueue(info gtapi.PullRequestInfo, mergeability, landedOn, activity string) prQueueReport {
	r := prQueueReport{Number: info.PRNumber, Queue: prQueueNotQueued, State: string(info.State), Base: info.BaseRefName}
	if landedOn != "" {
		r.Queue = prQueueLanded
		r.Base = landedOn
		r.Squash = info.MergeCommitSha
		return r
	}
	if info.State != gtapi.PROpen {
		return r
	}
	exit, out := mqLastExit(activity)
	switch {
	case gtapi.QueuedMergeability(mergeability), !out && gtapi.InMergeQueue(&info, ""):
		r.Queue = prQueueQueued
		if info.MergeQueueStatus != nil {
			r.Enqueued = info.MergeQueueStatus.EnqueuedCommit
		}
	case out && exit.Reason != "":
		r.Queue = prQueueEvicted
		r.Evicted = exit.Reason
		r.EvictedAt = exit.At
	}
	return r
}

func renderPRQueue(reports []prStatusReport) string {
	var b strings.Builder
	for _, r := range reports {
		fmt.Fprintf(&b, "#%d  %s", r.Number, r.Queue)
		switch r.Queue {
		case prQueueLanded:
			fmt.Fprintf(&b, " · squash %s on %s", shortSHA(r.Squash), r.Base)
		case prQueueQueued:
			if r.Mergeability == gtapi.MergeabilityFailureHandling {
				b.WriteString(" (failure handling)")
			}
			if r.Enqueued == "" {
				fmt.Fprintf(&b, " · into %s", r.Base)
			} else {
				fmt.Fprintf(&b, " · enqueued %s into %s", shortSHA(r.Enqueued), r.Base)
			}
		case prQueueEvicted:
			fmt.Fprintf(&b, ": %s at %s", r.Evicted, r.EvictedAt)
			if r.Conflicting {
				b.WriteString(" · conflicting")
			}
		case prQueueNotQueued:
			fmt.Fprintf(&b, " · %s", strings.ToLower(r.State))
		}
		if r.CI != nil {
			b.WriteString(shipSep + prCIValue(*r.CI))
		}
		b.WriteString(shipSep + prApprovalValue(r.Approval) + shipSep + r.Verdict)
		if r.Stale != nil {
			b.WriteString(" · " + r.Stale.String())
		}
		b.WriteString("\n")
	}
	return b.String()
}

func prCIValue(ci prCIReport) string {
	switch {
	case len(ci.Failing) > 0:
		named, rest := ci.Failing, ""
		if len(named) > prNamedFailures {
			named, rest = named[:prNamedFailures], fmt.Sprintf(" +%d more", len(ci.Failing)-prNamedFailures)
		}
		return "ci red: " + strings.Join(named, ", ") + rest
	case ci.Rollup != "":
		return "ci red: rollup " + strings.ToLower(ci.Rollup)
	case ci.State == prCIPending:
		return fmt.Sprintf("ci pending: %d running", ci.Running)
	}
	return "ci " + string(ci.State)
}

func prApprovalValue(a prApprovalReport) string {
	switch a.State {
	case prApproved:
		if len(a.Approvers) == 0 {
			return "approved"
		}
		return "approved by " + strings.Join(a.Approvers, ", ")
	case prChangesRequested:
		return "changes requested by " + strings.Join(a.ChangesRequested, ", ")
	case prReviewRequired:
		if len(a.Approvers) == 0 {
			return "review required"
		}
		return "review required, approved by " + strings.Join(a.Approvers, ", ")
	}
	return "no review"
}
