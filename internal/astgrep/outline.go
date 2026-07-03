package astgrep

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/yasyf/cc-context/internal/anchor"
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

// RenderOutline renders files as a `# <path>` header per file, then one
// `L<line>#<hash>  <signature>` per top-level item with members indented one
// level and left bare (they live inside the parent's anchored span). The anchor
// hashes the item's real source line via fs; a cache miss leaves the span bare.
// ast-grep reports 0-based lines; oneBased shifts them to the ccx 1-based
// convention. A run with no items anywhere collapses to a single no-symbols hint.
func RenderOutline(files []OutlineFile, fs *anchor.Files) string {
	var b strings.Builder
	var items int
	for _, f := range files {
		if len(f.Items) == 0 {
			continue
		}
		fmt.Fprintf(&b, "# %s\n", f.Path)
		for _, it := range f.Items {
			items++
			writeOutlineItem(&b, it, 0, f.Path, fs)
		}
	}
	if items == 0 {
		return "# no symbols\n"
	}
	return b.String()
}

// writeOutlineItem writes one item as `<indent>L<line>  <signature>`, anchoring
// the depth-0 line marker with a content hash of its source line when fs can read
// it. Members recurse at the next indent level and stay bare. The signature falls
// back to the name when ast-grep emits none (e.g. a struct field).
func writeOutlineItem(b *strings.Builder, it OutlineItem, depth int, path string, fs *anchor.Files) {
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
	fmt.Fprintf(b, "%sL%d%s  %s\n", strings.Repeat("  ", depth), line, anchored, sig)
	for _, m := range it.Members {
		writeOutlineItem(b, m, depth+1, path, fs)
	}
}
