package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
	"github.com/yasyf/cc-context/internal/gtapi"
	"github.com/yasyf/cc-context/internal/render"
	"github.com/yasyf/cc-context/internal/vcs"
)

const prRecreateReason = "Graphite lost its stack record"

type prRecreateSource struct {
	Number int    `json:"number"`
	Title  string `json:"title"`
	Body   string `json:"body"`
	State  string `json:"state"`
	Draft  bool   `json:"draft"`
	Labels []struct {
		Name string `json:"name"`
	} `json:"labels"`
	Base struct {
		Ref  string `json:"ref"`
		Repo struct {
			DefaultBranch string `json:"default_branch"`
		} `json:"repo"`
	} `json:"base"`
	Head struct {
		Ref string `json:"ref"`
		SHA string `json:"sha"`
	} `json:"head"`
}

func newVcsPRRecreateCmd() *cobra.Command {
	var repo string
	cmd := &cobra.Command{
		Use:   "recreate <number>",
		Short: "Replace an open pull request Graphite no longer tracks with a fresh one on the same branch",
		Long: `Replace an open pull request whose Graphite stack record still holds pull
requests that already landed or closed. Graphite reads such a pull request as
untracked, so the merge queue cannot enqueue it, and neither a fresh head nor a
close and reopen clears the record.

The command closes the pull request, since GitHub allows one open pull request
per branch and base, then opens a new one through Graphite's submit API so
Graphite records it: same branch, head, base, title, draft state, and body,
with a closing "Replaces #<number> (` + prRecreateReason + `)." line. It
then copies the old pull request's labels and comments on it with a link to
the new one. When the
submit fails, the old pull request is reopened.

The new pull request starts without the old one's reviews, so its approvals
must be given again. Nothing runs this automatically: ccx vcs stack submit
names the command when it refuses such a pull request.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runVcsPRRecreate(cmd, args[0], repo)
		},
	}
	cmd.Flags().StringVarP(&repo, "repo", "R", "", "the owner/name repository (default: the current checkout's)")
	return cmd
}

func runVcsPRRecreate(cmd *cobra.Command, arg, repo string) error {
	ctx := cmd.Context()
	number, err := strconv.Atoi(strings.TrimPrefix(arg, "#"))
	if err != nil || number <= 0 {
		return fmt.Errorf("pr recreate: %q is not a pull request number", arg)
	}
	dir := render.Dir(workingDir(ctx))
	if repo == "" {
		looked, err := vcs.LookupRepo(ctx, dir, false)
		if err != nil {
			return fmt.Errorf("pr recreate: name the repository with --repo: %w", err)
		}
		repo = looked.NameWithOwner
	}
	owner, name, ok := strings.Cut(repo, "/")
	if !ok {
		return fmt.Errorf("pr recreate: %q is not an owner/name repository", repo)
	}

	out, err := ghAPI(ctx, dir, ghPullPath(repo, number))
	if err != nil {
		return fmt.Errorf("pr recreate: read #%d: %w", number, err)
	}
	var old prRecreateSource
	if err := json.Unmarshal([]byte(out), &old); err != nil {
		return fmt.Errorf("pr recreate: parse #%d: %w", number, err)
	}
	if old.State != "open" {
		return fmt.Errorf("pr recreate: #%d is %s; only an open pull request is recreated", number, old.State)
	}
	baseSha, err := ghAPI(ctx, dir, fmt.Sprintf("repos/%s/git/ref/heads/%s", repo, old.Base.Ref), "--jq", ".object.sha")
	if err != nil {
		return fmt.Errorf("pr recreate: read %s's head: %w", old.Base.Ref, err)
	}

	if _, err := ghAPI(ctx, dir, ghPatchPullArgv(repo, number, "-f", "state=closed")[1:]...); err != nil {
		return fmt.Errorf("pr recreate: close #%d: %w", number, err)
	}
	draft := old.Draft
	submitted, err := gtAPI(ctx).SubmitPullRequests(ctx, gtapi.SubmitRequest{
		RepoOwner:       owner,
		RepoName:        name,
		TrunkBranchName: old.Base.Repo.DefaultBranch,
		PRs: []gtapi.SubmitPR{{
			Action:  gtapi.SubmitCreate,
			Head:    old.Head.Ref,
			HeadSha: old.Head.SHA,
			Base:    old.Base.Ref,
			BaseSha: strings.TrimSpace(baseSha),
			Title:   old.Title,
			Body:    strings.TrimSpace(old.Body + fmt.Sprintf("\n\nReplaces #%d (%s).", number, prRecreateReason)),
			Draft:   &draft,
		}},
	})
	if err == nil && len(submitted) != 1 {
		err = fmt.Errorf("graphite answered %d pull requests for one create", len(submitted))
	}
	if err != nil {
		if _, reopenErr := ghAPI(context.WithoutCancel(ctx), dir, ghPatchPullArgv(repo, number, "-f", "state=open")[1:]...); reopenErr != nil {
			return fmt.Errorf("pr recreate: #%d is closed, Graphite opened no replacement, and reopening #%d failed too — reopen it by hand: %w", number, number, errors.Join(err, reopenErr))
		}
		return fmt.Errorf("pr recreate: Graphite opened no replacement for #%d, so it was reopened: %w", number, err)
	}
	fresh := submitted[0]
	if len(old.Labels) > 0 {
		argv := []string{"-X", "POST", fmt.Sprintf("repos/%s/issues/%d/labels", repo, fresh.PRNumber), "--silent"}
		for _, label := range old.Labels {
			argv = append(argv, "-f", "labels[]="+label.Name)
		}
		if _, err := ghAPI(ctx, dir, argv...); err != nil {
			return fmt.Errorf("pr recreate: opened #%d %s, but copying #%d's labels failed: %w", fresh.PRNumber, fresh.PRURL, number, err)
		}
	}
	note := fmt.Sprintf("body=Replaced by #%d (%s).", fresh.PRNumber, prRecreateReason)
	if _, err := ghAPI(ctx, dir, "-X", "POST", fmt.Sprintf("repos/%s/issues/%d/comments", repo, number), "-f", note, "--silent"); err != nil {
		return fmt.Errorf("pr recreate: opened #%d %s, but linking it from #%d failed: %w", fresh.PRNumber, fresh.PRURL, number, err)
	}
	cmd.Printf("recreated #%d as #%d %s · closed #%d with a link to it\n", number, fresh.PRNumber, fresh.PRURL, number)
	return nil
}
