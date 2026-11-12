package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/yasyf/cc-context/internal/cache"
	"github.com/yasyf/cc-context/internal/render"
	"github.com/yasyf/cc-context/internal/vcs"
)

// shipHookConfigNames are the prek config filenames probed at root; ship skips
// hooks silently when none is present.
var shipHookConfigNames = []string{".pre-commit-config.yaml", ".pre-commit-config.yml", "prek.toml"}

var shipHookEntryLine = regexp.MustCompile(`(?m)^[\s-]*entry\s*[:=]\s*(.+)$`)

var shipHookIDLine = regexp.MustCompile(`(?m)^[\s-]*id\s*[:=]\s*["']?([^\s"'#]+)`)

// shipHookIndexLock is the lock git holds over the index it is rewriting. It
// sits beside the index, which sibling worktrees do not share: each linked
// worktree's index lives under its own admin dir, so a commit contends over the
// lock in Checkout.GitDir — CommonDir only for the repository's own working copy
// — and never over the common dir's.
const shipHookIndexLock = "index.lock"

// shipHookFindingsLead precedes the first pass's output when that pass came back
// nonzero. Ship adopts any auto-fixes and commits regardless, so this output is
// the only place a genuine finding is ever shown; CI is what grades it.
const shipHookFindingsLead = "ship: hooks: prek reported the following; any auto-fixes were adopted and the commit proceeds — CI grades the result\n"

// shipHookStartNotice announces the hook run to a caller that is not getting the
// stream. Off a terminal the first pass is discarded, so without this line a
// prek suite whose Go linters take minutes is indistinguishable from a hang —
// and the caller that concludes "hung", kills ship, and finishes the commit by
// hand gets a commit no linter ever saw. It names the config's own hooks, so a
// repo with no linter in its suite is never told a lint pass is running.
const shipHookStartNotice = "ship: hooks: running prek's %s over %d file(s); a whole-module hook can take minutes\n"

// shipHookLintLock serializes ship's golangci-lint passes across the machine.
// golangci-lint's own lock cannot: it unlinks the file on release, so a waiter
// holding the old inode and a newcomer creating the next one both run.
const shipHookLintLock = "golangci-lint"

const (
	shipHookLockDir        = "ship-hooks"
	shipHookLintBusy       = "parallel golangci-lint is running"
	shipHookReportLine     = "- hook id: "
	shipHookLintIssuesExit = "1"
	shipHookLintAttempts   = 6
	shipHookLintPeek       = time.Second

	shipHookLintWaitNotice  = "ship: hooks: another ship is running golangci-lint; waiting up to %s for it to finish\n"
	shipHookLintRetryNotice = "ship: hooks: another golangci-lint holds its lock; retrying in %s (attempt %d of %d)\n"
)

var (
	shipHookLintWait       = 8 * time.Minute
	shipHookLintRetryDelay = 30 * time.Second
)

// prek folds every hook's exit code into its own, so its report is the only
// place golangci-lint's survives.
var shipHookLintExit = regexp.MustCompile(`(?m)^- hook id: \S*golangci\S*\n- exit code: (\d+)$`)

// shipHookFailedReport matches one hook prek reported Failed: its id, its
// "- " metadata lines, then its output, which prek indents by two spaces.
var shipHookFailedReport = regexp.MustCompile(`(?m)Failed\n- hook id: (\S+)\n(?:- .*\n)*((?:  .*\n|\n)*)`)

// shipHookAborted is prek's report of a hook whose entry it could not start,
// which ends the run before any hook's report.
var shipHookAborted = regexp.MustCompile("(?m)^error: Failed to run hook `([^`]+)`")

var shipHookUnstarted = regexp.MustCompile(`(?m)^- exit code: 12[67]$|mise ERROR .*not trusted`)

var shipLintIssue = regexp.MustCompile(`(?m)^\s*\S+\.go:\d+(?::\d+)?: .* \(([\w-]+)\)$`)

var shipPublicHooks = []string{"public-names", shipPublicBodyHook}

const shipPublicBodyHook = "public-pr-body"

var errShipHookLintNoVerdict = errors.New("nothing was committed; rerun ship, or pass --no-verify to commit without the lint and let CI grade it")

