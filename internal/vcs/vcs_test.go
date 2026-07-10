package vcs

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestDetect(t *testing.T) {
	root := t.TempDir()

	jjDir := filepath.Join(root, "jjrepo")
	mustMkdir(t, filepath.Join(jjDir, ".jj"))
	mustMkdir(t, filepath.Join(jjDir, ".git")) // colocated: jj wins
	colocatedChild := filepath.Join(jjDir, "pkg", "sub")
	mustMkdir(t, colocatedChild)

	gitDir := filepath.Join(root, "gitrepo")
	mustMkdir(t, filepath.Join(gitDir, ".git"))
	gitChild := filepath.Join(gitDir, "internal")
	mustMkdir(t, gitChild)

	plain := filepath.Join(root, "plain")
	mustMkdir(t, plain)

	tests := []struct {
		id   string
		dir  string
		want Kind
	}{
		{"jj root", jjDir, JJ},
		{"jj wins over colocated git", jjDir, JJ},
		{"jj from nested child", colocatedChild, JJ},
		{"git root", gitDir, Git},
		{"git from nested child", gitChild, Git},
		{"none", plain, None},
	}
	for _, tt := range tests {
		t.Run(tt.id, func(t *testing.T) {
			if got := Detect(tt.dir); got != tt.want {
				t.Fatalf("Detect(%q) = %v, want %v", tt.dir, got, tt.want)
			}
		})
	}
}

func TestTranslateRevset(t *testing.T) {
	tests := []struct {
		id     string
		source string
		want   translation
	}{
		{"empty is working tree", "", translationWorkingTree},
		{"uncommitted is working tree", "uncommitted", translationWorkingTree},
		{"@- maps to HEAD", "@-", translationHEAD},
		{"bare @ is jj-only", "@", translationJJOnly},
		{"staged maps to staged", "staged", translationStaged},
		{"trunk()..@ is default branch", "trunk()..@", translationDefaultBranch},
		{"main..@ is default branch", "main..@", translationDefaultBranch},
		{"master..@ is default branch", "master..@", translationDefaultBranch},
		{"HEAD~1 is ref vs working", "HEAD~1", translationRefVsWorking},
		{"git range passes through", "main..feat", translationPassthrough},
		{"sha is ref vs working", "a1b2c3d", translationRefVsWorking},
		{"branch name is ref vs working", "feature-x", translationRefVsWorking},
		{"single ref is ref vs working", "feature", translationRefVsWorking},
		{"@+ marker is jj-only", "@+", translationJJOnly},
		{"@-- chain is a git candidate", "@--", translationRefVsWorking},
		{"dag range is jj-only", "::@", translationJJOnly},
		{"ancestors operator is jj-only", "foo::bar", translationJJOnly},
		{"union operator is jj-only", "main | feat", translationJJOnly},
		{"intersection operator is jj-only", "x&y", translationJJOnly},
		{"negation operator is jj-only", "~x", translationJJOnly},
		{"embedded-@ ref is a git candidate resolveJJ disambiguates", "show@op", translationRefVsWorking},
		{"embedded-@ range stays jj (git cannot rev-parse a range)", "main@origin..feat", translationJJOnly},
	}
	for _, tt := range tests {
		t.Run(tt.id, func(t *testing.T) {
			if got := translateRevset(tt.source); got != tt.want {
				t.Fatalf("translateRevset(%q) = %v, want %v", tt.source, got, tt.want)
			}
		})
	}
}

