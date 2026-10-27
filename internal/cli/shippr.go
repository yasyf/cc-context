package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/yasyf/cc-context/internal/render"
	"github.com/yasyf/cc-context/internal/vcs"
)

// prBodyStdin is the --pr-body-file value that reads the body from stdin.
const prBodyStdin = "-"

// prBodyTempPrefix names the temp file a body read from stdin is materialized
// into.
const prBodyTempPrefix = "ccx-pr-body-"

// prCarriedTempPrefix names the temp file a restated body carrying live
// release trailers is written to. A failed restate leaves it in place for
// the retry to name.
const prCarriedTempPrefix = "ccx-pr-carried-"

// prURLMarker separates a pull request URL's repository from its number.
const prURLMarker = "/pull/"

// prState is the open pull request ship found for a branch.
type prState struct {
	Number  int    `json:"number"`
	URL     string `json:"html_url"`
	IsDraft bool   `json:"draft"`
}

// prMeta is the caller's stated pull request metadata; an unset field is never
// written, so a re-ship that passes neither leaves a human's edits alone.
type prMeta struct {
	title     string
	bodyPath  string // materialized: a real path, never "-"
	bodyBlank bool   // bodyPath holds nothing but whitespace
	draft     *bool  // nil unless --draft/--publish was explicitly Changed
	base      string
}

func (m prMeta) writesBody() bool {
	return m.bodyPath != "" && !m.bodyBlank
}

// stated names the fields this invocation restated, in report order. No stated
// field means no gh pr edit at all, which is what keeps a re-ship off a
// description someone hand-edited.
func (m prMeta) stated() []string {
	var fields []string
	if m.title != "" {
		fields = append(fields, "title")
	}
	if m.bodyPath != "" {
		fields = append(fields, "body")
	}
	if m.base != "" {
		fields = append(fields, "base")
	}
	return fields
}

// shipPRRequested reports whether this invocation asked ship to touch a pull
// request at all, so a ship that did not ask makes no gh pr call. A bare
// --draft/--publish only counts outside the graphite lane: the graphite submit already
// owns the draft state of every PR it opens.
func shipPRRequested(cmd *cobra.Command, l lane, o shipOpts) bool {
	switch {
	case o.noPR:
		return false
	case len(o.prTitle) > 0 || len(o.prBodyFile) > 0 || (!l.gt && o.parent != ""):
		return true
	default:
		return !l.gt && (cmd.Flags().Changed("draft") || cmd.Flags().Changed("publish"))
	}
}

func materializePRBodyStdin(cmd *cobra.Command, o *shipOpts) (func(), error) {
	cleanup := func() {}
	taken := false
	values := slices.Clone(o.prBodyFile)
	for i, value := range values {
		if _, path := splitPRValue(value, ""); path != prBodyStdin {
			continue
		}
		switch {
		case taken:
			return cleanup, errors.New(`ship: only one --pr-body-file may read stdin ("-")`)
		case !stdinPiped(cmd):
			return cleanup, errors.New(`ship: --pr-body-file - reads the body from stdin, which is a terminal — pipe the body in or pass a path`)
		}
		tmp, remove, err := materializeStdin(cmd.InOrStdin())
		if err != nil {
			return cleanup, err
		}
		values[i] = strings.TrimSuffix(value, prBodyStdin) + tmp
		cleanup, taken = remove, true
	}
	o.prBodyFile = values
	return cleanup, nil
}

func resolvePRMeta(cmd *cobra.Command, o shipOpts, tip string) (map[string]prMeta, error) {
	meta := map[string]prMeta{}
	for _, value := range o.prTitle {
		branch, title := splitPRValue(value, tip)
		entry := meta[branch]
		if entry.title != "" {
			return nil, fmt.Errorf("ship: --pr-title given twice for branch %s", branch)
		}
		entry.title = title
		meta[branch] = entry
	}

	for _, value := range o.prBodyFile {
		branch, path := splitPRValue(value, tip)
		entry := meta[branch]
		if entry.bodyPath != "" {
			return nil, fmt.Errorf("ship: --pr-body-file given twice for branch %s", branch)
		}
		body, err := os.ReadFile(path) //nolint:gosec // the caller names the body file
		if err != nil {
			return nil, fmt.Errorf("ship: --pr-body-file %s: %w", path, err)
		}
		entry.bodyPath = path
		entry.bodyBlank = strings.TrimSpace(string(body)) == ""
		meta[branch] = entry
	}

	if cmd.Flags().Changed("draft") || cmd.Flags().Changed("publish") {
		entry := meta[tip]
		draft := o.draft
		entry.draft = &draft
		meta[tip] = entry
	}
	return meta, nil
}

