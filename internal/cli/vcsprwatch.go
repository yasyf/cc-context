package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/yasyf/cc-context/internal/ghapi"
	"github.com/yasyf/cc-context/internal/gtapi"
	"github.com/yasyf/cc-context/internal/render"
	"github.com/yasyf/cc-context/internal/vcs"
)

const (
	prWatchUntilLanded = "landed"
	prWatchUntilClosed = "closed"
	prWatchUntilNever  = "never"

	prWatchDefaultInterval = 60 * time.Second
	prWatchMaxFails        = 10
	prWatchRateFloor       = 100
	prWatchRateFallback    = 5 * time.Minute
)

type prWatchSnapshot struct {
	State     string   `json:"state,omitempty"`
	Head      string   `json:"head,omitempty"`
	Mergeable string   `json:"mergeable,omitempty"`
	Green     bool     `json:"green,omitempty"`
	Failing   []string `json:"failing,omitempty"`
	Approved  bool     `json:"approved,omitempty"`
	Queued    bool     `json:"queued,omitempty"`
	Evicted   string   `json:"evicted,omitempty"`
	Squash    string   `json:"squash,omitempty"`
}

func (s prWatchSnapshot) terminal() bool { return s.State != "" && s.State != "OPEN" }

func (s prWatchSnapshot) landed() bool { return s.terminal() && s.Squash != "" }

type prWatchEvent struct {
	At     time.Time `json:"at"`
	PR     int       `json:"pr,omitempty"`
	Event  string    `json:"event"`
	Detail string    `json:"detail,omitempty"`
	Head   string    `json:"head,omitempty"`
}

func (e prWatchEvent) String() string {
	if e.PR == 0 {
		return strings.TrimSpace(e.Event + " " + e.Detail)
	}
	line := fmt.Sprintf("#%d %s", e.PR, e.Event)
	switch {
	case e.Detail == "":
	case e.Event == "ejected":
		line += " (" + e.Detail + ")"
	default:
		line += " " + e.Detail
	}
	return line
}

func sha9(sha string) string {
	if len(sha) <= 9 {
		return sha
	}
	return sha[:9]
}

func prWatchStep(number int, prev, next prWatchSnapshot) (prWatchSnapshot, []prWatchEvent) {
	// GitHub answers UNKNOWN until it recomputes mergeability, so UNKNOWN
	// carries the last verdict rather than resetting it.
	if next.Mergeable == "" || next.Mergeable == "UNKNOWN" {
		next.Mergeable = prev.Mergeable
	}
	var events []prWatchEvent
	emit := func(kind, detail string) {
		events = append(events, prWatchEvent{PR: number, Event: kind, Detail: detail, Head: next.Head})
	}
	moved := prev.Head != "" && next.Head != prev.Head
	if moved {
		emit("new-head", sha9(next.Head))
	}
	if next.terminal() {
		if next.landed() {
			emit("landed", sha9(next.Squash))
		} else {
			emit("closed-without-squash", "")
		}
		return next, events
	}
	switch {
	case next.Queued && !prev.Queued:
		emit("queued", "")
	case prev.Queued && !next.Queued:
		emit("ejected", next.Evicted)
	}
	if next.Mergeable == "CONFLICTING" && prev.Mergeable != "CONFLICTING" {
		emit("conflicting", "")
	}
	wasFailing, wasGreen := prev.Failing, prev.Green
	if moved {
		wasFailing, wasGreen = nil, false
	}
	if fresh := slices.DeleteFunc(slices.Clone(next.Failing), func(name string) bool {
		return slices.Contains(wasFailing, name)
	}); len(fresh) > 0 {
		emit("red", strings.Join(fresh, ", "))
	}
	if next.Green && !wasGreen {
		emit("green", "")
	}
	switch {
	case next.Approved && !prev.Approved:
		emit("approved", "")
	case prev.Approved && !next.Approved:
		emit("approval-dismissed", "")
	}
	return next, events
}

type prWatchTick struct {
	snapshots  map[int]prWatchSnapshot
	discovered []int
	remaining  int
	resetAt    time.Time
}

