package cli

import (
	"bytes"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/yasyf/cc-context/internal/render"
	"github.com/yasyf/cc-context/internal/vcs"
	"github.com/yasyf/cc-context/internal/vcstest"
)

const (
	thinTestDepth   = 4
	thinNotesLogEnv = "CCX_FAKE_CC_NOTES_LOG"
	thinFakeCCNotes = `#!/bin/sh
printf '%s %s\n' "$(pwd -P)" "$*" >> "$CCX_FAKE_CC_NOTES_LOG"
if [ -n "$CCX_FAKE_CC_NOTES_UNKNOWN" ]; then
	echo 'Error: unknown command "storage" for "cc-notes"' >&2
	exit 1
fi
[ $# -eq 4 ] && [ "$1 $2 $3" = "storage bind --source" ] || { echo "unexpected cc-notes $*" >&2; exit 2; }
backend=$(git -C "$4" config --local --get cc-notes.storage)
case $? in
0) ;;
1) backend=$(git -C "$4" rev-parse --path-format=absolute --git-common-dir) || exit 1 ;;
*) exit 1 ;;
esac
bound=$(git config --local --get cc-notes.storage)
case "$bound" in
"") git config --local cc-notes.storage "$backend" && echo "bound $(pwd -P) to records at $backend" ;;
"$backend") echo "already bound $(pwd -P) to records at $backend" ;;
*) echo "error: context bound to a different backend: bound to $bound, asked to bind $backend" >&2; exit 1 ;;
esac
`
)

func thinRepo(t *testing.T, names ...string) *vcstest.Fixture {
	t.Helper()
	f := shipGTRepo(t, vcstest.GTStack(names...))
	stubOpenPRs(t, f, nil, names...)
	mustRun(t, f.Env(), f.RemoteDir, "git", "config", "uploadpack.allowFilter", "true")
	bin := t.TempDir()
	writeExecutable(t, filepath.Join(bin, "cc-notes"), thinFakeCCNotes)
	f.PrependPATH(bin)
	f.Setenv(thinNotesLogEnv, filepath.Join(bin, "calls.log"))
	return f
}

func thinNotesCalls(t *testing.T, f *vcstest.Fixture) []string {
	t.Helper()
	var log string
	for _, kv := range f.Env() {
		if v, ok := strings.CutPrefix(kv, thinNotesLogEnv+"="); ok {
			log = v
		}
	}
	data, err := os.ReadFile(log)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
}

func thinGrowTrunk(t *testing.T, f *vcstest.Fixture, commits int) {
	t.Helper()
	clone := filepath.Join(t.TempDir(), "upstream")
	mustRun(t, f.Env(), filepath.Dir(clone), "git", "clone", "-q", "--branch", "main", f.RemoteDir, clone)
	for _, kv := range [][2]string{{"user.email", "t@t.t"}, {"user.name", "t"}, {"commit.gpgsign", "false"}} {
		mustRun(t, f.Env(), clone, "git", "config", kv[0], kv[1])
	}
	offset, err := strconv.Atoi(gitAt(t, f.Env(), clone, "rev-list", "--count", "HEAD"))
	if err != nil {
		t.Fatal(err)
	}
	for i := offset; i < offset+commits; i++ {
		var seed [32]byte
		copy(seed[:], fmt.Sprintf("blob-%d", i))
		blob := make([]byte, 32<<10)
		if _, err := rand.NewChaCha8(seed).Read(blob); err != nil {
			t.Fatal(err)
		}
		writeShipFile(t, clone, fmt.Sprintf("excluded/blob-%03d.bin", i), string(blob))
		writeShipFile(t, clone, fmt.Sprintf("keep/step-%03d.txt", i), fmt.Sprintf("step %d\n", i))
		writeShipFile(t, clone, "trunk.txt", fmt.Sprintf("trunk %d\n", i))
		mustRun(t, f.Env(), clone, "git", "add", "-A")
		mustRun(t, f.Env(), clone, "git", "commit", "-qm", fmt.Sprintf("trunk %d", i))
	}
	mustRun(t, f.Env(), clone, "git", "push", "-q", "origin", "main")
	mustRun(t, f.Env(), f.Dir, "git", "fetch", "-q", "origin")
}

func thinTestHome(t *testing.T, f *vcstest.Fixture) string {
	t.Helper()
	var home string
	for _, kv := range f.Env() {
		if v, ok := strings.CutPrefix(kv, "HOME="); ok {
			home = v
		}
	}
	resolved, err := filepath.EvalSymlinks(home)
	if err != nil {
		t.Fatal(err)
	}
	return resolved
}

func thinTestStore(t *testing.T, f *vcstest.Fixture) string {
	t.Helper()
	sum := sha256.Sum256([]byte(filepath.Clean(f.RemoteDir)))
	return filepath.Join(thinTestHome(t, f), ".claude", "stores", hex.EncodeToString(sum[:])[:12], filepath.Base(f.Dir))
}

func thinTestLane(t *testing.T, f *vcstest.Fixture, name string) string {
	t.Helper()
	return filepath.Join(thinTestHome(t, f), ".claude", "worktrees", filepath.Base(f.Dir), name)
}

func thinNew(t *testing.T, f *vcstest.Fixture, dir string, args ...string) (string, string) {
	t.Helper()
	out, errOut, err := runStackCmdIn(t, f, dir, append([]string{"new"}, args...)...)
	if err != nil {
		t.Fatalf("stack new %v: %v\n%s", args, err, errOut)
	}
	segs := strings.Split(out, shipSep)
	return out, segs[len(segs)-1]
}

func thinCommit(t *testing.T, f *vcstest.Fixture, dir, file, content string) string {
	t.Helper()
	writeShipFile(t, dir, file, content)
	mustRun(t, f.Env(), dir, "git", "add", file)
	mustRun(t, f.Env(), dir, "git", "commit", "-qm", file)
	return gitAt(t, f.Env(), dir, "rev-parse", "HEAD")
}

func thinGitCode(t *testing.T, f *vcstest.Fixture, dir string, args ...string) int {
	t.Helper()
	_, code, _, err := render.RunCLIExitCodeEnv(f.ContextIn(dir), render.Dir(dir), "git", args, gitRecordedHistoryEnv)
	if err != nil {
		t.Fatal(err)
	}
	return code
}

func thinRefs(t *testing.T, f *vcstest.Fixture, dir string) string {
	t.Helper()
	return gitAt(t, f.Env(), dir, "for-each-ref", "--format=%(refname) %(objectname)")
}

type thinSnapshot struct {
	head, refs string
	index      []byte
}

func thinSnap(t *testing.T, f *vcstest.Fixture) thinSnapshot {
	t.Helper()
	index, err := os.ReadFile(gitAt(t, f.Env(), f.Dir, "rev-parse", "--path-format=absolute", "--git-path", "index"))
	if err != nil {
		t.Fatal(err)
	}
	return thinSnapshot{head: gitAt(t, f.Env(), f.Dir, "rev-parse", "HEAD"), refs: thinRefs(t, f, f.Dir), index: index}
}

func thinRequireSource(t *testing.T, f *vcstest.Fixture, before thinSnapshot) {
	t.Helper()
	after := thinSnap(t, f)
	if after.head != before.head || after.refs != before.refs || !bytes.Equal(after.index, before.index) {
		t.Errorf("source checkout changed:\nhead %s → %s\nrefs %s\n→ %s\nindex equal %v", before.head, after.head, before.refs, after.refs, bytes.Equal(after.index, before.index))
	}
}

func thinOnto(t *testing.T, f *vcstest.Fixture, dir, ancestor, branch string) bool {
	t.Helper()
	ok, err := gitIsAncestor(f.ContextIn(dir), render.Dir(dir), "test", ancestor, branch)
	if err != nil {
		t.Fatal(err)
	}
	return ok
}

type thinGTRow struct {
	state, parentRevision, branchRevision string
}