func TestResolveJJ(t *testing.T) {
	const dir = "/repo"
	branch := func(context.Context, string) (string, error) { return "develop", nil }
	branchBoom := func(context.Context, string) (string, error) { return "", errors.New("no origin") }
	commit := func(_ context.Context, _, rev string) (string, error) {
		switch rev {
		case "@":
			return "AAAAAAA", nil
		case "@-":
			return "BBBBBBB", nil
		default:
			return "", errors.New("unexpected rev " + rev)
		}
	}
	commitBoom := func(context.Context, string, string) (string, error) { return "", errors.New("no jj") }
	// resolve stands in for git rev-parse: release@1 is a real git ref, every
	// other embedded-@ form (a jj bookmark@remote) is unresolvable.
	resolve := func(_ context.Context, _, ref string) (string, bool) {
		if ref == "release@1" {
			return "RRRRRRR", true
		}
		return "", false
	}

	tests := []struct {
		id           string
		source       string
		scope        string
		branch       branchLookup
		commit       workingCopyLookup
		wantTrans    string
		wantUseTilth bool
		wantArgv     []string
		wantErr      bool
	}{
		{
			id: "working tree is @-..@", source: "", branch: branch, commit: commit,
			wantTrans: "BBBBBBB..AAAAAAA", wantUseTilth: true,
		},
		{
			id: "uncommitted is @-..@", source: "uncommitted", branch: branch, commit: commit,
			wantTrans: "BBBBBBB..AAAAAAA", wantUseTilth: true,
		},
		{
			id: "@- (working vs @-) is @-..@", source: "@-", branch: branch, commit: commit,
			wantTrans: "BBBBBBB..AAAAAAA", wantUseTilth: true,
		},
		{
			id: "single ref is ref..@", source: "main", branch: branch, commit: commit,
			wantTrans: "main..AAAAAAA", wantUseTilth: true,
		},
		{
			id: "staged passes through to tilth", source: "staged", branch: branch, commit: commit,
			wantTrans: "staged", wantUseTilth: true,
		},
		{
			id: "default branch is branch..@", source: "trunk()..@", branch: branch, commit: commit,
			wantTrans: "develop..AAAAAAA", wantUseTilth: true,
		},
		{
			id: "committed range passes through", source: "main..feat", branch: branch, commit: commit,
			wantTrans: "main..feat", wantUseTilth: true,
		},
		{
			id: "default branch lookup error", source: "main..@", branch: branchBoom, commit: commit,
			wantUseTilth: false, wantErr: true,
		},
		{
			id: "working-copy lookup error", source: "", branch: branch, commit: commitBoom,
			wantUseTilth: false, wantErr: true,
		},
		{
			id: "bare @ falls back to jj", source: "@", branch: branch, commit: commit,
			wantUseTilth: false, wantArgv: []string{"jj", "diff", "--stat", "-r", "@"},
		},
		{
			id: "dag revset falls back to jj", source: "::@", branch: branch, commit: commit,
			wantUseTilth: false, wantArgv: []string{"jj", "diff", "--stat", "-r", "::@"},
		},
		{
			id: "jj fallback threads scope as path filter", source: "@", scope: "internal", branch: branch, commit: commit,
			wantUseTilth: false, wantArgv: []string{"jj", "diff", "--stat", "-r", "@", "internal"},
		},
		{
			id: "embedded-@ git ref resolves to ref..@", source: "release@1", branch: branch, commit: commit,
			wantTrans: "release@1..AAAAAAA", wantUseTilth: true,
		},
		{
			id: "embedded-@ jj bookmark falls back to jj", source: "main@origin", branch: branch, commit: commit,
			wantUseTilth: false, wantArgv: []string{"jj", "diff", "--stat", "-r", "main@origin"},
		},
		{
			id: "embedded-@ range falls back to jj", source: "main@origin..feat", branch: branch, commit: commit,
			wantUseTilth: false, wantArgv: []string{"jj", "diff", "--stat", "-r", "main@origin..feat"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.id, func(t *testing.T) {
			gotTrans, gotUse, gotArgv, err := resolveJJ(context.Background(), dir, tt.source, tt.scope, tt.branch, tt.commit, resolve)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("resolveJJ(%q) err = nil, want error", tt.source)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveJJ(%q) unexpected err: %v", tt.source, err)
			}
			if gotTrans != tt.wantTrans {
				t.Errorf("translated = %q, want %q", gotTrans, tt.wantTrans)
			}
			if gotUse != tt.wantUseTilth {
				t.Errorf("useTilth = %v, want %v", gotUse, tt.wantUseTilth)
			}
			if !reflect.DeepEqual(gotArgv, tt.wantArgv) {
				t.Errorf("fallbackArgv = %v, want %v", gotArgv, tt.wantArgv)
			}
		})
	}
}

