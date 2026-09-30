package rmtree

import (
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/yasyf/cc-context/internal/cleanup"
)

type fixture struct {
	root, job, payload, outside string
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	root := t.TempDir()
	f := fixture{root: root, job: filepath.Join(root, "job"), outside: filepath.Join(root, "outside")}
	f.payload = filepath.Join(f.job, cleanup.PayloadName)
	mkdir(t, f.payload)
	mkdir(t, filepath.Join(f.job, "sibling"))
	write(t, filepath.Join(f.job, "job.json"), "{}")
	write(t, filepath.Join(f.job, "sibling", "keep"), "keep")
	mkdir(t, filepath.Join(f.outside, "dir"))
	write(t, filepath.Join(f.outside, "file"), "outside")
	write(t, filepath.Join(f.outside, "dir", "inner"), "inner")
	return f
}

func (f fixture) open(t *testing.T) cleanup.Deletion {
	t.Helper()
	id, _, err := cleanup.LstatID(f.payload)
	if err != nil {
		t.Fatalf("LstatID(payload): %v", err)
	}
	d, err := Deleter{}.Open(f.job, cleanup.PayloadName, id)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() {
		if err := d.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return d
}

func (f fixture) snapshot(t *testing.T, skip string) map[string]string {
	t.Helper()
	seen := map[string]string{}
	err := filepath.WalkDir(f.root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == skip {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		desc := info.Mode().String()
		switch {
		case info.Mode()&fs.ModeSymlink != 0:
			target, err := os.Readlink(path)
			if err != nil {
				return err
			}
			desc += " -> " + target
		case info.Mode().IsRegular():
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			desc += " " + string(data)
		}
		seen[strings.TrimPrefix(path, f.root)] = desc
		return nil
	})
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	return seen
}

func (f fixture) assertOnlyPayloadGone(t *testing.T, before map[string]string) {
	t.Helper()
	if _, err := os.Lstat(f.payload); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("Lstat(payload) error = %v, want fs.ErrNotExist", err)
	}
	if after := f.snapshot(t, ""); !maps.Equal(after, before) {
		t.Errorf("tree outside the payload changed:\n got %v\nwant %v", after, before)
	}
}

func mkdir(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
}

func symlink(t *testing.T, target, path string) {
	t.Helper()
	if err := os.Symlink(target, path); err != nil {
		t.Fatalf("symlink: %v", err)
	}
}