func thinGTRowOf(t *testing.T, store, branch string) (thinGTRow, bool) {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(store, ".git", ".graphite_metadata.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	var row thinGTRow
	err = db.QueryRow(`SELECT COALESCE(state, ''), COALESCE(parent_branch_revision, ''), COALESCE(branch_revision, '') FROM branch_metadata WHERE branch_name = ?`, branch).Scan(&row.state, &row.parentRevision, &row.branchRevision)
	if errors.Is(err, sql.ErrNoRows) {
		return thinGTRow{}, false
	}
	if err != nil {
		t.Fatalf("read %s's gt row: %v", branch, err)
	}
	return row, true
}

func thinGTState(t *testing.T, store, branch string) string {
	t.Helper()
	row, ok := thinGTRowOf(t, store, branch)
	if !ok {
		t.Fatalf("%s has no gt row", branch)
	}
	return row.state
}

func thinGTParent(t *testing.T, f *vcstest.Fixture, dir, branch string) string {
	t.Helper()
	state, err := gtStateQuery(f.ContextIn(dir), render.Dir(dir), "test")
	if err != nil {
		t.Fatal(err)
	}
	parents := state[branch].Parents
	if len(parents) != 1 {
		t.Fatalf("%s's gt parents = %#v", branch, parents)
	}
	return parents[0].Ref
}

func thinPublishedParent(t *testing.T) (*vcstest.Fixture, *stackPublication) {
	t.Helper()
	f := thinRepo(t, "parent")
	thinCommit(t, f, f.Dir, "parent.txt", "parent work\n")
	if _, _, err := runStackCmd(t, f, "submit"); err != nil {
		t.Fatal(err)
	}
	receipt, err := stackReadPublication(f.Context(), render.Dir(f.Dir), "parent")
	if err != nil || receipt == nil {
		t.Fatalf("publication = %#v, %v", receipt, err)
	}
	thinGrowTrunk(t, f, 12)
	return f, receipt
}

func TestThinCanonicalRemote(t *testing.T) {
	t.Parallel()
	tests := []struct{ raw, want string }{
		{"git@GitHub.com:yasyf/cc-context.git", "github.com/yasyf/cc-context"},
		{"https://github.com/yasyf/cc-context", "github.com/yasyf/cc-context"},
		{"https://github.com:443/yasyf/cc-context", "github.com/yasyf/cc-context"},
		{"ssh://git@github.com/yasyf/cc-context.git/", "github.com/yasyf/cc-context"},
		{"ssh://git@github.com:22/yasyf/cc-context.git", "github.com/yasyf/cc-context"},
		{"ssh://git@host:2222/team/repo.git", "host:2222/team/repo"},
		{"ssh://git@host:2200/team/repo.git", "host:2200/team/repo"},
		{"https://[::1]:8443/team/repo", "[::1]:8443/team/repo"},
		{"git@host:Team/Repo.git", "host/Team/Repo"},
		{"file:///srv/git/Repo.git", "/srv/git/Repo.git"},
		{"/srv/git/Repo.git", "/srv/git/Repo.git"},
		{"/srv/git/repo.git", "/srv/git/repo.git"},
		{"/srv/git/repo", "/srv/git/repo"},
		{"https://user:s3cret@host/team/repo.git", "host/team/repo"},
	}
	for _, tt := range tests {
		got, err := thinCanonicalRemote(tt.raw)
		if err != nil || got != tt.want {
			t.Errorf("thinCanonicalRemote(%q) = %q, %v, want %q", tt.raw, got, err, tt.want)
		}
	}
	for _, raw := range []string{"relative/repo", "https://github.com/", "file://relative/repo", "https://user:s3cret@host:bad/team/repo"} {
		got, err := thinCanonicalRemote(raw)
		if err == nil {
			t.Errorf("thinCanonicalRemote(%q) = %q, want an error", raw, got)
		} else if strings.Contains(err.Error(), "s3cret") || strings.Contains(err.Error(), raw) {
			t.Errorf("thinCanonicalRemote(%q) error %q echoes the remote", raw, err)
		}
	}
}

func TestStackNewThinCreatesAThinStore(t *testing.T) {
	t.Parallel()
	f := thinRepo(t)
	f.Setenv(stackNewEnv, "")
	thinGrowTrunk(t, f, 12)
	before := thinSnap(t, f)

	out, lane := thinNew(t, f, f.Dir, "lane1", "--depth", strconv.Itoa(thinTestDepth))
	store := thinTestStore(t, f)
	if want := "created thin store " + store + shipSep + "cut lane1 onto main" + shipSep + thinTestLane(t, f, "lane1"); out != want {
		t.Fatalf("stack new = %q, want %q", out, want)
	}
	for key, want := range map[string]string{
		"feature.experimental":             "true",
		"feature.manyFiles":                "true",
		"pack.threads":                     "2",
		"remote.origin.fetch":              "+refs/heads/main:refs/remotes/origin/main",
		"remote.origin.promisor":           "true",
		"remote.origin.partialclonefilter": "blob:none",
		"remote.origin.tagopt":             "--no-tags",
		"branch.main.remote":               "origin",
		"branch.main.merge":                "refs/heads/main",
		"extensions.worktreeConfig":        "true",
	} {
		if got := gitAt(t, f.Env(), store, "config", "--local", "--get-all", key); got != want {
			t.Errorf("store %s = %q, want %q", key, got, want)
		}
	}
	for args, want := range map[string]string{
		"rev-parse --is-shallow-repository":     "true",
		"rev-parse --show-ref-format":           "files",
		"rev-list --count origin/main":          strconv.Itoa(thinTestDepth),
		"symbolic-ref refs/remotes/origin/HEAD": "refs/remotes/origin/main",
		"for-each-ref refs/tags":                "",
	} {
		if got := gitAt(t, f.Env(), store, strings.Fields(args)...); got != want {
			t.Errorf("store %s = %q, want %q", args, got, want)
		}
	}
	if got := gitAt(t, f.Env(), lane, "rev-parse", "--path-format=absolute", "--git-common-dir"); got != filepath.Join(store, ".git") {
		t.Errorf("lane common dir = %s, want the store's", got)
	}
	if got, want := gitAt(t, f.Env(), lane, "rev-parse", "HEAD"), gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "main"); got != want {
		t.Errorf("lane head = %s, want the remote trunk %s", got, want)
	}
	if got, err := os.ReadFile(filepath.Join(lane, "trunk.txt")); err != nil || strings.TrimSpace(string(got)) != gitAt(t, f.Env(), f.RemoteDir, "show", "main:trunk.txt") {
		t.Errorf("root file trunk.txt = %q, %v", got, err)
	}
	for _, dir := range []string{"keep", "excluded"} {
		if _, err := os.Stat(filepath.Join(lane, dir)); !os.IsNotExist(err) {
			t.Errorf("%s materialized outside the root-only cone: %v", dir, err)
		}
	}
	excluded := strings.Fields(gitAt(t, f.Env(), lane, "ls-tree", "--name-only", "HEAD", "excluded/"))
	blob := gitAt(t, f.Env(), lane, "rev-parse", "HEAD:"+excluded[len(excluded)-1])
	if code := thinGitCode(t, f, lane, "cat-file", "-e", blob); code == 0 {
		t.Error("an excluded blob reached the store")
	}
	if got := thinGTParent(t, f, lane, "lane1"); got != "main" {
		t.Errorf("lane1's gt parent = %s, want main", got)
	}

	thinCommit(t, f, lane, "lane1.txt", "lane work\n")
	writeShipFile(t, lane, "lane1.txt", "more lane work\n")
	report, errOut, err := runShipCmdFull(f.ContextIn(lane), t, "--dry-run", "-m", "more lane work")
	if err != nil {
		t.Fatalf("ship --dry-run in the lane: %v\n%s", err, errOut)
	}
	if !strings.Contains(report, "main") || !strings.Contains(report, "lane1") {
		t.Errorf("dry run = %q, want lane1 planned onto main", report)
	}
	t.Logf("dry run:\n%s", report)
	thinRequireSource(t, f, before)
}

func TestStackNewThinReusesTheStore(t *testing.T) {
	t.Parallel()
	f := thinRepo(t)
	thinGrowTrunk(t, f, 8)
	before := thinSnap(t, f)
	store := thinTestStore(t, f)
	_, a := thinNew(t, f, f.Dir, "a", "--thin", "--depth", strconv.Itoa(thinTestDepth))
	out, b := thinNew(t, f, f.Dir, "b", "--thin")
	if strings.Contains(out, "created thin store") {
		t.Errorf("second lane = %q, want the store reused", out)
	}
	thinCommit(t, f, a, "a.txt", "a\n")
	out, c := thinNew(t, f, a, "c", "--include", "keep")
	if want := "cut c onto a" + shipSep + thinTestLane(t, f, "c"); out != want {
		t.Errorf("nested lane = %q, want %q", out, want)
	}
	for _, lane := range []string{a, b, c} {
		if got := gitAt(t, f.Env(), lane, "rev-parse", "--path-format=absolute", "--git-common-dir"); got != filepath.Join(store, ".git") {
			t.Errorf("%s common dir = %s, want the one store", lane, got)
		}
	}
	kept := strings.Fields(gitAt(t, f.Env(), c, "ls-tree", "--name-only", "HEAD", "keep/"))
	if _, err := os.Stat(filepath.Join(c, kept[0])); err != nil {
		t.Errorf("--include keep left keep unmaterialized: %v", err)
	}
	if _, err := os.Stat(filepath.Join(c, "excluded")); !os.IsNotExist(err) {
		t.Errorf("nested lane materialized excluded: %v", err)
	}
	if got := thinGTParent(t, f, c, "c"); got != "a" {
		t.Errorf("c's gt parent = %s, want a", got)
	}
	refs := thinRefs(t, f, store)
	for _, args := range [][]string{
		{"d", "--thin", "--depth", "8"},
		{"d", "--full-history"},
	} {
		if _, _, err := runStackCmdIn(t, f, a, append([]string{"new"}, args...)...); err == nil {
			t.Errorf("stack new %v in the store succeeded, want a refusal", args)
		}
	}
	if _, _, err := runStackCmd(t, f, "new", "d", "--thin", "--depth", "8"); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Errorf("--depth on an existing store = %v, want a refusal", err)
	}
	if got := thinRefs(t, f, store); got != refs {
		t.Errorf("refusals moved store refs:\n%s\n→\n%s", refs, got)
	}
	thinRequireSource(t, f, before)
}

