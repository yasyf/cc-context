package astgrep

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/yasyf/cc-context/internal/outline"
)

var (
	identRe      = regexp.MustCompile(`^[A-Za-z_$][\w$]*(?:\.[A-Za-z_$][\w$]*)*$`)
	jsNamedRe    = regexp.MustCompile(`(?m)^\s*import\s+(?:type\s+)?(?:[\w$]+\s*,\s*)?\{([^}]*)\}\s*from\s*["']([^"']+)["']`)
	jsDefaultRe  = regexp.MustCompile(`(?m)^\s*import\s+(?:type\s+)?([\w$]+)\s*(?:,\s*(?:\{[^}]*\}|\*\s+as\s+[\w$]+))?\s*from\s*["']([^"']+)["']`)
	jsStarRe     = regexp.MustCompile(`(?m)^\s*import\s+(?:type\s+)?(?:[\w$]+\s*,\s*)?\*\s+as\s+([\w$]+)\s*from\s*["']([^"']+)["']`)
	jsReexportRe = regexp.MustCompile(`(?m)^\s*export\s+(?:type\s+)?\{([^}]*)\}\s*from\s*["']([^"']+)["']`)
	jsExportAll  = regexp.MustCompile(`(?m)^\s*export\s+\*\s+from\s*["']([^"']+)["']`)
	jsAbstractRe = regexp.MustCompile(`(?m)^(\s*(?:export\s+)?(?:default\s+)?)abstract(\s+class\b)`)
	jsStaticRe   = regexp.MustCompile(`^(?:(?:public|protected|private|readonly|override|declare|async)\s+)*static\b`)
	pyFromRe     = regexp.MustCompile(`(?m)^([ \t]*)from\s+([.\w]+)\s+import\s+(?:\(([^)]*)\)|([^\n#]*))`)
	pyImportRe   = regexp.MustCompile(`(?m)^([ \t]*)import\s+([^\n#]+)`)
	pyClassRe    = regexp.MustCompile(`^\s*class\s+\w+\s*(?:\[.*?\])?\s*\((.*)\)\s*$`)
	goEmbedRe    = regexp.MustCompile("^\\*?([A-Za-z_]\\w*)(?:\\.([A-Za-z_]\\w*))?(?:\\[[^\\]]*\\])?\\s*(?:`[^`]*`)?\\s*(?://.*)?$")
	goImportRe   = regexp.MustCompile(`^\s*(?:import\s+)?(?:([\w.]+)\s+)?"([^"]+)"`)
	goReceiverRe = regexp.MustCompile(`^func\s*\(\s*(?:\w+\s+)?\*?(\w+)`)
	goMajorRe    = regexp.MustCompile(`^v\d+$`)
)

var jsSuffixes = []string{"", ".ts", ".tsx", ".d.ts", ".mts", ".cts", ".js", ".jsx", ".mjs", ".cjs", "/index.ts", "/index.tsx", "/index.js", "/index.jsx"}

// Base is one ancestor of an outlined type: the name the declaration writes, the
// file and 1-based line of its definition, and the members it contributes that
// the type and every nearer ancestor leave unoverridden. Unresolved says why the
// definition was not found; Path is then empty.
type Base struct {
	Name       string
	Path       string
	Line       int
	Members    []Member
	Unresolved string
}

// Member is an inherited member and the file declaring it, which for a Go
// method can be a sibling of the file declaring its receiver type.
type Member struct {
	Path string
	OutlineItem
}

// Inheritance resolves the ancestors of outlined types from source alone: a
// TS/JS class's or interface's extends clause, a Python class's bases, and a Go
// struct's or interface's embedded types. It follows same-file declarations,
// imports, re-exports, and a Go package's sibling files, memoizing every outline
// and file read for the life of one request.
type Inheritance struct {
	ctx       context.Context
	root      string
	outlines  map[string]OutlineFile
	seeds     map[string]OutlineFile
	sources   map[string]string
	goPkgs    map[string][]OutlineFile
	parentsOf map[string][]link
	exports   map[string]*export
}

