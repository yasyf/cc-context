// Command chunkdump walks a directory, chunks every eligible file with the
// semsearch chunker, and prints one JSON object per chunk — {path, start_line,
// end_line, content} — for the semble injection experiment. Content is the
// byte-exact chunk text (boundaries are character-granular, so line spans
// alone are lossy at mid-line splits). Paths are relative to the walk root
// (posix). It is a plain recursive walk with no gitignore handling.
package main

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/yasyf/cc-context/internal/semsearch/chunk"
)

type record struct {
	Path      string `json:"path"`
	StartLine int    `json:"start_line"`
	EndLine   int    `json:"end_line"`
	Content   string `json:"content"`
}

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: chunkdump <dir>")
		os.Exit(2)
	}
	if err := run(os.Args[1]); err != nil {
		fmt.Fprintln(os.Stderr, "chunkdump:", err)
		os.Exit(1)
	}
}

func run(root string) error {
	enc := json.NewEncoder(os.Stdout)
	return filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read %s: %w", path, err)
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return fmt.Errorf("relativize %s: %w", path, err)
		}
		rel = filepath.ToSlash(rel)
		for _, c := range chunk.Chunk(rel, content) {
			if err := enc.Encode(record{Path: c.Path, StartLine: c.StartLine, EndLine: c.EndLine, Content: c.Content}); err != nil {
				return err
			}
		}
		return nil
	})
}