func TestStackNewThinSharesTheSourceNotes(t *testing.T) {
	t.Parallel()
	f := thinRepo(t)
	thinGrowTrunk(t, f, 6)
	store := thinTestStore(t, f)
	backend := gitAt(t, f.Env(), f.Dir, "rev-parse", "--path-format=absolute", "--git-common-dir")
	bindsSource := func(call string) (string, bool) {
		dir, args, _ := strings.Cut(call, " ")
		src, ok := strings.CutPrefix(args, "storage bind --source ")
		return dir, ok && gitAt(t, f.Env(), src, "rev-parse", "--path-format=absolute", "--git-common-dir") == backend
	}

	thinNew(t, f, f.Dir, "a", "--thin", "--depth", strconv.Itoa(thinTestDepth))
	calls := thinNotesCalls(t, f)
	if len(calls) != 1 {
		t.Fatalf("cc-notes calls creating the store = %q, want one bind", calls)
	}
	dir, ok := bindsSource(calls[0])
	if !ok {
		t.Errorf("cc-notes %q, want storage bind --source the source checkout", calls[0])
	}
	stage := filepath.Dir(dir)
	if dir == store || filepath.Base(dir) != filepath.Base(store) || filepath.Dir(stage) != filepath.Dir(store) || !strings.HasPrefix(filepath.Base(stage), "."+filepath.Base(store)+"-") {
		t.Errorf("bind ran in %s, want the staged clone beside %s before it was installed", dir, store)
	}
	if got := gitAt(t, f.Env(), store, "config", "--local", "--get", thinNotesKey); got != backend {
		t.Errorf("store %s = %q, want the source's records %q", thinNotesKey, got, backend)
	}

	thinNew(t, f, f.Dir, "b", "--thin")
	calls = thinNotesCalls(t, f)
	if len(calls) != 2 {
		t.Fatalf("cc-notes calls reusing the store = %q, want a second bind", calls)
	}
	if dir, ok := bindsSource(calls[1]); !ok || dir != store {
		t.Errorf("reuse ran cc-notes %q, want storage bind --source the source checkout in %s", calls[1], store)
	}

	other := filepath.Join(t.TempDir(), "other", filepath.Base(f.Dir))
	if err := os.MkdirAll(filepath.Dir(other), 0o750); err != nil {
		t.Fatal(err)
	}
	mustRun(t, f.Env(), filepath.Dir(other), "git", "clone", "-q", "--branch", "main", f.RemoteDir, other)
	refs := thinRefs(t, f, store)
	if _, _, err := runStackCmdIn(t, f, other, "new", "c", "--thin"); err == nil || !strings.Contains(err.Error(), "bound to a different backend") {
		t.Errorf("stack new --thin from another checkout of the same origin = %v, want the binding mismatch", err)
	}
	if got := gitAt(t, f.Env(), store, "config", "--local", "--get", thinNotesKey); got != backend {
		t.Errorf("mismatch retargeted the store to %q, want %q", got, backend)
	}
	if got := thinRefs(t, f, store); got != refs {
		t.Errorf("mismatch moved store refs:\n%s\n→\n%s", refs, got)
	}
	if _, err := os.Stat(thinTestLane(t, f, "c")); !os.IsNotExist(err) {
		t.Errorf("mismatch left a lane: %v", err)
	}

	mustRun(t, f.Env(), store, "git", "config", "--local", "--unset", thinNotesKey)
	calls = thinNotesCalls(t, f)
	bind := "cc-notes -R " + store + " storage bind --source "
	if _, _, err := runStackCmd(t, f, "new", "d", "--thin"); err == nil || !strings.Contains(err.Error(), bind) {
		t.Errorf("stack new --thin on an unbound store = %v, want a refusal naming %q", err, bind)
	}
	if got := thinNotesCalls(t, f); len(got) != len(calls) {
		t.Errorf("an unbound store ran cc-notes %q, want it left unbound", got[len(calls):])
	}
	if _, err := os.Stat(thinTestLane(t, f, "d")); !os.IsNotExist(err) {
		t.Errorf("unbound refusal left a lane: %v", err)
	}
}

func TestStackNewThinValidatesNotesInsideStore(t *testing.T) {
	t.Parallel()
	f := thinRepo(t)
	thinGrowTrunk(t, f, 6)
	before := thinSnap(t, f)
	store := thinTestStore(t, f)
	_, a := thinNew(t, f, f.Dir, "a", "--thin", "--depth", strconv.Itoa(thinTestDepth))
	binding := gitAt(t, f.Env(), store, "config", "--local", "--get", thinNotesKey)
	for i, dir := range []string{store, a} {
		name := "nested-" + strconv.Itoa(i)
		thinNew(t, f, dir, name, "--thin")
		calls := thinNotesCalls(t, f)
		if len(calls) != i+2 || calls[len(calls)-1] != store+" storage bind --source "+dir {
			t.Fatalf("nested bind calls = %q, want validation in %s from %s", calls, store, dir)
		}
		if got := gitAt(t, f.Env(), store, "config", "--local", "--get", thinNotesKey); got != binding {
			t.Errorf("nested creation changed binding to %q, want %q", got, binding)
		}
	}

	refs := thinRefs(t, f, store)
	f.Setenv("CCX_FAKE_CC_NOTES_UNKNOWN", "1")
	if _, _, err := runStackCmdIn(t, f, a, "new", "unvalidated", "--thin"); err == nil || !strings.Contains(err.Error(), `unknown command "storage"`) {
		t.Errorf("nested creation with a refusing cc-notes = %v, want its refusal", err)
	}
	if _, err := os.Stat(thinTestLane(t, f, "unvalidated")); !os.IsNotExist(err) {
		t.Errorf("cc-notes refusal left a lane: %v", err)
	}
	if got := thinRefs(t, f, store); got != refs {
		t.Errorf("cc-notes refusal moved store refs:\n%s\n→\n%s", refs, got)
	}
	if got := gitAt(t, f.Env(), store, "config", "--local", "--get", thinNotesKey); got != binding {
		t.Errorf("cc-notes refusal changed binding to %q, want %q", got, binding)
	}
	f.Setenv("CCX_FAKE_CC_NOTES_UNKNOWN", "")
	mustRun(t, f.Env(), store, "git", "config", "--local", "--unset", thinNotesKey)
	calls := thinNotesCalls(t, f)
	for i, dir := range []string{store, a} {
		name := "legacy-" + strconv.Itoa(i)
		if _, _, err := runStackCmdIn(t, f, dir, "new", name, "--thin"); err == nil || !strings.Contains(err.Error(), "storage bind --source <original-full-checkout>") {
			t.Errorf("unbound nested creation = %v, want an original-source binding hint", err)
		}
		if _, err := os.Stat(thinTestLane(t, f, name)); !os.IsNotExist(err) {
			t.Errorf("unbound refusal left a lane: %v", err)
		}
	}
	if got := thinNotesCalls(t, f); len(got) != len(calls) {
		t.Errorf("unbound store ran cc-notes %q, want a refusal before bind", got)
	}
	if got := thinRefs(t, f, store); got != refs {
		t.Errorf("unbound refusals moved store refs:\n%s\n→\n%s", refs, got)
	}
	thinRequireSource(t, f, before)
}

func TestStackNewThinRefusesWithoutNotesBinding(t *testing.T) {
	t.Parallel()
	f := thinRepo(t)
	f.Setenv(stackNewEnv, "")
	before := thinSnap(t, f)
	f.Setenv("CCX_FAKE_CC_NOTES_UNKNOWN", "1")

	_, _, err := runStackCmd(t, f, "new", "lane1", "--depth", strconv.Itoa(thinTestDepth))
	if err == nil || !strings.Contains(err.Error(), `unknown command "storage"`) {
		t.Fatalf("stack new default thin with a cc-notes lacking storage bind = %v, want its refusal", err)
	}
	store := thinTestStore(t, f)
	entries, err := os.ReadDir(filepath.Dir(store))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("refused creation left %v beside %s, want neither a store nor its staging", entries, store)
	}
	if _, err := os.Stat(thinTestLane(t, f, "lane1")); !os.IsNotExist(err) {
		t.Errorf("refused creation left a lane: %v", err)
	}
	thinRequireSource(t, f, before)
}