type prWatchPoller interface {
	poll(ctx context.Context, numbers []int, prev map[int]prWatchSnapshot) (prWatchTick, error)
}

type prWatchSource struct {
	gh     *ghapi.Client
	gt     *gtapi.Client
	owner  string
	name   string
	prefix string
	warn   io.Writer
}

type prWatchNode struct {
	Number         int    `json:"number"`
	State          string `json:"state"`
	HeadRefOid     string `json:"headRefOid"`
	Mergeable      string `json:"mergeable"`
	ReviewDecision string `json:"reviewDecision"`
	MergeCommit    *struct {
		OID string `json:"oid"`
	} `json:"mergeCommit"`
	Checks struct {
		Nodes []struct {
			Commit struct {
				StatusCheckRollup *statusRollup `json:"statusCheckRollup"`
			} `json:"commit"`
		} `json:"nodes"`
	} `json:"checks"`
	Comments *struct {
		Nodes []struct {
			Body string `json:"body"`
		} `json:"nodes"`
	} `json:"comments"`
}

type prWatchHistory struct {
	Target struct {
		History struct {
			Nodes []struct {
				OID             string `json:"oid"`
				MessageHeadline string `json:"messageHeadline"`
			} `json:"nodes"`
		} `json:"history"`
	} `json:"target"`
}

type prWatchRefs struct {
	Nodes []struct {
		Name                   string `json:"name"`
		AssociatedPullRequests struct {
			Nodes []struct {
				Number int `json:"number"`
			} `json:"nodes"`
		} `json:"associatedPullRequests"`
	} `json:"nodes"`
}

type prWatchCompare struct {
	Compare *struct {
		Status string `json:"status"`
	} `json:"compare"`
}

type prWatchResponse struct {
	RateLimit struct {
		Remaining int       `json:"remaining"`
		ResetAt   time.Time `json:"resetAt"`
	} `json:"rateLimit"`
	Repository map[string]json.RawMessage `json:"repository"`
}

const prWatchNodeFields = "number state headRefOid mergeable reviewDecision mergeCommit { oid } " +
	"checks: commits(last: 1) { nodes { commit { statusCheckRollup { state contexts(first: 100) { nodes { __typename " +
	"... on CheckRun { name conclusion status } ... on StatusContext { context state } } } } } } }"

func prWatchQuery(numbers []int, activity map[int]bool, squashes map[int]string, prefix bool) string {
	decls := []string{"$owner: String!", "$repo: String!"}
	var fields strings.Builder
	fields.WriteString("    trunk: defaultBranchRef { target { ... on Commit { history(first: 100) { nodes { oid messageHeadline } } } } }\n")
	if prefix {
		// associatedPullRequests answers empty under any refPrefix deeper than
		// refs/heads/, so the lane's prefix goes in query and is re-checked.
		decls = append(decls, "$lanePrefix: String!")
		fields.WriteString("    lane: refs(refPrefix: \"refs/heads/\", query: $lanePrefix, first: 100) { nodes { name associatedPullRequests(states: [OPEN], first: 5) { nodes { number } } } }\n")
	}
	for i, number := range numbers {
		decls = append(decls, fmt.Sprintf("$p%d: Int!", i))
		selection := prWatchNodeFields
		if activity[number] {
			selection += " comments(last: 100) { nodes { body } }"
		}
		fmt.Fprintf(&fields, "    p%d: pullRequest(number: $p%d) { %s }\n", i, i, selection)
		if _, ok := squashes[number]; ok {
			decls = append(decls, fmt.Sprintf("$m%d: String!", i))
			fmt.Fprintf(&fields, "    m%d: defaultBranchRef { compare(headRef: $m%d) { status } }\n", i, i)
		}
	}
	return fmt.Sprintf("query(%s) {\n  rateLimit { remaining resetAt }\n  repository(owner: $owner, name: $repo) {\n%s  }\n}",
		strings.Join(decls, ", "), fields.String())
}

