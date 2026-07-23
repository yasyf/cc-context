// Package ripgrep runs every `ccx code grep` through ripgrep — or system grep
// when rg is absent — and reshapes the engine's output into the house grep
// format, stamping content anchors onto each frame at generation time before
// render.Cap bounds it.
package ripgrep

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/bmatcuk/doublestar/v4"

	"github.com/yasyf/cc-context/anchor"
	"github.com/yasyf/cc-context/internal/backend"
	"github.com/yasyf/cc-context/internal/render"
	"github.com/yasyf/cc-context/internal/secrets"
)

// exitNoMatch is the exit code both rg and grep return for a clean no-match; it
// is tolerated (empty stdout, empty stderr) and distinguished from a real error
// exactly as astgrep.Run tolerates ast-grep's no-match.
const exitNoMatch = 1

// DefaultBudget caps an otherwise-unbudgeted grep so a flooding match set never
// blows the context window. render.Cap is a no-op at budget<=0, so only the CLI
// and MCP surfaces apply this default; codeexec leaves the grep uncapped by
// contract, filtering the output inside its sandbox.
const DefaultBudget = 2000

// maxContext bounds each of -A/-B/-C so a runaway context request can never bury
// the match frame — the payload — under leading context inside the output budget.
const maxContext = 100

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
	if a.FilesWithMatches {
		var conflicts []string
		for _, c := range []struct {
			flag string
			n    int
		}{
			{"--expand", a.Expand},
			{"-A/--after-context", a.After},
			{"-B/--before-context", a.Before},
			{"-C/--context", a.Context},
		} {
			if c.n != 0 {
				conflicts = append(conflicts, c.flag)
			}
		}
		if len(conflicts) > 0 {
			return fmt.Errorf("grep --files-with-matches cannot be combined with %s", strings.Join(conflicts, ", "))
		}
	}
	return nil
}

