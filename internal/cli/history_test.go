package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

func TestParseCommits(t *testing.T) {
	tests := []struct {
		name string
		out  string
		want []historyCommit
	}{
		{
			name: "empty",
			out:  "",
			want: nil,
		},
		{
			name: "single record with name-status",
			out:  "aaa1111\x002026-06-24\x00feat: rework dispatch\n\nM\tinternal/cli/run.go\n",
			want: []historyCommit{{"aaa1111", "2026-06-24", "feat: rework dispatch", "internal/cli/run.go"}},
		},
		{
			name: "multiple records, blank line before each status",
			out:  "aaa1111\x002026-06-24\x00feat: x\n\nM\ta.go\nbbb2222\x002026-06-23\x00chore: y\n\nM\tb.go\n",
			want: []historyCommit{
				{"aaa1111", "2026-06-24", "feat: x", "a.go"},
				{"bbb2222", "2026-06-23", "chore: y", "b.go"},
			},
		},
		{
			name: "rename status uses the destination path",
			out:  "ccc3333\x002026-06-20\x00refactor: rename\n\nR090\told.go\tnew.go\n",
			want: []historyCommit{{"ccc3333", "2026-06-20", "refactor: rename", "new.go"}},
		},
		{
			name: "subject containing colon and spaces is preserved whole",
			out:  "ddd4444\x002026-06-19\x00fix: keep a: b, c in subject\n\nM\tf.go\n",
			want: []historyCommit{{"ddd4444", "2026-06-19", "fix: keep a: b, c in subject", "f.go"}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := parseCommits(tt.out); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("parseCommits() = %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestParseNumstat(t *testing.T) {
	tests := []struct {
		name        string
		out         string
		wantParents []string
		wantAdded   int
		wantDeleted int
	}{
		{
			name:        "normal commit with one parent",
			out:         "parent0\n\n7\t2\tinternal/cli/run.go\n",
			wantParents: []string{"parent0"},
			wantAdded:   7,
			wantDeleted: 2,
		},
		{
			name:        "root commit has no parents",
			out:         "\n\n40\t0\tinternal/cli/run.go\n",
			wantParents: []string{},
			wantAdded:   40,
			wantDeleted: 0,
		},
		{
			name:        "merge commit lists two parents",
			out:         "p1 p2\n\n1\t1\tf.go\n",
			wantParents: []string{"p1", "p2"},
			wantAdded:   1,
			wantDeleted: 1,
		},
		{
			name:        "binary file reports dash counts as zero",
			out:         "parent0\n\n-\t-\tlogo.png\n",
			wantParents: []string{"parent0"},
			wantAdded:   0,
			wantDeleted: 0,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotParents, gotAdded, gotDeleted := parseNumstat(tt.out)
			if !reflect.DeepEqual(gotParents, tt.wantParents) {
				t.Errorf("parents = %#v, want %#v", gotParents, tt.wantParents)
			}
			if gotAdded != tt.wantAdded || gotDeleted != tt.wantDeleted {
				t.Errorf("(+%d/-%d), want (+%d/-%d)", gotAdded, gotDeleted, tt.wantAdded, tt.wantDeleted)
			}
		})
	}
}

func TestChangedSymbols(t *testing.T) {
	tests := []struct {
		name string
		diff string
		want []string
	}{
		{
			name: "mixed changed and unchanged sections",
			diff: "# Diff: internal/cli/run.go — 2 symbols touched, +7/−2 lines\n\n" +
				"## [ ] runOp (L15-22, unchanged)\n" +
				"## [~] dispatchOp — body changed (L28-47)\n" +
				"## [-] oldHelper — deleted (L50-55)\n",
			want: []string{"~dispatchOp", "-oldHelper"},
		},
		{
			name: "added symbol",
			diff: "## [+] runOp — added (L13-24)\n",
			want: []string{"+runOp"},
		},
		{
			name: "symbol name containing parentheses survives",
			diff: "## [~] import ( — body changed (L3-10)\n" +
				"## [ ] import ( (L3-11, unchanged)\n",
			want: []string{"~import ("},
		},
		{
			name: "no structural symbols (non-structural file)",
			diff: "# Diff: AGENTS.md — 0 symbols touched, +3/−1 lines\n",
			want: nil,
		},
		{
			name: "all unchanged yields nothing",
			diff: "## [ ] runOp (L15-22, unchanged)\n## [ ] dispatchOp (L28-40, unchanged)\n",
			want: nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := changedSymbols(tt.diff); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("changedSymbols() = %#v, want %#v", got, tt.want)
			}
		})
	}
}

// TestHistoryCommand drives newHistoryCmd end to end against a fake git and a fake
// tilth on PATH: the git log enumeration, the per-commit numstat probe, and the
// per-commit tilth structural diff are all stubbed by scripts that echo canned
// output and record their argv. It asserts the exact per-commit output shape
// (symbols, degraded numstat, and the root "(added)" fallback), that the git log
// argv carries --follow and -n, that the tilth range is "<sha>^..<sha>" scoped to
// the path, and that the parentless root commit is never handed to tilth.
func TestHistoryCommand(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake shell scripts are POSIX-only")
	}
	dir := t.TempDir()
	argvLog := filepath.Join(dir, "argv.log")
	writeFake(t, dir, "git", fakeGit)
	writeFake(t, dir, "tilth", fakeTilth)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("FAKE_ARGV_LOG", argvLog)

	var out bytes.Buffer
	cmd := newHistoryCmd()
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"internal/cli/run.go", "-n", "3"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute() error = %v\noutput:\n%s", err, out.String())
	}

	wantOut := "aaa1111 2026-06-24 feat: rework dispatch\n" +
		"    ~dispatchOp, -oldHelper\n" +
		"bbb2222 2026-06-23 chore: reformat\n" +
		"    (+3/-1)\n" +
		"ccc3333 2026-06-20 initial import\n" +
		"    (added)\n"
	if got := out.String(); got != wantOut {
		t.Errorf("output =\n%q\nwant\n%q", got, wantOut)
	}

	logged, err := os.ReadFile(argvLog)
	if err != nil {
		t.Fatalf("read argv log: %v", err)
	}
	argv := string(logged)
	wantContains := []string{
		"git log --follow --format=%h%x00%ad%x00%s --date=short -n 3 --name-status -- internal/cli/run.go",
		"tilth diff aaa1111^..aaa1111 --scope internal/cli/run.go",
		"tilth diff bbb2222^..bbb2222 --scope internal/cli/run.go",
		"git show --numstat --format=%P ccc3333 -- internal/cli/run.go",
	}
	for _, want := range wantContains {
		if !strings.Contains(argv, want) {
			t.Errorf("argv log missing %q\nlog:\n%s", want, argv)
		}
	}
	// The root commit has no parent, so it must never be handed to tilth.
	if strings.Contains(argv, "ccc3333^..ccc3333") {
		t.Errorf("root commit was diffed against a nonexistent parent\nlog:\n%s", argv)
	}

	// --budget caps the whole report, appending the explicit omission footer.
	var capped bytes.Buffer
	cmd = newHistoryCmd()
	cmd.SetOut(&capped)
	cmd.SetErr(&capped)
	cmd.SetArgs([]string{"internal/cli/run.go", "-n", "3", "--budget", "5"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute(--budget) error = %v", err)
	}
	if capped.Len() >= len(wantOut) || !strings.Contains(capped.String(), "tokens omitted") {
		t.Errorf("--budget did not cap output:\n%s", capped.String())
	}
}