// shipRunHooks runs prek (via uvx) over the files ship is about to commit, once:
// it adopts whatever the hooks fixed in place and lets the commit proceed, and
// CI grades what remains. prek's exit code cannot separate a genuine finding
// from files it modified, and the retry pass that used to resolve that ambiguity
// doubled the most expensive thing ship does — the Go hooks declare
// pass_filenames: false, so each pass analyzes the whole module however small
// the commit — to answer a question CI answers anyway. A nonzero pass that
// changed the files it was handed reports "hooks fixed"; one that left them
// byte-for-byte as they were fixed nothing and reports "hooks failed". Either
// way its output is surfaced rather than swallowed, since it is the only place
// a real finding is shown. The exception is a golangci-lint that never gave a
// verdict: a pass that runs it waits its turn behind shipRunLintPass, and one
// that still got no verdict stops the ship with nothing committed, because CI
// grading a finding is no substitute for a typecheck that never happened.
// External hook execution retains
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
	if render.LookPath(ctx, "uvx") == "" {
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
	if kind == vcs.Git {
		if err := shipMaterializeHookEntries(ctx, dir, config); err != nil {
			return "", false, err
		}
	}
	data, err := os.ReadFile(config) //nolint:gosec // the path is one of shipHookConfigNames under the repo root, not untrusted input
	if err != nil {
		return "", false, fmt.Errorf("ship: hooks: read %s: %w", filepath.Base(config), err)
	}

	streamed := shipStreamCI(errW)
	var buf bytes.Buffer
	var passW io.Writer = &buf
	if streamed {
		passW = io.MultiWriter(errW, &buf)
	} else if _, werr := fmt.Fprintf(errW, shipHookStartNotice, strings.Join(shipHookIDs(data), ", "), len(files)); werr != nil {
		return "", false, fmt.Errorf("ship: hooks: announce the run: %w", werr)
	}
	before := shipHookSnapshot(dir, files)
	// Leading-dash filenames intentionally reach prek unchanged so it fails loudly.
	argv := append([]string{"prek", "run", "--cd", string(dir), "--files"}, files...)
	var passErr error
	retried := false
	if shipHookRunsLint(data, files) {
		retried, passErr = shipRunLintPass(ctx, errW, dir, argv, &buf, passW)
	} else {
		passErr = shipRunPrek(ctx, dir, argv, passW)
	}
	if errors.Is(passErr, errShipHookLintNoVerdict) {
		if !streamed {
			if _, werr := errW.Write(buf.Bytes()); werr != nil {
				return "", false, fmt.Errorf("ship: hooks: report the lint failure: %w", werr)
			}
		}
		return "", false, passErr
	}
	failed, unrun := shipHookOutcomes(buf.Bytes())
	if refused := slices.DeleteFunc(append(failed, unrun...), func(id string) bool { return !slices.Contains(shipPublicHooks, id) }); len(refused) > 0 {
		if !streamed {
			if _, werr := errW.Write(buf.Bytes()); werr != nil {
				return "", false, fmt.Errorf("ship: hooks: report the refusal: %w", werr)
			}
		}
		return "", false, fmt.Errorf("ship: hooks: %s refused what this commit would make public, so nothing was committed or pushed; reword each line it names above, then ship again", strings.Join(refused, ", "))
	}
	if passErr == nil && !retried {
		return "hooks ok", covered, nil
	}
	after := shipHookSnapshot(dir, files)
	if kind == vcs.Git {
		if _, err := shipGitAdd(ctx, dir, o); err != nil {
			return "", false, err
		}
	}
	rederived, err := shipHookFiles(ctx, dir, kind, o)
	if err != nil {
		return "", false, err
	}
	if len(rederived) == 0 {
		return "", false, errors.New("ship: hooks: auto-fixes reverted every pending change; nothing to commit")
	}
	if !streamed && passErr != nil {
		if _, werr := io.WriteString(errW, shipHookFindingsLead); werr != nil {
			return "", false, fmt.Errorf("ship: hooks: report the findings: %w", werr)
		}
		if _, werr := errW.Write(buf.Bytes()); werr != nil {
			return "", false, fmt.Errorf("ship: hooks: report the findings: %w", werr)
		}
	}
	fixed := !maps.Equal(before, after) || !slices.Equal(files, rederived)
	switch {
	case passErr != nil:
		return shipHookFailedSegment(buf.Bytes(), fixed), covered, nil
	case fixed:
		return "hooks fixed", covered, nil
	}
	return "hooks ok", covered, nil
}

