package backend

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
)

// ErrPathNotFound reports that a path op's target does not exist on disk;
// internal/cli.ExitCode maps it to exit 3.
var ErrPathNotFound = errors.New("path not found")

// ResolvePath expands a leading tilde and validates a file-target op's path
// (OpRead, OpDeps), returning ErrPathNotFound for a missing target so the CLI maps
// it to exit 3. Every other op passes through unchanged.
func ResolvePath(op Op, a Args) (Args, error) {
	switch op {
	case OpRead, OpDeps:
	default:
		return a, nil
	}

	path, err := expandTilde(a.Path)
	if err != nil {
		return a, fmt.Errorf("%s %q: expand home: %w", op, a.Path, err)
	}
	a.Path = path

	info, err := os.Stat(a.Path)
	if errors.Is(err, fs.ErrNotExist) {
		return a, fmt.Errorf("%s %q: %w: %w", op, a.Path, ErrPathNotFound, err)
	}
	if err != nil {
		return a, fmt.Errorf("%s %q: %w", op, a.Path, err)
	}
	if info.IsDir() {
		return a, fmt.Errorf("%s %q: is a directory — outline it with 'ccx code outline <path>' or list it with 'ccx repo find'", op, a.Path)
	}
	if !info.Mode().IsRegular() {
		return a, fmt.Errorf("%s %q: not a regular file", op, a.Path)
	}
	return a, nil
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
