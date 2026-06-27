package vcs

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
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
		{"dag range is jj-only", "::@", translationJJOnly},
		{"ancestors operator is jj-only", "foo::bar", translationJJOnly},
		{"union operator is jj-only", "main | feat", translationJJOnly},
		{"intersection operator is jj-only", "x&y", translationJJOnly},
		{"negation operator is jj-only", "~x", translationJJOnly},
		{"op-log style with @ is jj-only", "show@op", translationJJOnly},
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
	}
	for _, tt := range tests {
		t.Run(tt.id, func(t *testing.T) {
			gotTrans, gotUse, gotArgv, err := resolveJJ(context.Background(), dir, tt.source, tt.scope, tt.branch, tt.commit)
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
	root := t.TempDir()
	gitRepo := filepath.Join(root, "git")
	mustMkdir(t, filepath.Join(gitRepo, ".git"))

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

func mustMkdir(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o750); err != nil {
		t.Fatalf("mkdir %q: %v", path, err)
	}
}
