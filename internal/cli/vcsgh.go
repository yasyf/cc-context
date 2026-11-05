package cli

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/yasyf/cc-context/internal/ghapi"
	"github.com/yasyf/cc-context/internal/lookpath"
	"github.com/yasyf/cc-context/internal/render"
	"github.com/yasyf/cc-context/internal/vcs"
)

func newVcsGhCmd() *cobra.Command {
	var maxAge time.Duration
	cmd := &cobra.Command{
		Use:   "gh [--max-age DUR] -- <gh args>...",
		Short: "Run gh through ccx's GitHub gateway: the App's quota for reads, a shared cache, and a floor",
		Long: `Run gh through ccx's GitHub gateway, the one path every GitHub read on the
machine should take.

A read runs with GH_TOKEN set to the read-only GitHub App installation token
ccx's own reads use, so a poll such as gh pr checks, gh pr view, gh run watch,
or a GraphQL query draws on the installation's quota instead of the gh user's.
A read is a gh api GET outside graphql, a gh api graphql query that is not a
mutation (-f query=..., -F query=@file, or --input with a {"query", "variables"}
body, - for stdin), a gh search, or a command whose verb is view, list, status,
checks, diff, or watch. The repository is the one --repo, a gh api endpoint
under repos/OWNER/REPO, or GH_REPO names, else the current checkout's; set
GH_REPO for a GraphQL query. With no repository to name, no app installed on
it, or GH_TOKEN or GITHUB_TOKEN set, the read runs on the user's token as gh
would alone. On the app token @me names the gh user, not the app. A gh api
endpoint outside the installation, such as user or orgs/ORG/installations,
runs on the user's token, and so does a GraphQL query the app is forbidden.

Every request is charged to its pool (app or user, and core, graphql, or
search) in a machine-wide ledger, under the caller CCX_CALLER names, else
CCI_AGENT_ID, else the script or command that started ccx; set CCX_CALLER in
every script and daemon. ccx vcs auth status prints each pool's remainder,
reset, and top callers. Before a read, ccx reads its pool's standing from
GitHub's free rate_limit when the ledger's is over a minute old. A pool under
1000 left before its reset takes no more reads: ccx serves the cached answer
to the same read, however old, saying so on stderr; else a read on the app's
pool runs on the user's pool instead while that pool has over half its limit
left; else ccx exits 1 without calling GitHub
and prints one line starting "ccx: GitHub rate limit floor:" naming the pool,
its remainder and limit, its reset time, and the top callers. Back off until
that reset.

--max-age serves an identical read (same repository, arguments, and stdin)
from the machine-wide cache while its stored answer is younger than DUR. Only
a read that exited 0 is stored, nothing expires a stored answer but a later
read, a later write to its repository, or an hour's age, and a watch is never
cached. GH_TOKEN, GITHUB_TOKEN, or GH_HOST keeps a read out of the cache. A read is cached only when its answer cannot depend on the checkout: it
names its repository, or it is a gh api call with no {owner}, {repo}, or {branch}
placeholder. --web, --log, --log-failed, gh api rate_limit, and an answer over
1 MiB are never cached.

A command that reads as a person rather than a repository runs on the user's
token: gh's status commands, gh repo list, gh search with no --repo, anything
naming a viewer field, and any command outside pr, issue, run, workflow, release,
label, cache, ruleset, repo view, and search. So does gh pr, issue, or repo view
with no --json, since the app's token reads a requested team reviewer's name as
empty. So does a read with its stdout on
a terminal, unless it watches. A read the app token is refused runs again on the
user's. gh auth, config, alias, extension, completion, help, and --help send no
request ccx charges. A child ccx spawns carries CCX_CHILD=1, so a shim can pass
it straight to the real gh.

Anything else, such as gh pr close or a gh api write, runs on the user's
token as plain gh would, charged to its pool but never refused or cached. It
expires the cached reads of the repository it names, else of the checkout's,
and of every repository when it is a GraphQL mutation naming none. Any write
expires a cached read that names no one repository. A
merge (gh pr merge, a write to repos/OWNER/REPO/pulls/N/merge or /merges, or a
GraphQL mergePullRequest, enablePullRequestAutoMerge, enqueuePullRequest, or
mergeBranch), and a write to a ruleset or
branch protection, is refused before it runs: those stay on plain gh, where
the merge hooks gate them. CCX_GH_RUN_GUARDED=1 runs them instead, for a local
shim that puts this gateway behind the name gh, so the hooks still read
gh pr merge on the command line. CCX_GH names the real gh for such a machine.
Every argument from the gh command on passes to gh untouched, so a gh flag
such as --allow-escape-sequences needs no -- before it.
Standard streams pass through, and ccx exits with gh's code. A read that fails
once its token has expired, such as a watch that outlived it, runs again on a
fresh one.`,
		Example: "  ccx vcs gh -- pr checks 123\n  ccx vcs gh --max-age 5m -- pr view 123 --json state\n" +
			"  GH_REPO=o/r ccx vcs gh --max-age 1m -- api graphql -f query='{ repository(owner: \"o\", name: \"r\") { name } }'\n" +
			"  ccx vcs gh -- run watch 456 --exit-status",
		Args: func(_ *cobra.Command, args []string) error {
			if len(args) == 0 {
				return errors.New("name the gh command: ccx vcs gh -- pr checks 123")
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error { return runVcsGh(cmd, args, maxAge) },
	}
	cmd.Flags().DurationVar(&maxAge, "max-age", 0, "serve an identical read answered within this long from the machine-wide cache")
	cmd.Flags().SetInterspersed(false)
	return cmd
}

const ghFloorPrefix = "ccx: GitHub rate limit floor: "

func runVcsGh(cmd *cobra.Command, argv []string, maxAge time.Duration) error {
	ctx := cmd.Context()
	stdin, err := ghStdin(cmd, argv)
	if err != nil {
		return err
	}
	reads := ghReads(argv, stdin)
	if !reads && ghGuarded(argv, stdin) && os.Getenv(ghRunGuardedEnv) == "" {
		return fmt.Errorf("vcs gh: merge with gh pr merge; the merge hooks gate that form: %s", ghCommand(argv))
	}
	dir := workingDir(ctx)
	if ghNoRequest(argv) {
		return ghExit(ghRun(ctx, cmd, dir, argv, nil, stdin, cmd.OutOrStdout(), cmd.ErrOrStderr()))
	}
	resource := ghResource(argv)
	if !reads {
		ghNoteLedgerError(cmd, reviewsAPI().Charge(ctx, ghapi.IdentityUser, resource, ghChargeTarget(argv)))
		err := ghRun(ctx, cmd, dir, argv, nil, stdin, cmd.OutOrStdout(), cmd.ErrOrStderr())
		ghInvalidate(ghWrittenRepo(ctx, argv, func() string { return ghCheckoutRepo(ctx, dir) }))
		return ghExit(err)
	}
	scope := cmp.Or(ghTargetRepo(ctx, argv), ghGraphQLRepo(argv))
	watches := ghWatches(argv)
	interactive := !watches && ghTerminal(cmd.OutOrStdout())
	userToken := interactive || ghReadsOutsideInstallation(argv) || ghQueryReadsViewer(argv, stdin)
	cacheable := maxAge > 0 && !interactive && !ghOtherIdentity(ctx) && ghCacheable(argv, scope)
	key := ghCacheKey(cmp.Or(scope, dir), argv, stdin)
	if cacheable {
		if hit, ok := ghCached(key); ok && time.Since(hit.At) < maxAge && hit.At.After(ghInvalidatedAt(scope)) {
			_, err := cmd.OutOrStdout().Write(hit.Stdout)
			return err
		}
	}
	capture := cacheable || resource == ghapi.ResourceGraphQL && argv[0] == "api"
	live := watches || interactive
	args := argv
	spilled := false
	for {
		var env []string
		var expires time.Time
		var err error
		if !userToken {
			env, expires, err = ghRepoReadEnv(ctx, render.Dir(dir), scope, ghapi.AppWatchMargin)
		}
		if err != nil && !errors.Is(err, vcs.ErrNoGitHub) {
			ghNoteLedgerError(cmd, err)
		}
		if len(env) > 0 && slices.ContainsFunc(argv, ghMe.MatchString) {
			if args, err = ghResolveMe(ctx, render.Dir(dir), argv); err != nil {
				return err
			}
		}
		api, identity := reviewsAPI().AsUser(), ghapi.IdentityUser
		if len(env) > 0 {
			api, identity = reviewsAPI().ForRepo(cmp.Or(scope, ghCheckoutRepo(ctx, dir))), ghapi.IdentityApp
		}
		admit := api.Admit(ctx, identity, resource)
		if spilled && admit == nil {
			admit = api.AdmitSpill(ctx, resource)
		}
		if err := admit; err != nil {
			if hit, ok := ghCached(key); ok && hit.At.After(ghInvalidatedAt(scope)) && errors.As(err, new(*ghapi.FloorError)) {
				_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "ccx: serving the answer cached at %s; %v\n", hit.At.Local().Format(time.Kitchen), err)
				_, werr := cmd.OutOrStdout().Write(hit.Stdout)
				return werr
			}
			if identity == ghapi.IdentityApp && errors.As(err, new(*ghapi.FloorError)) {
				userToken, spilled = true, true
				continue
			}
			if errors.As(err, new(*ghapi.FloorError)) {
				return ghFloor(cmd, err)
			}
			ghNoteLedgerError(cmd, err)
		}
		ghNoteLedgerError(cmd, api.Charge(ctx, identity, resource, ghChargeTarget(argv)))
		started := time.Now()
		if !capture {
			var stderr bytes.Buffer
			sink := io.Writer(&stderr)
			if live {
				sink = io.MultiWriter(cmd.ErrOrStderr(), &stderr)
			}
			err = ghRun(ctx, cmd, dir, args, env, stdin, cmd.OutOrStdout(), sink)
			if err != nil && identity == ghapi.IdentityApp && ghAppRefused(stderr.Bytes()) {
				userToken = true
				continue
			}
			if !live {
				_, _ = cmd.ErrOrStderr().Write(stderr.Bytes())
			}
		} else {
			var stdout, stderr bytes.Buffer
			var tries int
			tries, err = retryTransient(ctx, func() error {
				stdout.Reset()
				stderr.Reset()
				err := ghRun(ctx, cmd, dir, args, env, stdin, &stdout, &stderr)
				if err == nil || stderr.Len() == 0 {
					return err
				}
				return fmt.Errorf("%w: %s", err, stderr.Bytes())
			})
			if err != nil && tries > 1 {
				_, _ = fmt.Fprintf(&stderr, "ccx: gh failed after %d tries\n", tries)
			}
			if err != nil && identity == ghapi.IdentityApp && (ghForbidden(stdout.Bytes()) || ghAppRefused(stderr.Bytes())) {
				userToken = true
				continue
			}
			if err == nil && cacheable && stdout.Len() <= ghCacheMaxAnswer {
				ghNoteLedgerError(cmd, ghStore(key, stdout.Bytes(), started))
			}
			_, _ = cmd.ErrOrStderr().Write(stderr.Bytes())
			if _, werr := cmd.OutOrStdout().Write(stdout.Bytes()); werr != nil {
				return werr
			}
		}
		if err == nil || expires.IsZero() || time.Now().Before(expires) || ctx.Err() != nil {
			return ghExit(err)
		}
	}
}

const ghRunGuardedEnv = "CCX_GH_RUN_GUARDED"

func ghRun(ctx context.Context, cmd *cobra.Command, dir string, args, env []string, stdin []byte, stdout, stderr io.Writer) error {
	childEnv := slices.Concat(os.Environ(), render.EnvFrom(ctx), env, []string{render.ChildEnv})
	gh := exec.CommandContext(ctx, lookpath.For(childEnv).Bin("gh"), args...) //nolint:gosec // args is the caller's own gh command
	gh.Dir = dir
	gh.Env = childEnv
	gh.Stdin, gh.Stdout, gh.Stderr = cmd.InOrStdin(), stdout, stderr
	if stdin != nil {
		gh.Stdin = bytes.NewReader(stdin)
	}
	return gh.Run()
}

func ghExit(err error) error {
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return &ExitError{Code: exitErr.ExitCode()}
	}
	return err
}

