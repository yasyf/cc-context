package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
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
	shipCIQuietPolls        = 2
	jjNearestBookmarkRevset = "heads(::@- & bookmarks())"
	jjDescribeTemplate      = `commit_id.short() ++ "\n" ++ description.first_line()`
	jjBookmarkTemplate      = `local_bookmarks.map(|b| b.name()).join(" ") ++ " "`
	jjTrunkBookmarkTemplate = `remote_bookmarks.map(|b| b.name()).join(" ") ++ " "`
	jjAncestorRevsetFmt     = `bookmarks(exact:%q) & ::@-`
	jjStackRevsetFmt        = `bookmarks(exact:%q)..@-`
	jjConflictRevsetFmt     = `conflicts() & (bookmarks(exact:%q)..@-)::`
	jjStackLineTemplate     = `commit_id.short() ++ " " ++ description.first_line() ++ "\n"`
	jjOpIDTemplate          = `id`
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
	message   string
	noPush    bool
	noWatch   bool
	noVerify  bool
	amend     bool
	budget    int
	paths     []string
	bookmark  string
	skipHunks []string
	onlyHunks []string
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
		Use:   "ship [paths...]",
		Short: "Commit, push, and watch CI in one step",
		Long: `Commit, push, and watch CI in one step.

On a jj repo, ship fetches from the remote first and, when the target bookmark is no longer an ancestor of the local stack, rebases the stack onto it before advancing and pushing; a rebase that would conflict is rolled back and reported instead of pushed.`,
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			o.paths = args
			return runShip(cmd, o)
		},
	}
	cmd.Flags().StringVarP(&o.message, "message", "m", "", "commit message")
	cmd.Flags().BoolVar(&o.noPush, "no-push", false, "commit only; do not push or watch CI")
	cmd.Flags().BoolVar(&o.noWatch, "no-watch", false, "push but do not watch CI")
	cmd.Flags().BoolVar(&o.noVerify, "no-verify", false, "skip pre-commit hooks (uvx prek) before committing")
	cmd.Flags().BoolVar(&o.amend, "amend", false, "fold the working copy into the parent commit")
	cmd.Flags().IntVar(&o.budget, "budget", shipLogBudget, "token budget for the CI failure log excerpt (0 = uncapped)")
	cmd.Flags().StringVar(&o.bookmark, "bookmark", "", "jj bookmark to advance and push")
	cmd.Flags().StringArrayVar(&o.skipHunks, "skip-hunk", nil, "commit everything except this hunk ref (repeatable; refs from ccx vcs hunks)")
	cmd.Flags().StringArrayVar(&o.onlyHunks, "only-hunk", nil, "commit only this hunk ref in its file (repeatable; refs from ccx vcs hunks)")
	return cmd
}

func runShip(cmd *cobra.Command, o shipOpts) error {
	ctx := cmd.Context()
	kind, root := vcs.DetectRoot(workingDir())
	if kind == vcs.None {
		return errors.New("ship: no git or jj repository in the working directory")
	}
	if o.bookmark != "" && kind != vcs.JJ {
		return errors.New("ship: --bookmark applies only to jj repositories")
	}
	if !o.amend && o.message == "" {
		return errors.New("ship: -m/--message is required unless --amend")
	}

	sel, err := parseShipSelection(ctx, kind, o)
	if err != nil {
		return err
	}
	if sel != nil {
		if err := resolveShipSelection(ctx, kind, sel); err != nil {
			return err
		}
	}

	hookSeg, err := shipCommit(ctx, cmd.ErrOrStderr(), root, kind, o, sel)
	if err != nil {
		return err
	}

	short, subject, err := shipDescribe(ctx, kind)
	if err != nil {
		return err
	}
	segments := make([]string, 0, 5)
	if hookSeg != "" {
		segments = append(segments, hookSeg)
	}
	committedSegment := len(segments)
	segments = append(segments, fmt.Sprintf("committed %s %q", short, subject))

	if o.noPush {
		segments = append(segments, "not pushed")
		cmd.Println(strings.Join(segments, shipSep))
		return nil
	}

	branch, rebased, err := shipPush(ctx, kind, o)
	if err != nil {
		return err
	}
	if rebased > 0 {
		short, subject, err = shipDescribe(ctx, kind)
		if err != nil {
			return err
		}
		segments[committedSegment] = fmt.Sprintf("committed %s %q", short, subject)
		segments = append(segments, fmt.Sprintf("rebased %d commit(s) onto %s", rebased, branch))
	}
	segments = append(segments, fmt.Sprintf("pushed %s → origin", branch))

	if o.noWatch {
		cmd.Println(strings.Join(segments, shipSep))
		return nil
	}

	ciSeg, report, ciErr := shipWatchCI(ctx, cmd.ErrOrStderr(), kind, o.budget)
	if ciSeg == "" {
		cmd.Println(strings.Join(segments, shipSep))
		return ciErr
	}
	segments = append(segments, ciSeg)
	cmd.Println(strings.Join(segments, shipSep))
	for _, line := range report {
		cmd.Println(line)
	}
	return ciErr
}

