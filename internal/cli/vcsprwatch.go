package cli

import (
	"cmp"
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
	prWatchUntilCI     = "ci"

	prWatchCIGreen = "green"
	prWatchCIRed   = "red"

	prWatchJobAbsent = "ABSENT"

	prWatchDefaultInterval = 60 * time.Second
	prWatchMaxFails        = 10
	prWatchJobRegister     = time.Minute
	prWatchJobAppear       = 10 * time.Minute
)

type prWatchSnapshot struct {
	State     string    `json:"state,omitempty"`
	Head      string    `json:"head,omitempty"`
	Mergeable string    `json:"mergeable,omitempty"`
	Green     bool      `json:"green,omitempty"`
	Failing   []string  `json:"failing,omitempty"`
	Approved  bool      `json:"approved,omitempty"`
	Queued    bool      `json:"queued,omitempty"`
	Evicted   string    `json:"evicted,omitempty"`
	Squash    string    `json:"squash,omitempty"`
	Job       string    `json:"job,omitempty"`
	CI        string    `json:"ci,omitempty"`
	JobID     int64     `json:"jobId,omitempty"`
	HeadSince time.Time `json:"headSince,omitzero"`
}

func (s prWatchSnapshot) terminal() bool { return s.State != "" && s.State != "OPEN" }

func (s prWatchSnapshot) landed() bool { return s.terminal() && s.Squash != "" }

func (s prWatchSnapshot) jobSettled() bool {
	return s.Job != "" && statusClassify(s.Job) != statusRunning
}

func (s prWatchSnapshot) jobReport(repo string) string {
	state := strings.ToLower(s.Job) + " on " + sha9(s.Head)
	if statusClassify(s.Job) != statusFailed || s.JobID == 0 {
		return state
	}
	return fmt.Sprintf("%s: ccx vcs job-log %d -R %s", state, s.JobID, repo)
}

type prWatchEvent struct {
	At     time.Time `json:"at"`
	Repo   string    `json:"repo,omitempty"`
	PR     int       `json:"pr,omitempty"`
	Event  string    `json:"event"`
	Detail string    `json:"detail,omitempty"`
	Head   string    `json:"head,omitempty"`
}

func (e prWatchEvent) String() string {
	if e.PR == 0 {
		return strings.TrimSpace(e.Event + " " + e.Detail)
	}
	line := fmt.Sprintf("%s#%d %s", e.Repo, e.PR, e.Event)
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
	job    string
	ci     bool
	once   bool
	since  time.Time
}

func (s prWatchSource) poll(ctx context.Context, numbers []int, prev map[int]prWatchSnapshot) (prWatchTick, error) {
	want := prstate.Want{PRs: numbers}
	if s.job != "" || s.ci {
		want.After = s.since
	}
	if s.prefix != "" {
		want.Prefixes = []string{s.prefix}
	}
	read := s.store.Read
	if s.once {
		read = s.store.Peek
	}
	st, err := read(ctx, want)
	if err != nil {
		return prWatchTick{}, err
	}
	tick := prWatchTick{snapshots: make(map[int]prWatchSnapshot, len(numbers))}
	if s.prefix != "" {
		tick.discovered = st.Lanes[s.prefix].PRs
	}
	for _, number := range numbers {
		pr, since := st.PRs[number], st.PRs[number].PolledAt
		if pr.PushedHead != "" && pr.PushedHead != pr.HeadRefOid {
			pr.HeadRefOid, pr.Rollup, since = pr.PushedHead, nil, pr.PushedAt
		}
		snap := prWatchSnapshotOf(number, pr, st.Trunk, prev[number])
		if s.job != "" || s.ci {
			snap.HeadSince = since
			if was := prev[number]; was.Head == snap.Head && !was.HeadSince.IsZero() {
				snap.HeadSince = was.HeadSince
			}
		}
		if s.job != "" {
			snap.Job, snap.JobID = prWatchJobState(pr.Rollup, s.job, pr.PolledAt.Sub(snap.HeadSince))
		}
		if s.ci {
			snap.CI = prWatchCIState(snap, pr.PolledAt.Sub(snap.HeadSince))
		}
		tick.snapshots[number] = snap
	}
	return tick, nil
}

