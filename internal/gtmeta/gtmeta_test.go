package gtmeta_test

import (
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yasyf/cc-context/internal/gtmeta"
	"github.com/yasyf/cc-context/internal/vcstest"

	// modernc.org/sqlite registers the "sqlite" driver these tests read the
	// metadata database back through.
	_ "modernc.org/sqlite"
)

// TestReadTracksAGTStack drives every case over one gt-tracked stack, each
// subtest mutating what the last left behind: a real gt track costs a minute
// and a half here, so a fixture per case would buy isolation with ten.
func TestReadTracksAGTStack(t *testing.T) {
	f := vcstest.Repo(t, vcstest.GT())
	for _, name := range []string{"feat1", "feat2"} {
		run(t, f.Dir, "git", "switch", "-qc", name)
		write(t, filepath.Join(f.Dir, name+".txt"), name+"\n")
		run(t, f.Dir, "git", "add", name+".txt")
		run(t, f.Dir, "git", "commit", "-qm", name)
		run(t, f.Dir, "gt", "track", "-f", "--no-interactive")
	}
	commonDir := filepath.Join(f.Dir, ".git")
	trunkHead := head(t, f.Dir, "main")
	feat1Head := head(t, f.Dir, "feat1")

	t.Run("reports the trunk and each branch's parent", func(t *testing.T) {
		assertState(t, read(t, commonDir), gtmeta.State{
			"main":  {Trunk: true, Head: trunkHead},
			"feat1": {Head: feat1Head, Parents: []gtmeta.Ref{{Ref: "main", SHA: trunkHead}}},
			"feat2": {Head: head(t, f.Dir, "feat2"), Parents: []gtmeta.Ref{{Ref: "feat1", SHA: feat1Head}}},
		})
	})

	t.Run("normalizes an unfrozen branch", func(t *testing.T) {
		run(t, f.Dir, "gt", "freeze", "feat1", "--no-interactive")
		if got := read(t, commonDir)["feat1"].State; got != "frozen" {
			t.Fatalf("frozen state = %q, want frozen", got)
		}
		run(t, f.Dir, "gt", "unfreeze", "feat1", "--no-interactive")
		var native map[string]struct{ State string }
		if err := json.Unmarshal([]byte(run(t, f.Dir, "gt", "state", "--no-interactive")), &native); err != nil {
			t.Fatalf("decode native gt state: %v", err)
		}
		if got := native["feat1"].State; got != "" {
			t.Fatalf("native unfrozen state = %q, want no hold", got)
		}
		if got := read(t, commonDir)["feat1"].State; got != "" {
			t.Errorf("unfrozen state = %q, want no hold", got)
		}
	})

	// gt revalidates the whole database on every invocation, so a stack it still
	// reads back unchanged after a commit and a restack it never saw is a stack
	// it agrees with.
	t.Run("keeps a natively committed branch tracked", func(t *testing.T) {
		run(t, f.Dir, "git", "switch", "-q", "feat1")
		write(t, filepath.Join(f.Dir, "native.txt"), "native\n")
		run(t, f.Dir, "git", "add", "native.txt")
		run(t, f.Dir, "git", "commit", "-qm", "native")
		committed := head(t, f.Dir, "feat1")

		assertState(t, read(t, commonDir), gtmeta.State{
			"main":  {Trunk: true, Head: trunkHead},
			"feat1": {Head: committed, Parents: []gtmeta.Ref{{Ref: "main", SHA: trunkHead}}},
			"feat2": {NeedsRestack: true, Parents: []gtmeta.Ref{{Ref: "feat1", SHA: feat1Head}}},
		})

		run(t, f.Dir, "git", "switch", "-q", "feat2")
		run(t, f.Dir, "git", "rebase", "--onto", committed, feat1Head)
		if err := gtmeta.RecordRestacked(t.Context(), commonDir, map[string]string{"feat2": committed}); err != nil {
			t.Fatalf("RecordRestacked: %v", err)
		}
		feat1Head = committed

		want := gtmeta.State{
			"main":  {Trunk: true, Head: trunkHead},
			"feat1": {Head: committed, Parents: []gtmeta.Ref{{Ref: "main", SHA: trunkHead}}},
			"feat2": {Head: head(t, f.Dir, "feat2"), Parents: []gtmeta.Ref{{Ref: "feat1", SHA: committed}}},
		}
		assertState(t, read(t, commonDir), want)
		run(t, f.Dir, "gt", "state")
		assertState(t, read(t, commonDir), want)
	})

	t.Run("flags a branch whose parent moved", func(t *testing.T) {
		run(t, f.Dir, "git", "switch", "-q", "main")
		write(t, filepath.Join(f.Dir, "moved.txt"), "moved\n")
		run(t, f.Dir, "git", "add", "moved.txt")
		run(t, f.Dir, "git", "commit", "-qm", "moved")

		assertState(t, read(t, commonDir), gtmeta.State{
			"main":  {Trunk: true},
			"feat1": {NeedsRestack: true, Parents: []gtmeta.Ref{{Ref: "main", SHA: trunkHead}}},
			"feat2": {Parents: []gtmeta.Ref{{Ref: "feat1", SHA: feat1Head}}},
		})
	})

	t.Run("drops a branch whose ref is gone", func(t *testing.T) {
		run(t, f.Dir, "git", "branch", "-qD", "feat2")

		assertState(t, read(t, commonDir), gtmeta.State{
			"main":  {Trunk: true},
			"feat1": {NeedsRestack: true, Parents: []gtmeta.Ref{{Ref: "main", SHA: trunkHead}}},
		})
	})

	t.Run("drops a branch whose parent ref is gone", func(t *testing.T) {
		run(t, f.Dir, "git", "branch", "-qD", "feat1")

		assertState(t, read(t, commonDir), gtmeta.State{"main": {Trunk: true}})
	})
}