func countFiles(t *testing.T, dir string) int {
	t.Helper()
	files := 0
	err := filepath.WalkDir(dir, func(_ string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.Type().IsRegular() {
			files++
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", dir, err)
	}
	return files
}

func populate(t *testing.T, dir string, width, depth, files int) {
	t.Helper()
	for i := range files {
		write(t, filepath.Join(dir, fmt.Sprintf("file-%d", i)), "x")
	}
	if depth == 0 {
		return
	}
	for i := range width {
		sub := filepath.Join(dir, fmt.Sprintf("dir-%d", i))
		mkdir(t, sub)
		populate(t, sub, width, depth-1, files)
	}
}

func nest(t *testing.T, dir string, levels int) {
	t.Helper()
	fd, err := unix.Open(dir, dirFlags, 0)
	if err != nil {
		t.Fatalf("open %s: %v", dir, err)
	}
	for range levels {
		if err := unix.Mkdirat(fd, "d", 0o755); err != nil {
			t.Fatalf("mkdirat: %v", err)
		}
		next, err := unix.Openat(fd, "d", dirFlags, 0)
		if err != nil {
			t.Fatalf("openat: %v", err)
		}
		if err := unix.Close(fd); err != nil {
			t.Fatalf("close: %v", err)
		}
		fd = next
	}
	if err := unix.Close(fd); err != nil {
		t.Fatalf("close: %v", err)
	}
}

func drain(t *testing.T, d cleanup.Deletion, limit int) (total, steps int) {
	t.Helper()
	for {
		n, done, err := d.Step(limit, time.Minute)
		if err != nil {
			t.Fatalf("Step(%d) after %d removed: %v", limit, total, err)
		}
		total += n
		steps++
		if done {
			return total, steps
		}
		if n != limit {
			t.Fatalf("Step(%d) removed %d with entries remaining, want %d", limit, n, limit)
		}
	}
}

func assertClosed(t *testing.T, d cleanup.Deletion) {
	t.Helper()
	n, done, err := d.Step(1, time.Minute)
	var failure *Error
	if n != 0 || done || !errors.Is(err, fs.ErrClosed) || !errors.As(err, &failure) || failure.Op != "step" {
		t.Errorf("Step on a closed deletion = (%d, %v, %v), want (0, false, step error wrapping fs.ErrClosed)", n, done, err)
	}
	if err := d.Close(); err != nil {
		t.Errorf("repeated Close = %v, want nil", err)
	}
}

func TestStepRemovesExactCounts(t *testing.T) {
	shapes := []struct {
		name                string
		width, depth, files int
		want                int
	}{
		{name: "wide", width: 3, depth: 3, files: 4, want: 200},
		{name: "deep", width: 1, depth: 40, files: 2, want: 123},
		{name: "empty", width: 0, depth: 0, files: 0, want: 1},
	}
	for _, shape := range shapes {
		for _, limit := range []int{1, 7, 100} {
			t.Run(fmt.Sprintf("%s/limit-%d", shape.name, limit), func(t *testing.T) {
				f := newFixture(t)
				populate(t, f.payload, shape.width, shape.depth, shape.files)
				before := f.snapshot(t, f.payload)
				d := f.open(t)

				total, steps := drain(t, d, limit)

				if total != shape.want {
					t.Errorf("removed %d entries, want %d", total, shape.want)
				}
				if want := (shape.want + limit - 1) / limit; steps != want {
					t.Errorf("took %d steps, want %d", steps, want)
				}
				f.assertOnlyPayloadGone(t, before)
				assertClosed(t, d)
			})
		}
	}
}

func TestSymlinksAreRemovedNotFollowed(t *testing.T) {
	f := newFixture(t)
	mkdir(t, filepath.Join(f.payload, "nested"))
	symlink(t, filepath.Join(f.outside, "file"), filepath.Join(f.payload, "to-file"))
	symlink(t, filepath.Join(f.outside, "dir"), filepath.Join(f.payload, "to-dir"))
	symlink(t, filepath.Join(f.outside, "dir"), filepath.Join(f.payload, "nested", "to-dir"))
	symlink(t, "../../outside/dir", filepath.Join(f.payload, "relative"))
	symlink(t, filepath.Join(f.outside, "missing"), filepath.Join(f.payload, "dangling"))
	before := f.snapshot(t, f.payload)

	total, _ := drain(t, f.open(t), 100)

	if total != 7 {
		t.Errorf("removed %d entries, want 7", total)
	}
	f.assertOnlyPayloadGone(t, before)
}

func TestDirectorySwappedForSymlinkBetweenSteps(t *testing.T) {
	t.Run("held directory", func(t *testing.T) {
		f := newFixture(t)
		victim := filepath.Join(f.payload, "victim")
		mkdir(t, victim)
		populate(t, victim, 0, 0, 5)
		before := f.snapshot(t, f.payload)
		d := f.open(t)

		if n, done, err := d.Step(2, time.Minute); n != 2 || done || err != nil {
			t.Fatalf("Step(2) = (%d, %v, %v), want (2, false, nil)", n, done, err)
		}
		if err := os.Rename(victim, filepath.Join(f.payload, "moved")); err != nil {
			t.Fatalf("rename: %v", err)
		}
		symlink(t, filepath.Join(f.outside, "dir"), victim)
		total, _ := drain(t, d, 100)

		if total != 6 {
			t.Errorf("removed %d entries after the swap, want 6", total)
		}
		f.assertOnlyPayloadGone(t, before)
	})

	t.Run("unvisited directory", func(t *testing.T) {
		f := newFixture(t)
		for _, name := range []string{"one", "two"} {
			mkdir(t, filepath.Join(f.payload, name))
			populate(t, filepath.Join(f.payload, name), 0, 0, 2)
		}
		before := f.snapshot(t, f.payload)
		d := f.open(t)

		if n, done, err := d.Step(1, time.Minute); n != 1 || done || err != nil {
			t.Fatalf("Step(1) = (%d, %v, %v), want (1, false, nil)", n, done, err)
		}
		untouched := filepath.Join(f.payload, "one")
		if entries, err := os.ReadDir(untouched); err != nil || len(entries) != 2 {
			untouched = filepath.Join(f.payload, "two")
		}
		if err := os.RemoveAll(untouched); err != nil {
			t.Fatalf("remove: %v", err)
		}
		symlink(t, filepath.Join(f.outside, "dir"), untouched)
		total, _ := drain(t, d, 100)

		if total != 4 {
			t.Errorf("removed %d entries after the swap, want 4", total)
		}
		f.assertOnlyPayloadGone(t, before)
	})
}

func TestLockedDirectoriesAreDeleted(t *testing.T) {
	tests := []struct {
		name string
		mode os.FileMode
	}{
		{"read-only", 0o555},
		{"unreadable", 0o000},
		{"unsearchable", 0o444},
		{"write-only", 0o300},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			locked := filepath.Join(f.payload, "locked")
			mkdir(t, filepath.Join(locked, "inner"))
			populate(t, locked, 0, 0, 3)
			write(t, filepath.Join(locked, "inner", "leaf"), "x")
			before := f.snapshot(t, f.payload)
			if err := os.Chmod(locked, tt.mode); err != nil {
				t.Fatalf("chmod: %v", err)
			}
			t.Cleanup(func() { _ = os.Chmod(locked, 0o700) })
			d := f.open(t)

			total, done := 0, false
			for !done {
				n, finished, err := d.Step(100, time.Minute)
				if runtime.GOOS == "linux" && errors.Is(err, unix.EOPNOTSUPP) {
					t.Skip("this kernel has no fchmodat2, so a directory cannot be chmodded without following symlinks")
				}
				if err != nil {
					t.Fatalf("Step: %v", err)
				}
				total, done = total+n, finished
			}

			if total != 7 {
				t.Errorf("removed %d entries, want 7", total)
			}
			f.assertOnlyPayloadGone(t, before)
		})
	}
}

