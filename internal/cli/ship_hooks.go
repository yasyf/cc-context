package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
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

// shipHookFindingsLead precedes the first pass's output when that pass came back
// nonzero. Ship adopts the auto-fixes and commits regardless, so this output is
// the only place a genuine finding is ever shown; CI is what grades it.
const shipHookFindingsLead = "ship: hooks: prek reported the following; auto-fixes were adopted and the commit proceeds — CI grades the result\n"

// shipHookStartNotice announces the hook run to a caller that is not getting the
// stream. Off a terminal the first pass is discarded, so without this line a
// prek suite whose Go linters take minutes is indistinguishable from a hang —
// and the caller that concludes "hung", kills ship, and finishes the commit by
// hand gets a commit no linter ever saw.
const shipHookStartNotice = "ship: hooks: running prek over %d file(s) — a cold Go lint pass can take several minutes\n"

// shipRunHooks runs prek (via uvx) over the files ship is about to commit, once:
// it adopts whatever the hooks fixed in place and lets the commit proceed, and
// CI grades what remains. prek's exit code cannot separate a genuine finding
// from files it modified, and the retry pass that used to resolve that ambiguity
// doubled the most expensive thing ship does — the Go hooks declare
// pass_filenames: false, so each pass analyzes the whole module however small
// the commit — to answer a question CI answers anyway. A nonzero pass therefore
// reports "hooks fixed" and its output is surfaced rather than swallowed, since
// it is the only place a real finding is shown. External hook execution retains
// the same staging window as a manual git add followed by git commit. A later jj
// push-time auto-rebase may incorporate upstream content the hooks did not
// inspect; upstream CI covers that boundary. The covered result reports whether
// this pass discharges the commit's hook obligations, so the caller can suppress
// Git's duplicate run; a message-stage config, which prek run --files never
// reaches, keeps it false.
func shipRunHooks(ctx context.Context, errW io.Writer, dir render.Dir, kind vcs.Kind, o shipOpts) (seg string, covered bool, err error) {
	if o.noVerify {
		return "", false, nil
	}
	config := shipHookConfigPath(string(dir))
	if config == "" {
		return "", false, nil
	}
	files, err := shipHookFiles(ctx, dir, kind, o)
	if err != nil {
		return "", false, err
	}
	if len(files) == 0 {
		return "", false, nil
	}
	if kind == vcs.JJ {
		if _, err := os.Stat(filepath.Join(string(dir), ".git")); err != nil {
			return "hooks no-git", false, nil
		}
	}
	if _, err := exec.LookPath("uvx"); err != nil {
		return "hooks uvx-missing", false, nil
	}
	if err := shipRefuseIndexLock(dir); err != nil {
		return "", false, err
	}
	msgStage, err := shipHookConfigHasMsgStage(config)
	if err != nil {
		return "", false, err
	}
	covered = !msgStage

	streamed := shipStreamCI(errW)
	var buf bytes.Buffer
	var passW io.Writer = &buf
	if streamed {
		passW = io.MultiWriter(errW, &buf)
	} else if _, werr := fmt.Fprintf(errW, shipHookStartNotice, len(files)); werr != nil {
		return "", false, fmt.Errorf("ship: hooks: announce the run: %w", werr)
	}
	// Leading-dash filenames intentionally reach prek unchanged so it fails loudly.
	argv := append([]string{"prek", "run", "--cd", string(dir), "--files"}, files...)
	if err := shipRunPrek(ctx, dir, argv, passW); err == nil {
		return "hooks ok", covered, nil
	}
	if kind == vcs.Git {
		if err := shipGitAdd(ctx, dir, o); err != nil {
			return "", false, err
		}
	}
	files, err = shipHookFiles(ctx, dir, kind, o)
	if err != nil {
		return "", false, err
	}
	if len(files) == 0 {
		return "", false, errors.New("ship: hooks: auto-fixes reverted every pending change; nothing to commit")
	}
	if !streamed {
		if _, werr := io.WriteString(errW, shipHookFindingsLead); werr != nil {
			return "", false, fmt.Errorf("ship: hooks: report the findings: %w", werr)
		}
		if _, werr := errW.Write(buf.Bytes()); werr != nil {
			return "", false, fmt.Errorf("ship: hooks: report the findings: %w", werr)
		}
	}
	return "hooks fixed", covered, nil
}

// shipRunPrek spawns prek with uvx in dir, whose pin is what keeps GIT_DIR and
// GIT_WORK_TREE off the hook child — each would otherwise point git at whatever
// checkout invoked ccx, which from a linked worktree is not the one being
// committed. Output goes to w on the pass a human can watch, and a nil w
// discards it: off a terminal the only reader is a capture, which would take
// the first pass's pre-fix failures for the verdict the retry pass decides.
func shipRunPrek(ctx context.Context, dir render.Dir, argv []string, w io.Writer) error {
	if w == nil {
		w = io.Discard
	}
	return render.RunCLIStream(ctx, dir, "uvx", argv, w)
}

// shipRefuseIndexLock refuses while another process holds the index lock of the
// checkout under root, and passes a jj working copy with no git backing, which
// has no index to contend over. The refusal carries the lock's mtime because
// git's own failure names neither the holder nor its age, and only the age
// separates a live sibling session (seconds) from a crashed process's leftovers
// (hours).
func shipRefuseIndexLock(dir render.Dir) error {
	c, err := vcs.ResolveCheckout(string(dir))
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

func shipChangedPaths(ctx context.Context, dir render.Dir, kind vcs.Kind, o shipOpts) ([]string, error) {
	var out string
	switch kind {
	case vcs.JJ:
		argv := []string{"diff", "--name-only"}
		if len(o.rootPaths) > 0 {
			argv = append(argv, "--")
			argv = append(argv, o.rootPaths...)
		}
		var err error
		out, err = render.RunCLI(ctx, dir, "jj", argv)
		if err != nil {
			return nil, fmt.Errorf("ship: jj diff: %w", err)
		}
	case vcs.Git:
		argv := []string{"diff", "--cached", "--name-only", "--diff-filter=d", "-z"}
		if len(o.rootPaths) > 0 {
			argv = append(argv, "--")
			argv = append(argv, o.rootPaths...)
		}
		var err error
		out, err = render.RunCLI(ctx, dir, "git", argv)
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
func shipHookFiles(ctx context.Context, dir render.Dir, kind vcs.Kind, o shipOpts) ([]string, error) {
	changed, err := shipChangedPaths(ctx, dir, kind, o)
	if err != nil {
		return nil, err
	}
	var files []string
	for _, line := range changed {
		if _, err := os.Lstat(filepath.Join(string(dir), line)); err != nil {
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
		r, err := rootRel(root, p)
		if err != nil {
			return nil, err
		}
		rel = append(rel, r)
	}
	return rel, nil
}
