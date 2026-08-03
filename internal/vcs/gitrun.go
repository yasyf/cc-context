package vcs

import (
	"context"
	"fmt"
	"strings"

	"github.com/yasyf/cc-context/internal/render"
)

// gitLiteralPathspecs disables pathspec globbing and the :(magic) prefix for
// every child the runner spawns, so a path git itself just enumerated —
// "sub/[id].go", ":(exclude)x" — addresses itself and not its neighbors on the
// way back in. It is set on the environment rather than as a --literal-pathspecs
// flag because a leading flag shifts every positional the fake-git test harnesses
// read.
const gitLiteralPathspecs = "GIT_LITERAL_PATHSPECS=1"

// GitArgs is one NUL-framed git invocation, split into the slots that decide how
// each token is parsed. The runner assembles them structurally — --end-of-options
// between Sub and Revs, -- before Paths, -z last among the options — so there is
// no flat argv for a caller to omit a separator from: a rev cannot be read as an
// option and a path cannot be read as either.
type GitArgs struct {
	// Dir is the working directory the child runs in; empty inherits ccx's own.
	Dir string
	// GitDir is the repository passed as --git-dir, for a query about a repository
	// other than the one Dir sits in; empty omits the flag.
	GitDir string
	// Sub is the subcommand and its own options ("diff", "--cached", "-M").
	Sub []string
	// Revs are the revisions, each already qualified by construction (GitRef).
	Revs []GitRef
	// Paths are repo-relative pathspecs, the only channel to a pathspec; they land
	// after -- and are matched literally.
	Paths []string
}

// gitRun assembles a's slots into an argv and returns the child's raw stdout.
// extraSub carries the shape's own options, appended after the caller's Sub.
// Every shape helper goes through here, so -z, --end-of-options, --, and
// GIT_LITERAL_PATHSPECS are properties of the mechanism, not of a call site.
func gitRun(ctx context.Context, a GitArgs, extraSub ...string) (string, error) {
	for _, rev := range a.Revs {
		if rev == pathspecSeparator {
			return "", fmt.Errorf("%s: %q is git's pathspec separator, not a revision", strings.Join(a.Sub, " "), rev)
		}
	}
	out, err := render.RunCLIEnvDir(ctx, a.Dir, "git", gitArgv(a, extraSub...), []string{gitLiteralPathspecs})
	if err != nil {
		return "", fmt.Errorf("%s: %w", strings.Join(a.Sub, " "), err)
	}
	return out, nil
}

// pathspecSeparator is the token the runner interposes ahead of Paths, and the
// one token a rev may never be: git reads it structurally wherever it lands, so
// a "--" arriving through Revs — which UnsafeRef makes reachable from a
// user-typed revision — would silently become an empty pathspec set that matches
// nothing, reporting a clean answer at exit 0.
const pathspecSeparator GitRef = "--"

// gitArgv lays a's slots out in git's own order. --end-of-options always
// separates the options from the revisions, so a rev can never be parsed as an
// option; -- precedes the pathspecs, and only when there are pathspecs, because a
// subcommand that takes none (git worktree list) rejects a bare -- outright.
func gitArgv(a GitArgs, extraSub ...string) []string {
	argv := make([]string, 0, len(a.Sub)+len(extraSub)+len(a.Revs)+len(a.Paths)+5)
	if a.GitDir != "" {
		argv = append(argv, "--git-dir", a.GitDir)
	}
	argv = append(argv, a.Sub...)
	argv = append(argv, extraSub...)
	argv = append(argv, "-z", "--end-of-options")
	for _, rev := range a.Revs {
		argv = append(argv, string(rev))
	}
	if len(a.Paths) > 0 {
		argv = append(argv, string(pathspecSeparator))
		argv = append(argv, a.Paths...)
	}
	return argv
}

// nulTokens splits a -z stream into its tokens, dropping the terminator that
// closes the final one. An empty stream yields no tokens; an interior empty
// token is preserved, because that is how the porcelain shapes separate records.
func nulTokens(out string) []string {
	if out == "" {
		return nil
	}
	tokens := strings.Split(out, "\x00")
	if last := len(tokens) - 1; tokens[last] == "" {
		tokens = tokens[:last]
	}
	return tokens
}