// splitPRValue splits a repeatable pull request flag value into the branch it
// scopes and the value itself. A value is branch-scoped only when the text
// before its first "=" is a name git would accept as a branch, so a bare title
// carrying an "=" still lands on tip.
func splitPRValue(value, tip string) (branch, rest string) {
	name, rest, ok := strings.Cut(value, "=")
	if !ok || strings.ContainsAny(name, " \t") || !legalBranchName(name) {
		return tip, value
	}
	return name, rest
}

func unscopedPRValue(values []string) string {
	for _, value := range values {
		if branch, rest := splitPRValue(value, ""); branch == "" {
			return rest
		}
	}
	return ""
}

func shipMessageFromPR(o shipOpts) (string, error) {
	title := unscopedPRValue(o.prTitle)
	if title == "" {
		return "", errShipMessageRequired
	}
	path := unscopedPRValue(o.prBodyFile)
	if path == "" {
		return title, nil
	}
	body, err := os.ReadFile(path) //nolint:gosec // the caller names the body file
	if err != nil {
		return "", fmt.Errorf("ship: --pr-body-file %s: %w", path, err)
	}
	if prose := commitBodyFromPR(string(body)); prose != "" {
		return title + "\n\n" + prose, nil
	}
	return title, nil
}

var (
	prHeadingRE = regexp.MustCompile(`^#{1,6}\s+(.*\S)\s*$`)
	prSummaryRE = regexp.MustCompile(`^<summary>.*</summary>$`)
)

func commitBodyFromPR(body string) string {
	var out []string
	label := ""
	for _, line := range strings.Split(strings.ReplaceAll(body, "\r\n", "\n"), "\n") {
		trimmed := strings.TrimSpace(line)
		switch {
		case trimmed == "<details>" || trimmed == "</details>" || prSummaryRE.MatchString(trimmed):
			continue
		case prHeadingRE.MatchString(trimmed):
			label = prHeadingRE.FindStringSubmatch(trimmed)[1] + ": "
		case trimmed == "":
			if len(out) > 0 && out[len(out)-1] != "" {
				out = append(out, "")
			}
		default:
			out = append(out, label+line)
			label = ""
		}
	}
	return strings.TrimRight(strings.Join(out, "\n"), "\n")
}

// materializeStdin writes r to a temp file, because ship needs the body as a
// path: gh takes --body-file, ship may need the body twice in one run when a
// create races into an edit, and it has to be readable before the commit forms.
func materializeStdin(r io.Reader) (string, func(), error) {
	f, err := os.CreateTemp("", prBodyTempPrefix+"*")
	if err != nil {
		return "", nil, fmt.Errorf("ship: create pr body file: %w", err)
	}
	path := f.Name()
	remove := func() { _ = os.Remove(path) }
	if _, err := io.Copy(f, r); err != nil {
		_ = f.Close()
		remove()
		return "", nil, fmt.Errorf("ship: read pr body from stdin: %w", err)
	}
	if err := f.Close(); err != nil {
		remove()
		return "", nil, fmt.Errorf("ship: write pr body file: %w", err)
	}
	return path, remove, nil
}

// shipPRRepo resolves the repository the pull request step names in --repo, and
// does it before any mutation: an unreachable GitHub is a refusal, not a
// half-finished ship. A plan that stays on trunk outside the graphite lane
// opens no pull request, so it needs no repository.
func shipPRRepo(ctx context.Context, l lane, plan branchPlan, base string) (string, error) {
	if !l.gt && plan.trunk != "" && plan.name == plan.trunk {
		return "", nil
	}
	repo, err := vcs.LookupRepo(ctx, l.dir(), false)
	if err != nil {
		return "", fmt.Errorf("ship: a pull request needs GitHub metadata: %w", err)
	}
	if !l.gt && plan.trunk == "" && base == "" {
		return "", fmt.Errorf("ship: %w; pass --parent <base-branch> to name the pull request base before publishing", vcs.ErrNoTrunk)
	}
	return repo.NameWithOwner, nil
}