func TestOpenRefusals(t *testing.T) {
	tests := []struct {
		name    string
		arrange func(t *testing.T, f fixture) (dir, name string, want cleanup.FileID)
		wantErr error
	}{
		{
			name: "missing payload",
			arrange: func(t *testing.T, f fixture) (string, string, cleanup.FileID) {
				id, _, err := cleanup.LstatID(f.payload)
				if err != nil {
					t.Fatalf("LstatID: %v", err)
				}
				if err := os.Remove(f.payload); err != nil {
					t.Fatalf("remove: %v", err)
				}
				return f.job, cleanup.PayloadName, id
			},
			wantErr: fs.ErrNotExist,
		},
		{
			name: "missing job folder",
			arrange: func(_ *testing.T, f fixture) (string, string, cleanup.FileID) {
				return filepath.Join(f.root, "absent"), cleanup.PayloadName, cleanup.FileID{Dev: 1, Ino: 1}
			},
			wantErr: fs.ErrNotExist,
		},
		{
			name: "another directory at the payload name",
			arrange: func(t *testing.T, f fixture) (string, string, cleanup.FileID) {
				write(t, filepath.Join(f.payload, "precious"), "precious")
				id, _, err := cleanup.LstatID(f.payload)
				if err != nil {
					t.Fatalf("LstatID: %v", err)
				}
				id.Ino++
				return f.job, cleanup.PayloadName, id
			},
			wantErr: cleanup.ErrIdentity,
		},
		{
			name: "regular file at the payload name",
			arrange: func(t *testing.T, f fixture) (string, string, cleanup.FileID) {
				if err := os.Remove(f.payload); err != nil {
					t.Fatalf("remove: %v", err)
				}
				write(t, f.payload, "precious")
				id, _, err := cleanup.LstatID(f.payload)
				if err != nil {
					t.Fatalf("LstatID: %v", err)
				}
				return f.job, cleanup.PayloadName, id
			},
			wantErr: cleanup.ErrIdentity,
		},
		{
			name: "symlink at the payload name",
			arrange: func(t *testing.T, f fixture) (string, string, cleanup.FileID) {
				if err := os.Remove(f.payload); err != nil {
					t.Fatalf("remove: %v", err)
				}
				symlink(t, filepath.Join(f.outside, "dir"), f.payload)
				id, _, err := cleanup.LstatID(filepath.Join(f.outside, "dir"))
				if err != nil {
					t.Fatalf("LstatID: %v", err)
				}
				return f.job, cleanup.PayloadName, id
			},
			wantErr: cleanup.ErrIdentity,
		},
		{
			name: "symlink as the job folder",
			arrange: func(t *testing.T, f fixture) (string, string, cleanup.FileID) {
				link := filepath.Join(f.root, "job-link")
				symlink(t, f.job, link)
				id, _, err := cleanup.LstatID(f.payload)
				if err != nil {
					t.Fatalf("LstatID: %v", err)
				}
				return link, cleanup.PayloadName, id
			},
			wantErr: unix.ENOTDIR,
		},
		{
			name: "name reaching outside the job folder",
			arrange: func(t *testing.T, f fixture) (string, string, cleanup.FileID) {
				id, _, err := cleanup.LstatID(filepath.Join(f.outside, "dir"))
				if err != nil {
					t.Fatalf("LstatID: %v", err)
				}
				return f.job, "../outside/dir", id
			},
			wantErr: fs.ErrInvalid,
		},
		{
			name: "parent as the name",
			arrange: func(t *testing.T, f fixture) (string, string, cleanup.FileID) {
				id, _, err := cleanup.LstatID(f.root)
				if err != nil {
					t.Fatalf("LstatID: %v", err)
				}
				return f.job, "..", id
			},
			wantErr: fs.ErrInvalid,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			dir, name, want := tt.arrange(t, f)
			before := f.snapshot(t, "")

			d, err := Deleter{}.Open(dir, name, want)

			if d != nil {
				t.Errorf("Open returned a deletion alongside error %v", err)
			}
			if !errors.Is(err, tt.wantErr) {
				t.Errorf("Open error = %v, want one wrapping %v", err, tt.wantErr)
			}
			if after := f.snapshot(t, ""); !maps.Equal(after, before) {
				t.Errorf("a refused Open changed the tree:\n got %v\nwant %v", after, before)
			}
		})
	}
}