const envClaudeSessionKey = "CLAUDE_CODE_SESSION_ID"

func withSessionTrailer(message string) string {
	id := os.Getenv(envClaudeSessionKey)
	if id == "" || message == "" {
		return message
	}
	return message + "\n\nClaude-Session-Id: " + id
}

// shipCommit stages, runs pre-commit hooks, and commits. Hunk-scoped selections
// report "hooks hunk-skip" instead: external prek would inspect full worktree
// files, not the partial content being committed through a throwaway index.
// It returns the hook summary segment to prepend to the ship summary.
func shipCommit(ctx context.Context, errW io.Writer, root string, kind vcs.Kind, o shipOpts, sel *shipSelection) (string, error) {
	o.message = withSessionTrailer(o.message)
	var seg string
	if kind == vcs.Git && sel == nil {
		if err := shipGitAdd(ctx, o); err != nil {
			return "", err
		}
	}
	if sel != nil && !o.noVerify && shipHasHookConfig(root) {
		seg = "hooks hunk-skip"
	}
	if sel == nil {
		var err error
		seg, err = shipRunHooks(ctx, errW, root, kind, o)
		if err != nil {
			return "", err
		}
	}
	switch kind {
	case vcs.JJ:
		return seg, shipCommitJJ(ctx, o, sel)
	case vcs.Git:
		return seg, shipCommitGit(ctx, o, sel)
	default:
		return "", errors.New("ship: commit: unsupported vcs")
	}
}

// shipGitAdd stages the ship's paths (or everything, when unscoped) into the real
// index ahead of hook attempts and the commit.
func shipGitAdd(ctx context.Context, o shipOpts) error {
	addArgv := []string{"add", "-A"}
	if len(o.paths) > 0 {
		addArgv = append(addArgv, "--")
		addArgv = append(addArgv, o.paths...)
	}
	if _, err := render.RunCLI(ctx, "git", addArgv); err != nil {
		return fmt.Errorf("ship: git add: %w", err)
	}
	return nil
}

func shipCommitJJ(ctx context.Context, o shipOpts, sel *shipSelection) error {
	if sel != nil {
		return shipCommitJJSelect(ctx, o, sel)
	}
	argv := make([]string, 0, 4+len(o.paths))
	switch {
	case o.amend && o.message != "":
		argv = append(argv, "squash", "-m", o.message)
	case o.amend:
		argv = append(argv, "squash", "--use-destination-message")
	default:
		argv = append(argv, "commit", "-m", o.message)
	}
	if len(o.paths) > 0 {
		argv = append(argv, "--")
		argv = append(argv, o.paths...)
	}
	if _, err := render.RunCLI(ctx, "jj", argv); err != nil {
		return fmt.Errorf("ship: jj %s: %w", argv[0], err)
	}
	return nil
}

// shipCommitJJSelect commits a hunk selection through jj's diff-editor protocol:
// it writes a plan tempfile plus a sidecar, points a throwaway merge tool at
// ccx's own apply-selection subcommand, and lets jj drive the partial commit
// inside its transaction. On failure it prefers the sidecar's structured reason
// over raw jj stderr.
func shipCommitJJSelect(ctx context.Context, o shipOpts, sel *shipSelection) error {
	sidecar, err := os.CreateTemp("", "ccx-ship-result-*")
	if err != nil {
		return fmt.Errorf("ship: create result file: %w", err)
	}
	sidecarPath := sidecar.Name()
	_ = sidecar.Close()
	defer func() { _ = os.Remove(sidecarPath) }()

	planBytes, err := json.Marshal(buildSelectionPlan(sel, sidecarPath))
	if err != nil {
		return fmt.Errorf("ship: encode selection plan: %w", err)
	}
	planFile, err := os.CreateTemp("", "ccx-ship-plan-*.json")
	if err != nil {
		return fmt.Errorf("ship: create selection plan: %w", err)
	}
	planPath := planFile.Name()
	defer func() { _ = os.Remove(planPath) }()
	if _, err := planFile.Write(planBytes); err != nil {
		_ = planFile.Close()
		return fmt.Errorf("ship: write selection plan: %w", err)
	}
	if err := planFile.Close(); err != nil {
		return fmt.Errorf("ship: write selection plan: %w", err)
	}

	argv, err := jjSelectArgv(o, planPath)
	if err != nil {
		return err
	}
	if _, err := render.RunCLI(ctx, "jj", argv); err != nil {
		if reason := readSidecar(sidecarPath); reason != "" {
			return fmt.Errorf("ship: %s: %w", reason, err)
		}
		return fmt.Errorf("ship: jj %s: %w", argv[0], err)
	}
	return nil
}

