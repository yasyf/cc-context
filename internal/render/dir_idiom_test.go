package render

import (
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestNoArgvWorkingDirectoryFlags keeps one idiom for naming a child's working
// copy. Dir is it; a tool's own flag for the same thing — gt's --cwd, git's -C —
// puts a second, unenforceable one back.
func TestNoArgvWorkingDirectoryFlags(t *testing.T) {
	root := repoRootForTest(t)
	banned := []string{`"--cwd"`, `"-C", dir`, `"-C", root`}
	err := filepath.WalkDir(filepath.Join(root, "internal"), func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == "testdata" || strings.HasPrefix(d.Name(), ".") {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, b := range banned {
			if strings.Contains(string(src), b) {
				rel, _ := filepath.Rel(root, path)
				t.Errorf("%s builds %s into an argv; pass a render.Dir instead", rel, b)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk internal: %v", err)
	}
}

func repoRootForTest(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve this test's own path")
	}
	return filepath.Dir(filepath.Dir(filepath.Dir(file)))
}