// shipHookFailedSegment names, apart from the hooks that failed, each hook that
// never ran, so a commit no linter saw does not read as one with findings.
func shipHookFailedSegment(output []byte, fixed bool) string {
	failed, unrun := shipHookOutcomes(output)
	var segs []string
	switch {
	case fixed:
		segs = append(segs, "hooks fixed")
	case len(failed) > 0:
		segs = append(segs, "hooks failed: "+strings.Join(failed, ", "))
	case len(unrun) == 0:
		segs = append(segs, "hooks failed")
	}
	if len(unrun) > 0 {
		segs = append(segs, "hooks not run: "+strings.Join(unrun, ", "))
	}
	return strings.Join(segs, shipSep)
}

// shipHookOutcomes splits the hooks prek's output reports Failed into those
// that gave a verdict and those that never ran: prek could not start its
// entry, the shell could not (exit 126 or 127), mise refused the configuration
// it runs under, or golangci-lint reported only typecheck issues, since then
// its packages never loaded and no linter looked at them.
func shipHookOutcomes(output []byte) (failed, unrun []string) {
	for _, m := range shipHookFailedReport.FindAllSubmatch(output, -1) {
		id := string(m[1])
		if shipHookUnstarted.Match(m[0]) || strings.Contains(id, "golangci") && shipLintOnlyTypecheck(m[2]) {
			unrun = append(unrun, id)
			continue
		}
		failed = append(failed, id)
	}
	for _, m := range shipHookAborted.FindAllSubmatch(output, -1) {
		unrun = append(unrun, string(m[1]))
	}
	return failed, unrun
}

func shipLintOnlyTypecheck(output []byte) bool {
	issues := shipLintIssue.FindAllSubmatch(output, -1)
	return len(issues) > 0 && !slices.ContainsFunc(issues, func(m [][]byte) bool { return string(m[1]) != "typecheck" })
}

func shipCheckPRBodies(ctx context.Context, errW io.Writer, dir render.Dir, o shipOpts) error {
	if o.noVerify || len(o.prBodyFile) == 0 {
		return nil
	}
	config := shipHookConfigPath(string(dir))
	if config == "" || render.LookPath(ctx, "uvx") == "" {
		return nil
	}
	if _, err := os.Stat(filepath.Join(string(dir), ".git")); err != nil {
		return nil
	}
	data, err := os.ReadFile(config) //nolint:gosec // the path is one of shipHookConfigNames under the repo root, not untrusted input
	if err != nil {
		return fmt.Errorf("ship: hooks: read %s: %w", filepath.Base(config), err)
	}
	if !slices.Contains(shipHookIDs(data), shipPublicBodyHook) {
		return nil
	}
	argv := []string{"prek", "run", shipPublicBodyHook, "--hook-stage", "manual", "--cd", string(dir), "--files"}
	for _, value := range o.prBodyFile {
		_, path := splitPRValue(value, "")
		abs, err := filepath.Abs(path)
		if err != nil {
			return fmt.Errorf("ship: --pr-body-file %s: %w", path, err)
		}
		argv = append(argv, abs)
	}
	var buf bytes.Buffer
	if err := shipRunPrek(ctx, dir, argv, &buf); err != nil {
		if _, werr := errW.Write(buf.Bytes()); werr != nil {
			return fmt.Errorf("ship: hooks: report the refusal: %w", werr)
		}
		return fmt.Errorf("ship: hooks: %s did not pass the pull request body (%w), so nothing was committed or pushed; fix what it printed above, then ship again", shipPublicBodyHook, err)
	}
	return nil
}

func shipHookSnapshot(dir render.Dir, files []string) map[string][sha256.Size]byte {
	sums := make(map[string][sha256.Size]byte, len(files))
	for _, f := range files {
		data, err := os.ReadFile(filepath.Join(string(dir), f)) //nolint:gosec // f is a root-relative path git or jj listed as part of this commit
		if err != nil {
			continue
		}
		sums[f] = sha256.Sum256(data)
	}
	return sums
}

