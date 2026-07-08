package vcs

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

const (
	fakeFullID   = "1111111111111111111111111111111111111111"
	fakeParentID = "2222222222222222222222222222222222222222"
)

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
	resolve := func(_ context.Context, _, ref string) (string, bool) {
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

// TestShowBuildsArgv drives Show against a fake git and a fake jj that record
// their argv and print a canned NUL-separated record. It proves Show selects the
// VCS by Detect, defaults the ref per-VCS, and builds the exact underlying argv.
func TestShowBuildsArgv(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake shell scripts are POSIX-only")
	}
	wantCommit := Commit{
		ShortID: "abc1234",
		Author:  "Ada Lovelace",
		Email:   "ada@example.com",
		Date:    "2026-07-02",
		Subject: "Add the widget",
		Body:    "Explain the widget.",
		Range:   fakeParentID + ".." + fakeFullID,
	}
	tests := []struct {
		name     string
		marker   string
		bin      string
		ref      string
		wantArgv func(dir string) []string
	}{
		{
			name:   "git default ref",
			marker: ".git",
			bin:    "git",
			ref:    "",
			wantArgv: func(dir string) []string {
				return []string{"-C", dir, "show", "--no-patch", "--format=" + gitShowFormat, "--date=short", "HEAD"}
			},
		},
		{
			name:   "git explicit ref",
			marker: ".git",
			bin:    "git",
			ref:    "deadbeef",
			wantArgv: func(dir string) []string {
				return []string{"-C", dir, "show", "--no-patch", "--format=" + gitShowFormat, "--date=short", "deadbeef"}
			},
		},
		{
			name:   "jj default ref",
			marker: ".jj",
			bin:    "jj",
			ref:    "",
			wantArgv: func(string) []string {
				return []string{"log", "--no-graph", "-r", "@-", "-T", jjShowTemplate}
			},
		},
		{
			name:   "jj native ref passes through untranslated",
			marker: ".jj",
			bin:    "jj",
			ref:    "@",
			wantArgv: func(string) []string {
				return []string{"log", "--no-graph", "-r", "@", "-T", jjShowTemplate}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.Mkdir(filepath.Join(dir, tt.marker), 0o750); err != nil {
				t.Fatalf("mkdir %s: %v", tt.marker, err)
			}
			record := filepath.Join(t.TempDir(), "argv")
			fakeDir := writeFakeVCS(t, tt.bin)
			t.Setenv("PATH", fakeDir+string(os.PathListSeparator)+os.Getenv("PATH"))
			t.Setenv("SHOW_RECORD", record)

			got, err := Show(context.Background(), dir, tt.ref)
			if err != nil {
				t.Fatalf("Show(%q) error = %v", tt.ref, err)
			}
			if got != wantCommit {
				t.Errorf("Show() = %+v, want %+v", got, wantCommit)
			}

			gotArgv := readRecordedArgv(t, record)
			wantArgv := tt.wantArgv(dir)
			if !reflect.DeepEqual(gotArgv, wantArgv) {
				t.Errorf("%s argv =\n%q\nwant\n%q", tt.bin, gotArgv, wantArgv)
			}
		})
	}
}

// writeFakeVCS writes an executable named bin that records its argv (one per
// line) to $SHOW_RECORD and prints a canned NUL-separated commit record,
// returning the directory to prepend to PATH.
func writeFakeVCS(t *testing.T, bin string) string {
	t.Helper()
	dir := t.TempDir()
	// Each NUL separator is its own printf: `\0` followed by a digit (the date and
	// ids start with digits) is otherwise read as an octal escape, not a separator.
	script := "#!/bin/sh\n" +
		"printf '%s\\n' \"$@\" > \"$SHOW_RECORD\"\n" +
		"printf 'abc1234'\n" +
		"printf '\\0'; printf 'Ada Lovelace'\n" +
		"printf '\\0'; printf 'ada@example.com'\n" +
		"printf '\\0'; printf '2026-07-02'\n" +
		"printf '\\0'; printf '" + fakeFullID + "'\n" +
		"printf '\\0'; printf '" + fakeParentID + "'\n" +
		"printf '\\0'; printf 'Add the widget\\n\\nExplain the widget.\\n'\n"
	path := filepath.Join(dir, bin)
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil { //nolint:gosec // fake VCS script must be owner-executable
		t.Fatalf("write fake %s: %v", bin, err)
	}
	return dir
}

// readRecordedArgv reads the newline-delimited argv the fake recorded, dropping
// the trailing empty element from printf's final newline.
func readRecordedArgv(t *testing.T, record string) []string {
	t.Helper()
	data, err := os.ReadFile(record)
	if err != nil {
		t.Fatalf("read record: %v", err)
	}
	return strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
}
