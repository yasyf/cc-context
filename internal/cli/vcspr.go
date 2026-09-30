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
}

// prStateRoot is where every ccx process keeps the shared pull request cache;
// TestMain points it at a scratch directory.
var prStateRoot = prstate.DefaultRoot

func openPRState(ctx context.Context, repo string, warn io.Writer) (*prstate.Store, error) {
	src, err := prstate.NewGitHub(reviewsAPI(), gtAPI(ctx), repo, warn)
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
		Short: "Report whether each pull request is queued, evicted, not queued, or landed",
		Long: `Report whether each pull request is queued, evicted, not queued, or landed.

Pass all pull request numbers in one invocation. The command fetches their
statuses together and prints one result per pull request in input order.

The answer comes from Graphite's own record of the pull request, the one gt
reads, so a pull request enqueued from the Graphite web UI reads queued even
though it carries no merge label. A merge label is not the answer either way:
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
minutes. The queue closes what it
lands, so a landed pull request reads CLOSED with a null mergedAt on GitHub;
the squash is what settles it.

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
}

func newVcsPRStateCmd() *cobra.Command {
	var repo string
	var prefixes []string
	var wait time.Duration
	cmd := &cobra.Command{
		Use:   "state [<number>...]",
		Short: "Print the shared cache's record of each pull request and lane prefix as JSON",
		Long: `Print the machine-wide pull request cache's record of each named pull request,
and the open pull requests on branches under each --lane-prefix, as one JSON
object. Records are at most 30 seconds old; a read of anything older polls
GitHub once for every pull request any process on the machine watches in the
repository, the same poll ccx vcs pr watch and ccx vcs pr status share.

While GitHub rate-limits the machine the command fails naming the next probe,
unless --wait allows sitting it out.`,
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
	st, err := readPRStateWaiting(ctx, store, prstate.Want{PRs: numbers, Prefixes: prefixes}, wait)
	if err != nil {
		return fmt.Errorf("pr state: %w", err)
	}
	report := prStateReport{Repo: repo, PolledAt: st.PolledAt, Trunk: st.Trunk.Name, Lanes: map[string][]int{}, PRs: map[int]prstate.PR{}}
	for _, prefix := range prefixes {
		report.Lanes[prefix] = st.Lanes[prefix].PRs
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

func collectPRQueue(ctx context.Context, repo string, numbers []int, warn io.Writer) ([]prQueueReport, error) {
	store, err := openPRState(ctx, repo, warn)
	if err != nil {
		return nil, fmt.Errorf("pr status: %w", err)
	}
	st, err := readPRStateWaiting(ctx, store, prstate.Want{PRs: numbers}, prStatusRateLimitWait)
	if err != nil {
		return nil, fmt.Errorf("pr status: %w", err)
	}
	reports := make([]prQueueReport, 0, len(numbers))
	for _, number := range numbers {
		pr := st.PRs[number]
		if pr.Graphite == nil {
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
		var activity string
		if landedOn == "" && info.State == gtapi.PROpen && (prInGraphiteMq(info) || pr.QueueLabelled()) {
			activity = pr.Activity
		}
		r := classifyPRQueue(info, landedOn, activity)
		r.Conflicting = r.Queue == prQueueEvicted && pr.MergeStateStatus == "DIRTY"
		reports = append(reports, r)
	}
	return reports, nil
}

// readPRStateWaiting reads through store, sitting out GitHub's rate limit to
// each next probe for as long as wait allows.
func readPRStateWaiting(ctx context.Context, store *prstate.Store, want prstate.Want, wait time.Duration) (prstate.State, error) {
	deadline := time.Now().Add(wait)
	for {
		st, err := store.Read(ctx, want)
		var limited *prstate.LimitedError
		if !errors.As(err, &limited) || limited.ProbeAt.After(deadline) {
			return st, err
		}
		if err := sleepCtx(ctx, time.Until(limited.ProbeAt)); err != nil {
			return st, err
		}
	}
}

func prInGraphiteMq(info gtapi.PullRequestInfo) bool {
	return info.MergeQueueStatus != nil && info.MergeQueueStatus.IsInGraphiteMq
}

// classifyPRQueue settles one pull request's queue state, where landedOn is the
// branch its squash is on and activity its merge activity comment. Landed is
// checked first because Graphite keeps isInGraphiteMq set on a pull request it
// has already merged; queued needs the pull request still open for the same
// reason, and no exit in the activity since its last admission because the
// flag also outlives an eviction.
func classifyPRQueue(info gtapi.PullRequestInfo, landedOn, activity string) prQueueReport {
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
	case out && exit.Reason != "":
		r.Queue = prQueueEvicted
		r.Evicted = exit.Reason
		r.EvictedAt = exit.At
	case !out && prInGraphiteMq(info):
		r.Queue = prQueueQueued
		r.Enqueued = info.MergeQueueStatus.EnqueuedCommit
	}
	return r
}

func renderPRQueue(reports []prQueueReport) string {
	var b strings.Builder
	for _, r := range reports {
		fmt.Fprintf(&b, "#%d  %s", r.Number, r.Queue)
		switch r.Queue {
		case prQueueLanded:
			fmt.Fprintf(&b, " · squash %s on %s", shortSHA(r.Squash), r.Base)
		case prQueueQueued:
			fmt.Fprintf(&b, " · enqueued %s into %s", shortSHA(r.Enqueued), r.Base)
		case prQueueEvicted:
			fmt.Fprintf(&b, ": %s at %s", r.Evicted, r.EvictedAt)
			if r.Conflicting {
				b.WriteString(" · conflicting")
			}
		case prQueueNotQueued:
			fmt.Fprintf(&b, " · %s", strings.ToLower(r.State))
		}
		b.WriteString("\n")
	}
	return b.String()
}
