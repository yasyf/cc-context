package cli

import (
	"encoding/json"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/yasyf/cc-context/internal/nativemeta"
	"github.com/yasyf/cc-context/internal/vcstest"
)

func TestStackListReadsNativeCatalogWithoutGraphiteOrAuthentication(t *testing.T) {
	f := vcstest.Repo(t, vcstest.Remote())
	common := filepath.Join(f.Dir, ".git")
	trunk := shipHead(t, f)
	mustRun(t, f.Env(), f.Dir, "git", "switch", "-qc", "base")
	writeShipFile(t, f.Dir, "base.txt", "base\n")
	mustRun(t, f.Env(), f.Dir, "git", "add", "base.txt")
	mustRun(t, f.Env(), f.Dir, "git", "commit", "-qm", "base")
	base := shipHead(t, f)
	mustRun(t, f.Env(), f.Dir, "git", "switch", "-qc", "child")
	writeShipFile(t, f.Dir, "child.txt", "child\n")
	mustRun(t, f.Env(), f.Dir, "git", "add", "child.txt")
	mustRun(t, f.Env(), f.Dir, "git", "commit", "-qm", "child")
	child := shipHead(t, f)
	if err := nativemeta.Init(f.Context(), common, "main"); err != nil {
		t.Fatal(err)
	}
	if err := nativemeta.AdoptRoot(f.Context(), common, "base", "main", trunk, base); err != nil {
		t.Fatal(err)
	}
	if err := nativemeta.AdoptRoot(f.Context(), common, "child", "base", base, child); err != nil {
		t.Fatal(err)
	}
	mustRun(t, f.Env(), f.Dir, "git", "switch", "-q", "base")
	held := f.WorktreePath("child")
	mustRun(t, f.Env(), f.Dir, "git", "worktree", "add", "-q", held, "child")
	held = gitAt(t, f.Env(), held, "rev-parse", "--show-toplevel")
	f.OnlyShimPATH(t)
	before := gitAt(t, f.Env(), f.Dir, "hash-object", filepath.Join(common, "gh-stack"))
	shipResetLog(t, f)
	out, _, err := runStackCmd(t, f, "list")
	if err != nil {
		t.Fatalf("native list: %v", err)
	}
	want := "base" + shipSep + "here\nchild" + shipSep + held
	if out != want {
		t.Fatalf("native list=%q, want %q", out, want)
	}
	out, _, err = runStackCmd(t, f, "list", "--json")
	if err != nil {
		t.Fatalf("native JSON list: %v", err)
	}
	var report stackListReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatal(err)
	}
	expected := stackListReport{Root: f.Dir, Branches: []stackListEntry{{Branch: "base", Path: f.Dir, Current: true}, {Branch: "child", Path: held}}}
	if !reflect.DeepEqual(report, expected) {
		t.Fatalf("native report=%+v, want %+v", report, expected)
	}
	for _, inv := range vcstest.Invocations(t, f.ArgvLog) {
		if len(inv) > 0 && inv[0] != "git" {
			t.Fatalf("local native listing invoked authentication or vendor tool: %v", inv)
		}
		if strings.Contains(strings.Join(inv, " "), "config --get ccx.stack") {
			t.Fatalf("listing used write-lane provider discovery: %v", inv)
		}
	}
	if after := gitAt(t, f.Env(), f.Dir, "hash-object", filepath.Join(common, "gh-stack")); after != before {
		t.Fatal("listing mutated native catalog")
	}
	if got := gitAt(t, f.Env(), f.Dir, "rev-parse", "base", "child"); got != base+"\n"+child {
		t.Fatal("listing moved stack refs")
	}
}
