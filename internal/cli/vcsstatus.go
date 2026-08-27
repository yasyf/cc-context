package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/yasyf/cc-context/internal/render"
	"github.com/yasyf/cc-context/internal/vcs"
)

// statusBudget caps the human report, which grows with the stack.
const statusBudget = 6000

// statusMergeableRetry is the pause before re-asking for a mergeable GitHub
// answered UNKNOWN. The first request only schedules the background merge
// commit; the answer lands on a later one.
const statusMergeableRetry = 1500 * time.Millisecond

// statusUnknown is GitHub's answer when it has not computed a mergeability yet.
const statusUnknown = "UNKNOWN"

// statusDirtyPaths caps how many uncommitted paths one line names.
const statusDirtyPaths = 8

// vcsStatus is every branch of the stack, what its pull request is waiting on,
// and what would land if it merged now.
type vcsStatus struct {
	Lane       string         `json:"lane"`
	Root       string         `json:"root"`
	Repo       string         `json:"repo,omitempty"`
	Branch     string         `json:"branch,omitempty"`
	Trunk      string         `json:"trunk,omitempty"`
	Dirty      bool           `json:"dirty"`
	DirtyFiles []string       `json:"dirty_files,omitempty"`
	Required   []string       `json:"required_checks,omitempty"`
	StackError string         `json:"stack_error,omitempty"`
	PRError    string         `json:"pr_error,omitempty"`
	Branches   []statusBranch `json:"branches,omitempty"`
}

// statusDiverge counts a branch against trunk, and is absent when no
// remote-tracking trunk ref resolved to count against.
type statusDiverge struct {
	Ahead  int `json:"ahead"`
	Behind int `json:"behind"`
}

// statusBranch is one branch of the stack, in stack order.
type statusBranch struct {
	Name         string         `json:"branch"`
	Current      bool           `json:"current,omitempty"`
	Holder       string         `json:"holder"`
	NeedsRestack bool           `json:"needs_restack,omitempty"`
	Diverge      *statusDiverge `json:"diverge,omitempty"`
	PR           *statusPR      `json:"pr,omitempty"`
	Blockers     []string       `json:"blockers,omitempty"`
}

// statusPR is one pull request's landing state. Mergeable is GitHub's own
// three-valued answer rather than a boolean, because UNKNOWN — which it returns
// until it has computed the merge commit — is neither of the others.
type statusPR struct {
	Number         int            `json:"number"`
	URL            string         `json:"url"`
	State          string         `json:"state"`
	Merged         bool           `json:"merged"`
	Draft          bool           `json:"draft,omitempty"`
	HasBody        bool           `json:"has_body"`
	Head           string         `json:"head"`
	Base           string         `json:"base"`
	Mergeable      string         `json:"mergeable"`
	MergeState     string         `json:"merge_state"`
	ReviewDecision string         `json:"review_decision,omitempty"`
	Labels         []string       `json:"labels,omitempty"`
	ChecksState    string         `json:"checks_state,omitempty"`
	Checks         []statusCheck  `json:"checks,omitempty"`
	Reviews        []statusReview `json:"reviews,omitempty"`
	Queue          *statusQueue   `json:"queue,omitempty"`
}

// statusCheck is one check on the head commit. Required is membership of the
// base branch's protection rule, and is false for every check when nobody may
// read the rules — the report says nothing about requirements in that case.
type statusCheck struct {
	Name     string `json:"name"`
	State    string `json:"state"`
	Required bool   `json:"required,omitempty"`
}

// statusReview is one reviewer's standing verdict. Stale means it was cast on a
// commit the branch has since moved off, which GitHub still counts toward
// reviewDecision.
type statusReview struct {
	Author string `json:"author"`
	Bot    bool   `json:"bot,omitempty"`
	State  string `json:"state"`
	Commit string `json:"commit,omitempty"`
	Stale  bool   `json:"stale,omitempty"`
}

type vcsStatusOpts struct {
	json    bool
	refresh bool
	noGT    bool
	budget  int
}

