package cli

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/BurntSushi/toml"
	"github.com/bmatcuk/doublestar/v4"
	"github.com/spf13/cobra"

	"github.com/yasyf/cc-context/internal/render"
)

const (
	regenFile      = ".ccx.toml"
	regenTailLines = 20
)

// regenScrubbed is what git rev-parse --local-env-vars names: a hook or
// wrapper that exported one would point the generator's own git calls at
// another repository, object store, index, or config.
var regenScrubbed = []string{
	"GIT_ALTERNATE_OBJECT_DIRECTORIES", "GIT_CONFIG", "GIT_CONFIG_PARAMETERS", "GIT_CONFIG_COUNT", "GIT_OBJECT_DIRECTORY",
	"GIT_DIR", "GIT_WORK_TREE", "GIT_IMPLICIT_WORK_TREE", "GIT_GRAFT_FILE", "GIT_INDEX_FILE", "GIT_NO_REPLACE_OBJECTS",
	"GIT_REPLACE_REF_BASE", "GIT_PREFIX", "GIT_SHALLOW_FILE", "GIT_COMMON_DIR",
}

// regenGenerator is one [[generated]] entry of .ccx.toml: the files a command
// rewrites, as repository-relative doublestar patterns, and the command sh runs
// from the repository root to rewrite them.
type regenGenerator struct {
	Paths []string `toml:"paths"`
	Run   string   `toml:"run"`
}

func (g regenGenerator) owns(path string) bool {
	return slices.ContainsFunc(g.Paths, func(pattern string) bool { return doublestar.MatchUnvalidated(pattern, path) })
}

func regenLoad(ctx context.Context, dir render.Dir, rev string) ([]regenGenerator, error) {
	listed, err := render.RunCLI(ctx, dir, "git", []string{"ls-tree", "--full-tree", "--name-only", rev, "--", regenFile})
	if err != nil {
		return nil, fmt.Errorf("stack rebase: look for %s at %.12s: %w", regenFile, rev, err)
	}
	if strings.TrimSpace(listed) == "" {
		return nil, nil
	}
	raw, err := render.RunCLI(ctx, dir, "git", []string{"show", rev + ":" + regenFile})
	if err != nil {
		return nil, fmt.Errorf("stack rebase: read %s at %.12s: %w", regenFile, rev, err)
	}
	return regenParse(raw, fmt.Sprintf("%s at %.12s", regenFile, rev))
}

func regenStaged(ctx context.Context, dir render.Dir) ([]regenGenerator, error) {
	staged, err := render.RunCLI(ctx, dir, "git", []string{"--literal-pathspecs", "ls-files", "-s", "--", regenFile})
	if err != nil {
		return nil, fmt.Errorf("stack rebase: look for %s in the index: %w", regenFile, err)
	}
	if strings.TrimSpace(staged) == "" {
		return nil, nil
	}
	if meta, _, _ := strings.Cut(staged, "\t"); !strings.HasSuffix(meta, " 0") {
		return nil, fmt.Errorf("stack rebase: %s is itself conflicted; resolve and git add it first", regenFile)
	}
	raw, err := render.RunCLI(ctx, dir, "git", []string{"show", ":" + regenFile})
	if err != nil {
		return nil, fmt.Errorf("stack rebase: read %s from the index: %w", regenFile, err)
	}
	return regenParse(raw, regenFile+" in the index")
}

func regenParse(raw, where string) ([]regenGenerator, error) {
	var cfg struct {
		Generated []regenGenerator `toml:"generated"`
	}
	md, err := toml.Decode(raw, &cfg)
	if err != nil {
		return nil, fmt.Errorf("stack rebase: %s: %w", where, err)
	}
	if undecoded := md.Undecoded(); len(undecoded) > 0 {
		return nil, fmt.Errorf("stack rebase: %s: unknown keys %v", where, undecoded)
	}
	for i, g := range cfg.Generated {
		if len(g.Paths) == 0 || strings.TrimSpace(g.Run) == "" {
			return nil, fmt.Errorf("stack rebase: %s: generated entry %d needs both paths and run", where, i+1)
		}
		for _, pattern := range g.Paths {
			if !doublestar.ValidatePattern(pattern) {
				return nil, fmt.Errorf("stack rebase: %s: generated entry %d: bad path pattern %q", where, i+1, pattern)
			}
		}
	}
	return cfg.Generated, nil
}

func regenDeclared(gens []regenGenerator, paths []string) []string {
	return slices.DeleteFunc(slices.Clone(paths), func(p string) bool { return regenOwner(gens, p) == nil })
}

