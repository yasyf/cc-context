package cli

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/yasyf/cc-context/internal/ghapi"
	"github.com/yasyf/cc-context/internal/render"
	"github.com/yasyf/cc-context/internal/vcs"
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
The repository is the one --repo, a gh api endpoint under repos/OWNER/REPO,
or GH_REPO names, else the current checkout's. With no repository to name, as
outside a checkout, no app installed on it, or GH_TOKEN or GITHUB_TOKEN set,
gh runs on the user's token as it would alone. On the app token @me names the
gh user, not the app: an @me qualifier or flag value becomes the login gh is
signed in as. A gh api endpoint outside the installation, anything but
repos/... and rate_limit, runs on the user's token, since the app's token
gets HTTP 403 from endpoints such as user and orgs/ORG/installations.
Standard streams pass through, and ccx exits with gh's code. A command that fails once its token has
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
	repo := ghTargetRepo(ctx, argv)
	userToken := ghReadsOutsideInstallation(argv)
	args := argv
	for {
		var env []string
		var expires time.Time
		var err error
		if !userToken {
			env, expires, err = ghRepoReadEnv(ctx, render.Dir(dir), repo, ghapi.AppWatchMargin)
		}
		if err != nil && !errors.Is(err, vcs.ErrNoGitHub) {
			return err
		}
		if len(env) > 0 && slices.ContainsFunc(argv, ghMe.MatchString) {
			if args, err = ghResolveMe(ctx, render.Dir(dir), argv); err != nil {
				return err
			}
		}
		gh := exec.CommandContext(ctx, "gh", args...) //nolint:gosec // args is the caller's own gh command
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

var ghMe = regexp.MustCompile(`(^|[\s:=,])@me\b`)

func ghResolveMe(ctx context.Context, dir render.Dir, argv []string) ([]string, error) {
	out, err := render.RunCLI(ctx, dir, "gh", []string{"config", "get", "--host", "github.com", "user"})
	if err != nil {
		return nil, fmt.Errorf("vcs gh: read the gh user that @me names: %w", err)
	}
	login := strings.TrimSpace(out)
	if login == "" {
		return nil, errors.New("vcs gh: gh names no signed-in user for @me; run gh auth login")
	}
	resolved := make([]string, len(argv))
	for i, arg := range argv {
		resolved[i] = ghMe.ReplaceAllString(arg, "${1}"+login)
	}
	return resolved, nil
}

// ghReadsOutsideInstallation reports whether argv is a gh api read of an
// endpoint the app's installation token does not reach: anything but a
// repository's repos/... endpoint or the installation's own rate_limit.
func ghReadsOutsideInstallation(argv []string) bool {
	if argv[0] != "api" {
		return false
	}
	_, endpoint := ghAPIRequest(argv[1:])
	u, err := url.Parse(endpoint)
	if err != nil {
		return false
	}
	path := strings.TrimPrefix(u.Path, "/")
	return path != "rate_limit" && !strings.HasPrefix(path, "repos/")
}

// ghTargetRepo returns the repository argv's gh command reads: the one --repo
// or -R names, else a gh api endpoint's repos/OWNER/REPO, else GH_REPO's, ""
// when none names one.
func ghTargetRepo(ctx context.Context, argv []string) string {
	repo := ghRepoFlag(argv)
	if repo == "" && argv[0] == "api" {
		_, endpoint := ghAPIRequest(argv[1:])
		repo = ghEndpointRepo(endpoint)
	}
	if repo == "" {
		repo = ghRepoName(render.Getenv(ctx, "GH_REPO"))
	}
	return repo
}

// ghEndpointRepo returns OWNER/REPO for a gh api endpoint under
// repos/OWNER/REPO, "" for any other endpoint or one whose OWNER or REPO is a
// placeholder gh fills from the checkout.
func ghEndpointRepo(endpoint string) string {
	u, err := url.Parse(endpoint)
	if err != nil {
		return ""
	}
	parts := strings.SplitN(strings.TrimPrefix(u.Path, "/"), "/", 4)
	if len(parts) < 3 || parts[0] != "repos" || parts[1] == "" || parts[2] == "" || strings.ContainsAny(parts[1]+parts[2], "{}") {
		return ""
	}
	return parts[1] + "/" + parts[2]
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

// ghAPIReads reports whether gh api's args make a GET outside graphql.
func ghAPIReads(args []string) bool {
	method, endpoint := ghAPIRequest(args)
	return endpoint != "graphql" && method == "GET"
}

// ghAPIRequest returns the method and endpoint gh api's args name: gh sends a
// POST once a field or --input is present unless -X names the method.
func ghAPIRequest(args []string) (method, endpoint string) {
	sends := false
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
	switch {
	case method != "":
	case sends:
		method = "POST"
	default:
		method = "GET"
	}
	return method, endpoint
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