func shipHookRunsLint(config []byte, files []string) bool {
	return bytes.Contains(config, []byte("golangci")) &&
		slices.ContainsFunc(files, func(f string) bool { return strings.HasSuffix(f, ".go") })
}

func shipHookLintNoVerdict(cause error) error {
	return fmt.Errorf("ship: hooks: golangci-lint gave no verdict (%w): %w", cause, errShipHookLintNoVerdict)
}

func shipRunLintPass(ctx context.Context, errW io.Writer, dir render.Dir, argv []string, buf *bytes.Buffer, passW io.Writer) (retried bool, err error) {
	lockDir, err := cache.UserDir(shipHookLockDir)
	if err != nil {
		return false, shipHookLintNoVerdict(err)
	}
	ran := false
	pass := func() error {
		ran = true
		var passErr error
		retried, passErr = shipRunPrekUntilLinted(ctx, errW, dir, argv, buf, passW)
		return passErr
	}
	peek, cancelPeek := context.WithTimeout(ctx, shipHookLintPeek)
	defer cancelPeek()
	err = cache.WithLock(peek, lockDir, shipHookLintLock, pass)
	if !ran && errors.Is(err, context.DeadlineExceeded) {
		if _, werr := fmt.Fprintf(errW, shipHookLintWaitNotice, shipHookLintWait); werr != nil {
			return false, fmt.Errorf("ship: hooks: announce the wait: %w", werr)
		}
		wait, cancelWait := context.WithTimeout(ctx, shipHookLintWait)
		defer cancelWait()
		err = cache.WithLock(wait, lockDir, shipHookLintLock, pass)
	}
	if !ran {
		return false, shipHookLintNoVerdict(fmt.Errorf("wait for another ship's lint: %w", err))
	}
	return retried, err
}

func shipRunPrekUntilLinted(ctx context.Context, errW io.Writer, dir render.Dir, argv []string, buf *bytes.Buffer, passW io.Writer) (retried bool, err error) {
	for attempt := 1; ; attempt++ {
		buf.Reset()
		err := shipRunPrek(ctx, dir, argv, passW)
		if err == nil {
			return retried, nil
		}
		busy := bytes.Contains(buf.Bytes(), []byte(shipHookLintBusy))
		exit := shipHookLintFailedExit(buf.Bytes())
		switch {
		case !busy && exit != "":
			return retried, shipHookLintNoVerdict(fmt.Errorf("its hook exited %s", exit))
		case !busy && !bytes.Contains(buf.Bytes(), []byte(shipHookReportLine)):
			return retried, shipHookLintNoVerdict(fmt.Errorf("prek reported no hook: %w", err))
		case !busy:
			return retried, err
		case attempt == shipHookLintAttempts:
			return retried, shipHookLintNoVerdict(fmt.Errorf("another golangci-lint held its lock through %d attempts", attempt))
		}
		if _, werr := fmt.Fprintf(errW, shipHookLintRetryNotice, shipHookLintRetryDelay, attempt, shipHookLintAttempts); werr != nil {
			return retried, fmt.Errorf("ship: hooks: announce the retry: %w", werr)
		}
		select {
		case <-ctx.Done():
			return retried, shipHookLintNoVerdict(ctx.Err())
		case <-time.After(shipHookLintRetryDelay):
		}
		retried = true
	}
}

func shipHookLintFailedExit(output []byte) string {
	for _, m := range shipHookLintExit.FindAllSubmatch(output, -1) {
		if string(m[1]) != shipHookLintIssuesExit {
			return string(m[1])
		}
	}
	return ""
}