// shipPR is the single pull-request-owning step for every lane. It opens the
// branch's pull request when there is none and restates exactly the fields this
// invocation named when there is, so a description someone edited by hand
// survives a re-ship that does not mention it.
func shipPR(ctx context.Context, errW io.Writer, l lane, nwo, remote, branch, trunk, subject string, meta map[string]prMeta, stack []stackEntry) (string, error) {
	if l.gt {
		return shipPRGT(ctx, nwo, meta, stack)
	}
	// A pull request from trunk into trunk is not a thing, and on a personal
	// repository that is the normal place to ship from.
	if trunk != "" && branch == trunk {
		return "no PR (on trunk)", nil
	}
	m := meta[branch]
	pr, found, err := lookupPR(ctx, nwo, branch)
	if err != nil {
		return "", err
	}
	if found {
		cciClaimPRs(ctx, errW, l.dir(), "ship", nwo, nil, []int{pr.Number})
		return shipPREdit(ctx, nwo, pr, m)
	}
	base, picked := m.base, false
	if base == "" {
		if base, err = shipPRBase(ctx, l.dir(), remote, branch, trunk); err != nil {
			return "", err
		}
		picked = base != trunk
	}
	created, viaGraphQL, err := shipPRCreate(ctx, nwo, branch, base, subject, m)
	if err != nil {
		return "", err
	}
	cciClaimPRs(ctx, errW, l.dir(), "ship", nwo, []cciOpened{{pr: created.Number, branch: branch}}, nil)
	return shipPRSegment(created, base, picked, viaGraphQL), nil
}

// shipPRBase is the base a new pull request opens against when --parent named
// none: the nearest ancestor of the branch that a remote branch points at and
// the remote trunk does not carry, and trunk when there is none. A branch cut
// from an unlanded one holds that branch's commits, so a pull request based on
// trunk proposes them as its own and only the file count says so.
func shipPRBase(ctx context.Context, dir render.Dir, remote, branch, trunk string) (string, error) {
	if remote == "" || trunk == "" {
		return trunk, nil
	}
	if _, err := stackRemoteHeads(ctx, dir, "ship", remote, []string{trunk}, ""); err != nil {
		return "", err
	}
	out, err := render.RunCLI(ctx, dir, "git", []string{"ls-remote", "--heads", remote})
	if err != nil {
		return "", fmt.Errorf("ship: git ls-remote --heads %s: %w", remote, err)
	}
	heads := map[string][]string{}
	for line := range strings.Lines(out) {
		sha, ref, _ := strings.Cut(strings.TrimSpace(line), "\t")
		name, ok := strings.CutPrefix(ref, "refs/heads/")
		if ok && name != branch && name != trunk {
			heads[sha] = append(heads[sha], name)
		}
	}
	if len(heads) == 0 {
		return trunk, nil
	}
	head := "refs/heads/" + branch
	exclude := "^refs/remotes/" + remote + "/" + trunk
	out, err = render.RunCLI(ctx, dir, "git", []string{"rev-list", head, exclude})
	if err != nil {
		return "", fmt.Errorf("ship: git rev-list %s %s: %w", head, exclude, err)
	}
	candidates := map[string]string{}
	for line := range strings.Lines(out) {
		sha := strings.TrimSpace(line)
		for _, name := range heads[sha] {
			candidates[name] = sha
		}
	}
	base, nearest := trunk, 0
	for _, name := range slices.Sorted(maps.Keys(candidates)) {
		ahead, err := gitCommitsAhead(ctx, dir, "ship", candidates[name], head)
		if err != nil {
			return "", err
		}
		if ahead == 0 || (base != trunk && ahead >= nearest) {
			continue
		}
		base, nearest = name, ahead
	}
	return base, nil
}

// lookupPR resolves branch's open pull request over REST, which answers an
// empty array when there is none. state=open matters: a merged pull request on
// a reused branch name must never be edited.
func lookupPR(ctx context.Context, nwo, branch string) (prState, bool, error) {
	out, err := render.RunCLI(ctx, render.Ambient, "gh", ghPullsByHeadArgv(nwo, branch, "open"))
	if err != nil {
		return prState{}, false, fmt.Errorf("ship: gh api pulls: %w", err)
	}
	var prs []prState
	if err := json.Unmarshal([]byte(out), &prs); err != nil {
		return prState{}, false, fmt.Errorf("ship: parse gh api pulls: %w", err)
	}
	if len(prs) == 0 {
		return prState{}, false, nil
	}
	return prs[0], true, nil
}