// TestHistoryFollowsRename drives newHistoryCmd against a fake git whose --follow
// log crosses two renames (old.go → mid.go → new.go). It proves each commit's
// structural diff is scoped to the file's name AT that commit — the rename
// destination — not the queried path, so pre-rename commits resolve instead of
// handing tilth a path that never existed there (the crash this fixes).
func TestHistoryFollowsRename(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake shell scripts are POSIX-only")
	}
	dir := t.TempDir()
	argvLog := filepath.Join(dir, "argv.log")
	writeFake(t, dir, "git", fakeGitRename)
	writeFake(t, dir, "tilth", fakeTilthRename)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("FAKE_ARGV_LOG", argvLog)

	var out bytes.Buffer
	cmd := newHistoryCmd()
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"pkg/new.go", "-n", "5"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute() error = %v\noutput:\n%s", err, out.String())
	}

	wantOut := "c3aaaaa 2026-06-24 c3: rename mid to new\n" +
		"    +Baz\n" +
		"c2bbbbb 2026-06-23 c2: rename old to mid\n" +
		"    +Bar\n" +
		"c1ccccc 2026-06-20 c1: add old\n" +
		"    (added)\n"
	if got := out.String(); got != wantOut {
		t.Errorf("output =\n%q\nwant\n%q", got, wantOut)
	}

	logged, err := os.ReadFile(argvLog)
	if err != nil {
		t.Fatalf("read argv log: %v", err)
	}
	argv := string(logged)
	wantContains := []string{
		"git log --follow --format=%h%x00%ad%x00%s --date=short -n 5 --name-status -- pkg/new.go",
		"tilth diff c3aaaaa^..c3aaaaa --scope pkg/new.go",
		"tilth diff c2bbbbb^..c2bbbbb --scope pkg/mid.go",
	}
	for _, want := range wantContains {
		if !strings.Contains(argv, want) {
			t.Errorf("argv log missing %q\nlog:\n%s", want, argv)
		}
	}
	// The bug scoped every commit to the queried path; the pre-rename commit must
	// use its own name, never pkg/new.go.
	if strings.Contains(argv, "c2bbbbb^..c2bbbbb --scope pkg/new.go") {
		t.Errorf("pre-rename commit scoped to the post-rename path\nlog:\n%s", argv)
	}
	// The root commit has no parent, so it is never handed to tilth.
	if strings.Contains(argv, "c1ccccc^..c1ccccc") {
		t.Errorf("root commit was diffed against a nonexistent parent\nlog:\n%s", argv)
	}
}

