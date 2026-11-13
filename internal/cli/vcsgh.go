package cli

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
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
GH_REPO for a GraphQL query. With no repository to name or no app installed on
it, the read runs on the user's token as gh would alone. With GH_TOKEN or
GITHUB_TOKEN set, it runs on that token, charged to the pool the token spends:
an installation's when ccx minted it for one, else the user's.
CCX_GITHUB_APP_TOKEN_FILE, which names a token ccx vcs auth token --json
printed on another machine, reads through that token instead of minting one,
and GH_TOKEN or GITHUB_TOKEN then pins nothing. On the app token @me names the
gh user, not the app. A gh api endpoint outside the installation, such as user
or orgs/ORG/installations, runs on the user's token, and so does a GraphQL
query the app is forbidden.

Every request gh sends is charged, once gh exits, to the pool its URL draws
from (app or user, and core, graphql, or search) in a machine-wide ledger,
under the caller CCX_CALLER names, else CCI_AGENT_ID, else the script or
command that started ccx; set CCX_CALLER in every script and daemon. ccx reads
what gh sent from the request lines GH_DEBUG=1 has it print, which never reach
stderr, so gh api --paginate is charged a request a page, and a command that
sends none, such as gh api 'repos/{owner}/{repo}' outside a checkout, nothing.
ccx vcs auth status prints each pool's remainder, reset, and top callers.
Before a read, ccx reads its pool's standing from GitHub's free rate_limit
when the ledger's is over a minute old. A pool under
1000 left in any open window takes no more reads: ccx serves the cached answer
to the same read, however old, saying so on stderr; else a read on the app's
pool runs on the user's pool instead while every window of that pool has over
half its limit left; else ccx exits 1 without calling GitHub
and prints one line starting "ccx: GitHub rate limit floor:" naming the pool,
its remainder and limit, its reset time, and the top callers. Back off until
that reset.

--max-age serves an identical read (same repository, arguments, and stdin)
from the machine-wide cache while its stored answer is younger than DUR. Only
a read that exited 0 is stored, nothing expires a stored answer but a later
read, a later write to its repository, or an hour's age, and a watch is never
cached. GH_TOKEN or GITHUB_TOKEN while it pins the user's token, or GH_HOST, keeps a read out of the cache. A read is cached only when its answer cannot depend on the checkout: it
names its repository, or it is a gh api call with no {owner}, {repo}, or {branch}
placeholder. --web, --log, --log-failed, gh api rate_limit, and an answer over
1 MiB are never cached. Nor is a read of check, commit status, or workflow run
state: gh pr checks, gh pr status, gh run, gh workflow view, a commit's
check-runs, check-suites, or statuses, actions runs or jobs, or any argument or
stdin naming statusCheckRollup, checkRuns, checkSuites, or a GraphQL status
field. That state changes on a push or a finishing job, with no write here to
expire it, and gh pr checks exits 0 only when every check passed, so a cached
answer would be a green that outlives the head it graded.

