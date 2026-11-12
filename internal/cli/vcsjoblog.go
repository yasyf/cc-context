package cli

import (
	"bytes"
	"cmp"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/yasyf/cc-context/internal/render"
	"github.com/yasyf/cc-context/internal/vcs"
)

const (
	jobLogBudget    = 4000
	jobLogCompleted = "completed"
	jobStepFailed   = "##[error]Process completed with exit code"
)

var (
	pytestSummaryRE = regexp.MustCompile(`(?m)^(?:\d{4}-\d\d-\d\dT\S+Z )?=+ short test summary info =+\r?$`)
	failedTestRE    = regexp.MustCompile(`(?m)^(?:\d{4}-\d\d-\d\dT\S+Z )?(?:FAILED|ERROR) ([^\s\[]+(?:\[.*?\])?)(?: - .*)?\r?$`)
	pytestEndRE     = regexp.MustCompile(`\d+ (?:failed|errors?)\b.* in [\d.]+s\b`)
)

func newVcsJobLogCmd() *cobra.Command {
	var (
		repo   string
		budget int
		failed bool
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
404.

--failed-tests prints, one per line, the pytest node ids in the short test
summary of the pytest run that failed the job's step, and nothing when no such
run ended it: a summary followed by other output before the step failed, such
as a negative control's expected failure, names no test. ccx vcs merge
--red-on-main reads a red job the same way.`,
		Example: "  ccx vcs job-log 126958155452\n  ccx vcs job-log 126958155452 -R o/r --budget 0\n  ccx vcs job-log 126958155452 -R o/r --failed-tests",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := strconv.ParseInt(args[0], 10, 64)
			if err != nil {
				return fmt.Errorf("vcs job-log: job id must be a number, not %q", args[0])
			}
			return runVcsJobLog(cmd, id, repo, budget, failed)
		},
	}
	cmd.Flags().StringVarP(&repo, "repo", "R", "", "the owner/name repository (default: the current checkout's)")
	cmd.Flags().IntVar(&budget, "budget", jobLogBudget, "token budget for the log's tail (0 = uncapped)")
	cmd.Flags().BoolVar(&failed, "failed-tests", false, "print only the pytest node ids the run that failed the job's step names, one per line")
	cmd.MarkFlagsMutuallyExclusive("budget", "failed-tests")
	return cmd
}

func runVcsJobLog(cmd *cobra.Command, id int64, repo string, budget int, failed bool) error {
	ctx := cmd.Context()
	if repo == "" {
		looked, err := vcs.LookupRepo(ctx, render.Dir(workingDir(ctx)), false)
		if err != nil {
			return fmt.Errorf("vcs job-log: name the repository with --repo: %w", err)
		}
		repo = looked.NameWithOwner
	}
	job := fmt.Sprintf("repos/%s/actions/jobs/%d", repo, id)
	log, err := jobLogRead(cmd, []string{"api", job + "/logs"})
	if err != nil {
		status, statusErr := jobLogRead(cmd, []string{"api", job, "--jq", ".status"})
		if status = strings.TrimSpace(status); statusErr == nil && status != jobLogCompleted {
			return fmt.Errorf("vcs job-log: job %d is %s; GitHub serves its log once it settles", id, status)
		}
		return fmt.Errorf("vcs job-log: job %d in %s: %w", id, repo, err)
	}
	plain := strings.TrimPrefix(log, "\ufeff")
	if failed {
		for _, test := range failedTests(plain) {
			if _, err := fmt.Fprintln(cmd.OutOrStdout(), test); err != nil {
				return err
			}
		}
		return nil
	}
	_, err = fmt.Fprint(cmd.OutOrStdout(), render.CapTail(plain, budget))
	return err
}

func failedTests(log string) []string {
	step, _, ended := strings.Cut(ansiRE.ReplaceAllString(log, ""), jobStepFailed)
	run := step[:max(strings.LastIndex(step, "\n"), 0)]
	headers := pytestSummaryRE.FindAllStringIndex(run, -1)
	if !ended || len(headers) == 0 || !pytestEndRE.MatchString(run[strings.LastIndex(run, "\n")+1:]) {
		return nil
	}
	var tests []string
	for _, test := range failedTestRE.FindAllStringSubmatch(run[headers[len(headers)-1][0]:], -1) {
		tests = append(tests, test[1])
	}
	return tests
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