// shipPRCreate opens the branch's pull request over REST. The repository and
// an explicit base are load-bearing: from a fork, the base would otherwise
// resolve to the parent and can target upstream. The body is only what the
// caller stated, never the commit message, which would publish
// withSessionTrailer's Claude-Session-Id line into the description.
func shipPRCreate(ctx context.Context, nwo, branch, base, subject string, m prMeta) (prState, bool, error) {
	title := m.title
	if title == "" {
		title = subject
	}
	draft := m.draft != nil && *m.draft
	return createPR(ctx, nwo, branch, base, title, m.bodyPath, draft)
}

func shipPRSegment(pr prState, base string, picked, viaGraphQL bool) string {
	seg := fmt.Sprintf("opened PR #%d %s", pr.Number, pr.URL)
	if picked {
		seg += " onto " + base
	}
	if pr.IsDraft {
		seg += " [draft]"
	}
	return seg + overGraphQL(viaGraphQL)
}

func overGraphQL(viaGraphQL bool) string {
	if viaGraphQL {
		return " over GraphQL"
	}
	return ""
}

// prCreateAttempts bounds the creates a server error may repeat.
const prCreateAttempts = 2

// createPR posts the pull request. GitHub can open one and still answer an
// error, so a refused create fails only when the branch has no open pull
// request afterward. A server error that left none is posted again: GitHub
// refuses a second open pull request for one head and base. A rate limit is
// not waited out but sent to GraphQL's createPullRequest, which GitHub meters
// apart from REST writes; viaGraphQL reports that it opened the pull request.
func createPR(ctx context.Context, nwo, branch, base, title, bodyPath string, draft bool) (pr prState, viaGraphQL bool, err error) {
	argv := prCreateArgv(nwo, branch, base, title, bodyPath, draft)
	for attempt := 1; ; attempt++ {
		out, err := ghAPIWaiting(ctx, render.Ambient, ghRateLimitWait, 0, argv[1:]...)
		if err == nil {
			var pr prState
			if err := json.Unmarshal([]byte(out), &pr); err != nil {
				return prState{}, false, fmt.Errorf("ship: parse gh api create pull: %w", err)
			}
			return pr, false, nil
		}
		pr, found, lookupErr := lookupPR(ctx, nwo, branch)
		if found {
			return pr, false, nil
		}
		var refusal *ghRefusal
		refused := lookupErr == nil && errors.As(err, &refusal)
		if attempt < prCreateAttempts && refused && refusal.status >= http.StatusInternalServerError {
			continue
		}
		retry := []string{ghCommand(prCreateArgv(nwo, branch, base, title, prRetryBody(bodyPath), draft))}
		if !refused || !strings.Contains(strings.ToLower(refusal.message), "rate limit") {
			return prState{}, false, prStepError("the push", "create", retry, nil, errors.Join(err, lookupErr))
		}
		pr, gqlErr := createPRGraphQL(ctx, nwo, branch, base, title, bodyPath, draft)
		if gqlErr != nil {
			graphQL := []string{prCreateGraphQLRetry(nwo, branch, base, title, prRetryBody(bodyPath), draft)}
			return prState{}, true, prStepError("the push", "create", retry, graphQL, errors.Join(err, fmt.Errorf("graphql fallback: %w", gqlErr)))
		}
		return pr, true, nil
	}
}

func createPRGraphQL(ctx context.Context, nwo, branch, base, title, bodyPath string, draft bool) (prState, error) {
	id, err := render.RunCLI(ctx, render.Ambient, "gh", repoNodeIDArgv(nwo))
	if err != nil {
		return prState{}, err
	}
	out, err := render.RunCLI(ctx, render.Ambient, "gh", append(prCreateGraphQLArgv(branch, base, title, bodyPath, draft), "-f", "id="+strings.TrimSpace(id)))
	if err != nil {
		return prState{}, err
	}
	var pr prState
	if err := json.Unmarshal([]byte(out), &pr); err != nil {
		return prState{}, fmt.Errorf("parse createPullRequest: %w", err)
	}
	return pr, nil
}

const repoNodeIDQuery = `query($owner:String!,$repo:String!){repository(owner:$owner,name:$repo){id}}`