type family int

const (
	famNone family = iota
	famJS
	famPython
	famGo
)

type baseRef struct {
	written   string
	qualifier string
	name      string
	invalid   string
}

type decl struct {
	path string
	item OutlineItem
}

type link struct {
	decl       decl
	written    string
	unresolved string
}

type export struct {
	decl decl
	why  string
}

// NewInheritance returns a resolver for files outlined relative to root.
func NewInheritance(ctx context.Context, root string) *Inheritance {
	return &Inheritance{
		ctx:       ctx,
		root:      root,
		outlines:  map[string]OutlineFile{},
		seeds:     map[string]OutlineFile{},
		sources:   map[string]string{},
		goPkgs:    map[string][]OutlineFile{},
		parentsOf: map[string][]link{},
		exports:   map[string]*export{},
	}
}

// Annotate sets Bases on every top-level type declaration in files that names
// an ancestor, so RenderOutline lists what each one inherits.
func (in *Inheritance) Annotate(files []OutlineFile) {
	for fi := range files {
		for ii := range files[fi].Items {
			it := &files[fi].Items[ii]
			it.Bases = in.Ancestors(files[fi].Path, *it)
		}
	}
}

// Ancestors lists every ancestor of the type declaration it, declared in the
// file at path, in the order its language looks members up: a TS/JS chain
// nearest first, a Python class's C3 method resolution order, and a Go type's
// embedded types by embedding depth, where a name two types at one depth both
// declare is ambiguous and promotes from neither. A member the type or an earlier
// ancestor declares shadows the same name further along. Anything but a class,
// interface, or struct has none.
func (in *Inheritance) Ancestors(path string, it OutlineItem) []Base {
	if !isTypeDecl(it.SymbolType) {
		return nil
	}
	self := decl{path: path, item: it}
	if len(in.parents(self)) == 0 {
		return nil
	}
	shadowed := in.names(self)
	switch familyOf(path) {
	case famGo:
		return in.promoted(self, shadowed)
	case famPython:
		return in.contribute(in.mro(self, map[string]bool{})[1:], shadowed)
	default:
		return in.contribute(in.chain(self), shadowed)
	}
}

func (in *Inheritance) contribute(order []link, shadowed map[string]bool) []Base {
	var out []Base
	for _, l := range order {
		if l.unresolved != "" {
			out = append(out, Base{Name: l.written, Unresolved: l.unresolved})
			continue
		}
		b := l.base()
		for _, m := range in.members(l.decl) {
			if k := memberKey(m); !shadowed[k] && inheritable(m) {
				b.Members = append(b.Members, m)
			}
		}
		for k := range in.names(l.decl) {
			shadowed[k] = true
		}
		out = append(out, b)
	}
	return out
}

func (in *Inheritance) chain(self decl) []link {
	var out []link
	seen := map[string]bool{self.key(): true}
	var walk func(d decl)
	walk = func(d decl) {
		for _, p := range in.parents(d) {
			switch {
			case p.unresolved != "":
				out = append(out, p)
			case !seen[p.decl.key()]:
				seen[p.decl.key()] = true
				out = append(out, p)
				walk(p.decl)
			}
		}
	}
	walk(self)
	return out
}

func (in *Inheritance) mro(d decl, visiting map[string]bool) []link {
	self := link{decl: d, written: d.item.Name}
	if visiting[d.key()] {
		return []link{self}
	}
	visiting[d.key()] = true
	defer delete(visiting, d.key())
	parents := in.parents(d)
	seqs := make([][]link, 0, len(parents)+1)
	for _, p := range parents {
		if p.unresolved != "" {
			seqs = append(seqs, []link{p})
		} else {
			seqs = append(seqs, in.mro(p.decl, visiting))
		}
	}
	return append([]link{self}, c3Merge(append(seqs, parents))...)
}

