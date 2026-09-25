package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/yasyf/cc-context/internal/render"
	"github.com/yasyf/cc-context/internal/vcstest"
)

func publishedSparseParent(t *testing.T) (*vcstest.Fixture, *stackPublication) {
	t.Helper()
	f := stackRebaseRepo(t, "parent")
	writeShipFile(t, f.Dir, "keep/parent.txt", "parent contents\n")
	writeShipFile(t, f.Dir, "excluded/large.txt", strings.Repeat("excluded\n", 1024))
	mustRun(t, f.Env(), f.Dir, "git", "add", "keep", "excluded")
	mustRun(t, f.Env(), f.Dir, "git", "commit", "-qm", "parent files")
	stackAdvanceTrunk(t, f, "upstream.txt", "new trunk\n")
	if _, _, err := runStackCmd(t, f, "submit"); err != nil {
		t.Fatal(err)
	}
	receipt, err := stackReadPublication(f.Context(), render.Dir(f.Dir), "parent")
	if err != nil || receipt == nil {
		t.Fatalf("publication = %#v, %v", receipt, err)
	}
	if receipt.Source == receipt.Head {
		t.Fatal("fixture must have distinct source and published parent")
	}
	mustRun(t, f.Env(), f.Dir, "git", "sparse-checkout", "set", "--no-cone", "/keep/")
	return f, receipt
}

func TestStackNewPublishedParentInheritsSparseBeforeCheckout(t *testing.T) {
	f, receipt := publishedSparseParent(t)
	index := gitAt(t, f.Env(), f.Dir, "rev-parse", "--path-format=absolute", "--git-path", "index")
	before, err := os.ReadFile(index)
	if err != nil {
		t.Fatal(err)
	}
	patterns := gitAt(t, f.Env(), f.Dir, "rev-parse", "--path-format=absolute", "--git-path", "info/sparse-checkout")
	wantPatterns, err := os.ReadFile(patterns)
	if err != nil {
		t.Fatal(err)
	}
	child := filepath.Join(t.TempDir(), "child")
	shipResetLog(t, f)
	if _, _, err := runStackCmd(t, f, "new", "child", "--parent", "parent", "--published-parent", "--sparse", "--path", child); err != nil {
		t.Fatal(err)
	}
	if got := gitAt(t, f.Env(), child, "rev-parse", "HEAD"); got != receipt.Head {
		t.Fatalf("child started at %s, want publication %s", got, receipt.Head)
	}
	if got := shipHead(t, f); got != receipt.Source {
		t.Fatal("source branch moved")
	}
	after, err := os.ReadFile(index)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("source index changed: %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(child, "keep", "parent.txt")); err != nil || string(got) != "parent contents\n" {
		t.Fatalf("included file = %q, %v", got, err)
	}
	for _, file := range []string{"excluded/large.txt", "upstream.txt", "parent.txt"} {
		if _, err := os.Stat(filepath.Join(child, file)); !os.IsNotExist(err) {
			t.Fatalf("excluded %s materialized: %v", file, err)
		}
	}
	childPatterns := gitAt(t, f.Env(), child, "rev-parse", "--path-format=absolute", "--git-path", "info/sparse-checkout")
	if got, err := os.ReadFile(childPatterns); err != nil || !bytes.Equal(got, wantPatterns) {
		t.Fatalf("patterns changed: %q %v", got, err)
	}
	if got, err := os.ReadFile(patterns); err != nil || !bytes.Equal(got, wantPatterns) {
		t.Fatalf("caller patterns changed: %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(f.Dir, "keep", "parent.txt")); err != nil || string(got) != "parent contents\n" {
		t.Fatalf("caller files changed: %q %v", got, err)
	}
	state, err := gtStateQuery(f.ContextIn(child), render.Dir(child), "test")
	if err != nil {
		t.Fatal(err)
	}
	if parents := state["child"].Parents; len(parents) != 1 || parents[0].Ref != "parent" || parents[0].SHA != receipt.Head {
		t.Fatalf("child boundary = %#v", parents)
	}
	created, populated := false, false
	for _, argv := range vcstest.Invocations(t, f.ArgvLog) {
		if len(argv) > 1 && argv[0] == "git" && argv[1] == "fetch" {
			t.Fatalf("published-parent validation fetched after observing the remote: %v", argv)
		}
		if len(argv) > 2 && argv[0] == "git" && argv[1] == "worktree" && argv[2] == "add" {
			created = true
			if !slices.Contains(argv, "--no-checkout") || argv[len(argv)-1] != receipt.Head {
				t.Fatalf("broad or unpinned creation: %v", argv)
			}
		}
		if len(argv) > 1 && argv[0] == "git" && slices.Contains(argv, "read-tree") {
			populated = true
			if !created {
				t.Fatal("populated before no-checkout creation")
			}
		}
	}
	if !created || !populated {
		t.Fatal("missing bounded creation/population steps")
	}
	writeShipFile(t, child, "keep/child.txt", "child work\n")
	mustRun(t, f.Env(), child, "git", "add", "keep/child.txt")
	mustRun(t, f.Env(), child, "git", "commit", "-qm", "child work")
	if _, _, err := runStackCmdIn(t, f, child, "submit"); err != nil {
		t.Fatal(err)
	}
	published := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "child")
	if count := gitAt(t, f.Env(), child, "rev-list", "--count", receipt.Head+".."+published); count != "1" {
		t.Fatalf("child publication included %s commits", count)
	}
	if files := gitAt(t, f.Env(), child, "diff", "--name-only", receipt.Head+".."+published); files != "keep/child.txt" {
		t.Fatalf("child publication files = %q", files)
	}
	if got := gitAt(t, f.Env(), f.Dir, "rev-parse", "parent"); got != receipt.Source {
		t.Fatal("child publication moved parent source")
	}
}

