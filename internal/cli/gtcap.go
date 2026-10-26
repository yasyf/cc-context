package cli

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/yasyf/cc-context/internal/gtapi"
	"github.com/yasyf/cc-context/internal/render"
)

const (
	gtStackCapMessage = "Only opening PRs for the first"
	gtUnrecordedDir   = "ccx-graphite-unrecorded"
	gtSegmentPrefix   = "ccx-segment/"
)

// gtStackCapped reports whether Graphite refused an entry for its cap on how
// many branches one stack may open pull requests for.
func gtStackCapped(e *gtapi.SubmitError) bool {
	return slices.ContainsFunc(e.Failed, func(f gtapi.BranchSubmitError) bool {
		return strings.Contains(f.Message, gtStackCapMessage)
	})
}

func gtSegmentBase(branch string) string { return gtSegmentPrefix + branch }

func gtSubmitOnSegment(ctx context.Context, errW io.Writer, client *gtapi.Client, dir render.Dir, prefix, owner, name, trunk string, b gtSubmitBranch, pr gtapi.SubmitPR) ([]gtapi.SubmittedPR, error) {
	segment := gtSegmentBase(pr.Head)
	if b.parkedOn != segment {
		if err := gtPushSegment(ctx, dir, prefix, segment, pr.BaseSha); err != nil {
			return nil, err
		}
	}
	pr.Base = segment
	out, err := client.SubmitPullRequests(ctx, gtapi.SubmitRequest{RepoOwner: owner, RepoName: name, TrunkBranchName: trunk, PRs: []gtapi.SubmitPR{pr}})
	if err != nil {
		return nil, err
	}
	number := pr.PRNumber
	for _, created := range out {
		number = created.PRNumber
	}
	if _, err := fmt.Fprintf(errW, "%s: graphite caps the stack under %s, so ccx recorded %s's PR #%d on %s, a copy of %s at %s that no landing deletes; the stack submit after %s lands moves it onto %s\n",
		prefix, b.base, pr.Head, number, segment, b.base, shortOID(pr.BaseSha), b.base, trunk); err != nil {
		return nil, fmt.Errorf("%s: name the segment Graphite recorded: %w", prefix, err)
	}
	return out, nil
}

func gtPushSegment(ctx context.Context, dir render.Dir, prefix, segment, sha string) error {
	heads, err := stackListRemoteHeads(ctx, dir, prefix, "origin", []string{segment})
	if err != nil {
		return err
	}
	argv := pushArgv("origin", "--no-verify", "--force-with-lease="+gtRestackRef(segment)+":"+heads[segment], sha+":"+gtRestackRef(segment))
	if _, err := render.RunCLI(ctx, dir, "git", argv); err != nil {
		return fmt.Errorf("%s: push segment base %s at %s: %w", prefix, segment, shortOID(sha), err)
	}
	return nil
}

func gtDropSegment(ctx context.Context, errW io.Writer, dir render.Dir, prefix, nwo string, b gtSubmitBranch) error {
	segment := gtSegmentBase(b.name)
	if b.parkedOn != segment {
		return nil
	}
	pull, found, err := gtOpenPull(ctx, dir, nwo, b.name)
	if err != nil || !found || pull.Base.Ref == segment {
		return err
	}
	argv := pushArgv("origin", "--no-verify", "--force-with-lease="+gtRestackRef(segment)+":"+b.baseSha, ":"+gtRestackRef(segment))
	if _, err := render.RunCLI(ctx, dir, "git", argv); err != nil {
		return fmt.Errorf("%s: delete segment base %s: %w", prefix, segment, err)
	}
	if _, err := fmt.Fprintf(errW, "%s: graphite records #%d on %s, so ccx deleted its segment base %s\n", prefix, pull.Number, pull.Base.Ref, segment); err != nil {
		return fmt.Errorf("%s: name the segment ccx deleted: %w", prefix, err)
	}
	return nil
}

