package cli

import (
	"cmp"
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/yasyf/cc-context/internal/gtapi"
	"github.com/yasyf/cc-context/internal/gtmeta"
	"github.com/yasyf/cc-context/internal/nativemeta"
	"github.com/yasyf/cc-context/internal/render"
	"github.com/yasyf/cc-context/internal/vcs"
)

type (
	stackProviderKey struct{}
	stackParentKey   struct{}
	stackHereKey     struct{}
)

// stackHere is the branch a stack command run from a detached HEAD stands on,
// named by --branch, and the root of the working copy it stands in.
type stackHere struct {
	root, branch string
}

func withStackProvider(ctx context.Context, native bool) context.Context {
	provider := "graphite"
	if native {
		provider = "github"
	}
	return gtmeta.WithProvider(context.WithValue(ctx, stackProviderKey{}, native), provider)
}

func nativeProvider(ctx context.Context, dir render.Dir) (bool, error) {
	if native, known := ctx.Value(stackProviderKey{}).(bool); known {
		return native, nil
	}
	if vcs.Detect(string(dir)) == vcs.None {
		return true, nil
	}
	ck, err := vcs.ResolveCheckout(string(dir))
	if err != nil {
		return false, err
	}
	branch, err := gitCurrentBranch(ctx, dir, "stack provider")
	if err != nil {
		return false, err
	}
	provider, err := gtmeta.Provider(ctx, ck.CommonDir, branch)
	return provider == "github", err
}

func nativeStackRun(ctx context.Context, dir render.Dir, argv []string, extraEnv ...string) (gtResult, error) {
	var output strings.Builder
	run := func(args ...string) (string, error) {
		out, err := render.RunCLIEnv(ctx, dir, "git", args, extraEnv)
		output.WriteString(out)
		return strings.TrimSpace(out), err
	}
	value := func(flag string) string {
		for i := 1; i+1 < len(argv); i++ {
			if argv[i] == flag {
				return argv[i+1]
			}
		}
		return ""
	}
	common, err := gtCommonDir(ctx, dir, "native stack")
	if err != nil {
		return gtResult{}, err
	}
	if argv[0] == "init" {
		return gtResult{}, nativemeta.Init(ctx, common, value("--trunk"))
	}
	trunk, err := nativemeta.ReadTrunk(ctx, common)
	if err != nil {
		return gtResult{}, err
	}
	branch, err := gitCurrentBranch(ctx, dir, "native stack")
	if err != nil {
		return gtResult{}, err
	}
	parent := value("--parent")
	switch argv[0] {
	case "create":
		parent = cmp.Or(value("--onto"), branch)
		branch = argv[1]
		args := []string{"switch", "-c", branch}
		if parent != "" {
			args = append(args, parent)
		}
		if _, err = run(args...); err != nil {
			return gtResult{Output: output.String()}, err
		}
		args = []string{"commit", "-m", value("-m")}
		if slices.Contains(argv, "--no-verify") {
			args = append(args, "--no-verify")
		}
		if _, err = run(args...); err != nil {
			return gtResult{Output: output.String()}, err
		}
	case "track":
		if len(argv) > 1 && !strings.HasPrefix(argv[1], "-") {
			branch = argv[1]
		}
	default:
		return gtResult{}, fmt.Errorf("native stack: unsupported operation %q", argv[0])
	}
	if parent == "" {
		parent = trunk
	}
	head, err := run("rev-parse", "--verify", "refs/heads/"+branch)
	if err != nil {
		return gtResult{Output: output.String()}, err
	}
	base, err := run("merge-base", parent, branch)
	if err != nil {
		return gtResult{Output: output.String()}, err
	}
	err = nativemeta.AdoptRoot(ctx, common, branch, parent, base, head)
	return gtResult{Output: output.String()}, err
}

func nativeRefuseQueuedChanges(ctx context.Context, client *gtapi.Client, owner, name, prefix string, plan []gtSubmitBranch, known map[string]gtapi.PullRequestInfo) error {
	var numbers []int
	for _, branch := range plan {
		if pr, found := known[branch.name]; found && (branch.head != pr.Newest().HeadSha || branch.base != pr.BaseRefName) {
			numbers = append(numbers, pr.PRNumber)
		}
	}
	if len(numbers) == 0 {
		return nil
	}
	statuses, err := client.MergeabilityStatuses(ctx, owner, name, numbers)
	if err != nil {
		return fmt.Errorf("%s: read the merge queue before changing published pull requests: %w", prefix, err)
	}
	var queued []string
	for _, number := range numbers {
		status, known := statuses[number]
		if !known {
			return fmt.Errorf("%s: merge queue state of #%d is unknown; cannot change its published head or base", prefix, number)
		}
		if gtapi.Restacking(status) {
			queued = append(queued, fmt.Sprintf("#%d", number))
		}
	}
	if len(queued) > 0 {
		return fmt.Errorf("%s: %s queued; dequeue before changing their published heads or bases", prefix, strings.Join(queued, ", "))
	}
	return nil
}