func (s prWatchSource) poll(ctx context.Context, numbers []int, prev map[int]prWatchSnapshot) (prWatchTick, error) {
	infos := s.queueRecords(ctx, numbers)
	activity := map[int]bool{}
	squashes := map[int]string{}
	for _, number := range numbers {
		info, ok := infos[number]
		if prev[number].Queued || ok && info.State == gtapi.PROpen && prInGraphiteMq(info) {
			activity[number] = true
		}
		if ok && info.MergeCommitSha != "" && info.State != gtapi.PROpen {
			squashes[number] = info.MergeCommitSha
		}
	}
	vars := map[string]any{"owner": s.owner, "repo": s.name}
	if s.prefix != "" {
		vars["lanePrefix"] = s.prefix
	}
	for i, number := range numbers {
		vars[fmt.Sprintf("p%d", i)] = number
		if sha, ok := squashes[number]; ok {
			vars[fmt.Sprintf("m%d", i)] = sha
		}
	}
	resp, err := ghapi.GraphQL[prWatchResponse](ctx, s.gh, prWatchQuery(numbers, activity, squashes, s.prefix != ""), vars)
	if err != nil {
		return prWatchTick{}, err
	}
	tick := prWatchTick{
		snapshots: make(map[int]prWatchSnapshot, len(numbers)),
		remaining: resp.RateLimit.Remaining,
		resetAt:   resp.RateLimit.ResetAt,
	}
	var trunk prWatchHistory
	if err := decodeRaw(resp.Repository["trunk"], &trunk); err != nil {
		return prWatchTick{}, err
	}
	if s.prefix != "" {
		var refs prWatchRefs
		if err := decodeRaw(resp.Repository["lane"], &refs); err != nil {
			return prWatchTick{}, err
		}
		for _, ref := range refs.Nodes {
			if !strings.HasPrefix(ref.Name, s.prefix) {
				continue
			}
			for _, pr := range ref.AssociatedPullRequests.Nodes {
				tick.discovered = append(tick.discovered, pr.Number)
			}
		}
	}
	for i, number := range numbers {
		var node prWatchNode
		if err := decodeRaw(resp.Repository[fmt.Sprintf("p%d", i)], &node); err != nil {
			return prWatchTick{}, err
		}
		var onTrunk bool
		if _, ok := squashes[number]; ok {
			var compared prWatchCompare
			if err := decodeRaw(resp.Repository[fmt.Sprintf("m%d", i)], &compared); err != nil {
				return prWatchTick{}, err
			}
			onTrunk = compared.Compare != nil && (compared.Compare.Status == "BEHIND" || compared.Compare.Status == "IDENTICAL")
		}
		info, known := infos[number]
		tick.snapshots[number] = prWatchSnapshotOf(number, node, trunk, info, known, onTrunk, prev[number])
	}
	return tick, nil
}

func (s prWatchSource) queueRecords(ctx context.Context, numbers []int) map[int]gtapi.PullRequestInfo {
	if len(numbers) == 0 || s.gt == nil {
		return nil
	}
	infos, err := s.gt.PullRequestInfo(ctx, gtapi.PullRequestInfoRequest{
		RepoOwner:  s.owner,
		RepoName:   s.name,
		PRNumbers:  numbers,
		Consistent: true,
		Callsite:   "ccx",
	})
	if err != nil {
		_, _ = fmt.Fprintf(s.warn, "pr watch: graphite: %v; queue state held from the last tick\n", err)
		return nil
	}
	byNumber := make(map[int]gtapi.PullRequestInfo, len(infos))
	for _, info := range infos {
		byNumber[info.PRNumber] = info
	}
	return byNumber
}