func newVcsStatusCmd() *cobra.Command {
	var o vcsStatusOpts
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Report every branch of the stack and what blocks each from landing",
		Long: `Report every branch of the stack and what blocks each from landing.

Per branch, in stack order: its divergence from trunk, its pull request's
mergeability, the checks on its head and which of them the base branch
requires, every standing review and whether it was cast on the head, and what
the Graphite merge queue is holding.

That last one is the fact no GitHub field carries. The queue snapshots a pull
request when it admits it, so a push after that lands the older commit and
silently drops the rest; status reconstructs the snapshot from the queue's own
activity comment and the branch's head history, and names the commits a merge
would leave behind.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runVcsStatus(cmd, o)
		},
	}
	cmd.Flags().BoolVar(&o.json, "json", false, "emit the report as JSON")
	cmd.Flags().BoolVar(&o.refresh, "refresh", false, "refetch the cached GitHub metadata and graphite reachability")
	cmd.Flags().BoolVar(&o.noGT, "no-gt", false, "ignore a live graphite config and report the current branch alone")
	cmd.Flags().IntVar(&o.budget, "budget", statusBudget, "token budget for the human report (0 = uncapped)")
	return cmd
}

func runVcsStatus(cmd *cobra.Command, o vcsStatusOpts) error {
	ctx := cmd.Context()
	l, err := resolveLaneReport(ctx, "status", workingDir(), o.noGT, o.refresh)
	if err != nil {
		return err
	}
	st, err := collectVcsStatus(ctx, l, o)
	if err != nil {
		return err
	}
	if o.json {
		data, err := json.MarshalIndent(st, "", "  ")
		if err != nil {
			return fmt.Errorf("status: marshal report: %w", err)
		}
		cmd.Println(string(data))
		return nil
	}
	cmd.Print(render.Cap(renderVcsStatus(st), o.budget))
	return nil
}

func collectVcsStatus(ctx context.Context, l lane, o vcsStatusOpts) (vcsStatus, error) {
	st := vcsStatus{Lane: kindLabel(l.kind), Root: l.root}
	if l.gt {
		st.Lane = "gt"
	}
	if l.broken != nil {
		return st, nil
	}
	if err := statusRepo(ctx, l, o, &st); err != nil {
		return vcsStatus{}, err
	}
	if err := statusWorkingCopy(ctx, l, &st); err != nil {
		return vcsStatus{}, err
	}
	trunk, hasTrunk, err := statusTrunkRef(ctx, l, &st)
	if err != nil {
		return vcsStatus{}, err
	}
	branches, state := statusStack(ctx, l, &st)
	if len(branches) == 0 {
		return st, nil
	}
	if err := statusLocalBranches(ctx, l, state, branches, trunk, hasTrunk, &st); err != nil {
		return vcsStatus{}, err
	}
	statusResolvePRs(ctx, l, &st)
	for i := range st.Branches {
		st.Branches[i].Blockers = statusBlockers(st.Branches[i])
	}
	return st, nil
}

// statusRepo names the GitHub repository, reusing the record the lane gates
// already read. A repository GitHub cannot answer for leaves the name empty and
// the pull request pass a no-op.
func statusRepo(ctx context.Context, l lane, o vcsStatusOpts, st *vcsStatus) error {
	if l.repo != nil {
		st.Repo = l.repo.NameWithOwner
		return nil
	}
	repo, err := vcs.LookupRepo(ctx, l.dir(), o.refresh)
	switch {
	case err == nil:
		st.Repo = repo.NameWithOwner
	case errors.Is(err, vcs.ErrNoGitHub):
		st.PRError = err.Error()
	default:
		return err
	}
	return nil
}

// statusWorkingCopy reads the branch and every uncommitted path.
func statusWorkingCopy(ctx context.Context, l lane, st *vcsStatus) error {
	if l.kind == vcs.JJ && !l.gt {
		names, err := jjBookmarkNames(ctx, l.dir(), "status", jjNearestBookmarkRevset)
		if err != nil {
			return err
		}
		st.Branch = strings.Join(names, " ")
	} else {
		branch, err := gitCurrentBranch(ctx, l.dir(), "status")
		if err != nil {
			return err
		}
		st.Branch = branch
	}
	entries, err := vcs.GitStatus(ctx, vcs.GitArgs{Dir: l.dir(), Sub: []string{"status"}})
	if err != nil {
		return fmt.Errorf("status: %w", err)
	}
	for _, e := range entries {
		st.DirtyFiles = append(st.DirtyFiles, e.Path)
	}
	st.Dirty = len(st.DirtyFiles) > 0
	return nil
}

// statusStack resolves the branches to report, bottom-up. The gt lane reports
// the whole stack; every other lane reports the branch it is standing on, which
// is the only one it can speak for. Standing on trunk there is no stack to
// report and nothing landing, so the report is the repository's alone.
func statusStack(ctx context.Context, l lane, st *vcsStatus) ([]string, gtState) {
	if st.Branch == "" || st.Branch == st.Trunk {
		return nil, nil
	}
	if !l.gt {
		return []string{st.Branch}, nil
	}
	stack, state, err := gtStackAll(ctx, l.dir(), "status")
	if err != nil {
		st.StackError = err.Error()
		return []string{st.Branch}, nil
	}
	return stack, state
}

// statusLocalBranches fills what git and gt know about each branch: which
// working copy holds it, whether gt says it sits off its parent, and how far it
// has diverged from trunk.
func statusLocalBranches(ctx context.Context, l lane, state gtState, branches []string, trunk vcs.Trunk, hasTrunk bool, st *vcsStatus) error {
	holders, err := vcs.BranchHolders(ctx, l.checkout)
	if err != nil {
		return fmt.Errorf("status: %w", err)
	}
	for _, name := range branches {
		b := statusBranch{
			Name:         name,
			Current:      name == st.Branch,
			Holder:       statusHolder(holders[name], l.checkout.Root),
			NeedsRestack: state[name].NeedsRestack,
		}
		if hasTrunk {
			d, err := statusDivergence(ctx, l, trunk, name)
			if err != nil {
				return err
			}
			b.Diverge = d
		}
		st.Branches = append(st.Branches, b)
	}
	return nil
}

// statusTrunkRef names the trunk and qualifies it into the remote-tracking ref
// rev-list must receive. A repository with no trunk ref reports no divergence
// rather than refusing the whole report.
func statusTrunkRef(ctx context.Context, l lane, st *vcsStatus) (vcs.Trunk, bool, error) {
	st.Trunk = statusGTTrunk(ctx, l, st)
	if l.kind == vcs.JJ && !l.gt {
		return vcs.Trunk{}, false, nil
	}
	remote, err := vcs.GitRemoteFor(ctx, l.dir(), st.Branch)
	if err != nil {
		return vcs.Trunk{}, false, fmt.Errorf("status: %w", err)
	}
	resolve := func() (vcs.Trunk, error) {
		if st.Trunk != "" {
			return vcs.TrunkFromName(ctx, l.dir(), remote, st.Trunk)
		}
		return vcs.ResolveTrunk(ctx, l.dir(), remote)
	}
	trunk, err := resolve()
	switch {
	case err == nil:
		st.Trunk = trunk.Name()
		return trunk, true, nil
	case errors.Is(err, vcs.ErrNoTrunk):
		return vcs.Trunk{}, false, nil
	default:
		return vcs.Trunk{}, false, fmt.Errorf("status: %w", err)
	}
}

// statusGTTrunk is the branch gt calls trunk, which outranks origin's HEAD on
// the gt lane and is empty everywhere else. State gt cannot answer for lands in
// StackError, the way ccx vcs info reports one.
func statusGTTrunk(ctx context.Context, l lane, st *vcsStatus) string {
	if !l.gt {
		return ""
	}
	state, err := gtStateQuery(ctx, l.dir(), "status")
	if err != nil {
		st.StackError = err.Error()
		return ""
	}
	trunk, err := gtTrunkBranch("status", state)
	if err != nil {
		st.StackError = err.Error()
		return ""
	}
	return trunk
}

// statusDivergence counts the symmetric difference between trunk and branch,
// which is the one rev-list shape naming both sides in a single walk.
func statusDivergence(ctx context.Context, l lane, trunk vcs.Trunk, branch string) (*statusDiverge, error) {
	span := string(trunk.Ref()) + "..." + string(vcs.LocalBranchRef(branch))
	out, err := render.RunCLI(ctx, l.dir(), "git", []string{"rev-list", "--left-right", "--count", span})
	if err != nil {
		return nil, fmt.Errorf("status: git rev-list %s: %w", span, err)
	}
	fields := strings.Fields(out)
	if len(fields) != 2 {
		return nil, fmt.Errorf("status: malformed rev-list count %q for %s", out, span)
	}
	behind, err := strconv.Atoi(fields[0])
	if err != nil {
		return nil, fmt.Errorf("status: malformed rev-list count %q for %s: %w", out, span, err)
	}
	ahead, err := strconv.Atoi(fields[1])
	if err != nil {
		return nil, fmt.Errorf("status: malformed rev-list count %q for %s: %w", out, span, err)
	}
	return &statusDiverge{Ahead: ahead, Behind: behind}, nil
}

// statusBlockers names what stands between this branch and trunk, worst first.
// A branch with an empty list is one nothing known is holding back.
func statusBlockers(b statusBranch) []string {
	var out []string
	if b.NeedsRestack {
		out = append(out, "sits off its parent — run ccx vcs stack restack")
	}
	if b.PR == nil {
		return append(out, "no pull request — run ccx vcs ship")
	}
	pr := b.PR
	if q := pr.Queue; q != nil && q.Drifted() {
		noun := plural(len(q.Dropped), "commit", "commits")
		verb := fmt.Sprintf("is holding %s, so %d %s would not land", shortSHA(q.Held), len(q.Dropped), noun)
		if pr.Merged {
			verb = fmt.Sprintf("landed %s, so %d %s never landed", shortSHA(q.Held), len(q.Dropped), noun)
		}
		out = append(out, "the merge queue "+verb)
	}
	if pr.Merged {
		return out
	}
	if pr.State != "OPEN" {
		return append(out, "the pull request is "+strings.ToLower(pr.State))
	}
	out = append(out, statusMergeBlockers(pr)...)
	for _, c := range pr.Checks {
		if c.Required && !statusCheckPassed(c.State) {
			out = append(out, "required check "+c.Name+" is "+strings.ToLower(c.State))
		}
	}
	for _, r := range pr.Reviews {
		if r.Stale && r.State == "APPROVED" {
			out = append(out, r.Author+"'s approval is stale — it was cast on "+shortSHA(r.Commit))
		}
	}
	return out
}

// statusMergeBlockers reads the pull request's own landing state.
func statusMergeBlockers(pr *statusPR) []string {
	var out []string
	if pr.Draft {
		out = append(out, "the pull request is a draft")
	}
	if !pr.HasBody {
		out = append(out, "the pull request has no body")
	}
	switch pr.Mergeable {
	case "CONFLICTING":
		out = append(out, "the branch conflicts with its base")
	case statusUnknown:
		out = append(out, "GitHub has not computed a mergeability yet")
	}
	switch pr.ReviewDecision {
	case "CHANGES_REQUESTED":
		out = append(out, "a reviewer requested changes")
	case "REVIEW_REQUIRED":
		out = append(out, "the pull request is not approved")
	}
	return out
}

// statusCheckPassed reports whether one check's state clears a merge. GitHub
// spells a skipped check as a non-failure, and a neutral one as no opinion.
func statusCheckPassed(state string) bool {
	switch state {
	case "SUCCESS", "SKIPPED", "NEUTRAL":
		return true
	default:
		return false
	}
}

// statusRequired collects the check contexts the rule covering base names. An
// empty result means either no rule or no permission to read one, which is why
// the report marks required checks rather than claiming the rest are optional.
func statusRequired(rules []statusProtectionRule, base string) []string {
	var out []string
	for _, rule := range rules {
		if ok, err := path.Match(rule.Pattern, base); err != nil || !ok {
			continue
		}
		for _, check := range rule.RequiredStatusChecks {
			out = append(out, check.Context)
		}
	}
	return out
}

func renderVcsStatus(st vcsStatus) string {
	var b strings.Builder
	line := func(label, value string) {
		fmt.Fprintf(&b, "%-*s%s\n", infoLabelWidth, label, value)
	}
	line("lane", st.Lane)
	line("root", st.Root)
	if st.Repo != "" {
		line("repo", st.Repo)
	}
	if st.Trunk != "" {
		line("trunk", st.Trunk)
	}
	line("dirty", statusDirtyValue(st))
	if len(st.Required) > 0 {
		line("required", strings.Join(st.Required, shipSep))
	}
	if st.StackError != "" {
		line("stack", st.StackError)
	}
	if len(st.Branches) == 0 && st.Branch != "" && st.Branch == st.Trunk {
		line("stack", st.Branch+" is trunk — check out a branch of the stack you mean")
	}
	if st.PRError != "" {
		line("github", st.PRError)
	}
	for _, branch := range st.Branches {
		b.WriteString("\n")
		renderStatusBranch(line, branch)
	}
	return b.String()
}

func renderStatusBranch(line func(string, string), branch statusBranch) {
	line("branch", statusBranchValue(branch))
	if pr := branch.PR; pr != nil {
		line("pr", statusPRValue(*pr))
		// A closed pull request's mergeability tracks a base it will never
		// merge into, so GitHub keeps answering CONFLICTING long after it landed.
		if pr.State == "OPEN" {
			line("merge", statusMergeValue(*pr))
		}
		if pr.Queue != nil {
			line("queue", mqValue(*pr.Queue, pr.Head))
			for _, c := range pr.Queue.Dropped {
				line("dropped", shortSHA(c.OID)+" "+c.Subject)
			}
		}
		if len(pr.Checks) > 0 {
			line("checks", statusChecksValue(pr.Checks))
		}
		if len(pr.Reviews) > 0 {
			line("reviews", statusReviewsValue(pr.Reviews, pr.Head))
		}
	}
	for _, blocker := range branch.Blockers {
		line("blocked", blocker)
	}
}

// statusDirtyValue counts the uncommitted paths and names the first few, so a
// wide working copy cannot crowd the branches under it out of the report.
func statusDirtyValue(st vcsStatus) string {
	if !st.Dirty {
		return "no"
	}
	noun := plural(len(st.DirtyFiles), "file", "files")
	named := st.DirtyFiles
	rest := ""
	if len(named) > statusDirtyPaths {
		named, rest = named[:statusDirtyPaths], fmt.Sprintf(", +%d more", len(st.DirtyFiles)-statusDirtyPaths)
	}
	return fmt.Sprintf("yes (%d %s)%s%s%s", len(st.DirtyFiles), noun, shipSep, strings.Join(named, ", "), rest)
}

// statusHolder names the working copy holding a branch the way stack list does,
// so "here" and a sibling's path read the same across both commands.
func statusHolder(holder, root string) string {
	switch holder {
	case "":
		return "no working copy"
	case root:
		return "here"
	default:
		return holder
	}
}

func statusBranchValue(branch statusBranch) string {
	segs := []string{branch.Name, branch.Holder}
	if branch.Current {
		segs = append(segs, "current")
	}
	if d := branch.Diverge; d != nil {
		segs = append(segs, fmt.Sprintf("ahead %d", d.Ahead), fmt.Sprintf("behind %d", d.Behind))
	}
	if branch.NeedsRestack {
		segs = append(segs, "needs restack")
	}
	return strings.Join(segs, shipSep)
}

func statusPRValue(pr statusPR) string {
	end := strings.ToLower(pr.State)
	if pr.Merged {
		end = "merged"
	}
	segs := []string{fmt.Sprintf("#%d", pr.Number), end}
	if pr.Draft {
		segs = append(segs, "draft")
	}
	if !pr.HasBody {
		segs = append(segs, "no body")
	}
	return strings.Join(append(segs, shortSHA(pr.Head), pr.URL), shipSep)
}

func statusMergeValue(pr statusPR) string {
	segs := []string{"mergeable " + strings.ToLower(pr.Mergeable)}
	if pr.MergeState != "" {
		segs = append(segs, "state "+strings.ToLower(pr.MergeState))
	}
	if pr.ReviewDecision != "" {
		segs = append(segs, "review "+strings.ToLower(pr.ReviewDecision))
	}
	if pr.ChecksState != "" {
		segs = append(segs, "checks "+strings.ToLower(pr.ChecksState))
	}
	if len(pr.Labels) > 0 {
		segs = append(segs, "labels "+strings.Join(pr.Labels, ", "))
	}
	return strings.Join(segs, shipSep)
}

func statusChecksValue(checks []statusCheck) string {
	segs := make([]string, 0, len(checks))
	for _, c := range checks {
		seg := c.Name + " " + strings.ToLower(c.State)
		if c.Required {
			seg += " (required)"
		}
		segs = append(segs, seg)
	}
	return strings.Join(segs, shipSep)
}

func statusReviewsValue(reviews []statusReview, head string) string {
	segs := make([]string, 0, len(reviews))
	for _, r := range reviews {
		seg := r.Author + " " + strings.ToLower(r.State)
		if r.Bot {
			seg += " (bot)"
		}
		if r.Stale {
			seg += " on " + shortSHA(r.Commit) + ", stale against " + shortSHA(head)
		}
		segs = append(segs, seg)
	}
	return strings.Join(segs, shipSep)
}

// statusGH reports whether gh is on PATH to ask GitHub with.
func statusGH() bool {
	_, err := exec.LookPath("gh")
	return err == nil
}