A command that reads as a person rather than a repository runs on the user's
token: gh's status commands, gh repo list, gh search with no --repo, anything
naming a viewer field, and any command outside pr, issue, run, workflow, release,
label, cache, ruleset, repo view, and search. So does gh pr, issue, or repo view
with no --json, since the app's token reads a requested team reviewer's name as
empty. So does a read with its stdout on
a terminal, unless it watches. A read the app token is refused runs again on the
user's, and one GitHub refuses for rate there moves to the user's pool on the
same terms as a spent app pool. gh auth, config, alias, extension, completion,
help, and --help send no request ccx charges. A child ccx spawns carries
CCX_CHILD=1, so a shim can pass it straight to the real gh; ccx admits and
charges that child itself.

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
Every argument from the gh command on passes to gh, so a gh flag needs no --
before it. Off a terminal, gh api, pr diff, gist view, repo read-file, and
release download also get --allow-escape-sequences, and ccx strips every
terminal escape sequence from what they print, so gh never refuses a log,
file, diff, gist, or asset for carrying one and no saved read keeps one.
Output whose first 512 bytes read as binary to gh's own content sniff, once
their escape sequences are gone, passes verbatim. Standard streams otherwise
pass through, and ccx exits with gh's code. A read that fails once its token
has expired, such as a watch that outlived it, runs again on a fresh one.`,
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

func runVcsGh(cmd *cobra.Command, argv []string, maxAge time.Duration) (err error) {
	ctx := cmd.Context()
	stdin, err := ghStdin(cmd, argv)
	if err != nil {
		return err
	}
	reads := ghReads(argv, stdin)
	if !reads && ghGuarded(argv, stdin) && os.Getenv(ghRunGuardedEnv) == "" {
		return fmt.Errorf("vcs gh: merge with gh pr merge; the merge hooks gate that form: %s", ghCommand(argv))
	}
	out := cmd.OutOrStdout()
	if !ghTerminal(out) && ghGuardsEscapes(argv) {
		plain := &ghPlainWriter{w: out}
		defer func() { err = cmp.Or(err, plain.Close()) }()
		out, argv = plain, ghAllowEscapes(argv)
	}
	dir := workingDir(ctx)
	if ghNoRequest(argv) {
		return ghExit(ghChild(ctx, cmd, dir, argv, nil, stdin, out, cmd.ErrOrStderr()).Run())
	}
	resource := ghResource(argv)
	if !reads {
		sent, err := ghRun(ctx, cmd, dir, argv, nil, stdin, out, cmd.ErrOrStderr())
		ghNoteLedgerError(cmd, reviewsAPI().Sent(ctx, ghapi.IdentityUser, sent))
		ghInvalidate(ghWrittenRepo(ctx, argv, func() string { return ghCheckoutRepo(ctx, dir) }))
		return ghExit(err)
	}
	scope := cmp.Or(ghTargetRepo(ctx, argv), ghGraphQLRepo(argv))
	watches := ghWatches(argv)
	interactive := !watches && ghTerminal(out)
	userToken := interactive || ghReadsOutsideInstallation(argv) || ghQueryReadsViewer(argv, stdin)
	cacheable := maxAge > 0 && !interactive && !ghOtherIdentity(ctx) && ghCacheable(argv, stdin, scope)
	key := ghCacheKey(cmp.Or(scope, dir), argv, stdin)
	if cacheable {
		if hit, ok := ghCached(key); ok && time.Since(hit.At) < maxAge && hit.At.After(ghInvalidatedAt(scope)) {
			_, err := out.Write(hit.Stdout)
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
			repo := scope
			if repo == "" {
				repo = ghCheckoutRepo(ctx, dir)
			}
			api, identity = reviewsAPI().ForRepo(repo), ghapi.IdentityApp
		}
		admit := api.Admit(ctx, identity, resource)
		if spilled && admit == nil {
			admit = api.AdmitSpill(ctx, resource)
		}
		if err := admit; err != nil {
			if hit, ok := ghCached(key); ok && hit.At.After(ghInvalidatedAt(scope)) && errors.As(err, new(*ghapi.FloorError)) {
				_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "ccx: serving the answer cached at %s; %v\n", hit.At.Local().Format(time.Kitchen), err)
				_, werr := out.Write(hit.Stdout)
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
		started := time.Now()
		if !capture {
			var stderr bytes.Buffer
			sink := io.Writer(&stderr)
			if live {
				sink = io.MultiWriter(cmd.ErrOrStderr(), &stderr)
			}
			var sent []string
			sent, err = ghRun(ctx, cmd, dir, args, env, stdin, out, sink)
			ghNoteLedgerError(cmd, api.Sent(ctx, identity, sent))
			if err != nil && identity == ghapi.IdentityApp && ghAppRateLimited(nil, stderr.Bytes()) {
				userToken, spilled = true, true
				continue
			}
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
				sent, err := ghRun(ctx, cmd, dir, args, env, stdin, &stdout, &stderr)
				ghNoteLedgerError(cmd, api.Sent(ctx, identity, sent))
				if err == nil || stderr.Len() == 0 {
					return err
				}
				return fmt.Errorf("%w: %s", err, stderr.Bytes())
			})
			if err != nil && tries > 1 {
				_, _ = fmt.Fprintf(&stderr, "ccx: gh failed after %d tries\n", tries)
			}
			if err != nil && identity == ghapi.IdentityApp && ghAppRateLimited(stdout.Bytes(), stderr.Bytes()) {
				userToken, spilled = true, true
				continue
			}
			if err != nil && identity == ghapi.IdentityApp && (ghGraphQLRefused(stdout.Bytes(), "FORBIDDEN") || ghAppRefused(stderr.Bytes())) {
				userToken = true
				continue
			}
			if err == nil && cacheable && stdout.Len() <= ghCacheMaxAnswer {
				ghNoteLedgerError(cmd, ghStore(key, stdout.Bytes(), started))
			}
			_, _ = cmd.ErrOrStderr().Write(stderr.Bytes())
			if _, werr := out.Write(stdout.Bytes()); werr != nil {
				return werr
			}
		}
		if err == nil || expires.IsZero() || time.Now().Before(expires) || ctx.Err() != nil {
			return ghExit(err)
		}
	}
}

const ghRunGuardedEnv = "CCX_GH_RUN_GUARDED"

// ghRun runs gh under GH_DEBUG=1 and returns the URL of each request it sent.
func ghRun(ctx context.Context, cmd *cobra.Command, dir string, args, env []string, stdin []byte, stdout, stderr io.Writer) ([]string, error) {
	sent := &ghSent{w: stderr}
	err := ghChild(ctx, cmd, dir, args, slices.Concat(env, []string{ghDebugEnv}), stdin, stdout, sent).Run()
	flushed := sent.flush()
	return sent.urls, cmp.Or(err, flushed)
}

func ghChild(ctx context.Context, cmd *cobra.Command, dir string, args, env []string, stdin []byte, stdout, stderr io.Writer) *exec.Cmd {
	childEnv := slices.Concat(os.Environ(), render.EnvFrom(ctx), env, []string{render.ChildEnv})
	gh := exec.CommandContext(ctx, lookpath.For(childEnv).Bin("gh"), args...) //nolint:gosec // args is the caller's own gh command
	gh.Dir = dir
	gh.Env = childEnv
	gh.Stdin, gh.Stdout, gh.Stderr = cmd.InOrStdin(), stdout, stderr
	if stdin != nil {
		gh.Stdin = bytes.NewReader(stdin)
	}
	return gh
}

const ghDebugEnv = "GH_DEBUG=1"

var ghDebugLine = regexp.MustCompile(`^(\* |\[git [^\n]*\]\n?$)`)

// ghSent passes gh's stderr on without the lines GH_DEBUG adds, keeping the
// URL each "* Request to" line names.
type ghSent struct {
	w    io.Writer
	line []byte
	urls []string
}

func (s *ghSent) Write(b []byte) (int, error) {
	s.line = append(s.line, b...)
	for {
		end := bytes.IndexByte(s.line, '\n')
		if end < 0 {
			return len(b), nil
		}
		if err := s.take(s.line[:end+1]); err != nil {
			return 0, err
		}
		s.line = s.line[end+1:]
	}
}

func (s *ghSent) flush() error {
	if len(s.line) == 0 {
		return nil
	}
	line := s.line
	s.line = nil
	return s.take(line)
}

func (s *ghSent) take(line []byte) error {
	if url, ok := bytes.CutPrefix(line, []byte("* Request to ")); ok {
		s.urls = append(s.urls, string(bytes.TrimSpace(url)))
	}
	if ghDebugLine.Match(line) {
		return nil
	}
	_, err := s.w.Write(line)
	return err
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

const ghAllowEscapesFlag = "--allow-escape-sequences"

// ghEscapeGuarded are the gh commands besides gh api that refuse content
// carrying a terminal escape sequence unless given --allow-escape-sequences.
var ghEscapeGuarded = map[string]bool{"pr diff": true, "gist view": true, "repo read-file": true, "release download": true}

func ghGuardsEscapes(argv []string) bool {
	return argv[0] == "api" || len(argv) > 1 && ghEscapeGuarded[argv[0]+" "+argv[1]]
}

// ghAllowEscapes returns argv with --allow-escape-sequences right after its gh
// command, ahead of any --.
func ghAllowEscapes(argv []string) []string {
	if slices.Contains(argv, ghAllowEscapesFlag) {
		return argv
	}
	at := 2
	if argv[0] == "api" {
		at = 1
	}
	return slices.Concat(argv[:at], []string{ghAllowEscapesFlag}, argv[at:])
}

// ghSniffLen is how much of a body http.DetectContentType reads, and so how much
// gh's own guard reads to call it binary or text.
const ghSniffLen = 512

// ghPlainWriter strips every terminal escape sequence from the text gh prints,
// holding it whole until Close as gh's own guard does. Output whose first
// ghSniffLen bytes read as binary streams through verbatim, since gh never
// refuses it off a terminal and a strip would corrupt it.
type ghPlainWriter struct {
	w      io.Writer
	text   []byte
	binary bool
}

func (p *ghPlainWriter) Write(b []byte) (int, error) {
	if p.binary {
		return p.w.Write(b)
	}
	sniffed := len(p.text) >= ghSniffLen
	p.text = append(p.text, b...)
	if sniffed || len(p.text) < ghSniffLen || !ghBinary(p.text[:ghSniffLen]) {
		return len(b), nil
	}
	p.binary = true
	if _, err := p.w.Write(p.text); err != nil {
		return 0, err
	}
	return len(b), nil
}

func (p *ghPlainWriter) Close() error {
	if p.binary {
		return nil
	}
	if !ghBinary(p.text[:min(len(p.text), ghSniffLen)]) {
		p.text = ansiRE.ReplaceAll(p.text, nil)
	}
	_, err := p.w.Write(p.text)
	return err
}

// ghBinary reports whether head reads as binary to http.DetectContentType once
// its escape sequences are gone, since the BEL closing an OSC hyperlink is a
// control byte the sniff alone calls binary.
func ghBinary(head []byte) bool {
	return !strings.HasPrefix(http.DetectContentType(ansiRE.ReplaceAll(head, nil)), "text/")
}

// ghOtherIdentity reports whether this read may not run as the signed-in gh
// user, so its answer must not reach, or come from, the shared cache.
func ghOtherIdentity(ctx context.Context) bool {
	return ghapi.UserTokenPinned(ctx) || render.Getenv(ctx, "GH_HOST") != ""
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

// ghAppRateLimited reports whether GitHub refused a read on the app's token for
// rate: gh's own error, or a GraphQL error typed RATE_LIMIT.
func ghAppRateLimited(stdout, stderr []byte) bool {
	return bytes.Contains(bytes.ToLower(stderr), []byte("rate limit")) || ghGraphQLRefused(stdout, "RATE_LIMIT")
}

func ghGraphQLRefused(stdout []byte, kind string) bool {
	var body struct {
		Errors []ghapi.GraphQLMessage `json:"errors"`
	}
	return json.Unmarshal(stdout, &body) == nil && slices.ContainsFunc(body.Errors, func(m ghapi.GraphQLMessage) bool { return strings.HasPrefix(m.Type, kind) })
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

func ghCacheable(argv []string, stdin []byte, scope string) bool {
	if ghWatches(argv) || ghReadsFiles(argv) || ghReadsCIState(argv, stdin) || slices.ContainsFunc(argv, ghUncachedFlag) || slices.ContainsFunc(argv, ghCheckoutPlaceholder) {
		return false
	}
	if argv[0] != "api" {
		return scope != ""
	}
	_, endpoint := ghAPIRequest(argv[1:])
	return strings.TrimPrefix(endpoint, "/") != "rate_limit"
}

var (
	ghCIStateCommands = map[string]bool{"pr checks": true, "pr status": true, "workflow view": true}
	ghCIStateEndpoint = regexp.MustCompile(`(^|/)repos/[^/]+/[^/]+/(commits/[^/]+/(check-runs|check-suites|status|statuses)|statuses/[^/?]+|check-runs|check-suites|actions/(runs|jobs|workflows/[^/]+/runs))(/|$|\?)`)
	ghCIStateField    = regexp.MustCompile(`(?i)statusCheckRollup|checkRuns|checkSuites|\bstatus\s*\{`)
)

// ghReadsCIState reports whether argv, given stdin, reads check, commit status,
// or workflow run state.
func ghReadsCIState(argv []string, stdin []byte) bool {
	if argv[0] == "run" || len(argv) > 1 && ghCIStateCommands[argv[0]+" "+argv[1]] {
		return true
	}
	if argv[0] == "api" {
		if _, endpoint := ghAPIRequest(argv[1:]); ghCIStateEndpoint.MatchString(endpoint) {
			return true
		}
	}
	return slices.ContainsFunc(argv, ghCIStateField.MatchString) || ghCIStateField.Match(stdin)
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

type ghSpillKey struct{}

// chargeGHChild admits a gh child's read through the floor of the pool its
// token spends, AdmitSpill's for a read ghRead moved off the app's pool, and
// charges each request the child sent to that pool once it exits. gh api
// rate_limit spends none.
func chargeGHChild(ctx context.Context, bin string, argv, env []string) (*render.Tap, error) {
	if bin != "gh" || ghNoRequest(argv) || ghReadsRateLimit(argv) {
		return nil, nil
	}
	ctx = render.WithEnv(ctx, env...)
	api, resource := reviewsAPI(), ghResource(argv)
	switch {
	case !ghReads(argv, nil):
	case ctx.Value(ghSpillKey{}) != nil:
		if err := api.AdmitSpill(ctx, resource); err != nil {
			return nil, err
		}
	default:
		if err := api.Admit(ctx, ghapi.IdentityUser, resource); err != nil {
			return nil, err
		}
	}
	sent := &ghSent{}
	return &render.Tap{
		Env:    []string{ghDebugEnv},
		Stderr: func(w io.Writer) io.Writer { sent.w = w; return sent },
		Done: func() {
			if err := errors.Join(sent.flush(), api.Sent(ctx, ghapi.IdentityUser, sent.urls)); err != nil {
				render.Announce(ctx, "%v", err)
			}
		},
	}, nil
}

func ghReadsRateLimit(argv []string) bool {
	if argv[0] != "api" {
		return false
	}
	_, endpoint := ghAPIRequest(argv[1:])
	return strings.TrimPrefix(endpoint, "/") == "rate_limit"
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