func prWatchSnapshotOf(number int, node prWatchNode, trunk prWatchHistory, info gtapi.PullRequestInfo, known, squashOnTrunk bool, prev prWatchSnapshot) prWatchSnapshot {
	snap := prWatchSnapshot{
		State:     node.State,
		Head:      node.HeadRefOid,
		Mergeable: node.Mergeable,
		Approved:  node.ReviewDecision == "APPROVED",
	}
	var rollup *statusRollup
	if len(node.Checks.Nodes) > 0 {
		rollup = node.Checks.Nodes[0].Commit.StatusCheckRollup
	}
	if rollup != nil {
		snap.Green = rollup.State == "SUCCESS"
		for _, check := range statusChecks(rollup) {
			if statusClassify(check.State) == statusFailed {
				snap.Failing = append(snap.Failing, check.Name)
			}
		}
		slices.Sort(snap.Failing)
	}
	switch {
	case node.State == "MERGED" && node.MergeCommit != nil:
		snap.Squash = node.MergeCommit.OID
	case node.State == "CLOSED":
		subject := prSquashSubject(number)
		for _, commit := range trunk.Target.History.Nodes {
			if subject.MatchString(commit.MessageHeadline) {
				snap.Squash = commit.OID
				break
			}
		}
		if snap.Squash == "" && squashOnTrunk {
			snap.Squash = info.MergeCommitSha
		}
	}
	if !known {
		snap.Queued, snap.Evicted = prev.Queued, prev.Evicted
		return snap
	}
	var activity string
	if node.Comments != nil {
		for _, comment := range node.Comments.Nodes {
			if strings.HasPrefix(comment.Body, mqActivityHeading) {
				activity = comment.Body
			}
		}
	}
	report := classifyPRQueue(info, "", activity)
	snap.Queued = report.Queue == prQueueQueued
	snap.Evicted = mqPlain(report.Evicted)
	return snap
}

func decodeRaw(raw json.RawMessage, into any) error {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	if err := json.Unmarshal(raw, into); err != nil {
		return fmt.Errorf("pr watch: decode graphql response: %w", err)
	}
	return nil
}

type prWatchState struct {
	PRs map[int]prWatchSnapshot `json:"prs"`
}

func loadPRWatchState(path string) (map[int]prWatchSnapshot, error) {
	snaps := map[int]prWatchSnapshot{}
	if path == "" {
		return snaps, nil
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return snaps, nil
	}
	if err != nil {
		return nil, fmt.Errorf("pr watch: read state: %w", err)
	}
	var state prWatchState
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, fmt.Errorf("pr watch: parse state %s: %w", path, err)
	}
	if state.PRs != nil {
		snaps = state.PRs
	}
	return snaps, nil
}