func TestInterferenceAtThePayloadRoot(t *testing.T) {
	t.Run("entries removed by someone else", func(t *testing.T) {
		f := newFixture(t)
		populate(t, f.payload, 0, 0, 4)
		before := f.snapshot(t, f.payload)
		d := f.open(t)

		if n, done, err := d.Step(1, time.Minute); n != 1 || done || err != nil {
			t.Fatalf("Step(1) = (%d, %v, %v), want (1, false, nil)", n, done, err)
		}
		entries, err := os.ReadDir(f.payload)
		if err != nil || len(entries) != 3 {
			t.Fatalf("payload holds %d entries (err %v), want 3", len(entries), err)
		}
		for _, entry := range entries {
			if err := os.Remove(filepath.Join(f.payload, entry.Name())); err != nil {
				t.Fatalf("remove: %v", err)
			}
		}
		n, done, err := d.Step(100, time.Minute)

		if n != 1 || !done || err != nil {
			t.Errorf("Step after the entries vanished = (%d, %v, %v), want (1, true, nil)", n, done, err)
		}
		f.assertOnlyPayloadGone(t, before)
	})

	displacements := []struct {
		name     string
		displace func(f fixture) error
	}{
		{"payload removed by someone else", func(f fixture) error { return os.Remove(f.payload) }},
		{"payload renamed away by someone else", func(f fixture) error {
			return os.Rename(f.payload, filepath.Join(f.job, "parked"))
		}},
	}
	for _, tt := range displacements {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			d := f.open(t)
			if err := tt.displace(f); err != nil {
				t.Fatalf("displace: %v", err)
			}
			before := f.snapshot(t, "")

			n, done, err := d.Step(100, time.Minute)

			var failure *Error
			if n != 0 || done || !errors.Is(err, cleanup.ErrIdentity) || !errors.As(err, &failure) {
				t.Fatalf("Step = (%d, %v, %v), want (0, false, error wrapping cleanup.ErrIdentity)", n, done, err)
			}
			if failure.Op != "bind" || failure.Rel != "." {
				t.Errorf("failure = %s %s, want bind .", failure.Op, failure.Rel)
			}
			if errors.Is(err, fs.ErrNotExist) {
				t.Errorf("Step error %v wraps fs.ErrNotExist, which reads as a payload already deleted", err)
			}
			if after := f.snapshot(t, ""); !maps.Equal(after, before) {
				t.Errorf("the tree changed after the payload left its name:\n got %v\nwant %v", after, before)
			}
			assertClosed(t, d)
		})
	}

	t.Run("payload replaced by another directory", func(t *testing.T) {
		f := newFixture(t)
		populate(t, f.payload, 0, 0, 5)
		d := f.open(t)
		if n, done, err := d.Step(3, time.Minute); n != 3 || done || err != nil {
			t.Fatalf("Step(3) = (%d, %v, %v), want (3, false, nil)", n, done, err)
		}
		parked := filepath.Join(f.job, "parked")
		if err := os.Rename(f.payload, parked); err != nil {
			t.Fatalf("rename: %v", err)
		}
		mkdir(t, filepath.Join(f.payload, "inner"))
		write(t, filepath.Join(f.payload, "precious"), "precious")
		write(t, filepath.Join(f.payload, "inner", "leaf"), "leaf")
		if got := countFiles(t, parked); got != 2 {
			t.Fatalf("the parked payload holds %d files, want 2", got)
		}
		before := f.snapshot(t, "")

		n, done, err := d.Step(100, time.Minute)

		var failure *Error
		if n != 0 || done || !errors.Is(err, cleanup.ErrIdentity) || !errors.As(err, &failure) {
			t.Fatalf("Step = (%d, %v, %v), want (0, false, error wrapping cleanup.ErrIdentity)", n, done, err)
		}
		if failure.Op != "bind" || failure.Rel != "." {
			t.Errorf("failure = %s %s, want bind .", failure.Op, failure.Rel)
		}
		if after := f.snapshot(t, ""); !maps.Equal(after, before) {
			t.Errorf("the replacement or the parked payload was touched:\n got %v\nwant %v", after, before)
		}
		assertClosed(t, d)
	})
}

func TestPayloadDisplacedWithinASlice(t *testing.T) {
	tests := []struct {
		name     string
		displace func(t *testing.T, f fixture)
	}{
		{"removed", func(t *testing.T, f fixture) {
			if err := os.Remove(f.payload); err != nil {
				t.Fatalf("remove: %v", err)
			}
		}},
		{"renamed away", func(t *testing.T, f fixture) {
			if err := os.Rename(f.payload, filepath.Join(f.job, "parked")); err != nil {
				t.Fatalf("rename: %v", err)
			}
		}},
		{"replaced by another directory", func(t *testing.T, f fixture) {
			if err := os.Rename(f.payload, filepath.Join(f.job, "parked")); err != nil {
				t.Fatalf("rename: %v", err)
			}
			mkdir(t, f.payload)
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			d := f.open(t).(*deletion)
			tt.displace(t, f)
			before := f.snapshot(t, "")

			n, done, err := d.advance()

			var failure *Error
			if n != 0 || done || !errors.Is(err, cleanup.ErrIdentity) || !errors.As(err, &failure) {
				t.Fatalf("advance = (%d, %v, %v), want (0, false, error wrapping cleanup.ErrIdentity)", n, done, err)
			}
			if failure.Op != "rmdir" || failure.Rel != "." {
				t.Errorf("failure = %s %s, want rmdir .", failure.Op, failure.Rel)
			}
			if errors.Is(err, fs.ErrNotExist) {
				t.Errorf("advance error %v wraps fs.ErrNotExist, which reads as a payload already deleted", err)
			}
			if after := f.snapshot(t, ""); !maps.Equal(after, before) {
				t.Errorf("the tree changed after the payload left its name:\n got %v\nwant %v", after, before)
			}
		})
	}
}