func ghFloor(cmd *cobra.Command, err error) error {
	var floor *ghapi.FloorError
	if !errors.As(err, &floor) {
		return err
	}
	_, _ = fmt.Fprintln(cmd.ErrOrStderr(), ghFloorPrefix+floor.Error())
	return &ExitError{Code: 1}
}

// ghNoteLedgerError says on stderr that the quota ledger failed: a broken ledger
// costs a count, never the gh command it was counting.
func ghNoteLedgerError(cmd *cobra.Command, err error) {
	if err != nil {
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "ccx: %v\n", err)
	}
}

func ghTerminal(w io.Writer) bool {
	file, ok := w.(*os.File)
	if !ok {
		return false
	}
	info, err := file.Stat()
	if err != nil || info.Mode()&os.ModeCharDevice == 0 {
		return false
	}
	null, err := os.Stat(os.DevNull)
	return err != nil || !os.SameFile(info, null)
}

// ghOtherIdentity reports whether this read may not run as the signed-in gh
// user, so its answer must not reach, or come from, the shared cache.
func ghOtherIdentity(ctx context.Context) bool {
	return render.Getenv(ctx, "GH_TOKEN") != "" || render.Getenv(ctx, "GITHUB_TOKEN") != "" || render.Getenv(ctx, "GH_HOST") != ""
}