func (in *Inheritance) promoted(self decl, shadowed map[string]bool) []Base {
	var out []Base
	seen := map[string]bool{self.key(): true}
	level := []decl{self}
	for len(level) > 0 {
		var next []link
		for _, d := range level {
			for _, p := range in.parents(d) {
				if p.unresolved == "" {
					if seen[p.decl.key()] {
						continue
					}
					seen[p.decl.key()] = true
				}
				next = append(next, p)
			}
		}
		declared := map[string]int{}
		for _, l := range next {
			if l.unresolved == "" {
				for k := range in.names(l.decl) {
					declared[k]++
				}
			}
		}
		level = nil
		for _, l := range next {
			if l.unresolved != "" {
				out = append(out, Base{Name: l.written, Unresolved: l.unresolved})
				continue
			}
			b := l.base()
			for _, m := range in.members(l.decl) {
				if k := memberKey(m); !shadowed[k] && declared[k] == 1 {
					b.Members = append(b.Members, m)
				}
			}
			out = append(out, b)
			level = append(level, l.decl)
		}
		for k := range declared {
			shadowed[k] = true
		}
	}
	return out
}

func (in *Inheritance) parents(d decl) []link {
	if ps, ok := in.parentsOf[d.key()]; ok {
		return ps
	}
	var ps []link
	for _, ref := range in.heritage(d.path, d.item) {
		if ref.invalid != "" {
			ps = append(ps, link{written: ref.written, unresolved: ref.invalid})
			continue
		}
		target, why := in.resolve(d.path, ref)
		ps = append(ps, link{decl: target, written: ref.written, unresolved: why})
	}
	in.parentsOf[d.key()] = ps
	return ps
}

func (in *Inheritance) names(d decl) map[string]bool {
	names := map[string]bool{}
	for _, m := range in.members(d) {
		names[memberKey(m)] = true
	}
	if familyOf(d.path) == famGo {
		for _, p := range in.heritage(d.path, d.item) {
			names[p.name] = true
		}
	}
	return names
}

func (in *Inheritance) members(d decl) []Member {
	var members []Member
	for _, m := range d.item.Members {
		members = append(members, Member{Path: d.path, OutlineItem: m})
	}
	if familyOf(d.path) != famGo {
		return members
	}
	for _, f := range in.goPackage(filepath.Dir(d.path)) {
		for _, it := range f.Items {
			if recv, ok := GoReceiver(it.Signature); ok && recv == d.item.Name {
				members = append(members, Member{Path: f.Path, OutlineItem: it})
			}
		}
	}
	return members
}

func (d decl) key() string {
	return filepath.Clean(d.path) + "\x00" + d.item.Name
}

func (l link) key() string {
	if l.unresolved != "" {
		return "\x00" + l.written
	}
	return l.decl.key()
}

func (l link) base() Base {
	return Base{Name: l.decl.item.Name, Path: l.decl.path, Line: oneBased(l.decl.item.Range.Start.Line)}
}

func c3Merge(seqs [][]link) []link {
	var out []link
	emitted := map[string]bool{}
	for {
		live := seqs[:0]
		for _, s := range seqs {
			if len(s) > 0 {
				live = append(live, s)
			}
		}
		seqs = live
		if len(seqs) == 0 {
			return out
		}
		head := seqs[0][0]
		for _, s := range seqs {
			if !inTail(seqs, s[0].key()) {
				head = s[0]
				break
			}
		}
		if !emitted[head.key()] {
			emitted[head.key()] = true
			out = append(out, head)
		}
		for i := range seqs {
			if seqs[i][0].key() == head.key() {
				seqs[i] = seqs[i][1:]
			}
		}
	}
}

func inTail(seqs [][]link, key string) bool {
	for _, s := range seqs {
		for _, l := range s[1:] {
			if l.key() == key {
				return true
			}
		}
	}
	return false
}