func shipCommitGit(ctx context.Context, o shipOpts, sel *shipSelection) error {
	if sel != nil {
		return shipCommitGitSelect(ctx, o, sel)
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
	if o.noVerify {
		argv = append(argv, "--no-verify")
	}
	if len(o.paths) > 0 {
		argv = append(argv, "--")
		argv = append(argv, o.paths...)
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

func shipPush(ctx context.Context, kind vcs.Kind, o shipOpts) (string, int, error) {
	switch kind {
	case vcs.JJ:
		return shipPushJJ(ctx, o)
	case vcs.Git:
		branch, err := shipPushGit(ctx, o.amend)
		return branch, 0, err
	default:
		return "", 0, errors.New("ship: push: unsupported vcs")
	}
}

func shipPushJJ(ctx context.Context, o shipOpts) (string, int, error) {
	if _, err := render.RunCLI(ctx, "jj", []string{"git", "fetch"}); err != nil {
		return "", 0, fmt.Errorf("ship: jj git fetch: %w", err)
	}

	target := o.bookmark
	diverged := false
	if target == "" {
		trunkNames, err := jjTrunkBookmarkNames(ctx)
		if err != nil {
			return "", 0, err
		}
		if len(trunkNames) != 1 {
			return "", 0, fmt.Errorf("ship: cannot resolve the trunk bookmark from %q; pass --bookmark <name>", trunkNames)
		}
		trunk := trunkNames[0]

		names, err := jjBookmarkNames(ctx, jjNearestBookmarkRevset)
		if err != nil {
			return "", 0, err
		}
		switch len(names) {
		case 0:
			diverged = true
		case 1:
		default:
			return "", 0, fmt.Errorf("ship: multiple nearest bookmarks %q; pass --bookmark <name> to choose one", strings.Join(names, ", "))
		}
		if !diverged && names[0] != trunk {
			return "", 0, fmt.Errorf("ship: nearest bookmark %q is not trunk %q — pass --bookmark %s to advance it deliberately", names[0], trunk, names[0])
		}
		if diverged {
			target = trunk
		} else {
			target = names[0]
		}
	}

	// jj treats a bare NAMES argument as a glob and no-ops with exit 0 on
	// zero matches, and a conflicted bookmark resolves to multiple commits,
	// which rebase would silently treat as a merge destination; resolve the
	// exact name up front so both fail loudly.
	heads, err := jjLogLines(ctx, fmt.Sprintf(`bookmarks(exact:%q)`, target))
	if err != nil {
		return "", 0, err
	}
	switch {
	case len(heads) == 0:
		return "", 0, fmt.Errorf("ship: bookmark %q not found", target)
	case len(heads) > 1:
		return "", 0, fmt.Errorf("ship: bookmark %q is conflicted (%d heads); resolve it (jj bookmark list --conflicted) before shipping", target, len(heads))
	}

	if o.bookmark != "" {
		ancestors, err := jjBookmarkNames(ctx, fmt.Sprintf(jjAncestorRevsetFmt, target))
		if err != nil {
			return "", 0, err
		}
		diverged = len(ancestors) == 0
	}

	rebased := 0
	if diverged {
		var err error
		rebased, err = jjRebaseOnto(ctx, target)
		if err != nil {
			return "", 0, err
		}
	}

	if _, err := render.RunCLI(ctx, "jj", []string{"bookmark", "move", "exact:" + target, "--to", "@-"}); err != nil {
		return "", 0, fmt.Errorf("ship: advance bookmark %q: %w", target, err)
	}
	if _, err := render.RunCLI(ctx, "jj", []string{"git", "push", "--bookmark", "exact:" + target}); err != nil {
		return "", 0, fmt.Errorf("ship: jj git push: %w", err)
	}
	return target, rebased, nil
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

func jjTrunkBookmarkNames(ctx context.Context) ([]string, error) {
	out, err := render.RunCLI(ctx, "jj", []string{"log", "-r", "trunk()", "--no-graph", "-T", jjTrunkBookmarkTemplate})
	if err != nil {
		return nil, fmt.Errorf("ship: jj trunk bookmark: %w", err)
	}
	var names []string
	seen := map[string]bool{}
	for _, name := range strings.Fields(out) {
		if !seen[name] {
			names = append(names, name)
			seen[name] = true
		}
	}
	return names, nil
}

func jjLogLines(ctx context.Context, rev string) ([]string, error) {
	out, err := render.RunCLI(ctx, "jj", []string{"log", "-r", rev, "--no-graph", "-T", jjStackLineTemplate})
	if err != nil {
		return nil, fmt.Errorf("ship: jj log %q: %w", rev, err)
	}
	var lines []string
	for _, line := range strings.Split(out, "\n") {
		if line != "" {
			lines = append(lines, line)
		}
	}
	return lines, nil
}

func jjOpID(ctx context.Context) (string, error) {
	out, err := render.RunCLI(ctx, "jj", []string{"op", "log", "-n", "1", "--no-graph", "-T", jjOpIDTemplate})
	if err != nil {
		return "", fmt.Errorf("ship: jj op log: %w", err)
	}
	opID := strings.TrimSpace(out)
	if opID == "" {
		return "", fmt.Errorf("ship: malformed jj operation ID %q", out)
	}
	return opID, nil
}

func jjRebaseOnto(ctx context.Context, target string) (int, error) {
	stack, err := jjLogLines(ctx, fmt.Sprintf(jjStackRevsetFmt, target))
	if err != nil {
		return 0, err
	}
	if len(stack) == 0 {
		return 0, fmt.Errorf("ship: %q..@- is empty — the commit already landed on %q; refusing to move the bookmark backwards", target, target)
	}

	opID, err := jjOpID(ctx)
	if err != nil {
		return 0, err
	}
	if _, err := render.RunCLI(ctx, "jj", []string{"rebase", "-b", "@-", "--destination", fmt.Sprintf(`bookmarks(exact:%q)`, target)}); err != nil {
		return 0, fmt.Errorf("ship: jj rebase onto %q: %w", target, err)
	}

	// rebase -b @- rewrites every descendant of the stack, including siblings
	// of @; check the whole rewritten set without including conflicts below it.
	conflicts, err := jjLogLines(ctx, fmt.Sprintf(jjConflictRevsetFmt, target))
	if err != nil {
		_, rerr := render.RunCLI(ctx, "jj", []string{"op", "restore", opID})
		if rerr == nil {
			return 0, fmt.Errorf("ship: conflict check after rebase onto %q failed (rebase rolled back): %w", target, err)
		}
		return 0, fmt.Errorf("ship: conflict check after rebase onto %q failed: %w; rollback also failed: %w — run: jj op restore %s", target, err, rerr, opID)
	}
	if len(conflicts) > 0 {
		// Restore the whole operation so a conflicted @ rolls back with the stack.
		if _, rerr := render.RunCLI(ctx, "jj", []string{"op", "restore", opID}); rerr != nil {
			return 0, fmt.Errorf("ship: rebase onto %q conflicted and rollback failed: %w — run: jj op restore %s, then resolve manually", target, rerr, opID)
		}
		return 0, fmt.Errorf("ship: rebase onto %q conflicts in %d commit(s); rolled back to the pre-rebase state\nconflicted:\n  %s\nresolve manually: jj rebase -b @- --destination 'bookmarks(exact:%q)', fix the conflicts (jj status), then: jj bookmark move exact:%s --to @- && jj git push --bookmark exact:%s", target, len(conflicts), strings.Join(conflicts, "\n  "), target, target, target)
	}
	return len(stack), nil
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
		hasWorkflows, err := shipHasWorkflows()
		if err != nil {
			return "CI error", nil, err
		}
		if !hasWorkflows {
			return "CI no-run", nil, nil
		}
		return "CI unconfirmed", nil, fmt.Errorf("ship: no CI run was registered for the pushed commit; workflows may be paths-filtered or dispatch-only (on: workflow_dispatch); confirm manually: gh run list --commit %s", sha)
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
	quiet := 0
	for quiet < shipCIQuietPolls {
		if err := sleepCtx(ctx, shipCIPollInterval); err != nil {
			return "CI error", report, err
		}
		more, err := findCIRuns(ctx, sha)
		if err != nil {
			report = append(report, fmt.Sprintf("check: gh run list --commit %s", sha))
			return "CI error", report, err
		}
		if process(more) == 0 {
			quiet++
		} else {
			quiet = 0
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
// and always ends with the full-log pointer plus the ci-triage agent handoff.
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
	return append(lines, fmt.Sprintf("full log: gh run view %s --log-failed", id)+shipSep+"triage: spawn the cc-context:ci-triage agent with this run id")
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

func shipHasWorkflows() (bool, error) {
	entries, err := os.ReadDir(filepath.Join(workingDir(), ".github", "workflows"))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("ship: read GitHub Actions workflows: %w", err)
	}
	for _, entry := range entries {
		ext := filepath.Ext(entry.Name())
		if !entry.IsDir() && (ext == ".yml" || ext == ".yaml") {
			return true, nil
		}
	}
	return false, nil
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
		out, err := render.RunCLI(ctx, "gh", []string{"run", "list", "--commit", sha, "--limit", "50", "--json", "databaseId,workflowName,status,url"})
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