func repoNodeIDArgv(nwo string) []string {
	owner, repo, _ := strings.Cut(nwo, "/")
	return []string{"api", "graphql", "-f", "owner=" + owner, "-f", "repo=" + repo, "-f", "query=" + repoNodeIDQuery, "--jq", ".data.repository.id"}
}

// prCreateMutation aliases the created pull request's fields to the REST names
// prState decodes.
const prCreateMutation = `mutation($id:ID!,$head:String!,$base:String!,$title:String!,$body:String!,$draft:Boolean!)` +
	`{createPullRequest(input:{repositoryId:$id,headRefName:$head,baseRefName:$base,title:$title,body:$body,draft:$draft})` +
	`{pullRequest{number,html_url:url,draft:isDraft}}}`

// prCreateGraphQLArgv is prCreateArgv over GraphQL, less the repository's node
// id.
func prCreateGraphQLArgv(branch, base, title, bodyPath string, draft bool) []string {
	argv := []string{"api", "graphql", "-f", "query=" + prCreateMutation, "-f", "head=" + branch, "-f", "base=" + base, "-f", "title=" + title}
	if bodyPath != "" {
		argv = append(argv, "-F", "body=@"+bodyPath)
	} else {
		argv = append(argv, "-f", "body=")
	}
	return append(argv, "-F", "draft="+strconv.FormatBool(draft), "--jq", ".data.createPullRequest.pullRequest")
}

// prCreateGraphQLRetry is prCreateGraphQLArgv as a person re-runs it, the node
// id looked up inline.
func prCreateGraphQLRetry(nwo, branch, base, title, bodyPath string, draft bool) string {
	return ghCommand(prCreateGraphQLArgv(branch, base, title, bodyPath, draft)) + ` -f id="$(` + ghCommand(repoNodeIDArgv(nwo)) + `)"`
}

func prCreateArgv(nwo, branch, trunk, title, bodyPath string, draft bool) []string {
	argv := []string{"api", "-X", "POST", "repos/" + nwo + "/pulls", "-f", "head=" + branch, "-f", "base=" + trunk, "-f", "title=" + title}
	if bodyPath != "" {
		argv = append(argv, "-F", "body=@"+bodyPath)
	} else {
		argv = append(argv, "-f", "body=")
	}
	if draft {
		argv = append(argv, "-F", "draft=true")
	}
	return argv
}

// shipPREdit restates the fields this invocation named on an existing pull
// request. Zero stated fields means zero calls.
func shipPREdit(ctx context.Context, nwo string, pr prState, m prMeta) (string, error) {
	fields := m.stated()
	// REST has no draft toggle, so a transition is gh pr ready's own verb — and
	// only a real transition is worth a call.
	var ready []string
	readyField := ""
	if m.draft != nil && *m.draft != pr.IsDraft {
		ready = []string{"pr", "ready", strconv.Itoa(pr.Number), "--repo", nwo}
		readyField = "ready"
		if *m.draft {
			ready = append(ready, "--undo")
			readyField = "draft"
		}
	}
	restateFailed := func(m prMeta, viaGraphQL bool, err error) error {
		var then []string
		if ready != nil {
			then = []string{ghCommand(ready)}
		}
		retry := append([]string{ghCommand(prRetryArgv(nwo, pr.Number, m))}, then...)
		var graphQL []string
		if viaGraphQL {
			graphQL = append([]string{prUpdateRetry(nwo, pr.Number, m)}, then...)
		}
		return prStepError("the push", "restate", retry, graphQL, err)
	}
	viaGraphQL := false
	if len(fields) > 0 {
		carried, cleanup, err := withLiveReleaseTrailers(ctx, nwo, pr.Number, m)
		if err != nil {
			return "", restateFailed(m, false, err)
		}
		if viaGraphQL, err = restatePR(ctx, nwo, pr.Number, carried); err != nil {
			return "", restateFailed(carried, viaGraphQL, err)
		}
		cleanup()
	}
	if ready != nil {
		if _, err := render.RunCLI(ctx, render.Ambient, "gh", ready); err != nil {
			return "", prStepError("the push", "restate", []string{ghCommand(ready)}, nil, err)
		}
		fields = append(fields, readyField)
	}
	if len(fields) == 0 {
		return "", nil
	}
	return fmt.Sprintf("updated PR #%d %s (%s)", pr.Number, pr.URL, strings.Join(fields, ", ")) + overGraphQL(viaGraphQL), nil
}

