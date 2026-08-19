package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/yasyf/cc-context/internal/render"
	"github.com/yasyf/cc-context/internal/vcs"
)

// shipHookConfigNames are the prek config filenames probed at root; ship skips
// hooks silently when none is present.
var shipHookConfigNames = []string{".pre-commit-config.yaml", ".pre-commit-config.yml", "prek.toml"}

// shipHookIndexLock is the lock git holds over the index it is rewriting. It
// sits beside the index, which sibling worktrees do not share: each linked
// worktree's index lives under its own admin dir, so a commit contends over the
// lock in Checkout.GitDir — CommonDir only for the repository's own working copy
// — and never over the common dir's.
const shipHookIndexLock = "index.lock"

// shipHookScrubbedEnv are the variables a hook child must not inherit: each pins
// git to whatever checkout invoked ccx, which from a linked worktree is not the
// one being committed.
var shipHookScrubbedEnv = []string{"GIT_DIR", "GIT_WORK_TREE"}

// shipHookRetryLead separates the two streamed prek passes, which run the same
// hooks over the same files and would otherwise read as one run repeating itself.
const shipHookRetryLead = "ship: hooks: re-running prek over the auto-fixed files\n"

// shipRunHooks runs prek (via uvx) over the files ship is about to commit, with
// an auto-fix-then-verify policy: prek's exit code cannot tell a genuine failure
// from files it modified in place, so a nonzero first run is re-staged for Git,
// re-derived, and retried once before it is treated as a real failure. A flaky
// hook that passes unchanged on retry is indistinguishable and reports "hooks
// fixed". External hook execution retains the same staging window as a manual
// git add followed by git commit. A later jj push-time auto-rebase may incorporate
// upstream content the hooks did not inspect; upstream CI covers that boundary.
// The covered result reports whether this pass discharges the commit's hook
// obligations, so the caller can suppress Git's duplicate run; a message-stage
// config, which prek run --files never reaches, keeps it false.
// The first pass streams to errW only on a terminal, where a human watches both
// passes go by in order; off one it is discarded, since the auto-fix policy makes
// its output a pre-fix snapshot the retry may already have repaired.
func shipRunHooks(ctx context.Context, errW io.Writer, root string, kind vcs.Kind, o shipOpts) (seg string, covered bool, err error) {
	if o.noVerify {
		return "", false, nil
	}
	config := shipHookConfigPath(root)
	if config == "" {
		return "", false, nil
	}
	files, err := shipHookFiles(ctx, root, kind, o)
	if err != nil {
		return "", false, err
	}
	if len(files) == 0 {
		return "", false, nil
	}
	if kind == vcs.JJ {
		if _, err := os.Stat(filepath.Join(root, ".git")); err != nil {
			return "hooks no-git", false, nil
		}
	}
	if _, err := exec.LookPath("uvx"); err != nil {
		return "hooks uvx-missing", false, nil
	}
	if err := shipRefuseIndexLock(root); err != nil {
		return "", false, err
	}
	msgStage, err := shipHookConfigHasMsgStage(config)
	if err != nil {
		return "", false, err
	}
	covered = !msgStage

	streamed := shipStreamCI(errW)
	var firstW io.Writer
	if streamed {
		firstW = errW
	}
	// Leading-dash filenames intentionally reach prek unchanged so it fails loudly.
	argv := append([]string{"prek", "run", "--cd", root, "--files"}, files...)
	if err := shipRunPrek(ctx, argv, firstW); err == nil {
		return "hooks ok", covered, nil
	}
	if kind == vcs.Git {
		if err := shipGitAdd(ctx, o); err != nil {
			return "", false, err
		}
	}
	files, err = shipHookFiles(ctx, root, kind, o)
	if err != nil {
		return "", false, err
	}
	if len(files) == 0 {
		return "", false, errors.New("ship: hooks: auto-fixes reverted every pending change; nothing to commit")
	}
	if streamed {
		if _, err := io.WriteString(errW, shipHookRetryLead); err != nil {
			return "", false, fmt.Errorf("ship: hooks: report the retry: %w", err)
		}
	}
	argv = append([]string{"prek", "run", "--cd", root, "--files"}, files...)
	if err := shipRunPrek(ctx, argv, errW); err != nil {
		return "", false, fmt.Errorf("ship: hooks: %w — pre-commit hooks still failing after auto-fix; fix them or re-run with --no-verify", err)
	}
	return "hooks fixed", covered, nil
}