func gtAPIForRepo(ctx context.Context, repo string) *gtapi.Client {
	if client, ok := ctx.Value(gtAPIKey{}).(*gtapi.Client); ok {
		return client
	}
	remote, err := render.RunCLI(ctx, render.Dir(workingDir(ctx)), "git", []string{"remote", "get-url", "origin"})
	if err == nil {
		canonical, parseErr := thinCanonicalRemote(strings.TrimSpace(remote))
		if parseErr == nil && strings.EqualFold(canonical, "github.com/"+repo) {
			client := gtAPI(ctx)
			checkout, err := vcs.ResolveCheckout(workingDir(ctx))
			if err != nil {
				return client.WithProviderError(err)
			}
			legacy, err := vcs.GraphiteRepo(checkout)
			if err != nil {
				return client.WithProviderError(err)
			}
			if !legacy {
				return client
			}
			owners, err := gtmeta.BranchProviders(ctx, checkout.CommonDir)
			if err != nil {
				return client.WithProviderError(err)
			}
			return client.WithBranchProviders(owners)
		}
	}
	return gtAPINativeDefault()
}

func nativeReadTracking(ctx context.Context, l lane, run *stackRebaseRun, live []string, prs map[string]*stackPR) ([]stackUntracked, error) {
	var numbers []int
	for _, name := range live {
		if pr := prs[name]; pr != nil && pr.State == "OPEN" {
			numbers = append(numbers, pr.Number)
		}
	}
	if len(numbers) == 0 {
		return nil, nil
	}
	owner, repo, err := gtRepoOwnerName(ctx, l, stackRebasePrefix)
	if err != nil {
		return nil, err
	}
	infos, err := gtAPI(ctx).PullRequestInfo(ctx, gtapi.PullRequestInfoRequest{RepoOwner: owner, RepoName: repo, PRNumbers: numbers, Callsite: "ccx"})
	if err != nil {
		return nil, err
	}
	byNumber := make(map[int]gtapi.PullRequestInfo, len(infos))
	for _, info := range infos {
		byNumber[info.PRNumber] = info
	}
	var untracked []stackUntracked
	for _, name := range live {
		pr := prs[name]
		if pr == nil || pr.State != "OPEN" {
			continue
		}
		branch := run.branch(name)
		info, found := byNumber[pr.Number]
		if found && nativeStackTracksBranch(info, branch, run.Trunk, prs[branch.Parent]) {
			continue
		}
		untracked = append(untracked, stackUntracked{Branch: name, PR: pr.Number, Parent: branch.Parent, Head: branch.NewHead, Stale: info.BaseRefName})
	}
	return untracked, nil
}

func nativeStackTracksBranch(info gtapi.PullRequestInfo, branch *stackRebaseBranch, trunk string, parent *stackPR) bool {
	if info.State != gtapi.PROpen || info.HeadRefName != branch.Name || info.Newest().HeadSha != branch.NewHead || info.BaseRefName != branch.Parent {
		return false
	}
	if branch.Parent == trunk {
		return true
	}
	return info.StackNumber > 0 && parent != nil && info.DependentPRNumber == parent.Number
}

func nativeQueueProbe(ctx context.Context, l lane) *statusProbe {
	probeCtx, cancel := context.WithTimeout(ctx, mqProbeTimeout)
	defer cancel()
	owner, repo, err := gtRepoOwnerName(probeCtx, l, "queue status")
	if err != nil {
		return &statusProbe{Verdict: queueUnknown, Detail: err.Error()}
	}
	branch, err := gitCurrentBranch(probeCtx, l.dir(), "queue status")
	if err != nil {
		return &statusProbe{Verdict: queueUnknown, Detail: err.Error()}
	}
	client := gtAPI(probeCtx)
	infos, err := client.PullRequestInfo(probeCtx, gtapi.PullRequestInfoRequest{RepoOwner: owner, RepoName: repo, PRHeadRefNames: []string{branch}, Callsite: "ccx"})
	if err != nil {
		return &statusProbe{Verdict: queueUnknown, Detail: err.Error()}
	}
	var numbers []int
	for _, info := range infos {
		if info.State == gtapi.PRMerged {
			head, err := gitRevParse(probeCtx, l.dir(), "queue status", "HEAD")
			if err != nil {
				return &statusProbe{Verdict: queueUnknown, Detail: err.Error()}
			}
			if info.Newest().HeadSha != head {
				return &statusProbe{Verdict: queueUnknown, Detail: fmt.Sprintf("merged PR #%d describes an earlier head of %s", info.PRNumber, branch)}
			}
			return &statusProbe{Verdict: queueMerged}
		}
		if info.State == gtapi.PROpen {
			numbers = append(numbers, info.PRNumber)
		}
	}
	statuses, err := client.MergeabilityStatuses(probeCtx, owner, repo, numbers)
	if err != nil {
		return &statusProbe{Verdict: queueUnknown, Detail: err.Error()}
	}
	for _, number := range numbers {
		state, known := statuses[number]
		if !known {
			return &statusProbe{Verdict: queueUnknown, Detail: fmt.Sprintf("queue state of #%d is unknown", number)}
		}
		if gtapi.Restacking(state) {
			return &statusProbe{Verdict: queueMerging, Detail: state}
		}
	}
	return &statusProbe{Verdict: queueReady}
}