func TestStackThinTwoBranchRebaseContinue(t *testing.T) {
	t.Parallel()
	f := thinRepo(t)
	stubOpenPRs(t, f, nil, "a", "b")
	thinGrowTrunk(t, f, 8)
	before := thinSnap(t, f)
	store := thinTestStore(t, f)
	_, laneA := thinNew(t, f, f.Dir, "a", "--thin", "--depth", strconv.Itoa(thinTestDepth))
	thinCommit(t, f, laneA, "c.txt", "a\n")
	_, laneB := thinNew(t, f, laneA, "b")
	thinCommit(t, f, laneB, "d.txt", "b\n")
	restackAdvanceRemote(t, f, "main", "c.txt", "trunk\n")
	heads := map[string]string{}
	for _, branch := range []string{"a", "b"} {
		heads[branch] = gitAt(t, f.Env(), store, "rev-parse", branch)
	}

	_, _, err := runStackCmdIn(t, f, laneB, "rebase", "--no-push")
	if err == nil || !strings.Contains(err.Error(), "a does not rebase onto main cleanly") {
		t.Fatalf("rebase = %v, want a's conflict on c.txt", err)
	}
	ws := stackWorkspaceOf(t, err)
	if filepath.Dir(ws) != filepath.Dir(laneA) {
		t.Errorf("conflict workspace %s is outside the repository's pool", ws)
	}
	writeShipFile(t, ws, "c.txt", "trunk\na\n")
	mustRun(t, f.Env(), ws, "git", "add", "c.txt")

	_, _, err = runStackCmdIn(t, f, ws, "continue")
	if err == nil || !strings.Contains(err.Error(), "a is checked out in "+laneA) || !strings.Contains(err.Error(), stackResumeAdvice) {
		t.Fatalf("continue = %v, want the held parent refused with the resume step", err)
	}
	for branch, head := range heads {
		if got := gitAt(t, f.Env(), store, "rev-parse", branch); got != head {
			t.Errorf("%s moved to %s before the refusal", branch, got)
		}
	}
	mustRun(t, f.Env(), laneA, "git", "switch", "--detach", "-q")
	out, _, err := runStackCmdIn(t, f, laneB, "continue")
	if err != nil {
		t.Fatalf("continue after releasing the holder: %v", err)
	}
	t.Logf("continue: %s", out)
	if !thinOnto(t, f, store, "origin/main", "a") || !thinOnto(t, f, store, "a", "b") {
		t.Error("the stack did not land on the new trunk")
	}
	if got := gitAt(t, f.Env(), store, "show", "a:c.txt"); got != "trunk\na" {
		t.Errorf("a's c.txt = %q, want the resolution", got)
	}
	if got := gitAt(t, f.Env(), store, "show", "b:d.txt"); got != "b" {
		t.Errorf("b's d.txt = %q", got)
	}

	restackAdvanceRemote(t, f, "main", "e.txt", "trunk\n")
	refs, remote := thinRefs(t, f, store), thinRefs(t, f, f.RemoteDir)
	api := stubGTAPI(t)
	f.Decorate(api.ctx)
	out, _, err = runStackCmdIn(t, f, laneB, "rebase", "--dry-run")
	if err != nil {
		t.Fatalf("rebase --dry-run: %v", err)
	}
	if !strings.Contains(out, "a") || !strings.Contains(out, "b") {
		t.Errorf("dry run = %q, want both branches planned", out)
	}
	t.Logf("rebase --dry-run:\n%s", out)
	if heads := api.submitHeads(); len(heads) != 0 {
		t.Errorf("dry run submitted %v", heads)
	}
	if got := thinRefs(t, f, f.RemoteDir); got != remote {
		t.Errorf("dry run moved remote refs")
	}
	if got := thinRefs(t, f, store); !thinOnlyTrunkMoved(refs, got) {
		t.Errorf("dry run moved store refs past trunk:\n%s\n→\n%s", refs, got)
	}
	thinRequireSource(t, f, before)
}

func TestStackThinSubmitRecordsPushTracking(t *testing.T) {
	t.Parallel()
	f := thinRepo(t)
	stubOpenPRs(t, f, nil, "lane1")
	thinGrowTrunk(t, f, 6)
	store := thinTestStore(t, f)
	_, lane := thinNew(t, f, f.Dir, "lane1", "--thin", "--depth", strconv.Itoa(thinTestDepth))
	first := thinCommit(t, f, lane, "lane1.txt", "one\n")
	if _, errOut, err := runStackCmdIn(t, f, lane, "submit"); err != nil {
		t.Fatalf("submit: %v\n%s", err, errOut)
	}
	if got := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "lane1"); got != first {
		t.Fatalf("remote lane1 = %s, want %s", got, first)
	}
	if got := gitAt(t, f.Env(), store, "rev-parse", "refs/remotes/origin/lane1"); got != first {
		t.Errorf("origin/lane1 = %s, want the pushed %s", got, first)
	}
	if got := gitAt(t, f.Env(), store, "reflog", "show", "--format=%gs", "refs/remotes/origin/lane1"); !strings.Contains(got, "update by push") {
		t.Errorf("origin/lane1 reflog = %q, want git's push entry", got)
	}

	second := thinCommit(t, f, lane, "lane1.txt", "two\n")
	shipResetLog(t, f)
	if _, errOut, err := runStackCmdIn(t, f, lane, "submit"); err != nil {
		t.Fatalf("second submit: %v\n%s", err, errOut)
	}
	lookups := 0
	for _, argv := range vcstest.Invocations(t, f.ArgvLog) {
		if slices.Equal(argv, []string{"git", "cat-file", "--batch-check"}) {
			lookups++
		}
		if len(argv) > 1 && argv[1] == "for-each-ref" && slices.Contains(argv, "--format=%(refname)") {
			t.Errorf("submit enumerated refs for the adoption marks: %v", argv)
		}
		for _, arg := range argv {
			if strings.HasPrefix(arg, thinAdoptedPrefix) {
				t.Errorf("submit swept the adoption-mark namespace: %v", argv)
			}
		}
	}
	if lookups != 1 {
		t.Errorf("submit looked up adoption marks %d times, want one batched lookup", lookups)
	}
	if got := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "lane1"); got != second {
		t.Errorf("remote lane1 = %s, want %s", got, second)
	}
	if got := gitAt(t, f.Env(), store, "rev-parse", "refs/remotes/origin/lane1"); got != second {
		t.Errorf("origin/lane1 = %s, want %s", got, second)
	}
	mustRun(t, f.Env(), store, "git", "fetch", "-q")
	if got := gitAt(t, f.Env(), store, "config", "--get-all", "remote.origin.fetch"); got != "+refs/heads/main:refs/remotes/origin/main" {
		t.Errorf("store fetch specs = %q, want trunk alone", got)
	}
}

func TestStackNewThinAdoptsAPublishedParent(t *testing.T) {
	t.Parallel()
	f, receipt := thinPublishedParent(t)
	before := thinSnap(t, f)
	store := thinTestStore(t, f)
	child := thinTestLane(t, f, "child")
	args := []string{"new", "child", "--parent", "parent", "--published-parent"}

	_, _, err := runStackCmd(t, f, append(args, "--thin", "--depth", strconv.Itoa(thinTestDepth))...)
	if err == nil || !strings.Contains(err.Error(), "pass --deepen") {
		t.Fatalf("stack new = %v, want the base beyond the store refused", err)
	}
	depth := gitAt(t, f.Env(), store, "rev-list", "--count", "origin/main")
	if depth != strconv.Itoa(thinTestDepth) {
		t.Fatalf("store depth = %s, want %d", depth, thinTestDepth)
	}
	thinRequireNoChild(t, f, store, child)

	_, _, err = runStackCmd(t, f, append(args, "--thin", "--deepen", "--max-depth", "4")...)
	if err == nil || !strings.Contains(err.Error(), "raise --max-depth") {
		t.Fatalf("stack new = %v, want the capped deepen refused", err)
	}
	if got := gitAt(t, f.Env(), store, "rev-list", "--count", "origin/main"); got != strconv.Itoa(2*thinTestDepth) {
		t.Errorf("store depth after the capped deepen = %s, want %d", got, 2*thinTestDepth)
	}
	thinRequireNoChild(t, f, store, child)

	shipResetLog(t, f)
	out, lane := thinNew(t, f, f.Dir, append(args[1:], "--thin", "--deepen", "--max-depth", "64")...)
	if lane != child || !strings.Contains(out, "deepened "+store+" by 64 commits to reach "+shortOID(receipt.Base)) {
		t.Fatalf("stack new = %q, want the bounded deepen named", out)
	}
	for _, argv := range vcstest.Invocations(t, f.ArgvLog) {
		if len(argv) < 2 || argv[0] != "git" || !slices.Contains(argv, "fetch") {
			continue
		}
		for _, arg := range argv {
			if strings.HasPrefix(arg, "--depth") || strings.HasPrefix(arg, "--unshallow") || strings.HasPrefix(arg, "--shallow") {
				t.Errorf("fetch %v rewrote the store's boundary", argv)
			}
		}
	}
	if got := gitAt(t, f.Env(), store, "rev-parse", "refs/heads/parent"); got != receipt.Head {
		t.Errorf("store parent = %s, want its publication %s", got, receipt.Head)
	}
	if got := thinGTState(t, store, "parent"); got != "frozen" {
		t.Errorf("parent's gt state = %q, want frozen", got)
	}
	if got := gitAt(t, f.Env(), store, "rev-parse", stackPublicationRef("parent", "receipt")); got != receipt.OID {
		t.Errorf("store receipt = %s, want %s", got, receipt.OID)
	}
	if got := gitAt(t, f.Env(), store, "rev-parse", thinAdoptedRef("parent")); got != receipt.Head {
		t.Errorf("adoption mark = %s, want %s", got, receipt.Head)
	}
	if got := gitAt(t, f.Env(), child, "rev-parse", "HEAD"); got != receipt.Head {
		t.Errorf("child head = %s, want %s", got, receipt.Head)
	}
	if got := thinGTParent(t, f, child, "child"); got != "parent" {
		t.Errorf("child's gt parent = %s, want parent", got)
	}

	stubOpenPRs(t, f, nil, "parent", "child")
	head := thinCommit(t, f, child, "child.txt", "child work\n")
	if _, errOut, err := runStackCmdIn(t, f, child, "submit"); err != nil {
		t.Fatalf("child submit: %v\n%s", err, errOut)
	}
	if got := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "parent"); got != receipt.Head {
		t.Errorf("child submit moved the adopted parent to %s", got)
	}
	if got := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "child"); got != head {
		t.Errorf("remote child = %s, want %s", got, head)
	}
	if got := gitAt(t, f.Env(), store, "rev-parse", "refs/heads/parent"); got != receipt.Head {
		t.Errorf("child submit moved the store's parent to %s", got)
	}
	thinRequireSource(t, f, before)
}

