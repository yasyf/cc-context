package cli

import (
	"context"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/yasyf/cc-context/internal/gtapi"
	"github.com/yasyf/cc-context/internal/gtmeta"
	"github.com/yasyf/cc-context/internal/nativemeta"
	"github.com/yasyf/cc-context/internal/render"
	"github.com/yasyf/cc-context/internal/vcs"
)

type stackProviderKey struct{}

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

func nativeStackRun(ctx context.Context, dir render.Dir, argv []string, errW io.Writer, extraEnv ...string) (gtResult, error) {
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
	trunk, err := nativemeta.ReadTrunk(common)
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
		branch = argv[1]
		parent = value("--onto")
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
	if err = nativemeta.AdoptRoot(ctx, common, branch, trunk, base, head); err == nil && parent != trunk {
		err = nativemeta.Reparent(ctx, common, map[string]string{branch: parent})
	}
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
			return gtAPI(ctx)
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