// prWatchJobState reads one check off the head's rollup, onHead after the
// watch first saw that head. A check the head has not named within
// prWatchJobAppear never will. Sooner, a finished rollup that never names the
// check, with every Actions suite on the head finished too, means the name is
// wrong or the check does not run on this head, once the head has had
// prWatchJobRegister for its workflows to register their suites. An open suite
// may yet report it, however finished the rollup.
func prWatchJobState(rollup *statusRollup, job string, onHead time.Duration) (string, int64) {
	state, id := "", int64(0)
	if rollup != nil {
		for _, check := range statusChecks(rollup) {
			if check.Name != job {
				continue
			}
			if state == "" || statusClassify(check.State) == statusRunning || statusClassify(check.State) == statusFailed && statusClassify(state) != statusRunning {
				state, id = check.State, check.ID
			}
		}
	}
	switch {
	case state != "":
		return state, id
	case onHead >= prWatchJobAppear:
		return prWatchJobAbsent, 0
	case onHead >= prWatchJobRegister && rollup != nil && rollup.UnfinishedActions == 0 && rollup.State != "" && statusClassify(rollup.State) != statusRunning:
		return prWatchJobAbsent, 0
	}
	return "", 0
}

// prWatchCIState settles the head's checks: red once one finished failing,
// green once every one finished and passed, after the head has had
// prWatchJobRegister for its workflows to register their suites.
func prWatchCIState(snap prWatchSnapshot, onHead time.Duration) string {
	switch {
	case len(snap.Failing) > 0:
		return prWatchCIRed
	case snap.Green && onHead >= prWatchJobRegister:
		return prWatchCIGreen
	}
	return ""
}