var thinAdoptArgs = []string{"--parent", "parent", "--published-parent", "--thin", "--deepen", "--max-depth", "64"}

func thinRequireAdopted(t *testing.T, f *vcstest.Fixture, store string, receipt *stackPublication) {
	t.Helper()
	for ref, want := range map[string]string{
		"refs/heads/parent":                      receipt.Head,
		thinAdoptedRef("parent"):                 receipt.Head,
		stackPublicationRef("parent", "receipt"): receipt.OID,
	} {
		if got := gitAt(t, f.Env(), store, "rev-parse", ref); got != want {
			t.Errorf("store %s = %s, want %s", ref, got, want)
		}
	}
	row, ok := thinGTRowOf(t, store, "parent")
	if want := (thinGTRow{state: "frozen", parentRevision: receipt.Base, branchRevision: receipt.Head}); !ok || row != want {
		t.Errorf("parent's gt row = %+v, %v, want %+v", row, ok, want)
	}
}

func thinPublishParentAgain(t *testing.T, f *vcstest.Fixture, prior *stackPublication) *stackPublication {
	t.Helper()
	thinCommit(t, f, f.Dir, "parent.txt", "more parent work\n")
	if _, errOut, err := runStackCmd(t, f, "submit"); err != nil {
		t.Fatalf("source submit: %v\n%s", err, errOut)
	}
	receipt, err := stackReadPublication(f.Context(), render.Dir(f.Dir), "parent")
	if err != nil || receipt == nil || receipt.Head == prior.Head {
		t.Fatalf("second publication = %#v, %v, want a new head past %s", receipt, err, shortOID(prior.Head))
	}
	if got := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "parent"); got != receipt.Head {
		t.Fatalf("remote parent = %s, want the second publication %s", got, receipt.Head)
	}
	return receipt
}

func TestStackThinAdoptedParentAdvancesUnderAChild(t *testing.T) {
	t.Parallel()
	f, first := thinPublishedParent(t)
	stubOpenPRs(t, f, nil, "parent", "child1", "child2")
	store := thinTestStore(t, f)
	_, child1 := thinNew(t, f, f.Dir, append([]string{"child1"}, thinAdoptArgs...)...)
	own := thinCommit(t, f, child1, "child1.txt", "child1 work\n")
	thinRequireAdopted(t, f, store, first)

	second := thinPublishParentAgain(t, f, first)
	before := thinSnap(t, f)
	out, child2 := thinNew(t, f, f.Dir, append([]string{"child2"}, thinAdoptArgs...)...)
	if strings.Contains(out, "deepened") {
		t.Errorf("stack new = %q, want no deepen for a base the store already holds", out)
	}
	thinRequireAdopted(t, f, store, second)
	if got := gitAt(t, f.Env(), child2, "rev-parse", "HEAD"); got != second.Head {
		t.Errorf("child2 head = %s, want the refreshed parent %s", got, second.Head)
	}
	for lane, want := range map[string]string{"child1": first.Head, "child2": second.Head} {
		row, ok := thinGTRowOf(t, store, lane)
		if !ok || row.parentRevision != want {
			t.Errorf("%s's gt parent revision = %+v, %v, want the fork pinned at %s", lane, row, ok, shortOID(want))
		}
	}
	if got := gitAt(t, f.Env(), child1, "rev-parse", "HEAD"); got != own {
		t.Fatalf("refreshing the parent moved child1 to %s", got)
	}

	api := stubGTAPI(t)
	f.Decorate(api.ctx)
	out, errOut, err := runStackCmdIn(t, f, child1, "submit")
	if err != nil {
		t.Fatalf("child1 submit: %v\n%s", err, errOut)
	}
	t.Logf("child1 submit:\n%s", out)
	head := gitAt(t, f.Env(), child1, "rev-parse", "HEAD")
	if head == own || !thinOnto(t, f, store, second.Head, "child1") {
		t.Errorf("child1 = %s, want it replayed onto the parent's new head %s (was %s)", shortOID(head), shortOID(second.Head), shortOID(own))
	}
	if got := gitAt(t, f.Env(), store, "rev-list", "--count", "parent..child1"); got != "1" {
		t.Errorf("commits above parent = %s, want child1's one", got)
	}
	if got := gitAt(t, f.Env(), store, "diff", "--name-only", "parent", "child1"); got != "child1.txt" {
		t.Errorf("child1 changes %q above parent, want child1.txt alone", got)
	}
	if row, ok := thinGTRowOf(t, store, "child1"); !ok || row.parentRevision != second.Head {
		t.Errorf("child1's gt parent revision = %+v, %v, want the new fork %s", row, ok, shortOID(second.Head))
	}
	if got := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "parent"); got != second.Head {
		t.Errorf("remote parent = %s, want the source's publication %s untouched", got, second.Head)
	}
	if got := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "child1"); got != head {
		t.Errorf("remote child1 = %s, want %s", got, head)
	}
	if heads := api.submitHeads(); slices.Contains(heads, "parent") || !slices.Contains(heads, "child1") {
		t.Errorf("submitted %v, want child1 alone", heads)
	}
	thinRequireAdopted(t, f, store, second)
	for _, sha := range []string{first.Head, own} {
		if code := thinGitCode(t, f, store, "cat-file", "-e", sha+"^{commit}"); code != 0 {
			t.Errorf("the old fork %s left the store", shortOID(sha))
		}
	}
	thinRequireSource(t, f, before)
}

