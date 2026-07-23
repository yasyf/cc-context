// Package index is the walker + persistent index cache behind native semantic
// search: it enumerates a repo's indexable files (semble's .gitignore /
// .sembleignore / denylist / size+data-language gates), chunks and embeds them,
// and persists chunk metadata plus the float32 vector matrix under a per-repo
// cache dir so a warm re-query reuses unchanged files.
package index

import (
	"fmt"
	"os"
	"sort"
	"strings"
)

// File-eligibility gates, ported verbatim from semble/index/files.py.
const (
	maxFileBytes   = 1_000_000 // files larger than this are never indexed
	emptyFileBytes = 128       // only files below this are checked for whitespace-only content
)

// ContentType selects a family of languages to index — semble's ContentType.
type ContentType string

// The indexable content families. Data languages (json, csv, …) belong to no
// family, so they are never indexed.
const (
	ContentCode   ContentType = "code"
	ContentDocs   ContentType = "docs"
	ContentConfig ContentType = "config"
)

// defaultContent matches the ccx CLI/MCP default (`--content "code docs"`).
var defaultContent = []ContentType{ContentCode, ContentDocs}

// codeLanguages is every language that is neither doc, config, nor data —
// semble's _CODE_LANGUAGES = ALL - DOCS - CONFIG - DATA.
var codeLanguages = func() map[string]bool {
	code := map[string]bool{}
	for _, lang := range extensionToLanguage {
		if docLanguages[lang] || configLanguages[lang] || dataLanguages[lang] {
			continue
		}
		code[lang] = true
	}
	return code
}()

// languagesFor returns the language set backing a content type.
func languagesFor(t ContentType) map[string]bool {
	switch t {
	case ContentCode:
		return codeLanguages
	case ContentDocs:
		return docLanguages
	case ContentConfig:
		return configLanguages
	default:
		panic(fmt.Sprintf("index: unknown content type %q", t))
	}
}

// ParseContent resolves a whitespace-separated content spec ("code docs",
// "all", …) into content types, defaulting empty input to code+docs. It fails
// loudly on an unknown token.
func ParseContent(spec string) ([]ContentType, error) {
	fields := strings.Fields(spec)
	if len(fields) == 0 {
		return append([]ContentType(nil), defaultContent...), nil
	}
	seen := map[ContentType]bool{}
	var out []ContentType
	add := func(t ContentType) {
		if !seen[t] {
			seen[t] = true
			out = append(out, t)
		}
	}
	for _, f := range fields {
		switch f {
		case "all":
			add(ContentCode)
			add(ContentDocs)
			add(ContentConfig)
		case string(ContentCode), string(ContentDocs), string(ContentConfig):
			add(ContentType(f))
		default:
			return nil, fmt.Errorf("index: unknown content type %q (want code|docs|config|all)", f)
		}
	}
	return out, nil
}

// ContentKey renders a content-type set as a stable, order-independent string
// for cache-validity comparison and resident-cache keying.
func ContentKey(types []ContentType) string {
	vals := make([]string, len(types))
	for i, t := range types {
		vals[i] = string(t)
	}
	sort.Strings(vals)
	return strings.Join(vals, ",")
}

// Extensions returns the sorted set of file extensions backing the languages of
// the given content types — semble/index/files.py get_extensions.
func Extensions(types []ContentType) []string {
	langs := map[string]bool{}
	for _, t := range types {
		for lang := range languagesFor(t) {
			langs[lang] = true
		}
	}
	extSet := map[string]bool{}
	for ext, lang := range extensionToLanguage {
		if langs[lang] {
			extSet[ext] = true
		}
	}
	exts := make([]string, 0, len(extSet))
	for ext := range extSet {
		exts = append(exts, ext)
	}
	sort.Strings(exts)
	return exts
}

// DetectLanguage returns the language for a path's extension, or "" — semble's
// detect_language. Used for the find_related same-language selector.
func DetectLanguage(path string) string {
	return extensionToLanguage[strings.ToLower(extOf(path))]
}

// fileStatus classifies a candidate file for indexing — semble's FileStatus.
type fileStatus int

const (
	statusValid fileStatus = iota
	statusTooLarge
	statusEmpty
)

// getFileStatus applies semble's size + whitespace-only gates (get_file_status
// with write_time=None): a file over maxFileBytes is too large; a file under
// emptyFileBytes whose content is whitespace-only is empty; otherwise valid.
func getFileStatus(path string) (fileStatus, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return statusValid, fmt.Errorf("stat %q: %w", path, err)
	}
	size := fi.Size()
	if size > maxFileBytes {
		return statusTooLarge, nil
	}
	if size < emptyFileBytes {
		text, err := readFileText(path)
		if err != nil {
			return statusValid, err
		}
		if strings.TrimSpace(text) == "" {
			return statusEmpty, nil
		}
	}
	return statusValid, nil
}

// readFileText reads a file as UTF-8 with invalid bytes replaced by U+FFFD,
// mirroring semble's read_file_text (read_text(errors="replace")).
func readFileText(path string) (string, error) {
	data, err := os.ReadFile(path) //nolint:gosec // walked repo file under the caller's own tree
	if err != nil {
		return "", fmt.Errorf("read %q: %w", path, err)
	}
	return strings.ToValidUTF8(string(data), "�"), nil
}

// extOf returns the final extension of a slash- or OS-separated path, including
// the leading dot, or "" when the basename has none.
func extOf(path string) string {
	base := path
	if i := strings.LastIndexAny(base, "/\\"); i >= 0 {
		base = base[i+1:]
	}
	if i := strings.LastIndexByte(base, '.'); i > 0 {
		return base[i:]
	}
	return ""
}