func ghQueryReadsViewer(argv []string, stdin []byte) bool {
	if argv[0] != "api" {
		return false
	}
	query, ok := ghGraphQLQuery(argv[1:], stdin)
	return ok && strings.Contains(strings.ToLower(query), "viewer")
}

var ghAppRefusal = regexp.MustCompile(`(?i)not accessible by integration|could not resolve to a (user|repository)|HTTP 403`)

func ghAppRefused(stderr []byte) bool {
	return ghAppRefusal.Match(stderr) && !bytes.Contains(bytes.ToLower(stderr), []byte("rate limit"))
}

func ghCheckoutRepo(ctx context.Context, dir string) string {
	repo, err := vcs.LookupRepo(ctx, render.Dir(dir), false)
	if err != nil {
		return ""
	}
	return repo.NameWithOwner
}

func ghForbidden(stdout []byte) bool {
	var body struct {
		Errors []ghapi.GraphQLMessage `json:"errors"`
	}
	return json.Unmarshal(stdout, &body) == nil && slices.ContainsFunc(body.Errors, func(m ghapi.GraphQLMessage) bool { return m.Type == "FORBIDDEN" })
}

var (
	ghGuardedEndpoint = regexp.MustCompile(`(^|/)repos/[^/]+/[^/]+/(pulls/[^/]+/merge|merges|rulesets|branches/.+/protection)(/|$|\?)|(^|/)orgs/[^/]+/rulesets`)
	ghGuardedMutation = regexp.MustCompile(`(?i)\b(mergePullRequest|enablePullRequestAutoMerge|enqueuePullRequest|mergeBranch|\w*Ruleset|\w*BranchProtectionRule)\b`)
)