func TestHeldSubdirectoryRenamedOutIsNotDeletedThrough(t *testing.T) {
	tests := []struct {
		name          string
		leaf, moved   string
		limit         int
		budget        time.Duration
		removedBefore int
		heldBefore    int
		removedAfter  int
		survivors     int
	}{
		{name: "the held leaf", leaf: "held", moved: "held", limit: 2, budget: time.Minute, removedBefore: 2, heldBefore: 2, removedAfter: 1, survivors: 3},
		{name: "a held ancestor of the leaf", leaf: "a/b/c", moved: "a/b", limit: 2, budget: time.Minute, removedBefore: 2, heldBefore: 4, removedAfter: 2, survivors: 3},
		{name: "the first held level", leaf: "a/b/c", moved: "a", limit: 2, budget: time.Minute, removedBefore: 2, heldBefore: 4, removedAfter: 1, survivors: 3},
		{name: "a directory entered but not yet emptied", leaf: "held", moved: "held", limit: 100, budget: time.Nanosecond, removedBefore: 0, heldBefore: 2, removedAfter: 1, survivors: 5},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			leaf := filepath.Join(f.payload, filepath.FromSlash(tt.leaf))
			mkdir(t, leaf)
			populate(t, leaf, 0, 0, 5)
			d := f.open(t)
			if n, done, err := d.Step(tt.limit, tt.budget); n != tt.removedBefore || done || err != nil {
				t.Fatalf("Step before the rename = (%d, %v, %v), want (%d, false, nil)", n, done, err, tt.removedBefore)
			}
			if held := len(d.(*deletion).stack); held != tt.heldBefore {
				t.Fatalf("the deletion holds %d levels before the rename, want %d", held, tt.heldBefore)
			}
			rescued := filepath.Join(f.outside, "rescued")
			if err := os.Rename(filepath.Join(f.payload, filepath.FromSlash(tt.moved)), rescued); err != nil {
				t.Fatalf("rename: %v", err)
			}
			before := f.snapshot(t, f.payload)

			n, done, err := d.Step(100, time.Minute)

			if n != tt.removedAfter || !done || err != nil {
				t.Errorf("Step after the rename = (%d, %v, %v), want (%d, true, nil)", n, done, err, tt.removedAfter)
			}
			if got := countFiles(t, rescued); got != tt.survivors {
				t.Errorf("the renamed subtree holds %d files, want %d", got, tt.survivors)
			}
			f.assertOnlyPayloadGone(t, before)
		})
	}
}

func TestHeldSubdirectoryReplacedGetsAFreshDescent(t *testing.T) {
	tests := []struct {
		name          string
		limit         int
		budget        time.Duration
		removedBefore int
		survivors     int
	}{
		{name: "after entries were removed from it", limit: 2, budget: time.Minute, removedBefore: 2, survivors: 3},
		{name: "before anything was removed from it", limit: 100, budget: time.Nanosecond, removedBefore: 0, survivors: 5},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			held := filepath.Join(f.payload, "held")
			mkdir(t, held)
			populate(t, held, 0, 0, 5)
			d := f.open(t)
			if n, done, err := d.Step(tt.limit, tt.budget); n != tt.removedBefore || done || err != nil {
				t.Fatalf("Step before the swap = (%d, %v, %v), want (%d, false, nil)", n, done, err, tt.removedBefore)
			}
			if levels := len(d.(*deletion).stack); levels != 2 {
				t.Fatalf("the deletion holds %d levels before the swap, want 2", levels)
			}
			rescued := filepath.Join(f.outside, "rescued")
			if err := os.Rename(held, rescued); err != nil {
				t.Fatalf("rename: %v", err)
			}
			mkdir(t, filepath.Join(held, "inner"))
			populate(t, held, 0, 0, 4)
			write(t, filepath.Join(held, "inner", "leaf"), "x")
			before := f.snapshot(t, f.payload)

			n, done, err := d.Step(100, time.Minute)

			if n != 8 || !done || err != nil {
				t.Errorf("Step after the swap = (%d, %v, %v), want (8, true, nil)", n, done, err)
			}
			if got := countFiles(t, rescued); got != tt.survivors {
				t.Errorf("the moved-away original holds %d files, want %d", got, tt.survivors)
			}
			f.assertOnlyPayloadGone(t, before)
		})
	}
}

func TestPayloadRenamedOutOfTheJobFolderIsNotDeletedThrough(t *testing.T) {
	f := newFixture(t)
	write(t, filepath.Join(f.payload, "private.txt"), "rescued\n")
	d := f.open(t)
	rescued := filepath.Join(f.outside, "rescued")
	if err := os.Rename(f.payload, rescued); err != nil {
		t.Fatalf("rename: %v", err)
	}
	before := f.snapshot(t, "")

	n, done, err := d.Step(100, time.Second)

	var failure *Error
	if n != 0 || done || !errors.Is(err, cleanup.ErrIdentity) || !errors.As(err, &failure) {
		t.Fatalf("Step = (%d, %v, %v), want (0, false, error wrapping cleanup.ErrIdentity)", n, done, err)
	}
	if failure.Op != "bind" || failure.Rel != "." {
		t.Errorf("failure = %s %s, want bind .", failure.Op, failure.Rel)
	}
	if errors.Is(err, fs.ErrNotExist) {
		t.Errorf("Step error %v wraps fs.ErrNotExist, which reads as a payload already deleted", err)
	}
	if got, err := os.ReadFile(filepath.Join(rescued, "private.txt")); err != nil || string(got) != "rescued\n" {
		t.Errorf("rescued/private.txt = (%q, %v), want it intact", got, err)
	}
	if after := f.snapshot(t, ""); !maps.Equal(after, before) {
		t.Errorf("the tree changed after the payload left the job folder:\n got %v\nwant %v", after, before)
	}
	assertClosed(t, d)
}