func TestStackThinPartialAdoptionNeverPushesTheParent(t *testing.T) {
	t.Parallel()
	f, receipt := thinPublishedParent(t)
	store := thinTestStore(t, f)
	child := thinTestLane(t, f, "child")
	thinNew(t, f, f.Dir, "lane1", "--parent", "main", "--thin", "--depth", strconv.Itoa(thinTestDepth))
	db := filepath.Join(store, ".git", ".graphite_metadata.db")
	if err := os.Chmod(db, 0o400); err != nil {
		t.Fatal(err)
	}
	_, _, err := runStackCmd(t, f, append([]string{"new", "child"}, thinAdoptArgs...)...)
	if chmodErr := os.Chmod(db, 0o600); chmodErr != nil {
		t.Fatal(chmodErr)
	}
	if err == nil || !strings.Contains(err.Error(), "gtmeta: adopt root") {
		t.Fatalf("stack new over a read-only gt database = %v, want the adoption refused", err)
	}
	if _, statErr := os.Stat(child); !os.IsNotExist(statErr) {
		t.Errorf("partial adoption left the lane %s: %v", child, statErr)
	}
	for _, ref := range []string{"refs/heads/parent", thinAdoptedRef("parent")} {
		if got := gitAt(t, f.Env(), store, "rev-parse", ref); got != receipt.Head {
			t.Errorf("store %s = %s, want %s", ref, got, receipt.Head)
		}
	}
	if row, ok := thinGTRowOf(t, store, "parent"); ok {
		t.Fatalf("partial adoption left parent a gt row %+v", row)
	}

	api := stubGTAPI(t)
	f.Decorate(api.ctx)
	ws := filepath.Join(t.TempDir(), "parent-ws")
	mustRun(t, f.Env(), store, "git", "worktree", "add", "-q", ws, "parent")
	orphan := thinCommit(t, f, ws, "parent.txt", "store work\n")
	mustRun(t, f.Env(), ws, "gt", "track", "-f", "--parent", "main", "--no-interactive")
	if got := thinGTState(t, store, "parent"); got == "frozen" {
		t.Fatalf("gt track left parent frozen")
	}
	remote := thinRefs(t, f, f.RemoteDir)
	_, errOut, err := runStackCmdIn(t, f, ws, "submit")
	if err == nil || !strings.Contains(err.Error(), "never pushes") || !strings.Contains(err.Error(), "parent") {
		t.Errorf("stack submit of the orphaned parent = %v\n%s, want the adoption mark refusing the push", err, errOut)
	}
	writeShipFile(t, ws, "parent.txt", "more store work\n")
	_, errOut, err = runShipCmdFull(f.ContextIn(ws), t, "--no-gt", "--no-watch", "--no-pr", "-m", "more store work")
	if err == nil || !strings.Contains(err.Error(), "never pushes") || !strings.Contains(err.Error(), "parent") {
		t.Errorf("ship of the orphaned parent = %v\n%s, want the adoption mark refusing the push", err, errOut)
	}
	if got := thinRefs(t, f, f.RemoteDir); got != remote {
		t.Errorf("the orphaned parent reached the remote:\n%s\n→\n%s", remote, got)
	}
	if heads := api.submitHeads(); len(heads) != 0 {
		t.Errorf("submitted %v, want nothing", heads)
	}
	if got := gitAt(t, f.Env(), store, "rev-parse", thinAdoptedRef("parent")); got != receipt.Head {
		t.Errorf("adoption mark = %s, want %s", got, receipt.Head)
	}

	if got := gitAt(t, f.Env(), store, "rev-parse", "refs/heads/parent"); got == receipt.Head || got == orphan {
		t.Errorf("store parent = %s, want the refused ship's commit past %s", got, shortOID(orphan))
	}
	mustRun(t, f.Env(), store, "git", "worktree", "remove", "--force", ws)
	mustRun(t, f.Env(), store, "git", "update-ref", "refs/heads/parent", receipt.Head)
	_, lane := thinNew(t, f, f.Dir, append([]string{"child"}, thinAdoptArgs...)...)
	if lane != child {
		t.Errorf("retried stack new cut %s, want %s", lane, child)
	}
	thinRequireAdopted(t, f, store, receipt)
	if got := thinGTParent(t, f, child, "child"); got != "parent" {
		t.Errorf("child's gt parent = %s, want parent", got)
	}
}

func thinRequireNoChild(t *testing.T, f *vcstest.Fixture, store, child string) {
	t.Helper()
	if _, err := os.Stat(child); !os.IsNotExist(err) {
		t.Errorf("refusal left the lane %s: %v", child, err)
	}
	for _, ref := range []string{"refs/heads/child", "refs/heads/parent", thinAdoptedRef("parent")} {
		if present, err := gitRefExists(f.ContextIn(store), render.Dir(store), "test", ref); err != nil || present {
			t.Errorf("refusal left %s in the store: %v %v", ref, present, err)
		}
	}
}

func TestStackRequireHistoryRefusesCutAncestry(t *testing.T) {
	t.Parallel()
	f := thinRepo(t)
	thinGrowTrunk(t, f, 12)
	store := thinTestStore(t, f)
	_, lane := thinNew(t, f, f.Dir, "lane1", "--thin", "--depth", strconv.Itoa(thinTestDepth))
	whole := thinCommit(t, f, lane, "lane1.txt", "lane\n")

	mustRun(t, f.Env(), f.Dir, "git", "switch", "-qc", "deep", "origin/main~10")
	thinCommit(t, f, f.Dir, "deep.txt", "deep\n")
	mustRun(t, f.Env(), f.Dir, "git", "push", "-q", "origin", "deep")
	mustRun(t, f.Env(), store, "git", "fetch", "-q", "origin", "+refs/heads/deep:refs/heads/deep")
	cut := gitAt(t, f.Env(), store, "rev-parse", "deep")

	ctx := f.ContextIn(lane)
	if err := stackRequireHistory(ctx, render.Dir(lane), "test", "origin", "main", map[string]string{"lane1": whole}); err != nil {
		t.Errorf("whole lane refused: %v", err)
	}
	err := stackRequireHistory(ctx, render.Dir(lane), "test", "origin", "main", map[string]string{"lane1": whole, "deep": cut})
	if err == nil || !strings.Contains(err.Error(), "deep's history") || !strings.Contains(err.Error(), "git -C "+store+" fetch --deepen=<commits> origin main") {
		t.Fatalf("cut ancestry = %v, want a refusal naming the explicit deepen", err)
	}
	if err := stackRequireHistory(f.Context(), render.Dir(f.Dir), "test", "origin", "main", map[string]string{"deep": gitAt(t, f.Env(), f.Dir, "rev-parse", "deep")}); err != nil {
		t.Errorf("full checkout refused: %v", err)
	}

	mustRun(t, f.Env(), lane, "git", "switch", "-q", "deep")
	writeShipFile(t, lane, "deep.txt", "more deep\n")
	remote := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "deep")
	_, errOut, err := runShipCmdFull(f.ContextIn(lane), t, "--no-gt", "--no-watch", "--no-pr", "-m", "deep work")
	if err == nil || !strings.Contains(err.Error(), "shallow boundary") {
		t.Fatalf("ship over cut ancestry = %v\n%s, want the history refusal", err, errOut)
	}
	if got := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "deep"); got != remote {
		t.Errorf("refused ship pushed deep to %s", got)
	}
}

func thinOnlyTrunkMoved(before, after string) bool {
	keep := func(s string) []string {
		var lines []string
		for line := range strings.Lines(s) {
			if !strings.HasPrefix(line, "refs/remotes/origin/main ") && !strings.HasPrefix(line, "refs/remotes/origin/HEAD ") {
				lines = append(lines, line)
			}
		}
		return lines
	}
	return slices.Equal(keep(before), keep(after))
}

func TestStackNewStorageModes(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		env     string
		noGT    bool
		inStore bool
		args    []string
		refusal string
		store   bool
		sparse  bool
	}{
		{name: "unset Git default", args: []string{"--parent", "main"}, store: true, sparse: true},
		{name: "unset plain Git default", noGT: true, args: []string{"--parent", "main"}, store: true, sparse: true},
		{name: "explicit full history with unset env", args: []string{"--parent", "main", "--full-history"}},
		{name: "env thin", env: "thin", args: []string{"--parent", "main"}, store: true, sparse: true},
		{name: "env full", env: "full", args: []string{"--parent", "main"}},
		{name: "full history overrides env thin", env: "thin", args: []string{"--parent", "main", "--full-history"}},
		{name: "env bogus", env: "shallow", args: []string{"--parent", "main"}, refusal: `CCX_STACK_NEW="shallow" is neither thin nor full`},
		{name: "thin and full history", args: []string{"--thin", "--full-history"}, refusal: "none of the others can be"},
		{name: "thin and no checkout", args: []string{"--thin", "--no-checkout"}, refusal: "none of the others can be"},
		{name: "unset default and no checkout", args: []string{"--parent", "main", "--no-checkout"}, refusal: "--no-checkout conflicts with thin storage"},
		{name: "env thin and no checkout", env: "thin", args: []string{"--parent", "main", "--no-checkout"}, refusal: "--no-checkout conflicts with thin storage"},
		{name: "include without sparse", env: "full", args: []string{"--parent", "main", "--include", "keep"}, refusal: "--include checks out directories in a sparse lane"},
		{name: "unset default source-only parent", args: []string{"--parent", "parent"}, refusal: "pass --published-parent"},
		{name: "unpublished source-only parent", args: []string{"--thin", "--parent", "parent"}, refusal: "pass --published-parent"},
		{name: "full history in the store", inStore: true, args: []string{"--full-history"}, refusal: "is a thin store"},
		{name: "depth in the store", inStore: true, args: []string{"--depth", "8"}, refusal: "--depth and --deepen apply only"},
		{name: "deepen without thin", env: "full", args: []string{"--parent", "main", "--deepen"}, refusal: "--depth and --deepen apply only"},
		{name: "env full in the store", env: "full", inStore: true, store: true, sparse: true},
		{name: "env thin and no checkout in the store", env: "thin", inStore: true, args: []string{"--no-checkout"}, store: true, sparse: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := thinRepo(t, "parent")
			mustRun(t, f.Env(), f.Dir, "git", "switch", "-q", "main")
			thinGrowTrunk(t, f, 6)
			dir := f.Dir
			if tt.inStore {
				_, dir = thinNew(t, f, f.Dir, "base", "--thin", "--depth", strconv.Itoa(thinTestDepth))
			}
			f.Setenv(stackNewEnv, tt.env)
			if tt.noGT {
				mustRun(t, f.Env(), f.Dir, "git", "config", nogtKey, "true")
			}
			refs := thinRefs(t, f, f.Dir)
			out, _, err := runStackCmdIn(t, f, dir, append([]string{"new", "child"}, tt.args...)...)
			if tt.refusal != "" {
				if err == nil || !strings.Contains(err.Error(), tt.refusal) {
					t.Fatalf("stack new = %q, %v, want a refusal containing %q", out, err, tt.refusal)
				}
				if got := thinRefs(t, f, f.Dir); got != refs {
					t.Errorf("refusal moved source refs")
				}
				if _, err := os.Stat(thinTestLane(t, f, "child")); !os.IsNotExist(err) {
					t.Errorf("refusal left a lane: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("stack new: %v", err)
			}
			segs := strings.Split(out, shipSep)
			lane := segs[len(segs)-1]
			common := gitAt(t, f.Env(), lane, "rev-parse", "--path-format=absolute", "--git-common-dir")
			if got := common == filepath.Join(thinTestStore(t, f), ".git"); got != tt.store {
				t.Errorf("lane in the thin store = %v, want %v (common dir %s)", got, tt.store, common)
			}
			if sparse := thinGitCode(t, f, lane, "config", "--get", "core.sparseCheckout") == 0; sparse != tt.sparse {
				t.Errorf("lane sparse = %v, want %v", sparse, tt.sparse)
			}
			if slices.Contains(tt.args, "--no-checkout") {
				if entries, err := os.ReadDir(lane); err != nil || len(entries) != 1 || entries[0].Name() != ".git" {
					t.Errorf("no-checkout lane materialized %v, %v", entries, err)
				}
			}
		})
	}
}