func prWatchSnapshotOf(number int, pr prstate.PR, trunk prstate.Trunk, prev prWatchSnapshot) prWatchSnapshot {
	snap := prWatchSnapshot{
		State:     pr.State,
		Head:      pr.HeadRefOid,
		Mergeable: pr.Mergeable,
		Approved:  pr.ReviewDecision == "APPROVED",
	}
	if pr.Rollup != nil {
		snap.Green, snap.Failing = prWatchChecks(pr.Rollup)
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

func prWatchChecks(rollup *statusRollup) (bool, []string) {
	green := rollup.UnfinishedActions == 0 && (rollup.State == "SUCCESS" || prCIOf(rollup).State == prCIGreen)
	var failing []string
	for _, check := range statusChecks(rollup) {
		if statusClassify(check.State) == statusFailed {
			failing = append(failing, check.Name)
		}
	}
	if len(failing) == 0 && (rollup.State == "FAILURE" || rollup.State == "ERROR") {
		failing = []string{"rollup " + strings.ToLower(rollup.State)}
	}
	slices.Sort(failing)
	return green, slices.Compact(failing)
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
	interval    time.Duration
	until       string
	json        bool
	once        bool
	state       string
	prefix      string
	job         string
	repo        string
	queueUnread bool
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
	if event.PR != 0 {
		event.Repo = r.opts.repo
	}
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

// watch follows numbers to the watch's end. A timeout or signal that stops it
// first names each pull request and head it left without a verdict.
func (r prWatchRun) watch(ctx context.Context, numbers []int, snaps map[int]prWatchSnapshot) error {
	err := r.follow(ctx, numbers, snaps)
	if ctx.Err() == nil || !errors.Is(err, ctx.Err()) {
		return err
	}
	watched := slices.Sorted(maps.Keys(snaps))
	for _, n := range numbers {
		if _, seen := snaps[n]; !seen {
			watched = append(watched, n)
		}
	}
	left := make([]string, 0, len(watched))
	for _, n := range watched {
		left = append(left, fmt.Sprintf("%s#%d on %s", r.opts.repo, n, cmp.Or(sha9(snaps[n].Head), "a head not yet read")))
	}
	return fmt.Errorf("pr watch: stopped with no verdict for %s: %w", strings.Join(left, ", "), err)
}

func (r prWatchRun) follow(ctx context.Context, numbers []int, snaps map[int]prWatchSnapshot) error {
	if r.opts.queueUnread {
		if err := r.emit(prWatchEvent{Event: "queue-unread", Detail: r.opts.repo + ": not a checkout here, so no queued or ejected line can appear"}); err != nil {
			return err
		}
	}
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
			if settled.jobSettled() && (settled.Job != snaps[n].Job || settled.Head != snaps[n].Head) {
				events = append(events, prWatchEvent{PR: n, Event: "job", Detail: r.opts.job + " " + settled.jobReport(r.opts.repo), Head: settled.Head})
			}
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

func (r prWatchRun) reachedJob(watched []int, snaps map[int]prWatchSnapshot) (bool, error) {
	var unmet []string
	for _, n := range watched {
		snap := snaps[n]
		switch {
		case snap.jobSettled() && snap.Job == prWatchJobAbsent:
			unmet = append(unmet, fmt.Sprintf("%s#%d head %s has no %s check", r.opts.repo, n, sha9(snap.Head), r.opts.job))
		case snap.jobSettled() && statusClassify(snap.Job) == statusFailed:
			unmet = append(unmet, fmt.Sprintf("%s#%d %s", r.opts.repo, n, snap.jobReport(r.opts.repo)))
		case snap.jobSettled():
		case snap.terminal():
			unmet = append(unmet, fmt.Sprintf("%s#%d closed before %s settled", r.opts.repo, n, r.opts.job))
		default:
			return false, nil
		}
	}
	if len(unmet) > 0 {
		return true, fmt.Errorf("pr watch: job %s: %s", r.opts.job, strings.Join(unmet, "; "))
	}
	return true, nil
}

func (r prWatchRun) reachedCI(watched []int, snaps map[int]prWatchSnapshot) (bool, error) {
	var unmet []string
	for _, n := range watched {
		snap := snaps[n]
		switch {
		case snap.CI == prWatchCIRed:
			unmet = append(unmet, fmt.Sprintf("%s#%d red on %s: %s", r.opts.repo, n, sha9(snap.Head), strings.Join(snap.Failing, ", ")))
		case snap.CI == prWatchCIGreen:
		case snap.terminal():
			unmet = append(unmet, fmt.Sprintf("%s#%d closed before its checks finished", r.opts.repo, n))
		default:
			return false, nil
		}
	}
	if len(unmet) > 0 {
		return true, fmt.Errorf("pr watch: ci: %s", strings.Join(unmet, "; "))
	}
	return true, nil
}

func (r prWatchRun) reached(watched []int, snaps map[int]prWatchSnapshot) (bool, error) {
	if len(watched) > 0 && r.opts.job != "" {
		return r.reachedJob(watched, snaps)
	}
	if len(watched) > 0 && r.opts.until == prWatchUntilCI {
		return r.reachedCI(watched, snaps)
	}
	if r.opts.until == prWatchUntilNever || len(watched) == 0 {
		return false, nil
	}
	var unlanded []string
	for _, n := range watched {
		if !snaps[n].terminal() {
			return false, nil
		}
		if !snaps[n].landed() {
			unlanded = append(unlanded, fmt.Sprintf("%s#%d", r.opts.repo, n))
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
		Use:   "watch [<number> | <owner/name#number>...]",
		Short: "Stream one line per state transition of each pull request until they land",
		Long: `Stream one line per state transition of each pull request, and nothing else.

Events: queued, ejected (was queued, now out of the queue and not landed,
with the queue's reason when its merge activity comment gives one),
conflicting, red <checks>, green, approved, approval-dismissed,
new-head <sha9>, landed <squash sha9>, and closed-without-squash. The first
poll reports each pull request's standing state the same way. Green waits for
every GitHub Actions check suite on the head to finish, so a fresh head whose
workflows have not reported yet does not read green on other apps' checks.

Polls go through the machine-wide pull request cache every ccx vcs pr
command shares: one batched GitHub GraphQL query and one Graphite request for
the pull requests this and any other process on the machine asked for since
the last poll, at most once per 30 seconds, whatever --interval says. While GitHub rate-limits
the machine it prints one "rate-limited until <time>" line and sleeps to the
next probe, one cheap request at most two minutes out.

Landed means a commit whose subject ends (#<number>) is on the default
branch, or the squash Graphite recorded is reachable from it.

--stack watches the current Graphite downstack; --lane-prefix watches every
open pull request whose branch starts with the prefix, re-reading the branch
list on every poll so new pull requests join. --until landed exits 0 once every
watched pull request landed and 1 when one closed without landing; closed
exits 0 once every one closed either way; ci exits 0 once every check on each
pull request's current head finished and passed, and 1 once one finished
failing, naming it, or when the pull request closed first; never keeps
watching. Like --job, ci reads the current head from a poll made after the
watch began, and gives a fresh head its first minute for its workflows to
register before it calls the head green.

--job <name> watches one named check on each pull request's current head instead
of --until: one "job <name> <state> on <head sha9>" line when it settles on the
head it names, exit 0 when it passed, skipped or went neutral, and 1 when it
failed, when the pull request closed first, or when the head has no check of
that name: none appeared within ten minutes of the watch seeing the head, or,
after its first minute, the rest of the head's checks and every GitHub Actions
run on it finished without it. The current head is the newest this machine's
ccx pushed until GitHub shows it, else GitHub's, read from a poll made after the
watch began, so a check that passed on the head before an amend never answers
for the amended one. A failure names the ccx vcs job-log command that reads the
job's log, while its run may still be going. A new head starts the check over.
A watch a timeout or signal stops before its end exits 1 with one stderr line
naming each pull request and head it left without a verdict.

--state persists the last snapshot so a restarted watch resumes without
replaying state it already reported, and --once polls a single time, for a
caller that drives the loop itself: it answers from the shared cache rather
than wait out the 30-second spacing, so it spends at most one poll's latency;
with --job it waits for the first shared poll made after it began.
Each pull request is a number, #number, or owner/name#number, the last being the
same as --repo. Without either, the repository is the current checkout's, so
every event line names the repository it read. Outside a checkout of that
repository the watch reads GitHub alone: it opens with a queue-unread line, and
every event but queued and ejected still reports.

Output is line-buffered for Monitor.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runVcsPRWatch(cmd, args, repo, stack, o)
		},
	}
	cmd.Flags().StringVarP(&repo, "repo", "R", "", "the owner/name repository (default: the current checkout's)")
	cmd.Flags().DurationVar(&o.interval, "interval", prWatchDefaultInterval, "time between polls")
	cmd.Flags().StringVar(&o.until, "until", prWatchUntilLanded, "exit once every pull request is: landed, closed, ci (its head's checks all finished), or never")
	cmd.Flags().BoolVar(&o.json, "json", false, "emit one JSON object per event")
	cmd.Flags().BoolVar(&stack, "stack", false, "watch every pull request in the current Graphite downstack")
	cmd.Flags().StringVar(&o.prefix, "lane-prefix", "", "watch every open pull request whose branch starts with this prefix")
	cmd.Flags().StringVar(&o.job, "job", "", "exit once this one check settles on every watched pull request's head")
	cmd.Flags().StringVar(&o.state, "state", "", "file that carries snapshots across runs")
	cmd.Flags().BoolVar(&o.once, "once", false, "poll once and exit")
	cmd.MarkFlagsMutuallyExclusive("job", "state")
	return cmd
}

func runVcsPRWatch(cmd *cobra.Command, args []string, repo string, stack bool, o prWatchOpts) error {
	ctx := cmd.Context()
	switch o.until {
	case prWatchUntilLanded, prWatchUntilClosed, prWatchUntilCI, prWatchUntilNever:
	default:
		return fmt.Errorf("pr watch: --until must be landed, closed, ci, or never, not %q", o.until)
	}
	if o.interval <= 0 {
		return errors.New("pr watch: --interval must be positive")
	}
	repo, numbers, err := prRefs(args, repo)
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
	o.repo = repo
	o.queueUnread = prQueueUnread(ctx, repo)
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
		poller: prWatchSource{store: store, prefix: o.prefix, job: o.job, ci: o.until == prWatchUntilCI, once: o.once, since: time.Now().UTC()},
		opts:   o,
		out:    cmd.OutOrStdout(),
		warn:   cmd.ErrOrStderr(),
		now:    time.Now,
		sleep:  sleepCtx,
	}
	return run.watch(ctx, numbers, snaps)
}

// prRefs reads pull request arguments, each N, #N or owner/name#N, and returns
// their numbers with the one repository that repo and the arguments name, "" when
// none does.
func prRefs(args []string, repo string) (string, []int, error) {
	numbers := make([]int, 0, len(args))
	for _, arg := range args {
		name, number, qualified := strings.Cut(arg, "#")
		if !qualified {
			name, number = "", arg
		}
		n, err := strconv.Atoi(number)
		if err != nil || n <= 0 || name != "" && strings.Count(name, "/") != 1 {
			return "", nil, fmt.Errorf("%q is not a pull request number or owner/name#number", arg)
		}
		if name != "" && repo != "" && !strings.EqualFold(name, repo) {
			return "", nil, fmt.Errorf("%s is not in %s; one command reads one repository", arg, repo)
		}
		repo = cmp.Or(repo, name)
		numbers = append(numbers, n)
	}
	return repo, numbers, nil
}
