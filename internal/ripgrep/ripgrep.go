// Package ripgrep runs a case-insensitive or word-boundary `ccx code grep`
// through ripgrep — or system grep when rg is absent — and reshapes either
// engine's output into the house grep format so render.Finalize anchors and caps
// it identically to tilth grep.
package ripgrep

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"

	"github.com/yasyf/cc-context/internal/backend"
	"github.com/yasyf/cc-context/internal/render"
)

// exitNoMatch is the exit code both rg and grep return for a clean no-match; it
// is tolerated (empty stdout, empty stderr) and distinguished from a real error
// exactly as astgrep.Run tolerates ast-grep's no-match.
const exitNoMatch = 1

// engine selects the concrete grep backend resolved from PATH.
type engine int

const (
	engineRipgrep engine = iota
	engineGrep
)

// runnerFn is the process boundary: it runs bin+argv and returns stdout,
// tolerating the clean no-match exit. run takes it as a parameter so tests drive
// the reshaper with a canned engine transcript instead of a real subprocess.
type runnerFn func(ctx context.Context, bin string, argv []string) (string, error)

// Run resolves rg (or system grep), searches for a.Query, and returns the hits
// reshaped into the house grep format, content-anchored, and budget-capped.
func Run(ctx context.Context, a backend.Args) (string, error) {
	eng, bin, err := resolveEngine()
	if err != nil {
		return "", err
	}
	return run(ctx, eng, bin, a, execEngine, statRegular)
}

// run is the engine-agnostic core: build argv, execute, parse, reshape, finalize.
// isFile is the regular-file seam parseGrep validates candidate splits through;
// production passes statRegular, tests inject a canned predicate.
func run(ctx context.Context, eng engine, bin string, a backend.Args, exec runnerFn, isFile func(string) bool) (string, error) {
	argv, err := buildArgv(eng, a)
	if err != nil {
		return "", err
	}
	raw, err := exec(ctx, bin, argv)
	if err != nil {
		return "", err
	}
	groups, err := parse(eng, raw, a.Expand > 0, isFile)
	if err != nil {
		return "", err
	}
	return render.Finalize(backend.OpGrep, reshape(a.Query, eng, groups), a.Budget)
}

// execEngine is the real process boundary, tolerating the no-match exit.
func execEngine(ctx context.Context, bin string, argv []string) (string, error) {
	return render.RunCLIAllowExit(ctx, bin, argv, exitNoMatch)
}

// resolveEngine prefers ripgrep and falls back to system grep; neither on PATH is
// a fatal error carrying an install hint.
func resolveEngine() (engine, string, error) {
	if bin, err := exec.LookPath("rg"); err == nil {
		return engineRipgrep, bin, nil
	}
	if bin, err := exec.LookPath("grep"); err == nil {
		return engineGrep, bin, nil
	}
	return 0, "", fmt.Errorf("ccx code grep -i/-w needs ripgrep or grep on PATH; install ripgrep: brew install ripgrep")
}

func buildArgv(eng engine, a backend.Args) ([]string, error) {
	switch eng {
	case engineRipgrep:
		return ripgrepArgv(a), nil
	case engineGrep:
		return grepArgv(a)
	default:
		return nil, fmt.Errorf("ripgrep: unknown engine %d", eng)
	}
}

// ripgrepArgv builds `rg --json --fixed-strings [-i] [-w] [--glob G] [-C N] -e
// <pattern> [-- scope]`. The pattern rides -e so a leading-dash literal is never
// mistaken for a flag, and the scope path rides after -- so a scope like
// "--hidden" is never parsed as an rg flag.
func ripgrepArgv(a backend.Args) []string {
	argv := []string{"--json", "--fixed-strings"}
	if a.IgnoreCase {
		argv = append(argv, "-i")
	}
	if a.Word {
		argv = append(argv, "-w")
	}
	if a.Glob != "" {
		argv = append(argv, "--glob", a.Glob)
	}
	if a.Expand > 0 {
		argv = append(argv, "-C", strconv.Itoa(a.Expand))
	}
	argv = append(argv, "-e", a.Query)
	if a.Scope != "" {
		argv = append(argv, "--", a.Scope)
	}
	return argv
}

