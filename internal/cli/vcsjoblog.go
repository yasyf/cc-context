package cli

import (
	"bytes"
	"cmp"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/yasyf/cc-context/internal/render"
	"github.com/yasyf/cc-context/internal/vcs"
)

const (
	jobLogBudget    = 4000
	jobLogCompleted = "completed"
)

func newVcsJobLogCmd() *cobra.Command {
	var (
		repo   string
		budget int
	)
	cmd := &cobra.Command{
		Use:   "job-log <job-id>",
		Short: "Print a settled GitHub Actions job's log, whether or not its run has finished",
		Long: `Print one GitHub Actions job's log as plain text as soon as that job settles,
without waiting for the rest of its run, which gh run view --log requires.

Escape sequences are stripped. Past --budget the end of the log is kept, since
that is where a failure says why, and a first line names what was dropped. The
read goes through the same gateway as ccx vcs gh.

A job that has not settled has no log to read yet, and the error says so.
ccx vcs pr watch --job names this command, with the job id, when a check fails.
The id of a check another app reports names no Actions job, so GitHub answers
404.`,
		Example: "  ccx vcs job-log 126958155452\n  ccx vcs job-log 126958155452 -R o/r --budget 0",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := strconv.ParseInt(args[0], 10, 64)
			if err != nil {
				return fmt.Errorf("vcs job-log: job id must be a number, not %q", args[0])
			}
			return runVcsJobLog(cmd, id, repo, budget)
		},
	}
	cmd.Flags().StringVarP(&repo, "repo", "R", "", "the owner/name repository (default: the current checkout's)")
	cmd.Flags().IntVar(&budget, "budget", jobLogBudget, "token budget for the log's tail (0 = uncapped)")
	return cmd
}

func runVcsJobLog(cmd *cobra.Command, id int64, repo string, budget int) error {
	ctx := cmd.Context()
	if repo == "" {
		looked, err := vcs.LookupRepo(ctx, render.Dir(workingDir(ctx)), false)
		if err != nil {
			return fmt.Errorf("vcs job-log: name the repository with --repo: %w", err)
		}
		repo = looked.NameWithOwner
	}
	job := fmt.Sprintf("repos/%s/actions/jobs/%d", repo, id)
	log, err := jobLogRead(cmd, []string{"api", job + "/logs", "--allow-escape-sequences"})
	if err != nil {
		status, statusErr := jobLogRead(cmd, []string{"api", job, "--jq", ".status"})
		if status = strings.TrimSpace(status); statusErr == nil && status != jobLogCompleted {
			return fmt.Errorf("vcs job-log: job %d is %s; GitHub serves its log once it settles", id, status)
		}
		return fmt.Errorf("vcs job-log: job %d in %s: %w", id, repo, err)
	}
	plain := strings.TrimPrefix(ansiRE.ReplaceAllString(log, ""), "\ufeff")
	_, err = fmt.Fprint(cmd.OutOrStdout(), render.CapTail(plain, budget))
	return err
}

func jobLogRead(cmd *cobra.Command, argv []string) (string, error) {
	var stdout, stderr bytes.Buffer
	gateway := &cobra.Command{}
	gateway.SetContext(cmd.Context())
	gateway.SetOut(&stdout)
	gateway.SetErr(&stderr)
	if err := runVcsGh(gateway, argv, 0); err != nil {
		return "", errors.New(cmp.Or(strings.TrimSpace(stderr.String()), err.Error()))
	}
	_, _ = cmd.ErrOrStderr().Write(stderr.Bytes())
	return stdout.String(), nil
}
