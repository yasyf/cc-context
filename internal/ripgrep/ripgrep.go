// Package ripgrep runs a case-insensitive, word-boundary, regex, or multi-file
// `ccx code grep` through ripgrep — or system grep when rg is absent — and
// reshapes either engine's output into the house grep format so render.Finalize
// anchors and caps it identically to tilth grep. Handles is the routing predicate.
package ripgrep

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
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

// DefaultBudget caps an otherwise-unbudgeted rg/grep grep so a flooding match set
// never blows the context window. render.Cap is a no-op at budget<=0, so only the
// CLI and MCP surfaces apply this default; codeexec leaves the grep uncapped by
// contract, filtering the output inside its sandbox.
const DefaultBudget = 2000

// Handles reports whether the rg/grep engine, not tilth, serves this grep. It is
// the single routing predicate the CLI, proxy, and MCP surfaces share: any of
// case-insensitivity, whole-word matching, regex, explicit file operands, or
// grep-style context lines (-A/-B/-C) needs a capability tilth's literal
// whole-tree search cannot express.
func Handles(a backend.Args) bool {
	return a.IgnoreCase || a.Word || a.Regex || len(a.Paths) > 0 || hasContext(a)
}

// maxContext bounds each of -A/-B/-C so a runaway context request can never bury
// the match frame — the payload — under leading context inside the output budget.
const maxContext = 100

// hasContext reports whether a carries any grep-style context request via
// -A/-B/-C (After/Before/Context). A negative value counts: it routes the grep to
// this engine so validateContext rejects it loudly rather than the >0 argv guards
// silently dropping it back to a contextless tilth search.
func hasContext(a backend.Args) bool {
	return a.After != 0 || a.Before != 0 || a.Context != 0
}

// validateContext rejects an out-of-range context request: a negative value names
// the offending flag, and a value past maxContext names the ceiling. It is the
// single gate every grep surface (CLI, MCP, exec) passes through via Run.
func validateContext(a backend.Args) error {
	for _, c := range []struct {
		flag string
		n    int
	}{{"-A/--after-context", a.After}, {"-B/--before-context", a.Before}, {"-C/--context", a.Context}} {
		if c.n < 0 {
			return fmt.Errorf("grep %s must be ≥ 0, got %d", c.flag, c.n)
		}
		if c.n > maxContext {
			return fmt.Errorf("grep %s is capped at %d context lines, got %d; narrow the search instead", c.flag, maxContext, c.n)
		}
	}
	return nil
}

// appendContext appends the -A/-B/-C context flags both rg and grep accept
// natively. -C sets both directions, so it is emitted alone when set; otherwise
// -A and -B ride independently.
func appendContext(argv []string, a backend.Args) []string {
	if a.Context > 0 {
		return append(argv, "-C", strconv.Itoa(a.Context))
	}
	if a.After > 0 {
		argv = append(argv, "-A", strconv.Itoa(a.After))
	}
	if a.Before > 0 {
		argv = append(argv, "-B", strconv.Itoa(a.Before))
	}
	return argv
}

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
	if err := validateContext(a); err != nil {
		return "", err
	}
	eng, bin, err := resolveEngine()
	if err != nil {
		return "", err
	}
	return run(ctx, eng, bin, a, execEngine)
}

// run is the engine-agnostic core: build argv, execute, parse, reshape, finalize.
func run(ctx context.Context, eng engine, bin string, a backend.Args, exec runnerFn) (string, error) {
	argv, err := buildArgv(eng, a)
	if err != nil {
		return "", err
	}
	raw, err := exec(ctx, bin, argv)
	if err != nil {
		return "", err
	}
	groups, err := parse(eng, raw)
	if err != nil {
		return "", err
	}
	return render.Finalize(backend.OpGrep, reshape(a.Query, eng, groups), a)
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
	return 0, "", fmt.Errorf("ccx code grep -i/-w/-E and multi-file search need ripgrep or grep on PATH; install ripgrep: brew install ripgrep")
}

func buildArgv(eng engine, a backend.Args) ([]string, error) {
	if len(a.Paths) > 0 && a.Glob != "" {
		return nil, fmt.Errorf("grep cannot combine explicit file paths with --glob %q; drop one", a.Glob)
	}
	switch eng {
	case engineRipgrep:
		return ripgrepArgv(a), nil
	case engineGrep:
		return grepArgv(a)
	default:
		return nil, fmt.Errorf("ripgrep: unknown engine %d", eng)
	}
}

// ripgrepArgv builds `rg --json [--fixed-strings] [-i] [-w] [--glob G] [-C N]
// [--no-ignore-parent] -e <pattern> [-- [scope] paths...]`. --fixed-strings is
// dropped for a regex query so the pattern reaches rg's Rust regex engine; any
// explicit Paths ride after -- alongside the scope operand, so rg searches those
// files. AnchorGrepArgs first peels an
// existing directory prefix off the glob into a path operand — composing onto an
// explicit --scope — exactly as the tilth route does, so the two engines search
// the same file set: the operand becomes the anchored directory and the glob its
// leftover basename pattern (empty when the anchor consumed the whole glob, so no
// -g is emitted and the operand alone selects the files). rg matches a bare
// basename glob against the printed path at any depth, matching tilth's recursive
// scope semantics — a full or absolute path glob would match nothing under the
// operand. The pattern rides -e so a leading-dash literal is never mistaken for a
// flag, and the operand rides after -- so a value like "--hidden" is never parsed
// as one. Whenever an operand is present --no-ignore-parent skips the outer
// .gitignore rg would otherwise apply to an explicit path while still honoring
// ignore files inside it — parity with tilth's scope semantics.
func ripgrepArgv(a backend.Args) []string {
	a = backend.AnchorGrepArgs(a)
	argv := []string{"--json"}
	if !a.Regex {
		argv = append(argv, "--fixed-strings")
	}
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
	argv = appendContext(argv, a)
	if a.Scope != "" {
		argv = append(argv, "--no-ignore-parent")
	}
	argv = append(argv, "-e", a.Query)
	if a.Scope != "" || len(a.Paths) > 0 {
		argv = append(argv, "--")
		if a.Scope != "" {
			argv = append(argv, a.Scope)
		}
		argv = append(argv, a.Paths...)
	}
	return argv
}

