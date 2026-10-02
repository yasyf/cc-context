package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/yasyf/cc-context/internal/cleanup"
	"github.com/yasyf/cc-context/internal/cleanup/relocate"
	"github.com/yasyf/cc-context/internal/render"
)

const (
	legacyQuarantineOwner  = "legacy quarantine import"
	cleanupStatusTimeout   = 10 * time.Second
	cleanupWaitPollFloor   = 10 * time.Millisecond
	cleanupWaitPollCeiling = time.Second
)

const cleanupReadNote = `It reads the running daemon and never installs, starts, or replaces one; an
older daemon on this ccx's protocol still answers.`

var cleanupHandoffTimeout = 2 * time.Minute

type cleanupKey struct{}

type cleanupPreviewKey struct{}

type cleanupPreviewer func(ctx context.Context, r cleanup.Request) (cleanup.Job, error)

type cleanupJobDamagedError struct {
	damaged cleanup.Damaged
	seen    cleanup.Phase
}

func (e *cleanupJobDamagedError) Error() string {
	return fmt.Sprintf("job %s was last seen %s, and the daemon now reports its record damaged: %s", e.damaged.ID, e.seen, e.damaged.Error)
}

type cleanupJobMissingError struct {
	id   string
	seen cleanup.Phase
}

func (e *cleanupJobMissingError) Error() string {
	return fmt.Sprintf("job %s was last seen %s, and the daemon no longer holds it: its completion cannot be proven, since it may have finished and been pruned or its record was lost", e.id, e.seen)
}

func withCleanup(ctx context.Context, svc cleanup.Service) context.Context {
	return context.WithValue(ctx, cleanupKey{}, svc)
}

var cleanupDefault = connectCleanup

func cleanupService(ctx context.Context) (cleanup.Service, error) {
	if svc, ok := ctx.Value(cleanupKey{}).(cleanup.Service); ok {
		return svc, nil
	}
	return cleanupDefault(ctx)
}

var cleanupReadDefault = reachCleanup

func cleanupReadService(ctx context.Context) (cleanup.Service, error) {
	if svc, ok := ctx.Value(cleanupKey{}).(cleanup.Service); ok {
		return svc, nil
	}
	return cleanupReadDefault(ctx)
}

func withCleanupPreview(ctx context.Context, preview cleanupPreviewer) context.Context {
	return context.WithValue(ctx, cleanupPreviewKey{}, preview)
}

var cleanupPreviewDefault cleanupPreviewer = previewCleanup

var cleanupServeDefault = runCleanupServe

func cleanupPreview(ctx context.Context) cleanupPreviewer {
	if preview, ok := ctx.Value(cleanupPreviewKey{}).(cleanupPreviewer); ok {
		return preview
	}
	return cleanupPreviewDefault
}

func cleanupObserveWorkspace(ws string) (cleanup.Registration, error) {
	registration, err := relocate.Observe(ws)
	if err != nil {
		return cleanup.Registration{}, fmt.Errorf("cleanup observe %s: %w", ws, err)
	}
	return registration, nil
}

func cleanupDeferWorkspace(ctx context.Context, commonDir, ws, owner string, expected cleanup.Registration, force bool) (cleanup.Receipt, error) {
	if err := expected.Validate(); err != nil {
		return cleanup.Receipt{}, fmt.Errorf("cleanup defer %s: expected registration: %w", ws, err)
	}
	git, err := cleanupGit(ctx, "cleanup defer")
	if err != nil {
		return cleanup.Receipt{}, err
	}
	svc, err := cleanupService(ctx)
	if err != nil {
		return cleanup.Receipt{}, fmt.Errorf("cleanup defer %s: %w", ws, err)
	}
	receipt, err := cleanupHandoff(ctx, func(ctx context.Context) (cleanup.Receipt, error) {
		return svc.Defer(ctx, cleanup.DeferRequest{Worktree: ws, CommonDir: commonDir, Owner: owner, Expected: expected, Force: force, Git: git})
	})
	if err != nil {
		return cleanup.Receipt{}, fmt.Errorf("cleanup defer %s: %w", ws, err)
	}
	return receipt, nil
}

func cleanupHandoff(ctx context.Context, handoff func(context.Context) (cleanup.Receipt, error)) (cleanup.Receipt, error) {
	bounded, cancel := context.WithTimeout(ctx, cleanupHandoffTimeout)
	defer cancel()
	receipt, err := handoff(bounded)
	if err != nil && ctx.Err() == nil && errors.Is(bounded.Err(), context.DeadlineExceeded) {
		return cleanup.Receipt{}, fmt.Errorf("the cleanup daemon did not answer within %s: it runs one removal at a time, and another tree's still holds it; run the command again to rejoin the job: %w", cleanupHandoffTimeout, err)
	}
	return receipt, err
}

