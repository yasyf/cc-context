package vcs

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// Every stream literal below was captured from git 2.55 with `od -c`; the shape
// helpers exist because none of them can be guessed from the non-NUL forms.

func TestParseNameStatusMeasuredStreams(t *testing.T) {
	tests := []struct {
		name   string
		stream string
		want   []NameStatusEntry
	}{
		{
			// git diff --cached --name-status -M -z
			name:   "modify add delete rename",
			stream: "M\x00a.go\x00A\x00fresh.go\x00D\x00keep.go\x00R100\x00old.go\x00new.go\x00",
			want: []NameStatusEntry{
				{Status: "M", New: "a.go"},
				{Status: "A", New: "fresh.go"},
				{Status: "D", New: "keep.go"},
				{Status: "R100", Old: "old.go", New: "new.go"},
			},
		},
		{
			// A rename of a path carrying a newline: without -z git would have
			// quoted both names and the newline would have framed a phantom record.
			name:   "newline in both rename paths",
			stream: "R100\x00new\nline.go\x00moved\nline.go\x00",
			want:   []NameStatusEntry{{Status: "R100", Old: "new\nline.go", New: "moved\nline.go"}},
		},
		{
			name:   "copy takes three tokens like a rename",
			stream: "C75\x00src.go\x00dst.go\x00",
			want:   []NameStatusEntry{{Status: "C75", Old: "src.go", New: "dst.go"}},
		},
		{name: "empty stream", stream: "", want: nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseNameStatus(nulTokens(tt.stream))
			if err != nil {
				t.Fatalf("parseNameStatus: %v", err)
			}
			if !slices.Equal(got, tt.want) {
				t.Fatalf("entries = %+v, want %+v", got, tt.want)
			}
		})
	}
}

// TestParseStatusMeasuredStreams pins the shape S1 reverses: `git status
// --porcelain -z` puts the *new* name in the entry and the original in the token
// after it, where `git diff --name-status -z` puts old first.
func TestParseStatusMeasuredStreams(t *testing.T) {
	tests := []struct {
		name   string
		stream string
		want   []StatusEntry
	}{
		{
			// git status --porcelain -z
			name:   "staged set with a rename",
			stream: "M  a.go\x00A  fresh.go\x00D  keep.go\x00R  new.go\x00old.go\x00?? untracked.go\x00",
			want: []StatusEntry{
				{X: 'M', Y: ' ', Path: "a.go"},
				{X: 'A', Y: ' ', Path: "fresh.go"},
				{X: 'D', Y: ' ', Path: "keep.go"},
				{X: 'R', Y: ' ', Path: "new.go", Orig: "old.go"},
				{X: '?', Y: '?', Path: "untracked.go"},
			},
		},
		{
			name:   "rename with a worktree modification on top",
			stream: "RM new.go\x00old.go\x00",
			want:   []StatusEntry{{X: 'R', Y: 'M', Path: "new.go", Orig: "old.go"}},
		},
		{
			name:   "newline in both rename paths, new first",
			stream: "R  moved\nline.go\x00new\nline.go\x00",
			want:   []StatusEntry{{X: 'R', Y: ' ', Path: "moved\nline.go", Orig: "new\nline.go"}},
		},
		{name: "clean tree", stream: "", want: nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseStatus(tt.stream)
			if err != nil {
				t.Fatalf("parseStatus: %v", err)
			}
			if !slices.Equal(got, tt.want) {
				t.Fatalf("entries = %+v, want %+v", got, tt.want)
			}
		})
	}
}

