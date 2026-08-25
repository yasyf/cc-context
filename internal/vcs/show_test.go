package vcs

import (
	"context"
	"errors"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yasyf/cc-context/internal/render"
	"github.com/yasyf/cc-context/internal/vcstest"
)

const (
	fakeFullID   = "1111111111111111111111111111111111111111"
	fakeParentID = "2222222222222222222222222222222222222222"
)

// exoticSubject carries a double quote, an em dash, and a zero-width joiner —
// the bytes a line-oriented header record would mangle and the reason the shared
// record is NUL-framed.
const exoticSubject = "He said \"don't\" — a\u200db"

func TestParseCommit(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		want    Commit
		wantErr bool
	}{
		{
			name: "subject and body",
			raw:  "abc1234\x00Ada Lovelace\x00ada@example.com\x002026-07-02\x00" + fakeFullID + "\x00" + fakeParentID + "\x00Add the widget\n\nExplain the widget.\n",
			want: Commit{
				ShortID: "abc1234",
				Author:  "Ada Lovelace",
				Email:   "ada@example.com",
				Date:    "2026-07-02",
				Subject: "Add the widget",
				Body:    "Explain the widget.",
				Range:   fakeParentID + ".." + fakeFullID,
			},
		},
		{
			name: "subject only",
			raw:  "abc1234\x00Ada\x00a@e.com\x002026-07-02\x00" + fakeFullID + "\x00" + fakeParentID + "\x00Just a subject\n",
			want: Commit{
				ShortID: "abc1234",
				Author:  "Ada",
				Email:   "a@e.com",
				Date:    "2026-07-02",
				Subject: "Just a subject",
				Body:    "",
				Range:   fakeParentID + ".." + fakeFullID,
			},
		},
		{
			name: "merge commit takes first parent",
			raw:  "abc1234\x00Ada\x00a@e.com\x002026-07-02\x00" + fakeFullID + "\x00" + fakeParentID + " 3333333333333333333333333333333333333333\x00Merge\n",
			want: Commit{
				ShortID: "abc1234",
				Author:  "Ada",
				Email:   "a@e.com",
				Date:    "2026-07-02",
				Subject: "Merge",
				Body:    "",
				Range:   fakeParentID + ".." + fakeFullID,
			},
		},
		{
			name:    "root commit has no parent",
			raw:     "abc1234\x00Ada\x00a@e.com\x002026-07-02\x00" + fakeFullID + "\x00\x00Subject\n",
			wantErr: true,
		},
		{
			name:    "too few fields",
			raw:     "abc1234\x00Ada\x00only three",
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseCommit(tt.raw)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("parseCommit(%q) error = nil, want error", tt.raw)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseCommit(%q) error = %v", tt.raw, err)
			}
			if got != tt.want {
				t.Errorf("parseCommit() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

// TestJJShowRevset drives the show-ref translation with an injected resolver: git
// symbolic refs resolve to commit ids, jj-native revsets short-circuit before the
// resolver, and an unresolvable ref passes through for jj to interpret.
func TestJJShowRevset(t *testing.T) {
	const dir = "/repo"
	const headSHA = "1111111111111111111111111111111111111111"
	const tagSHA = "2222222222222222222222222222222222222222"
	const relSHA = "3333333333333333333333333333333333333333"
	resolve := func(_ context.Context, _ render.Dir, ref string) (string, bool) {
		switch ref {
		case "HEAD", "HEAD~1", "HEAD^", "main", "deadbeef":
			return headSHA, true
		case "v1.0":
			return tagSHA, true
		case "release@1":
			return relSHA, true
		case "@", "@-", "@+":
			t.Fatalf("jj working-copy marker %q must not reach git rev-parse", ref)
		}
		return "", false
	}
	tests := []struct {
		id   string
		ref  string
		want string
	}{
		{"HEAD resolves to commit id", "HEAD", headSHA},
		{"HEAD~N resolves to commit id", "HEAD~1", headSHA},
		{"HEAD^ resolves to commit id", "HEAD^", headSHA},
		{"branch resolves to commit id", "main", headSHA},
		{"tag peels to commit id", "v1.0", tagSHA},
		{"sha resolves to commit id", "deadbeef", headSHA},
		{"bare @ passes through", "@", "@"},
		{"@- passes through", "@-", "@-"},
		{"@+ passes through", "@+", "@+"},
		{"@-- chain tries git then passes through", "@--", "@--"},
		{"embedded-@ git ref resolves to commit id", "release@1", relSHA},
		{"bookmark@remote falls through to jj", "main@origin", "main@origin"},
		{"dag revset passes through", "::@", "::@"},
		{"union revset passes through", "main | feat", "main | feat"},
		{"negation revset passes through", "~x", "~x"},
		{"unresolvable change id passes through", "zovstqty", "zovstqty"},
	}
	for _, tt := range tests {
		t.Run(tt.id, func(t *testing.T) {
			if got := jjShowRevset(context.Background(), dir, tt.ref, resolve); got != tt.want {
				t.Errorf("jjShowRevset(%q) = %q, want %q", tt.ref, got, tt.want)
			}
		})
	}
}

// TestShowGit drives Show against a real git repository whose history holds
// every header shape the parser has to survive: a subject with a body, a subject
// carrying bytes a line-oriented record would mangle, a merge whose range must
// name the first parent, and the parentless root commit that has no range at all.
func TestShowGit(t *testing.T) {
	f := vcstest.Repo(t)
	dir := f.Dir

	write(t, dir, "f.txt", "widget\n")
	runGit(t, dir, "add", "f.txt")
	runGit(t, dir, "commit", "-qm", "Add the widget\n\nExplain the widget.")
	widget := gitOutput(t, dir, "rev-parse", "HEAD")
	root := gitOutput(t, dir, "rev-parse", "HEAD~1")

	runGit(t, dir, "switch", "-qc", "exotic")
	write(t, dir, "f.txt", "exotic\n")
	runGit(t, dir, "add", "f.txt")
	runGit(t, dir, "commit", "-qm", exoticSubject)
	exotic := gitOutput(t, dir, "rev-parse", "HEAD")

	runGit(t, dir, "switch", "-qc", "side", root)
	write(t, dir, "s.txt", "side\n")
	runGit(t, dir, "add", "s.txt")
	runGit(t, dir, "commit", "-qm", "side")
	side := gitOutput(t, dir, "rev-parse", "HEAD")

	runGit(t, dir, "switch", "-qc", "merged", widget)
	runGit(t, dir, "merge", "-q", "--no-ff", "-m", "Merge side", side)
	merge := gitOutput(t, dir, "rev-parse", "HEAD")
	runGit(t, dir, "switch", "-q", "main")

	tests := []struct {
		id      string
		ref     string
		want    Commit
		wantErr bool
	}{
		{
			id:  "empty ref defaults to HEAD",
			ref: "",
			want: Commit{
				ShortID: gitField(t, dir, widget, "%h"),
				Author:  "t",
				Email:   "t@t.t",
				Date:    gitField(t, dir, widget, "%ad"),
				Subject: "Add the widget",
				Body:    "Explain the widget.",
				Range:   root + ".." + widget,
			},
		},
		{
			id:  "sha resolves",
			ref: widget,
			want: Commit{
				ShortID: gitField(t, dir, widget, "%h"),
				Author:  "t",
				Email:   "t@t.t",
				Date:    gitField(t, dir, widget, "%ad"),
				Subject: "Add the widget",
				Body:    "Explain the widget.",
				Range:   root + ".." + widget,
			},
		},
		{
			id:  "branch name resolves",
			ref: "exotic",
			want: Commit{
				ShortID: gitField(t, dir, exotic, "%h"),
				Author:  "t",
				Email:   "t@t.t",
				Date:    gitField(t, dir, exotic, "%ad"),
				Subject: exoticSubject,
				Range:   widget + ".." + exotic,
			},
		},
		{
			id:  "merge ranges against its first parent",
			ref: merge,
			want: Commit{
				ShortID: gitField(t, dir, merge, "%h"),
				Author:  "t",
				Email:   "t@t.t",
				Date:    gitField(t, dir, merge, "%ad"),
				Subject: "Merge side",
				Range:   widget + ".." + merge,
			},
		},
		{id: "root commit has no range to diff", ref: root, wantErr: true},
		{id: "unknown revision errors", ref: "no-such-ref", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.id, func(t *testing.T) {
			got, err := Show(context.Background(), render.Dir(dir), tt.ref)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("Show(%q) = %+v, want error", tt.ref, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("Show(%q) error = %v", tt.ref, err)
			}
			if got != tt.want {
				t.Errorf("Show(%q) =\n %+v\nwant\n %+v", tt.ref, got, tt.want)
			}
		})
	}
}

// TestShowGitFlagShapedRef proves --end-of-options keeps a flag-shaped ref a
// revision rather than a git option — --output=<path> is a file-clobber vector.
func TestShowGitFlagShapedRef(t *testing.T) {
	f := vcstest.Repo(t)
	pwned := filepath.Join(t.TempDir(), "pwned")

	got, err := Show(context.Background(), render.Dir(f.Dir), "--output="+pwned)
	if err == nil {
		t.Fatalf("Show(--output=…) = %+v, want an unknown-revision error", got)
	}
	assertUnwritten(t, pwned)
}

// TestShowGitTargetsItsDirNotTheCWD pins git's -C to the directory Show was
// given: the fixture the test runs inside holds only a parentless root commit,
// so a Show that fell back to the working directory could not succeed here.
func TestShowGitTargetsItsDirNotTheCWD(t *testing.T) {
	// other is built first so the working directory ends in f, the parentless
	// fixture: a Show that ignored its dir argument would answer from there.
	other := vcstest.Repo(t).Dir
	write(t, other, "seed.txt", "two\n")
	runGit(t, other, "add", "-A")
	runGit(t, other, "commit", "-qm", "c")
	f := vcstest.Repo(t)

	if got, err := Show(context.Background(), render.Dir(f.Dir), ""); err == nil {
		t.Fatalf("Show(fixture) = %+v, want the root commit to have no range", got)
	}
	got, err := Show(context.Background(), render.Dir(other), "")
	if err != nil {
		t.Fatalf("Show(other) error = %v", err)
	}
	if got.Subject != "c" || got.Author != "t" || got.Email != "t@t.t" {
		t.Errorf("Show(other) = %+v, want the second repo's commit", got)
	}
}

// TestShowJJ drives Show against a real colocated jj repository: the default ref
// is @-, jj-native revsets and change ids reach jj untranslated, and a git
// symbolic ref resolves through git rev-parse before jj ever sees it.
func TestShowJJ(t *testing.T) {
	f := vcstest.Repo(t, vcstest.JJ())
	dir := f.Dir

	write(t, dir, "f.txt", "widget\n")
	runJJ(t, dir, "commit", "-m", "Add the widget\n\nExplain the widget.")

	initID := jjField(t, dir, "@--", "commit_id")
	widgetID := jjField(t, dir, "@-", "commit_id")
	wcID := jjField(t, dir, "@", "commit_id")
	widget := Commit{
		ShortID: jjField(t, dir, "@-", "commit_id.short()"),
		Author:  "t",
		Email:   "t@t.t",
		Date:    jjField(t, dir, "@-", `author.timestamp().format("%Y-%m-%d")`),
		Subject: "Add the widget",
		Body:    "Explain the widget.",
		Range:   initID + ".." + widgetID,
	}

	tests := []struct {
		id      string
		ref     string
		want    Commit
		wantErr bool
	}{
		{id: "empty ref defaults to @-", ref: "", want: widget},
		{
			id:  "@ names the working copy",
			ref: "@",
			want: Commit{
				ShortID: jjField(t, dir, "@", "commit_id.short()"),
				Author:  "t",
				Email:   "t@t.t",
				Date:    jjField(t, dir, "@", `author.timestamp().format("%Y-%m-%d")`),
				Range:   widgetID + ".." + wcID,
			},
		},
		{id: "change id passes through to jj", ref: jjField(t, dir, "@-", "change_id"), want: widget},
		{id: "git symbolic ref resolves before jj sees it", ref: "HEAD", want: widget},
		{id: "unresolvable revset errors", ref: "no-such-revset", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.id, func(t *testing.T) {
			got, err := Show(context.Background(), render.Dir(dir), tt.ref)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("Show(%q) = %+v, want error", tt.ref, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("Show(%q) error = %v", tt.ref, err)
			}
			if got != tt.want {
				t.Errorf("Show(%q) =\n %+v\nwant\n %+v", tt.ref, got, tt.want)
			}
		})
	}
}

// TestShowJJNativeRevsetNeverRunsGit reads the one observable the returned
// commit cannot carry: a colocated repository answers @- through either binary,
// so only the tool calls expose a translation that ran with nothing to translate.
func TestShowJJNativeRevsetNeverRunsGit(t *testing.T) {
	f := vcstest.Repo(t, vcstest.JJ())
	if got := vcstest.Invocations(t, f.ArgvLog); got != nil {
		t.Fatalf("fixture construction leaked into the argv log: %v", got)
	}

	if _, err := Show(context.Background(), render.Dir(f.Dir), "@-"); err != nil {
		t.Fatalf("Show(@-) error = %v", err)
	}

	got := vcstest.Invocations(t, f.ArgvLog)
	if len(got) == 0 {
		t.Fatal("Show(@-) ran no tool at all")
	}
	for _, argv := range got {
		if argv[0] != "jj" {
			t.Errorf("Show(@-) ran %v, want jj alone: @- is jj-native and needs no git resolution", argv)
		}
	}
}

// gitField reads one --format placeholder off sha through git log, so an
// expectation is built by a different plumbing command than the one under test.
func gitField(t *testing.T, dir, sha, placeholder string) string {
	t.Helper()
	return gitOutput(t, dir, "log", "-1", "--date=short", "--format="+placeholder, sha)
}

// jjField evaluates a jj template against rev and returns its stdout trimmed;
// runJJ folds stderr in, which jj uses for the hints a commit id must not carry.
func jjField(t *testing.T, dir, rev, template string) string {
	t.Helper()
	cmd := exec.Command("jj", "log", "--no-graph", "-r", rev, "-T", template) //nolint:gosec // fixed jj verb; dir is the fixture repo and the template is a test literal
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		stderr := ""
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			stderr = string(ee.Stderr)
		}
		t.Fatalf("jj log -r %s -T %s: %v\n%s", rev, template, err, stderr)
	}
	return strings.TrimSpace(string(out))
}