func TestJobFolderDisplacedBetweenStepsIsNotDeletedThrough(t *testing.T) {
	tests := []struct {
		name     string
		displace func(t *testing.T, jobs, job, rescued string)
		payload  string
	}{
		{
			name: "the job folder renamed away",
			displace: func(t *testing.T, _, job, rescued string) {
				if err := os.Rename(job, rescued); err != nil {
					t.Fatalf("rename: %v", err)
				}
			},
			payload: cleanup.PayloadName,
		},
		{
			name: "the job folder replaced by another directory",
			displace: func(t *testing.T, _, job, rescued string) {
				if err := os.Rename(job, rescued); err != nil {
					t.Fatalf("rename: %v", err)
				}
				mkdir(t, filepath.Join(job, cleanup.PayloadName))
				write(t, filepath.Join(job, cleanup.PayloadName, "precious"), "precious")
			},
			payload: cleanup.PayloadName,
		},
		{
			name: "the job folder renamed away behind a symlink at its path",
			displace: func(t *testing.T, _, job, rescued string) {
				if err := os.Rename(job, rescued); err != nil {
					t.Fatalf("rename: %v", err)
				}
				symlink(t, rescued, job)
			},
			payload: cleanup.PayloadName,
		},
		{
			name: "the directory holding the job folder renamed away",
			displace: func(t *testing.T, jobs, _, rescued string) {
				if err := os.Rename(jobs, rescued); err != nil {
					t.Fatalf("rename: %v", err)
				}
			},
			payload: filepath.Join("id", cleanup.PayloadName),
		},
		{
			name: "the directory holding the job folder replaced by a file",
			displace: func(t *testing.T, jobs, _, rescued string) {
				if err := os.Rename(jobs, rescued); err != nil {
					t.Fatalf("rename: %v", err)
				}
				write(t, jobs, "precious")
			},
			payload: filepath.Join("id", cleanup.PayloadName),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			jobs := filepath.Join(f.root, "jobs")
			f.job = filepath.Join(jobs, "id")
			f.payload = filepath.Join(f.job, cleanup.PayloadName)
			mkdir(t, f.payload)
			populate(t, f.payload, 0, 0, 5)
			d := f.open(t)
			if n, done, err := d.Step(2, time.Minute); n != 2 || done || err != nil {
				t.Fatalf("Step before the displacement = (%d, %v, %v), want (2, false, nil)", n, done, err)
			}
			rescued := filepath.Join(f.outside, "rescued")
			tt.displace(t, jobs, f.job, rescued)
			before := f.snapshot(t, "")

			n, done, err := d.Step(100, time.Minute)

			var failure *Error
			if n != 0 || done || !errors.Is(err, cleanup.ErrIdentity) || !errors.As(err, &failure) {
				t.Fatalf("Step = (%d, %v, %v), want (0, false, error wrapping cleanup.ErrIdentity)", n, done, err)
			}
			if failure.Op != "bind" || failure.Rel != ".." {
				t.Errorf("failure = %s %s, want bind ..", failure.Op, failure.Rel)
			}
			if errors.Is(err, fs.ErrNotExist) {
				t.Errorf("Step error %v wraps fs.ErrNotExist, which reads as a payload already deleted", err)
			}
			if got := countFiles(t, filepath.Join(rescued, tt.payload)); got != 3 {
				t.Errorf("the moved payload holds %d files, want 3", got)
			}
			if after := f.snapshot(t, ""); !maps.Equal(after, before) {
				t.Errorf("the tree changed after the job folder left its path:\n got %v\nwant %v", after, before)
			}
			assertClosed(t, d)
		})
	}
}

func TestBudgetExpiryReturnsPartialProgress(t *testing.T) {
	f := newFixture(t)
	populate(t, f.payload, 0, 0, 2000)
	before := f.snapshot(t, f.payload)
	d := f.open(t)

	n, done, err := d.Step(1<<20, time.Nanosecond)
	if n != 1 || done || err != nil {
		t.Fatalf("Step with a 1ns budget = (%d, %v, %v), want (1, false, nil)", n, done, err)
	}
	entries, err := os.ReadDir(f.payload)
	if err != nil || len(entries) != 1999 {
		t.Fatalf("payload holds %d entries (err %v) after one entry's budget, want 1999", len(entries), err)
	}
	total, _ := drain(t, d, 500)

	if total != 2000 {
		t.Errorf("removed %d entries after the budgeted step, want 2000", total)
	}
	f.assertOnlyPayloadGone(t, before)
}

func TestReopenAfterCloseFinishesTheRest(t *testing.T) {
	f := newFixture(t)
	populate(t, f.payload, 3, 3, 4)
	before := f.snapshot(t, f.payload)
	d := f.open(t)

	if n, done, err := d.Step(50, time.Minute); n != 50 || done || err != nil {
		t.Fatalf("Step(50) = (%d, %v, %v), want (50, false, nil)", n, done, err)
	}
	if err := d.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	assertClosed(t, d)
	total, _ := drain(t, f.open(t), 100)

	if total != 150 {
		t.Errorf("reopened deletion removed %d entries, want 150", total)
	}
	f.assertOnlyPayloadGone(t, before)
}