func ghGuarded(argv []string, stdin []byte) bool {
	switch {
	case len(argv) > 1 && argv[0] == "pr" && argv[1] == "merge":
		return true
	case argv[0] != "api":
		return false
	}
	_, endpoint := ghAPIRequest(argv[1:])
	if ghapi.Resource(endpoint) != ghapi.ResourceGraphQL {
		return ghGuardedEndpoint.MatchString(endpoint)
	}
	query, ok := ghGraphQLQuery(argv[1:], stdin)
	return !ok || ghGuardedMutation.MatchString(query)
}

func ghCacheable(argv []string, scope string) bool {
	if ghWatches(argv) || ghReadsFiles(argv) || slices.ContainsFunc(argv, ghUncachedFlag) || slices.ContainsFunc(argv, ghCheckoutPlaceholder) {
		return false
	}
	if argv[0] != "api" {
		return scope != ""
	}
	_, endpoint := ghAPIRequest(argv[1:])
	return strings.TrimPrefix(endpoint, "/") != "rate_limit"
}

func ghUncachedFlag(arg string) bool {
	return arg == "--web" || arg == "-w" || arg == "--log" || arg == "--log-failed"
}

func ghCheckoutPlaceholder(arg string) bool {
	return strings.Contains(arg, "{owner}") || strings.Contains(arg, "{repo}") || strings.Contains(arg, "{branch}")
}

func ghReadsFiles(argv []string) bool {
	for i, arg := range argv {
		switch {
		case arg == "--input" && i+1 < len(argv) && argv[i+1] != "-":
			return true
		case strings.HasPrefix(arg, "--input=") && arg != "--input=-":
			return true
		case strings.Contains(arg, "=@") && !strings.HasSuffix(arg, "=@-"):
			return true
		}
	}
	return false
}

func ghWatches(argv []string) bool {
	return len(argv) > 1 && argv[1] == "watch" || slices.Contains(argv, "--watch")
}

var ghCoreCommands = map[string]bool{"run": true, "workflow": true, "release": true, "cache": true, "secret": true, "variable": true, "gist": true, "ruleset": true, "attestation": true}