// TestHistoryLiveSmoke runs the real command against this repository. It is
// skipped unless CCX_LIVE_SMOKE is set, since it shells out to the real git
// history and the pinned tilth binary. Assertions are loose (shape, not content)
// so an evolving history does not make it flaky.
func TestHistoryLiveSmoke(t *testing.T) {
	if os.Getenv("CCX_LIVE_SMOKE") == "" {
		t.Skip("set CCX_LIVE_SMOKE=1 to run the live smoke against real git + tilth")
	}
	t.Chdir("../..") // repo root, so the pathspecs below resolve

	block := regexp.MustCompile(`(?m)^[0-9a-f]{7,} \d{4}-\d{2}-\d{2} .+\n {4}\S`)
	// internal/vendor/vendor.go was renamed from tilth.go; --follow crosses the
	// rename, so a pre-rename commit is scoped to its then-current name — the case
	// that used to crash when every commit was scoped to the queried path.
	for _, path := range []string{"AGENTS.md", "internal/cli/run.go", "internal/vendor/vendor.go"} {
		var out bytes.Buffer
		cmd := newHistoryCmd()
		cmd.SetOut(&out)
		cmd.SetErr(&out)
		cmd.SetArgs([]string{path, "-n", "3"})
		if err := cmd.Execute(); err != nil {
			t.Fatalf("history %s: %v\noutput:\n%s", path, err, out.String())
		}
		t.Logf("history %s -n 3:\n%s", path, out.String())
		if !block.MatchString(out.String()) {
			t.Errorf("history %s produced no well-formed commit block:\n%s", path, out.String())
		}
	}
}

// writeFake writes name as an executable script under dir.
func writeFake(t *testing.T, dir, name, body string) {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(body), 0o700); err != nil { //nolint:gosec // fake tool script must be owner-executable
		t.Fatalf("write fake %s: %v", name, err)
	}
}

const fakeGit = `#!/bin/sh
printf 'git %s\n' "$*" >> "$FAKE_ARGV_LOG"
case "$1" in
log)
	printf 'aaa1111\0002026-06-24\000feat: rework dispatch\n\nM\tinternal/cli/run.go\n'
	printf 'bbb2222\0002026-06-23\000chore: reformat\n\nM\tinternal/cli/run.go\n'
	printf 'ccc3333\0002026-06-20\000initial import\n\nA\tinternal/cli/run.go\n'
	;;
show)
	case "$4" in
	aaa1111) printf 'parent0\n\n7\t2\tinternal/cli/run.go\n' ;;
	bbb2222) printf 'parentx\n\n3\t1\tinternal/cli/run.go\n' ;;
	ccc3333) printf '\n\n40\t0\tinternal/cli/run.go\n' ;;
	esac
	;;
esac
`

const fakeTilth = `#!/bin/sh
printf 'tilth %s\n' "$*" >> "$FAKE_ARGV_LOG"
case "$2" in
'aaa1111^..aaa1111')
	cat <<'EOF'
# Diff: internal/cli/run.go — 2 symbols touched, +7/−2 lines

## [ ] runOp (L15-22, unchanged)
## [~] dispatchOp — body changed (L28-47)
## [-] oldHelper — deleted (L50-55)
EOF
	;;
'bbb2222^..bbb2222')
	cat <<'EOF'
# Diff: internal/cli/run.go — 0 symbols touched, +3/−1 lines

## [ ] runOp (L15-22, unchanged)
EOF
	;;
esac
`

// fakeGitRename models a --follow log that crosses two renames: the file is named
// old.go at c1 (root, added), renamed to mid.go at c2, and to new.go at c3. show
// keys on the sha ($4); c1 is parentless so its numstat first line is empty.
const fakeGitRename = `#!/bin/sh
printf 'git %s\n' "$*" >> "$FAKE_ARGV_LOG"
case "$1" in
log)
	printf 'c3aaaaa\0002026-06-24\000c3: rename mid to new\n\nR090\tpkg/mid.go\tpkg/new.go\n'
	printf 'c2bbbbb\0002026-06-23\000c2: rename old to mid\n\nR090\tpkg/old.go\tpkg/mid.go\n'
	printf 'c1ccccc\0002026-06-20\000c1: add old\n\nA\tpkg/old.go\n'
	;;
show)
	case "$4" in
	c3aaaaa) printf 'p2\n\n1\t0\tpkg/new.go\n' ;;
	c2bbbbb) printf 'p1\n\n1\t0\tpkg/mid.go\n' ;;
	c1ccccc) printf '\n\n3\t0\tpkg/old.go\n' ;;
	esac
	;;
esac
`

// fakeTilthRename emits one added symbol per rename commit's range, scoped to that
// commit's own name; c1 is a root commit and is never handed to tilth.
const fakeTilthRename = `#!/bin/sh
printf 'tilth %s\n' "$*" >> "$FAKE_ARGV_LOG"
case "$2" in
'c3aaaaa^..c3aaaaa') printf '## [+] Baz — added (L7-7)\n' ;;
'c2bbbbb^..c2bbbbb') printf '## [+] Bar — added (L5-5)\n' ;;
esac
`