// nativeStackMove records one branch on the base its open pull request has in a
// native GitHub stack, where this checkout's record names another parent or
// none at all.
type nativeStackMove struct {
	branch, parent, was string
	fork, head          string
	pr, stack           int
}

func (m nativeStackMove) String() string {
	was := ", which this checkout did not track"
	if m.was != "" {
		was = ", where this checkout recorded " + m.was
	}
	return fmt.Sprintf("%s on %s, the base of #%d in native stack #%d%s", m.branch, m.parent, m.pr, m.stack, was)
}

// nativeStackParents names the branches of from's downstack whose native stack
// base differs from the parent this checkout records, and the state with them
// recorded there. GitHub's stack is the truth: the local record is only this
// checkout's last write, and a branch retargeted on GitHub, or tracked here
// before its parent was, leaves it naming trunk, which a restack then replays
// the branch onto. A base this checkout does not track is adopted from its own
// pull request. Each level is one batched read, and the moves come bottom-up,
// so applying them in order records every parent before its child.
func nativeStackParents(ctx context.Context, l lane, state gtState, from string) ([]nativeStackMove, gtState, error) {
	if !l.native || from == "" {
		return nil, state, nil
	}
	trunk, err := gtTrunkBranch("ship", state)
	if err != nil || from == trunk {
		return nil, state, err
	}
	pending, err := gtDownstack("ship", state, from, trunk)
	if err != nil {
		return nil, nil, err
	}
	owner, repo, err := gtRepoOwnerName(ctx, l, "ship")
	if err != nil {
		return nil, nil, err
	}
	seen := map[string]bool{}
	for _, branch := range pending {
		seen[branch] = true
	}
	child := map[string]string{}
	var moves []nativeStackMove
	for len(pending) > 0 {
		infos, err := gtAPI(ctx).PullRequestInfo(ctx, gtapi.PullRequestInfoRequest{RepoOwner: owner, RepoName: repo, PRHeadRefNames: pending, Callsite: "ccx"})
		if err != nil {
			return nil, nil, fmt.Errorf("ship: read the native stack under %s: %w", from, err)
		}
		open := map[string]gtapi.PullRequestInfo{}
		for _, info := range infos {
			if info.State == gtapi.PROpen && info.StackNumber != 0 {
				open[info.HeadRefName] = info
			}
		}
		var next []string
		for _, branch := range pending {
			info, stacked := open[branch]
			s, tracked := state[branch]
			switch {
			case !stacked && !tracked:
				return nil, nil, refuse("ship: %s, the native stack base of %s, is untracked here and has no open pull request in a native stack — ship it onto its own parent first with ccx vcs ship --parent <branch>", branch, child[branch])
			case !stacked, tracked && s.Parents[0].Ref == info.BaseRefName:
				continue
			}
			m := nativeStackMove{branch: branch, parent: info.BaseRefName, pr: info.PRNumber, stack: info.StackNumber}
			if tracked {
				m.was = s.Parents[0].Ref
			}
			moves = append(moves, m)
			if seen[m.parent] || m.parent == trunk {
				continue
			}
			if _, known := state[m.parent]; known {
				below, err := gtDownstack("ship", state, m.parent, trunk)
				if err != nil {
					return nil, nil, err
				}
				for _, b := range below {
					if !seen[b] {
						seen[b] = true
						next = append(next, b)
					}
				}
				continue
			}
			exists, err := gitRefExists(ctx, l.dir(), "ship", gtRestackRef(m.parent))
			if err != nil {
				return nil, nil, err
			}
			if !exists {
				return nil, nil, refuse("ship: %s, the native stack base of %s, is no local branch — fetch it, then ship again", m.parent, branch)
			}
			seen[m.parent], child[m.parent] = true, branch
			next = append(next, m.parent)
		}
		pending = next
	}
	slices.Reverse(moves)
	planned := maps.Clone(state)
	for i, m := range moves {
		head := state[m.branch].Head
		if head == "" {
			if head, err = gtRestackHead(ctx, "ship", l.dir(), m.branch); err != nil {
				return nil, nil, err
			}
		}
		parentHead := planned[m.parent].Head
		fork, err := gtMergeBase(ctx, l.dir(), "ship", parentHead, head)
		if err != nil {
			return nil, nil, err
		}
		moves[i].head, moves[i].fork = head, fork
		planned[m.branch] = gtBranchState{Head: head, State: state[m.branch].State, NeedsRestack: fork != parentHead, Parents: []gtRef{{Ref: m.parent, SHA: fork}}}
	}
	return moves, planned, nil
}

