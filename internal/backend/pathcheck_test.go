package backend

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"syscall"
	"testing"
)

// TestNormalizeGlobs pins the canonical form every engine sees: a bare directory
// glob selects no file in rg, so an include naming one becomes "dir/**", while an
// exclusion keeps its bare form because excluding a directory prunes the subtree.
func TestNormalizeGlobs(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "pkg", "sub"), 0o750); err != nil {
		t.Fatal(err)
	}
	writePathcheckFile(t, filepath.Join(dir, "pkg", "f.go"))
	t.Chdir(dir)

	tests := []struct {
		name  string
		globs []string
		want  []string
	}{
		{"no globs", nil, nil},
		{"metachar glob untouched", []string{"*.go"}, []string{"*.go"}},
		{"existing dir becomes a subtree include", []string{"pkg"}, []string{"pkg/**"}},
		{"trailing slash collapses into the subtree include", []string{"pkg/"}, []string{"pkg/**"}},
		{"nested dir normalizes too", []string{"pkg/sub"}, []string{"pkg/sub/**"}},
		{"regular file is left alone", []string{"pkg/f.go"}, []string{"pkg/f.go"}},
		{"missing path is left alone", []string{"nope"}, []string{"nope"}},
		{"exclusion keeps its bare directory form", []string{"!pkg"}, []string{"!pkg"}},
		{"leading ./ is dropped", []string{"./**/*.go"}, []string{"**/*.go"}},
		{"leading ./ is dropped on an exclusion too", []string{"!./x/*.go"}, []string{"!x/*.go"}},
		{"whole list normalizes in place", []string{"pkg", "!*.md", "*.go"}, []string{"pkg/**", "!*.md", "*.go"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := normalizeGlobs(tt.globs); !slices.Equal(got, tt.want) {
				t.Errorf("normalizeGlobs(%q) = %q, want %q", tt.globs, got, tt.want)
			}
		})
	}
}