// GitPaths runs a NUL-framed path enumeration (git diff --name-only, git ls-files)
// and returns the paths verbatim. Without -z git quotes any path carrying a
// newline, a quote, or a non-ASCII byte, so the caller would have to unquote a
// leading '"' back off a zero-width-joiner filename; here there is nothing to
// unquote.
func GitPaths(ctx context.Context, a GitArgs) ([]string, error) {
	out, err := gitRun(ctx, a)
	if err != nil {
		return nil, err
	}
	return nulTokens(out), nil
}

// NameStatusEntry is one file of a `git diff --name-status -z` stream. New is the
// entry's path under every status; Old carries the pre-image path on a rename or
// copy, which git emits before the new one — the reverse of the porcelain status
// shape, which is why neither order is ever spelled positionally at a call site.
type NameStatusEntry struct {
	// Status is git's raw status token: "M", "A", "D", "T", "U", "B", or a rename
	// or copy with its similarity score ("R100", "C75").
	Status string
	// Old is the pre-image path on a rename or copy, empty otherwise.
	Old string
	// New is the post-image path — the file as it exists after the diff.
	New string
}

// Renamed reports whether the entry is a rename, the statuses whose Old is the
// path a caller must read the pre-image blob from.
func (e NameStatusEntry) Renamed() bool { return strings.HasPrefix(e.Status, "R") }

// GitNameStatus runs a `git diff --name-status` enumeration and decodes its
// NUL-framed records. Rename detection is the caller's to request via Sub (-M).
func GitNameStatus(ctx context.Context, a GitArgs) ([]NameStatusEntry, error) {
	out, err := gitRun(ctx, a, "--name-status")
	if err != nil {
		return nil, err
	}
	return parseNameStatus(nulTokens(out))
}

// parseNameStatus decodes the status-then-path token stream shared by
// `git diff --name-status -z` and `git log --name-status -z`: a status token,
// then one path token, or — when the status is a rename or copy — the old path
// then the new one.
func parseNameStatus(tokens []string) ([]NameStatusEntry, error) {
	var entries []NameStatusEntry
	for i := 0; i < len(tokens); {
		status := tokens[i]
		if status == "" {
			return nil, fmt.Errorf("name-status: empty status token at %d", i)
		}
		want := 2
		if status[0] == 'R' || status[0] == 'C' {
			want = 3
		}
		if i+want > len(tokens) {
			return nil, fmt.Errorf("name-status: status %q wants %d tokens, %d left", status, want, len(tokens)-i)
		}
		if want == 3 {
			entries = append(entries, NameStatusEntry{Status: status, Old: tokens[i+1], New: tokens[i+2]})
		} else {
			entries = append(entries, NameStatusEntry{Status: status, New: tokens[i+1]})
		}
		i += want
	}
	return entries, nil
}

// StatusEntry is one entry of a `git status --porcelain -z` stream. Path is the
// entry's own path — on a rename or copy the *new* name, with Orig holding the
// original, which is the reverse of the name-status shape's old-then-new order.
type StatusEntry struct {
	// X is the index status code, Y the worktree one; "??" is an untracked file.
	X, Y byte
	// Path is the file's current name.
	Path string
	// Orig is the pre-image path on a rename or copy, empty otherwise.
	Orig string
}

// GitStatus runs `git status --porcelain` and decodes its NUL-framed entries.
func GitStatus(ctx context.Context, a GitArgs) ([]StatusEntry, error) {
	out, err := gitRun(ctx, a, "--porcelain")
	if err != nil {
		return nil, err
	}
	return parseStatus(out)
}

// parseStatus decodes `git status --porcelain -z`: each entry is "XY <path>",
// and an R or C in either column pulls the original path from the next token.
func parseStatus(out string) ([]StatusEntry, error) {
	tokens := nulTokens(out)
	var entries []StatusEntry
	for i := 0; i < len(tokens); i++ {
		tok := tokens[i]
		if len(tok) < 4 || tok[2] != ' ' {
			return nil, fmt.Errorf("status --porcelain: malformed entry %q", tok)
		}
		e := StatusEntry{X: tok[0], Y: tok[1], Path: tok[3:]}
		if e.X == 'R' || e.X == 'C' || e.Y == 'R' || e.Y == 'C' {
			i++
			if i >= len(tokens) {
				return nil, fmt.Errorf("status --porcelain: entry %q wants an origin path, stream ended", tok)
			}
			e.Orig = tokens[i]
		}
		entries = append(entries, e)
	}
	return entries, nil
}