func TestJJFallbackArgv(t *testing.T) {
	if got := jjFallbackArgv("", ""); !reflect.DeepEqual(got, []string{"jj", "diff", "--stat"}) {
		t.Fatalf("empty source argv = %v", got)
	}
	if got := jjFallbackArgv("@", ""); !reflect.DeepEqual(got, []string{"jj", "diff", "--stat", "-r", "@"}) {
		t.Fatalf("@ source argv = %v", got)
	}
	if got := jjFallbackArgv("@", "internal"); !reflect.DeepEqual(got, []string{"jj", "diff", "--stat", "-r", "@", "internal"}) {
		t.Fatalf("@ source with scope argv = %v", got)
	}
}

// TestRawHunkArgvForGitRepo exercises the plain-git path end to end: in a git repo
// ResolveDiffSource passes the source through untouched, and RawHunkArgvFor omits
// the ref for a working-tree diff while passing a real ref through.
func TestRawHunkArgvForGitRepo(t *testing.T) {
	gitRepo := initLiveGitRepo(t) // two commits so HEAD~1 resolves past ref validation

	tests := []struct {
		id     string
		source string
		file   string
		want   []string
	}{
		{
			id: "git working tree omits the ref", source: "uncommitted", file: "a.go",
			want: []string{"git", "-C", gitRepo, "diff", "--", "a.go"},
		},
		{
			id: "git empty source omits the ref", source: "", file: "a.go",
			want: []string{"git", "-C", gitRepo, "diff", "--", "a.go"},
		},
		{
			id: "git ref passes through", source: "HEAD~1", file: "sub/b.go",
			want: []string{"git", "-C", gitRepo, "diff", "HEAD~1", "--", "sub/b.go"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.id, func(t *testing.T) {
			tilthSource, useTilth, _, err := ResolveDiffSource(context.Background(), gitRepo, tt.source, "")
			if err != nil {
				t.Fatalf("ResolveDiffSource(%q) err: %v", tt.source, err)
			}
			got := RawHunkArgvFor(gitRepo, tt.source, tilthSource, useTilth, tt.file)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("RawHunkArgvFor = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestRawHunkArgvFor(t *testing.T) {
	const dir = "/repo"
	tests := []struct {
		id          string
		source      string
		tilthSource string
		useTilth    bool
		file        string
		want        []string
	}{
		{
			id: "jj uncommitted range diffs the commit range", source: "uncommitted", tilthSource: "BBBBBBB..AAAAAAA", useTilth: true, file: "a.go",
			want: []string{"git", "-C", dir, "diff", "BBBBBBB..AAAAAAA", "--", "a.go"},
		},
		{
			id: "staged diffs against the index", source: "staged", tilthSource: stagedSource, useTilth: true, file: "a.go",
			want: []string{"git", "-C", dir, "diff", "--cached", "--", "a.go"},
		},
		{
			id: "jj ref vs working diffs ref..@", source: "main", tilthSource: "main..AAAAAAA", useTilth: true, file: "sub/b.go",
			want: []string{"git", "-C", dir, "diff", "main..AAAAAAA", "--", "sub/b.go"},
		},
		{
			id: "jj-only revset falls back to jj diff", source: "::@", tilthSource: "", useTilth: false, file: "a.go",
			want: []string{"jj", "diff", "--git", "-r", "::@", "a.go"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.id, func(t *testing.T) {
			got := RawHunkArgvFor(dir, tt.source, tt.tilthSource, tt.useTilth, tt.file)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("RawHunkArgvFor = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestResolveDiffSourceGitValidatesRefs drives the injectable git-branch resolver
// directly: working-tree sentinels skip validation, a valid ref passes through, a
// bogus ref errors naming the ref, and both endpoints of a range are validated
// while empty endpoints are skipped.
func TestResolveDiffSourceGitValidatesRefs(t *testing.T) {
	// resolve stands in for git rev-parse: these refs are real revisions, every
	// other name is unknown.
	resolve := func(_ context.Context, _, ref string) (string, bool) {
		switch ref {
		case "HEAD", "main", "feat", "abc123":
			return "COMMIT-" + ref, true
		}
		return "", false
	}
	const dir = "/repo"

	tests := []struct {
		id           string
		source       string
		wantTrans    string
		wantUseTilth bool
		wantErr      string // substring the error must contain; "" means no error
	}{
		{id: "empty skips validation", source: "", wantTrans: "", wantUseTilth: true},
		{id: "uncommitted skips validation", source: "uncommitted", wantTrans: "uncommitted", wantUseTilth: true},
		{id: "staged skips validation", source: "staged", wantTrans: "staged", wantUseTilth: true},
		{id: "valid single ref passes through", source: "HEAD", wantTrans: "HEAD", wantUseTilth: true},
		{id: "bogus single ref errors naming the ref", source: "bogus", wantUseTilth: false, wantErr: `"bogus"`},
		{id: "two-dot range validates both endpoints", source: "main..feat", wantTrans: "main..feat", wantUseTilth: true},
		{id: "three-dot range validates both endpoints", source: "main...feat", wantTrans: "main...feat", wantUseTilth: true},
		{id: "two-dot range with a bogus endpoint errors", source: "main..nope", wantUseTilth: false, wantErr: `"nope"`},
		{id: "empty left endpoint validates only the right", source: "..feat", wantTrans: "..feat", wantUseTilth: true},
		{id: "empty right endpoint validates only the left", source: "main..", wantTrans: "main..", wantUseTilth: true},
	}
	for _, tt := range tests {
		t.Run(tt.id, func(t *testing.T) {
			gotTrans, gotUse, gotArgv, err := resolveGit(context.Background(), dir, tt.source, resolve)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("resolveGit(%q) err = %v, want it to contain %q", tt.source, err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveGit(%q) unexpected err: %v", tt.source, err)
			}
			if gotTrans != tt.wantTrans {
				t.Errorf("translated = %q, want %q", gotTrans, tt.wantTrans)
			}
			if gotUse != tt.wantUseTilth {
				t.Errorf("useTilth = %v, want %v", gotUse, tt.wantUseTilth)
			}
			if gotArgv != nil {
				t.Errorf("fallbackArgv = %v, want nil", gotArgv)
			}
		})
	}
}

// TestResolveDiffSourceGitRejectsBogusRef proves the wired-up ResolveDiffSource
// (real git rev-parse) errors on a nonexistent ref in a live git repo instead of
// letting tilth silently render an empty diff.
func TestResolveDiffSourceGitRejectsBogusRef(t *testing.T) {
	dir := t.TempDir()
	runGit(t, dir, "init", "-q")
	if _, _, _, err := ResolveDiffSource(context.Background(), dir, "nonexistent-ref", ""); err == nil {
		t.Fatalf("ResolveDiffSource(git, %q) err = nil, want an error", "nonexistent-ref")
	} else if !strings.Contains(err.Error(), "nonexistent-ref") {
		t.Errorf("error %q does not name the bogus ref", err)
	}
}

func mustMkdir(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o750); err != nil {
		t.Fatalf("mkdir %q: %v", path, err)
	}
}

// initLiveGitRepo stands up a real git repo with two commits, so a relative ref
// like HEAD~1 resolves through the git-branch ref validation.
func initLiveGitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	runGit(t, dir, "init", "-q")
	runGit(t, dir, "config", "user.email", "t@t.t")
	runGit(t, dir, "config", "user.name", "t")
	seed := filepath.Join(dir, "seed.txt")
	for i, content := range []string{"one\n", "two\n"} {
		if err := os.WriteFile(seed, []byte(content), 0o600); err != nil {
			t.Fatalf("write seed rev %d: %v", i, err)
		}
		runGit(t, dir, "add", "-A")
		runGit(t, dir, "commit", "-qm", "c")
	}
	return dir
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...) //nolint:gosec // fixed git argv; dir is a test TempDir, args are literals
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}