func memberKey(m Member) string {
	if familyOf(m.Path) == famJS && jsStaticRe.MatchString(m.Signature) {
		return "static " + m.Name
	}
	return m.Name
}

func (in *Inheritance) heritage(path string, it OutlineItem) []baseRef {
	lines := strings.Split(in.source(path), "\n")
	start, end := it.Range.Start.Line, it.Range.End.Line
	if start >= len(lines) {
		return nil
	}
	end = min(end, len(lines)-1)
	lines = append([]string{lines[start][min(it.Range.Start.Column, len(lines[start])):]}, lines[start+1:end+1]...)
	switch familyOf(path) {
	case famJS:
		return jsHeritage(headerText(lines, '{', true), it.SymbolType)
	case famPython:
		return pyHeritage(headerText(lines, ':', false))
	case famGo:
		return goEmbedded(strings.Join(lines, "\n"))
	default:
		return nil
	}
}

func (in *Inheritance) resolve(path string, ref baseRef) (decl, string) {
	switch familyOf(path) {
	case famJS:
		return in.resolveJS(path, ref)
	case famPython:
		return in.resolvePython(path, ref)
	default:
		return in.resolveGo(path, ref)
	}
}

func (in *Inheritance) resolveJS(path string, ref baseRef) (decl, string) {
	if ref.qualifier == "" {
		if d, ok := in.declIn(path, ref.name); ok {
			return d, ""
		}
	}
	spec, name, ok := jsImport(in.source(path), ref)
	if !ok {
		return decl{}, "not declared in this file or imported"
	}
	target, ok := in.jsModule(path, spec)
	if !ok {
		return decl{}, fmt.Sprintf("imported from %q, which resolves to no repo file", spec)
	}
	return in.jsExport(target, name)
}

func (in *Inheritance) jsExport(path, name string) (decl, string) {
	return in.exported(path, name, func() (decl, string) {
		src := in.source(path)
		for _, m := range jsReexportRe.FindAllStringSubmatch(src, -1) {
			for orig, local := range importList(m[1]) {
				if local != name {
					continue
				}
				if target, ok := in.jsModule(path, m[2]); ok {
					return in.jsExport(target, orig)
				}
			}
		}
		for _, m := range jsExportAll.FindAllStringSubmatch(src, -1) {
			if target, ok := in.jsModule(path, m[1]); ok {
				if d, why := in.jsExport(target, name); why == "" {
					return d, ""
				}
			}
		}
		return decl{}, fmt.Sprintf("%s declares no %s", path, name)
	})
}

func (in *Inheritance) exported(path, name string, reexport func() (decl, string)) (decl, string) {
	if d, ok := in.declIn(path, name); ok {
		return d, ""
	}
	key := filepath.Clean(path) + "\x00" + name
	if e, ok := in.exports[key]; ok {
		if e == nil {
			return decl{}, fmt.Sprintf("%s re-exports %s in a cycle", path, name)
		}
		return e.decl, e.why
	}
	in.exports[key] = nil
	d, why := reexport()
	in.exports[key] = &export{decl: d, why: why}
	return d, why
}

func (in *Inheritance) jsModule(from, spec string) (string, bool) {
	var bases []string
	if strings.HasPrefix(spec, ".") {
		bases = []string{filepath.Join(filepath.Dir(in.abs(from)), spec)}
	} else {
		bases = in.ancestorJoins(from, spec)
	}
	for _, base := range bases {
		for _, stem := range []string{base, strings.TrimSuffix(base, filepath.Ext(base))} {
			for _, suffix := range jsSuffixes {
				if rel, ok := in.repoFile(stem + suffix); ok {
					return rel, true
				}
			}
		}
	}
	return "", false
}

func (in *Inheritance) resolvePython(path string, ref baseRef) (decl, string) {
	if ref.qualifier == "" {
		if d, ok := in.declIn(path, ref.name); ok {
			return d, ""
		}
	}
	module, name, ok := pyImport(in.source(path), ref)
	if !ok {
		return decl{}, "not declared in this file or imported"
	}
	target, ok := in.pyModule(path, module)
	if !ok {
		return decl{}, fmt.Sprintf("imported from %s, which resolves to no repo file", module)
	}
	return in.pyExport(target, name)
}