// TestRecordRestackedClearsNeedsRestack pins the one column a restack writes.
// NeedsRestack is derived, not stored — gt compares parent_branch_revision
// against the parent's live head — so a rebase nobody records reads as never
// having happened.
func TestRecordRestackedClearsNeedsRestack(t *testing.T) {
	f := vcstest.Repo(t, vcstest.GT())
	run(t, f.Dir, "git", "switch", "-qc", "feat")
	write(t, filepath.Join(f.Dir, "feat.txt"), "feat\n")
	run(t, f.Dir, "git", "add", "feat.txt")
	run(t, f.Dir, "git", "commit", "-qm", "feat")
	run(t, f.Dir, "gt", "track", "-f", "--no-interactive")
	run(t, f.Dir, "git", "switch", "-q", "main")
	write(t, filepath.Join(f.Dir, "moved.txt"), "moved\n")
	run(t, f.Dir, "git", "add", "moved.txt")
	run(t, f.Dir, "git", "commit", "-qm", "moved")
	commonDir := filepath.Join(f.Dir, ".git")

	if !read(t, commonDir)["feat"].NeedsRestack {
		t.Fatal("feat does not need a restack after trunk moved under it")
	}
	if err := gtmeta.RecordRestacked(t.Context(), commonDir, map[string]string{"feat": head(t, f.Dir, "main")}); err != nil {
		t.Fatalf("RecordRestacked: %v", err)
	}
	state := read(t, commonDir)
	if state["feat"].NeedsRestack {
		t.Error("feat still needs a restack after its parent revision was recorded")
	}
	if got, want := state["feat"].Parents, []gtmeta.Ref{{Ref: "main", SHA: head(t, f.Dir, "main")}}; len(got) != 1 || got[0] != want[0] {
		t.Errorf("parents = %v, want %v — a restack changes the revision, never the parent", got, want)
	}
}

// TestRecordRestackedRefusesAnUntrackedBranch pins that the write is an update,
// never an insert: a branch with no row was never tracked, and inventing one
// would hand gt a stack entry nothing else agrees with.
func TestRecordRestackedRefusesAnUntrackedBranch(t *testing.T) {
	dir := t.TempDir()
	vcstest.WriteGraphiteMeta(t, dir, `{"main":{"trunk":true}}`)

	err := gtmeta.RecordRestacked(t.Context(), dir, map[string]string{"ghost": "deadbeef"})
	if err == nil || !strings.Contains(err.Error(), "ghost") {
		t.Errorf("RecordRestacked = %v, want a refusal naming ghost", err)
	}
}

