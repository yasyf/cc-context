package backend

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"

	"github.com/yasyf/cc-context/internal/gitdir"
)

// ErrPathNotFound reports that a path op's target does not exist on disk;
// internal/cli.ExitCode maps it to exit 3.
var ErrPathNotFound = errors.New("path not found")

const leftOutListed = 5

// ErrOutsideSparseCheckout reports a path tracked in the index but left out of
// the sparse checkout; it always arrives wrapped with ErrPathNotFound.
var ErrOutsideSparseCheckout = errors.New("outside the sparse checkout")

// ResolvePath resolves filesystem operands before dispatch and returns a note
// for each uniquely inferred sibling path.
func ResolvePath(op Op, a Args) (Args, string, error) {
	a.Globs = normalizeGlobs(a.Globs)
	var note strings.Builder
	switch op {
	case OpRead, OpDeps, OpEdit:
		path, err := expandTilde(a.Path)
		if err != nil {
			return a, "", fmt.Errorf("%s %q: expand home: %w", op, a.Path, err)
		}
		a.Path = path
		resolved, resolutionNote, err := resolveLenient(op, a.Path)
		if err != nil {
			return a, "", err
		}
		a.Path = resolved
		note.WriteString(resolutionNote)
		if err := checkFile(op, a.Path); err != nil {
			return a, "", err
		}
	case OpGrep, OpStructural, OpReplace:
		a.Paths = append([]string(nil), a.Paths...)
		var unread []string
		var unreadErrs []error
		for i, path := range a.Paths {
			resolved, resolutionNote, err := resolveLenient(op, path)
			if op != OpReplace && errors.Is(err, ErrOutsideSparseCheckout) {
				unread = append(unread, path)
				unreadErrs = append(unreadErrs, err)
				continue
			}
			if err != nil {
				return a, "", err
			}
			a.Paths[i] = resolved
			note.WriteString(resolutionNote)
		}
		if len(unread) == len(a.Paths) && len(unread) > 0 {
			return a, "", errors.Join(unreadErrs...)
		}
		for _, err := range unreadErrs {
			fmt.Fprintf(&note, "# note: %v; searched the other paths\n", err)
		}
		a.Paths = slices.DeleteFunc(a.Paths, func(p string) bool { return slices.Contains(unread, p) })
		if len(a.Paths) == 0 {
			note.WriteString(sparseScopeNote())
		}
	case OpSearch:
		if a.Path == "" {
			note.WriteString(sparseScopeNote())
		}
	default:
		return a, "", nil
	}
	return a, note.String(), nil
}

// normalizeGlobs canonicalizes each glob into the form the engines match: a
// leading "./" goes, since no engine prints it, and an include naming an
// existing directory becomes "dir/**", since a bare directory glob selects no
// file. An exclusion keeps its bare form — excluding a directory already prunes
// the subtree.
func normalizeGlobs(globs []string) []string {
	if len(globs) == 0 {
		return nil
	}
	out := make([]string, len(globs))
	for i, g := range globs {
		pattern, negated := strings.CutPrefix(g, "!")
		pattern = strings.TrimPrefix(pattern, "./")
		if !negated && pattern != "" && !strings.ContainsAny(pattern, globMeta) {
			if info, err := os.Stat(pattern); err == nil && info.IsDir() {
				pattern = strings.TrimSuffix(pattern, "/") + "/**"
			}
		}
		if negated {
			pattern = "!" + pattern
		}
		out[i] = pattern
	}
	return out
}

