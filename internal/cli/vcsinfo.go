package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"

	"github.com/spf13/cobra"

	"github.com/yasyf/cc-context/internal/render"
	"github.com/yasyf/cc-context/internal/vcs"
)

// infoLabelWidth left-aligns every human report label into one column.
const infoLabelWidth = 12

// infoDeclinedPrefix leads lane_reason, so the note reads as a verdict on the
// graphite lane rather than a bare quote from whatever declined it.
const infoDeclinedPrefix = "graphite declined: "

// vcsInfo is the lane a mutating VCS command would take in this working copy
// and the inputs the gate weighed reaching it. It reports; it never chooses.
type vcsInfo struct {
	Lane          string        `json:"lane"`
	LaneReason    string        `json:"lane_reason,omitempty"`
	VCS           string        `json:"vcs"`
	Root          string        `json:"root"`
	Worktree      *worktreeInfo `json:"worktree,omitempty"`
	CheckoutError string        `json:"checkout_error,omitempty"`
	Branch        string        `json:"branch"`
	BranchKind    string        `json:"branch_kind"`
	Detached      bool          `json:"detached"`
	Trunk         string        `json:"trunk"`
	TrunkHolder   string        `json:"trunk_holder,omitempty"`
	Dirty         bool          `json:"dirty"`
	DirtyFiles    int           `json:"dirty_files"`
	Graphite      graphiteInfo  `json:"graphite"`
	GitHub        *vcs.Repo     `json:"github"`
	GitHubError   string        `json:"github_error,omitempty"`
	Downstack     []stackEntry  `json:"downstack,omitempty"`
}

// graphiteInfo is what the graphite lane has available here. Reachable carries
// the probe's own verdict rather than a boolean, so a repo Graphite refuses, one
// nobody could get an answer about, and one never probed at all — no live
// config, or no gt to ask with, which leaves it empty — all stay distinct.
// Reason explains anything but a yes.
type graphiteInfo struct {
	Config    bool   `json:"config"`
	CLI       bool   `json:"cli"`
	Version   string `json:"version,omitempty"`
	Reachable string `json:"reachable,omitempty"`
	Reason    string `json:"reason,omitempty"`
	Untracked bool   `json:"untracked,omitempty"`
	// StackError is graphite state info could not resolve into a trunk and a
	// downstack. It is reported rather than fatal because an unparseable state
	// and an unresolvable parent chain are what someone runs info to find out.
	StackError string `json:"stack_error,omitempty"`
}

// worktreeInfo places a linked working copy inside its repository: which of the
// shapes git and jj can attach it by, where the repository's own working copy
// sits, and the admin dir every sibling contends over.
type worktreeInfo struct {
	Shape     string `json:"shape"`
	MainRoot  string `json:"main_root"`
	CommonDir string `json:"common_dir,omitempty"`
	RepoKey   string `json:"repo_key"`
}

// stackEntry is one branch in the graphite downstack and the pull request it
// maps to, so a caller can tell which PRs a submit would leave bodyless.
type stackEntry struct {
	Branch  string `json:"branch"`
	PR      int    `json:"pr,omitempty"`
	URL     string `json:"url,omitempty"`
	HasBody bool   `json:"has_body"`
}

type vcsInfoOpts struct {
	json    bool
	refresh bool
	noGT    bool
}

func newVcsInfoCmd() *cobra.Command {
	var o vcsInfoOpts
	cmd := &cobra.Command{
		Use:     "info",
		Aliases: []string{"lane"},
		Short:   "Report the lane a mutating VCS command would take here, and why",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runVcsInfo(cmd, o)
		},
	}
	cmd.Flags().BoolVar(&o.json, "json", false, "emit the report as JSON")
	cmd.Flags().BoolVar(&o.refresh, "refresh", false, "refetch the cached GitHub metadata and graphite reachability")
	cmd.Flags().BoolVar(&o.noGT, "no-gt", false, "ignore a live graphite config and fall back to the jj/git detection")
	return cmd
}