// grepArgv builds `grep -rnHFI --null [-i] [-w] [-C N] --exclude-dir=.[!./]* --exclude=.[!./]*
// [--include=G] -e <pattern> -- <root>` from flags common to BSD and GNU grep. A
// regex query swaps -rnHFI for -rnHEI (ERE, the closest dialect to rg's Rust regex).
// The -H flag forces the filename prefix the parser splits on: GNU grep omits it
// for a single explicit file operand (BSD prints it), which read as "no matches".
// --null terminates that filename with a NUL — a byte no path or source line
// contains — so parseGrep splits the path from the "line:text" tail with no
// filename-vs-content ambiguity (the long form is portable: BSD -Z is not --null).
// The -I flag skips binary files and the dotdir/dotfile excludes skip hidden paths,
// both mirroring ripgrep's defaults so the two engines return the same hit set;
// -- terminates flag parsing so a directory-rooted scope never reads as a flag.
// The `.[!./]*` glob — not the simpler
// `.*` — is deliberate: BSD grep also fnmatches --exclude patterns against the
// whole "./"-prefixed path, so `.*` (and `.?*`, `.[!.]*`) match the search root
// "." and every "./sub" path, excluding the entire tree; requiring a non-dot,
// non-slash second character matches a hidden *basename* without ever matching
// "." or the "./" prefix. --glob is translated to an --include and/or a rooted
// scope; a glob shape grep cannot express, or a directory-rooted glob combined
// with an explicit --scope, fails fast. Explicit Paths are the operands directly;
// they cannot combine with --glob, and the excludes are dropped — GNU grep applies
// --exclude to command-line operands, so a named dotfile must not be silently
// skipped.
func grepArgv(a backend.Args) ([]string, error) {
	flags := "-rnHFI"
	if a.Regex {
		flags = "-rnHEI"
	}
	argv := []string{flags, "--null"}
	if a.IgnoreCase {
		argv = append(argv, "-i")
	}
	if a.Word {
		argv = append(argv, "-w")
	}
	if a.Expand > 0 {
		argv = append(argv, "-C", strconv.Itoa(a.Expand))
	}
	argv = appendContext(argv, a)

	if len(a.Paths) > 0 {
		if a.Glob != "" {
			return nil, fmt.Errorf("grep fallback cannot combine explicit file paths with --glob %q; drop one", a.Glob)
		}
		argv = append(argv, "-e", a.Query, "--")
		if a.Scope != "" {
			argv = append(argv, a.Scope)
		}
		return append(argv, a.Paths...), nil
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

// parse folds an engine's raw output into per-file groups.
func parse(eng engine, raw string) ([]fileGroup, error) {
	switch eng {
	case engineRipgrep:
		return parseRipgrep(raw)
	case engineGrep:
		return parseGrep(raw), nil
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

// parseGrep folds `grep -rnHFI --null` output into per-file groups. --null prints
// each path terminated by a NUL, the one byte a filename and a source line can
// never contain, so the path is exactly the bytes before the first NUL. The tail
// after the NUL is "<line><sep><text>": a ':' separator marks a match line, a '-'
// separator a -A/-B/-C context line. Lines with no NUL — the "--" group separator
// and the trailing blank — are dropped.
func parseGrep(raw string) []fileGroup {
	b := newGroupBuilder()
	for _, line := range strings.Split(raw, "\n") {
		nul := strings.IndexByte(line, 0)
		if nul < 0 {
			continue
		}
		num, isMatch, text, ok := splitNulTail(line[nul+1:])
		if !ok {
			continue
		}
		b.add(line[:nul], num, text, isMatch)
	}
	return b.groups()
}

// splitNulTail parses the "<digits><sep><text>" tail that follows a --null path
// terminator: sep ':' marks a match line, '-' a context line. ok is false for a
// tail without a leading digit run closed by ':' or '-'.
func splitNulTail(tail string) (num int, isMatch bool, text string, ok bool) {
	i := 0
	for i < len(tail) && tail[i] >= '0' && tail[i] <= '9' {
		i++
	}
	if i == 0 || i >= len(tail) || (tail[i] != ':' && tail[i] != '-') {
		return 0, false, "", false
	}
	n, _ := strconv.Atoi(tail[:i])
	return n, tail[i] == ':', tail[i+1:], true
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

// NoMatch is the house no-match header both the ripgrep and tilth grep routes
// emit so a zero-hit search reads identically regardless of engine.
func NoMatch(query string) string {
	return fmt.Sprintf("# grep: %q — no matches\n", query)
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
		b.WriteString(NoMatch(query))
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