// grepArgv builds `grep -rnFI [-i] [-w] [-C N] --exclude-dir=.[!./]* --exclude=.[!./]*
// [--include=G] -e <pattern> -- <root>` from flags common to BSD and GNU grep. The
// -I flag skips binary files and the dotdir/dotfile excludes skip hidden paths,
// both mirroring ripgrep's defaults so the two engines return the same hit set;
// -- terminates flag parsing so a directory-rooted scope never reads as a flag.
// The `.[!./]*` glob — not the simpler
// `.*` — is deliberate: BSD grep also fnmatches --exclude patterns against the
// whole "./"-prefixed path, so `.*` (and `.?*`, `.[!.]*`) match the search root
// "." and every "./sub" path, excluding the entire tree; requiring a non-dot,
// non-slash second character matches a hidden *basename* without ever matching
// "." or the "./" prefix. --glob is translated to an --include and/or a rooted
// scope; a glob shape grep cannot express, or a directory-rooted glob combined
// with an explicit --scope, fails fast.
func grepArgv(a backend.Args) ([]string, error) {
	argv := []string{"-rnFI"}
	if a.IgnoreCase {
		argv = append(argv, "-i")
	}
	if a.Word {
		argv = append(argv, "-w")
	}
	if a.Expand > 0 {
		argv = append(argv, "-C", strconv.Itoa(a.Expand))
	}
	argv = append(argv, "--exclude-dir=.[!./]*", "--exclude=.[!./]*")

	include, globRoot, err := translateGlob(a.Glob)
	if err != nil {
		return nil, err
	}
	if a.Scope != "" && globRoot != "" {
		return nil, fmt.Errorf("grep fallback cannot combine --scope with a directory-rooted --glob %q; install ripgrep for full glob support: brew install ripgrep", a.Glob)
	}
	root := a.Scope
	if root == "" {
		root = "."
	}
	if include != "" {
		argv = append(argv, "--include="+include)
	}
	if globRoot != "" {
		root = globRoot
	}
	argv = append(argv, "-e", a.Query, "--", root)
	return argv, nil
}

// translateGlob maps the ccx --glob shapes system grep can express onto an
// --include pattern and/or a rooted search directory: a bare basename glob
// ("*.go") → include; "dir/**" → root dir; "dir/**/*.ext" → root dir + include.
// Braces and mid-path wildcards have no grep equivalent and fail fast.
func translateGlob(glob string) (include, root string, err error) {
	if glob == "" {
		return "", "", nil
	}
	if strings.ContainsAny(glob, "{}") {
		return "", "", untranslatable(glob)
	}
	if !strings.Contains(glob, "/") {
		return glob, "", nil
	}
	if dir, ok := strings.CutSuffix(glob, "/**"); ok {
		if strings.ContainsAny(dir, "*?[]") {
			return "", "", untranslatable(glob)
		}
		return "", dir, nil
	}
	if dir, rest, ok := strings.Cut(glob, "/**/"); ok {
		if strings.ContainsAny(dir, "*?[]") || strings.Contains(rest, "/") {
			return "", "", untranslatable(glob)
		}
		return rest, dir, nil
	}
	return "", "", untranslatable(glob)
}

func untranslatable(glob string) error {
	return fmt.Errorf("grep fallback cannot translate glob %q (braces and mid-path wildcards are unsupported); install ripgrep for full glob support: brew install ripgrep", glob)
}

// grepLine is one output line: a matched or context line at num carrying its
// source text.
type grepLine struct {
	num     int
	text    string
	isMatch bool
}

// fileGroup collects one file's lines in stream (line) order.
type fileGroup struct {
	path  string
	lines []grepLine
}