// shipRunPrek spawns prek with uvx in dir, whose pin is what keeps GIT_DIR and
// GIT_WORK_TREE off the hook child — each would otherwise point git at whatever
// checkout invoked ccx, which from a linked worktree is not the one being
// committed. A hook that runs through mise installs what is missing, and a mise
// older than the one that wrote a repository's mise.lock rewrites that file as it
// installs, which ship would then commit; locked mode installs from the file as
// written. It refuses a tool the file does not list, so it is set only where a
// mise.lock exists. The child trusts the repository's mise config, whose
// scripts it runs anyway, or a fresh clone's hooks never start under mise.
func shipRunPrek(ctx context.Context, dir render.Dir, argv []string, w io.Writer) error {
	env := []string{"MISE_TRUSTED_CONFIG_PATHS=" + string(dir)}
	if _, err := os.Stat(filepath.Join(string(dir), "mise.lock")); err == nil {
		env = append(env, "MISE_LOCKED=1")
	}
	return render.RunCLIStreamEnv(ctx, dir, "uvx", argv, w, env)
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

func shipMaterializeHookEntries(ctx context.Context, dir render.Dir, config string) error {
	data, err := os.ReadFile(config) //nolint:gosec // the path is one of shipHookConfigNames under the repo root, not untrusted input
	if err != nil {
		return fmt.Errorf("ship: hooks: read %s: %w", filepath.Base(config), err)
	}
	var dirs []string
	for _, entry := range shipHookEntryPaths(data) {
		if _, err := os.Lstat(filepath.Join(string(dir), entry)); err == nil {
			continue
		}
		if parent := path.Dir(entry); parent != "." {
			dirs = append(dirs, parent)
		}
	}
	sparse, err := stackConfigBool(ctx, dir, "core.sparseCheckout", false)
	if err != nil || !sparse {
		return err
	}
	slices.Sort(dirs)
	dirs = slices.Compact(dirs)
	cone, err := stackConfigBool(ctx, dir, "core.sparseCheckoutCone", false)
	if err != nil {
		return err
	}
	if cone {
		if len(dirs) == 0 {
			return nil
		}
		return stackAddCone(ctx, "ship: hooks", dir, dirs)
	}
	patterns, err := shipMissingRootFiles(ctx, dir)
	if err != nil {
		return err
	}
	for _, d := range dirs {
		patterns = append(patterns, "/"+d+"/")
	}
	if len(patterns) == 0 {
		return nil
	}
	if _, err := render.RunCLI(ctx, dir, "git", append([]string{"sparse-checkout", "add", "--skip-checks", "--"}, patterns...)); err != nil {
		return fmt.Errorf("ship: hooks: check out %s in %s: %w", strings.Join(patterns, ", "), dir, err)
	}
	return nil
}

func shipMissingRootFiles(ctx context.Context, dir render.Dir) ([]string, error) {
	out, err := render.RunCLI(ctx, dir, "git", []string{"ls-tree", "-z", "HEAD"})
	if err != nil {
		return nil, fmt.Errorf("ship: hooks: list the root files: %w", err)
	}
	var missing []string
	for _, entry := range strings.Split(out, "\x00") {
		meta, name, ok := strings.Cut(entry, "\t")
		if !ok || strings.Fields(meta)[1] != "blob" {
			continue
		}
		if _, err := os.Lstat(filepath.Join(string(dir), name)); err != nil {
			missing = append(missing, "/"+name)
		}
	}
	return missing, nil
}

func shipHookIDs(config []byte) []string {
	matches := shipHookIDLine.FindAllSubmatch(config, -1)
	ids := make([]string, 0, len(matches))
	for _, match := range matches {
		ids = append(ids, string(match[1]))
	}
	return ids
}

func shipHookEntryPaths(config []byte) []string {
	var paths []string
	for _, match := range shipHookEntryLine.FindAllSubmatch(config, -1) {
		for _, word := range strings.Fields(string(match[1])) {
			word = strings.Trim(word, `"'`)
			if !strings.Contains(word, "/") || path.IsAbs(word) || strings.ContainsAny(word, "$:") || strings.HasPrefix(word, "-") {
				continue
			}
			if clean := path.Clean(word); !strings.HasPrefix(clean, "../") {
				paths = append(paths, clean)
			}
		}
	}
	return paths
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

// rootRelPaths rebases ship paths onto root, for the jj diff that runs with its
// working directory pinned there.
func rootRelPaths(ctx context.Context, root string, paths []string) ([]string, error) {
	rel := make([]string, 0, len(paths))
	for _, p := range paths {
		r, err := rootRel(ctx, root, p)
		if err != nil {
			return nil, err
		}
		rel = append(rel, r)
	}
	return rel, nil
}
