package chunk

import (
	"path/filepath"
	"strings"
)

// ContentType classifies a language by the kind of content it holds, mirroring
// semble's CODE/DOCS/CONFIG/DATA buckets in files.py.
type ContentType string

const (
	ContentCode   ContentType = "code"
	ContentDocs   ContentType = "docs"
	ContentConfig ContentType = "config"
	ContentData   ContentType = "data"
)

// allLanguages is semble's ALL_LANGUAGES: every value in the extension map.
var allLanguages = func() map[string]struct{} {
	m := make(map[string]struct{}, len(extToLanguage))
	for _, lang := range extToLanguage {
		m[lang] = struct{}{}
	}
	return m
}()

// DetectLanguage returns semble's language name for a path's extension, matching
// files.detect_language: the lowercased Path.suffix looked up in the extension
// map. ok is false for an unmapped or suffix-less name.
func DetectLanguage(path string) (lang string, ok bool) {
	name := filepath.Base(path)
	i := strings.LastIndexByte(name, '.')
	if i <= 0 || i >= len(name)-1 {
		return "", false
	}
	lang, ok = extToLanguage[strings.ToLower(name[i:])]
	return lang, ok
}

// Classify returns a language's content type. Languages absent from the doc,
// config, and data sets are code, matching semble's _CODE_LANGUAGES complement.
func Classify(lang string) ContentType {
	if _, ok := docLanguages[lang]; ok {
		return ContentDocs
	}
	if _, ok := configLanguages[lang]; ok {
		return ContentConfig
	}
	if _, ok := dataLanguages[lang]; ok {
		return ContentData
	}
	return ContentCode
}

// isSupportedLanguage reports whether semble would attempt a tree-sitter parse
// for lang (files.is_supported_language: membership in ALL_LANGUAGES).
func isSupportedLanguage(lang string) bool {
	_, ok := allLanguages[lang]
	return ok
}