func runVcsInfo(cmd *cobra.Command, o vcsInfoOpts) error {
	ctx := cmd.Context()
	l, err := resolveLaneReport(ctx, "info", workingDir(), o.noGT, o.refresh)
	if err != nil {
		return err
	}
	info, err := collectVcsInfo(ctx, l, o)
	if err != nil {
		return err
	}
	if o.json {
		data, err := json.MarshalIndent(info, "", "  ")
		if err != nil {
			return fmt.Errorf("info: marshal report: %w", err)
		}
		cmd.Println(string(data))
		return nil
	}
	cmd.Print(renderVcsInfo(info))
	return nil
}

func collectVcsInfo(ctx context.Context, l lane, o vcsInfoOpts) (vcsInfo, error) {
	info := vcsInfo{
		Lane:       kindLabel(l.kind),
		VCS:        kindLabel(l.kind),
		Root:       l.root,
		BranchKind: "branch",
	}
	if l.gt {
		info.Lane = "gt"
	}
	if l.note != "" {
		info.LaneReason = infoDeclinedPrefix + l.note
	}
	if l.kind == vcs.JJ {
		info.BranchKind = "bookmark"
	}
	if l.broken != nil {
		info.CheckoutError = l.broken.Error()
		return info, nil
	}

	graphite, err := infoGraphite(ctx, l, o)
	if err != nil {
		return vcsInfo{}, err
	}
	info.Graphite = graphite

	switch {
	case l.gt:
		err = infoGT(ctx, l, &info)
	case l.kind == vcs.JJ:
		err = infoJJ(ctx, l, &info)
	default:
		err = infoGit(ctx, l, &info)
	}
	if err != nil {
		return vcsInfo{}, err
	}
	if err := infoWorktree(ctx, l, &info); err != nil {
		return vcsInfo{}, err
	}
	if err := infoGitHub(ctx, l, o, &info); err != nil {
		return vcsInfo{}, err
	}
	return info, nil
}

// infoGraphite reports what the graphite lane has here. gt is only asked
// anything when the config is live, so a repo Graphite has never seen costs no
// subprocess and leaves Reachable unprobed. The gates probe only on the way to
// the gt lane, so a repo that declined earlier — --no-gt, ccx.nogt, someone
// else's repo — is probed here instead: what graphite has available is worth
// reporting even where the lane refused to use it.
func infoGraphite(ctx context.Context, l lane, o vcsInfoOpts) (graphiteInfo, error) {
	config, err := vcs.GraphiteRepo(l.checkout)
	if err != nil {
		return graphiteInfo{}, fmt.Errorf("info: %w", err)
	}
	g := graphiteInfo{Config: config}
	if !g.Config {
		return g, nil
	}
	if _, err := exec.LookPath("gt"); err != nil {
		return g, nil
	}
	g.CLI = true
	out, err := render.RunCLIDir(ctx, l.root, "gt", []string{"--version"})
	if err != nil {
		return graphiteInfo{}, fmt.Errorf("info: gt --version: %w", err)
	}
	g.Version = strings.TrimSpace(out)
	if l.verdict != "" {
		g.Reachable, g.Reason = string(l.verdict), l.note
		return g, nil
	}
	verdict, why, err := gtReachability(ctx, l.root, o.refresh)
	if err != nil {
		return graphiteInfo{}, err
	}
	g.Reachable, g.Reason = string(verdict), why
	return g, nil
}

// infoGT reports the gt lane, whose trunk is the branch gt state marks as one,
// and whose downstack is what a gt submit would publish.
func infoGT(ctx context.Context, l lane, info *vcsInfo) error {
	if err := infoGitBranch(ctx, l.root, info); err != nil {
		return err
	}
	if err := infoGitDirty(ctx, l.root, info); err != nil {
		return err
	}
	infoGTStack(ctx, l, info)
	return nil
}

// infoGTStack fills the trunk and the downstack a gt submit would publish.
// Graphite state it cannot resolve lands in StackError rather than aborting the
// report: an unresolvable stack is a diagnosis, and refusing to print the branch,
// dirtiness, and repository around it withholds the rest of the answer too.
func infoGTStack(ctx context.Context, l lane, info *vcsInfo) {
	state, err := gtStateQuery(ctx, "info")
	if err != nil {
		info.Graphite.StackError = err.Error()
		return
	}
	trunk, err := gtTrunkBranch("info", state)
	if err != nil {
		info.Graphite.StackError = err.Error()
		return
	}
	info.Trunk = trunk
	if info.Branch == "" || info.Branch == trunk {
		return
	}
	chain, err := gtDownstack("info", state, info.Branch, trunk)
	var untracked *errGTUntracked
	switch {
	case errors.As(err, &untracked):
		info.Graphite.Untracked = true
	case err != nil:
		info.Graphite.StackError = err.Error()
	default:
		info.Downstack = infoDownstack(ctx, l.root, chain)
	}
}