func TestWorktreeRmRemovesAThinLaneFromTheSource(t *testing.T) {
	t.Parallel()
	f := thinRepo(t)
	thinGrowTrunk(t, f, 6)
	store := thinTestStore(t, f)
	_, byName := thinNew(t, f, f.Dir, "byname", "--thin", "--depth", strconv.Itoa(thinTestDepth))
	_, byPath := thinNew(t, f, f.Dir, "bypath", "--thin")
	fixtureCleanup(t, f)

	if _, err := runWorktreeCmdIn(f.Context(), t, "rm", "byname"); err != nil {
		t.Fatalf("rm byname: %v", err)
	}
	if _, err := runWorktreeCmdIn(f.Context(), t, "rm", "--path", byPath); err != nil {
		t.Fatalf("rm --path: %v", err)
	}
	for _, lane := range []string{byName, byPath} {
		if _, err := os.Stat(lane); !os.IsNotExist(err) {
			t.Errorf("%s still on disk after rm: %v", lane, err)
		}
		if worktreeRegistered(t, f.Env(), store, lane) {
			t.Errorf("store still registers %s", lane)
		}
	}
	if got := gitAt(t, f.Env(), store, "rev-parse", "--is-shallow-repository"); got != "true" {
		t.Errorf("store after rm reads shallow = %q", got)
	}
	for _, branch := range []string{"byname", "bypath"} {
		if present, err := gitRefExists(f.ContextIn(store), render.Dir(store), "test", "refs/heads/"+branch); err != nil || !present {
			t.Errorf("rm dropped the store's branch %s: %v %v", branch, present, err)
		}
	}
	if _, err := runWorktreeCmdIn(f.Context(), t, "rm", "byname"); err == nil {
		t.Error("rm of a removed lane succeeded")
	}
}

func TestStackNewThinFootprint(t *testing.T) {
	t.Parallel()
	f := thinRepo(t)
	thinGrowTrunk(t, f, 24)
	shipResetLog(t, f)
	thinNew(t, f, f.Dir, "lane1", "--thin", "--depth", strconv.Itoa(thinTestDepth))
	thinCalls := len(vcstest.Invocations(t, f.ArgvLog))
	shipResetLog(t, f)
	thinNew(t, f, f.Dir, "lane2", "--full-history", "--parent", "main")
	fullCalls := len(vcstest.Invocations(t, f.ArgvLog))

	full := filepath.Join(t.TempDir(), "full")
	mustRun(t, f.Env(), filepath.Dir(full), "git", "clone", "-q", "--no-local", "--no-checkout", f.RemoteDir, full)
	thinObjects, thinBytes := thinFootprint(t, f, thinTestStore(t, f))
	fullObjects, fullBytes := thinFootprint(t, f, full)
	t.Logf("thin store: %d objects, %d KiB in %d git/gt calls; full clone: %d objects, %d KiB; full-history lane: %d calls", thinObjects, thinBytes, thinCalls, fullObjects, fullBytes, fullCalls)
	if thinObjects >= fullObjects || thinBytes >= fullBytes {
		t.Errorf("thin store holds %d objects in %d KiB, want fewer than the full clone's %d in %d KiB", thinObjects, thinBytes, fullObjects, fullBytes)
	}
}

func thinFootprint(t *testing.T, f *vcstest.Fixture, dir string) (int, int) {
	t.Helper()
	stats := map[string]int{}
	for line := range strings.Lines(gitAt(t, f.Env(), dir, "count-objects", "-v")) {
		key, value, _ := strings.Cut(strings.TrimSpace(line), ": ")
		n, err := strconv.Atoi(value)
		if err != nil {
			t.Fatalf("count-objects %q: %v", line, err)
		}
		stats[key] = n
	}
	return stats["count"] + stats["in-pack"], stats["size"] + stats["size-pack"]
}

func TestThinIsStoreRequiresTheDerivedPath(t *testing.T) {
	t.Parallel()
	f := thinRepo(t)
	thinGrowTrunk(t, f, 6)
	store := thinTestStore(t, f)
	_, lane := thinNew(t, f, f.Dir, "lane1", "--thin", "--depth", strconv.Itoa(thinTestDepth))
	decoy := filepath.Join(filepath.Dir(filepath.Dir(store)), "0123456789ab", filepath.Base(store))
	mustRun(t, f.Env(), filepath.Dir(store), "git", "clone", "-q", "--no-local", f.RemoteDir, decoy)
	head := gitAt(t, f.Env(), decoy, "rev-parse", "HEAD")
	for _, tt := range []struct {
		dir  string
		want bool
	}{{store, true}, {lane, true}, {f.Dir, false}} {
		ck, err := vcs.ResolveCheckout(tt.dir)
		if err != nil {
			t.Fatal(err)
		}
		if got, err := thinIsStore(f.ContextIn(tt.dir), ck); err != nil || got != tt.want {
			t.Errorf("thinIsStore(%s) = %v, %v, want %v", tt.dir, got, err, tt.want)
		}
	}
	ck, err := vcs.ResolveCheckout(decoy)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := thinIsStore(f.ContextIn(decoy), ck); err == nil || !strings.Contains(err.Error(), "derives "+store) {
		t.Errorf("thinIsStore(decoy) = %v, %v, want a refusal naming the store its remote derives", got, err)
	}
	if err := thinRecordPush(f.ContextIn(decoy), render.Dir(decoy), "origin", map[string]string{"decoy": head}); err == nil {
		t.Error("push tracking accepted a store-shaped checkout whose remote derives another store")
	}
	if present, err := gitRefExists(f.ContextIn(decoy), render.Dir(decoy), "test", "refs/remotes/origin/decoy"); err != nil || present {
		t.Errorf("push tracking wrote into a checkout that only sits where a store would: %v %v", present, err)
	}
}

func TestThinHistoryProbesPropagateFailures(t *testing.T) {
	t.Parallel()
	f := thinRepo(t)
	thinGrowTrunk(t, f, 6)
	store := thinTestStore(t, f)
	thinNew(t, f, f.Dir, "lane1", "--thin", "--depth", strconv.Itoa(thinTestDepth))
	ctx, dir := f.ContextIn(store), render.Dir(store)
	if ok, err := thinReachable(ctx, dir, strings.Repeat("0", 40), "refs/remotes/origin/main"); err != nil || ok {
		t.Errorf("absent commit = %v, %v, want unreachable without error", ok, err)
	}
	tree := gitAt(t, f.Env(), store, "rev-parse", "HEAD^{tree}")
	if _, err := thinReachable(ctx, dir, tree, "refs/remotes/origin/main"); err == nil {
		t.Error("a tree passed for a commit was taken as unreachable, want an error")
	}
	notRepo := t.TempDir()
	if _, err := thinReachable(f.ContextIn(notRepo), render.Dir(notRepo), strings.Repeat("0", 40), "HEAD"); err == nil {
		t.Error("a failing cat-file was taken as an absent commit, want an error")
	}
	shallow, err := stackShallowSet(filepath.Join(store, ".git"))
	if err != nil || len(shallow) == 0 {
		t.Fatalf("shallow set = %v, %v", shallow, err)
	}
	if _, err := stackRangeWhole(ctx, dir, shallow, "refs/heads/no-such-branch"); err == nil {
		t.Error("a failing rev-list was taken as a cut range, want an error")
	}
	err = stackRequireHistory(ctx, dir, "test", "origin", "main", map[string]string{"ghost": "refs/heads/no-such-branch"})
	if err == nil || strings.Contains(err.Error(), "shallow boundary") {
		t.Errorf("a failing merge-base = %v, want the git failure, not a history refusal", err)
	}
}

