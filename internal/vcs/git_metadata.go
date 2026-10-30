package vcs

import (
	"bytes"
	"os"
)

func readGitMetadata(path string) ([]byte, error) {
	file, err := os.Open(path) //nolint:gosec // git's admin metadata and recorded worktree linkage paths
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()

	var contents bytes.Buffer
	contents.Grow(1024)
	_, err = contents.ReadFrom(file)
	return contents.Bytes(), err
}