func gtKeepOnSegment(ctx context.Context, dir render.Dir, commonDir, nwo string, pr gtapi.SubmitPR) (stackEntry, error) {
	pr.Base = gtSegmentBase(pr.Head)
	pull, found, err := gtOpenPull(ctx, dir, nwo, pr.Head)
	if err != nil {
		return stackEntry{}, err
	}
	if !found {
		if pull, err = gtCreatePull(ctx, dir, nwo, pr); err != nil {
			return stackEntry{}, err
		}
	} else if _, err := gtRetargetOnGitHub(ctx, dir, nwo, &pull, pr.Base); err != nil {
		return stackEntry{}, err
	}
	if err := gtMarkUnrecorded(commonDir, pr.Head); err != nil {
		return stackEntry{}, err
	}
	return stackEntry{Branch: pr.Head, PR: pull.Number, URL: pull.HTMLURL, HasBody: strings.TrimSpace(pull.Body) != "", State: string(gtapi.PROpen)}, nil
}

// gtRetargetOnGitHub moves an open pull request onto base through GitHub alone,
// reporting whether it had to move.
func gtRetargetOnGitHub(ctx context.Context, dir render.Dir, nwo string, pull *ghPull, base string) (bool, error) {
	if pull.Base.Ref == base {
		return false, nil
	}
	if _, err := render.RunCLI(ctx, dir, "gh", ghPatchPullArgv(nwo, pull.Number, "-f", "base="+base)); err != nil {
		return false, fmt.Errorf("gh api: move PR #%d onto %s: %w", pull.Number, base, err)
	}
	pull.Base.Ref = base
	return true, nil
}

func gtOpenPull(ctx context.Context, dir render.Dir, nwo, head string) (ghPull, bool, error) {
	out, err := render.RunCLI(ctx, dir, "gh", ghPullsByHeadArgv(nwo, head, "open"))
	if err != nil {
		return ghPull{}, false, fmt.Errorf("gh api: read the open pull request of %s: %w", head, err)
	}
	var pulls []ghPull
	if err := json.Unmarshal([]byte(out), &pulls); err != nil {
		return ghPull{}, false, fmt.Errorf("gh api: parse the open pull request of %s: %w", head, err)
	}
	if len(pulls) == 0 {
		return ghPull{}, false, nil
	}
	return pulls[0], true, nil
}

func gtCreatePull(ctx context.Context, dir render.Dir, nwo string, pr gtapi.SubmitPR) (ghPull, error) {
	argv := []string{"api", "-X", "POST", "repos/" + nwo + "/pulls", "-f", "head=" + pr.Head, "-f", "base=" + pr.Base, "-f", "title=" + pr.Title, "-f", "body=" + pr.Body}
	if pr.Draft != nil && *pr.Draft {
		argv = append(argv, "-F", "draft=true")
	}
	out, err := render.RunCLI(ctx, dir, "gh", argv)
	if err != nil {
		return ghPull{}, fmt.Errorf("gh api: open the pull request of %s: %w", pr.Head, err)
	}
	var pull ghPull
	if err := json.Unmarshal([]byte(out), &pull); err != nil {
		return ghPull{}, fmt.Errorf("gh api: parse the pull request opened for %s: %w", pr.Head, err)
	}
	return pull, nil
}

func gtUnrecordedPath(commonDir, branch string) string {
	return filepath.Join(commonDir, gtUnrecordedDir, fmt.Sprintf("%x", sha256.Sum256([]byte(branch))))
}

func gtMarkUnrecorded(commonDir, branch string) error {
	path := gtUnrecordedPath(commonDir, branch)
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return fmt.Errorf("mark %s as unrecorded by Graphite: %w", branch, err)
	}
	if err := os.WriteFile(path, []byte(branch+"\n"), 0o600); err != nil {
		return fmt.Errorf("mark %s as unrecorded by Graphite: %w", branch, err)
	}
	return nil
}

func gtClearUnrecorded(commonDir, branch string) error {
	if err := os.Remove(gtUnrecordedPath(commonDir, branch)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("clear %s's unrecorded mark: %w", branch, err)
	}
	return nil
}

func gtUnrecorded(commonDir, branch string) (bool, error) {
	_, err := os.Stat(gtUnrecordedPath(commonDir, branch))
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read %s's unrecorded mark: %w", branch, err)
	}
	return true, nil
}

func gtUnrecordedOf(commonDir string, plan []gtSubmitBranch) (map[string]bool, error) {
	marked := map[string]bool{}
	for _, b := range plan {
		unrecorded, err := gtUnrecorded(commonDir, b.name)
		if err != nil {
			return nil, err
		}
		marked[b.name] = unrecorded
	}
	return marked, nil
}
