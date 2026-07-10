package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/yasyf/cc-context/internal/render"
	"github.com/yasyf/cc-context/internal/vcs"
)

const (
	shipSep                 = " · "
	shipLogBudget           = 2000
	jjNearestBookmarkRevset = "heads(::@- & bookmarks())"
	jjDescribeTemplate      = `commit_id.short() ++ "\n" ++ description.first_line()`
	jjBookmarkTemplate      = `local_bookmarks.map(|b| b.name()).join(" ")`
)

var (
	shipCIPollTries    = 12
	shipCIPollInterval = 5 * time.Second
)

// ansiRE matches CSI escape sequences (colour, cursor moves) so a captured log
// can be stripped to plain text before it is budget-capped.
var ansiRE = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]`)

// shipStreamCI reports whether CI watch output should stream live to w, which is
// true only when w is a real terminal. It mirrors stdinPiped's device check.
var shipStreamCI = func(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	info, err := f.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}

type shipOpts struct {
	message string
	noPush  bool
	noWatch bool
	amend   bool
	budget  int
}

type ciRun struct {
	DatabaseID   int64  `json:"databaseId"`
	WorkflowName string `json:"workflowName"`
	URL          string `json:"url"`
}

type ciView struct {
	WorkflowName string    `json:"workflowName"`
	Conclusion   string    `json:"conclusion"`
	StartedAt    time.Time `json:"startedAt"`
	UpdatedAt    time.Time `json:"updatedAt"`
	URL          string    `json:"url"`
	Jobs         []ciJob   `json:"jobs"`
}

type ciJob struct {
	Name       string   `json:"name"`
	Conclusion string   `json:"conclusion"`
	Steps      []ciStep `json:"steps"`
}

type ciStep struct {
	Name       string `json:"name"`
	Conclusion string `json:"conclusion"`
}

func newShipCmd() *cobra.Command {
	var o shipOpts
	cmd := &cobra.Command{
		Use:   "ship",
		Short: "Commit, push, and watch CI in one step",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runShip(cmd, o)
		},
	}
	cmd.Flags().StringVarP(&o.message, "message", "m", "", "commit message")
	cmd.Flags().BoolVar(&o.noPush, "no-push", false, "commit only; do not push or watch CI")
	cmd.Flags().BoolVar(&o.noWatch, "no-watch", false, "push but do not watch CI")
	cmd.Flags().BoolVar(&o.amend, "amend", false, "fold the working copy into the parent commit")
	cmd.Flags().IntVar(&o.budget, "budget", shipLogBudget, "token budget for the CI failure log excerpt (0 = uncapped)")
	return cmd
}

func runShip(cmd *cobra.Command, o shipOpts) error {
	ctx := cmd.Context()
	kind := vcs.Detect(workingDir())
	if kind == vcs.None {
		return errors.New("ship: no git or jj repository in the working directory")
	}
	if !o.amend && o.message == "" {
		return errors.New("ship: -m/--message is required unless --amend")
	}

	if err := shipCommit(ctx, kind, o); err != nil {
		return err
	}

	short, subject, err := shipDescribe(ctx, kind)
	if err != nil {
		return err
	}
	segments := []string{fmt.Sprintf("committed %s %q", short, subject)}

	if o.noPush {
		segments = append(segments, "not pushed")
		cmd.Println(strings.Join(segments, shipSep))
		return nil
	}

	branch, err := shipPush(ctx, kind, o.amend)
	if err != nil {
		return err
	}
	segments = append(segments, fmt.Sprintf("pushed %s → origin", branch))

	if o.noWatch {
		cmd.Println(strings.Join(segments, shipSep))
		return nil
	}

	ciSeg, report, ciErr := shipWatchCI(ctx, cmd.ErrOrStderr(), kind, o.budget)
	if ciSeg == "" {
		return ciErr
	}
	segments = append(segments, ciSeg)
	cmd.Println(strings.Join(segments, shipSep))
	for _, line := range report {
		cmd.Println(line)
	}
	return ciErr
}

func shipCommit(ctx context.Context, kind vcs.Kind, o shipOpts) error {
	switch kind {
	case vcs.JJ:
		return shipCommitJJ(ctx, o)
	case vcs.Git:
		return shipCommitGit(ctx, o)
	default:
		return errors.New("ship: commit: unsupported vcs")
	}
}

func shipCommitJJ(ctx context.Context, o shipOpts) error {
	var argv []string
	switch {
	case o.amend && o.message != "":
		argv = []string{"squash", "-m", o.message}
	case o.amend:
		argv = []string{"squash", "--use-destination-message"}
	default:
		argv = []string{"commit", "-m", o.message}
	}
	if _, err := render.RunCLI(ctx, "jj", argv); err != nil {
		return fmt.Errorf("ship: jj %s: %w", argv[0], err)
	}
	return nil
}

func shipCommitGit(ctx context.Context, o shipOpts) error {
	if _, err := render.RunCLI(ctx, "git", []string{"add", "-A"}); err != nil {
		return fmt.Errorf("ship: git add: %w", err)
	}
	var argv []string
	switch {
	case o.amend && o.message != "":
		argv = []string{"commit", "--amend", "-m", o.message}
	case o.amend:
		argv = []string{"commit", "--amend", "--no-edit"}
	default:
		argv = []string{"commit", "-m", o.message}
	}
	if _, err := render.RunCLI(ctx, "git", argv); err != nil {
		return fmt.Errorf("ship: git commit: %w", err)
	}
	return nil
}

func shipDescribe(ctx context.Context, kind vcs.Kind) (short, subject string, err error) {
	switch kind {
	case vcs.Git:
		out, rerr := render.RunCLI(ctx, "git", []string{"log", "-1", "--format=%h%x00%s"})
		if rerr != nil {
			return "", "", fmt.Errorf("ship: git log: %w", rerr)
		}
		return splitDescribe(out, "\x00")
	case vcs.JJ:
		out, rerr := render.RunCLI(ctx, "jj", []string{"log", "-r", "@-", "--no-graph", "-T", jjDescribeTemplate})
		if rerr != nil {
			return "", "", fmt.Errorf("ship: jj log: %w", rerr)
		}
		return splitDescribe(out, "\n")
	default:
		return "", "", errors.New("ship: describe: unsupported vcs")
	}
}

func splitDescribe(out, sep string) (string, string, error) {
	parts := strings.SplitN(strings.TrimRight(out, "\n"), sep, 2)
	if len(parts) != 2 {
		return "", "", fmt.Errorf("ship: malformed commit description %q", out)
	}
	return parts[0], parts[1], nil
}

func shipPush(ctx context.Context, kind vcs.Kind, amend bool) (string, error) {
	switch kind {
	case vcs.JJ:
		return shipPushJJ(ctx)
	case vcs.Git:
		return shipPushGit(ctx, amend)
	default:
		return "", errors.New("ship: push: unsupported vcs")
	}
}

func shipPushJJ(ctx context.Context) (string, error) {
	names, err := jjBookmarkNames(ctx, jjNearestBookmarkRevset)
	if err != nil {
		return "", err
	}
	if len(names) == 0 {
		return "", fmt.Errorf("ship: no bookmark to advance (%s matched none)", jjNearestBookmarkRevset)
	}
	branch := names[0]
	if _, err := render.RunCLI(ctx, "jj", []string{"bookmark", "move", "--from", jjNearestBookmarkRevset, "--to", "@-"}); err != nil {
		return "", fmt.Errorf("ship: advance bookmark %q: %w", branch, err)
	}
	if _, err := render.RunCLI(ctx, "jj", []string{"git", "push"}); err != nil {
		return "", fmt.Errorf("ship: jj git push: %w", err)
	}
	return branch, nil
}

func shipPushGit(ctx context.Context, amend bool) (string, error) {
	out, err := render.RunCLI(ctx, "git", []string{"branch", "--show-current"})
	if err != nil {
		return "", fmt.Errorf("ship: git branch --show-current: %w", err)
	}
	branch := strings.TrimSpace(out)
	if branch == "" {
		return "", errors.New("ship: detached HEAD; no branch to push")
	}
	argv := []string{"push"}
	if amend {
		argv = []string{"push", "--force-with-lease"}
	}
	if _, err := render.RunCLI(ctx, "git", argv); err != nil {
		return "", fmt.Errorf("ship: git push: %w", err)
	}
	return branch, nil
}

func jjBookmarkNames(ctx context.Context, rev string) ([]string, error) {
	out, err := render.RunCLI(ctx, "jj", []string{"log", "-r", rev, "--no-graph", "-T", jjBookmarkTemplate})
	if err != nil {
		return nil, fmt.Errorf("ship: jj bookmarks at %q: %w", rev, err)
	}
	return strings.Fields(out), nil
}

// shipWatchCI watches every CI run on the pushed commit and builds a per-run
// report. Only a shipHeadSHA failure yields an empty segment; infra failures
// return a segment so the summary still prints before the nonzero exit.
func shipWatchCI(ctx context.Context, errW io.Writer, kind vcs.Kind, budget int) (string, []string, error) {
	if _, err := exec.LookPath("gh"); err != nil {
		return "CI gh-missing", nil, nil
	}
	sha, err := shipHeadSHA(ctx, kind)
	if err != nil {
		return "", nil, err
	}
	runs, err := findCIRuns(ctx, sha)
	if err != nil {
		report := []string{fmt.Sprintf("check: gh run list --commit %s", sha)}
		return "CI error", report, err
	}
	if len(runs) == 0 {
		return "CI no-run", nil, nil
	}
	return reportCIRuns(ctx, errW, sha, runs, budget)
}

// reportCIRuns watches every run for sha and, after each batch, re-lists to catch
// workflows that registered late; a run is watched, viewed, and reported exactly
// once. A settle-time re-list failure takes the infra path with the report so far
// preserved.
func reportCIRuns(ctx context.Context, errW io.Writer, sha string, runs []ciRun, budget int) (string, []string, error) {
	type redRun struct {
		id   string
		view ciView
	}
	var report []string
	var reds []redRun
	seen := map[string]bool{}
	viewFailed := false

	process := func(batch []ciRun) int {
		n := 0
		for _, run := range batch {
			id := strconv.FormatInt(run.DatabaseID, 10)
			if seen[id] {
				continue
			}
			seen[id] = true
			n++
			// Watch drives live progress; gh run view is the authoritative conclusion,
			// so a dropped watch on a green run still passes.
			_ = watchCIRun(ctx, errW, id)
			view, err := viewCIRun(ctx, id)
			if err != nil {
				viewFailed = true
				report = append(report,
					strings.Join([]string{run.WorkflowName, "view-error", run.URL}, shipSep),
					fmt.Sprintf("view error: %v", err))
				continue
			}
			report = append(report, ciRunLine(view))
			if view.Conclusion == "" {
				// Watch exited early (transient): the run has no conclusion yet, which
				// is indeterminate, not red — take the infra path, not --log-failed.
				viewFailed = true
				report = append(report, fmt.Sprintf("run %s has not concluded; check: gh run view %s", id, id))
				continue
			}
			if !ciGreen(view.Conclusion) {
				reds = append(reds, redRun{id: id, view: view})
			}
		}
		return n
	}

	process(runs)
	for {
		more, err := findCIRuns(ctx, sha)
		if err != nil {
			report = append(report, fmt.Sprintf("check: gh run list --commit %s", sha))
			return "CI error", report, err
		}
		if process(more) == 0 {
			break
		}
	}

	if len(reds) > 0 {
		per := budget / len(reds)
		if budget > 0 && per < 1 {
			per = 1
		}
		for _, r := range reds {
			report = append(report, ciFailureDetail(ctx, r.id, r.view, per)...)
		}
		return "CI failure", report, fmt.Errorf("ship: CI failed for %d run(s) on the pushed commit", len(reds))
	}
	if viewFailed {
		return "CI error", report, errors.New("ship: gh run view could not read the CI run conclusion")
	}
	return "CI success", report, nil
}

// watchCIRun blocks until run id concludes, streaming gh's progress to errW on a
// real terminal and otherwise buffering it away.
func watchCIRun(ctx context.Context, errW io.Writer, id string) error {
	if shipStreamCI(errW) {
		return render.RunCLIStream(ctx, "gh", []string{"run", "watch", id, "--exit-status", "--compact"}, errW)
	}
	_, err := render.RunCLI(ctx, "gh", []string{"run", "watch", id, "--exit-status"})
	return err
}

func viewCIRun(ctx context.Context, id string) (ciView, error) {
	out, err := render.RunCLI(ctx, "gh", []string{"run", "view", id, "--json", "workflowName,conclusion,startedAt,updatedAt,url,jobs"})
	if err != nil {
		return ciView{}, fmt.Errorf("ship: gh run view %s: %w", id, err)
	}
	var view ciView
	if err := json.Unmarshal([]byte(out), &view); err != nil {
		return ciView{}, fmt.Errorf("ship: parse gh run view %s: %w", id, err)
	}
	return view, nil
}

func ciRunLine(view ciView) string {
	parts := []string{view.WorkflowName, view.Conclusion}
	if d := ciDuration(view.StartedAt, view.UpdatedAt); d != "" {
		parts = append(parts, d)
	}
	parts = append(parts, view.URL)
	return strings.Join(parts, shipSep)
}

// ciFailureDetail names each red job and its failed steps, appends the
// ANSI-stripped, budget-capped --log-failed excerpt (fetch failure is non-fatal),
// and always ends with the full-log pointer.
func ciFailureDetail(ctx context.Context, id string, view ciView, budget int) []string {
	var lines []string
	for _, job := range view.Jobs {
		if ciGreen(job.Conclusion) {
			continue
		}
		line := "failed: " + job.Name
		var steps []string
		for _, s := range job.Steps {
			if !ciGreen(s.Conclusion) {
				steps = append(steps, s.Name)
			}
		}
		if len(steps) > 0 {
			line += shipSep + strings.Join(steps, ", ")
		}
		lines = append(lines, line)
	}
	if log, err := render.RunCLI(ctx, "gh", []string{"run", "view", id, "--log-failed"}); err != nil {
		lines = append(lines, fmt.Sprintf("log unavailable: %v", err))
	} else if excerpt := strings.TrimRight(render.Cap(ansiRE.ReplaceAllString(log, ""), budget), "\n"); excerpt != "" {
		lines = append(lines, excerpt)
	}
	return append(lines, fmt.Sprintf("full log: gh run view %s --log-failed", id))
}

// ciGreen reports whether a conclusion counts as passing; skipped and neutral
// (path-filtered workflows) are green, not failures.
func ciGreen(conclusion string) bool {
	switch conclusion {
	case "success", "skipped", "neutral":
		return true
	default:
		return false
	}
}

// ciDuration formats end-start as whole seconds, omitting it for a zero start or
// a negative span so the report never shows a negative duration.
func ciDuration(start, end time.Time) string {
	if start.IsZero() {
		return ""
	}
	d := end.Sub(start)
	if d < 0 {
		return ""
	}
	return fmt.Sprintf("%ds", int(d.Round(time.Second).Seconds()))
}

func shipHeadSHA(ctx context.Context, kind vcs.Kind) (string, error) {
	switch kind {
	case vcs.Git:
		out, err := render.RunCLI(ctx, "git", []string{"rev-parse", "HEAD"})
		if err != nil {
			return "", fmt.Errorf("ship: git rev-parse HEAD: %w", err)
		}
		return strings.TrimSpace(out), nil
	case vcs.JJ:
		out, err := render.RunCLI(ctx, "jj", []string{"log", "-r", "@-", "--no-graph", "-T", "commit_id"})
		if err != nil {
			return "", fmt.Errorf("ship: jj log commit_id: %w", err)
		}
		return strings.TrimSpace(out), nil
	default:
		return "", errors.New("ship: head sha: unsupported vcs")
	}
}

// findCIRuns polls gh for the runs on sha (server-side --commit filter, no
// client-side compare). Transient list or parse errors are tolerated across the
// window; an exhausted window returns the last error, or nil,nil for a clean
// no-run.
func findCIRuns(ctx context.Context, sha string) ([]ciRun, error) {
	var lastErr error
	for i := 0; i < shipCIPollTries; i++ {
		out, err := render.RunCLI(ctx, "gh", []string{"run", "list", "--commit", sha, "--limit", "10", "--json", "databaseId,workflowName,status,url"})
		switch {
		case err != nil:
			lastErr = fmt.Errorf("ship: gh run list: %w", err)
		default:
			var runs []ciRun
			if uerr := json.Unmarshal([]byte(out), &runs); uerr != nil {
				lastErr = fmt.Errorf("ship: parse gh run list: %w", uerr)
			} else if len(runs) > 0 {
				return runs, nil
			} else {
				lastErr = nil
			}
		}
		if i < shipCIPollTries-1 {
			if serr := sleepCtx(ctx, shipCIPollInterval); serr != nil {
				return nil, serr
			}
		}
	}
	return nil, lastErr
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
