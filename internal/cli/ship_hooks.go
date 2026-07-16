package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/yasyf/cc-context/internal/render"
	"github.com/yasyf/cc-context/internal/vcs"
)

// shipHookConfigNames are the prek config filenames probed at root; ship skips
// hooks silently when none is present.
var shipHookConfigNames = []string{".pre-commit-config.yaml", ".pre-commit-config.yml", "prek.toml"}

// shipRunHooks runs prek (via uvx) over the files ship is about to commit, with
// an auto-fix-then-verify policy: prek's exit code cannot tell a genuine failure
// from files it modified in place, so a nonzero first run is re-staged for Git,
// re-derived, and retried once before it is treated as a real failure. A flaky
// hook that passes unchanged on retry is indistinguishable and reports "hooks
// fixed". External hook execution retains the same staging window as a manual
// git add followed by git commit. A later jj push-time auto-rebase may incorporate
// upstream content the hooks did not inspect; upstream CI covers that boundary.
func shipRunHooks(ctx context.Context, errW io.Writer, root string, kind vcs.Kind, o shipOpts) (string, error) {
	if o.noVerify {
		return "", nil
	}
	if !shipHasHookConfig(root) {
		return "", nil
	}
	files, err := shipHookFiles(ctx, root, kind, o)
	if err != nil {
		return "", err
	}
	if len(files) == 0 {
		return "", nil
	}
	if kind == vcs.JJ {
		if _, err := os.Stat(filepath.Join(root, ".git")); err != nil {
			return "hooks no-git", nil
		}
	}
	if _, err := exec.LookPath("uvx"); err != nil {
		return "hooks uvx-missing", nil
	}

	// Leading-dash filenames intentionally reach prek unchanged so it fails loudly.
	argv := append([]string{"prek", "run", "--cd", root, "--files"}, files...)
	if _, err := render.RunCLI(ctx, "uvx", argv); err == nil {
		return "hooks ok", nil
	}
	if kind == vcs.Git {
		if err := shipGitAdd(ctx, o); err != nil {
			return "", err
		}
	}
	files, err = shipHookFiles(ctx, root, kind, o)
	if err != nil {
		return "", err
	}
	if len(files) == 0 {
		return "", errors.New("ship: hooks: auto-fixes reverted every pending change; nothing to commit")
	}
	argv = append([]string{"prek", "run", "--cd", root, "--files"}, files...)
	if err := render.RunCLIStream(ctx, "uvx", argv, errW); err != nil {
		return "", fmt.Errorf("ship: hooks: %w — pre-commit hooks still failing after auto-fix; fix them or re-run with --no-verify", err)
	}
	return "hooks fixed", nil
}

// shipHasHookConfig reports whether root holds a prek-recognized config file.
func shipHasHookConfig(root string) bool {
	for _, name := range shipHookConfigNames {
		if _, err := os.Stat(filepath.Join(root, name)); err == nil {
			return true
		}
	}
	return false
}

// shipHookFiles lists the root-relative files ship is about to commit, for
// scoping the prek run. JJ: `jj diff --name-only` with its working directory
// pinned to root (jj emits cwd-relative paths; scoped o.paths are rebased to
// match) — the call also snapshots @ and, colocated, syncs the git index, which
// is what makes new files visible to prek. Git: NUL-delimited
// `git diff --cached --name-only --diff-filter=d` after shipGitAdd; git output
// is root-relative from any cwd. The existence filter drops jj-tracked deletions
// but keeps broken symlinks; a jj filename containing a newline splits and is
// dropped (git's NUL lane is immune) — accepted, like leading-dash names. For
// --amend this lists what is being folded; unchanged files already in the
// amended commit are not re-hooked.
func shipHookFiles(ctx context.Context, root string, kind vcs.Kind, o shipOpts) ([]string, error) {
	var out string
	switch kind {
	case vcs.JJ:
		argv := []string{"diff", "--name-only"}
		if len(o.paths) > 0 {
			rel, err := rootRelPaths(root, o.paths)
			if err != nil {
				return nil, fmt.Errorf("ship: hook files: %w", err)
			}
			argv = append(argv, "--")
			argv = append(argv, rel...)
		}
		var err error
		out, err = render.RunCLIDir(ctx, root, "jj", argv)
		if err != nil {
			return nil, fmt.Errorf("ship: jj diff: %w", err)
		}
	case vcs.Git:
		argv := []string{"diff", "--cached", "--name-only", "--diff-filter=d", "-z"}
		if len(o.paths) > 0 {
			argv = append(argv, "--")
			argv = append(argv, o.paths...)
		}
		var err error
		out, err = render.RunCLI(ctx, "git", argv)
		if err != nil {
			return nil, fmt.Errorf("ship: git diff: %w", err)
		}
	default:
		return nil, fmt.Errorf("ship: hook files: unsupported vcs")
	}
	separator := "\n"
	if kind == vcs.Git {
		separator = "\x00"
	}
	var files []string
	for _, line := range strings.Split(out, separator) {
		if line == "" {
			continue
		}
		if _, err := os.Lstat(filepath.Join(root, line)); err != nil {
			continue
		}
		files = append(files, line)
	}
	return files, nil
}

// rootRelPaths rebases cwd-relative ship paths onto root, for the jj diff that
// runs with its working directory pinned there.
func rootRelPaths(root string, paths []string) ([]string, error) {
	rel := make([]string, 0, len(paths))
	for _, p := range paths {
		abs, err := filepath.Abs(p)
		if err != nil {
			return nil, err
		}
		r, err := filepath.Rel(root, abs)
		if err != nil {
			return nil, err
		}
		rel = append(rel, r)
	}
	return rel, nil
}