func infoGit(ctx context.Context, l lane, info *vcsInfo) error {
	if err := infoGitBranch(ctx, l.root, info); err != nil {
		return err
	}
	remote, err := gitRemoteFor(ctx, "info", info.Branch)
	if err != nil {
		return err
	}
	trunk, err := gitRemoteTrunk(ctx, "info", remote)
	if err != nil {
		return err
	}
	info.Trunk = trunk
	return infoGitDirty(ctx, l.root, info)
}

// infoJJ reports the bookmark ship would target — the same nearest-bookmark
// revset shipPreflightJJ resolves — and the trunk bookmark, which is empty
// unless it resolves to exactly one name.
func infoJJ(ctx context.Context, l lane, info *vcsInfo) error {
	names, err := jjBookmarkNames(ctx, "info", jjNearestBookmarkRevset)
	if err != nil {
		return err
	}
	info.Branch = strings.Join(names, " ")
	trunkNames, err := jjTrunkBookmarkNames(ctx, "info")
	if err != nil {
		return err
	}
	if len(trunkNames) == 1 {
		info.Trunk = trunkNames[0]
	}
	out, err := render.RunCLIDir(ctx, l.root, "jj", []string{"diff", "--name-only"})
	if err != nil {
		return fmt.Errorf("info: jj diff --name-only: %w", err)
	}
	info.DirtyFiles = countInfoLines(out)
	info.Dirty = info.DirtyFiles > 0
	return nil
}

// infoGitBranch reads the checked-out branch. An empty answer is a detached
// HEAD, which info reports rather than refusing — unlike shipPreflightGit,
// which has a push to protect.
func infoGitBranch(ctx context.Context, root string, info *vcsInfo) error {
	out, err := render.RunCLIDir(ctx, root, "git", []string{"branch", "--show-current"})
	if err != nil {
		return fmt.Errorf("info: git branch --show-current: %w", err)
	}
	info.Branch = strings.TrimSpace(out)
	info.Detached = info.Branch == ""
	return nil
}

func infoGitDirty(ctx context.Context, root string, info *vcsInfo) error {
	out, err := render.RunCLIDir(ctx, root, "git", []string{"status", "--porcelain"})
	if err != nil {
		return fmt.Errorf("info: git status --porcelain: %w", err)
	}
	info.DirtyFiles = countInfoLines(out)
	info.Dirty = info.DirtyFiles > 0
	return nil
}

// infoDownstack resolves chain — gt's branch-first walk — into base-first
// entries carrying each branch's PR, in one round trip for the whole stack. A
// branch with no PR, no gh to ask with, or a query GitHub refused still reports
// its name: the caller needs the whole submit set.
func infoDownstack(ctx context.Context, root string, chain []string) []stackEntry {
	entries := make([]stackEntry, 0, len(chain))
	for i := len(chain) - 1; i >= 0; i-- {
		entries = append(entries, stackEntry{Branch: chain[i]})
	}
	if len(entries) == 0 {
		return entries
	}
	if _, err := exec.LookPath("gh"); err != nil {
		return entries
	}
	resolveDownstackPRs(ctx, root, entries)
	return entries
}