// TreeRecord is one record of a NUL-framed object listing — `git ls-tree`,
// `git ls-files --stage` — whose single tab separates git's attribute field from
// the path. The listing and the numstat stream are separate shapes because their
// tab arity differs, and only a shape that knows its own arity can cut a record
// without eating a tab out of a filename.
type TreeRecord struct {
	// Attrs is git's space-joined attribute field verbatim: "<mode> <type>
	// <object>" for ls-tree, "<mode> <object> <stage>" for ls-files --stage.
	Attrs string
	Path  string
}

// GitTreeRecords runs an object listing and decodes its NUL-framed records.
func GitTreeRecords(ctx context.Context, a GitArgs) ([]TreeRecord, error) {
	out, err := gitRun(ctx, a)
	if err != nil {
		return nil, err
	}
	return parseTreeRecords(out)
}

// parseTreeRecords decodes an object listing, cutting each record at its first
// tab: the attribute field never carries one and a path may.
func parseTreeRecords(out string) ([]TreeRecord, error) {
	var records []TreeRecord
	for _, tok := range nulTokens(out) {
		attrs, path, ok := strings.Cut(tok, "\t")
		if !ok {
			return nil, fmt.Errorf("tree records: record %q has no path field", tok)
		}
		records = append(records, TreeRecord{Attrs: attrs, Path: path})
	}
	return records, nil
}

// NumstatRecord is one record of a `--numstat -z` stream: git's two line counts
// and the path they belong to. A rename leaves the record's path field empty and
// follows it with two more tokens, old then new; those land in Old and Path so no
// caller counts tokens.
type NumstatRecord struct {
	// Added and Deleted are git's counts as written — "-" for a binary file.
	Added, Deleted string
	// Path is the post-image path.
	Path string
	// Old is the pre-image path on a rename record, empty otherwise.
	Old string
}

// GitNumstat runs a `--numstat` diff and decodes its NUL-framed records.
//
// fields are --format placeholders for the commit the records belong to (git
// show --numstat --format=%P); they come back in header, ahead of the records
// and never mixed into them. A numstat describing no commit passes none and
// reads header as empty.
func GitNumstat(ctx context.Context, a GitArgs, fields ...string) (header []string, records []NumstatRecord, err error) {
	var extraSub []string
	if len(fields) > 0 {
		extraSub = []string{"--format=" + strings.Join(fields, "%x00")}
	}
	out, err := gitRun(ctx, a, append(extraSub, "--numstat")...)
	if err != nil {
		return nil, nil, err
	}
	return parseNumstat(out, len(fields))
}

// parseNumstat decodes a numstat stream behind nfields header tokens, stripping
// the newline git strands between a commit header and its first record. Each
// record is cut after its two count fields, so the path keeps any tab of its own.
func parseNumstat(out string, nfields int) (header []string, records []NumstatRecord, err error) {
	tokens := nulTokens(out)
	if nfields > len(tokens) {
		return nil, nil, fmt.Errorf("numstat: header wants %d fields, %d tokens left", nfields, len(tokens))
	}
	header = append([]string(nil), tokens[:nfields]...)
	tokens = tokens[nfields:]
	if nfields > 0 && len(tokens) > 0 {
		tokens[0] = strings.TrimPrefix(tokens[0], "\n")
	}

	for i := 0; i < len(tokens); i++ {
		fields := strings.SplitN(tokens[i], "\t", 3)
		if len(fields) < 3 {
			return nil, nil, fmt.Errorf("numstat: record %q wants two counts and a path", tokens[i])
		}
		rec := NumstatRecord{Added: fields[0], Deleted: fields[1], Path: fields[2]}
		if rec.Path == "" {
			if i+2 >= len(tokens) {
				return nil, nil, fmt.Errorf("numstat: rename record %q wants two path tokens, %d left", tokens[i], len(tokens)-i-1)
			}
			rec.Old, rec.Path = tokens[i+1], tokens[i+2]
			i += 2
		}
		records = append(records, rec)
	}
	return header, records, nil
}

