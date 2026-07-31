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
	Lane        string       `json:"lane"`
	LaneReason  string       `json:"lane_reason,omitempty"`
	VCS         string       `json:"vcs"`
	Root        string       `json:"root"`
	Branch      string       `json:"branch"`
	BranchKind  string       `json:"branch_kind"`
	Detached    bool         `json:"detached"`
	Trunk       string       `json:"trunk"`
	Dirty       bool         `json:"dirty"`
	DirtyFiles  int          `json:"dirty_files"`
	Graphite    graphiteInfo `json:"graphite"`
	GitHub      *vcs.Repo    `json:"github"`
	GitHubError string       `json:"github_error,omitempty"`
	Downstack   []stackEntry `json:"downstack,omitempty"`
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
	l, err := resolveLaneRefresh(ctx, "info", workingDir(), o.noGT, o.refresh)
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
	config, err := vcs.GraphiteRepo(ctx, l.root)
	if err != nil {
		return graphiteInfo{}, err
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
	state, err := gtStateQuery(ctx, "info")
	if err != nil {
		return err
	}
	trunk, err := gtTrunkBranch("info", state)
	if err != nil {
		return err
	}
	info.Trunk = trunk
	if err := infoGitDirty(ctx, l.root, info); err != nil {
		return err
	}
	if info.Branch == "" || info.Branch == trunk {
		return nil
	}
	chain, err := gtDownstack("info", state, info.Branch, trunk)
	var untracked *errGTUntracked
	if errors.As(err, &untracked) {
		info.Graphite.Untracked = true
		return nil
	}
	if err != nil {
		return err
	}
	info.Downstack = infoDownstack(ctx, l.root, chain)
	return nil
}

func infoGit(ctx context.Context, l lane, info *vcsInfo) error {
	if err := infoGitBranch(ctx, l.root, info); err != nil {
		return err
	}
	remote, err := gitRemoteFor(ctx, info.Branch)
	if err != nil {
		return err
	}
	trunk, err := gitRemoteTrunk(ctx, remote)
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
// entries carrying each branch's PR. A branch with no PR, or no gh to ask
// with, still reports its name: the caller needs the whole submit set.
func infoDownstack(ctx context.Context, root string, chain []string) []stackEntry {
	_, ghErr := exec.LookPath("gh")
	entries := make([]stackEntry, 0, len(chain))
	for i := len(chain) - 1; i >= 0; i-- {
		entry := stackEntry{Branch: chain[i]}
		if ghErr == nil {
			infoBranchPR(ctx, root, &entry)
		}
		entries = append(entries, entry)
	}
	return entries
}

func infoBranchPR(ctx context.Context, root string, entry *stackEntry) {
	out, err := render.RunCLIDir(ctx, root, "gh", []string{"pr", "view", entry.Branch, "--json", "number,url,body"})
	if err != nil {
		return
	}
	var pr struct {
		Number int    `json:"number"`
		URL    string `json:"url"`
		Body   string `json:"body"`
	}
	if err := json.Unmarshal([]byte(out), &pr); err != nil {
		return
	}
	entry.PR = pr.Number
	entry.URL = pr.URL
	entry.HasBody = strings.TrimSpace(pr.Body) != ""
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
	switch {
	case info.Detached:
		line("branch", "(detached)")
	case info.Branch != "":
		line("branch", info.Branch)
	}
	if info.Trunk != "" {
		line("trunk", info.Trunk)
	}
	line("dirty", infoDirtyValue(info))
	if info.Graphite.Config {
		line("graphite", infoGraphiteValue(info.Graphite, info.Trunk))
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
