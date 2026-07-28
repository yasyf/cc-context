package backend

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// ErrPathNotFound reports that a path op's target does not exist on disk;
// internal/cli.ExitCode maps it to exit 3.
var ErrPathNotFound = errors.New("path not found")

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
		for i, path := range a.Paths {
			resolved, resolutionNote, err := resolveLenient(op, path)
			if err != nil {
				return a, "", err
			}
			a.Paths[i] = resolved
			note.WriteString(resolutionNote)
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
		return path, "", fmt.Errorf("%s %q: %w: %w", op, path, ErrPathNotFound, err)
	case 1:
		return candidates[0], fmt.Sprintf("# note: %s → %s\n", path, candidates[0]), nil
	default:
		return path, "", fmt.Errorf("%s %q: several extension matches: %s", op, path, strings.Join(candidates, ", "))
	}
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