func regenOwner(gens []regenGenerator, path string) *regenGenerator {
	for i := range gens {
		if gens[i].owns(path) {
			return &gens[i]
		}
	}
	return nil
}

func regenCovering(gens []regenGenerator, paths []string) []regenGenerator {
	var out []regenGenerator
	for i := range gens {
		if slices.ContainsFunc(paths, func(p string) bool { return regenOwner(gens, p) == &gens[i] }) {
			out = append(out, gens[i])
		}
	}
	return out
}

func stackRegenerate(ctx context.Context, cmd *cobra.Command, ws string, gens []regenGenerator, paths []string) error {
	dir := render.Dir(ws)
	unmerged, err := stackUnmerged(ctx, ws)
	if err != nil {
		return err
	}
	var live, deleted []string
	for _, p := range paths {
		kept, err := regenSettle(ctx, dir, ws, p, slices.Contains(unmerged, p))
		if err != nil {
			return err
		}
		if kept {
			live = append(live, p)
		} else {
			deleted = append(deleted, p)
		}
	}
	ran := regenCovering(gens, live)
	for _, g := range ran {
		argv := make([]string, 0, 2*len(regenScrubbed)+3)
		for _, name := range regenScrubbed {
			argv = append(argv, "-u", name)
		}
		argv = append(argv, "sh", "-c", g.Run)
		stdout, code, stderr, err := render.RunCLIExitCode(ctx, dir, "env", argv)
		if err != nil {
			return fmt.Errorf("generator `%s` did not run: %w", g.Run, err)
		}
		if code != 0 {
			tail := stderr
			if strings.TrimSpace(tail) == "" {
				tail = stdout
			}
			return fmt.Errorf("generator `%s` exited %d; output tail:\n%s", g.Run, code, regenTail(tail))
		}
	}
	touched, err := regenTouched(ctx, dir)
	if err != nil {
		return err
	}
	outputs := slices.Clone(live)
	var stray []string
	for _, p := range touched {
		if slices.Contains(deleted, p) {
			continue
		}
		if regenOwner(ran, p) == nil {
			stray = append(stray, p)
			continue
		}
		outputs = append(outputs, p)
	}
	if len(stray) > 0 {
		return fmt.Errorf("generators wrote outside their declared paths: %s", strings.Join(stray, ", "))
	}
	slices.Sort(outputs)
	outputs = slices.Compact(outputs)
	for _, p := range outputs {
		if err := regenNoMarkers(ws, p); err != nil {
			return err
		}
	}
	for _, p := range deleted {
		if err := os.Remove(filepath.Join(ws, p)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("stack rebase: remove deleted output %s: %w", p, err)
		}
	}
	if len(deleted) > 0 {
		if _, err := render.RunCLI(ctx, dir, "git", append([]string{"--literal-pathspecs", "rm", "-q", "-f", "--ignore-unmatch", "--"}, deleted...)); err != nil {
			return fmt.Errorf("stack rebase: stage deleted outputs in %s: %w", ws, err)
		}
	}
	if len(outputs) > 0 {
		if _, err := render.RunCLI(ctx, dir, "git", append([]string{"--literal-pathspecs", "add", "-A", "--"}, outputs...)); err != nil {
			return fmt.Errorf("stack rebase: stage the regenerated paths in %s: %w", ws, err)
		}
	}
	if left, err := stackUnmerged(ctx, ws); err != nil {
		return err
	} else if len(left) > 0 {
		return fmt.Errorf("still conflicted after regenerating: %s", strings.Join(left, ", "))
	}
	for _, p := range deleted {
		cmd.Println("deleted " + p + shipSep + "the replayed commit deletes it")
	}
	for _, p := range outputs {
		cmd.Println("regenerated " + p + shipSep + regenOwner(ran, p).Run)
	}
	return nil
}

func regenSettle(ctx context.Context, dir render.Dir, ws, path string, conflicted bool) (kept bool, err error) {
	stages, err := render.RunCLI(ctx, dir, "git", []string{"--literal-pathspecs", "ls-files", "-s", "--", path})
	if err != nil {
		return false, fmt.Errorf("stack rebase: read the index stages of %s: %w", path, err)
	}
	if !conflicted {
		return strings.TrimSpace(stages) != "", nil
	}
	for line := range strings.Lines(stages) {
		if meta, _, ok := strings.Cut(line, "\t"); ok && strings.HasSuffix(meta, " 3") {
			if _, err := render.RunCLI(ctx, dir, "git", []string{"--literal-pathspecs", "checkout", "--theirs", "--", path}); err != nil {
				return false, fmt.Errorf("stack rebase: take the replayed side of %s: %w", path, err)
			}
			return true, nil
		}
	}
	if err := os.Remove(filepath.Join(ws, path)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return false, fmt.Errorf("stack rebase: drop %s, which the replayed commit deletes: %w", path, err)
	}
	return false, nil
}