func cleanupGit(ctx context.Context, prefix string) (string, error) {
	git := render.LookPath(ctx, "git")
	switch {
	case git == "":
		return "", fmt.Errorf("%s: git is not on PATH", prefix)
	case !filepath.IsAbs(git):
		return "", fmt.Errorf("%s: git resolves to %q, not an absolute path", prefix, git)
	}
	return git, nil
}

func cleanupJobErr(prefix, id string, err error) error {
	if errors.Is(err, cleanup.ErrUnknownJob) {
		return fmt.Errorf("%s: job %s %w: %w", prefix, id, ErrNotFound, err)
	}
	return fmt.Errorf("%s: %w", prefix, err)
}

func newCleanupCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "cleanup",
		Short: "Inspect and steer the macOS worktree deletion queue",
		Long: `Inspect and steer the macOS worktree deletion queue.

On macOS, "worktree rm" hands each linked worktree to a per-user daemon: the
removal returns once the tree has left its path and git's registry, and the
daemon deletes the relocated files afterward in bounded, throttled slices.
These commands read that queue and steer its physical deletion. Linux removes
inline and runs no daemon.`,
		Args: cobra.NoArgs,
		RunE: groupHelp,
	}
	cmd.AddCommand(
		newCleanupStatusCmd(),
		newCleanupWaitCmd(),
		newCleanupPauseCmd(),
		newCleanupResumeCmd(),
		newCleanupRetryCmd(),
		newCleanupServeCmd(),
		newCleanupAdoptCmd(),
		newCleanupWatchersCmd(),
	)
	return cmd
}

