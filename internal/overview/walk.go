package overview

import (
	"context"
	"fmt"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/boyter/gocodewalker"

	"github.com/yasyf/cc-context/internal/find"
)

// sourceExts is the set of file extensions the language census counts — code plus
// the lightweight markup that signals a project's surface. Moved verbatim from the
// former internal/cli census so the native overview reports the same languages.
var sourceExts = map[string]bool{
	"go": true, "py": true, "ts": true, "tsx": true, "js": true, "jsx": true,
	"rs": true, "java": true, "kt": true, "rb": true, "php": true, "cs": true,
	"c": true, "h": true, "cc": true, "cpp": true, "hpp": true,
	"swift": true, "scala": true, "sh": true, "lua": true,
	"md": true, "proto": true, "sql": true,
}

// depDirs are the build and dependency directories skipped only at the walk root;
// skipping them at any depth would erase a real package like internal/vendor.
var depDirs = map[string]bool{
	"node_modules": true, "vendor": true, "target": true,
	"dist": true, "build": true, ".venv": true,
}

// topLevelDepDir reports whether rel's first path segment is a root-level dep dir.
func topLevelDepDir(rel string) bool {
	seg, _, ok := strings.Cut(rel, "/")
	return ok && depDirs[seg]
}

// repoCensus is the single-walk fingerprint feeding the languages, tests, and dirs
// sections: extension counts, per-language test-file counts, and the directory tree.
type repoCensus struct {
	exts  map[string]int
	tests map[string]int
	tree  *dirNode
}

// dirNode is one directory in the walk-derived tree: files counts its direct regular
// files, total the recursive count (computed after the walk), and children its
// immediate subdirectories that hold at least one walked file.
type dirNode struct {
	files    int
	total    int
	children map[string]*dirNode
}

func newDirNode() *dirNode {
	return &dirNode{children: map[string]*dirNode{}}
}

// add records one file living under the given directory segments (empty for a
// root-level file), creating intermediate nodes as needed.
func (n *dirNode) add(segs []string) {
	cur := n
	for _, s := range segs {
		c, ok := cur.children[s]
		if !ok {
			c = newDirNode()
			cur.children[s] = c
		}
		cur = c
	}
	cur.files++
}

// computeTotal fills total for the node and its descendants, returning the recursive
// file count rooted here.
func (n *dirNode) computeTotal() int {
	sum := n.files
	for _, c := range n.children {
		sum += c.computeTotal()
	}
	n.total = sum
	return sum
}

// walkRepo walks root once with the shared ignore + VCS-store-skip discipline
// (gitignore-honoring, hidden and build/dep dirs skipped) and folds every file into
// the census: extension counts, test-file classification, and the directory tree.
func walkRepo(ctx context.Context, root string) (*repoCensus, error) {
	queue := make(chan *gocodewalker.File, 256)
	w := gocodewalker.NewFileWalker(root, queue)
	w.ExcludeDirectory = find.VCSStoreDirs // VCS stores: skip at any depth
	w.SetErrorHandler(func(error) bool { return true })

	errc := make(chan error, 1)
	go func() { errc <- w.Start() }()

	c := &repoCensus{exts: map[string]int{}, tests: map[string]int{}, tree: newDirNode()}
	var stop error
	for f := range queue {
		if stop != nil {
			continue // keep draining so Start can close the queue
		}
		if ctx.Err() != nil {
			stop = fmt.Errorf("overview: walk cancelled: %w", ctx.Err())
			w.Terminate()
			continue
		}
		rel, err := filepath.Rel(root, f.Location)
		if err != nil {
			continue
		}
		slashRel := filepath.ToSlash(rel)
		if topLevelDepDir(slashRel) {
			continue
		}
		c.observe(slashRel)
	}
	if err := <-errc; err != nil {
		return nil, fmt.Errorf("overview: walk %q: %w", root, err)
	}
	if stop != nil {
		return nil, stop
	}
	c.tree.computeTotal()
	return c, nil
}

// observe folds one slash-relative file path into the census.
func (c *repoCensus) observe(rel string) {
	name := path.Base(rel)
	ext := lowerExt(name)
	if sourceExts[ext] {
		c.exts[ext]++
	}
	if cat := testCategory(name, ext); cat != "" {
		c.tests[cat]++
	}
	dir := path.Dir(rel)
	var segs []string
	if dir != "." {
		segs = strings.Split(dir, "/")
	}
	c.tree.add(segs)
}

// testCategory classifies a file as a test of a language family, or "" when it is
// not a test file: Go's *_test.go, Python's test_*.py / *_test.py, and the
// JS/TS *.spec.* / *.test.* convention.
func testCategory(name, ext string) string {
	switch ext {
	case "go":
		if strings.HasSuffix(name, "_test.go") {
			return "go"
		}
	case "py":
		if strings.HasPrefix(name, "test_") || strings.HasSuffix(name, "_test.py") {
			return "py"
		}
	}
	if strings.Contains(name, ".spec.") || strings.Contains(name, ".test.") {
		return "js"
	}
	return ""
}