func regenTouched(ctx context.Context, dir render.Dir) ([]string, error) {
	changed, err := render.RunCLI(ctx, dir, "git", []string{"diff", "--name-only", "-z"})
	if err != nil {
		return nil, fmt.Errorf("stack rebase: list the regenerated paths: %w", err)
	}
	untracked, err := render.RunCLI(ctx, dir, "git", []string{"ls-files", "--others", "--exclude-standard", "-z"})
	if err != nil {
		return nil, fmt.Errorf("stack rebase: list the regenerated paths: %w", err)
	}
	paths := slices.DeleteFunc(strings.Split(changed+untracked, "\x00"), func(p string) bool { return p == "" })
	slices.Sort(paths)
	return slices.Compact(paths), nil
}

func regenNoMarkers(ws, path string) error {
	raw, err := os.ReadFile(filepath.Join(ws, path)) //nolint:gosec // path is a git-listed file of ccx's own conflict workspace, not untrusted input
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("stack rebase: read the regenerated %s: %w", path, err)
	}
	for line := range strings.Lines(string(raw)) {
		if strings.HasPrefix(line, "<<<<<<< ") || strings.HasPrefix(line, ">>>>>>> ") {
			return fmt.Errorf("the regenerated %s still carries conflict markers", path)
		}
	}
	return nil
}

func regenTail(out string) string {
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) > regenTailLines {
		lines = lines[len(lines)-regenTailLines:]
	}
	return "  " + strings.Join(lines, "\n  ")
}

// stackRegenPlan names, per moving branch, the declared generators a conflict
// would run: those owning a path the branch changed that trunk also moved. The
// declaration is trunk's, or the branch's own when the branch changes it.
func stackRegenPlan(ctx context.Context, dir render.Dir, run *stackRebaseRun) ([]string, error) {
	var lines []string
	for _, b := range run.Branches {
		if b.Landed != "" || b.Held != "" {
			continue
		}
		own, err := regenChanged(ctx, dir, b.OldBase, b.Head)
		if err != nil {
			return nil, err
		}
		declaredAt := run.Pin
		if slices.Contains(own, regenFile) {
			declaredAt = b.Head
		}
		gens, err := regenLoad(ctx, dir, declaredAt)
		if err != nil {
			lines = append(lines, b.Name+shipSep+err.Error()+shipSep+"its generated conflicts stop for a human")
			continue
		}
		if len(gens) == 0 {
			continue
		}
		fork := &b
		for parent := run.branch(fork.Parent); parent != nil && parent.Held == ""; parent = run.branch(fork.Parent) {
			fork = parent
		}
		onto := run.Pin
		if parent := run.branch(fork.Parent); parent != nil {
			onto = parent.Head
		}
		upstream, err := regenChanged(ctx, dir, fork.OldBase, onto)
		if err != nil {
			return nil, err
		}
		kept, err := regenChanged(ctx, dir, b.OldBase, b.Head, "--diff-filter=d")
		if err != nil {
			return nil, err
		}
		both := regenDeclared(gens, slices.DeleteFunc(kept, func(p string) bool { return !slices.Contains(upstream, p) }))
		for i := range gens {
			owned := slices.DeleteFunc(slices.Clone(both), func(p string) bool { return regenOwner(gens, p) != &gens[i] })
			if len(owned) > 0 {
				lines = append(lines, fmt.Sprintf("%s%sregenerates %s on conflict%s%s", b.Name, shipSep, strings.Join(owned, ", "), shipSep, gens[i].Run))
			}
		}
	}
	return lines, nil
}

func regenChanged(ctx context.Context, dir render.Dir, from, to string, filter ...string) ([]string, error) {
	out, err := render.RunCLI(ctx, dir, "git", slices.Concat([]string{"diff", "--name-only", "-z"}, filter, []string{from, to}))
	if err != nil {
		return nil, fmt.Errorf("stack rebase: diff %.12s..%.12s: %w", from, to, err)
	}
	return slices.DeleteFunc(strings.Split(out, "\x00"), func(p string) bool { return p == "" }), nil
}