// PorcelainRecord is one record of a `--porcelain -z` attribute stream, keyed by
// attribute name. A valueless attribute (worktree list's "bare", "detached") is
// present with an empty value, so presence is read with the two-result map index
// and never by comparing the value to "".
type PorcelainRecord map[string]string

// GitPorcelainRecords runs a `--porcelain -z` attribute enumeration and decodes
// its records, which an empty token separates.
func GitPorcelainRecords(ctx context.Context, a GitArgs) ([]PorcelainRecord, error) {
	out, err := gitRun(ctx, a, "--porcelain")
	if err != nil {
		return nil, err
	}
	return parsePorcelainRecords(out), nil
}

// parsePorcelainRecords decodes a `--porcelain -z` attribute stream, whose
// records an empty token separates.
func parsePorcelainRecords(out string) []PorcelainRecord {
	var records []PorcelainRecord
	rec := PorcelainRecord{}
	for _, tok := range nulTokens(out) {
		if tok == "" {
			if len(rec) > 0 {
				records = append(records, rec)
				rec = PorcelainRecord{}
			}
			continue
		}
		key, value, _ := strings.Cut(tok, " ")
		rec[key] = value
	}
	if len(rec) > 0 {
		records = append(records, rec)
	}
	return records
}

// logSentinel prefixes every `git log` header so a commit boundary is
// unambiguous: with -z the header's own terminator is a NUL like every other
// token's, leaving no way to tell the next commit's first field from the
// previous commit's next path.
const logSentinel = "\x01"

// LogCommit is one commit of a `git log --name-status -z` stream: the requested
// format placeholders' values, in the order asked for, and the commit's changed
// files. A merge or an empty commit carries no entries.
type LogCommit struct {
	// Fields holds one value per placeholder passed to GitLogNameStatus.
	Fields []string
	// Entries are the commit's changed files, decoded like GitNameStatus's.
	Entries []NameStatusEntry
}

// GitLogNameStatus runs `git log --name-status` over the given format
// placeholders (e.g. "%h", "%ad", "%s") and decodes the interleaved stream of
// commit headers and name-status records. It owns the framing entirely: the
// placeholders are joined with %x00 and prefixed with the sentinel that marks a
// header, and the stray newline git writes between a header and its first record
// is stripped.
func GitLogNameStatus(ctx context.Context, a GitArgs, fields ...string) ([]LogCommit, error) {
	if len(fields) == 0 {
		return nil, fmt.Errorf("log --name-status: no format placeholders")
	}
	format := "--format=" + logSentinel + strings.Join(fields, "%x00")
	out, err := gitRun(ctx, a, format, "--name-status")
	if err != nil {
		return nil, err
	}
	return parseLogNameStatus(out, len(fields))
}

// parseLogNameStatus decodes the interleaved header/record stream, splitting on
// the sentinel that opens each header and stripping the newline git strands
// ahead of a commit's first record.
func parseLogNameStatus(out string, nfields int) ([]LogCommit, error) {
	tokens := nulTokens(out)

	var commits []LogCommit
	for i := 0; i < len(tokens); {
		if !strings.HasPrefix(tokens[i], logSentinel) {
			return nil, fmt.Errorf("log --name-status: expected a commit header at token %d, got %q", i, tokens[i])
		}
		if i+nfields > len(tokens) {
			return nil, fmt.Errorf("log --name-status: header wants %d fields, %d tokens left", nfields, len(tokens)-i)
		}
		values := make([]string, nfields)
		copy(values, tokens[i:i+nfields])
		values[0] = strings.TrimPrefix(values[0], logSentinel)
		i += nfields

		start := i
		for i < len(tokens) && !strings.HasPrefix(tokens[i], logSentinel) {
			i++
		}
		body := append([]string(nil), tokens[start:i]...)
		if len(body) > 0 {
			body[0] = strings.TrimPrefix(body[0], "\n")
		}
		entries, err := parseNameStatus(body)
		if err != nil {
			return nil, err
		}
		commits = append(commits, LogCommit{Fields: values, Entries: entries})
	}
	return commits, nil
}