func (in *Inheritance) pyExport(path, name string) (decl, string) {
	return in.exported(path, name, func() (decl, string) {
		if module, orig, ok := pyImport(in.source(path), baseRef{name: name}); ok {
			if target, ok := in.pyModule(path, module); ok {
				return in.pyExport(target, orig)
			}
		}
		return decl{}, fmt.Sprintf("%s declares no %s", path, name)
	})
}

func (in *Inheritance) pyModule(from, module string) (string, bool) {
	rest := strings.TrimLeft(module, ".")
	dots := len(module) - len(rest)
	sub := filepath.FromSlash(strings.ReplaceAll(rest, ".", "/"))
	var bases []string
	if dots > 0 {
		dir := filepath.Dir(in.abs(from))
		for range dots - 1 {
			dir = filepath.Dir(dir)
		}
		bases = []string{filepath.Join(dir, sub)}
	} else {
		bases = in.ancestorJoins(from, sub)
	}
	for _, base := range bases {
		for _, cand := range []string{base + ".py", base + ".pyi", filepath.Join(base, "__init__.py")} {
			if rel, ok := in.repoFile(cand); ok {
				return rel, true
			}
		}
	}
	return "", false
}

func (in *Inheritance) resolveGo(path string, ref baseRef) (decl, string) {
	dir := filepath.Dir(path)
	if ref.qualifier != "" {
		imp, ok := goImport(in.source(path), ref.qualifier)
		if !ok {
			return decl{}, fmt.Sprintf("package %s is not imported", ref.qualifier)
		}
		pkg, ok := in.goPackageDir(path, imp)
		if !ok {
			return decl{}, fmt.Sprintf("imported from %q, outside the module", imp)
		}
		dir = pkg
	}
	for _, f := range in.goPackage(dir) {
		for _, it := range f.Items {
			if it.Name == ref.name && (isTypeDecl(it.SymbolType) || it.SymbolType == "typeParameter") {
				return decl{path: f.Path, item: it}, ""
			}
		}
	}
	return decl{}, fmt.Sprintf("not declared in package %s", dir)
}

func (in *Inheritance) goPackageDir(from, importPath string) (string, bool) {
	dir := filepath.Dir(in.abs(from))
	for {
		if data, err := os.ReadFile(filepath.Join(dir, "go.mod")); err == nil { //nolint:gosec // the module's own go.mod, walked up from an outlined file
			mod := goModulePath(string(data))
			if mod == "" || (importPath != mod && !strings.HasPrefix(importPath, mod+"/")) {
				return "", false
			}
			pkg := filepath.Join(dir, filepath.FromSlash(strings.TrimPrefix(strings.TrimPrefix(importPath, mod), "/")))
			rel, err := filepath.Rel(in.root, pkg)
			if err != nil || strings.HasPrefix(rel, "..") {
				return "", false
			}
			return rel, true
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", false
		}
		dir = parent
	}
}

func (in *Inheritance) goPackage(dir string) []OutlineFile {
	if files, ok := in.goPkgs[dir]; ok {
		return files
	}
	entries, _ := os.ReadDir(in.abs(dir))
	var paths, missing []string
	for _, e := range entries {
		if n := e.Name(); !e.IsDir() && strings.HasSuffix(n, ".go") && !strings.HasSuffix(n, "_test.go") {
			p := filepath.Join(dir, n)
			paths = append(paths, p)
			if _, ok := in.seeds[p]; !ok {
				missing = append(missing, p)
			}
		}
	}
	if len(missing) > 0 {
		fetched, _ := OutlinePaths(in.ctx, missing, OutlineOpts{})
		in.seed(fetched)
	}
	files := make([]OutlineFile, len(paths))
	for i, p := range paths {
		files[i] = in.outlineOf(p)
	}
	in.goPkgs[dir] = files
	return files
}

