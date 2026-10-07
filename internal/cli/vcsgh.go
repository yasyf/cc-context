package cli

import (
	"errors"
	"os"
	"os/exec"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/yasyf/cc-context/internal/ghapi"
	"github.com/yasyf/cc-context/internal/render"
)

func newVcsGhCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "gh -- <gh args>...",
		Short: "Run a read-only gh command on the GitHub App's quota",
		Long: `Run gh with GH_TOKEN set to the read-only GitHub App installation token
ccx's own reads use, so a poll such as gh pr checks, gh pr view, or
gh run watch draws on the installation's quota instead of the gh user's.
ccx vcs auth status names the app and both quotas.

The token is read-only, so a write fails; run writes through gh itself. The
repository is --repo's (or GH_REPO's) when named, else the current checkout's.
With no app installed on it, or GH_TOKEN or GITHUB_TOKEN set, gh runs on the
user's token as it would alone. Standard streams pass through, and ccx exits
with gh's code.`,
		Example: "  ccx vcs gh -- pr checks 123\n  ccx vcs gh -- run watch 456 --exit-status",
		Args: func(cmd *cobra.Command, args []string) error {
			if cmd.ArgsLenAtDash() != 0 || len(args) == 0 {
				return errors.New("name the gh command after --: ccx vcs gh -- pr checks 123")
			}
			return nil
		},
		RunE: runVcsGh,
	}
}

func runVcsGh(cmd *cobra.Command, argv []string) error {
	ctx := cmd.Context()
	dir := workingDir(ctx)
	repo := ghRepoFlag(argv)
	if repo == "" {
		repo = ghRepoName(render.Getenv(ctx, "GH_REPO"))
	}
	env, _, err := ghRepoReadEnv(ctx, render.Dir(dir), repo, ghapi.AppWatchMargin)
	if err != nil {
		return err
	}
	gh := exec.CommandContext(ctx, "gh", argv...) //nolint:gosec // argv is the caller's own gh command
	gh.Dir = dir
	gh.Env = slices.Concat(os.Environ(), render.EnvFrom(ctx), env)
	gh.Stdin, gh.Stdout, gh.Stderr = cmd.InOrStdin(), cmd.OutOrStdout(), cmd.ErrOrStderr()
	err = gh.Run()
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return &ExitError{Code: exitErr.ExitCode()}
	}
	return err
}

// ghRepoFlag returns the repository gh's --repo or -R names in argv, "" when
// none does.
func ghRepoFlag(argv []string) string {
	for i, arg := range argv {
		switch {
		case arg == "--":
			return ""
		case arg == "--repo" || arg == "-R":
			if i+1 < len(argv) {
				return ghRepoName(argv[i+1])
			}
		case strings.HasPrefix(arg, "--repo="):
			return ghRepoName(strings.TrimPrefix(arg, "--repo="))
		case strings.HasPrefix(arg, "-R"):
			return ghRepoName(strings.TrimPrefix(arg[2:], "="))
		}
	}
	return ""
}

// ghRepoName reduces gh's [HOST/]OWNER/REPO to OWNER/REPO.
func ghRepoName(value string) string {
	parts := strings.Split(value, "/")
	if len(parts) < 3 {
		return value
	}
	return strings.Join(parts[len(parts)-2:], "/")
}
