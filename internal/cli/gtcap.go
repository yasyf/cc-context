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
)

// gtStackCapped reports whether Graphite refused an entry for its cap on how
// many branches above trunk one stack may open pull requests for. The cap
// counts from trunk, so no narrower submit gets the branch past it.
func gtStackCapped(e *gtapi.SubmitError) bool {
	return slices.ContainsFunc(e.Failed, func(f gtapi.BranchSubmitError) bool {
		return strings.Contains(f.Message, gtStackCapMessage)
	})
}

// gtOpenPastTheCap gives a branch Graphite refused at its stack cap the pull
// request the submit asked for, through GitHub alone: it opens one with the
// carried title and body, or moves an open one onto the planned base. Graphite
// records neither, so the branch is marked for a stack submit once the stack
// is split.
func gtOpenPastTheCap(ctx context.Context, errW io.Writer, dir render.Dir, commonDir, prefix, nwo string, pr gtapi.SubmitPR) (stackEntry, error) {
	pull, found, err := gtOpenPull(ctx, dir, nwo, pr.Head)
	if err != nil {
		return stackEntry{}, err
	}
	did := "kept"
	switch {
	case !found:
		if pull, err = gtCreatePull(ctx, dir, nwo, pr); err != nil {
			return stackEntry{}, err
		}
		did = "opened"
	case pull.Base.Ref != pr.Base:
		if _, err := render.RunCLI(ctx, dir, "gh", ghPatchPullArgv(nwo, pull.Number, "-f", "base="+pr.Base)); err != nil {
			return stackEntry{}, fmt.Errorf("gh api: move PR #%d onto %s: %w", pull.Number, pr.Base, err)
		}
		pull.Base.Ref = pr.Base
		did = "retargeted"
	}
	if err := gtMarkUnrecorded(commonDir, pr.Head); err != nil {
		return stackEntry{}, err
	}
	if _, err := fmt.Fprintf(errW, "%s: graphite refused %s at its stack cap, so ccx %s PR #%d %s on %s through GitHub alone; Graphite has no record of it, so run ccx vcs stack submit once the stack is split\n",
		prefix, pr.Head, did, pull.Number, pull.HTMLURL, pr.Base); err != nil {
		return stackEntry{}, fmt.Errorf("%s: name the pull request Graphite did not record: %w", prefix, err)
	}
	return stackEntry{Branch: pr.Head, PR: pull.Number, URL: pull.HTMLURL, HasBody: strings.TrimSpace(pull.Body) != "", State: string(gtapi.PROpen)}, nil
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
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("mark %s as unrecorded by Graphite: %w", branch, err)
	}
	if err := os.WriteFile(path, []byte(branch+"\n"), 0o644); err != nil {
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