// TestNameStatusAndStatusDisagreeOnRenameOrder is the whole reason the two shapes
// are separate helpers with named fields: fed the same rename, the two streams
// carry the paths in opposite order, so any caller that read "the token after the
// status" positionally would report the wrong direction in one of them.
func TestNameStatusAndStatusDisagreeOnRenameOrder(t *testing.T) {
	ns, err := parseNameStatus(nulTokens("R100\x00old.go\x00new.go\x00"))
	if err != nil {
		t.Fatalf("parseNameStatus: %v", err)
	}
	st, err := parseStatus("R  new.go\x00old.go\x00")
	if err != nil {
		t.Fatalf("parseStatus: %v", err)
	}
	if ns[0].Old != "old.go" || ns[0].New != "new.go" {
		t.Fatalf("name-status = %+v, want old.go → new.go", ns[0])
	}
	if st[0].Orig != "old.go" || st[0].Path != "new.go" {
		t.Fatalf("status = %+v, want orig old.go / path new.go", st[0])
	}
	nsSecond, stSecond := nulTokens("R100\x00old.go\x00new.go\x00")[2], nulTokens("R  new.go\x00old.go\x00")[1]
	if nsSecond == stSecond {
		t.Fatalf("the two streams' second path token agree (%q) — the fixture no longer covers the reversal", nsSecond)
	}
}