func newCleanupStatusCmd() *cobra.Command {
	var (
		asJSON bool
		limit  int
	)
	cmd := &cobra.Command{
		Use:   "status [job-id]",
		Short: "Report the deletion queue, or one job",
		Long: `Report the deletion queue, or one job.

Each line is "<job-id> · <state> · <original path> · <n> entries", unfinished
jobs first in queue order, then the most recently finished, under a header
naming the daemon, its fseventsd throttle, and whether deletion is paused. The
report is bounded by --limit and says how many jobs it omitted; it never scans
a tree for a total or estimates time remaining. While deletion is paused, a job
waiting only on physical deletion reads "paused".

` + cleanupReadNote,
		Args: cobra.MatchAll(cobra.MaximumNArgs(1), cleanupJobIDArg),
		RunE: func(cmd *cobra.Command, args []string) error {
			if limit < 0 {
				return fmt.Errorf("cleanup status: --limit %d is negative", limit)
			}
			q := cleanup.Query{Limit: limit}
			if len(args) == 1 {
				q.JobID = args[0]
			}
			return runCleanupStatus(cmd, q, asJSON)
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit the report as JSON")
	cmd.Flags().IntVar(&limit, "limit", 0, "cap on the jobs reported (0 = the daemon's default)")
	return cmd
}

func runCleanupStatus(cmd *cobra.Command, q cleanup.Query, asJSON bool) error {
	ctx := cmd.Context()
	svc, err := cleanupReadService(ctx)
	if err != nil {
		return fmt.Errorf("cleanup status: %w", err)
	}
	report, err := readCleanupStatus(ctx, svc, q)
	if err != nil {
		return cleanupJobErr("cleanup status", q.JobID, err)
	}
	if asJSON {
		data, err := json.MarshalIndent(report, "", "  ")
		if err != nil {
			return fmt.Errorf("cleanup status: marshal report: %w", err)
		}
		cmd.Println(string(data))
		return nil
	}
	cmd.Print(renderCleanupReport(report))
	return nil
}

func readCleanupStatus(ctx context.Context, svc cleanup.Service, q cleanup.Query) (cleanup.Report, error) {
	ctx, cancel := context.WithTimeout(ctx, cleanupStatusTimeout)
	defer cancel()
	report, err := svc.Status(ctx, q)
	if errors.Is(err, context.DeadlineExceeded) {
		return cleanup.Report{}, fmt.Errorf("the daemon answered its hello but sent no report within %s: %w", cleanupStatusTimeout, err)
	}
	return report, err
}

func waitCleanupReceipt(ctx context.Context, receipt cleanup.Receipt) error {
	svc, err := cleanupReadService(ctx)
	if err != nil {
		return err
	}
	_, err = waitCleanupJob(ctx, svc, receipt.JobID, cleanup.Phase(receipt.State))
	return err
}

func waitCleanupJob(ctx context.Context, svc cleanup.Service, id string, seen cleanup.Phase) (cleanup.Job, error) {
	for delay := cleanupWaitPollFloor; ; delay = min(2*delay, cleanupWaitPollCeiling) {
		report, err := readCleanupStatus(ctx, svc, cleanup.Query{JobID: id})
		if seen != "" && errors.Is(err, cleanup.ErrUnknownJob) {
			return cleanup.Job{}, vanishedCleanupJob(ctx, svc, id, seen)
		}
		if err != nil {
			return cleanup.Job{}, err
		}
		job := report.Jobs[0]
		seen = job.Phase
		switch {
		case job.Phase == cleanup.PhaseDone:
			return job, nil
		case job.Blocked != nil:
			return job, &cleanup.BlockedError{Job: job}
		}
		select {
		case <-ctx.Done():
			return cleanup.Job{}, ctx.Err()
		case <-time.After(delay):
		}
	}
}

func vanishedCleanupJob(ctx context.Context, svc cleanup.Service, id string, seen cleanup.Phase) error {
	report, err := readCleanupStatus(ctx, svc, cleanup.Query{})
	if err != nil {
		return fmt.Errorf("job %s was last seen %s, and the daemon no longer holds it; reading the queue for a damaged record: %w", id, seen, err)
	}
	if i := slices.IndexFunc(report.Damaged, func(d cleanup.Damaged) bool { return d.ID == id }); i >= 0 {
		return &cleanupJobDamagedError{damaged: report.Damaged[i], seen: seen}
	}
	return &cleanupJobMissingError{id: id, seen: seen}
}

func cleanupJobIDArg(cmd *cobra.Command, args []string) error {
	if slices.Contains(args, "") {
		return fmt.Errorf("cleanup %s: the job id is empty", cmd.Name())
	}
	return nil
}

func renderCleanupReport(r cleanup.Report) string {
	var b strings.Builder
	deletion := "deletion running"
	if r.Paused {
		deletion = "deletion paused"
	}
	b.WriteString(strings.Join([]string{"cleanup daemon " + r.Version, "pid " + strconv.Itoa(r.PID), governorSegment(r.Governor), deletion}, shipSep) + "\n")
	if len(r.Jobs) == 0 {
		b.WriteString("no cleanup jobs\n")
	}
	for _, job := range r.Jobs {
		b.WriteString(cleanupJobLine(job, r.Paused) + "\n")
	}
	if r.Omitted > 0 {
		fmt.Fprintf(&b, "omitted %d more %s\n", r.Omitted, plural(r.Omitted, "job", "jobs"))
	}
	for _, d := range r.Damaged {
		b.WriteString(strings.Join([]string{"damaged " + d.ID, d.Error}, shipSep) + "\n")
	}
	return b.String()
}

func governorSegment(g cleanup.Governor) string {
	seg := "governor " + g.State
	if g.State == "clear" || g.State == "throttled" {
		seg += fmt.Sprintf(" at %.0f%% cpu", g.CPUPercent)
	}
	if g.Detail != "" {
		seg += ": " + g.Detail
	}
	return seg
}

func cleanupJobLine(job cleanup.Job, paused bool) string {
	state := string(job.State())
	if paused && job.Blocked == nil && job.Phase.Physical() {
		state = "paused"
	}
	segs := []string{job.ID, state, job.Original, strconv.FormatUint(job.Removed, 10) + " entries"}
	switch {
	case job.Blocked != nil:
		segs = append(segs, job.Blocked.Reason+": "+job.Blocked.Detail)
	case job.Phase == cleanup.PhaseWaiting && len(job.Errors) > 0:
		segs = append(segs, job.Errors[len(job.Errors)-1].Message)
	}
	return strings.Join(segs, shipSep)
}

func newCleanupWaitCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "wait <job-id>",
		Short: "Wait until one job's tree is deleted",
		Long: `Wait until one job's tree is deleted.

It returns once the job is done, and fails with the blockage when the job stops
for an operator instead. It polls the job's status until then, and fails when
the daemon leaves any one poll unanswered for ` + cleanupStatusTimeout.String() + `.

` + cleanupReadNote,
		Args: cobra.MatchAll(cobra.ExactArgs(1), cleanupJobIDArg),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			svc, err := cleanupReadService(ctx)
			if err != nil {
				return fmt.Errorf("cleanup wait: %w", err)
			}
			job, err := waitCleanupJob(ctx, svc, args[0], "")
			if err != nil {
				return cleanupJobErr("cleanup wait", args[0], err)
			}
			cmd.Println(cleanupJobLine(job, false))
			return nil
		},
	}
}

func newCleanupPauseCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "pause",
		Short: "Stop physical deletion across the queue",
		Long: `Stop physical deletion across the queue.

Logical removals still run, so "worktree rm" keeps freeing paths; their trees
wait in the queue, durably, until "resume".`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			svc, err := cleanupService(ctx)
			if err != nil {
				return fmt.Errorf("cleanup pause: %w", err)
			}
			if err := svc.Pause(ctx); err != nil {
				return fmt.Errorf("cleanup pause: %w", err)
			}
			cmd.Println("deletion paused")
			return nil
		},
	}
}

func newCleanupResumeCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "resume",
		Short: "Let physical deletion continue across the queue",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			svc, err := cleanupService(ctx)
			if err != nil {
				return fmt.Errorf("cleanup resume: %w", err)
			}
			if err := svc.Resume(ctx); err != nil {
				return fmt.Errorf("cleanup resume: %w", err)
			}
			cmd.Println("deletion resumed")
			return nil
		},
	}
}

func newCleanupRetryCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "retry <job-id>",
		Short: "Run a blocked job again from the phase it stopped in",
		Long: `Run a blocked job again from the phase it stopped in.

A blocked job is never retried on its own: address the cause its blockage
names, then retry it. The job re-verifies everything it checked before.`,
		Args: cobra.MatchAll(cobra.ExactArgs(1), cleanupJobIDArg),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			svc, err := cleanupService(ctx)
			if err != nil {
				return fmt.Errorf("cleanup retry: %w", err)
			}
			job, err := svc.Retry(ctx, args[0])
			if err != nil {
				return cleanupJobErr("cleanup retry", args[0], err)
			}
			cmd.Println("retried " + cleanupJobLine(job, false))
			return nil
		},
	}
}

func newCleanupServeCmd() *cobra.Command {
	return &cobra.Command{
		Use:    "serve",
		Short:  "Run the per-user deletion daemon; launchd starts it",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cleanupServeDefault(cmd.Context())
		},
	}
}

func newCleanupAdoptCmd() *cobra.Command {
	var r cleanup.AdoptRequest
	cmd := &cobra.Command{
		Use:   "adopt",
		Short: "Queue one tree the retired janitor parked in its quarantine",
		Long: `Queue one tree the retired janitor parked in its quarantine.

Every flag is required and names exactly one tree: its canonical path, the
identity the operator validated, the repository it belonged to, and the legacy
recovery ref that must still resolve to the saved head. Nothing is discovered
and no manifest is read; any condition that fails refuses with nothing moved.`,
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runCleanupAdopt(cmd, r)
		},
	}
	cmd.Flags().StringVar(&r.Source, "source", "", "canonical absolute path of the quarantined tree")
	cmd.Flags().Uint64Var(&r.Tree.Dev, "dev", 0, "device number of the quarantined tree")
	cmd.Flags().Uint64Var(&r.Tree.Ino, "ino", 0, "inode number of the quarantined tree")
	cmd.Flags().StringVar(&r.CommonDir, "common-dir", "", "git common directory the tree was a worktree of")
	cmd.Flags().StringVar(&r.Head, "head", "", "commit the manifest saved for the tree")
	cmd.Flags().StringVar(&r.RecoveryRef, "recovery-ref", "", "legacy ref pinning the head")
	cmd.Flags().StringVar(&r.Original, "original", "", "path the worktree used to be registered at")
	for _, name := range []string{"source", "dev", "ino", "common-dir", "head", "recovery-ref", "original"} {
		_ = cmd.MarkFlagRequired(name)
	}
	return cmd
}

func runCleanupAdopt(cmd *cobra.Command, r cleanup.AdoptRequest) error {
	ctx := cmd.Context()
	git, err := cleanupGit(ctx, "cleanup adopt")
	if err != nil {
		return err
	}
	r.Owner, r.Git = legacyQuarantineOwner, git
	svc, err := cleanupService(ctx)
	if err != nil {
		return fmt.Errorf("cleanup adopt: %w", err)
	}
	adopter, ok := svc.(cleanup.Adopter)
	if !ok {
		return fmt.Errorf("cleanup adopt: %T adopts no quarantined tree", svc)
	}
	receipt, err := adopter.Adopt(ctx, r)
	if err != nil {
		return fmt.Errorf("cleanup adopt: %w", err)
	}
	cmd.Println(strings.Join([]string{"adopted " + filepath.Base(r.Source), "legacy quarantine", r.Source, "deletion queued " + receipt.JobID}, shipSep))
	return nil
}