// parse folds an engine's raw output into per-file groups. tryContext and isFile
// only matter for system grep, whose "path:line:text" match delimiter and
// "path-line-text" context delimiter are ambiguous when the matched text embeds
// either shape; see parseGrep.
func parse(eng engine, raw string, tryContext bool, isFile func(string) bool) ([]fileGroup, error) {
	switch eng {
	case engineRipgrep:
		return parseRipgrep(raw)
	case engineGrep:
		return parseGrep(raw, tryContext, isFile), nil
	default:
		return nil, fmt.Errorf("ripgrep: unknown engine %d", eng)
	}
}

// rgEvent is one line of the rg --json event stream. Only match and context
// events carry a line; begin/end/summary are ignored.
type rgEvent struct {
	Type string `json:"type"`
	Data struct {
		Path       rgText `json:"path"`
		Lines      rgText `json:"lines"`
		LineNumber int    `json:"line_number"`
	} `json:"data"`
}

// rgText is an rg --json path or line payload: UTF-8 content arrives in text,
// and non-UTF-8 content (a binary match line, a non-UTF-8 filename) arrives as a
// base64 bytes field instead.
type rgText struct {
	Text  string `json:"text"`
	Bytes string `json:"bytes"`
}

// decode returns the payload's text, base64-decoding the bytes field when rg put
// non-UTF-8 content there instead. Invalid UTF-8 in the decoded bytes becomes
// replacement runes so the reshaped frame is never silently blank.
func (t rgText) decode() (string, error) {
	if t.Text != "" {
		return t.Text, nil
	}
	if t.Bytes == "" {
		return "", nil
	}
	raw, err := base64.StdEncoding.DecodeString(t.Bytes)
	if err != nil {
		return "", fmt.Errorf("ripgrep: decode rg bytes payload: %w", err)
	}
	return strings.ToValidUTF8(string(raw), "\uFFFD"), nil
}

// parseRipgrep folds the rg --json match/context events into per-file groups in
// first-appearance order.
func parseRipgrep(raw string) ([]fileGroup, error) {
	b := newGroupBuilder()
	for _, line := range strings.Split(raw, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var ev rgEvent
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			return nil, fmt.Errorf("ripgrep: parse rg --json event: %w", err)
		}
		switch ev.Type {
		case "match", "context":
			path, err := ev.Data.Path.decode()
			if err != nil {
				return nil, err
			}
			text, err := ev.Data.Lines.decode()
			if err != nil {
				return nil, err
			}
			b.add(path, ev.Data.LineNumber, text, ev.Type == "match")
		}
	}
	return b.groups(), nil
}

// statRegular reports whether path is a regular file on disk. It is the default
// seam parseGrep validates candidate paths through; the grep child ran in this
// same process's working directory, so a relative path resolves identically here.
// A directory named like a leading path prefix (e.g. "pkg" ahead of a colon-named
// file) must not satisfy it, or that directory would steal the field split.
func statRegular(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}

// parseGrep folds `grep -rnFI` output into per-file groups. Match lines use a
// ":line:" delimiter, -C context lines use a "-line-" delimiter, and "--" group
// separators are dropped. Both delimiters can appear inside the matched text —
// `const layout = "15:04:05"` embeds ":04:", `data[0:2:4]` embeds ":2:", a
// filename like `2024-01-migrate.go` embeds "-01-" — so a purely lexical split
// invents phantom paths. Instead every "sep digits sep" boundary is a candidate
// and the true field split is the leftmost one whose path is a regular file on
// disk (memoized per unique path); a directory named like a prefix cannot steal
// it. A match line's path wins over a context line's when both validate; when the
// search ran without -C (tryContext is false) a line can only be a match, so the
// "-line-" form is never tried.
func parseGrep(raw string, tryContext bool, isFile func(string) bool) []fileGroup {
	memo := map[string]bool{}
	valid := func(path string) bool {
		if v, ok := memo[path]; ok {
			return v
		}
		v := isFile(path)
		memo[path] = v
		return v
	}

	b := newGroupBuilder()
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSuffix(line, "\r")
		if line == "" || line == "--" {
			continue
		}
		if path, num, text, ok := firstValidSplit(line, ':', valid); ok {
			b.add(path, num, text, true)
			continue
		}
		if tryContext {
			if path, num, text, ok := firstValidSplit(line, '-', valid); ok {
				b.add(path, num, text, false)
			}
		}
	}
	return b.groups()
}