func TestStackNewPublishedParentNoCheckout(t *testing.T) {
	f, receipt := publishedSparseParent(t)
	child := filepath.Join(t.TempDir(), "child")
	if _, _, err := runStackCmd(t, f, "new", "child", "--published-parent", "--no-checkout", "--path", child); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(child)
	if err != nil || len(entries) != 1 || entries[0].Name() != ".git" {
		t.Fatalf("no-checkout child materialized: %v %v", entries, err)
	}
	if head := gitAt(t, f.Env(), child, "rev-parse", "HEAD"); head != receipt.Head {
		t.Fatal("unmaterialized child has wrong head")
	}
}

func TestStackNewPublishedParentRefusesChangedIdentities(t *testing.T) {
	for _, change := range []string{"source", "remote", "receipt"} {
		t.Run(change, func(t *testing.T) {
			f, receipt := publishedSparseParent(t)
			child := filepath.Join(t.TempDir(), "child")
			switch change {
			case "source":
				mustRun(t, f.Env(), f.Dir, "git", "commit", "--allow-empty", "-qm", "new unpublished source")
			case "remote":
				foreign := gitAt(t, f.Env(), f.Dir, "commit-tree", receipt.Head+"^{tree}", "-p", receipt.Head, "-m", "foreign remote")
				mustRun(t, f.Env(), f.Dir, "git", "push", "-q", "origin", foreign+":refs/heads/parent")
			case "receipt":
				gitBin := shipDisplaceShim(t, f, "git")
				writeShipExecutable(t, f.ShimBin, "git", "#!/bin/sh\nif [ -z \"$CCX_SHIM_DEPTH\" ] && [ \"$1\" = worktree ] && [ \"$2\" = add ]; then\n  '"+gitBin+"' \"$@\" || exit $?\n  CCX_SHIM_DEPTH=1 git update-ref '"+stackPublicationRef("parent", "receipt")+"' '"+receipt.Source+"' '"+receipt.OID+"' || exit $?\n  exit 0\nfi\nexec '"+gitBin+"' \"$@\"\n")
			}
			source := shipHead(t, f)
			_, _, err := runStackCmd(t, f, "new", "child", "--published-parent", "--no-checkout", "--path", child)
			if err == nil {
				t.Fatal("changed parent identity accepted")
			}
			if got := shipHead(t, f); got != source {
				t.Fatal("refusal changed source")
			}
			_, childErr := os.Stat(child)
			present, refErr := gitRefExists(f.Context(), render.Dir(f.Dir), "test", "refs/heads/child")
			if change == "receipt" {
				if childErr != nil || refErr != nil || !present || !strings.Contains(err.Error(), "incomplete") {
					t.Fatalf("incomplete child was discarded: %v %v %v", childErr, refErr, err)
				}
			} else if !os.IsNotExist(childErr) || refErr != nil || present {
				t.Fatalf("pre-creation refusal left artifacts: %v %v %v", childErr, refErr, present)
			}
		})
	}
}

