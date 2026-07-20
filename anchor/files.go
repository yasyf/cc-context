package anchor

import "path/filepath"

// Files is a per-response cache of loaded files, keyed by resolved path. It
// serves a single anchor-emission pass — never reuse it across calls, or a
// stale line table resolves anchors against pre-edit content.
type Files struct {
	root  string
	cache map[string]*File
}

// NewFiles builds a Files cache that resolves relative paths against root.
func NewFiles(root string) *Files {
	return &Files{root: root, cache: map[string]*File{}}
}

// LineAt returns the raw text of path's 1-indexed line n, byte-for-byte as it
// sits on disk, loading the file once and caching it. It reports false on any
// miss — unreadable file or line out of range — so the caller emits a bare
// span rather than an anchor over text it cannot vouch for.
func (fs *Files) LineAt(path string, n int) (string, bool) {
	if n < 1 {
		return "", false
	}
	f := fs.load(path)
	if f == nil {
		return "", false
	}
	if n > len(f.lines) {
		return "", false
	}
	return f.lines[n-1], true
}

func (fs *Files) load(path string) *File {
	key := path
	if !filepath.IsAbs(key) {
		key = filepath.Join(fs.root, key)
	}
	if f, ok := fs.cache[key]; ok {
		return f
	}
	f, err := Load(key)
	if err != nil {
		return nil
	}
	fs.cache[key] = f
	return f
}