// firstValidSplit returns the leftmost "<path><sep><digits><sep><text>" split of
// line whose path is a regular file (via isFile). grep prints the path field
// first, so the leftmost boundary backed by a real file is the true split even
// when the matched text embeds the same "sep digits sep" shape further along.
func firstValidSplit(line string, sep byte, isFile func(string) bool) (path string, num int, text string, ok bool) {
	for i := 1; i < len(line); i++ {
		if line[i] != sep {
			continue
		}
		j := i + 1
		for j < len(line) && line[j] >= '0' && line[j] <= '9' {
			j++
		}
		if j == i+1 || j >= len(line) || line[j] != sep {
			continue
		}
		candidate := line[:i]
		if !isFile(candidate) {
			continue
		}
		n, _ := strconv.Atoi(line[i+1 : j])
		return candidate, n, line[j+1:], true
	}
	return "", 0, "", false
}

// groupBuilder accumulates lines into per-file groups, files in first-appearance
// order.
type groupBuilder struct {
	order []string
	byKey map[string]*fileGroup
}

func newGroupBuilder() *groupBuilder {
	return &groupBuilder{byKey: map[string]*fileGroup{}}
}

func (b *groupBuilder) add(path string, num int, text string, isMatch bool) {
	path = strings.TrimPrefix(path, "./")
	g, ok := b.byKey[path]
	if !ok {
		g = &fileGroup{path: path}
		b.byKey[path] = g
		b.order = append(b.order, path)
	}
	g.lines = append(g.lines, grepLine{num: num, text: strings.TrimRight(text, "\r\n"), isMatch: isMatch})
}

func (b *groupBuilder) groups() []fileGroup {
	out := make([]fileGroup, 0, len(b.order))
	for _, path := range b.order {
		out = append(out, *b.byKey[path])
	}
	return out
}

// reshape renders groups into the house grep format render.Finalize anchors:
// a "### <path>:<match lines>" section header per file, then a "→ [N] <text>"
// frame for each match and a "  [N] <text>" frame for each context line.
func reshape(query string, eng engine, groups []fileGroup) string {
	matches := 0
	for _, g := range groups {
		for _, l := range g.lines {
			if l.isMatch {
				matches++
			}
		}
	}

	var b strings.Builder
	if matches == 0 {
		fmt.Fprintf(&b, "# grep: %q — no matches\n", query)
		writeEngineNote(&b, eng)
		return b.String()
	}
	fmt.Fprintf(&b, "# grep: %q — %d matches in %d files\n", query, matches, len(groups))
	writeEngineNote(&b, eng)

	for _, g := range groups {
		var lines []string
		for _, l := range g.lines {
			if l.isMatch {
				lines = append(lines, strconv.Itoa(l.num))
			}
		}
		fmt.Fprintf(&b, "\n### %s:%s\n", g.path, strings.Join(lines, ","))
		for _, l := range g.lines {
			arrow := "  "
			if l.isMatch {
				arrow = "→ "
			}
			fmt.Fprintf(&b, "%s[%d] %s\n", arrow, l.num, l.text)
		}
	}
	return b.String()
}

// writeEngineNote discloses the system-grep degradation (hidden and binary paths
// skipped to match rg, but .gitignore not honored) so the model reads the
// fallback for what it is.
func writeEngineNote(b *strings.Builder, eng engine) {
	if eng == engineGrep {
		b.WriteString("# engine: system grep (ripgrep not found); hidden and binary files skipped; .gitignore not applied\n")
	}
}