func savePRWatchState(path string, snaps map[int]prWatchSnapshot) error {
	if path == "" {
		return nil
	}
	data, err := json.Marshal(prWatchState{PRs: snaps})
	if err != nil {
		return fmt.Errorf("pr watch: encode state: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("pr watch: write state: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("pr watch: write state: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("pr watch: write state: %w", err)
	}
	return nil
}

type prWatchOpts struct {
	interval time.Duration
	until    string
	json     bool
	once     bool
	state    string
	prefix   string
}

type prWatchRun struct {
	poller prWatchPoller
	opts   prWatchOpts
	out    io.Writer
	warn   io.Writer
	now    func() time.Time
	sleep  func(context.Context, time.Duration) error
}

func (r prWatchRun) emit(event prWatchEvent) error {
	event.At = r.now().UTC()
	line := event.String()
	if r.opts.json {
		data, err := json.Marshal(event)
		if err != nil {
			return fmt.Errorf("pr watch: encode event: %w", err)
		}
		line = string(data)
	}
	if _, err := fmt.Fprintln(r.out, line); err != nil {
		return fmt.Errorf("pr watch: write event: %w", err)
	}
	return nil
}

func (r prWatchRun) rateLimited(ctx context.Context, until time.Time) error {
	if err := r.emit(prWatchEvent{Event: "rate-limited", Detail: "until " + until.UTC().Format(time.RFC3339)}); err != nil {
		return err
	}
	if r.opts.once {
		return nil
	}
	return r.sleep(ctx, until.Sub(r.now()))
}

func prWatchRateLimitError(err error) bool {
	var gql *ghapi.GraphQLError
	if errors.As(err, &gql) {
		return slices.ContainsFunc(gql.Messages, func(m ghapi.GraphQLMessage) bool { return m.Type == "RATE_LIMITED" })
	}
	var status *ghapi.StatusError
	return errors.As(err, &status) && (status.Status == 403 || status.Status == 429) &&
		strings.Contains(strings.ToLower(status.Message), "rate limit")
}

func (r prWatchRun) watch(ctx context.Context, numbers []int, snaps map[int]prWatchSnapshot) error {
	watched := slices.Clone(numbers)
	fails := 0
	var resetAt time.Time
	for {
		open := slices.DeleteFunc(slices.Clone(watched), func(n int) bool { return snaps[n].terminal() })
		if len(open) == 0 && r.opts.prefix == "" {
			if r.opts.once {
				return nil
			}
			if err := r.sleep(ctx, r.opts.interval); err != nil {
				return err
			}
			continue
		}
		tick, err := r.poller.poll(ctx, open, snaps)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			if prWatchRateLimitError(err) {
				until := resetAt
				if !until.After(r.now()) {
					until = r.now().Add(prWatchRateFallback)
				}
				if err := r.rateLimited(ctx, until); err != nil || r.opts.once {
					return err
				}
				continue
			}
			fails++
			_, _ = fmt.Fprintf(r.warn, "pr watch: poll failed (%d in a row): %v\n", fails, err)
			if fails >= prWatchMaxFails || r.opts.once {
				return fmt.Errorf("pr watch: %d polls in a row failed: %w", fails, err)
			}
			if err := r.sleep(ctx, r.opts.interval); err != nil {
				return err
			}
			continue
		}
		fails = 0
		resetAt = tick.resetAt
		for _, n := range open {
			settled, events := prWatchStep(n, snaps[n], tick.snapshots[n])
			snaps[n] = settled
			for _, event := range events {
				if err := r.emit(event); err != nil {
					return err
				}
			}
		}
		joined := false
		for _, n := range tick.discovered {
			if !slices.Contains(watched, n) {
				watched, joined = append(watched, n), true
			}
		}
		if err := savePRWatchState(r.opts.state, r.kept(watched, snaps)); err != nil {
			return err
		}
		if done, err := r.reached(watched, snaps); done || err != nil {
			return err
		}
		if tick.remaining < prWatchRateFloor && tick.resetAt.After(r.now()) {
			if err := r.rateLimited(ctx, tick.resetAt); err != nil || r.opts.once {
				return err
			}
			continue
		}
		if joined {
			continue
		}
		if r.opts.once {
			return nil
		}
		if err := r.sleep(ctx, r.opts.interval); err != nil {
			return err
		}
	}
}

func (r prWatchRun) kept(watched []int, snaps map[int]prWatchSnapshot) map[int]prWatchSnapshot {
	kept := make(map[int]prWatchSnapshot, len(watched))
	for _, n := range watched {
		kept[n] = snaps[n]
	}
	return kept
}

func (r prWatchRun) reached(watched []int, snaps map[int]prWatchSnapshot) (bool, error) {
	if r.opts.until == prWatchUntilNever || len(watched) == 0 {
		return false, nil
	}
	var unlanded []string
	for _, n := range watched {
		if !snaps[n].terminal() {
			return false, nil
		}
		if !snaps[n].landed() {
			unlanded = append(unlanded, "#"+strconv.Itoa(n))
		}
	}
	if r.opts.until == prWatchUntilLanded && len(unlanded) > 0 {
		return true, fmt.Errorf("pr watch: closed without landing: %s", strings.Join(unlanded, " "))
	}
	return true, nil
}

func newVcsPRWatchCmd() *cobra.Command {
	o := prWatchOpts{interval: prWatchDefaultInterval, until: prWatchUntilLanded}
	var repo string
	var stack bool
	cmd := &cobra.Command{
		Use:   "watch [<number>...]",
		Short: "Stream one line per state transition of each pull request until they land",
		Long: `Stream one line per state transition of each pull request, and nothing else.

Events: queued, ejected (was queued, now out of the queue and not landed,
with the queue's reason when its merge activity comment gives one),
conflicting, red <checks>, green, approved, approval-dismissed,
new-head <sha9>, landed <squash sha9>, and closed-without-squash. The first
poll reports each pull request's standing state the same way.

Each poll is one batched GitHub GraphQL query for every pull request, which
also reads the rate limit, plus one Graphite request for the merge queue
state ccx vcs pr status reads. When the limit runs low it prints one
"rate-limited until <time>" line and sleeps to the reset.

Landed means a commit whose subject ends (#<number>) is on the default
branch, or the squash Graphite recorded is reachable from it.

--stack watches the current Graphite downstack; --lane-prefix watches every
open pull request whose branch starts with the prefix, re-reading the branch
list on every poll so new pull requests join. --until landed exits 0 once every
watched pull request landed and 1 when one closed without landing; closed
exits 0 once every one closed either way; never keeps watching.

--state persists the last snapshot so a restarted watch resumes without
replaying state it already reported, and --once polls a single time, for a
caller that drives the loop itself. Output is line-buffered for Monitor.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runVcsPRWatch(cmd, args, repo, stack, o)
		},
	}
	cmd.Flags().StringVarP(&repo, "repo", "R", "", "the owner/name repository (default: the current checkout's)")
	cmd.Flags().DurationVar(&o.interval, "interval", prWatchDefaultInterval, "time between polls")
	cmd.Flags().StringVar(&o.until, "until", prWatchUntilLanded, "exit once every pull request is: landed, closed, or never")
	cmd.Flags().BoolVar(&o.json, "json", false, "emit one JSON object per event")
	cmd.Flags().BoolVar(&stack, "stack", false, "watch every pull request in the current Graphite downstack")
	cmd.Flags().StringVar(&o.prefix, "lane-prefix", "", "watch every open pull request whose branch starts with this prefix")
	cmd.Flags().StringVar(&o.state, "state", "", "file that carries snapshots across runs")
	cmd.Flags().BoolVar(&o.once, "once", false, "poll once and exit")
	return cmd
}

func runVcsPRWatch(cmd *cobra.Command, args []string, repo string, stack bool, o prWatchOpts) error {
	ctx := cmd.Context()
	switch o.until {
	case prWatchUntilLanded, prWatchUntilClosed, prWatchUntilNever:
	default:
		return fmt.Errorf("pr watch: --until must be landed, closed, or never, not %q", o.until)
	}
	if o.interval <= 0 {
		return errors.New("pr watch: --interval must be positive")
	}
	numbers, err := prWatchNumbers(args)
	if err != nil {
		return err
	}
	if stack {
		_, targets, err := resolveStackReviewTargets(ctx, cmd.ErrOrStderr(), time.Now())
		if err != nil {
			return fmt.Errorf("pr watch: %w", err)
		}
		for _, target := range targets {
			numbers = append(numbers, target.Number)
		}
	}
	if len(numbers) == 0 && o.prefix == "" {
		return errors.New("pr watch: name pull requests, --stack, or --lane-prefix")
	}
	if repo == "" {
		looked, err := vcs.LookupRepo(ctx, render.Dir(workingDir(ctx)), false)
		if err != nil {
			return fmt.Errorf("pr watch: name the repository with --repo: %w", err)
		}
		repo = looked.NameWithOwner
	}
	owner, name, ok := strings.Cut(repo, "/")
	if !ok {
		return fmt.Errorf("pr watch: malformed repository name %q", repo)
	}
	snaps, err := loadPRWatchState(o.state)
	if err != nil {
		return err
	}
	if o.prefix != "" {
		numbers = append(numbers, slices.Collect(maps.Keys(snaps))...)
	}
	slices.Sort(numbers)
	numbers = slices.Compact(numbers)
	run := prWatchRun{
		poller: prWatchSource{gh: reviewsAPI(), gt: gtAPI(ctx), owner: owner, name: name, prefix: o.prefix, warn: cmd.ErrOrStderr()},
		opts:   o,
		out:    cmd.OutOrStdout(),
		warn:   cmd.ErrOrStderr(),
		now:    time.Now,
		sleep:  sleepCtx,
	}
	return run.watch(ctx, numbers, snaps)
}

func prWatchNumbers(args []string) ([]int, error) {
	numbers := make([]int, 0, len(args))
	for _, arg := range args {
		n, err := strconv.Atoi(strings.TrimPrefix(arg, "#"))
		if err != nil || n <= 0 {
			return nil, fmt.Errorf("pr watch: %q is not a pull request number", arg)
		}
		numbers = append(numbers, n)
	}
	return numbers, nil
}