func TestEntriesCreatedDuringDeletionAreRemoved(t *testing.T) {
	const initial, late = 600, 300
	f := newFixture(t)
	populate(t, f.payload, 0, 0, initial)
	before := f.snapshot(t, f.payload)
	d := f.open(t)

	type outcome struct {
		created int
		err     error
	}
	started := make(chan struct{})
	finished := make(chan outcome, 1)
	go func() {
		var out outcome
		for i := range late {
			err := os.WriteFile(filepath.Join(f.payload, fmt.Sprintf("late-%d", i)), nil, 0o644)
			switch {
			case err == nil:
				out.created++
			case !errors.Is(err, fs.ErrNotExist):
				out.err = err
			}
			if i == 0 {
				close(started)
			}
		}
		finished <- out
	}()
	<-started
	total, _ := drain(t, d, 5)
	out := <-finished

	if out.err != nil {
		t.Fatalf("concurrent create: %v", out.err)
	}
	if out.created < 1 || out.created > late {
		t.Fatalf("created %d late entries, want between 1 and %d", out.created, late)
	}
	if want := initial + out.created + 1; total != want {
		t.Errorf("removed %d entries, want %d (%d initial, %d created meanwhile, the root)", total, want, initial, out.created)
	}
	f.assertOnlyPayloadGone(t, before)
}

func TestSpecialEntriesAndAwkwardNames(t *testing.T) {
	f := newFixture(t)
	names := []string{"new\nline", "tab\there", "ünïcödé ☃ 雪", "-rf", " leading space"}
	for _, name := range names {
		write(t, filepath.Join(f.payload, name), "x")
	}
	mkdir(t, filepath.Join(f.payload, "dir\nwith newline"))
	write(t, filepath.Join(f.payload, "dir\nwith newline", "in\nner"), "x")
	if err := unix.Mkfifo(filepath.Join(f.payload, "fifo"), 0o644); err != nil {
		t.Fatalf("mkfifo: %v", err)
	}
	sock, err := unix.Socket(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		t.Fatalf("socket: %v", err)
	}
	t.Chdir(f.job)
	if err := unix.Bind(sock, &unix.SockaddrUnix{Name: cleanup.PayloadName + "/sock"}); err != nil {
		t.Fatalf("bind: %v", err)
	}
	if err := unix.Close(sock); err != nil {
		t.Fatalf("close: %v", err)
	}
	for name, want := range map[string]fs.FileMode{"fifo": fs.ModeNamedPipe, "sock": fs.ModeSocket} {
		info, err := os.Lstat(filepath.Join(f.payload, name))
		if err != nil || info.Mode().Type() != want {
			t.Fatalf("Lstat(%s) = (%v, %v), want type %v", name, info, err, want)
		}
	}
	before := f.snapshot(t, f.payload)

	total, _ := drain(t, f.open(t), 3)

	if total != 10 {
		t.Errorf("removed %d entries, want 10", total)
	}
	f.assertOnlyPayloadGone(t, before)
}

func TestLargeDirectoryLosesNoEntry(t *testing.T) {
	f := newFixture(t)
	populate(t, f.payload, 0, 0, 10_000)
	before := f.snapshot(t, f.payload)

	total, steps := drain(t, f.open(t), 100)

	if total != 10_001 {
		t.Errorf("removed %d entries, want 10001", total)
	}
	if steps != 101 {
		t.Errorf("took %d steps, want 101", steps)
	}
	f.assertOnlyPayloadGone(t, before)
}

func TestDepthLimit(t *testing.T) {
	t.Run("at the limit", func(t *testing.T) {
		f := newFixture(t)
		nest(t, f.payload, MaxDepth-1)
		before := f.snapshot(t, f.payload)

		total, _ := drain(t, f.open(t), 100)

		if total != MaxDepth {
			t.Errorf("removed %d entries, want %d", total, MaxDepth)
		}
		f.assertOnlyPayloadGone(t, before)
	})

	t.Run("past the limit", func(t *testing.T) {
		f := newFixture(t)
		nest(t, f.payload, MaxDepth)
		before := f.snapshot(t, f.payload)
		d := f.open(t)

		n, done, err := d.Step(100, time.Minute)

		var failure *Error
		if n != 0 || done || !errors.Is(err, ErrDepth) || !errors.As(err, &failure) {
			t.Fatalf("Step = (%d, %v, %v), want (0, false, error wrapping ErrDepth)", n, done, err)
		}
		if want := strings.TrimSuffix(strings.Repeat("d/", MaxDepth), "/"); failure.Op != "descend" || failure.Rel != want {
			t.Errorf("failure = %s at a %d-byte path, want descend at the %d-level chain", failure.Op, len(failure.Rel), MaxDepth)
		}
		assertClosed(t, d)
		if after := f.snapshot(t, f.payload); !maps.Equal(after, before) {
			t.Errorf("tree outside the payload changed:\n got %v\nwant %v", after, before)
		}
		var st unix.Stat_t
		fd, err := unix.Open(f.payload, dirFlags, 0)
		if err != nil {
			t.Fatalf("open payload: %v", err)
		}
		if err := unix.Fstatat(fd, "d", &st, unix.AT_SYMLINK_NOFOLLOW); err != nil || !isDir(&st) {
			t.Errorf("the refused chain's first level is gone: %v", err)
		}
		if err := unix.Close(fd); err != nil {
			t.Fatalf("close: %v", err)
		}
	})
}