func TestResolvePath(t *testing.T) {
	tests := []struct {
		name            string
		op              Op
		setup           func(*testing.T) (Args, Args)
		wantErrIs       []error
		wantNotErrIs    []error
		wantContains    []string
		wantPathInError bool
		wantNote        string
	}{
		{
			name: "non-read passthrough",
			op:   OpSearch,
			setup: func(*testing.T) (Args, Args) {
				a := Args{Path: "/nonexistent/garbage", Full: true}
				return a, a
			},
		},
		{
			name: "relative file unchanged",
			op:   OpRead,
			setup: func(t *testing.T) (Args, Args) {
				t.Chdir(t.TempDir())
				writePathcheckFile(t, "f.txt")
				a := Args{Path: "f.txt", Full: true}
				return a, a
			},
		},
		{
			name: "absolute out-of-repo file unchanged",
			op:   OpRead,
			setup: func(t *testing.T) (Args, Args) {
				path := filepath.Join(t.TempDir(), "f.txt")
				writePathcheckFile(t, path)
				a := Args{Path: path}
				return a, a
			},
		},
		{
			name: "home-relative file expands",
			op:   OpRead,
			setup: func(t *testing.T) (Args, Args) {
				home := t.TempDir()
				t.Setenv("HOME", home)
				path := filepath.Join(home, "f.txt")
				writePathcheckFile(t, path)
				return Args{Path: "~/f.txt"}, Args{Path: path}
			},
		},
		{
			name: "tilde expansion is textual, not cleaned",
			op:   OpRead,
			setup: func(t *testing.T) (Args, Args) {
				home := t.TempDir()
				t.Setenv("HOME", home)
				if err := os.Mkdir(filepath.Join(home, "a"), 0o700); err != nil {
					t.Fatalf("mkdir fixture: %v", err)
				}
				writePathcheckFile(t, filepath.Join(home, "b.txt"))
				return Args{Path: "~/a/../b.txt"}, Args{Path: home + "/a/../b.txt"}
			},
		},
		{
			name: "missing absolute file",
			op:   OpRead,
			setup: func(t *testing.T) (Args, Args) {
				path := filepath.Join(t.TempDir(), "missing.txt")
				a := Args{Path: path}
				return a, a
			},
			wantErrIs:       []error{ErrPathNotFound, fs.ErrNotExist},
			wantPathInError: true,
		},
		{
			name: "empty operand is path-not-found without extension inference",
			op:   OpRead,
			setup: func(t *testing.T) (Args, Args) {
				t.Chdir(t.TempDir())
				writePathcheckFile(t, ".env")
				a := Args{Path: ""}
				return a, a
			},
			wantErrIs:    []error{ErrPathNotFound},
			wantNotErrIs: []error{fs.ErrNotExist},
		},
		{
			name: "trailing slash over file operand is path-not-found",
			op:   OpRead,
			setup: func(t *testing.T) (Args, Args) {
				path := filepath.Join(t.TempDir(), "a.go")
				writePathcheckFile(t, path)
				a := Args{Path: path + string(os.PathSeparator)}
				return a, a
			},
			wantErrIs: []error{ErrPathNotFound, syscall.ENOTDIR},
		},
		{
			name: "directory",
			op:   OpRead,
			setup: func(t *testing.T) (Args, Args) {
				path := t.TempDir()
				a := Args{Path: path}
				return a, a
			},
			wantNotErrIs: []error{ErrPathNotFound},
			wantContains: []string{"is a directory", "ccx code outline"},
		},
		{
			name: "bare tilde directory",
			op:   OpRead,
			setup: func(t *testing.T) (Args, Args) {
				home := t.TempDir()
				t.Setenv("HOME", home)
				return Args{Path: "~"}, Args{Path: home}
			},
			wantNotErrIs: []error{ErrPathNotFound},
			wantContains: []string{"is a directory"},
		},
		{
			name: "named-user tilde stays literal",
			op:   OpRead,
			setup: func(t *testing.T) (Args, Args) {
				t.Chdir(t.TempDir())
				a := Args{Path: "~root/x"}
				return a, a
			},
			wantErrIs:    []error{ErrPathNotFound, fs.ErrNotExist},
			wantContains: []string{"~root/x"},
		},
		{
			name: "deps missing file is path-not-found (closes 50ae868)",
			op:   OpDeps,
			setup: func(t *testing.T) (Args, Args) {
				path := filepath.Join(t.TempDir(), "missing.go")
				a := Args{Path: path}
				return a, a
			},
			wantErrIs:       []error{ErrPathNotFound, fs.ErrNotExist},
			wantContains:    []string{"deps"},
			wantPathInError: true,
		},
		{
			name: "deps existing file passes",
			op:   OpDeps,
			setup: func(t *testing.T) (Args, Args) {
				t.Chdir(t.TempDir())
				writePathcheckFile(t, "a.go")
				a := Args{Path: "a.go"}
				return a, a
			},
		},
		{
			name: "unique extension sibling resolves",
			op:   OpRead,
			setup: func(t *testing.T) (Args, Args) {
				t.Chdir(t.TempDir())
				writePathcheckFile(t, "events.py")
				return Args{Path: "events"}, Args{Path: "events.py"}
			},
			wantNote: "# note: events → events.py\n",
		},
		{
			name: "multiple extension siblings error lists candidates",
			op:   OpGrep,
			setup: func(t *testing.T) (Args, Args) {
				t.Chdir(t.TempDir())
				writePathcheckFile(t, "events.go")
				writePathcheckFile(t, "events.py")
				a := Args{Paths: []string{"events"}}
				return a, a
			},
			wantContains: []string{"events.go", "events.py"},
		},
		{
			name: "missing grep operand is path-not-found",
			op:   OpGrep,
			setup: func(t *testing.T) (Args, Args) {
				t.Chdir(t.TempDir())
				a := Args{Paths: []string{"missing"}}
				return a, a
			},
			wantErrIs: []error{ErrPathNotFound, fs.ErrNotExist},
		},
		{
			name: "glob metachar operand passes unchanged",
			op:   OpGrep,
			setup: func(t *testing.T) (Args, Args) {
				t.Chdir(t.TempDir())
				a := Args{Paths: []string{"events/*"}}
				return a, a
			},
		},
		{
			name: "grep resolves one operand among existing paths",
			op:   OpGrep,
			setup: func(t *testing.T) (Args, Args) {
				t.Chdir(t.TempDir())
				writePathcheckFile(t, "existing.go")
				writePathcheckFile(t, "events.py")
				return Args{Paths: []string{"existing.go", "events"}}, Args{Paths: []string{"existing.go", "events.py"}}
			},
			wantNote: "# note: events → events.py\n",
		},
		{
			name: "grep resolution error leaves original paths unchanged",
			op:   OpGrep,
			setup: func(t *testing.T) (Args, Args) {
				t.Chdir(t.TempDir())
				writePathcheckFile(t, "first.go")
				return Args{Paths: []string{"first", "missing"}}, Args{Paths: []string{"first.go", "missing"}}
			},
			wantErrIs: []error{ErrPathNotFound, fs.ErrNotExist},
		},
		{
			name: "edit resolves unique extension sibling",
			op:   OpEdit,
			setup: func(t *testing.T) (Args, Args) {
				t.Chdir(t.TempDir())
				writePathcheckFile(t, "events.py")
				return Args{Path: "events"}, Args{Path: "events.py"}
			},
			wantNote: "# note: events → events.py\n",
		},
		{
			name: "structural resolves unique extension sibling",
			op:   OpStructural,
			setup: func(t *testing.T) (Args, Args) {
				t.Chdir(t.TempDir())
				writePathcheckFile(t, "events.py")
				return Args{Paths: []string{"events"}}, Args{Paths: []string{"events.py"}}
			},
			wantNote: "# note: events → events.py\n",
		},
		{
			name: "replace resolves unique extension sibling",
			op:   OpReplace,
			setup: func(t *testing.T) (Args, Args) {
				t.Chdir(t.TempDir())
				writePathcheckFile(t, "events.py")
				return Args{Paths: []string{"events"}}, Args{Paths: []string{"events.py"}}
			},
			wantNote: "# note: events → events.py\n",
		},
		{
			name: "grep globs normalize before dispatch",
			op:   OpGrep,
			setup: func(t *testing.T) (Args, Args) {
				dir := t.TempDir()
				if err := os.MkdirAll(filepath.Join(dir, "pkg"), 0o750); err != nil {
					t.Fatal(err)
				}
				t.Chdir(dir)
				return Args{Globs: []string{"pkg", "./*.go"}}, Args{Globs: []string{"pkg/**", "*.go"}}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			args, want := tt.setup(t)
			originalPaths := slices.Clone(args.Paths)
			got, note, err := ResolvePath(tt.op, args)
			if !reflect.DeepEqual(args.Paths, originalPaths) {
				t.Errorf("ResolvePath() mutated input paths = %v, want %v", args.Paths, originalPaths)
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("ResolvePath() args = %+v, want %+v", got, want)
			}
			if note != tt.wantNote {
				t.Errorf("ResolvePath() note = %q, want %q", note, tt.wantNote)
			}
			if len(tt.wantErrIs) == 0 && len(tt.wantNotErrIs) == 0 && len(tt.wantContains) == 0 {
				if err != nil {
					t.Fatalf("ResolvePath() error = %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("ResolvePath() error = nil, want error")
			}
			for _, target := range tt.wantErrIs {
				if !errors.Is(err, target) {
					t.Errorf("errors.Is(%v, %v) = false, want true", err, target)
				}
			}
			for _, target := range tt.wantNotErrIs {
				if errors.Is(err, target) {
					t.Errorf("errors.Is(%v, %v) = true, want false", err, target)
				}
			}
			for _, want := range tt.wantContains {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error = %q, want containing %q", err, want)
				}
			}
			if tt.wantPathInError && !strings.Contains(err.Error(), got.Path) {
				t.Errorf("error = %q, want containing expanded path %q", err, got.Path)
			}
		})
	}
}

func writePathcheckFile(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, []byte("content\n"), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
}