func ghResource(argv []string) string {
	switch {
	case argv[0] == "api":
		_, endpoint := ghAPIRequest(argv[1:])
		return ghapi.Resource(endpoint)
	case argv[0] == "search":
		return ghapi.ResourceSearch
	case ghCoreCommands[argv[0]]:
		return ghapi.ResourceCore
	default:
		return ghapi.ResourceGraphQL
	}
}

func chargeGHChild(ctx context.Context, bin string, argv, env []string) error {
	resource, ok := ghUserPool(bin, argv, env)
	if !ok {
		return nil
	}
	return reviewsAPI().Charge(ctx, ghapi.IdentityUser, resource, ghChargeTarget(argv))
}

func ghChargeTarget(argv []string) string {
	if argv[0] == "api" {
		_, endpoint := ghAPIRequest(argv[1:])
		return endpoint
	}
	return "gh " + strings.Join(argv[:min(2, len(argv))], " ")
}

// ghExpireForChild expires the cached reads a finished child's gh write or git
// push changed.
func ghExpireForChild(ctx context.Context, bin string, argv []string) {
	checkout := func() string { return vcs.CachedRepoName(ctx, render.Dir(workingDir(ctx))) }
	switch {
	case bin == "gh" && len(argv) > 0 && !ghNoRequest(argv) && !ghReads(argv, nil):
		ghInvalidate(ghWrittenRepo(ctx, argv, checkout))
	case bin == "git" && len(argv) > 0 && argv[0] == "push":
		ghInvalidate(cmp.Or(checkout(), ghEveryRepo))
	}
}

func ghUserPool(bin string, argv, env []string) (string, bool) {
	appToken := slices.ContainsFunc(env, func(kv string) bool { return strings.HasPrefix(kv, "GH_TOKEN=") })
	if bin != "gh" || ghNoRequest(argv) || appToken {
		return "", false
	}
	return ghResource(argv), true
}

var ghOffline = map[string]bool{"auth": true, "config": true, "alias": true, "extension": true, "completion": true, "help": true, "version": true, "--version": true, "--help": true, "-h": true}

func ghNoRequest(argv []string) bool {
	return ghOffline[argv[0]] || slices.Contains(argv, "--help")
}

