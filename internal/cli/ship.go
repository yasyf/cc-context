package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/yasyf/cc-context/internal/render"
	"github.com/yasyf/cc-context/internal/vcs"
)

const (
	shipSep                 = " · "
	jjNearestBookmarkRevset = "heads(::@- & bookmarks())"
	jjDescribeTemplate      = `commit_id.short() ++ "\n" ++ description.first_line()`
	jjBookmarkTemplate      = `local_bookmarks.map(|b| b.name()).join(" ")`
)

var (
	shipCIPollTries    = 6
	shipCIPollInterval = 3 * time.Second
)

type shipOpts struct {
	message string
	noPush  bool
	noWatch bool
	amend   bool
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

	ciSeg, ciErr := shipWatchCI(ctx, kind)
	if ciSeg == "" {
		return ciErr
	}
	segments = append(segments, ciSeg)
	cmd.Println(strings.Join(segments, shipSep))
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

func shipWatchCI(ctx context.Context, kind vcs.Kind) (string, error) {
	if _, err := exec.LookPath("gh"); err != nil {
		return "CI gh-missing", nil
	}
	sha, err := shipHeadSHA(ctx, kind)
	if err != nil {
		return "", err
	}
	id, err := findCIRun(ctx, sha)
	if err != nil {
		return "", err
	}
	if id == "" {
		return "CI no-run", nil
	}
	if _, err := render.RunCLI(ctx, "gh", []string{"run", "watch", id, "--exit-status"}); err != nil {
		return "CI failure", fmt.Errorf("ship: CI run %s failed: %w", id, err)
	}
	return "CI success", nil
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

func findCIRun(ctx context.Context, sha string) (string, error) {
	type run struct {
		DatabaseID int64  `json:"databaseId"`
		HeadSha    string `json:"headSha"`
		Status     string `json:"status"`
	}
	for i := 0; i < shipCIPollTries; i++ {
		out, err := render.RunCLI(ctx, "gh", []string{"run", "list", "--limit", "1", "--json", "databaseId,headSha,status"})
		if err != nil {
			return "", fmt.Errorf("ship: gh run list: %w", err)
		}
		var runs []run
		if err := json.Unmarshal([]byte(out), &runs); err != nil {
			return "", fmt.Errorf("ship: parse gh run list: %w", err)
		}
		if len(runs) > 0 && runs[0].HeadSha == sha {
			return strconv.FormatInt(runs[0].DatabaseID, 10), nil
		}
		if i < shipCIPollTries-1 {
			if err := sleepCtx(ctx, shipCIPollInterval); err != nil {
				return "", err
			}
		}
	}
	return "", nil
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