func TestPassWithoutProgressIsStuck(t *testing.T) {
	f := newFixture(t)
	write(t, filepath.Join(f.payload, "pinned"), "pinned")
	before := f.snapshot(t, "")
	d := f.open(t).(*deletion)
	top := d.stack[0]

	name, ok, err := d.next(top)
	if name != "pinned" || !ok || err != nil {
		t.Fatalf("first next = (%q, %v, %v), want (pinned, true, nil)", name, ok, err)
	}
	name, ok, err = d.next(top)

	var failure *Error
	if name != "" || ok || !errors.Is(err, ErrStuck) || !errors.As(err, &failure) {
		t.Fatalf("next after an unproductive pass = (%q, %v, %v), want an error wrapping ErrStuck", name, ok, err)
	}
	if failure.Op != "read" || failure.Rel != "." {
		t.Errorf("failure = %s %s, want read .", failure.Op, failure.Rel)
	}
	if after := f.snapshot(t, ""); !maps.Equal(after, before) {
		t.Errorf("a stuck pass changed the tree:\n got %v\nwant %v", after, before)
	}
}

func TestEntryGoneAfterPassWithoutProgressIsNotStuck(t *testing.T) {
	f := newFixture(t)
	write(t, filepath.Join(f.payload, "fleeting"), "fleeting")
	before := f.snapshot(t, f.payload)
	d := f.open(t).(*deletion)

	name, ok, err := d.next(d.stack[0])
	if name != "fleeting" || !ok || err != nil {
		t.Fatalf("first next = (%q, %v, %v), want (fleeting, true, nil)", name, ok, err)
	}
	if err := os.Remove(filepath.Join(f.payload, "fleeting")); err != nil {
		t.Fatalf("remove: %v", err)
	}
	n, done, err := d.Step(100, time.Minute)

	if n != 1 || !done || err != nil {
		t.Errorf("Step after the unvisited entry vanished = (%d, %v, %v), want (1, true, nil)", n, done, err)
	}
	f.assertOnlyPayloadGone(t, before)
}

func TestNameListedButAbsentOnEveryPassIsStuck(t *testing.T) {
	f := newFixture(t)
	mkdir(t, filepath.Join(f.payload, "sub"))
	listing := filepath.Join(f.outside, "listing")
	mkdir(t, listing)
	write(t, filepath.Join(listing, "ghost"), "ghost")
	d := f.open(t).(*deletion)
	if n, done, err := d.advance(); n != 0 || done || err != nil || len(d.stack) != 2 {
		t.Fatalf("advance into sub = (%d, %v, %v) holding %d levels, want (0, false, nil) holding 2", n, done, err, len(d.stack))
	}
	held := d.stack[1]
	emptied := held.dir
	t.Cleanup(func() {
		if err := emptied.Close(); err != nil {
			t.Errorf("close sub: %v", err)
		}
	})
	ghosts, err := os.Open(listing)
	if err != nil {
		t.Fatalf("open listing: %v", err)
	}
	held.dir = ghosts
	before := f.snapshot(t, "")

	n, done, err := d.Step(100, 5*time.Second)

	var failure *Error
	if n != 0 || done || !errors.Is(err, ErrStuck) || !errors.As(err, &failure) {
		t.Fatalf("Step = (%d, %v, %v), want (0, false, error wrapping ErrStuck)", n, done, err)
	}
	if failure.Op != "read" || failure.Rel != "sub" {
		t.Errorf("failure = %s %s, want read sub", failure.Op, failure.Rel)
	}
	if after := f.snapshot(t, ""); !maps.Equal(after, before) {
		t.Errorf("a stuck directory changed the tree:\n got %v\nwant %v", after, before)
	}
	assertClosed(t, d)
}

// TestRefuseMount covers the device comparison alone: staging a nested mount
// needs root and a second volume, which no hermetic test has.
func TestRefuseMount(t *testing.T) {
	tests := []struct {
		name        string
		root, child uint64
		rel         string
		wantMount   bool
	}{
		{"same device", 16777234, 16777234, "a/b", false},
		{"another device", 16777234, 16777240, "a/b", true},
		{"device zero under a real one", 16777234, 0, "vendor", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := refuseMount(tt.root, tt.child, tt.rel)

			if !tt.wantMount {
				if err != nil {
					t.Fatalf("refuseMount = %v, want nil", err)
				}
				return
			}
			var failure *Error
			if !errors.Is(err, ErrMount) || !errors.As(err, &failure) {
				t.Fatalf("refuseMount = %v, want an *Error wrapping ErrMount", err)
			}
			if failure.Op != "descend" || failure.Rel != tt.rel {
				t.Errorf("failure = %s %s, want descend %s", failure.Op, failure.Rel, tt.rel)
			}
		})
	}
}

func TestErrorMessageAndCause(t *testing.T) {
	err := error(&Error{Op: "unlink", Rel: "a/b", Err: unix.EACCES})

	if got, want := err.Error(), "rmtree: unlink a/b: permission denied"; got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
	if !errors.Is(err, fs.ErrPermission) {
		t.Errorf("errors.Is(%v, fs.ErrPermission) = false, want true", err)
	}
}