// shipRunPrek spawns prek with shipHookScrubbedEnv stripped from the child
// environment, which is why it builds the command itself rather than going
// through render (whose helpers only extend os.Environ, and an empty GIT_DIR is
// fatal to git rather than absent). Output goes to w on the pass a human can
// watch, and a nil w routes the child to /dev/null: off a terminal the only
// reader is a capture, which would take the first pass's pre-fix failures for
// the verdict the retry pass actually decides.
func shipRunPrek(ctx context.Context, argv []string, w io.Writer) error {
	cmd := exec.CommandContext(ctx, "uvx", argv...) //nolint:gosec // argv is prek's fixed verb plus the files ship derived, not user free-text
	cmd.Env = shipHookEnv()
	cmd.Stdout, cmd.Stderr = w, w
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("uvx: %w", err)
	}
	return nil
}

func shipHookEnv() []string {
	env := os.Environ()
	kept := make([]string, 0, len(env))
	for _, entry := range env {
		name, _, _ := strings.Cut(entry, "=")
		if slices.Contains(shipHookScrubbedEnv, name) {
			continue
		}
		kept = append(kept, entry)
	}
	return kept
}

// shipRefuseIndexLock refuses while another process holds the index lock of the
// checkout under root, and passes a jj working copy with no git backing, which
// has no index to contend over. The refusal carries the lock's mtime because
// git's own failure names neither the holder nor its age, and only the age
// separates a live sibling session (seconds) from a crashed process's leftovers
// (hours).
func shipRefuseIndexLock(root string) error {
	c, err := vcs.ResolveCheckout(root)
	if err != nil {
		return fmt.Errorf("ship: hooks: %w", err)
	}
	if c.GitDir == "" {
		return nil
	}
	lock := filepath.Join(c.GitDir, shipHookIndexLock)
	info, err := os.Stat(lock)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("ship: hooks: stat %s: %w", lock, err)
	}
	return fmt.Errorf("ship: hooks: another process holds the git index lock: %s (held since %s) — wait for it to finish, or delete the lock if the process that took it is gone", lock, info.ModTime().Format(time.RFC3339))
}

// shipHookConfigPath returns root's prek-recognized config file, or "" when
// root holds none.
func shipHookConfigPath(root string) string {
	for _, name := range shipHookConfigNames {
		path := filepath.Join(root, name)
		if _, err := os.Stat(path); err == nil {
			return path
		}
	}
	return ""
}

// shipHasHookConfig reports whether root holds a prek-recognized config file.
func shipHasHookConfig(root string) bool {
	return shipHookConfigPath(root) != ""
}

// shipHookConfigHasMsgStage reports whether the config mentions commit-msg or
// prepare-commit-msg. The substring scan stands in for a YAML/TOML parse:
// over-matching only preserves Git's own hook run.
func shipHookConfigHasMsgStage(path string) (bool, error) {
	data, err := os.ReadFile(path) //nolint:gosec // the path is one of shipHookConfigNames under the repo root, not untrusted input
	if err != nil {
		return false, fmt.Errorf("ship: hooks: read %s: %w", filepath.Base(path), err)
	}
	return strings.Contains(string(data), "commit-msg"), nil
}

func shipChangedPaths(ctx context.Context, root string, kind vcs.Kind, o shipOpts) ([]string, error) {
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
		files = append(files, line)
	}
	return files, nil
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
	changed, err := shipChangedPaths(ctx, root, kind, o)
	if err != nil {
		return nil, err
	}
	var files []string
	for _, line := range changed {
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