// prEditArgv restates the stated fields through REST. -F body=@path sends the
// file's bytes as the string they are, where a bare -F would coerce a body
// reading "true" or "42" into JSON.
func prEditArgv(nwo string, number int, m prMeta) []string {
	var fields []string
	if m.title != "" {
		fields = append(fields, "-f", "title="+m.title)
	}
	if m.bodyPath != "" {
		fields = append(fields, "-F", "body=@"+m.bodyPath)
	}
	if m.base != "" {
		fields = append(fields, "-f", "base="+m.base)
	}
	return ghPatchPullArgv(nwo, number, fields...)
}

// restatePR writes the stated fields over REST, and over GraphQL's
// updatePullRequest when REST refuses with a rate limit: GitHub meters the two
// apart, so a secondary limit on REST writes leaves GraphQL open. viaGraphQL
// reports that the fallback ran. m's body already carries the live Release-*
// trailers withLiveReleaseTrailers added.
func restatePR(ctx context.Context, nwo string, number int, m prMeta) (viaGraphQL bool, err error) {
	_, err = render.RunCLI(ctx, render.Ambient, "gh", prEditArgv(nwo, number, m))
	if err == nil || !strings.Contains(strings.ToLower(err.Error()), "rate limit") {
		return false, err
	}
	id, gqlErr := render.RunCLI(ctx, render.Ambient, "gh", prNodeIDArgv(nwo, number))
	if gqlErr == nil {
		_, gqlErr = render.RunCLI(ctx, render.Ambient, "gh", append(prUpdateArgv(m), "-f", "id="+strings.TrimSpace(id)))
	}
	if gqlErr != nil {
		return true, errors.Join(err, fmt.Errorf("graphql fallback: %w", gqlErr))
	}
	return true, nil
}

// withLiveReleaseTrailers is m with its body carrying the release trailers
// that close pull request number's live body and that m's body neither
// restates nor retracts. A body that needed them is
// written to a temp file, which cleanup removes once the restate lands.
func withLiveReleaseTrailers(ctx context.Context, nwo string, number int, m prMeta) (carried prMeta, cleanup func(), err error) {
	noop := func() {}
	if m.bodyPath == "" {
		return m, noop, nil
	}
	out, err := render.RunCLI(ctx, render.Ambient, "gh", prBodyArgv(nwo, number))
	if err != nil {
		return m, noop, fmt.Errorf("ship: read PR #%d body: %w", number, err)
	}
	var live struct {
		Body string `json:"body"`
	}
	if err := json.Unmarshal([]byte(out), &live); err != nil {
		return m, noop, fmt.Errorf("ship: parse PR #%d body: %w", number, err)
	}
	next, err := os.ReadFile(m.bodyPath) //nolint:gosec // the caller names the body file
	if err != nil {
		return m, noop, fmt.Errorf("ship: --pr-body-file %s: %w", m.bodyPath, err)
	}
	body := carryReleaseTrailers(live.Body, string(next))
	if body == string(next) {
		return m, noop, nil
	}
	f, err := os.CreateTemp("", prCarriedTempPrefix+"*")
	if err != nil {
		return m, noop, fmt.Errorf("ship: create pr body file: %w", err)
	}
	remove := func() { _ = os.Remove(f.Name()) }
	if _, err := f.WriteString(body); err != nil {
		_ = f.Close()
		remove()
		return m, noop, fmt.Errorf("ship: write pr body file: %w", err)
	}
	if err := f.Close(); err != nil {
		remove()
		return m, noop, fmt.Errorf("ship: write pr body file: %w", err)
	}
	m.bodyPath = f.Name()
	return m, remove, nil
}

func prBodyArgv(nwo string, number int) []string {
	return []string{"api", "-X", "GET", ghPullPath(nwo, number)}
}

var gitTrailerRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]*:(\s|$)`)

const retractedTrailerValue = "-"

func carryReleaseTrailers(live, next string) string {
	lines := bodyLines(next)
	start := closingTrailerStart(lines)
	declared := map[string]bool{}
	kept := lines[:start:start]
	for _, line := range lines[start:] {
		key, value, _ := strings.Cut(strings.TrimSpace(line), ":")
		if strings.TrimSpace(line) != "" {
			declared[strings.ToLower(key)] = true
		}
		if !isReleaseTrailerKey(key) || strings.TrimSpace(value) != retractedTrailerValue {
			kept = append(kept, line)
		}
	}
	if len(kept) < len(lines) {
		next = strings.TrimRight(strings.Join(kept, "\n"), " \t\r\n") + "\n"
	}
	present := map[string]bool{}
	for _, line := range bodyLines(next) {
		present[strings.TrimSpace(line)] = true
	}
	var carried []string
	for _, line := range closingTrailers(live) {
		key, _, _ := strings.Cut(line, ":")
		if !isReleaseTrailerKey(key) || declared[strings.ToLower(key)] || present[line] {
			continue
		}
		carried = append(carried, line)
		present[line] = true
	}
	if len(carried) == 0 {
		return next
	}
	sep := "\n\n"
	if len(closingTrailers(next)) > 0 {
		sep = "\n"
	}
	return strings.TrimRight(next, " \t\r\n") + sep + strings.Join(carried, "\n") + "\n"
}

func isReleaseTrailerKey(key string) bool {
	key = strings.ToLower(key)
	return strings.HasPrefix(key, "release-") || key == "readers-first"
}

func closingTrailers(body string) []string {
	lines := bodyLines(body)
	var trailers []string
	for _, line := range lines[closingTrailerStart(lines):] {
		if trimmed := strings.TrimSpace(line); trimmed != "" {
			trailers = append(trailers, trimmed)
		}
	}
	return trailers
}

func closingTrailerStart(lines []string) int {
	start, end := len(lines), len(lines)
	for {
		for end > 0 && strings.TrimSpace(lines[end-1]) == "" {
			end--
		}
		paragraph := end
		for paragraph > 0 && strings.TrimSpace(lines[paragraph-1]) != "" {
			paragraph--
		}
		if paragraph == end {
			return start
		}
		for _, line := range lines[paragraph:end] {
			if !gitTrailerRE.MatchString(strings.TrimSpace(line)) {
				return start
			}
		}
		start, end = paragraph, paragraph
	}
}

func bodyLines(body string) []string {
	return strings.Split(strings.ReplaceAll(body, "\r\n", "\n"), "\n")
}

const prNodeIDQuery = `query($owner:String!,$repo:String!,$number:Int!){repository(owner:$owner,name:$repo){pullRequest(number:$number){id}}}`

func prNodeIDArgv(nwo string, number int) []string {
	owner, repo, _ := strings.Cut(nwo, "/")
	return []string{
		"api", "graphql", "-f", "owner=" + owner, "-f", "repo=" + repo, "-F", "number=" + strconv.Itoa(number),
		"-f", "query=" + prNodeIDQuery, "--jq", ".data.repository.pullRequest.id",
	}
}

// prUpdateArgv is prEditArgv over GraphQL, less the pull request's node id,
// declaring only the stated fields so an unstated one is never written.
func prUpdateArgv(m prMeta) []string {
	vars, input, fields := []string{"$id:ID!"}, []string{"pullRequestId:$id"}, []string{}
	if m.title != "" {
		vars, input = append(vars, "$title:String!"), append(input, "title:$title")
		fields = append(fields, "-f", "title="+m.title)
	}
	if m.bodyPath != "" {
		vars, input = append(vars, "$body:String!"), append(input, "body:$body")
		fields = append(fields, "-F", "body=@"+m.bodyPath)
	}
	if m.base != "" {
		vars, input = append(vars, "$base:String!"), append(input, "baseRefName:$base")
		fields = append(fields, "-f", "base="+m.base)
	}
	query := fmt.Sprintf("mutation(%s){updatePullRequest(input:{%s}){clientMutationId}}", strings.Join(vars, ","), strings.Join(input, ","))
	return append([]string{"api", "graphql", "--silent", "-f", "query=" + query}, fields...)
}

// prUpdateRetry is prUpdateArgv as a person re-runs it, the node id looked up
// inline.
func prUpdateRetry(nwo string, number int, m prMeta) string {
	m.bodyPath = prRetryBody(m.bodyPath)
	return ghCommand(prUpdateArgv(m)) + ` -f id="$(` + ghCommand(prNodeIDArgv(nwo, number)) + `)"`
}

// prRetryArgv is prEditArgv as a person re-runs it.
func prRetryArgv(nwo string, number int, m prMeta) []string {
	m.bodyPath = prRetryBody(m.bodyPath)
	return prEditArgv(nwo, number, m)
}

// prRetryBody is the body file a person re-runs a pull request write with. A
// body ship read from stdin lives in a temp file ship deletes on the way out,
// so its retry reads stdin again.
func prRetryBody(path string) string {
	if filepath.Dir(path) == filepath.Clean(os.TempDir()) && strings.HasPrefix(filepath.Base(path), prBodyTempPrefix) {
		return prBodyStdin
	}
	return path
}

// prStepError reports a pull request step that failed after the branch
// already reached GitHub, naming the commands that finish only that step, so
// a caller does not read the whole ship as failed and re-run it. graphQL is the
// same step over GraphQL, when REST refused it with a rate limit.
func prStepError(done, step string, retry, graphQL []string, err error) error {
	finish := strings.Join(retry, " && ")
	if len(graphQL) > 0 {
		finish += ", or over GraphQL: " + strings.Join(graphQL, " && ")
	}
	return fmt.Errorf("ship: %s already happened; only the pull request %s failed — finish it with: %s: %w", done, step, finish, err)
}

// shipPRGT backfills the pull requests the submit just opened, over the
// downstack the push step already resolved, restating exactly the branches
// this invocation named. A branch the caller did not name has nothing to
// write, so it is left alone whether or not it already has a body.
func shipPRGT(ctx context.Context, nwo string, meta map[string]prMeta, stack []stackEntry) (string, error) {
	pending := make(map[string]prMeta, len(stack))
	for _, entry := range stack {
		if m := meta[entry.Branch]; !entry.metaApplied && entry.PR != 0 && len(m.stated()) > 0 {
			pending[entry.Branch] = m
		}
	}
	carried := make(map[string]prMeta, len(pending))
	cleanups := make(map[string]func(), len(pending))
	for i := len(stack) - 1; i >= 0; i-- {
		entry := stack[i]
		m, ok := pending[entry.Branch]
		if !ok {
			continue
		}
		c, cleanup, err := withLiveReleaseTrailers(ctx, nwo, entry.PR, m)
		if err != nil {
			for _, cleanup := range cleanups {
				cleanup()
			}
			retry, _ := prRestatesLeft(nwo, pending, stack)
			return "", prStepError("the push and graphite submit", "restate", retry, nil, err)
		}
		carried[entry.Branch], cleanups[entry.Branch] = c, cleanup
	}
	segs := make([]string, 0, len(stack))
	for i := len(stack) - 1; i >= 0; i-- {
		entry := stack[i]
		m, ok := carried[entry.Branch]
		if !ok {
			continue
		}
		viaGraphQL, err := restatePR(ctx, nwo, entry.PR, m)
		if err != nil {
			retry, graphQL := prRestatesLeft(nwo, carried, stack[:i+1])
			if !viaGraphQL {
				graphQL = nil
			}
			return "", prStepError("the push and graphite submit", "restate", retry, graphQL, err)
		}
		cleanups[entry.Branch]()
		segs = append(segs, fmt.Sprintf("PR #%d %s", entry.PR, strings.Join(m.stated(), "+"))+overGraphQL(viaGraphQL))
	}
	if len(segs) == 0 {
		return "", nil
	}
	return "set " + strings.Join(segs, ", "), nil
}

// prRestatesLeft is every restate shipPRGT had yet to make when one failed, in
// the order it makes them, over REST and over GraphQL.
func prRestatesLeft(nwo string, meta map[string]prMeta, stack []stackEntry) (rest, graphQL []string) {
	for i := len(stack) - 1; i >= 0; i-- {
		entry := stack[i]
		m := meta[entry.Branch]
		if entry.PR == 0 || len(m.stated()) == 0 {
			continue
		}
		rest = append(rest, ghCommand(prRetryArgv(nwo, entry.PR, m)))
		graphQL = append(graphQL, prUpdateRetry(nwo, entry.PR, m))
	}
	return rest, graphQL
}

// prNumberFromURL reads the pull request number off the URL gh pr create prints,
// which is its only machine-readable output.
func prNumberFromURL(url string) (int, error) {
	i := strings.LastIndex(url, prURLMarker)
	if i < 0 {
		return 0, fmt.Errorf("ship: gh pr create printed no pull request URL: %q", url)
	}
	number, err := strconv.Atoi(strings.Trim(url[i+len(prURLMarker):], "/"))
	if err != nil {
		return 0, fmt.Errorf("ship: gh pr create printed a malformed pull request URL: %q", url)
	}
	return number, nil
}