// resolveDownstackPRs fills every entry's pull request from one gh api graphql
// call, which is the only batch gh exposes: pr list --head takes a single branch,
// so a list-based batch would have to over-fetch and filter, making --limit a
// correctness knob. {owner} and {repo} are gh's own placeholders for the
// repository of the working directory, so the batch needs no metadata lookup.
func resolveDownstackPRs(ctx context.Context, root string, entries []stackEntry) {
	argv := make([]string, 0, 8+2*len(entries))
	argv = append(argv, "api", "graphql", "-F", "owner={owner}", "-F", "repo={repo}")
	for i, entry := range entries {
		argv = append(argv, "-f", downstackPRAlias(i)+"="+entry.Branch)
	}
	argv = append(argv, "-f", "query="+downstackPRQuery(len(entries)))
	out, err := render.RunCLIDir(ctx, root, "gh", argv)
	if err != nil {
		return
	}
	var resp struct {
		Data struct {
			Repository map[string]struct {
				Nodes []struct {
					Number int    `json:"number"`
					URL    string `json:"url"`
					Body   string `json:"body"`
				} `json:"nodes"`
			} `json:"repository"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &resp); err != nil {
		return
	}
	for i := range entries {
		nodes := resp.Data.Repository[downstackPRAlias(i)].Nodes
		if len(nodes) == 0 {
			continue
		}
		entries[i].PR = nodes[0].Number
		entries[i].URL = nodes[0].URL
		entries[i].HasBody = strings.TrimSpace(nodes[0].Body) != ""
	}
}

// downstackPRAlias names one branch's field in the batched query. A GraphQL
// alias takes neither "/" nor "." nor "-", which branch names do, so the
// branch's position stands in for its name.
func downstackPRAlias(i int) string {
	return fmt.Sprintf("b%d", i)
}

// downstackPRQuery renders one aliased pullRequests field per branch. It names
// no state filter, because gh pr view — the per-branch call this replaced — has
// none either and resolves a merged pull request just as happily; and it orders
// descending because that is the one gh picks. A branch resubmitted after its
// first pull request closed carries two, and the oldest is the one ship must
// never write a body onto.
func downstackPRQuery(n int) string {
	decls := make([]string, 0, n+2)
	decls = append(decls, "$owner: String!", "$repo: String!")
	var fields strings.Builder
	for i := range n {
		alias := downstackPRAlias(i)
		decls = append(decls, "$"+alias+": String!")
		fmt.Fprintf(&fields, "    %s: pullRequests(headRefName: $%s, first: 1, orderBy: {field: CREATED_AT, direction: DESC}) { nodes { number url body } }\n", alias, alias)
	}
	return fmt.Sprintf("query(%s) {\n  repository(owner: $owner, name: $repo) {\n%s  }\n}", strings.Join(decls, ", "), fields.String())
}

// infoWorktree places this working copy inside its repository and names the
// checkout currently holding trunk — the answer to a gt restack that skipped a
// branch "because it is checked out in worktree W" and still exited 0. It reads
// the resolved checkout, not the process cwd, which in a linked worktree need not
// be the same tree. A trunk no checkout holds is normal — a detached main working
// copy under colocated jj — so an absent holder is silence, not a failure.
func infoWorktree(ctx context.Context, l lane, info *vcsInfo) error {
	ck := l.checkout
	if ck.Linked() {
		info.Worktree = &worktreeInfo{
			Shape:     infoShape(ck.Shape),
			MainRoot:  ck.MainRoot,
			CommonDir: ck.CommonDir,
			RepoKey:   ck.RepoKey(),
		}
	}
	if ck.CommonDir == "" || info.Trunk == "" {
		return nil
	}
	holders, err := vcs.BranchHolders(ctx, ck)
	if err != nil {
		return fmt.Errorf("info: %w", err)
	}
	info.TrunkHolder = holders[info.Trunk]
	return nil
}

// infoShape names how a working copy is attached to its repository.
func infoShape(shape vcs.Shape) string {
	switch shape {
	case vcs.ShapeMain:
		return "main"
	case vcs.ShapeGitWorktree:
		return "git worktree"
	case vcs.ShapeJJWorkspace:
		return "jj workspace"
	case vcs.ShapeColocatedWorktree:
		return "colocated worktree"
	default:
		panic(fmt.Sprintf("cli: no label for vcs shape %d", shape))
	}
}

// infoGitHub attaches the repository's GitHub metadata, reusing the record the
// gates read when they got as far as reading one. Every reason the answer is
// unknowable — gh off PATH, signed out, no GitHub remote, offline — lands in
// GitHubError instead of failing the report.
func infoGitHub(ctx context.Context, l lane, o vcsInfoOpts, info *vcsInfo) error {
	if l.repo != nil {
		info.GitHub = l.repo
		return nil
	}
	repo, err := vcs.LookupRepo(ctx, l.root, o.refresh)
	switch {
	case err == nil:
		info.GitHub = &repo
	case errors.Is(err, vcs.ErrNoGitHub):
		info.GitHubError = err.Error()
	default:
		return err
	}
	return nil
}

func countInfoLines(out string) int {
	n := 0
	for _, line := range strings.Split(out, "\n") {
		if strings.TrimSpace(line) != "" {
			n++
		}
	}
	return n
}

func renderVcsInfo(info vcsInfo) string {
	var b strings.Builder
	line := func(label, value string) {
		fmt.Fprintf(&b, "%-*s%s\n", infoLabelWidth, label, value)
	}
	line("lane", info.Lane)
	if info.LaneReason != "" {
		line("lane-note", info.LaneReason)
	}
	line("vcs", info.VCS)
	line("root", info.Root)
	if info.CheckoutError != "" {
		line("checkout", info.CheckoutError)
		return b.String()
	}
	if wt := info.Worktree; wt != nil {
		line("shape", wt.Shape)
		if wt.MainRoot != "" {
			line("main-root", wt.MainRoot)
		}
		if wt.CommonDir != "" {
			line("common-dir", wt.CommonDir)
		}
		line("repo-key", wt.RepoKey)
	}
	switch {
	case info.Detached:
		line("branch", "(detached)")
	case info.Branch != "":
		line("branch", info.Branch)
	}
	if info.Trunk != "" {
		line("trunk", info.Trunk)
	}
	if info.TrunkHolder != "" {
		line("trunk-held", info.TrunkHolder)
	}
	line("dirty", infoDirtyValue(info))
	if info.Graphite.Config {
		line("graphite", infoGraphiteValue(info.Graphite, info.Trunk))
	}
	if info.Graphite.StackError != "" {
		line("stack", info.Graphite.StackError)
	}
	switch {
	case info.GitHub != nil:
		line("repo", info.GitHub.NameWithOwner)
		line("visibility", infoVisibility(*info.GitHub))
		line("permission", info.GitHub.ViewerPermission)
		line("viewer", fmt.Sprintf("%s (affiliated: %s)", info.GitHub.ViewerLogin, infoAffiliation(*info.GitHub)))
	case info.GitHubError != "":
		line("github", strings.TrimPrefix(info.GitHubError, vcs.ErrNoGitHub.Error()+": "))
	}
	if len(info.Downstack) > 0 {
		line("downstack", infoDownstackValue(info.Downstack))
	}
	return b.String()
}

func infoDirtyValue(info vcsInfo) string {
	if !info.Dirty {
		return "no"
	}
	noun := "files"
	if info.DirtyFiles == 1 {
		noun = "file"
	}
	return fmt.Sprintf("yes (%d %s)", info.DirtyFiles, noun)
}

func infoGraphiteValue(g graphiteInfo, trunk string) string {
	segs := []string{"config live"}
	if g.CLI {
		seg := "gt"
		if g.Version != "" {
			seg += " " + g.Version
		}
		segs = append(segs, seg)
	}
	switch gtVerdict(g.Reachable) {
	case gtVerdictOK:
		segs = append(segs, "reachable")
	case gtVerdictDenied:
		segs = append(segs, "unreachable")
	case gtVerdictUnknown:
		segs = append(segs, "reachability unknown ("+g.Reason+")")
	}
	if g.Untracked {
		segs = append(segs, "branch untracked (gt track --parent "+trunk+")")
	}
	return strings.Join(segs, shipSep)
}

func infoVisibility(repo vcs.Repo) string {
	if repo.IsPrivate {
		return "private"
	}
	return "public"
}

// infoAffiliation names how the viewer relates to the repository's owner: their
// own account, an organization they belong to, or neither.
func infoAffiliation(repo vcs.Repo) string {
	switch {
	case repo.Personal():
		return "self"
	case repo.Affiliated:
		return "org"
	default:
		return "no"
	}
}

func infoDownstackValue(entries []stackEntry) string {
	segs := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.PR == 0 {
			segs = append(segs, e.Branch)
			continue
		}
		body := "no body"
		if e.HasBody {
			body = "body"
		}
		segs = append(segs, fmt.Sprintf("%s → PR #%d (%s)", e.Branch, e.PR, body))
	}
	return strings.Join(segs, shipSep)
}