func stackNativeAdopt(ctx context.Context, l lane, tr vcs.Trunk, state gtState, current string) (gtState, gtState, error) {
	adopted := gtState{}
	if !l.native || current == "" || current == tr.Name() {
		return state, adopted, nil
	}
	if _, tracked := state[current]; !tracked {
		pr, err := stackOpenPR(ctx, l, current)
		if err != nil || pr.PRNumber == 0 {
			return state, adopted, err
		}
		base := pr.BaseRefName
		if pr.StackNumber != 0 {
			base = tr.Name()
		}
		floor := string(tr.Ref())
		if base != tr.Name() {
			exists, err := gitRefExists(ctx, l.dir(), stackRebasePrefix, gtRestackRef(base))
			if err != nil {
				return nil, nil, err
			}
			if !exists {
				return nil, nil, refuse("%s: %s, the base of %s's open #%d, is no local branch — fetch it with git fetch %s %s:%s, then re-run", stackRebasePrefix, base, current, pr.PRNumber, tr.Remote(), base, base)
			}
			floor = gtRestackRef(base)
		}
		head, err := gtRestackHead(ctx, stackRebasePrefix, l.dir(), current)
		if err != nil {
			return nil, nil, err
		}
		fork, err := stackMergeBase(ctx, l.dir(), head, floor)
		if err != nil {
			return nil, nil, err
		}
		state = maps.Clone(state)
		state[current] = gtBranchState{Head: head, Parents: []gtRef{{Ref: base, SHA: fork}}}
		adopted[current] = state[current]
	}
	moves, planned, err := nativeStackParents(ctx, l, state, current)
	if err != nil {
		return nil, nil, err
	}
	for _, m := range moves {
		if planned[m.branch].State == "" {
			adopted[m.branch] = planned[m.branch]
		}
	}
	return planned, adopted, nil
}

func stackOpenPR(ctx context.Context, l lane, branch string) (gtapi.PullRequestInfo, error) {
	owner, repo, err := gtRepoOwnerName(ctx, l, stackRebasePrefix)
	if err != nil {
		return gtapi.PullRequestInfo{}, err
	}
	infos, err := gtAPI(ctx).PullRequestInfo(ctx, gtapi.PullRequestInfoRequest{RepoOwner: owner, RepoName: repo, PRHeadRefNames: []string{branch}, Callsite: "ccx"})
	if err != nil {
		return gtapi.PullRequestInfo{}, fmt.Errorf("%s: read the pull request of %s: %w", stackRebasePrefix, branch, err)
	}
	for _, info := range infos {
		if info.State == gtapi.PROpen && info.HeadRefName == branch {
			return info, nil
		}
	}
	return gtapi.PullRequestInfo{}, nil
}

// nativeStackFrom is the branch whose downstack a ship reads the native stack
// of: the --parent it moves onto when one is named, since that move outranks
// the branch's own pull request, and otherwise the branch once it is tracked.
func nativeStackFrom(state gtState, branch, parent string) string {
	if parent != "" {
		branch = parent
	}
	if _, tracked := state[branch]; !tracked {
		return ""
	}
	return branch
}

// nativeStackRecord writes moves into this checkout's record, keeping a held
// branch held, and reports each in the run's summary.
func nativeStackRecord(ctx context.Context, c *gtCache, state gtState, moves []nativeStackMove) (string, error) {
	if len(moves) == 0 {
		return "", nil
	}
	commonDir, err := c.common(ctx)
	if err != nil {
		return "", err
	}
	defer c.forget()
	var segs []string
	for _, m := range moves {
		adopt := gtmeta.AdoptRoot
		if state[m.branch].State != "" {
			adopt = gtmeta.AdoptFrozen
		}
		if err := adopt(ctx, commonDir, m.branch, m.parent, m.fork, m.head); err != nil {
			return strings.Join(segs, ""), fmt.Errorf("ship: record %s: %w", m, err)
		}
		segs = append(segs, "recorded "+m.String()+shipSep)
	}
	return strings.Join(segs, ""), nil
}