func (in *Inheritance) seed(files []OutlineFile) {
	for _, f := range files {
		f.Path = filepath.Clean(f.Path)
		in.seeds[f.Path] = f
	}
}

func (in *Inheritance) declIn(path, name string) (decl, bool) {
	f := in.outlineOf(path)
	for _, it := range f.Items {
		if !isTypeDecl(it.SymbolType) {
			continue
		}
		if it.Name == name || (name == "default" && strings.Contains(it.Signature, "export default")) {
			return decl{path: path, item: it}, true
		}
	}
	return decl{}, false
}

func (in *Inheritance) outlineOf(path string) OutlineFile {
	path = filepath.Clean(path)
	if f, ok := in.outlines[path]; ok {
		return f
	}
	f, seeded := in.seeds[path]
	abstract := familyOf(path) == famJS && jsAbstractRe.MatchString(in.source(path))
	if !seeded || abstract {
		f = OutlineFile{Path: path}
		if lang, ok := outline.LangForExt(path); ok {
			src := in.source(path)
			if abstract {
				src = jsAbstractRe.ReplaceAllString(src, "$1$2")
			}
			if files, err := OutlineStdin(in.ctx, []byte(src), lang); err == nil && len(files) == 1 {
				f.Items = files[0].Items
			}
		}
	}
	in.outlines[path] = f
	return f
}

func (in *Inheritance) source(path string) string {
	if s, ok := in.sources[path]; ok {
		return s
	}
	data, _ := os.ReadFile(in.abs(path)) //nolint:gosec // a repo file the outline or an import already named
	in.sources[path] = string(data)
	return in.sources[path]
}

func (in *Inheritance) abs(path string) string {
	if filepath.IsAbs(path) {
		return path
	}
	return filepath.Join(in.root, path)
}

func (in *Inheritance) repoFile(abs string) (string, bool) {
	info, err := os.Stat(abs) //nolint:gosec // an existence probe on an import candidate
	if err != nil || !info.Mode().IsRegular() {
		return "", false
	}
	rel, err := filepath.Rel(in.root, abs)
	if err != nil || strings.HasPrefix(rel, "..") {
		return "", false
	}
	return rel, true
}

func (in *Inheritance) ancestorJoins(from, sub string) []string {
	var out []string
	root := filepath.Clean(in.root)
	for dir := filepath.Dir(in.abs(from)); ; dir = filepath.Dir(dir) {
		out = append(out, filepath.Join(dir, sub))
		if dir == root || filepath.Dir(dir) == dir {
			return out
		}
	}
}

// GoReceiver returns the receiver type a Go method signature names — "func (w
// Widget) …", "func (w *Widget) …", "func (Widget) …" — and false for a plain
// function.
func GoReceiver(signature string) (string, bool) {
	m := goReceiverRe.FindStringSubmatch(signature)
	if m == nil {
		return "", false
	}
	return m[1], true
}

func familyOf(path string) family {
	lang, _ := outline.LangForExt(path)
	switch lang {
	case "typescript", "tsx", "javascript":
		return famJS
	case "python":
		return famPython
	case "go":
		return famGo
	default:
		return famNone
	}
}

func isTypeDecl(symbolType string) bool {
	switch symbolType {
	case "class", "interface", "struct":
		return true
	default:
		return false
	}
}

func inheritable(m Member) bool {
	switch familyOf(m.Path) {
	case famJS:
		return m.SymbolType != "constructor" && !strings.HasPrefix(m.Name, "#") && !strings.HasPrefix(m.Signature, "private ")
	case famPython:
		return !strings.HasPrefix(m.Name, "__") || strings.HasSuffix(m.Name, "__")
	default:
		return true
	}
}