// appendContext appends the -C/-A/-B context flags both rg and grep accept
// natively. -C comes first so BSD grep lets a following -A or -B override that
// direction.
func appendContext(argv []string, a backend.Args) []string {
	if a.Context > 0 {
		argv = append(argv, "-C", strconv.Itoa(a.Context))
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

type escalation int

const (
	escNone escalation = iota
	escAutoRegex
	escBothMissed
)

// runnerFn is the process boundary: it runs bin+argv and returns stdout,
// tolerating the clean no-match exit. run takes it as a parameter so tests drive
// the reshaper with a canned engine transcript instead of a real subprocess.
type runnerFn func(ctx context.Context, bin string, argv []string) (string, error)

// Run resolves rg (or system grep), searches for a.Query, and returns the hits
// reshaped into the house grep format, content-anchored, secret-masked per file
// — contiguous match+context blocks as one text, and the header's query echo
// pathlessly (unless a.RevealSecrets) — and budget-capped, with the shared
// masked-secrets footer appended after the cap.
func Run(ctx context.Context, a backend.Args) (string, error) {
	if err := validateContext(a); err != nil {
		return "", err
	}
	eng, bin, err := resolveEngine()
	if err != nil {
		return "", err
	}
	out, _, err := run(ctx, eng, bin, a, execEngine)
	return out, err
}

// MatchLine is one line of a file's grep hits: its 1-based line number, source
// text (trailing CR/LF trimmed), and whether it is a match line versus an
// -A/-B/-C context line.
type MatchLine struct {
	Num     int
	Text    string
	IsMatch bool
}

// FileMatch is one file's grep hits in stream (line) order.
type FileMatch struct {
	Path  string
	Lines []MatchLine
}

// Matches searches for a.Query exactly as Run does — same engine resolution,
// argv building, glob-anchor peeling, and clean no-match tolerance — but returns
// the parsed per-file hits instead of reshaped, content-anchored, budget-capped
// text: no anchoring, no capping. It is the composition entry point native symbol
// and deps build on; a clean no-match yields zero FileMatches, not an error.
func Matches(ctx context.Context, a backend.Args) ([]FileMatch, error) {
	if err := validateContext(a); err != nil {
		return nil, err
	}
	eng, bin, err := resolveEngine()
	if err != nil {
		return nil, err
	}
	groups, err := searchGroups(ctx, eng, bin, a, execEngine)
	if err != nil {
		return nil, err
	}
	return toFileMatches(groups), nil
}

// toFileMatches projects the internal per-file groups onto the exported
// FileMatch shape, preserving file and line order.
func toFileMatches(groups []fileGroup) []FileMatch {
	out := make([]FileMatch, 0, len(groups))
	for _, g := range groups {
		lines := make([]MatchLine, 0, len(g.lines))
		for _, l := range g.lines {
			lines = append(lines, MatchLine{Num: l.num, Text: l.text, IsMatch: l.isMatch})
		}
		out = append(out, FileMatch{Path: g.path, Lines: lines})
	}
	return out
}

// run is the engine-agnostic Run core: search into groups, then reshape and cap.
// found is decided from parsed groups before reshape/cap — capping can cut the
// no-match header and a matched header embeds the query, so never string-sniff.
func run(ctx context.Context, eng engine, bin string, a backend.Args, exec runnerFn) (string, bool, error) {
	if a.FilesWithMatches {
		return runFilesWithMatches(ctx, eng, bin, a, exec)
	}
	groups, err := searchGroups(ctx, eng, bin, a, exec)
	if err != nil {
		return "", false, err
	}
	esc := escNone
	autoMode := !a.Regex
	if autoMode && !anyMatch(groups) && escalatable(eng, a.Query) {
		ra := a
		ra.Regex = true
		rgroups, rerr := searchGroups(ctx, eng, bin, ra, exec)
		if rerr == nil {
			if anyMatch(rgroups) {
				groups = rgroups
				esc = escAutoRegex
			} else {
				esc = escBothMissed
			}
		}
	}
	cwd, err := os.Getwd()
	if err != nil {
		return "", false, fmt.Errorf("ripgrep: resolve cwd: %w", err)
	}
	displayQuery := a.Query
	var maskedIDs []string
	if !a.RevealSecrets {
		maskedIDs = maskGroups(groups)
		var fired []string
		displayQuery, fired = secrets.Mask(a.Query, "")
		maskedIDs = append(maskedIDs, fired...)
	}
	reshaped := reshape(displayQuery, eng, groups, esc, anchor.NewFiles(cwd))
	found := anyMatch(groups)
	out := render.Cap(reshaped, a.Budget)
	if autoMode && !found && hasBREEscape(a.Query) {
		out += regexHintLine + "\n"
	}
	return render.WithSecretsFooter(out, maskedIDs), found, nil
}

// maskGroups masks each file's contiguous match+context blocks, each as one
// text with the file's path as the rule context — so a multiline rule
// (private-key) fires across a block's lines — returning the fired rule ids in
// group then span order. A masked span that swallows line breaks folds the
// swallowed frames into the span's first: the surviving frame keeps its own
// number and is a match when any folded frame was.
func maskGroups(groups []fileGroup) []string {
	var ids []string
	for gi := range groups {
		ids = maskGroup(&groups[gi], ids)
	}
	return ids
}

// maskGroup rebuilds one group's frames from its blocks' masked lines,
// returning ids extended with the fired rules. A block is a run of
// consecutively numbered lines — the unit rg/grep emit between context gaps.
func maskGroup(g *fileGroup, ids []string) []string {
	var rebuilt []grepLine
	for start := 0; start < len(g.lines); {
		end := start + 1
		for end < len(g.lines) && g.lines[end].num == g.lines[end-1].num+1 {
			end++
		}
		block := g.lines[start:end]
		texts := make([]string, len(block))
		for i, l := range block {
			texts[i] = l.text
		}
		masked := secrets.MaskLines(texts, g.path)
		for j, ml := range masked {
			cover := len(block)
			if j+1 < len(masked) {
				cover = masked[j+1].Src
			}
			frame := grepLine{num: block[ml.Src].num, text: ml.Text}
			for _, l := range block[ml.Src:cover] {
				if l.isMatch {
					frame.isMatch = true
				}
			}
			rebuilt = append(rebuilt, frame)
			ids = append(ids, ml.Rules...)
		}
		start = end
	}
	g.lines = rebuilt
	return ids
}

func runFilesWithMatches(ctx context.Context, eng engine, bin string, a backend.Args, exec runnerFn) (string, bool, error) {
	paths, err := searchFilesWithMatches(ctx, eng, bin, a, exec)
	if err != nil {
		return "", false, err
	}
	if !a.Regex && len(paths) == 0 && escalatable(eng, a.Query) {
		ra := a
		ra.Regex = true
		rpaths, rerr := searchFilesWithMatches(ctx, eng, bin, ra, exec)
		if rerr == nil && len(rpaths) > 0 {
			paths = rpaths
		}
	}
	if len(paths) == 0 {
		// The no-match header echoes the query; mask it so a searched-for secret
		// value is never reprinted raw. The path list itself never echoes it.
		displayQuery := a.Query
		var maskedIDs []string
		if !a.RevealSecrets {
			displayQuery, maskedIDs = secrets.Mask(a.Query, "")
		}
		return render.WithSecretsFooter(renderFilesWithMatches(displayQuery, paths, a.Budget), maskedIDs), false, nil
	}
	return renderFilesWithMatches(a.Query, paths, a.Budget), true, nil
}

func renderFilesWithMatches(query string, paths []string, budget int) string {
	if len(paths) == 0 {
		return render.Cap(NoMatch(query), budget)
	}
	rendered := strings.Join(paths, "\n") + "\n"
	const charsPerToken = 4
	if budget <= 0 || budget > (len(rendered)-1)/charsPerToken {
		return rendered
	}

	limit := budget * charsPerToken
	var b strings.Builder
	cutoff := 0
	for cutoff < len(paths) && b.Len()+len(paths[cutoff])+1 <= limit {
		b.WriteString(paths[cutoff])
		b.WriteByte('\n')
		cutoff++
	}
	omitted := strings.Join(paths[cutoff:], "\n") + "\n"
	fmt.Fprintf(&b, "… +%d lines, ~%d tokens omitted — re-run with a larger --budget\n",
		len(paths)-cutoff, len(omitted)/charsPerToken)
	return b.String()
}

func escalatable(eng engine, q string) bool {
	if eng == engineGrep && strings.ContainsRune(q, '\\') {
		return false
	}
	if q == regexp.QuoteMeta(q) {
		return false
	}
	_, err := regexp.Compile(q)
	return err == nil
}

// searchGroups builds the engine argv, executes it, and folds the raw output into
// per-file groups: the parse layer Run and Matches share, everything before
// reshape, anchoring, and capping. An rg failure that is really a glob/type
// filter matching zero files is normalized to the clean no-match grep reports as
// exit 0; a regex-parse failure carrying a BRE escape is hinted.
func searchGroups(ctx context.Context, eng engine, bin string, a backend.Args, exec runnerFn) ([]fileGroup, error) {
	raw, err := searchOutput(ctx, eng, bin, a, exec)
	if err != nil {
		return nil, err
	}
	return parse(eng, raw)
}

func searchFilesWithMatches(ctx context.Context, eng engine, bin string, a backend.Args, exec runnerFn) ([]string, error) {
	raw, err := searchOutput(ctx, eng, bin, a, exec)
	if err != nil {
		return nil, err
	}
	return parseFilesWithMatches(eng, raw)
}

func searchOutput(ctx context.Context, eng engine, bin string, a backend.Args, exec runnerFn) (string, error) {
	argv, err := buildArgv(eng, a)
	if err != nil {
		return "", err
	}
	raw, err := exec(ctx, bin, argv)
	if err != nil {
		if eng != engineRipgrep {
			return "", err
		}
		if !ripgrepNoFiles(err) {
			return "", ripgrepRegexHint(err, a.Query)
		}
		raw = "" // a glob/type filter matched zero files: the clean no-match grep reports as exit 0
	}
	return raw, nil
}

// anyMatch reports whether any group carries a match line (context lines alone
// never count).
func anyMatch(groups []fileGroup) bool {
	for _, g := range groups {
		for _, l := range g.lines {
			if l.isMatch {
				return true
			}
		}
	}
	return false
}

// execEngine is the real process boundary, tolerating the no-match exit.
func execEngine(ctx context.Context, bin string, argv []string) (string, error) {
	return render.RunCLIAllowExit(ctx, bin, argv, exitNoMatch)
}

// ripgrepNoFiles reports whether an rg failure carries the exit-2 "no files were
// searched" diagnostic (a glob/type filter matched zero files), which the grep
// fallback reports as a clean exit-0 no-match. The phrase is unique to that case,
// so every other exit-2 stays an error; this reads rg's stderr, not our wording.
func ripgrepNoFiles(err error) bool {
	return strings.Contains(err.Error(), "No files were searched")
}

const regexHintLine = `hint: --regex is Rust syntax — alternation is |, groups ( ); BRE-style \| and \( match the literal character`

// ripgrepRegexHint appends regexHintLine to an rg regex-parse error when pattern
// carries a BRE-style \|, \(, or \) escape; any other error is returned unchanged.
// The parse-error signature is disjoint from ripgrepNoFiles's.
func ripgrepRegexHint(err error, pattern string) error {
	msg := err.Error()
	if !strings.Contains(msg, "regex parse error") && !strings.Contains(msg, "unclosed group") {
		return err
	}
	if !hasBREEscape(pattern) {
		return err
	}
	return fmt.Errorf("%w\n%s", err, regexHintLine)
}

func hasBREEscape(q string) bool {
	return strings.Contains(q, `\|`) || strings.Contains(q, `\(`) || strings.Contains(q, `\)`)
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
		kept, err := filterGlobPaths(a.Paths, a.Glob)
		if err != nil {
			return nil, err
		}
		a.Paths = kept
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

// filterGlobPaths partitions explicit grep operands against --glob so both engines
// search one file set: an existing regular file survives only when fileMatchesGlob
// keeps it (rg's -g does not filter explicit files, so they are prefiltered here),
// while a directory passes through for native --glob/--include recursion. Every
// file filtered out with nothing passing through is a loud clean no-match.
func filterGlobPaths(paths []string, glob string) ([]string, error) {
	var kept []string
	for _, p := range paths {
		info, err := os.Stat(p)
		if err != nil || info.IsDir() {
			kept = append(kept, p)
			continue
		}
		ok, err := fileMatchesGlob(glob, p)
		if err != nil {
			return nil, err
		}
		if ok {
			kept = append(kept, p)
		}
	}
	if len(kept) == 0 {
		return nil, fmt.Errorf("no paths match --glob %q", glob)
	}
	return kept, nil
}

// fileMatchesGlob reports whether file operand p matches glob under rg's -g
// convention: a slash-less glob matches the basename at any depth, a slashed glob
// the whole slash-normalized path.
func fileMatchesGlob(glob, p string) (bool, error) {
	p = filepath.ToSlash(p)
	if !strings.Contains(glob, "/") {
		return doublestar.Match(glob, path.Base(p))
	}
	return doublestar.Match(glob, p)
}

// AnchorGrepArgs peels an existing directory prefix off a.Glob into a.Scope so a
// glob anchored at a real directory is searched even when ignore rules exclude
// it. An explicit a.Scope composes rather than short-circuits: the prefix joins
// onto it, and the joined directory becomes the new scope. A fully-literal glob
// naming a regular file anchors to that file's parent directory with the basename
// as the glob. When the joined prefix does not exist on disk, a is returned
// unchanged. backend.SplitGlobAnchor does the metachar-free prefix split.
func AnchorGrepArgs(a backend.Args) backend.Args {
	dir, rest := backend.SplitGlobAnchor(a.Glob)
	if dir == "" {
		return a
	}
	joined := dir
	if a.Scope != "" {
		joined = filepath.Join(a.Scope, dir)
	}
	info, err := os.Stat(joined)
	if err != nil {
		return a
	}
	switch {
	case info.IsDir():
		a.Scope, a.Glob = joined, rest
	case rest == "" && info.Mode().IsRegular():
		a.Scope, a.Glob = filepath.Dir(joined), filepath.Base(joined)
	}
	return a
}

// ripgrepArgv builds `rg --json [--fixed-strings] [-i] [-w] [--glob G] [-C N]
// [--no-ignore-parent] -e <pattern> [-- [scope] paths...]`. Files-only mode
// replaces --json with --files-with-matches. --fixed-strings is
// dropped for a regex query so the pattern reaches rg's Rust regex engine; any
// explicit Paths ride after -- alongside the scope operand, so rg searches those
// files. With explicit Paths anchoring is skipped (buildArgv has already prefiltered
// the file operands against --glob, since rg's -g never filters explicit files), so
// -g only narrows a directory operand's recursion. Otherwise AnchorGrepArgs first
// peels an existing directory prefix off the glob into a path operand — composing onto an
// explicit --scope — so both engines (rg and the grep fallback) search the same
// file set: the operand becomes the anchored directory and the glob its
// leftover basename pattern (empty when the anchor consumed the whole glob, so no
// -g is emitted and the operand alone selects the files). rg matches a bare
// basename glob against the printed path at any depth — a full or absolute path
// glob would match nothing under the operand. The pattern rides -e so a
// leading-dash literal is never mistaken for a flag, and the operand rides after
// -- so a value like "--hidden" is never parsed as one. Whenever an operand is
// present --no-ignore-parent skips the outer .gitignore rg would otherwise apply
// to an explicit path while still honoring ignore files inside it.
func ripgrepArgv(a backend.Args) []string {
	if len(a.Paths) == 0 {
		a = AnchorGrepArgs(a)
	}
	argv := []string{"--json"}
	if a.FilesWithMatches {
		argv = []string{"--files-with-matches"}
	}
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
// [--include=G] -e <pattern> -- <root>` from flags common to BSD and GNU grep.
// Files-only mode uses -l without --null. A
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
// "." or the "./" prefix. The excludes apply on every branch, so a hidden file
// found by recursing a directory operand is skipped exactly as rg skips it — at the
// cost of a named dotfile operand being skipped too (GNU grep applies --exclude to
// command-line operands), an accepted fallback degradation the engine note discloses.
// --glob is translated to an --include and/or a rooted
// scope; a glob shape grep cannot express, or a directory-rooted glob combined
// with an explicit --scope, fails fast. Explicit Paths are the operands directly and
// combine with --glob: buildArgv prefilters the file operands, and the translated
// --include narrows any directory operand's recursion (a directory-rooted glob has no
// operand-compatible translation, so it fails fast). Anchoring is skipped with
// explicit Paths so an anchored glob can never widen past them; otherwise
// AnchorGrepArgs first peels an existing directory prefix off the glob
// into the scope — composing onto an explicit --scope — exactly as the ripgrep
// route does, so both engines search the same file set; the peel also
// keeps an anchored glob like pkg/*.go expressible here (the anchor becomes the
// search root and the basename remainder the --include) where the unpeeled form
// has no grep translation.
func grepArgv(a backend.Args) ([]string, error) {
	if len(a.Paths) == 0 {
		a = AnchorGrepArgs(a)
	}
	flags := "-rnHFI"
	if a.Regex {
		flags = "-rnHEI"
	}
	argv := []string{flags, "--null"}
	if a.FilesWithMatches {
		flags = "-rFI"
		if a.Regex {
			flags = "-rEI"
		}
		argv = []string{flags, "-l"}
	}
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
	argv = append(argv, "--exclude-dir=.[!./]*", "--exclude=.[!./]*")

	include, globRoot, err := translateGlob(a.Glob)
	if err != nil {
		return nil, err
	}

	if len(a.Paths) > 0 {
		if globRoot != "" {
			return nil, fmt.Errorf("grep fallback cannot combine explicit paths with a directory-rooted --glob %q; install ripgrep for full glob support: brew install ripgrep", a.Glob)
		}
		if include != "" {
			argv = append(argv, "--include="+include)
		}
		argv = append(argv, "-e", a.Query, "--")
		if a.Scope != "" {
			argv = append(argv, a.Scope)
		}
		return append(argv, a.Paths...), nil
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
// ("*.go") → include; "**/*.ext" (the remainder AnchorGrepArgs leaves after
// peeling "dir/**/*.ext", matching any depth exactly as --include does) →
// include; "dir/**" → root dir; "dir/**/*.ext" → root dir + include. Braces and
// mid-path wildcards have no grep equivalent and fail fast. A dot-leading
// basename fails fast too — grepArgv's unconditional hidden-path excludes prune
// what the glob selects, and an --include like ".*.go" fnmatches across '/' onto
// the "./" path prefix, matching plain "normal.go" — as does a bare "**/" (an
// empty basename would emit no --include and search everything, where rg matches
// nothing); loud inexpressibility beats silently wrong results.
func translateGlob(glob string) (include, root string, err error) {
	if glob == "" {
		return "", "", nil
	}
	if strings.ContainsAny(glob, "{}") {
		return "", "", untranslatable(glob)
	}
	if !strings.Contains(glob, "/") {
		if strings.HasPrefix(glob, ".") {
			return "", "", untranslatable(glob)
		}
		return glob, "", nil
	}
	if rest, ok := strings.CutPrefix(glob, "**/"); ok && !strings.Contains(rest, "/") {
		if rest == "" || strings.HasPrefix(rest, ".") {
			return "", "", untranslatable(glob)
		}
		return rest, "", nil
	}
	if dir, ok := strings.CutSuffix(glob, "/**"); ok {
		if strings.ContainsAny(dir, "*?[]") {
			return "", "", untranslatable(glob)
		}
		return "", dir, nil
	}
	if dir, rest, ok := strings.Cut(glob, "/**/"); ok {
		if strings.ContainsAny(dir, "*?[]") || strings.Contains(rest, "/") || rest == "" || strings.HasPrefix(rest, ".") {
			return "", "", untranslatable(glob)
		}
		return rest, dir, nil
	}
	return "", "", untranslatable(glob)
}

func untranslatable(glob string) error {
	return fmt.Errorf("grep fallback cannot translate glob %q (braces, mid-path wildcards, and dot-leading basenames are unsupported); install ripgrep for full glob support: brew install ripgrep", glob)
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

func parseFilesWithMatches(eng engine, raw string) ([]string, error) {
	switch eng {
	case engineRipgrep, engineGrep:
	default:
		return nil, fmt.Errorf("ripgrep: unknown engine %d", eng)
	}
	cwd, err := os.Getwd()
	if err != nil {
		return nil, fmt.Errorf("ripgrep: resolve cwd: %w", err)
	}
	cwdPrefix := cwd
	if !strings.HasSuffix(cwdPrefix, string(filepath.Separator)) {
		cwdPrefix += string(filepath.Separator)
	}
	var paths []string
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSuffix(line, "\r")
		if line == "" {
			continue
		}
		if filepath.IsAbs(line) {
			if relative, found := strings.CutPrefix(line, cwdPrefix); found {
				line = relative
			}
		}
		// grep roots an implicit search at "." and prefixes every hit with "./"; rg emits bare
		// relative paths. Strip the prefix so both engines return the same shape.
		line = strings.TrimPrefix(line, "."+string(filepath.Separator))
		paths = append(paths, line)
	}
	return paths, nil
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

// NoMatch is the house no-match header the grep engine emits so a zero-hit
// search reads identically whether rg or the system-grep fallback ran.
func NoMatch(query string) string {
	return fmt.Sprintf("# grep: %q — no matches\n", query)
}

// reshape renders groups into the house grep format: a "### <path>:<match lines>"
// section header per file, then a "→ [N#hash] <text>" frame for each match and a
// "  [N#hash] <text>" frame for each context line. Each frame's content anchor is
// stamped here at generation time, hashed from the file's line via files; a frame
// whose file or line the cache cannot resolve stays a bare "[N]".
func reshape(query string, eng engine, groups []fileGroup, esc escalation, files *anchor.Files) string {
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
		if esc == escBothMissed {
			fmt.Fprintf(&b, "# grep: %q — no matches (literal or regex)\n", query)
		} else {
			b.WriteString(NoMatch(query))
		}
		writeEngineNote(&b, eng)
		return b.String()
	}
	fmt.Fprintf(&b, "# grep: %q — %d matches in %d files", query, matches, len(groups))
	if esc == escAutoRegex {
		b.WriteString(" (auto-regex)")
	}
	b.WriteByte('\n')
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
			fmt.Fprintf(&b, "%s[%s] %s\n", arrow, anchoredSpan(g.path, l.num, files), l.text)
		}
	}
	return b.String()
}

// anchoredSpan renders a frame's bracket span, appending the content anchor
// hashed from path's line n when the cache resolves it; a miss — unreadable file
// or out-of-range line — yields the bare line number.
func anchoredSpan(path string, n int, files *anchor.Files) string {
	span := strconv.Itoa(n)
	if text, ok := files.LineAt(path, n); ok {
		span += "#" + string(anchor.Of(text))
	}
	return span
}

// writeEngineNote discloses the system-grep degradation (hidden and binary paths
// skipped to match rg, but .gitignore not honored) so the model reads the
// fallback for what it is.
func writeEngineNote(b *strings.Builder, eng engine) {
	if eng == engineGrep {
		b.WriteString("# engine: system grep (ripgrep not found); hidden and binary files skipped; .gitignore not applied\n")
	}
}