func ghStdin(cmd *cobra.Command, argv []string) ([]byte, error) {
	if argv[0] != "api" || !slices.ContainsFunc(argv, func(arg string) bool { return arg == "-" || arg == "--input=-" || strings.HasSuffix(arg, "=@-") }) {
		return nil, nil
	}
	body, err := io.ReadAll(cmd.InOrStdin())
	if err != nil {
		return nil, fmt.Errorf("vcs gh: read stdin: %w", err)
	}
	return body, nil
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

// ghNamesViewerField reports whether arg asks for a gh field the app's token
// answers differently: each comment's viewerDidAuthor, each reaction's
// viewerHasReacted, and a requested team reviewer's name, which it reads as empty.
func ghNamesViewerField(arg string) bool {
	lower := strings.ToLower(arg)
	return strings.Contains(lower, "comments") || strings.Contains(lower, "reactiongroups") || strings.Contains(lower, "reviewrequests")
}

// ghHumanView reports whether argv is gh pr, issue, or repo view's printed form,
// which asks for those fields without naming them. --json names its own.
func ghHumanView(argv []string) bool {
	if argv[1] != "view" || argv[0] != "pr" && argv[0] != "issue" && argv[0] != "repo" {
		return false
	}
	return !slices.ContainsFunc(argv, func(arg string) bool { return arg == "--json" || strings.HasPrefix(arg, "--json=") })
}

// ghAppCommands are the gh commands whose list, view, checks, diff, and watch
// answer the same on the app's installation token as on the user's.
var ghAppCommands = map[string]bool{"pr": true, "issue": true, "run": true, "workflow": true, "release": true, "label": true, "cache": true, "ruleset": true}

// ghReadsOutsideInstallation reports whether argv is a read the app's
// installation token cannot answer, or would answer for itself rather than the
// user: a viewer field, gh's status commands, a command outside ghAppCommands,
// gh repo list, a gh search with no --repo, or a gh api endpoint other than a
// repository's repos/..., graphql, or the installation's own rate_limit.
func ghReadsOutsideInstallation(argv []string) bool {
	if slices.ContainsFunc(argv, func(arg string) bool { return strings.Contains(strings.ToLower(arg), "viewer") }) {
		return true
	}
	if argv[0] != "api" {
		switch argv[0] {
		case "repo":
			return argv[1] != "view" || ghHumanView(argv)
		case "search":
			return ghRepoFlag(argv) == ""
		default:
			return !ghAppCommands[argv[0]] || argv[1] == "status" || ghHumanView(argv) || slices.ContainsFunc(argv, ghNamesViewerField)
		}
	}
	_, endpoint := ghAPIRequest(argv[1:])
	u, err := url.Parse(endpoint)
	if err != nil {
		return false
	}
	path := strings.TrimPrefix(u.Path, "/")
	return path != "rate_limit" && path != "graphql" && !strings.HasPrefix(path, "repos/")
}

// ghTargetRepo returns the repository argv's gh command reads: the one --repo
// or -R names, else a gh api endpoint's repos/OWNER/REPO, else a github.com URL's,
// else GH_REPO's, "" when none names one.
func ghTargetRepo(ctx context.Context, argv []string) string {
	repo := ghRepoFlag(argv)
	if repo == "" && argv[0] == "api" {
		_, endpoint := ghAPIRequest(argv[1:])
		repo = ghEndpointRepo(endpoint)
	}
	if repo == "" && argv[0] != "api" {
		repo = ghURLRepo(argv)
	}
	if repo == "" {
		repo = ghRepoName(render.Getenv(ctx, "GH_REPO"))
	}
	return repo
}

var ghGitHubURL = regexp.MustCompile(`^https://github\.com/([^/]+)/([^/#?]+)`)

// ghURLRepo returns the OWNER/REPO of the github.com URL a gh command names as a
// target, "" when it names none. A URL that follows a flag is that flag's value.
func ghURLRepo(argv []string) string {
	for i := 1; i < len(argv); i++ {
		if m := ghGitHubURL.FindStringSubmatch(argv[i]); m != nil && !strings.HasPrefix(argv[i-1], "-") {
			return m[1] + "/" + m[2]
		}
	}
	return ""
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

// ghReads reports whether argv, given stdin when it reads stdin, is a gh
// command the read-only app token serves.
func ghReads(argv []string, stdin []byte) bool {
	switch {
	case len(argv) == 0:
		return false
	case argv[0] == "api":
		return ghAPIReads(argv[1:], stdin)
	case argv[0] == "search":
		return true
	default:
		return len(argv) > 1 && !strings.HasPrefix(argv[0], "-") && ghReadVerbs[argv[1]]
	}
}

// ghAPIReads reports whether gh api's args make a GET outside graphql or a
// GraphQL query that is not a mutation.
func ghAPIReads(args []string, stdin []byte) bool {
	method, endpoint := ghAPIRequest(args)
	if ghapi.Resource(endpoint) != ghapi.ResourceGraphQL {
		return method == "GET"
	}
	query, ok := ghGraphQLQuery(args, stdin)
	return ok && !ghMutation.MatchString(ghGraphQLComment.ReplaceAllString(query, ""))
}

var (
	ghGraphQLComment = regexp.MustCompile(`#[^\n]*`)
	ghMutation       = regexp.MustCompile(`\bmutation\b`)
)

// ghGraphQLQuery returns the GraphQL document gh api's args send: a query
// field, raw or typed and read from a file or stdin with @, or an --input
// body's query.
func ghGraphQLQuery(args []string, stdin []byte) (string, bool) {
	read := func(source string) ([]byte, bool) {
		if source == "-" {
			return stdin, stdin != nil
		}
		body, err := os.ReadFile(source) //nolint:gosec // the caller's own gh argument
		return body, err == nil
	}
	for i := 0; i < len(args) && args[i] != "--"; i++ {
		flag, value, inline := ghSplitFlag(args[i])
		if !inline && (ghAPIFieldFlags[flag] || ghAPIValueFlags[flag] || flag == "-X" || flag == "--method") && i+1 < len(args) {
			i++
			value = args[i]
		}
		switch flag {
		case "-f", "--raw-field":
			if query, ok := strings.CutPrefix(value, "query="); ok {
				return query, true
			}
		case "-F", "--field":
			query, ok := strings.CutPrefix(value, "query=")
			if !ok {
				continue
			}
			source, file := strings.CutPrefix(query, "@")
			if !file {
				return query, true
			}
			body, ok := read(source)
			return string(body), ok
		case "--input":
			body, ok := read(value)
			var request struct {
				Query string `json:"query"`
			}
			if !ok || json.Unmarshal(body, &request) != nil {
				return "", false
			}
			return request.Query, true
		}
	}
	return "", false
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