func TestStackNewCreationModesRefuseBeforeMutation(t *testing.T) {
	f := stackRebaseRepo(t, "parent")
	for _, args := range [][]string{
		{"--published-parent"},
		{"--sparse"},
		{"--sparse", "--no-checkout"},
		{"--path", f.Dir},
		{"--path", filepath.Join(f.Dir, "nested")},
	} {
		if _, _, err := runStackCmd(t, f, append([]string{"new", "child"}, args...)...); err == nil {
			t.Fatalf("invalid mode accepted: %v", args)
		}
		if present, err := gitRefExists(f.Context(), render.Dir(f.Dir), "test", "refs/heads/child"); err != nil || present {
			t.Fatalf("invalid mode created branch: %v %v", present, err)
		}
	}
}

func TestStackNewPublishedParentPreservesConcurrentSourceMove(t *testing.T) {
	f, receipt := publishedSparseParent(t)
	changed := gitAt(t, f.Env(), f.Dir, "commit-tree", receipt.Source+"^{tree}", "-p", receipt.Source, "-m", "concurrent source")
	child := filepath.Join(t.TempDir(), "child")
	gitBin := shipDisplaceShim(t, f, "git")
	writeShipExecutable(t, f.ShimBin, "git", "#!/bin/sh\nif [ -z \"$CCX_SHIM_DEPTH\" ] && [ \"$1\" = worktree ] && [ \"$2\" = add ]; then\n  '"+gitBin+"' \"$@\" || exit $?\n  CCX_SHIM_DEPTH=1 git update-ref refs/heads/parent '"+changed+"' '"+receipt.Source+"' || exit $?\n  exit 0\nfi\nexec '"+gitBin+"' \"$@\"\n")
	_, _, err := runStackCmd(t, f, "new", "child", "--published-parent", "--no-checkout", "--path", child)
	if err == nil || !strings.Contains(err.Error(), "source changed") {
		t.Fatalf("source race accepted: %v", err)
	}
	if got := shipHead(t, f); got != changed {
		t.Fatal("concurrent source ref overwritten")
	}
	if _, err := os.Stat(child); err != nil {
		t.Fatalf("incomplete child was deleted: %v", err)
	}
	if !strings.Contains(err.Error(), child) || !strings.Contains(err.Error(), "incomplete") {
		t.Fatalf("missing recovery path: %v", err)
	}
}

func TestStackNewFailurePreservesConcurrentChildWork(t *testing.T) {
	for _, kind := range []string{"dirty file", "advanced ref"} {
		t.Run(kind, func(t *testing.T) {
			f, receipt := publishedSparseParent(t)
			child := filepath.Join(t.TempDir(), "child")
			parentHead := gitAt(t, f.Env(), f.Dir, "commit-tree", receipt.Source+"^{tree}", "-p", receipt.Source, "-m", "concurrent parent")
			childHead := receipt.Head
			mutation := "printf 'concurrent child\\n' > '" + filepath.Join(child, "note.txt") + "'\n"
			if kind == "advanced ref" {
				childHead = gitAt(t, f.Env(), f.Dir, "commit-tree", receipt.Head+"^{tree}", "-p", receipt.Head, "-m", "concurrent child")
				mutation = "CCX_SHIM_DEPTH=1 git update-ref refs/heads/child '" + childHead + "' '" + receipt.Head + "' || exit $?\n"
			}
			gtShim := shipDisplaceShim(t, f, "gt")
			writeShipExecutable(t, f.ShimBin, "gt", "#!/bin/sh\nif [ -z \"$CCX_SHIM_DEPTH\" ] && [ \"$1\" = track ]; then\n  '"+gtShim+"' \"$@\" || exit $?\n"+mutation+"  CCX_SHIM_DEPTH=1 git update-ref refs/heads/parent '"+parentHead+"' '"+receipt.Source+"' || exit $?\n  exit 0\nfi\nexec '"+gtShim+"' \"$@\"\n")
			_, _, err := runStackCmd(t, f, "new", "child", "--published-parent", "--no-checkout", "--path", child)
			if err == nil || !strings.Contains(err.Error(), "incomplete") || !strings.Contains(err.Error(), child) {
				t.Fatalf("missing incomplete-child error: %v", err)
			}
			if got := gitAt(t, f.Env(), child, "rev-parse", "HEAD"); got != childHead {
				t.Fatal("concurrent child commit lost")
			}
			if kind == "dirty file" {
				if data, err := os.ReadFile(filepath.Join(child, "note.txt")); err != nil || string(data) != "concurrent child\n" {
					t.Fatalf("concurrent child file lost: %q %v", data, err)
				}
			}
			state, err := gtStateQuery(f.ContextIn(child), render.Dir(child), "test")
			if err != nil {
				t.Fatal(err)
			}
			if _, ok := state["child"]; !ok {
				t.Fatal("concurrent child metadata forgotten")
			}
		})
	}
}

