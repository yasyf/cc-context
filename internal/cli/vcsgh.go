package cli

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strings"
	"time"

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

The token is read-only, so vcs gh runs only reads: a gh api GET outside
graphql (no field or --input, or -X GET), a gh search, or a command whose
verb is view, list, status, checks, diff, or watch. It refuses anything else
before running it and names the plain gh command to run on the user's token.
The repository is --repo's (or GH_REPO's) when named, else the current
checkout's. With no app installed on it, or GH_TOKEN or GITHUB_TOKEN set,
gh runs on the user's token as it would alone. Standard streams pass through,
and ccx exits with gh's code. A command that fails once its token has
expired, such as a watch that outlived it, runs again on a fresh one.`,
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
	if !ghReads(argv) {
		return fmt.Errorf("vcs gh runs only reads on a read-only GitHub App token; run this as plain gh on your own token: %s", ghCommand(argv))
	}
	ctx := cmd.Context()
	dir := workingDir(ctx)
	repo := ghRepoFlag(argv)
	if repo == "" {
		repo = ghRepoName(render.Getenv(ctx, "GH_REPO"))
	}
	for {
		env, expires, err := ghRepoReadEnv(ctx, render.Dir(dir), repo, ghapi.AppWatchMargin)
		if err != nil {
			return err
		}
		gh := exec.CommandContext(ctx, "gh", argv...) //nolint:gosec // argv is the caller's own gh command
		gh.Dir = dir
		gh.Env = slices.Concat(os.Environ(), render.EnvFrom(ctx), env)
		gh.Stdin, gh.Stdout, gh.Stderr = cmd.InOrStdin(), cmd.OutOrStdout(), cmd.ErrOrStderr()
		err = gh.Run()
		if err == nil || expires.IsZero() || time.Now().Before(expires) || ctx.Err() != nil {
			var exitErr *exec.ExitError
			if errors.As(err, &exitErr) {
				return &ExitError{Code: exitErr.ExitCode()}
			}
			return err
		}
	}
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

var (
	ghReadVerbs     = map[string]bool{"view": true, "list": true, "status": true, "checks": true, "diff": true, "watch": true}
	ghAPIFieldFlags = map[string]bool{"-f": true, "--raw-field": true, "-F": true, "--field": true, "--input": true}
	ghAPIValueFlags = map[string]bool{"-H": true, "--header": true, "-q": true, "--jq": true, "-t": true, "--template": true, "--hostname": true, "--cache": true, "-p": true, "--preview": true}
)

// ghReads reports whether argv is a gh command the read-only app token serves.
func ghReads(argv []string) bool {
	switch {
	case len(argv) == 0:
		return false
	case argv[0] == "api":
		return ghAPIReads(argv[1:])
	case argv[0] == "search":
		return true
	default:
		return len(argv) > 1 && !strings.HasPrefix(argv[0], "-") && ghReadVerbs[argv[1]]
	}
}

// ghAPIReads reports whether gh api's args make a GET outside graphql: gh
// sends a POST once a field or --input is present unless -X names the method.
func ghAPIReads(args []string) bool {
	method, sends, endpoint := "", false, ""
	for i := 0; i < len(args); i++ {
		if args[i] == "--" {
			if endpoint == "" && i+1 < len(args) {
				endpoint = args[i+1]
			}
			break
		}
		flag, value, inline := ghSplitFlag(args[i])
		if !inline && (flag == "-X" || flag == "--method" || ghAPIFieldFlags[flag] || ghAPIValueFlags[flag]) && i+1 < len(args) {
			i++
			value = args[i]
		}
		switch {
		case flag == "-X" || flag == "--method":
			method = strings.ToUpper(value)
		case ghAPIFieldFlags[flag]:
			sends = true
		case endpoint == "" && !strings.HasPrefix(flag, "-"):
			endpoint = flag
		}
	}
	if method == "" && sends {
		method = "POST"
	}
	return endpoint != "graphql" && (method == "" || method == "GET")
}

// ghSplitFlag splits a --name=value or glued -Xvalue argument into its flag
// and value, reporting whether the value came inline.
func ghSplitFlag(arg string) (flag, value string, inline bool) {
	switch {
	case strings.HasPrefix(arg, "--"):
		name, value, found := strings.Cut(arg, "=")
		return name, value, found
	case strings.HasPrefix(arg, "-") && len(arg) > 2:
		return arg[:2], strings.TrimPrefix(arg[2:], "="), true
	default:
		return arg, "", false
	}
}