func TestParseTreeRecordsMeasuredStreams(t *testing.T) {
	tests := []struct {
		name   string
		stream string
		want   []TreeRecord
	}{
		{
			// git ls-tree --full-tree -z HEAD
			name:   "ls-tree entries",
			stream: "100644 blob 1041aa30ece4fd652c746ec13714bf37c321f8c3\ta.go\x00040000 tree 423a1d93a71ebaaf980193d6763d995df4361f11\tsub\x00",
			want: []TreeRecord{
				{Attrs: "100644 blob 1041aa30ece4fd652c746ec13714bf37c321f8c3", Path: "a.go"},
				{Attrs: "040000 tree 423a1d93a71ebaaf980193d6763d995df4361f11", Path: "sub"},
			},
		},
		{
			// git ls-files --stage -z -- a.go
			name:   "ls-files stage entry",
			stream: "100644 b0bb22b1264e929a0aaf6d46115471d3ff18a10e 0\ta.go\x00",
			want:   []TreeRecord{{Attrs: "100644 b0bb22b1264e929a0aaf6d46115471d3ff18a10e 0", Path: "a.go"}},
		},
		{
			// A tab is legal in a filename and git writes it raw under -z, so the
			// cut is at the *first* tab, never the last.
			name:   "ls-tree entry whose path carries a tab",
			stream: "100644 blob 8c7e5a667f1b771847fe88c01c3de34413a1b220\ta\tb.go\x00",
			want:   []TreeRecord{{Attrs: "100644 blob 8c7e5a667f1b771847fe88c01c3de34413a1b220", Path: "a\tb.go"}},
		},
		{name: "empty listing", stream: "", want: nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseTreeRecords(tt.stream)
			if err != nil {
				t.Fatalf("parseTreeRecords: %v", err)
			}
			if !slices.Equal(got, tt.want) {
				t.Fatalf("records = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestParseNumstatMeasuredStreams(t *testing.T) {
	tests := []struct {
		name       string
		stream     string
		nfields    int
		wantHeader []string
		want       []NumstatRecord
	}{
		{
			// git show --numstat --format=%P -z HEAD, on a commit with a rename:
			// the rename record's path field is *empty* and two more tokens follow.
			name:       "numstat with a rename",
			stream:     "afa22ce90d6ce7b1b0c8b7fe76303aefb8bc6f8b\x00\n1\t1\ta.go\x001\t0\tfresh.go\x000\t1\tkeep.go\x000\t0\t\x00old.go\x00new.go\x00",
			nfields:    1,
			wantHeader: []string{"afa22ce90d6ce7b1b0c8b7fe76303aefb8bc6f8b"},
			want: []NumstatRecord{
				{Added: "1", Deleted: "1", Path: "a.go"},
				{Added: "1", Deleted: "0", Path: "fresh.go"},
				{Added: "0", Deleted: "1", Path: "keep.go"},
				{Added: "0", Deleted: "0", Old: "old.go", Path: "new.go"},
			},
		},
		{
			// The same on a root commit: %P is empty, so the header token is empty
			// too — the stranded newline still separates it from the first record.
			name:       "numstat on a root commit",
			stream:     "\x00\n2\t0\ta.go\x001\t0\tsub/[id].go\x00",
			nfields:    1,
			wantHeader: []string{""},
			want: []NumstatRecord{
				{Added: "2", Deleted: "0", Path: "a.go"},
				{Added: "1", Deleted: "0", Path: "sub/[id].go"},
			},
		},
		{
			name:       "numstat rename of a newline path",
			stream:     "e5a68f135efa2e20fefae1e08e10e61af0c344d3\x00\n0\t0\t\x00new\nline.go\x00moved\nline.go\x00",
			nfields:    1,
			wantHeader: []string{"e5a68f135efa2e20fefae1e08e10e61af0c344d3"},
			want:       []NumstatRecord{{Added: "0", Deleted: "0", Old: "new\nline.go", Path: "moved\nline.go"}},
		},
		{
			name:       "binary numstat reports dashes, not zeroes",
			stream:     "33324 0eee7c3db5b8b599890ca72bbfb4440ce57\x00\n-\t-\tbin.dat\x00",
			nfields:    1,
			wantHeader: []string{"33324 0eee7c3db5b8b599890ca72bbfb4440ce57"},
			want:       []NumstatRecord{{Added: "-", Deleted: "-", Path: "bin.dat"}},
		},
		{
			// The counts are two fields, so a path carrying a tab keeps it.
			name:       "numstat record whose path carries a tab",
			stream:     "1ee62c761a456cdd7269e5cf1ea88874\x00\n1\t1\ta\tb.go\x00",
			nfields:    1,
			wantHeader: []string{"1ee62c761a456cdd7269e5cf1ea88874"},
			want:       []NumstatRecord{{Added: "1", Deleted: "1", Path: "a\tb.go"}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			header, got, err := parseNumstat(tt.stream, tt.nfields)
			if err != nil {
				t.Fatalf("parseNumstat: %v", err)
			}
			if !slices.Equal(header, tt.wantHeader) {
				t.Fatalf("header = %q, want %q", header, tt.wantHeader)
			}
			if !slices.Equal(got, tt.want) {
				t.Fatalf("records = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestParsePorcelainRecordsMeasuredStreams(t *testing.T) {
	// git worktree list --porcelain -z, main checkout plus a detached linked one.
	const stream = "worktree /private/tmp/ccx-shapes\x00HEAD d7c9da550c8ec71cd4482cb21640d5b3ba0a0d39\x00branch refs/heads/main\x00\x00" +
		"worktree /private/tmp/ccx-shapes-wt2\x00HEAD d7c9da550c8ec71cd4482cb21640d5b3ba0a0d39\x00detached\x00\x00"
	got := parsePorcelainRecords(stream)
	if len(got) != 2 {
		t.Fatalf("records = %v, want 2", got)
	}
	if got[0]["worktree"] != "/private/tmp/ccx-shapes" || got[0]["branch"] != "refs/heads/main" {
		t.Fatalf("record 0 = %v", got[0])
	}
	if _, ok := got[0]["detached"]; ok {
		t.Fatalf("record 0 = %v, want no detached attribute", got[0])
	}
	// A valueless attribute is present with an empty value, so presence must be
	// read with the two-result index and never by comparing to "".
	value, ok := got[1]["detached"]
	if !ok || value != "" {
		t.Fatalf("record 1 detached = (%q, %v), want (\"\", true)", value, ok)
	}
	if _, ok := got[1]["branch"]; ok {
		t.Fatalf("record 1 = %v, want no branch attribute on a detached worktree", got[1])
	}
}

func TestParseLogNameStatusMeasuredStreams(t *testing.T) {
	tests := []struct {
		name   string
		stream string
		want   []LogCommit
	}{
		{
			// git log --follow --format=%x01%h%x00%ad%x00%s --date=short
			//   --name-status -z -- new.go
			name:   "follow across a rename",
			stream: "\x01d348d2c\x002026-08-02\x00second: rename+modify\x00\nR100\x00old.go\x00new.go\x00\x01afa22ce\x002026-08-02\x00init\x00\nA\x00old.go\x00",
			want: []LogCommit{
				{
					Fields:  []string{"d348d2c", "2026-08-02", "second: rename+modify"},
					Entries: []NameStatusEntry{{Status: "R100", Old: "old.go", New: "new.go"}},
				},
				{
					Fields:  []string{"afa22ce", "2026-08-02", "init"},
					Entries: []NameStatusEntry{{Status: "A", New: "old.go"}},
				},
			},
		},
		{
			// An empty commit carries no records at all — and, measured, no
			// stranded newline either: its header NUL abuts the next sentinel.
			name:   "empty commit between two ordinary ones",
			stream: "\x0105ea5cd\x002026-08-02\x00empty one\x00\x01d348d2c\x002026-08-02\x00second\x00\nM\x00a.go\x00R100\x00old.go\x00new.go\x00",
			want: []LogCommit{
				{Fields: []string{"05ea5cd", "2026-08-02", "empty one"}},
				{
					Fields: []string{"d348d2c", "2026-08-02", "second"},
					Entries: []NameStatusEntry{
						{Status: "M", New: "a.go"},
						{Status: "R100", Old: "old.go", New: "new.go"},
					},
				},
			},
		},
		{
			// A subject that itself ends in a path-like token: the sentinel, not the
			// token's shape, is what says where a commit starts.
			name:   "subject that looks like a status token",
			stream: "\x01abc1234\x002026-08-02\x00M\x00\nA\x00a.go\x00",
			want: []LogCommit{{
				Fields:  []string{"abc1234", "2026-08-02", "M"},
				Entries: []NameStatusEntry{{Status: "A", New: "a.go"}},
			}},
		},
		{name: "no commits", stream: "", want: nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseLogNameStatus(tt.stream, 3)
			if err != nil {
				t.Fatalf("parseLogNameStatus: %v", err)
			}
			if len(got) != len(tt.want) {
				t.Fatalf("commits = %+v, want %+v", got, tt.want)
			}
			for i, c := range got {
				if !slices.Equal(c.Fields, tt.want[i].Fields) || !slices.Equal(c.Entries, tt.want[i].Entries) {
					t.Fatalf("commit %d = %+v, want %+v", i, c, tt.want[i])
				}
			}
		})
	}
}

func TestParseRejectsMalformedStreams(t *testing.T) {
	if _, err := parseNameStatus(nulTokens("R100\x00old.go\x00")); err == nil {
		t.Fatal("truncated rename accepted")
	}
	if _, err := parseStatus("M\x00"); err == nil {
		t.Fatal("short status entry accepted")
	}
	if _, err := parseStatus("R  new.go\x00"); err == nil {
		t.Fatal("rename missing its origin path accepted")
	}
	if _, err := parseTreeRecords("no-tab-here\x00"); err == nil {
		t.Fatal("tree record with no path field accepted")
	}
	if _, _, err := parseNumstat("1\ta.go\x00", 0); err == nil {
		t.Fatal("numstat record missing a count field accepted")
	}
	if _, _, err := parseNumstat("0\t0\t\x00only-one\x00", 0); err == nil {
		t.Fatal("truncated numstat rename accepted")
	}
	if _, err := parseLogNameStatus("nosentinel\x00a\x00b\x00", 3); err == nil {
		t.Fatal("stream with no commit sentinel accepted")
	}
}

// TestGitArgvInterposesSeparators pins the property the type exists for: no
// caller can hand git a flat argv, so --end-of-options always precedes the revs
// and -- always precedes the pathspecs.
func TestGitArgvInterposesSeparators(t *testing.T) {
	tests := []struct {
		name     string
		args     GitArgs
		extraSub []string
		want     []string
	}{
		{
			name: "revs and paths",
			args: GitArgs{
				Sub:   []string{"diff", "--cached", "-M"},
				Revs:  []GitRef{HeadRef},
				Paths: []string{"a.go"},
			},
			extraSub: []string{"--name-status"},
			want:     []string{"diff", "--cached", "-M", "--name-status", "-z", "--end-of-options", "HEAD", "--", "a.go"},
		},
		{
			// git worktree list takes no pathspec and rejects a bare -- with exit
			// 129, so the separator appears only when there is something to separate.
			name:     "no paths means no --",
			args:     GitArgs{GitDir: "/repo/.git", Sub: []string{"worktree", "list"}},
			extraSub: []string{"--porcelain"},
			want:     []string{"--git-dir", "/repo/.git", "worktree", "list", "--porcelain", "-z", "--end-of-options"},
		},
		{
			name: "an option-shaped rev lands after --end-of-options",
			args: GitArgs{Sub: []string{"diff"}, Revs: []GitRef{UnsafeRef("--output=/tmp/pwned")}},
			want: []string{"diff", "-z", "--end-of-options", "--output=/tmp/pwned"},
		},
		{
			name: "an option-shaped path lands after --",
			args: GitArgs{Sub: []string{"diff"}, Paths: []string{"--output=/tmp/pwned"}},
			want: []string{"diff", "-z", "--end-of-options", "--", "--output=/tmp/pwned"},
		},
		{
			name: "several qualified revs keep their order",
			args: GitArgs{Sub: []string{"diff"}, Revs: []GitRef{RemoteBranchRef("origin", "main"), LocalBranchRef("feat")}},
			want: []string{"diff", "-z", "--end-of-options", "refs/remotes/origin/main", "refs/heads/feat"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := gitArgv(tt.args, tt.extraSub...)
			if !slices.Equal(got, tt.want) {
				t.Fatalf("argv = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestGitRunRefusesOptionInjectionViaRev exercises the argv against real git: a
// diff source spelled --output=<path> wrote that file before --end-of-options was
// interposed.
func TestGitRunRefusesOptionInjectionViaRev(t *testing.T) {
	dir := gitRepo(t)
	write(t, dir, "a.go", "package a\n")
	runGit(t, dir, "add", "-A")
	runGit(t, dir, "commit", "-qm", "init")

	target := filepath.Join(t.TempDir(), "pwned.txt")
	_, err := GitNameStatus(context.Background(), GitArgs{
		Dir:  dir,
		Sub:  []string{"diff"},
		Revs: []GitRef{UnsafeRef("--output=" + target)},
	})
	if err == nil {
		t.Fatal("git accepted an option-shaped rev")
	}
	if !strings.Contains(err.Error(), "must come before non-option arguments") {
		t.Fatalf("err = %v, want git's post-option refusal", err)
	}
	if _, statErr := os.Stat(target); statErr == nil {
		t.Fatalf("%s was written — the rev reached git's option parser", target)
	}
}

// TestGitRunMatchesPathspecsLiterally exercises GIT_LITERAL_PATHSPECS against
// real git: "sub/[id].go" is a character class to git's default pathspec parser,
// so without it the file's own name also selects its neighbors.
func TestGitRunMatchesPathspecsLiterally(t *testing.T) {
	dir := gitRepo(t)
	write(t, dir, "sub/[id].go", "package a\n")
	write(t, dir, "sub/i.go", "package a\n")
	runGit(t, dir, "add", "-A")
	runGit(t, dir, "commit", "-qm", "init")

	paths, err := GitPaths(context.Background(), GitArgs{
		Dir:   dir,
		Sub:   []string{"ls-files"},
		Paths: []string{"sub/[id].go"},
	})
	if err != nil {
		t.Fatalf("GitPaths: %v", err)
	}
	if !slices.Equal(paths, []string{"sub/[id].go"}) {
		t.Fatalf("paths = %q, want only sub/[id].go", paths)
	}

	// And the magic prefix is inert too: it names a file that does not exist.
	magic, err := GitPaths(context.Background(), GitArgs{
		Dir:   dir,
		Sub:   []string{"ls-files"},
		Paths: []string{":(exclude)sub/i.go"},
	})
	if err != nil {
		t.Fatalf("GitPaths: %v", err)
	}
	if len(magic) != 0 {
		t.Fatalf("paths = %q, want none — :(exclude) was honored as pathspec magic", magic)
	}
}

// TestGitRunOmitsThePathspecSeparatorWhenThereAreNoPaths pins why -- is
// conditional. Measured against git 2.55: after --end-of-options, a bare -- reads
// as an empty pathspec set that matches nothing, so `git status --porcelain -z
// --end-of-options --` reports a clean tree at exit 0 over a dirty one, and `git
// worktree list`, which takes no pathspec at all, rejects it with exit 129. Both
// failures are invisible to an argv assertion alone.
func TestGitRunOmitsThePathspecSeparatorWhenThereAreNoPaths(t *testing.T) {
	ctx := context.Background()
	dir := gitRepo(t)
	write(t, dir, "a.go", "package a\n")
	runGit(t, dir, "add", "-A")
	runGit(t, dir, "commit", "-qm", "init")
	write(t, dir, "a.go", "package a\nfunc Foo() {}\n")

	entries, err := GitStatus(ctx, GitArgs{Dir: dir, Sub: []string{"status"}})
	if err != nil {
		t.Fatalf("GitStatus: %v", err)
	}
	if len(entries) != 1 || entries[0].Path != "a.go" {
		t.Fatalf("entries = %+v, want the dirty a.go", entries)
	}

	if _, err := GitPorcelainRecords(ctx, GitArgs{Dir: dir, Sub: []string{"worktree", "list"}}); err != nil {
		t.Fatalf("GitPorcelainRecords: %v", err)
	}
}

// TestShapeHelpersAgainstLiveGit runs every helper against a real repository, so
// a fake that emitted newline-terminated or reordered tokens could not green the
// parsers on its own.
func TestShapeHelpersAgainstLiveGit(t *testing.T) {
	ctx := context.Background()
	dir := gitRepo(t)
	write(t, dir, "a.go", "package a\n")
	write(t, dir, "keep.go", "keep\n")
	write(t, dir, "old.go", "oldname\n")
	runGit(t, dir, "add", "-A")
	runGit(t, dir, "commit", "-qm", "init")

	write(t, dir, "a.go", "package a\nfunc Foo() {}\n")
	if err := os.Remove(filepath.Join(dir, "keep.go")); err != nil {
		t.Fatal(err)
	}
	runGit(t, dir, "mv", "old.go", "new.go")
	write(t, dir, "fresh.go", "fresh\n")
	runGit(t, dir, "add", "-A")

	t.Run("GitNameStatus", func(t *testing.T) {
		entries, err := GitNameStatus(ctx, GitArgs{Dir: dir, Sub: []string{"diff", "--cached", "-M"}})
		if err != nil {
			t.Fatalf("GitNameStatus: %v", err)
		}
		want := []NameStatusEntry{
			{Status: "M", New: "a.go"},
			{Status: "A", New: "fresh.go"},
			{Status: "D", New: "keep.go"},
			{Status: "R100", Old: "old.go", New: "new.go"},
		}
		if !slices.Equal(entries, want) {
			t.Fatalf("entries = %+v, want %+v", entries, want)
		}
	})

	t.Run("GitStatus", func(t *testing.T) {
		entries, err := GitStatus(ctx, GitArgs{Dir: dir, Sub: []string{"status"}})
		if err != nil {
			t.Fatalf("GitStatus: %v", err)
		}
		var rename StatusEntry
		for _, e := range entries {
			if e.X == 'R' {
				rename = e
			}
		}
		if rename.Path != "new.go" || rename.Orig != "old.go" {
			t.Fatalf("rename = %+v, want path new.go / orig old.go", rename)
		}
		if len(entries) != 4 {
			t.Fatalf("entries = %+v, want 4", entries)
		}
	})

	t.Run("GitPaths", func(t *testing.T) {
		paths, err := GitPaths(ctx, GitArgs{Dir: dir, Sub: []string{"diff", "--cached", "--name-only"}})
		if err != nil {
			t.Fatalf("GitPaths: %v", err)
		}
		// git detects the rename on its own here, so old.go never appears.
		if !slices.Equal(paths, []string{"a.go", "fresh.go", "keep.go", "new.go"}) {
			t.Fatalf("paths = %q", paths)
		}
	})

	runGit(t, dir, "commit", "-qm", "second")

	t.Run("GitTreeRecords ls-tree", func(t *testing.T) {
		records, err := GitTreeRecords(ctx, GitArgs{
			Dir:   dir,
			Sub:   []string{"ls-tree", "--full-tree"},
			Revs:  []GitRef{HeadRef},
			Paths: []string{"a.go"},
		})
		if err != nil {
			t.Fatalf("GitTreeRecords: %v", err)
		}
		if len(records) != 1 || records[0].Path != "a.go" || !strings.HasPrefix(records[0].Attrs, "100644 blob ") {
			t.Fatalf("records = %+v", records)
		}
	})

	t.Run("GitNumstat rename", func(t *testing.T) {
		header, records, err := GitNumstat(ctx, GitArgs{
			Dir:  dir,
			Sub:  []string{"show", "-M"},
			Revs: []GitRef{HeadRef},
		}, "%P")
		if err != nil {
			t.Fatalf("GitNumstat: %v", err)
		}
		if len(header) != 1 || len(strings.Fields(header[0])) != 1 {
			t.Fatalf("header = %q, want one parent hash", header)
		}
		var rename NumstatRecord
		for _, rec := range records {
			if rec.Old != "" {
				rename = rec
			}
		}
		if rename.Old != "old.go" || rename.Path != "new.go" {
			t.Fatalf("rename record = %+v, want old.go → new.go", rename)
		}
	})

	t.Run("GitNumstat root commit", func(t *testing.T) {
		header, records, err := GitNumstat(ctx, GitArgs{
			Dir:  dir,
			Sub:  []string{"show"},
			Revs: []GitRef{UnsafeRef("HEAD~1")},
		}, "%P")
		if err != nil {
			t.Fatalf("GitNumstat: %v", err)
		}
		if len(header) != 1 || header[0] != "" {
			t.Fatalf("header = %q, want one empty parent field", header)
		}
		if len(records) != 3 {
			t.Fatalf("records = %+v, want 3", records)
		}
	})

	t.Run("GitPorcelainRecords", func(t *testing.T) {
		records, err := GitPorcelainRecords(ctx, GitArgs{
			Dir: dir,
			Sub: []string{"worktree", "list"},
		})
		if err != nil {
			t.Fatalf("GitPorcelainRecords: %v", err)
		}
		if len(records) != 1 {
			t.Fatalf("records = %v, want 1", records)
		}
		if records[0]["branch"] == "" || records[0]["worktree"] == "" {
			t.Fatalf("record = %v", records[0])
		}
	})

	t.Run("GitLogNameStatus", func(t *testing.T) {
		commits, err := GitLogNameStatus(ctx, GitArgs{
			Dir:   dir,
			Sub:   []string{"log", "--follow", "--date=short"},
			Paths: []string{"new.go"},
		}, "%h", "%ad", "%s")
		if err != nil {
			t.Fatalf("GitLogNameStatus: %v", err)
		}
		if len(commits) != 2 {
			t.Fatalf("commits = %+v, want 2", commits)
		}
		if got := commits[0].Fields[2]; got != "second" {
			t.Fatalf("subject = %q, want second", got)
		}
		want := []NameStatusEntry{{Status: "R100", Old: "old.go", New: "new.go"}}
		if !slices.Equal(commits[0].Entries, want) {
			t.Fatalf("entries = %+v, want %+v", commits[0].Entries, want)
		}
		if got := commits[1].Fields[2]; got != "init" {
			t.Fatalf("subject = %q, want init", got)
		}
	})

	t.Run("GitLogNameStatus skips an empty commit's records", func(t *testing.T) {
		runGit(t, dir, "commit", "-q", "--allow-empty", "-m", "empty one")
		commits, err := GitLogNameStatus(ctx, GitArgs{
			Dir: dir,
			Sub: []string{"log", "--date=short"},
		}, "%h", "%ad", "%s")
		if err != nil {
			t.Fatalf("GitLogNameStatus: %v", err)
		}
		if len(commits) != 3 {
			t.Fatalf("commits = %+v, want 3", commits)
		}
		if commits[0].Fields[2] != "empty one" || len(commits[0].Entries) != 0 {
			t.Fatalf("empty commit = %+v", commits[0])
		}
		if len(commits[1].Entries) != 4 {
			t.Fatalf("second commit entries = %+v, want 4", commits[1].Entries)
		}
	})
}

// TestGitPathsKeepsUnquotableNames is the R4 shape: without -z git renders a
// zero-width joiner, a newline, and a quote as C escapes inside a quoted string,
// so a caller splitting on newlines sees a leading '"' glued to the name.
func TestGitPathsKeepsUnquotableNames(t *testing.T) {
	dir := gitRepo(t)
	names := []string{"zwj\u200djoin.go", "new\nline.go", "quote\"name.go"}
	for _, name := range names {
		write(t, dir, name, "package a\n")
	}
	runGit(t, dir, "add", "-A")
	runGit(t, dir, "commit", "-qm", "init")

	paths, err := GitPaths(context.Background(), GitArgs{Dir: dir, Sub: []string{"ls-files"}})
	if err != nil {
		t.Fatalf("GitPaths: %v", err)
	}
	want := []string{"new\nline.go", "quote\"name.go", "zwj\u200djoin.go"}
	if !slices.Equal(paths, want) {
		t.Fatalf("paths = %q, want %q", paths, want)
	}
}

// TestTabInAFilenameStaysInThePath is why the object listing and the numstat
// stream are separate helpers: a tab is legal in a filename, git writes it raw
// under -z, and the two shapes put a different number of tab-separated fields
// ahead of the path. A parser that split on every tab and took the last field
// would cut "a\tb.go" down to "b.go" in both.
func TestTabInAFilenameStaysInThePath(t *testing.T) {
	ctx := context.Background()
	dir := gitRepo(t)
	const tabbed = "a\tb.go"
	write(t, dir, tabbed, "package a\n")
	runGit(t, dir, "add", "-A")
	runGit(t, dir, "commit", "-qm", "init")

	tree, err := GitTreeRecords(ctx, GitArgs{
		Dir:  dir,
		Sub:  []string{"ls-tree", "--full-tree"},
		Revs: []GitRef{HeadRef},
	})
	if err != nil {
		t.Fatalf("GitTreeRecords: %v", err)
	}
	if len(tree) != 1 || tree[0].Path != tabbed {
		t.Fatalf("tree records = %+v, want one record at %q", tree, tabbed)
	}
	if !strings.HasPrefix(tree[0].Attrs, "100644 blob ") {
		t.Fatalf("attrs = %q, want git's mode/type/object field", tree[0].Attrs)
	}

	write(t, dir, tabbed, "package a\nfunc Foo() {}\n")
	runGit(t, dir, "commit", "-qam", "second")

	_, stat, err := GitNumstat(ctx, GitArgs{Dir: dir, Sub: []string{"show"}, Revs: []GitRef{HeadRef}}, "%P")
	if err != nil {
		t.Fatalf("GitNumstat: %v", err)
	}
	want := []NumstatRecord{{Added: "1", Deleted: "0", Path: tabbed}}
	if !slices.Equal(stat, want) {
		t.Fatalf("numstat records = %+v, want %+v", stat, want)
	}
}

// TestGitRunRefusesThePathspecSeparatorAsARev pins the one token a rev may never
// be. Measured against git 2.55: `git diff -M --name-status -z --end-of-options
// --` over a clean tree exits 0 with no output — indistinguishable from a real
// empty diff — so `ccx vcs diff -- --` reported "0 files" for a revision that
// does not exist. UnsafeRef is what makes it reachable, so the runner is where it
// stops.
func TestGitRunRefusesThePathspecSeparatorAsARev(t *testing.T) {
	dir := gitRepo(t)
	write(t, dir, "a.go", "package a\n")
	runGit(t, dir, "add", "-A")
	runGit(t, dir, "commit", "-qm", "init")

	_, err := GitNameStatus(context.Background(), GitArgs{
		Dir:  dir,
		Sub:  []string{"diff", "-M"},
		Revs: []GitRef{UnsafeRef("--")},
	})
	if err == nil {
		t.Fatalf("GitNameStatus accepted %q as a revision", "--")
	}
	if !strings.Contains(err.Error(), "pathspec separator") {
		t.Fatalf("error = %v, want it to name the pathspec separator", err)
	}
}