func headerText(lines []string, term byte, angles bool) string {
	var b strings.Builder
	depth := 0
	for _, ln := range lines {
		for j := 0; j < len(ln); j++ {
			switch c := ln[j]; {
			case c == '(' || c == '[' || (angles && c == '<'):
				depth++
			case c == ')' || c == ']' || (angles && c == '>' && (j == 0 || ln[j-1] != '=')):
				depth--
			case c == term && depth == 0:
				b.WriteString(ln[:j])
				return b.String()
			}
		}
		b.WriteString(ln)
		b.WriteByte(' ')
	}
	return b.String()
}

func jsHeritage(header, symbolType string) []baseRef {
	flat := collapseBrackets(header)
	i := wordIndex(flat, "extends")
	if i < 0 {
		return nil
	}
	clause := flat[i+len("extends"):]
	if j := wordIndex(clause, "implements"); j >= 0 {
		clause = clause[:j]
	}
	parts := []string{clause}
	if symbolType == "interface" {
		parts = strings.Split(clause, ",")
	}
	var refs []baseRef
	for _, p := range parts {
		if written := strings.TrimSuffix(strings.TrimSpace(p), "<>"); written != "" {
			refs = append(refs, newRef(written))
		}
	}
	return refs
}

func pyHeritage(header string) []baseRef {
	m := pyClassRe.FindStringSubmatch(header)
	if m == nil {
		return nil
	}
	var refs []baseRef
	for _, arg := range splitTopLevel(m[1]) {
		arg = strings.TrimSpace(arg)
		if arg == "" || arg == "object" || strings.HasPrefix(arg, "*") || strings.Contains(collapseBrackets(arg), "=") {
			continue
		}
		if k := strings.IndexByte(arg, '['); k >= 0 {
			arg = strings.TrimSpace(arg[:k])
		}
		refs = append(refs, newRef(arg))
	}
	return refs
}

func goEmbedded(decl string) []baseRef {
	open, end := strings.IndexByte(decl, '{'), strings.LastIndexByte(decl, '}')
	if open < 0 || end <= open {
		return nil
	}
	var refs []baseRef
	depth, start := 0, open+1
	for i := open + 1; i <= end; i++ {
		switch c := decl[i]; {
		case c == '{':
			depth++
		case c == '}' && i < end:
			depth--
		case (c == '\n' || c == ';' || i == end) && depth == 0:
			m := goEmbedRe.FindStringSubmatch(strings.TrimSpace(decl[start:i]))
			switch {
			case m == nil || m[1] == "any" || m[1] == "comparable":
			case m[2] != "":
				refs = append(refs, baseRef{written: m[1] + "." + m[2], qualifier: m[1], name: m[2]})
			default:
				refs = append(refs, baseRef{written: m[1], name: m[1]})
			}
			start = i + 1
		}
	}
	return refs
}

func newRef(written string) baseRef {
	if !identRe.MatchString(written) {
		return baseRef{written: written, invalid: "not a plain type name"}
	}
	ref := baseRef{written: written, name: written}
	if k := strings.LastIndexByte(written, '.'); k >= 0 {
		ref.qualifier, ref.name = written[:k], written[k+1:]
	}
	return ref
}

func jsImport(src string, ref baseRef) (spec, name string, ok bool) {
	if ref.qualifier != "" {
		for _, m := range jsStarRe.FindAllStringSubmatch(src, -1) {
			if m[1] == ref.qualifier {
				return m[2], ref.name, true
			}
		}
		return "", "", false
	}
	for _, m := range jsNamedRe.FindAllStringSubmatch(src, -1) {
		for orig, local := range importList(m[1]) {
			if local == ref.name {
				return m[2], orig, true
			}
		}
	}
	for _, m := range jsDefaultRe.FindAllStringSubmatch(src, -1) {
		if m[1] == ref.name {
			return m[2], "default", true
		}
	}
	return "", "", false
}

