package backend

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestResolveReadPath(t *testing.T) {
	tests := []struct {
		name            string
		op              Op
		setup           func(*testing.T) (Args, Args)
		wantErrIs       []error
		wantNotErrIs    []error
		wantContains    []string
		wantPathInError bool
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
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			args, want := tt.setup(t)
			got, err := ResolveReadPath(tt.op, args)
			if !reflect.DeepEqual(got, want) {
				t.Errorf("ResolveReadPath() args = %+v, want %+v", got, want)
			}
			if len(tt.wantErrIs) == 0 && len(tt.wantNotErrIs) == 0 && len(tt.wantContains) == 0 {
				if err != nil {
					t.Fatalf("ResolveReadPath() error = %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("ResolveReadPath() error = nil, want error")
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