func resolveLenient(op Op, path string) (string, string, error) {
	if path == "" {
		return path, "", fmt.Errorf("%s %q: %w", op, path, ErrPathNotFound)
	}
	if strings.ContainsAny(path, globMeta) {
		return path, "", nil
	}
	_, err := os.Stat(path)
	if err == nil {
		return path, "", nil
	}
	if !errors.Is(err, os.ErrNotExist) && !errors.Is(err, syscall.ENOTDIR) {
		return path, "", fmt.Errorf("%s %q: %w", op, path, err)
	}
	candidates, globErr := filepath.Glob(path + ".*")
	if globErr != nil {
		return path, "", fmt.Errorf("%s %q: infer extension: %w", op, path, globErr)
	}
	switch len(candidates) {
	case 0:
		if dir, ok := sparseExcludedDir(path); ok {
			return path, "", fmt.Errorf("%s %q: %w: %w; git sparse-checkout add %s checks it out", op, path, ErrPathNotFound, ErrOutsideSparseCheckout, dir)
		}
		return path, "", fmt.Errorf("%s %q: %w: %w", op, path, ErrPathNotFound, err)
	case 1:
		return candidates[0], fmt.Sprintf("# note: %s → %s\n", path, candidates[0]), nil
	default:
		return path, "", fmt.Errorf("%s %q: several extension matches: %s", op, path, strings.Join(candidates, ", "))
	}
}

// sparseScopeNote names the tracked directories under the working directory
// that a sparse checkout leaves out, which a search scoped to the working
// directory cannot see.
func sparseScopeNote() string {
	cwd, err := os.Getwd()
	if err != nil {
		return ""
	}
	root := gitdir.Root(cwd)
	if root == "" {
		return ""
	}
	if _, err := os.Stat(filepath.Join(gitdir.Dir(root), "info", "sparse-checkout")); err != nil {
		return ""
	}
	enabled, err := exec.Command("git", "config", "--type=bool", "core.sparseCheckout").Output()
	if err != nil || strings.TrimSpace(string(enabled)) != "true" {
		return ""
	}
	dirs, err := exec.Command("git", "ls-tree", "-d", "--name-only", "-z", "HEAD").Output()
	if err != nil {
		return ""
	}
	var leftOut []string
	for dir := range strings.SplitSeq(string(dirs), "\x00") {
		if _, err := os.Stat(dir); dir != "" && errors.Is(err, os.ErrNotExist) {
			leftOut = append(leftOut, dir)
		}
	}
	if len(leftOut) == 0 {
		return ""
	}
	listed := strings.Join(leftOut[:min(len(leftOut), leftOutListed)], ", ")
	if more := len(leftOut) - leftOutListed; more > 0 {
		listed += fmt.Sprintf(" and %d more", more)
	}
	return fmt.Sprintf("# note: not searched, outside the sparse checkout: %s; git sparse-checkout add <dir> checks one out\n", listed)
}

// sparseExcludedDir reports whether path is tracked but outside the sparse
// checkout, with the directory to name to "git sparse-checkout add", which in
// cone mode takes a directory, never a file.
func sparseExcludedDir(path string) (string, bool) {
	if sparseExcluded(strings.TrimSuffix(path, "/") + "/") {
		return path, true
	}
	if sparseExcluded(path) {
		return filepath.Dir(path), true
	}
	return "", false
}

func sparseExcluded(pathspec string) bool {
	out, err := exec.Command("git", "--literal-pathspecs", "ls-files", "-t", "-z", "--", pathspec).Output() //nolint:gosec // fixed git verb; the pathspec is literal
	if err != nil {
		return false
	}
	return slices.ContainsFunc(strings.Split(string(out), "\x00"), func(entry string) bool { return strings.HasPrefix(entry, "S ") })
}

func checkFile(op Op, path string) error {
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("%s %q: %w: %w", op, path, ErrPathNotFound, err)
	}
	if err != nil {
		return fmt.Errorf("%s %q: %w", op, path, err)
	}
	if info.IsDir() {
		return fmt.Errorf("%s %q: is a directory — outline it with 'ccx code outline <path>' or list it with 'ccx repo find'", op, path)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%s %q: not a regular file", op, path)
	}
	return nil
}

func expandTilde(path string) (string, error) {
	if path != "~" && !strings.HasPrefix(path, "~/") {
		return path, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	if path == "~" {
		return home, nil
	}
	// Textual expansion only: filepath.Join would lexically clean the path
	// (collapsing "link/../x" across symlinks), violating no-canonicalization.
	return home + "/" + strings.TrimPrefix(path, "~/"), nil
}