func pyImport(src string, ref baseRef) (module, name string, ok bool) {
	froms := topLevelFirst(pyFromRe.FindAllStringSubmatch(src, -1))
	if ref.qualifier == "" {
		for _, m := range froms {
			for orig, local := range importList(m[3] + m[4]) {
				if local == ref.name {
					return m[2], orig, true
				}
			}
		}
		return "", "", false
	}
	for _, m := range topLevelFirst(pyImportRe.FindAllStringSubmatch(src, -1)) {
		for orig, local := range importList(m[2]) {
			if local == ref.qualifier {
				return orig, ref.name, true
			}
		}
	}
	for _, m := range froms {
		for orig, local := range importList(m[3] + m[4]) {
			if local != ref.qualifier {
				continue
			}
			if strings.Trim(m[2], ".") == "" {
				return m[2] + orig, ref.name, true
			}
			return m[2] + "." + orig, ref.name, true
		}
	}
	return "", "", false
}

func topLevelFirst(matches [][]string) [][]string {
	sort.SliceStable(matches, func(i, j int) bool { return matches[i][1] == "" && matches[j][1] != "" })
	return matches
}

func goImport(src, qualifier string) (string, bool) {
	inBlock := false
	for _, ln := range strings.Split(src, "\n") {
		t := strings.TrimSpace(ln)
		switch {
		case strings.HasPrefix(t, "import ("):
			inBlock = true
			continue
		case inBlock && t == ")":
			inBlock = false
			continue
		case !inBlock && !strings.HasPrefix(t, "import "):
			if strings.HasPrefix(t, "func ") || strings.HasPrefix(t, "type ") || strings.HasPrefix(t, "var ") || strings.HasPrefix(t, "const ") {
				return "", false
			}
			continue
		}
		m := goImportRe.FindStringSubmatch(t)
		if m == nil {
			continue
		}
		local := m[1]
		if local == "" {
			segs := strings.Split(m[2], "/")
			local = segs[len(segs)-1]
			if goMajorRe.MatchString(local) && len(segs) > 1 {
				local = segs[len(segs)-2]
			}
		}
		if local == qualifier {
			return m[2], true
		}
	}
	return "", false
}

func goModulePath(gomod string) string {
	for _, ln := range strings.Split(gomod, "\n") {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(ln), "module "); ok {
			return strings.TrimSpace(rest)
		}
	}
	return ""
}

func importList(list string) map[string]string {
	out := map[string]string{}
	for _, entry := range strings.Split(list, ",") {
		entry = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(entry), "type "))
		if entry == "" {
			continue
		}
		orig, local := entry, entry
		if k := strings.Index(entry, " as "); k >= 0 {
			orig, local = strings.TrimSpace(entry[:k]), strings.TrimSpace(entry[k+4:])
		}
		out[orig] = local
	}
	return out
}

func collapseBrackets(s string) string {
	var b strings.Builder
	depth := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '(' || c == '[' || c == '<' || c == '{':
			if depth == 0 {
				b.WriteByte(c)
			}
			depth++
		case c == ')' || c == ']' || (c == '>' && (i == 0 || s[i-1] != '=')) || c == '}':
			depth--
			if depth == 0 {
				b.WriteByte(c)
			}
		case depth == 0:
			b.WriteByte(c)
		}
	}
	return b.String()
}

func splitTopLevel(s string) []string {
	var parts []string
	depth, last := 0, 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '(', '[', '{':
			depth++
		case ')', ']', '}':
			depth--
		case ',':
			if depth == 0 {
				parts = append(parts, s[last:i])
				last = i + 1
			}
		}
	}
	return append(parts, s[last:])
}

func wordIndex(s, word string) int {
	for off := 0; ; {
		i := strings.Index(s[off:], word)
		if i < 0 {
			return -1
		}
		i += off
		before := i == 0 || !isWordByte(s[i-1])
		after := i+len(word) == len(s) || !isWordByte(s[i+len(word)])
		if before && after {
			return i
		}
		off = i + len(word)
	}
}

func isWordByte(c byte) bool {
	return c == '_' || c == '$' || c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}