func TestStackNewSparseNeverOverwritesConcurrentChildFile(t *testing.T) {
	f, receipt := publishedSparseParent(t)
	child := filepath.Join(t.TempDir(), "child")
	gitShim := shipDisplaceShim(t, f, "git")
	writeShipExecutable(t, f.ShimBin, "git", "#!/bin/sh\nif [ -z \"$CCX_SHIM_DEPTH\" ] && [ \"$1\" = worktree ] && [ \"$2\" = add ]; then\n  '"+gitShim+"' \"$@\" || exit $?\n  mkdir -p '"+filepath.Join(child, "keep")+"'\n  printf 'concurrent child\\n' > '"+filepath.Join(child, "keep", "parent.txt")+"'\n  exit 0\nfi\nexec '"+gitShim+"' \"$@\"\n")
	_, _, err := runStackCmd(t, f, "new", "child", "--published-parent", "--sparse", "--path", child)
	if err == nil || !strings.Contains(err.Error(), "incomplete") {
		t.Fatalf("concurrent child file accepted: %v", err)
	}
	if data, err := os.ReadFile(filepath.Join(child, "keep", "parent.txt")); err != nil || string(data) != "concurrent child\n" {
		t.Fatalf("concurrent file overwritten: %q %v", data, err)
	}
	if _, err := os.Stat(filepath.Join(child, "excluded")); !os.IsNotExist(err) {
		t.Fatalf("excluded files materialized: %v", err)
	}
	if got := gitAt(t, f.Env(), child, "rev-parse", "HEAD"); got != receipt.Head {
		t.Fatal("child ref changed")
	}
}

func TestStackNewSparsePreservesConcurrentChildIndex(t *testing.T) {
	for _, suffix := range []string{"", ".lock"} {
		t.Run("index"+suffix, func(t *testing.T) {
			f, _ := publishedSparseParent(t)
			child := filepath.Join(t.TempDir(), "child")
			gitShim := shipDisplaceShim(t, f, "git")
			writeShipExecutable(t, f.ShimBin, "git", "#!/bin/sh\nif [ -z \"$CCX_SHIM_DEPTH\" ] && [ \"$1\" = worktree ] && [ \"$2\" = add ]; then\n  '"+gitShim+"' \"$@\" || exit $?\n  child_index=$(CCX_SHIM_DEPTH=1 git -C '"+child+"' rev-parse --path-format=absolute --git-path index)\n  printf 'concurrent index' > \"$child_index"+suffix+"\"\n  exit 0\nfi\nexec '"+gitShim+"' \"$@\"\n")
			_, _, err := runStackCmd(t, f, "new", "child", "--published-parent", "--sparse", "--path", child)
			if err == nil || !strings.Contains(err.Error(), "incomplete") {
				t.Fatalf("concurrent index accepted: %v", err)
			}
			index := gitAt(t, f.Env(), child, "rev-parse", "--path-format=absolute", "--git-path", "index")
			if data, err := os.ReadFile(index + suffix); err != nil || string(data) != "concurrent index" {
				t.Fatalf("concurrent index was changed: %q %v", data, err)
			}
			if _, err := os.Stat(filepath.Join(child, "keep")); !os.IsNotExist(err) {
				t.Fatalf("child populated despite conflicting index: %v", err)
			}
		})
	}
}
