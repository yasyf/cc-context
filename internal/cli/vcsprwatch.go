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

	"github.com/yasyf/cc-context/internal/cache"
	"github.com/yasyf/cc-context/internal/gtapi"
	"github.com/yasyf/cc-context/internal/prstate"
	"github.com/yasyf/cc-context/internal/render"
	"github.com/yasyf/cc-context/internal/vcs"
)

const (
	prWatchUntilLanded = "landed"
	prWatchUntilClosed = "closed"
	prWatchUntilNever  = "never"

	prWatchDefaultInterval = 60 * time.Second
	prWatchMaxFails        = 10
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
}

type prWatchPoller interface {
	poll(ctx context.Context, numbers []int, prev map[int]prWatchSnapshot) (prWatchTick, error)
}

type prWatchSource struct {
	store  *prstate.Store
	prefix string
}

func (s prWatchSource) poll(ctx context.Context, numbers []int, prev map[int]prWatchSnapshot) (prWatchTick, error) {
	want := prstate.Want{PRs: numbers}
	if s.prefix != "" {
		want.Prefixes = []string{s.prefix}
	}
	st, err := s.store.Read(ctx, want)
	if err != nil {
		return prWatchTick{}, err
	}
	tick := prWatchTick{snapshots: make(map[int]prWatchSnapshot, len(numbers))}
	if s.prefix != "" {
		tick.discovered = st.Lanes[s.prefix].PRs
	}
	for _, number := range numbers {
		tick.snapshots[number] = prWatchSnapshotOf(number, st.PRs[number], st.Trunk, prev[number])
	}
	return tick, nil
}

func prWatchSnapshotOf(number int, pr prstate.PR, trunk prstate.Trunk, prev prWatchSnapshot) prWatchSnapshot {
	snap := prWatchSnapshot{
		State:     pr.State,
		Head:      pr.HeadRefOid,
		Mergeable: pr.Mergeable,
		Approved:  pr.ReviewDecision == "APPROVED",
	}
	if rollup := pr.Rollup; rollup != nil {
		snap.Green = rollup.State == "SUCCESS" || prCIOf(rollup).State == prCIGreen
		for _, check := range statusChecks(rollup) {
			if statusClassify(check.State) == statusFailed {
				snap.Failing = append(snap.Failing, check.Name)
			}
		}
		if len(snap.Failing) == 0 && (rollup.State == "FAILURE" || rollup.State == "ERROR") {
			snap.Failing = []string{"rollup " + strings.ToLower(rollup.State)}
		}
		slices.Sort(snap.Failing)
	}
	known := pr.Graphite != nil
	var info gtapi.PullRequestInfo
	if known {
		info = *pr.Graphite
	}
	if pr.State != "OPEN" {
		snap.Squash = prWatchSquash(number, pr, trunk, info)
		if snap.Squash == "" && !known {
			snap.State = prev.State
		}
	}
	// Graphite records a merge before GitHub closes the pull request, and its
	// queue flag drops with it, so an open pull request Graphite calls merged
	// keeps its queue state until GitHub catches up.
	if !known || info.State != gtapi.PROpen && pr.State == "OPEN" {
		snap.Queued, snap.Evicted = prev.Queued, prev.Evicted
		return snap
	}
	report := classifyPRQueue(info, pr.Mergeability, "", pr.Activity)
	snap.Queued = report.Queue == prQueueQueued
	snap.Evicted = mqPlain(report.Evicted)
	return snap
}

func prWatchSquash(number int, pr prstate.PR, trunk prstate.Trunk, info gtapi.PullRequestInfo) string {
	if pr.State == "MERGED" && pr.MergeCommit != "" && pr.BaseRefName == trunk.Name {
		return pr.MergeCommit
	}
	subject := prSquashSubject(number)
	for _, commit := range trunk.History {
		if subject.MatchString(commit.Headline) {
			return commit.OID
		}
	}
	if trunk.Name != "" && slices.Contains(pr.SquashOn, trunk.Name) {
		return info.MergeCommitSha
	}
	return ""
}

type prWatchState struct {
	PRs map[int]prWatchSnapshot `json:"prs"`
}

func loadPRWatchState(path string) (map[int]prWatchSnapshot, error) {
	snaps := map[int]prWatchSnapshot{}
	if path == "" {
		return snaps, nil
	}
	data, err := os.ReadFile(filepath.Clean(path))
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
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return fmt.Errorf("pr watch: write state: %w", err)
	}
	if err := cache.Store(path, data, 0o600); err != nil {
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

func (r prWatchRun) watch(ctx context.Context, numbers []int, snaps map[int]prWatchSnapshot) error {
	watched := slices.Clone(numbers)
	fails := 0
	for {
		if done, err := r.reached(watched, snaps); done || err != nil {
			return err
		}
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
		var limited *prstate.LimitedError
		if errors.As(err, &limited) {
			if err := r.rateLimited(ctx, limited.ProbeAt); err != nil || r.opts.once {
				return err
			}
			continue
		}
		if missing := (*prstate.MissingError)(nil); errors.As(err, &missing) {
			return fmt.Errorf("pr watch: %w", err)
		}
		if err != nil {
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

Polls go through the machine-wide pull request cache every ccx vcs pr
command shares: one batched GitHub GraphQL query and one Graphite request for
every pull request any process on the machine watches in the repository, at
most once per 30 seconds, whatever --interval says. While GitHub rate-limits
the machine it prints one "rate-limited until <time>" line and sleeps to the
next probe, one cheap request at most two minutes out.

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
	numbers, err := prNumbers(args)
	if err != nil {
		return fmt.Errorf("pr watch: %w", err)
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
	snaps, err := loadPRWatchState(o.state)
	if err != nil {
		return err
	}
	if o.prefix != "" {
		numbers = append(numbers, slices.Collect(maps.Keys(snaps))...)
	}
	slices.Sort(numbers)
	numbers = slices.Compact(numbers)
	store, err := openPRState(ctx, repo, cmd.ErrOrStderr())
	if err != nil {
		return fmt.Errorf("pr watch: %w", err)
	}
	run := prWatchRun{
		poller: prWatchSource{store: store, prefix: o.prefix},
		opts:   o,
		out:    cmd.OutOrStdout(),
		warn:   cmd.ErrOrStderr(),
		now:    time.Now,
		sleep:  sleepCtx,
	}
	return run.watch(ctx, numbers, snaps)
}

func prNumbers(args []string) ([]int, error) {
	numbers := make([]int, 0, len(args))
	for _, arg := range args {
		n, err := strconv.Atoi(strings.TrimPrefix(arg, "#"))
		if err != nil || n <= 0 {
			return nil, fmt.Errorf("%q is not a pull request number", arg)
		}
		numbers = append(numbers, n)
	}
	return numbers, nil
}
