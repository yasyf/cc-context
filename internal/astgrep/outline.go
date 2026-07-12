package astgrep

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/yasyf/cc-context/internal/anchor"
	"github.com/yasyf/cc-context/internal/backend"
)

// OutlineFile is one `ast-grep outline --json=stream` record: a single source
// file and its top-level items. The schema is an ast-grep 0.44.0 alpha contract;
// re-verify it on a vendor version bump.
type OutlineFile struct {
	Path     string        `json:"path"`
	Language string        `json:"language"`
	Items    []OutlineItem `json:"items"`
}

// OutlineItem is a top-level declaration or a member of one. Only the fields the
// renderer consumes are decoded; Members holds its direct members (a class's
// methods, a struct's fields).
type OutlineItem struct {
	SymbolType string `json:"symbolType"`
	Name       string `json:"name"`
	Signature  string `json:"signature"`
	IsExported bool   `json:"isExported"`
	Range      struct {
		Start struct {
			Line int `json:"line"`
		} `json:"start"`
		End struct {
			Line int `json:"line"`
		} `json:"end"`
	} `json:"range"`
	Members []OutlineItem `json:"members"`
}

// ParseOutline decodes an `outline --json=stream` body (one JSON object per file)
// into outline files. A blank stream parses to zero files.
func ParseOutline(stream []byte) ([]OutlineFile, error) {
	var files []OutlineFile
	sc := bufio.NewScanner(bytes.NewReader(stream))
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var f OutlineFile
		if err := json.Unmarshal(line, &f); err != nil {
			return nil, fmt.Errorf("decode ast-grep outline json: %w", err)
		}
		files = append(files, f)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("scan ast-grep outline json: %w", err)
	}
	return files, nil
}

// WindowOutline restricts files to the items whose source span intersects the
// inclusive 1-indexed line range [start, end], recursing into members: a kept
// container keeps only its overlapping members. ast-grep reports 0-based lines,
// so each span is shifted to the ccx 1-based convention before the overlap test.
func WindowOutline(files []OutlineFile, start, end int) []OutlineFile {
	out := make([]OutlineFile, 0, len(files))
	for _, f := range files {
		f.Items = windowItems(f.Items, start, end)
		out = append(out, f)
	}
	return out
}

func windowItems(items []OutlineItem, start, end int) []OutlineItem {
	var kept []OutlineItem
	for _, it := range items {
		if oneBased(it.Range.End.Line) < start || oneBased(it.Range.Start.Line) > end {
			continue
		}
		it.Members = windowItems(it.Members, start, end)
		kept = append(kept, it)
	}
	return kept
}

// terseOutlineDefault bounds a default outline to top-level declarations, hiding
// each container's members behind a "(+N members)" note and the --deep/--full
// flags. It is the single default switch: the accuracy gate flips it to false to
// restore the full-depth default, leaving the flags intact.
const terseOutlineDefault = true

// DepthFor returns the render depth for an outline: unbounded when a asks for
// members (--deep or --full) or terseOutlineDefault is off, else 0 (top-level
// declarations only, members collapsed to a count).
func DepthFor(a backend.Args) int {
	if a.Deep || a.Full || !terseOutlineDefault {
		return maxOutlineDepth
	}
	return 0
}

// maxOutlineDepth is the effectively-unbounded depth a full outline renders at.
const maxOutlineDepth = 1 << 30

// RenderOutline renders files as a `# <path>` header per file, then one
// `L<line>#<hash>  <signature>` per top-level item. Members nest one indent level
// deeper up to maxDepth; at maxDepth a container's members collapse to a
// `(+N members)` note and a single trailing --deep/--full hint is appended. The
// anchor hashes the item's real source line via fs; a cache miss leaves the span
// bare. ast-grep reports 0-based lines; oneBased shifts them to the ccx 1-based
// convention. A run with no items anywhere collapses to a single no-symbols hint.
func RenderOutline(files []OutlineFile, fs *anchor.Files, maxDepth int) string {
	var b strings.Builder
	var items int
	hidden := false
	for _, f := range files {
		if len(f.Items) == 0 {
			continue
		}
		fmt.Fprintf(&b, "# %s\n", f.Path)
		for _, it := range f.Items {
			items++
			if writeOutlineItem(&b, it, 0, maxDepth, f.Path, fs) {
				hidden = true
			}
		}
	}
	if items == 0 {
		return "# no symbols\n"
	}
	if hidden {
		b.WriteString("members hidden — --deep or --full to expand\n")
	}
	return b.String()
}

// writeOutlineItem writes one item as `<indent>L<line>  <signature>`, anchoring
// the depth-0 line marker with a content hash of its source line when fs can read
// it. Members recurse one indent deeper while depth < maxDepth; at the depth
// ceiling a container's members collapse to a `(+N members)` suffix. It reports
// whether any member was hidden. The signature falls back to the name when
// ast-grep emits none (e.g. a struct field).
func writeOutlineItem(b *strings.Builder, it OutlineItem, depth, maxDepth int, path string, fs *anchor.Files) bool {
	sig := it.Signature
	if sig == "" {
		sig = it.Name
	}
	line := oneBased(it.Range.Start.Line)
	anchored := ""
	if depth == 0 {
		if src, ok := fs.LineAt(path, line); ok {
			anchored = "#" + anchor.Of(src).String()
		}
	}
	hideHere := depth >= maxDepth && len(it.Members) > 0
	suffix := ""
	if hideHere {
		n := countMembers(it.Members)
		unit := "members"
		if n == 1 {
			unit = "member"
		}
		suffix = fmt.Sprintf("  (+%d %s)", n, unit)
	}
	fmt.Fprintf(b, "%sL%d%s  %s%s\n", strings.Repeat("  ", depth), line, anchored, sig, suffix)
	hidden := hideHere
	if depth < maxDepth {
		for _, m := range it.Members {
			if writeOutlineItem(b, m, depth+1, maxDepth, path, fs) {
				hidden = true
			}
		}
	}
	return hidden
}

// countMembers counts an item's members recursively.
func countMembers(members []OutlineItem) int {
	n := len(members)
	for _, m := range members {
		n += countMembers(m.Members)
	}
	return n
}
