package vcs

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

func TestReadGitMetadataMatchesReadFile(t *testing.T) {
	for _, size := range []int{0, 1, 511, 512, 513, 4096, 1<<20 + 17} {
		t.Run(strconv.Itoa(size), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "metadata")
			contents := bytes.Repeat([]byte{'x', '\n', 0}, (size+2)/3)[:size]
			if err := os.WriteFile(path, contents, 0o600); err != nil {
				t.Fatal(err)
			}
			assertGitMetadataRead(t, path)
			got, err := readGitMetadata(path)
			if err != nil || got == nil {
				t.Fatalf("read = %v, %v; want owned nonnil bytes", got, err)
			}
			mustWriteFile(t, path, "replacement")
			if !bytes.Equal(got, contents) {
				t.Fatal("previous contents changed after file replacement")
			}
		})
	}
	root := t.TempDir()
	for _, name := range []string{"missing", "directory", "symlink", "broken-symlink"} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(root, name)
			switch name {
			case "directory":
				mustMkdir(t, path)
			case "symlink", "broken-symlink":
				target := filepath.Join(root, "target-"+name)
				if name == "symlink" {
					mustWriteFile(t, target, "ref: refs/heads/branch\ntrailing bytes\x00")
				}
				if err := os.Symlink(target, path); err != nil {
					t.Fatal(err)
				}
			}
			assertGitMetadataRead(t, path)
		})
	}
	if runtime.GOOS == "linux" {
		t.Run("zero-stat-proc", func(t *testing.T) {
			assertGitMetadataRead(t, "/proc/self/mountinfo")
		})
		t.Run("read-error-proc", func(t *testing.T) {
			assertGitMetadataRead(t, "/proc/self/mem")
		})
	}
}

func assertGitMetadataRead(t *testing.T, path string) {
	t.Helper()
	want, wantErr := os.ReadFile(path)
	got, gotErr := readGitMetadata(path)
	if !bytes.Equal(got, want) || (got == nil) != (want == nil) {
		t.Fatalf("contents differ for %q: got %d bytes, want %d", path, len(got), len(want))
	}
	if wantErr == nil {
		if gotErr != nil {
			t.Fatal(gotErr)
		}
		return
	}
	var wantPath, gotPath *os.PathError
	if !errors.As(wantErr, &wantPath) || !errors.As(gotErr, &gotPath) {
		t.Fatalf("errors = %v, %v; want PathErrors", gotErr, wantErr)
	}
	if gotPath.Op != wantPath.Op || gotPath.Path != wantPath.Path || !errors.Is(gotPath.Err, wantPath.Err) {
		t.Fatalf("error = %v, want %v", gotErr, wantErr)
	}
}

func TestRetiredHoldingsMetadata(t *testing.T) {
	for _, group := range []string{"retired-worktrees", "objects", "rr-cache", "arbitrary-admin-group"} {
		t.Run(group, func(t *testing.T) {
			common, root, admin := retiredMetadataFixture(t, t.TempDir(), group)
			alias := filepath.Join(common, "admin-alias")
			if err := os.Symlink(admin, alias); err != nil {
				t.Fatal(err)
			}
			relative, err := filepath.Rel(root, alias)
			if err != nil {
				t.Fatal(err)
			}
			mustWriteFile(t, filepath.Join(root, ".git"), gitdirPrefix+relative+"\n")
			mustWriteFile(t, filepath.Join(admin, "gitdir"), "../../../checkout/.git\n")
			stale := filepath.Join(common, group, "stale")
			mustMkdir(t, stale)
			mustWriteFile(t, filepath.Join(stale, "gitdir"), filepath.Join(root, ".git")+"\n")
			mustMkdir(t, filepath.Join(stale, "HEAD"))
			got, err := retiredHoldings(common)
			want := []Holding{{Path: root, Branch: "held"}}
			if err != nil || !reflect.DeepEqual(got, want) {
				t.Fatalf("holdings = %+v, %v; want %+v", got, err, want)
			}
			mustWriteFile(t, filepath.Join(admin, "HEAD"), "ref: refs/heads/held\n"+strings.Repeat("x", 1<<20))
			got, err = retiredHoldings(common)
			if err != nil || len(got) != 1 || got[0].Branch != "held\n"+strings.Repeat("x", 1<<20) {
				t.Fatalf("large HEAD was truncated: %v", err)
			}
			if err := os.Remove(filepath.Join(admin, "HEAD")); err != nil {
				t.Fatal(err)
			}
			mustMkdir(t, filepath.Join(admin, "HEAD"))
			_, err = retiredHoldings(common)
			var pathErr *os.PathError
			if !errors.As(err, &pathErr) || pathErr.Op != "read" || pathErr.Path != filepath.Join(admin, "HEAD") {
				t.Fatalf("HEAD error = %v; want read PathError", err)
			}
		})
	}
}

func TestRetiredHoldingsMetadataSyscalls(t *testing.T) {
	base, err := os.MkdirTemp(t.TempDir(), "ccx-census-syscalls-")
	if err != nil {
		t.Fatal(err)
	}
	common, root, _ := retiredMetadataFixture(t, base, "arbitrary-admin-group")
	got, err := retiredHoldings(common)
	want := []Holding{{Path: root, Branch: "held"}}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("holdings = %+v, %v; want %+v", got, err, want)
	}
}

func retiredMetadataFixture(t *testing.T, base, group string) (string, string, string) {
	t.Helper()
	common := filepath.Join(base, "common")
	root := filepath.Join(base, "checkout")
	admin := filepath.Join(common, group, "admin")
	mustMkdir(t, root)
	mustMkdir(t, admin)
	mustWriteFile(t, filepath.Join(admin, "gitdir"), filepath.Join(root, ".git")+"\n")
	mustWriteFile(t, filepath.Join(root, ".git"), gitdirPrefix+admin+"\n")
	mustWriteFile(t, filepath.Join(admin, "HEAD"), "ref: refs/heads/held\n")
	return common, root, admin
}

func BenchmarkReadGitMetadata(b *testing.B) {
	for _, size := range []int{80, 512, 1<<20 + 17} {
		b.Run(strconv.Itoa(size), func(b *testing.B) {
			path := filepath.Join(b.TempDir(), "metadata")
			if err := os.WriteFile(path, bytes.Repeat([]byte("x"), size), 0o600); err != nil {
				b.Fatal(err)
			}
			for _, reader := range []struct {
				name string
				read func(string) ([]byte, error)
			}{
				{"baseline", os.ReadFile},
				{"fixed", readGitMetadata},
			} {
				b.Run(reader.name, func(b *testing.B) {
					b.ReportAllocs()
					b.SetBytes(int64(size))
					for b.Loop() {
						data, err := reader.read(path)
						if err != nil || len(data) != size {
							b.Fatalf("read = %d bytes, %v; want %d bytes", len(data), err, size)
						}
					}
				})
			}
		})
	}
}
