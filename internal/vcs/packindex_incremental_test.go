package vcs

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/yasyf/cc-context/internal/render"
	"github.com/yasyf/cc-context/internal/vcstest"
)

func TestWritePackIndexIncrementalActualGit(t *testing.T) {
	f := vcstest.Repo(t)
	runGit(t, f, f.Dir, "repack", "-ad")
	before, err := CapturePackIndexes(f.Context(), render.Dir(f.Dir))
	if err != nil {
		t.Fatal(err)
	}
	runGit(t, f, f.Dir, "multi-pack-index", "write", "--no-bitmap")
	flat := filepath.Join(before.objectDir, "pack", "multi-pack-index")
	baseInfo, err := os.Stat(flat)
	if err != nil {
		t.Fatal(err)
	}
	baseBytes, err := os.ReadFile(flat)
	if err != nil {
		t.Fatal(err)
	}
	runGit(t, f, f.Dir, "commit", "--allow-empty", "-m", "delta")
	head := strings.TrimSpace(f.Out(t, "git", "rev-parse", "HEAD"))
	runGit(t, f, f.Dir, "repack", "-d")
	after, err := CapturePackIndexes(f.Context(), render.Dir(f.Dir))
	if err != nil {
		t.Fatal(err)
	}
	added := slices.DeleteFunc(slices.Clone(after.indexes), func(name string) bool {
		return slices.Contains(before.indexes, name)
	})
	if len(added) != 1 {
		t.Fatalf("new indexes = %q, want one delta pack", added)
	}
	if err := writePackIndex(f.Context(), render.Dir(f.Dir), before.objectDir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(flat); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("standalone MIDX after migration = %v", err)
	}
	chainDir := filepath.Join(before.objectDir, "pack", "multi-pack-index.d")
	chainPath := filepath.Join(chainDir, "multi-pack-index-chain")
	chainBytes, err := os.ReadFile(chainPath)
	if err != nil {
		t.Fatal(err)
	}
	layers := strings.Fields(string(chainBytes))
	if len(layers) != 2 {
		t.Fatalf("MIDX chain = %q, want reused base and one delta", layers)
	}
	basePath := filepath.Join(chainDir, "multi-pack-index-"+layers[0]+".midx")
	retainedInfo, err := os.Stat(basePath)
	if err != nil {
		t.Fatal(err)
	}
	retainedBytes, err := os.ReadFile(basePath)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(baseInfo, retainedInfo) || !bytes.Equal(baseBytes, retainedBytes) {
		t.Fatal("incremental migration replaced the existing base payload")
	}
	deltaPath := filepath.Join(chainDir, "multi-pack-index-"+layers[1]+".midx")
	deltaNames, err := readMIDXFile(f.Context(), deltaPath)
	if err != nil || !slices.Equal(deltaNames, added) {
		t.Fatalf("delta pack names = %q, %v; want %q", deltaNames, err, added)
	}
	covered, err := coveredPackIndexes(f.Context(), before.objectDir)
	if err != nil || len(covered) != len(after.indexes) {
		t.Fatalf("chain coverage = %v, %v; want %q", covered, err, after.indexes)
	}
	for _, name := range after.indexes {
		if _, found := covered[name]; !found {
			t.Fatalf("chain missing index %q", name)
		}
	}
	if typ := strings.TrimSpace(f.Out(t, "git", "cat-file", "-t", head)); typ != "commit" {
		t.Fatalf("delta object type = %q", typ)
	}
	paths := []string{chainPath, basePath, deltaPath}
	infos := make([]os.FileInfo, len(paths))
	payloads := make([][]byte, len(paths))
	for i, path := range paths {
		infos[i], err = os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		payloads[i], err = os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := writePackIndex(f.Context(), render.Dir(f.Dir), before.objectDir); err != nil {
		t.Fatal(err)
	}
	for i, path := range paths {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		payload, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if !os.SameFile(infos[i], info) || !infos[i].ModTime().Equal(info.ModTime()) || !bytes.Equal(payloads[i], payload) {
			t.Fatalf("no-op incremental write changed %s", filepath.Base(path))
		}
	}
}

func TestPackIndexPublishBelowFloorActualGit(t *testing.T) {
	f := vcstest.Repo(t)
	runGit(t, f, f.Dir, "repack", "-ad")
	runGit(t, f, f.Dir, "multi-pack-index", "write", "--no-bitmap")
	before, err := CapturePackIndexes(f.Context(), render.Dir(f.Dir))
	if err != nil {
		t.Fatal(err)
	}
	flat := filepath.Join(before.objectDir, "pack", "multi-pack-index")
	info, err := os.Stat(flat)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := os.ReadFile(flat)
	if err != nil {
		t.Fatal(err)
	}
	for _, add := range []bool{false, true} {
		if add {
			runGit(t, f, f.Dir, "commit", "--allow-empty", "-m", "below floor")
			runGit(t, f, f.Dir, "repack", "-d")
			current, err := CapturePackIndexes(f.Context(), render.Dir(f.Dir))
			if err != nil || len(current.indexes) <= len(before.indexes) {
				t.Fatalf("new complete indexes = %q, %v; before %q", current.indexes, err, before.indexes)
			}
		}
		if err := before.Publish(f.Context(), render.Dir(f.Dir)); err != nil {
			t.Fatal(err)
		}
		afterInfo, err := os.Stat(flat)
		if err != nil {
			t.Fatal(err)
		}
		afterPayload, err := os.ReadFile(flat)
		if err != nil {
			t.Fatal(err)
		}
		if !os.SameFile(info, afterInfo) || !info.ModTime().Equal(afterInfo.ModTime()) || !bytes.Equal(payload, afterPayload) {
			t.Fatalf("publication below the existing floor changed the flat MIDX; new pack = %t", add)
		}
		if _, err := os.Stat(filepath.Join(before.objectDir, "pack", "multi-pack-index.d", "multi-pack-index-chain")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("publication below the existing floor created a chain; new pack = %t, error = %v", add, err)
		}
	}
}
