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
		{"trunk()..@ is default branch", "trunk()..@", translationDefaultBranch},
		{"main..@ is default branch", "main..@", translationDefaultBranch},
		{"master..@ is default branch", "master..@", translationDefaultBranch},
		{"HEAD~1 passes through", "HEAD~1", translationPassthrough},
		{"git range passes through", "main..feat", translationPassthrough},
		{"sha passes through", "a1b2c3d", translationPassthrough},
		{"branch name passes through", "feature-x", translationPassthrough},
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
	boom := func(context.Context, string) (string, error) { return "", errors.New("no origin") }

	tests := []struct {
		id           string
		source       string
		scope        string
		lookup       branchLookup
		wantTrans    string
		wantUseTilth bool
		wantArgv     []string
		wantErr      bool
	}{
		{
			id: "working tree", source: "", lookup: branch,
			wantTrans: "", wantUseTilth: true,
		},
		{
			id: "uncommitted working tree", source: "uncommitted", lookup: branch,
			wantTrans: "", wantUseTilth: true,
		},
		{
			id: "@- resolves to HEAD", source: "@-", lookup: branch,
			wantTrans: "HEAD", wantUseTilth: true,
		},
		{
			id: "default branch resolved", source: "trunk()..@", lookup: branch,
			wantTrans: "develop", wantUseTilth: true,
		},
		{
			id: "default branch lookup error", source: "main..@", lookup: boom,
			wantUseTilth: false, wantErr: true,
		},
		{
			id: "git ref passthrough", source: "HEAD~2", lookup: branch,
			wantTrans: "HEAD~2", wantUseTilth: true,
		},
		{
			id: "bare @ falls back to jj", source: "@", lookup: branch,
			wantUseTilth: false, wantArgv: []string{"jj", "diff", "--stat", "-r", "@"},
		},
		{
			id: "dag revset falls back to jj", source: "::@", lookup: branch,
			wantUseTilth: false, wantArgv: []string{"jj", "diff", "--stat", "-r", "::@"},
		},
		{
			id: "jj fallback threads scope as path filter", source: "@", scope: "internal", lookup: branch,
			wantUseTilth: false, wantArgv: []string{"jj", "diff", "--stat", "-r", "@", "internal"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.id, func(t *testing.T) {
			gotTrans, gotUse, gotArgv, err := resolveJJ(context.Background(), dir, tt.source, tt.scope, tt.lookup)
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

func TestRawHunkArgv(t *testing.T) {
	root := t.TempDir()
	gitRepo := filepath.Join(root, "git")
	mustMkdir(t, filepath.Join(gitRepo, ".git"))
	jjRepo := filepath.Join(root, "jj")
	mustMkdir(t, filepath.Join(jjRepo, ".jj"))

	tests := []struct {
		id     string
		dir    string
		source string
		file   string
		want   []string
	}{
		{
			id: "git working tree omits the ref", dir: gitRepo, source: "uncommitted", file: "a.go",
			want: []string{"git", "-C", gitRepo, "diff", "--", "a.go"},
		},
		{
			id: "git empty source omits the ref", dir: gitRepo, source: "", file: "a.go",
			want: []string{"git", "-C", gitRepo, "diff", "--", "a.go"},
		},
		{
			id: "git ref passes through", dir: gitRepo, source: "HEAD~1", file: "sub/b.go",
			want: []string{"git", "-C", gitRepo, "diff", "HEAD~1", "--", "sub/b.go"},
		},
		{
			id: "jj @- maps to git HEAD", dir: jjRepo, source: "@-", file: "a.go",
			want: []string{"git", "-C", jjRepo, "diff", "HEAD", "--", "a.go"},
		},
		{
			id: "jj-only revset falls back to jj diff", dir: jjRepo, source: "::@", file: "a.go",
			want: []string{"jj", "diff", "--git", "-r", "::@", "a.go"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.id, func(t *testing.T) {
			got, err := RawHunkArgv(context.Background(), tt.dir, tt.source, tt.file)
			if err != nil {
				t.Fatalf("RawHunkArgv(%q, %q) err: %v", tt.dir, tt.source, err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("RawHunkArgv = %v, want %v", got, tt.want)
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