func TestAdoptRootRecordsBothSidesAtomically(t *testing.T) {
	dir := t.TempDir()
	vcstest.WriteGraphiteMeta(t, dir, `{"main":{"trunk":true}}`)
	if err := gtmeta.AdoptRoot(t.Context(), dir, "feature", "main", "base", "head"); err != nil {
		t.Fatal(err)
	}
	row, err := gtmeta.ReadPublishedChild(t.Context(), dir, "feature")
	if err != nil || row.Parent != "main" || row.ParentRevision != "base" || row.BranchRevision != "head" || row.Validation != "VALID" {
		t.Fatalf("feature row = %+v, %v", row, err)
	}
	db, err := sql.Open("sqlite", filepath.Join(dir, ".graphite_metadata.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	var children string
	if err := db.QueryRow(`SELECT children FROM branch_metadata WHERE branch_name = 'main'`).Scan(&children); err != nil || children != `["feature"]` {
		t.Fatalf("trunk children = %q, %v", children, err)
	}
	if err := gtmeta.AdoptRoot(t.Context(), dir, "feature", "main", "base2", "head2"); err != nil {
		t.Fatalf("re-adopting feature = %v, want its row rewritten", err)
	}
	if row, err := gtmeta.ReadPublishedChild(t.Context(), dir, "feature"); err != nil || row.ParentRevision != "base2" || row.BranchRevision != "head2" {
		t.Fatalf("re-adopted feature row = %+v, %v", row, err)
	}
	if err := db.QueryRow(`SELECT children FROM branch_metadata WHERE branch_name = 'main'`).Scan(&children); err != nil || children != `["feature"]` {
		t.Fatalf("trunk children after re-adopting = %q, %v", children, err)
	}
	if err := gtmeta.AdoptRoot(t.Context(), dir, "orphan", "missing", "base", "head"); err == nil {
		t.Fatal("missing trunk was accepted")
	}
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM branch_metadata WHERE branch_name = 'orphan'`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("orphan rows = %d, %v", count, err)
	}
}

func TestAdoptRootMovesARowOffTheParentItHad(t *testing.T) {
	dir := t.TempDir()
	vcstest.WriteGraphiteMeta(t, dir, `{"main":{"trunk":true},`+
		`"mid":{"parents":[{"ref":"main","sha":"aaaa"}]},`+
		`"feature":{"parents":[{"ref":"mid","sha":"bbbb"}]}}`)
	if err := gtmeta.AdoptRoot(t.Context(), dir, "feature", "main", "base", "head"); err != nil {
		t.Fatalf("AdoptRoot over an existing row = %v", err)
	}
	row, err := gtmeta.ReadPublishedChild(t.Context(), dir, "feature")
	if err != nil || row.Parent != "main" || row.ParentRevision != "base" || row.Validation != "VALID" {
		t.Fatalf("feature row = %+v, %v", row, err)
	}
	db, err := sql.Open("sqlite", filepath.Join(dir, ".graphite_metadata.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	for parent, want := range map[string]string{"main": `["mid","feature"]`, "mid": `[]`} {
		var children string
		if err := db.QueryRow(`SELECT children FROM branch_metadata WHERE branch_name = ?`, parent).Scan(&children); err != nil || children != want {
			t.Errorf("%s children = %q, %v, want %s", parent, children, err, want)
		}
	}
}

// TestReparentMovesARowAndItsNewParentsChildren pins both sides of a move. gt
// walks its own tree through the children column, not the parent pointer:
// measured against gt 1.8.6, a branch its new parent does not list back is
// dropped from gt log entirely, while gt state still reports the new parent.
func TestReparentMovesARowAndItsNewParentsChildren(t *testing.T) {
	dir := t.TempDir()
	vcstest.WriteGraphiteMeta(t, dir, `{"main":{"trunk":true},`+
		`"mid":{"parents":[{"ref":"main","sha":"aaaa"}]},`+
		`"gone":{"parents":[{"ref":"mid","sha":"bbbb"}]},`+
		`"kid":{"parents":[{"ref":"gone","sha":"cccc"}]}}`)

	if err := gtmeta.Reparent(t.Context(), dir, map[string]string{"kid": "mid"}); err != nil {
		t.Fatalf("Reparent: %v", err)
	}
	tests := []struct {
		branch   string
		parent   string
		revision string
		children string
	}{
		{branch: "main", children: `["mid"]`},
		{branch: "mid", parent: "main", revision: "aaaa", children: `["gone","kid"]`},
		{branch: "gone", parent: "mid", revision: "bbbb", children: `[]`},
		{branch: "kid", parent: "mid", revision: "cccc", children: `[]`},
	}
	for _, tt := range tests {
		t.Run(tt.branch, func(t *testing.T) {
			parent, revision, children := metaRow(t, dir, tt.branch)
			if parent != tt.parent {
				t.Errorf("parent = %q, want %q", parent, tt.parent)
			}
			if revision != tt.revision {
				t.Errorf("parent revision = %q, want %q — a move leaves the revision as it stood", revision, tt.revision)
			}
			if children != tt.children {
				t.Errorf("children = %s, want %s", children, tt.children)
			}
		})
	}
}

// TestReparentRefusesAnUntrackedBranch pins the write as an update: a branch
// with no row was never tracked, and the whole batch aborts rather than move
// the branches ahead of it in isolation.
func TestReparentRefusesAnUntrackedBranch(t *testing.T) {
	dir := t.TempDir()
	vcstest.WriteGraphiteMeta(t, dir, `{"main":{"trunk":true},`+
		`"kid":{"parents":[{"ref":"gone","sha":"cccc"}]}}`)

	err := gtmeta.Reparent(t.Context(), dir, map[string]string{"kid": "main", "ghost": "main"})
	if err == nil || !strings.Contains(err.Error(), `"ghost" has no branch_metadata row`) {
		t.Fatalf("Reparent = %v, want a refusal naming ghost", err)
	}
	parent, _, _ := metaRow(t, dir, "kid")
	if parent != "gone" {
		t.Errorf("kid's parent = %q, want gone — the aborted batch wrote nothing", parent)
	}
	if _, _, children := metaRow(t, dir, "main"); children != `[]` {
		t.Errorf("main's children = %s, want [] — the aborted batch wrote nothing", children)
	}
}

func metaRow(t *testing.T, commonDir, branch string) (parent, revision, children string) {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(commonDir, ".graphite_metadata.db"))
	if err != nil {
		t.Fatalf("open metadata in %s: %v", commonDir, err)
	}
	defer func() { _ = db.Close() }()

	query := `SELECT COALESCE(parent_branch_name, ''), COALESCE(parent_branch_revision, ''), COALESCE(children, '')
		FROM branch_metadata WHERE branch_name = ?`
	if err := db.QueryRow(query, branch).Scan(&parent, &revision, &children); err != nil {
		t.Fatalf("read %s in %s: %v", branch, commonDir, err)
	}
	return parent, revision, children
}

func TestLastSubmittedRoundTrip(t *testing.T) {
	dir := t.TempDir()
	vcstest.WriteGraphiteMeta(t, dir, `{"main":{"trunk":true},"feat":{"parents":[{"ref":"main","sha":"deadbeef"}]}}`)

	got, err := gtmeta.LastSubmitted(t.Context(), dir)
	if err != nil {
		t.Fatalf("LastSubmitted: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("LastSubmitted = %v, want none before any submit", got)
	}

	want := gtmeta.Version{HeadSha: "0f0f", BaseSha: "deadbeef", BaseName: "main"}
	if err := gtmeta.RecordSubmitted(t.Context(), dir, map[string]gtmeta.Version{"feat": want}); err != nil {
		t.Fatalf("RecordSubmitted: %v", err)
	}
	got, err = gtmeta.LastSubmitted(t.Context(), dir)
	if err != nil {
		t.Fatalf("LastSubmitted: %v", err)
	}
	if len(got) != 1 || got["feat"] != want {
		t.Errorf("LastSubmitted = %v, want feat recorded as %+v", got, want)
	}

	err = gtmeta.RecordSubmitted(t.Context(), dir, map[string]gtmeta.Version{"absent": want})
	if err == nil || !strings.Contains(err.Error(), `"absent" has no branch_metadata row`) {
		t.Errorf("RecordSubmitted(absent) = %v, want a refusal naming the missing row", err)
	}
}

// TestRecordSubmittedIsAllOrNothing pins the write to the atomic push it
// records: an untracked branch anywhere in the map aborts the transaction, so
// no branch is left holding a lease for a head the stack never submitted under.
func TestRecordSubmittedIsAllOrNothing(t *testing.T) {
	dir := t.TempDir()
	vcstest.WriteGraphiteMeta(t, dir, `{"main":{"trunk":true},"feat":{"parents":[{"ref":"main","sha":"deadbeef"}]},`+
		`"top":{"parents":[{"ref":"feat","sha":"0f0f"}]}}`)

	seed := gtmeta.Version{HeadSha: "0f0f", BaseSha: "deadbeef", BaseName: "main"}
	if err := gtmeta.RecordSubmitted(t.Context(), dir, map[string]gtmeta.Version{"feat": seed}); err != nil {
		t.Fatalf("seed RecordSubmitted: %v", err)
	}

	err := gtmeta.RecordSubmitted(t.Context(), dir, map[string]gtmeta.Version{
		"feat":  {HeadSha: "1a1a", BaseSha: "deadbeef", BaseName: "main"},
		"top":   {HeadSha: "2b2b", BaseSha: "1a1a", BaseName: "feat"},
		"ghost": {HeadSha: "3c3c", BaseSha: "2b2b", BaseName: "top"},
	})
	if err == nil || !strings.Contains(err.Error(), `"ghost" has no branch_metadata row`) {
		t.Fatalf("RecordSubmitted = %v, want a refusal naming ghost", err)
	}

	last, err := gtmeta.LastSubmitted(t.Context(), dir)
	if err != nil {
		t.Fatalf("LastSubmitted: %v", err)
	}
	if len(last) != 1 || last["feat"] != seed {
		t.Errorf("LastSubmitted = %v, want only the seeded %+v — the aborted batch wrote nothing", last, seed)
	}
}

func TestReadFailsWithoutGraphiteConfig(t *testing.T) {
	f := vcstest.Repo(t)
	_, err := gtmeta.Read(t.Context(), filepath.Join(f.Dir, ".git"))
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("read without graphite config: got %v, want a not-exist error", err)
	}
}

func read(t *testing.T, commonDir string) gtmeta.State {
	t.Helper()
	state, err := gtmeta.Read(t.Context(), commonDir)
	if err != nil {
		t.Fatalf("read %s: %v", commonDir, err)
	}
	return state
}

func assertState(t *testing.T, got, want gtmeta.State) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("branches: got %v, want %v", names(got), names(want))
	}
	for name, w := range want {
		g, ok := got[name]
		if !ok {
			t.Fatalf("branch %s: missing from %v", name, names(got))
		}
		if g.Trunk != w.Trunk || g.NeedsRestack != w.NeedsRestack {
			t.Errorf("branch %s: got trunk=%v restack=%v, want trunk=%v restack=%v", name, g.Trunk, g.NeedsRestack, w.Trunk, w.NeedsRestack)
		}
		if w.Head != "" && g.Head != w.Head {
			t.Errorf("branch %s: got head %s, want %s", name, g.Head, w.Head)
		}
		if len(g.Parents) != len(w.Parents) {
			t.Fatalf("branch %s: got parents %v, want %v", name, g.Parents, w.Parents)
		}
		for i, wp := range w.Parents {
			if g.Parents[i] != wp {
				t.Errorf("branch %s parent %d: got %+v, want %+v", name, i, g.Parents[i], wp)
			}
		}
	}
}

func names(state gtmeta.State) []string {
	out := make([]string, 0, len(state))
	for name := range state {
		out = append(out, name)
	}
	return out
}

func head(t *testing.T, dir, branch string) string {
	t.Helper()
	return strings.TrimSpace(run(t, dir, "git", "rev-parse", branch))
}

// run executes bin in dir and returns its output. The child's streams are a
// real file rather than a pipe: gt leaves a detached telemetry process holding
// whatever it inherited, and a pipe keeps Wait blocked until that one exits.
func run(t *testing.T, dir, bin string, args ...string) string {
	t.Helper()
	out, err := os.CreateTemp(t.TempDir(), "out")
	if err != nil {
		t.Fatalf("create output file: %v", err)
	}
	defer func() { _ = out.Close() }()

	cmd := exec.Command(bin, args...) //nolint:gosec // bin and args are fixture-authored, never user input
	cmd.Dir = dir
	cmd.Stdout = out
	cmd.Stderr = out
	runErr := cmd.Run()
	captured, err := os.ReadFile(out.Name())
	if err != nil {
		t.Fatalf("read output file: %v", err)
	}
	if runErr != nil {
		t.Fatalf("%s %v: %v\n%s", bin, args, runErr, captured)
	}
	return string(captured)
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// TestForgetDisownsTheBranchFromItsParent pins the other half of the same tree
// Reparent maintains: a forgotten branch left in its parent's children column is
// a name gt walks into and finds no row for, which is the corruption a prune is
// run to clear rather than to create.
func TestForgetDisownsTheBranchFromItsParent(t *testing.T) {
	dir := t.TempDir()
	vcstest.WriteGraphiteMeta(t, dir, `{"main":{"trunk":true},`+
		`"ghost":{"parents":[{"ref":"main","sha":"aaaa"}]},`+
		`"mid":{"parents":[{"ref":"main","sha":"bbbb"}]},`+
		`"kid":{"parents":[{"ref":"mid","sha":"cccc"}]}}`)

	if err := gtmeta.Forget(t.Context(), dir, []string{"ghost", "untracked"}); err != nil {
		t.Fatalf("Forget: %v", err)
	}
	if _, _, children := metaRow(t, dir, "main"); children != `["mid"]` {
		t.Errorf("main children = %s, want [\"mid\"] — ghost outlived its row", children)
	}
	if _, _, children := metaRow(t, dir, "mid"); children != `["kid"]` {
		t.Errorf("mid children = %s, want [\"kid\"] — an unrelated row was rewritten", children)
	}
}

func TestReadKeepsBranchesAboveARevisionGtRejected(t *testing.T) {
	f := vcstest.Repo(t, vcstest.GT())
	for _, name := range []string{"feat1", "feat2", "feat3"} {
		run(t, f.Dir, "git", "switch", "-qc", name)
		write(t, filepath.Join(f.Dir, name+".txt"), name+"\n")
		run(t, f.Dir, "git", "add", name+".txt")
		run(t, f.Dir, "git", "commit", "-qm", name)
		run(t, f.Dir, "gt", "track", "-f", "--no-interactive")
	}
	commonDir := filepath.Join(f.Dir, ".git")
	setValidation(t, commonDir, "feat2", "BAD_PARENT_REVISION")
	setValidation(t, commonDir, "feat3", "INVALID_PARENT")

	state := read(t, commonDir)
	for _, name := range []string{"feat2", "feat3"} {
		s, ok := state[name]
		if !ok {
			t.Fatalf("%s dropped from the stack: %v", name, names(state))
		}
		if !s.NeedsRestack {
			t.Errorf("%s does not need a restack, want one", name)
		}
	}
	if err := gtmeta.RecordRestacked(t.Context(), commonDir, map[string]string{"feat2": head(t, f.Dir, "feat1")}); err != nil {
		t.Fatalf("RecordRestacked: %v", err)
	}
	if read(t, commonDir)["feat2"].NeedsRestack {
		t.Error("feat2 still needs a restack after its revision was recorded")
	}
}

func setValidation(t *testing.T, commonDir, branch, result string) {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(commonDir, ".graphite_metadata.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.Exec(`UPDATE branch_metadata SET validation_result = ? WHERE branch_name = ?`, result, branch); err != nil {
		t.Fatalf("set %s validation: %v", branch, err)
	}
}

func TestReparentSameParentKeepsChildMembership(t *testing.T) {
	dir := t.TempDir()
	vcstest.WriteGraphiteMeta(t, dir, `{"main":{"trunk":true},"parent":{"parents":[{"ref":"main","sha":"aaaa"}]},"child":{"parents":[{"ref":"parent","sha":"bbbb"}]}}`)
	if err := gtmeta.Reparent(t.Context(), dir, map[string]string{"child": "parent"}); err != nil {
		t.Fatal(err)
	}
	if _, _, children := metaRow(t, dir, "parent"); children != `["child"]` {
		t.Fatalf("same-parent update lost membership: %s", children)
	}
	if parent, revision, _ := metaRow(t, dir, "child"); parent != "parent" || revision != "bbbb" {
		t.Fatalf("same-parent update changed boundary: %s %s", parent, revision)
	}
}