func TestStackNewThinRefusesADivergentSameNamedParent(t *testing.T) {
	t.Parallel()
	f := thinRepo(t, "p")
	thinGrowTrunk(t, f, 6)
	store := thinTestStore(t, f)
	_, native := thinNew(t, f, f.Dir, "p", "--thin", "--depth", strconv.Itoa(thinTestDepth), "--parent", "main")
	thinCommit(t, f, native, "native.txt", "store's own p\n")
	source, storeHead := gitAt(t, f.Env(), f.Dir, "rev-parse", "p"), gitAt(t, f.Env(), store, "rev-parse", "p")
	if source == storeHead {
		t.Fatal("fixture needs the source and store p to differ")
	}
	storeRefs, sourceRefs := thinRefs(t, f, store), thinRefs(t, f, f.Dir)

	_, _, err := runStackCmd(t, f, "new", "child", "--thin")
	if err == nil || !strings.Contains(err.Error(), "has its own p at "+shortOID(storeHead)) || !strings.Contains(err.Error(), "at "+shortOID(source)) {
		t.Fatalf("stack new = %v, want the divergent same-named parent refused", err)
	}
	if got := thinRefs(t, f, store); got != storeRefs {
		t.Errorf("refusal moved store refs:\n%s\n→\n%s", storeRefs, got)
	}
	if got := thinRefs(t, f, f.Dir); got != sourceRefs {
		t.Errorf("refusal moved source refs")
	}
	if _, err := os.Stat(thinTestLane(t, f, "child")); !os.IsNotExist(err) {
		t.Errorf("refusal left a lane: %v", err)
	}
	if out, lane := thinNew(t, f, native, "child"); gitAt(t, f.Env(), lane, "rev-parse", "HEAD") != gitAt(t, f.Env(), store, "rev-parse", "p") {
		t.Errorf("child cut from the store's own p = %q, want it on the store's p", out)
	}
}

func thinPushFiles(t *testing.T, f *vcstest.Fixture, files map[string]string, executable ...string) {
	t.Helper()
	clone := filepath.Join(t.TempDir(), "upstream")
	mustRun(t, f.Env(), filepath.Dir(clone), "git", "clone", "-q", "--branch", "main", f.RemoteDir, clone)
	for _, kv := range [][2]string{{"user.email", "t@t.t"}, {"user.name", "t"}, {"commit.gpgsign", "false"}} {
		mustRun(t, f.Env(), clone, "git", "config", kv[0], kv[1])
	}
	for name, content := range files {
		writeShipFile(t, clone, name, content)
	}
	mustRun(t, f.Env(), clone, "git", "add", "-A")
	for _, name := range executable {
		mustRun(t, f.Env(), clone, "git", "update-index", "--chmod=+x", name)
	}
	mustRun(t, f.Env(), clone, "git", "commit", "-qm", "metadata")
	mustRun(t, f.Env(), clone, "git", "push", "-q", "origin", "main")
	mustRun(t, f.Env(), f.Dir, "git", "fetch", "-q", "origin")
}

func TestStackNewThinHydratesAgentMetadata(t *testing.T) {
	t.Parallel()
	f := thinRepo(t)
	thinGrowTrunk(t, f, 6)
	marker := filepath.Join(t.TempDir(), "ran")
	hook := "#!/bin/sh\ntouch '" + marker + "'\n"
	thinPushFiles(t, f, map[string]string{
		".claude/settings.json":          `{"hooks":{"SessionStart":[{"hooks":[{"type":"command","command":".claude/hooks/session-start.sh"}]}]}}` + "\n",
		".claude/hooks/session-start.sh": hook,
		".agents/skills/demo/SKILL.md":   "---\nname: demo\n---\nDemo skill.\n",
		"scripts/setup.sh":               hook + "echo setup\n",
		"docs/object-hierarchy.md":       "# Objects\n",
		"CLAUDE.md":                      "@docs/object-hierarchy.md\n",
	}, ".claude/hooks/session-start.sh", "scripts/setup.sh")
	before := thinSnap(t, f)
	store := thinTestStore(t, f)

	_, lane := thinNew(t, f, f.Dir, "lane1", "--thin", "--depth", strconv.Itoa(thinTestDepth))
	for _, name := range []string{"CLAUDE.md", ".claude/settings.json", ".claude/hooks/session-start.sh", ".agents/skills/demo/SKILL.md"} {
		if _, err := os.Stat(filepath.Join(lane, name)); err != nil {
			t.Errorf("%s missing from the thin lane: %v", name, err)
		}
	}
	if info, err := os.Stat(filepath.Join(lane, ".claude", "hooks", "session-start.sh")); err != nil || info.Mode()&0o111 == 0 {
		t.Errorf("project hook lost its executable bit: %v %v", info, err)
	}
	for _, dir := range []string{"scripts", "docs", "excluded", "keep"} {
		if _, err := os.Stat(filepath.Join(lane, dir)); !os.IsNotExist(err) {
			t.Errorf("%s materialized in the thin lane: %v", dir, err)
		}
	}
	for _, name := range []string{"scripts/setup.sh", "docs/object-hierarchy.md"} {
		blob := gitAt(t, f.Env(), lane, "rev-parse", "HEAD:"+name)
		if code := thinGitCode(t, f, lane, "cat-file", "-e", blob); code == 0 {
			t.Errorf("%s's blob reached the store", name)
		}
	}

	writeShipExecutable(t, filepath.Join(store, ".git", "hooks"), "post-checkout", hook)
	_, nested := thinNew(t, f, lane, "lane2")
	if _, err := os.Stat(filepath.Join(nested, ".agents", "skills", "demo", "SKILL.md")); err != nil {
		t.Errorf("nested lane lost the project skill: %v", err)
	}
	_, second := thinNew(t, f, f.Dir, "lane3", "--thin")
	if _, err := os.Stat(filepath.Join(second, ".claude", "settings.json")); err != nil {
		t.Errorf("second lane lost the project settings: %v", err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Errorf("lane creation ran a hook or setup script: %v", err)
	}

	mustRun(t, f.Env(), lane, "git", "sparse-checkout", "add", "docs")
	if got, err := os.ReadFile(filepath.Join(lane, "docs", "object-hierarchy.md")); err != nil || string(got) != "# Objects\n" {
		t.Errorf("explicit hydration of an imported doc = %q, %v", got, err)
	}
	thinRequireSource(t, f, before)
}

func TestStackThinRefusesPushAfterStoreIdentityChanges(t *testing.T) {
	t.Parallel()
	f := thinRepo(t)
	stubOpenPRs(t, f, nil, "lane1")
	thinGrowTrunk(t, f, 6)
	store := thinTestStore(t, f)
	_, lane := thinNew(t, f, f.Dir, "lane1", "--thin", "--depth", strconv.Itoa(thinTestDepth))
	thinCommit(t, f, lane, "lane1.txt", "one\n")
	other := filepath.Join(t.TempDir(), "other.git")
	mustRun(t, f.Env(), filepath.Dir(other), "git", "clone", "-q", "--bare", f.RemoteDir, other)
	remote, mirror := thinRefs(t, f, f.RemoteDir), thinRefs(t, f, other)

	mustRun(t, f.Env(), store, "git", "remote", "set-url", "origin", other)
	_, _, err := runStackCmdIn(t, f, lane, "submit")
	if err == nil || !strings.Contains(err.Error(), "sits where ccx keeps thin stores") || !strings.Contains(err.Error(), "derives") {
		t.Fatalf("submit after the store's origin changed = %v, want the identity refusal", err)
	}
	if got := thinRefs(t, f, other); got != mirror {
		t.Errorf("refused submit pushed to the new origin:\n%s\n→\n%s", mirror, got)
	}
	if got := thinRefs(t, f, f.RemoteDir); got != remote {
		t.Errorf("refused submit pushed to the old origin")
	}

	mustRun(t, f.Env(), store, "git", "remote", "remove", "origin")
	ck, err := vcs.ResolveCheckout(lane)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := thinIsStore(f.ContextIn(lane), ck); err == nil || !strings.Contains(err.Error(), "has no origin remote") {
		t.Errorf("thinIsStore without an origin = %v, %v, want the identity refusal", got, err)
	}
	if err := thinRefuseAdoptedPush(f.ContextIn(lane), render.Dir(lane), "push", []string{"lane1"}); err == nil {
		t.Error("the adopted-parent guard passed a store whose identity it could not establish")
	}
}