// lowerExt returns name's lowercase extension without the dot, or "" when it has none.
func lowerExt(name string) string {
	return strings.ToLower(strings.TrimPrefix(filepath.Ext(name), "."))
}

// languagesLine renders "languages: go (96), md (21), py (12)" from the extension
// counts, sorted by count descending then extension ascending. It returns "" when no
// source files were counted.
func languagesLine(exts map[string]int) string {
	if len(exts) == 0 {
		return ""
	}
	type kv struct {
		ext string
		n   int
	}
	xs := make([]kv, 0, len(exts))
	for e, n := range exts {
		xs = append(xs, kv{e, n})
	}
	sort.Slice(xs, func(i, j int) bool {
		if xs[i].n != xs[j].n {
			return xs[i].n > xs[j].n
		}
		return xs[i].ext < xs[j].ext
	})
	parts := make([]string, len(xs))
	for i, x := range xs {
		parts[i] = fmt.Sprintf("%s (%d)", x.ext, x.n)
	}
	return "languages: " + strings.Join(parts, ", ")
}

// testsLine renders "tests: 41 test files (go)" from the per-language test counts,
// naming the languages by descending count then name. It returns "" when there are
// no test files.
func testsLine(tests map[string]int) string {
	total := 0
	for _, n := range tests {
		total += n
	}
	if total == 0 {
		return ""
	}
	type kv struct {
		lang string
		n    int
	}
	xs := make([]kv, 0, len(tests))
	for l, n := range tests {
		xs = append(xs, kv{l, n})
	}
	sort.Slice(xs, func(i, j int) bool {
		if xs[i].n != xs[j].n {
			return xs[i].n > xs[j].n
		}
		return xs[i].lang < xs[j].lang
	})
	langs := make([]string, len(xs))
	for i, x := range xs {
		langs[i] = x.lang
	}
	return fmt.Sprintf("tests: %d test files (%s)", total, strings.Join(langs, ", "))
}

// maxDirs caps how many top-level directories the dirs section lists before folding
// the rest into a "+N more" trailer.
const maxDirs = 12

// maxPkgNames caps how many subdirectory names a dir's "(N pkgs: …)" annotation lists.
const maxPkgNames = 4

// dirsLine renders "dirs: cmd/ccx · internal (24 pkgs: anchor, astgrep, …) · plugin"
// from the directory tree: top-level directories ordered by descending recursive file
// count (name-tiebroken), a single-child no-file chain collapsed into one path
// (cmd → cmd/ccx), and a "(N pkgs: names…)" annotation for a directory that holds
// subdirectories. It returns "" for a tree with no directories.
func dirsLine(tree *dirNode) string {
	if len(tree.children) == 0 {
		return ""
	}
	type entry struct {
		display string
		total   int
		pkgs    []string
	}
	es := make([]entry, 0, len(tree.children))
	for name, node := range tree.children {
		disp, eff := collapse(name, node)
		es = append(es, entry{display: disp, total: node.total, pkgs: sortedKeys(eff.children)})
	}
	sort.Slice(es, func(i, j int) bool {
		if es[i].total != es[j].total {
			return es[i].total > es[j].total
		}
		return es[i].display < es[j].display
	})
	more := 0
	if len(es) > maxDirs {
		more = len(es) - maxDirs
		es = es[:maxDirs]
	}
	parts := make([]string, len(es))
	for i, e := range es {
		parts[i] = e.display
		if len(e.pkgs) > 0 {
			parts[i] += fmt.Sprintf(" (%d pkgs: %s)", len(e.pkgs), joinNames(e.pkgs, maxPkgNames))
		}
	}
	line := "dirs: " + strings.Join(parts, " · ")
	if more > 0 {
		line += fmt.Sprintf(" · +%d more", more)
	}
	return line
}

// collapse folds a single-child directory that holds no direct files into a joined
// path (cmd, with only the ccx subdir, becomes "cmd/ccx"), returning the display path
// and the effective deepest node whose children annotate the entry.
func collapse(name string, n *dirNode) (string, *dirNode) {
	disp := name
	for n.files == 0 && len(n.children) == 1 {
		var k string
		var c *dirNode
		for k, c = range n.children {
			break
		}
		disp += "/" + k
		n = c
	}
	return disp, n
}

// joinNames joins names, showing at most maxN of them and appending "…" when more
// were elided.
func joinNames(names []string, maxN int) string {
	if len(names) > maxN {
		return strings.Join(names[:maxN], ", ") + ", …"
	}
	return strings.Join(names, ", ")
}

// sortedKeys returns the map keys sorted ascending.
func sortedKeys(m map[string]*dirNode) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}
