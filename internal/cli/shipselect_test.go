package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/yasyf/cc-context/anchor"
	"github.com/yasyf/cc-context/internal/hunk"
	"github.com/yasyf/cc-context/internal/vcs"
)

const (
	hunkBase    = "a\nb\nc\nd\ne\n"
	hunkCurrent = "A\nb\nc\nd\nE\n"
)

// hunkRefFor renders the post-image ref (path:A-B#digest) for the i-th hunk
// between base and current, matching what ccx vcs hunks prints.
func hunkRefFor(t *testing.T, path, base, current string, i int) string {
	t.Helper()
	hunks := hunk.Compute([]byte(base), []byte(current))
	if i < 0 || i >= len(hunks) {
		t.Fatalf("hunk index %d out of range (%d hunks)", i, len(hunks))
	}
	return path + ":" + hunkRange(hunks[i]) + "#" + hunks[i].Digest.String()
}

// hunkRange renders a hunk's post-image line range as "A-B" (or "A").
func hunkRange(h hunk.Hunk) string {
	if h.NewEnd > h.NewStart {
		return fmt.Sprintf("%d-%d", h.NewStart, h.NewEnd)
	}
	return strconv.Itoa(h.NewStart)
}

// setupHunkShip stands up a jj ship with a single hunk-scoped file on disk and
// its committed base wired into the fake jj `file show`, returning the ship log.
func setupHunkShip(t *testing.T, file string) string {
	t.Helper()
	log := setupShip(t, ".jj", false)
	if err := os.WriteFile(file, []byte(hunkCurrent), 0o644); err != nil { //nolint:gosec // test fixture file
		t.Fatalf("write %s: %v", file, err)
	}
	t.Setenv("JJ_FILE_SHOW_BASE", hunkBase)
	return log
}

// assertJJSelectCommit checks a jj commit/squash argv that drives the
// apply-selection diff tool: the fixed merge-tool wiring exactly, the dynamic
// program and plan-file args by shape, and the trailing message/path args exact.
func assertJJSelectCommit(t *testing.T, inv []string, verb string, tail []string) {
	t.Helper()
	if len(inv) < 10 {
		t.Fatalf("jj select commit argv too short: %v", inv)
	}
	for i, w := range []string{"jj", verb, "--config"} {
		if inv[i] != w {
			t.Errorf("commit argv[%d] = %q, want %q", i, inv[i], w)
		}
	}
	if !strings.HasPrefix(inv[3], `merge-tools.ccx-ship-select.program="`) || !strings.HasSuffix(inv[3], `"`) {
		t.Errorf("program config = %q, want a TOML-quoted program path", inv[3])
	}
	if inv[4] != "--config" {
		t.Errorf("commit argv[4] = %q, want --config", inv[4])
	}
	if !strings.HasPrefix(inv[5], `merge-tools.ccx-ship-select.edit-args=[`) {
		t.Errorf("edit-args config = %q, want a TOML array", inv[5])
	}
	for _, sub := range []string{`"vcs"`, `"apply-selection"`, `"--plan"`, `"$left"`, `"$right"`} {
		if !strings.Contains(inv[5], sub) {
			t.Errorf("edit-args %q missing %q", inv[5], sub)
		}
	}
	for i, w := range []string{"--config", "ui.diff-instructions=false", "--tool", "ccx-ship-select"} {
		if inv[6+i] != w {
			t.Errorf("commit argv[%d] = %q, want %q", 6+i, inv[6+i], w)
		}
	}
	if got := inv[10:]; !reflect.DeepEqual(got, tail) {
		t.Errorf("commit argv tail = %v, want %v", got, tail)
	}
}

func TestShipJJHunkArgv(t *testing.T) {
	log := setupHunkShip(t, "f.txt")
	ref := hunkRefFor(t, "f.txt", hunkBase, hunkCurrent, 0)

	if _, err := runShipCmd(t, "-m", "fix: frobnicate", "--no-push", "--skip-hunk", ref, "f.txt"); err != nil {
		t.Fatalf("ship error = %v", err)
	}
	inv := readInvocations(t, log)
	if len(inv) != 5 {
		t.Fatalf("want 5 jj invocations, got %d: %v", len(inv), inv)
	}
	if want := []string{"jj", "root"}; !reflect.DeepEqual(inv[0], want) {
		t.Errorf("repo root = %v, want %v", inv[0], want)
	}
	if want := []string{"jj", "file", "list", "-r", "@-", "--", `root:"f.txt"`}; !reflect.DeepEqual(inv[1], want) {
		t.Errorf("base existence probe = %v, want %v", inv[1], want)
	}
	if want := []string{"jj", "file", "show", "-r", "@-", "--", `root:"f.txt"`}; !reflect.DeepEqual(inv[2], want) {
		t.Errorf("pre-flight base read = %v, want %v", inv[2], want)
	}
	assertJJSelectCommit(t, inv[3], "commit", []string{"-m", "fix: frobnicate", "--", "f.txt"})
	if want := []string{"jj", "log", "-r", "@-", "--no-graph", "-T", jjDescribeTemplate}; !reflect.DeepEqual(inv[4], want) {
		t.Errorf("describe = %v, want %v", inv[4], want)
	}
}

func TestShipJJHunkOnlyArgv(t *testing.T) {
	log := setupHunkShip(t, "f.txt")
	ref := hunkRefFor(t, "f.txt", hunkBase, hunkCurrent, 0)

	if _, err := runShipCmd(t, "-m", "fix: frobnicate", "--no-push", "--only-hunk", ref, "f.txt"); err != nil {
		t.Fatalf("ship error = %v", err)
	}
	inv := readInvocations(t, log)
	if len(inv) != 5 {
		t.Fatalf("want 5 jj invocations, got %d: %v", len(inv), inv)
	}
	assertJJSelectCommit(t, inv[3], "commit", []string{"-m", "fix: frobnicate", "--", "f.txt"})
}

func TestShipHunkHooksAreReportedSkipped(t *testing.T) {
	log := setupHunkShip(t, "f.txt")
	root, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	writeShipHookFiles(t, root)
	ref := hunkRefFor(t, "f.txt", hunkBase, hunkCurrent, 0)

	got, err := runShipCmd(t, "-m", "fix: frobnicate", "--no-push", "--only-hunk", ref, "f.txt")
	if err != nil {
		t.Fatalf("ship error = %v", err)
	}
	want := `hooks hunk-skip · committed a1b2c3d "fix: frobnicate" · not pushed`
	if got != want {
		t.Errorf("summary = %q, want %q", got, want)
	}
	for _, inv := range readInvocations(t, log) {
		if inv[0] == "uvx" {
			t.Errorf("uvx invoked for a hunk-scoped ship: %v", inv)
		}
	}
}

func TestShipHunkNoVerifySilencesHookSegment(t *testing.T) {
	log := setupHunkShip(t, "f.txt")
	root, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	writeShipHookFiles(t, root)
	ref := hunkRefFor(t, "f.txt", hunkBase, hunkCurrent, 0)

	got, err := runShipCmd(t, "-m", "fix: frobnicate", "--no-push", "--no-verify", "--only-hunk", ref, "f.txt")
	if err != nil {
		t.Fatalf("ship error = %v", err)
	}
	want := `committed a1b2c3d "fix: frobnicate" · not pushed`
	if got != want {
		t.Errorf("summary = %q, want %q", got, want)
	}
	for _, inv := range readInvocations(t, log) {
		if inv[0] == "uvx" {
			t.Errorf("uvx invoked despite --no-verify: %v", inv)
		}
	}
}

func TestShipJJHunkAmendArgv(t *testing.T) {
	tests := []struct {
		name string
		args []string
		tail []string
	}{
		{"amend with message", []string{"--amend", "-m", "fix: frobnicate"}, []string{"-m", "fix: frobnicate", "--", "f.txt"}},
		{"amend no message", []string{"--amend"}, []string{"--use-destination-message", "--", "f.txt"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			log := setupHunkShip(t, "f.txt")
			ref := hunkRefFor(t, "f.txt", hunkBase, hunkCurrent, 0)
			args := append(append([]string{}, tt.args...), "--no-push", "--skip-hunk", ref, "f.txt")
			if _, err := runShipCmd(t, args...); err != nil {
				t.Fatalf("ship error = %v", err)
			}
			inv := readInvocations(t, log)
			if len(inv) != 5 {
				t.Fatalf("want 5 jj invocations, got %d: %v", len(inv), inv)
			}
			assertJJSelectCommit(t, inv[3], "squash", tt.tail)
		})
	}
}

func TestShipHunkRefusals(t *testing.T) {
	ref0 := hunkRefFor(t, "f.txt", hunkBase, hunkCurrent, 0)
	ref1 := hunkRefFor(t, "f.txt", hunkBase, hunkCurrent, 1)
	driftHash := hunk.Compute([]byte("x\n"), []byte("Y\n"))[0].Digest
	driftRef := "f.txt:1#" + driftHash.String()
	rootOnly := [][]string{{"jj", "root"}}
	resolveSeq := [][]string{
		{"jj", "root"},
		{"jj", "file", "list", "-r", "@-", "--", `root:"f.txt"`},
		{"jj", "file", "show", "-r", "@-", "--", `root:"f.txt"`},
	}

	tests := []struct {
		name    string
		args    []string
		wantErr string
		wantInv [][]string // nil = no VCS command ran
	}{
		{
			name:    "mutually exclusive flags",
			args:    []string{"--skip-hunk", ref0, "--only-hunk", ref1, "f.txt"},
			wantErr: "ship: --skip-hunk and --only-hunk cannot be combined",
		},
		{
			name:    "malformed ref",
			args:    []string{"--skip-hunk", "not-a-ref", "f.txt"},
			wantErr: `ship: invalid hunk ref "not-a-ref" (expected file:A-B#hash, from ccx vcs hunks)`,
		},
		{
			name:    "ref outside shipped paths",
			args:    []string{"--skip-hunk", ref0, "other.txt"},
			wantErr: "is outside the shipped paths",
			wantInv: rootOnly,
		},
		{
			name:    "drift",
			args:    []string{"--skip-hunk", driftRef, "f.txt"},
			wantErr: "the diff changed since listing; re-run: ccx vcs hunks f.txt",
			wantInv: resolveSeq,
		},
		{
			name:    "all excluded",
			args:    []string{"--skip-hunk", ref0, "--skip-hunk", ref1, "f.txt"},
			wantErr: "ship: all changes excluded in f.txt; drop the file from the ship instead",
			wantInv: resolveSeq,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			log := setupHunkShip(t, "f.txt")
			args := append([]string{"-m", "fix: frobnicate", "--no-push"}, tt.args...)
			_, err := runShipCmd(t, args...)
			if err == nil {
				t.Fatal("expected refusal, got nil")
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error = %q, want it to contain %q", err, tt.wantErr)
			}
			if got := readInvocations(t, log); !reflect.DeepEqual(got, tt.wantInv) {
				t.Errorf("invocations = %v, want %v (no mutating command may run)", got, tt.wantInv)
			}
		})
	}
}

// setupGitHunkShip stands up a git ship with a hunk-scoped file on disk (plus an
// optional whole-shipped sibling) and its committed base wired into the fake git
// `show`, returning the ship log.
func setupGitHunkShip(t *testing.T, hunkFile, wholeFile string) string {
	t.Helper()
	log := setupShip(t, ".git", false)
	if err := os.WriteFile(hunkFile, []byte(hunkCurrent), 0o644); err != nil { //nolint:gosec // test fixture file
		t.Fatalf("write %s: %v", hunkFile, err)
	}
	if wholeFile != "" {
		if err := os.WriteFile(wholeFile, []byte("whole\n"), 0o644); err != nil { //nolint:gosec // test fixture file
			t.Fatalf("write %s: %v", wholeFile, err)
		}
	}
	t.Setenv("GIT_FILE_SHOW_BASE", hunkBase)
	return log
}

// gitIdxCarriers walks a ship log, returning the git argv sequence with the "idx"
// marker records stripped, plus the set of git subcommands whose invocation carried
// the temp index (marked by a preceding "idx" record).
func gitIdxCarriers(t *testing.T, log string) (seq [][]string, idx map[string]bool) {
	t.Helper()
	idx = map[string]bool{}
	pending := false
	for _, rec := range readInvocations(t, log) {
		if rec[0] == "idx" {
			pending = true
			continue
		}
		seq = append(seq, rec)
		if pending && rec[0] == "git" && len(rec) > 1 {
			idx[rec[1]] = true
		}
		pending = false
	}
	return seq, idx
}

func TestShipGitHunkPlumbingSequence(t *testing.T) {
	log := setupGitHunkShip(t, "f.txt", "g.txt")
	ref := hunkRefFor(t, "f.txt", hunkBase, hunkCurrent, 0)

	if _, err := runShipCmd(t, "-m", "fix: frobnicate", "--no-push", "--skip-hunk", ref, "f.txt", "g.txt"); err != nil {
		t.Fatalf("ship error = %v", err)
	}
	seq, idx := gitIdxCarriers(t, log)
	want := [][]string{
		{"git", "rev-parse", "--show-toplevel"},                  // resolve repo root
		{"git", "ls-tree", "--full-tree", "HEAD", "--", "f.txt"}, // pre-flight base existence
		{"git", "show", "--end-of-options", "HEAD:f.txt"},        // pre-flight base read
		{"git", "read-tree", "HEAD"},
		{"git", "add", "-A", "--", "g.txt"},                      // the whole-shipped sibling
		{"git", "ls-tree", "--full-tree", "HEAD", "--", "f.txt"}, // stage-time base existence
		{"git", "show", "--end-of-options", "HEAD:f.txt"},        // stage-time base re-read
		{"git", "ls-tree", "--full-tree", "HEAD", "--", "f.txt"}, // stage-time mode
		{"git", "hash-object", "-w", "--stdin"},
		{"git", "update-index", "--add", "--cacheinfo", "100644,2222222222222222222222222222222222222222,f.txt"},
		{"git", "commit", "-m", "fix: frobnicate"},
		{"git", "restore", "--staged", "--", "f.txt", "g.txt"},
		{"git", "log", "-1", "--format=%h%x00%s"},
	}
	assertInvocations(t, seq, want)

	// The index-mutating calls carry the temp index; the read-only and object-db
	// calls must not (they operate against the real index / object store).
	for _, sub := range []string{"read-tree", "add", "update-index", "commit"} {
		if !idx[sub] {
			t.Errorf("git %s must carry the temp index", sub)
		}
	}
	for _, sub := range []string{"show", "ls-tree", "hash-object", "restore", "rev-parse"} {
		if idx[sub] {
			t.Errorf("git %s must not carry the temp index", sub)
		}
	}
	// The temp-index commit must never carry a pathspec (a "--"): that would commit
	// worktree state and smuggle the excluded hunk back in.
	for _, rec := range seq {
		if rec[0] == "git" && rec[1] == "commit" {
			for _, a := range rec[2:] {
				if a == "--" {
					t.Errorf("temp-index commit carried a pathspec: %v", rec)
				}
			}
		}
	}
}

func TestShipGitHunkNoVerify(t *testing.T) {
	tests := []struct {
		name         string
		noVerify     bool
		wantNoVerify bool
	}{
		{"default preserves native hooks", false, false},
		{"no verify reaches temp-index commit", true, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			log := setupGitHunkShip(t, "f.txt", "")
			ref := hunkRefFor(t, "f.txt", hunkBase, hunkCurrent, 0)
			args := []string{"-m", "fix: frobnicate", "--no-push", "--only-hunk", ref, "f.txt"}
			if tt.noVerify {
				args = append(args, "--no-verify")
			}
			if _, err := runShipCmd(t, args...); err != nil {
				t.Fatalf("ship error = %v", err)
			}
			seq, _ := gitIdxCarriers(t, log)
			var commit []string
			for _, inv := range seq {
				if len(inv) > 1 && inv[0] == "git" && inv[1] == "commit" {
					commit = inv
				}
			}
			gotNoVerify := false
			for _, arg := range commit {
				if arg == "--no-verify" {
					gotNoVerify = true
				}
			}
			if gotNoVerify != tt.wantNoVerify {
				t.Errorf("commit argv = %v, --no-verify present = %v, want %v", commit, gotNoVerify, tt.wantNoVerify)
			}
		})
	}
}

func TestShipGitHunkAmend(t *testing.T) {
	tests := []struct {
		name       string
		args       []string
		wantCommit []string
	}{
		{"amend with message", []string{"--amend", "-m", "fix: frobnicate"}, []string{"git", "commit", "--amend", "-m", "fix: frobnicate"}},
		{"amend no message", []string{"--amend"}, []string{"git", "commit", "--amend", "--no-edit"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			log := setupGitHunkShip(t, "f.txt", "")
			ref := hunkRefFor(t, "f.txt", hunkBase, hunkCurrent, 0)
			args := append(append([]string{}, tt.args...), "--no-push", "--skip-hunk", ref, "f.txt")
			if _, err := runShipCmd(t, args...); err != nil {
				t.Fatalf("ship error = %v", err)
			}
			seq, idx := gitIdxCarriers(t, log)
			var commit []string
			for _, rec := range seq {
				if rec[0] == "git" && rec[1] == "commit" {
					commit = rec
				}
			}
			if !reflect.DeepEqual(commit, tt.wantCommit) {
				t.Errorf("commit argv = %v, want %v", commit, tt.wantCommit)
			}
			if !idx["commit"] {
				t.Errorf("amend commit must carry the temp index")
			}
			// With only a hunk-scoped path, no whole file is staged, so no add runs.
			for _, rec := range seq {
				if rec[0] == "git" && rec[1] == "add" {
					t.Errorf("a sole hunk-scoped ship must run no git add, got %v", rec)
				}
			}
		})
	}
}

func TestShipGitHunkRefusals(t *testing.T) {
	ref0 := hunkRefFor(t, "f.txt", hunkBase, hunkCurrent, 0)
	ref1 := hunkRefFor(t, "f.txt", hunkBase, hunkCurrent, 1)
	driftHash := hunk.Compute([]byte("x\n"), []byte("Y\n"))[0].Digest
	driftRef := "f.txt:1#" + driftHash.String()
	rootOnly := [][]string{{"git", "rev-parse", "--show-toplevel"}}
	resolveSeq := [][]string{
		{"git", "rev-parse", "--show-toplevel"},
		{"git", "ls-tree", "--full-tree", "HEAD", "--", "f.txt"},
		{"git", "show", "--end-of-options", "HEAD:f.txt"},
	}

	tests := []struct {
		name    string
		args    []string
		wantErr string
		wantInv [][]string // nil = no VCS command ran
	}{
		{
			name:    "mutually exclusive flags",
			args:    []string{"--skip-hunk", ref0, "--only-hunk", ref1, "f.txt"},
			wantErr: "ship: --skip-hunk and --only-hunk cannot be combined",
		},
		{
			name:    "malformed ref",
			args:    []string{"--skip-hunk", "not-a-ref", "f.txt"},
			wantErr: `ship: invalid hunk ref "not-a-ref" (expected file:A-B#hash, from ccx vcs hunks)`,
		},
		{
			name:    "ref outside shipped paths",
			args:    []string{"--skip-hunk", ref0, "other.txt"},
			wantErr: "is outside the shipped paths",
			wantInv: rootOnly,
		},
		{
			name:    "drift",
			args:    []string{"--skip-hunk", driftRef, "f.txt"},
			wantErr: "the diff changed since listing; re-run: ccx vcs hunks f.txt",
			wantInv: resolveSeq,
		},
		{
			name:    "all excluded",
			args:    []string{"--skip-hunk", ref0, "--skip-hunk", ref1, "f.txt"},
			wantErr: "ship: all changes excluded in f.txt; drop the file from the ship instead",
			wantInv: resolveSeq,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			log := setupGitHunkShip(t, "f.txt", "")
			args := append([]string{"-m", "fix: frobnicate", "--no-push"}, tt.args...)
			_, err := runShipCmd(t, args...)
			if err == nil {
				t.Fatal("expected refusal, got nil")
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error = %q, want it to contain %q", err, tt.wantErr)
			}
			if got := readInvocations(t, log); !reflect.DeepEqual(got, tt.wantInv) {
				t.Errorf("invocations = %v, want %v (no mutating command may run)", got, tt.wantInv)
			}
		})
	}
}

// writeTempPlan marshals plan into a tempfile and returns its path.
func writeTempPlan(t *testing.T, plan selectionPlan) string {
	t.Helper()
	data, err := json.Marshal(plan)
	if err != nil {
		t.Fatalf("marshal plan: %v", err)
	}
	path := filepath.Join(t.TempDir(), "plan.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write plan: %v", err)
	}
	return path
}

func TestApplySelectionRewritesRight(t *testing.T) {
	tests := []struct {
		name      string
		base      string // "" writes no left file (new file, empty base)
		current   string
		mode      string
		hunkIdx   int
		wantRight string
		rightPerm os.FileMode
		wantLeft  string // expected left content afterwards (unchanged); "" = no left file
	}{
		{
			name:      "skip keeps the complement",
			base:      hunkBase,
			current:   hunkCurrent,
			mode:      "skip",
			hunkIdx:   0,
			wantRight: "a\nb\nc\nd\nE\n",
			rightPerm: 0o755,
			wantLeft:  hunkBase,
		},
		{
			name:      "only keeps the named hunk",
			base:      hunkBase,
			current:   hunkCurrent,
			mode:      "only",
			hunkIdx:   0,
			wantRight: "A\nb\nc\nd\ne\n",
			rightPerm: 0o644,
			wantLeft:  hunkBase,
		},
		{
			name:      "missing left is a new file",
			base:      "",
			current:   "new\ncontent\n",
			mode:      "only",
			hunkIdx:   0,
			wantRight: "new\ncontent\n",
			rightPerm: 0o644,
			wantLeft:  "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			leftDir := t.TempDir()
			rightDir := t.TempDir()
			leftPath := filepath.Join(leftDir, "f.txt")
			if tt.base != "" {
				if err := os.WriteFile(leftPath, []byte(tt.base), 0o644); err != nil { //nolint:gosec // test fixture
					t.Fatalf("write left: %v", err)
				}
			}
			rightPath := filepath.Join(rightDir, "f.txt")
			if err := os.WriteFile(rightPath, []byte(tt.current), tt.rightPerm); err != nil { //nolint:gosec // test asserts this perm survives
				t.Fatalf("write right: %v", err)
			}

			hunks := hunk.Compute([]byte(tt.base), []byte(tt.current))
			plan := selectionPlan{
				Files: map[string]planFile{
					"f.txt": {Mode: tt.mode, Hunks: []planHunk{{Range: hunkRange(hunks[tt.hunkIdx]), Digest: hunks[tt.hunkIdx].Digest.String()}}},
				},
				Result: filepath.Join(t.TempDir(), "sidecar"),
			}
			if err := runApplySelection(writeTempPlan(t, plan), leftDir, rightDir); err != nil {
				t.Fatalf("apply-selection error = %v", err)
			}

			got, err := os.ReadFile(rightPath) //nolint:gosec // test path
			if err != nil {
				t.Fatalf("read right: %v", err)
			}
			if string(got) != tt.wantRight {
				t.Errorf("right = %q, want %q", got, tt.wantRight)
			}
			info, err := os.Stat(rightPath)
			if err != nil {
				t.Fatalf("stat right: %v", err)
			}
			if info.Mode().Perm() != tt.rightPerm {
				t.Errorf("right perm = %v, want %v (mode must be preserved)", info.Mode().Perm(), tt.rightPerm)
			}
			left, err := os.ReadFile(leftPath) //nolint:gosec // test path
			switch {
			case tt.wantLeft == "":
				if !os.IsNotExist(err) {
					t.Errorf("left must stay absent, got err=%v content=%q", err, left)
				}
			case err != nil:
				t.Fatalf("read left: %v", err)
			case string(left) != tt.wantLeft:
				t.Errorf("left changed to %q, want %q (left is read-only)", left, tt.wantLeft)
			}
		})
	}
}

func TestApplySelectionFailureWritesSidecar(t *testing.T) {
	driftHash := hunk.Compute([]byte("x\n"), []byte("Y\n"))[0].Digest

	tests := []struct {
		name       string
		base       string
		current    string
		mode       string
		planHunks  []planHunk
		wantReason string
	}{
		{
			name:       "drift",
			base:       hunkBase,
			current:    hunkCurrent,
			mode:       "skip",
			planHunks:  []planHunk{{Range: "1", Digest: driftHash.String()}},
			wantReason: "the diff changed since listing; re-run: ccx vcs hunks f.txt",
		},
		{
			name:       "empty keep",
			base:       "a\n",
			current:    "A\n",
			mode:       "skip",
			planHunks:  nil, // filled from the single computed hunk below
			wantReason: "all changes excluded in f.txt; drop the file from the ship instead",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			leftDir := t.TempDir()
			rightDir := t.TempDir()
			if err := os.WriteFile(filepath.Join(leftDir, "f.txt"), []byte(tt.base), 0o644); err != nil { //nolint:gosec // test fixture
				t.Fatalf("write left: %v", err)
			}
			if err := os.WriteFile(filepath.Join(rightDir, "f.txt"), []byte(tt.current), 0o644); err != nil { //nolint:gosec // test fixture
				t.Fatalf("write right: %v", err)
			}
			hunks := tt.planHunks
			if hunks == nil {
				h := hunk.Compute([]byte(tt.base), []byte(tt.current))[0]
				hunks = []planHunk{{Range: hunkRange(h), Digest: h.Digest.String()}}
			}
			sidecar := filepath.Join(t.TempDir(), "sidecar")
			plan := selectionPlan{
				Files:  map[string]planFile{"f.txt": {Mode: tt.mode, Hunks: hunks}},
				Result: sidecar,
			}
			err := runApplySelection(writeTempPlan(t, plan), leftDir, rightDir)
			if err == nil {
				t.Fatal("expected apply-selection to fail, got nil")
			}
			if !strings.Contains(err.Error(), tt.wantReason) {
				t.Errorf("error = %q, want it to contain %q", err, tt.wantReason)
			}
			reason, rerr := os.ReadFile(sidecar) //nolint:gosec // test path
			if rerr != nil {
				t.Fatalf("read sidecar: %v", rerr)
			}
			if !strings.Contains(string(reason), tt.wantReason) {
				t.Errorf("sidecar = %q, want it to contain %q", reason, tt.wantReason)
			}
		})
	}
}

// TestHunkRefResolvesDuplicateDeletions checks identical deletions list as
// distinct refs and each freshly-listed ref resolves to its own hunk.
func TestHunkRefResolvesDuplicateDeletions(t *testing.T) {
	tests := []struct {
		name    string
		base    string
		current string
	}{
		{"identical adjacent deletions", "gone\na\ngone\n", "a\n"},
		{"interleaved identical deletions", "a\ngone\nb\nc\ngone\nd\n", "a\nb\nc\nd\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hunks := hunk.Compute([]byte(tt.base), []byte(tt.current))
			if len(hunks) != 2 || hunks[0].Digest != hunks[1].Digest {
				t.Fatalf("fixture must yield 2 same-digest hunks, got %d: %+v", len(hunks), hunks)
			}
			seen := map[string]bool{}
			for i := range hunks {
				refStr := hunkListRef("f.txt", hunks[i])
				if seen[refStr] {
					t.Fatalf("hunk %d re-lists an already-listed ref %q — identical deletions must get distinct lines", i, refStr)
				}
				seen[refStr] = true
				_, ref, err := hunk.ParseRef(refStr)
				if err != nil {
					t.Fatalf("ParseRef(%q): %v", refStr, err)
				}
				idx, err := matchHunkRef("f.txt", hunks, ref)
				if err != nil {
					t.Fatalf("matchHunkRef(%q): %v", refStr, err)
				}
				if idx != i {
					t.Errorf("freshly-listed ref %q for hunk %d resolved to hunk %d", refStr, i, idx)
				}
			}
		})
	}
}

// TestMatchHunkRefStaleDuplicateRefused checks a duplicate-digest ref whose line
// matches no hunk exactly is refused as drift, not silently nearest-matched.
func TestMatchHunkRefStaleDuplicateRefused(t *testing.T) {
	base := "a\nx\nb\nc\nd\nx\ne\n"
	current := "a\nb\nc\nd\ne\n"
	hunks := hunk.Compute([]byte(base), []byte(current))
	if len(hunks) != 2 || hunks[0].Digest != hunks[1].Digest {
		t.Fatalf("fixture must yield 2 same-digest deletions, got %d: %+v", len(hunks), hunks)
	}
	// hunks sit at post-image lines 2 and 5; line 3 is nearest (non-tie) to hunk 0
	// yet matches neither exactly — a stale ref that must be refused, not mis-picked.
	stale := anchor.Ref{Line: 3, Hash: hunks[0].Digest}
	if _, err := matchHunkRef("f.txt", hunks, stale); err == nil {
		t.Fatal("a stale duplicate-digest ref must be refused, not silently nearest-matched")
	} else if !strings.Contains(err.Error(), "re-run: ccx vcs hunks f.txt") {
		t.Errorf("error = %q, want the drift/re-list wording", err)
	}
}

// TestShowFileBaseDistinguishesAbsentFromFailure checks a file absent from the
// base is an empty base while an unresolvable base tree propagates the failure.
func TestShowFileBaseDistinguishesAbsentFromFailure(t *testing.T) {
	ctx := context.Background()
	t.Run("new file in a committed repo is an empty base", func(t *testing.T) {
		dir := initCliGitRepo(t)
		commitFile(t, dir, "tracked.txt", "x\n")
		if err := os.WriteFile("untracked.txt", []byte("new\n"), 0o644); err != nil { //nolint:gosec // test fixture
			t.Fatalf("write untracked: %v", err)
		}
		base, err := showFileBase(ctx, vcs.Git, "untracked.txt")
		if err != nil {
			t.Fatalf("a new file must yield an empty base, got err %v", err)
		}
		if len(base) != 0 {
			t.Errorf("new-file base = %q, want empty", base)
		}
	})
	t.Run("unresolvable base tree propagates", func(t *testing.T) {
		initCliGitRepo(t)                                                   // git init with no commit: HEAD is unborn
		if err := os.WriteFile("f.txt", []byte("x\n"), 0o644); err != nil { //nolint:gosec // test fixture
			t.Fatalf("write f.txt: %v", err)
		}
		if _, err := showFileBase(ctx, vcs.Git, "f.txt"); err == nil {
			t.Fatal("an unresolvable base tree must propagate, not swallow into an empty base")
		}
	})
}

// TestApplySelectionRefRoundTrip guards that the ref ccx vcs hunks emits
// (hunkRef) re-parses to the same anchor through the plan file, so listing and
// applying agree; the fixture carries a pure deletion so its post-image anchor is
// exercised too.
func TestApplySelectionRefRoundTrip(t *testing.T) {
	// a->A change (hunk 0) plus a pure deletion of "c" (hunk 1).
	hunks := hunk.Compute([]byte("a\nb\nc\nd\ne\n"), []byte("A\nb\nd\ne\n"))
	if len(hunks) != 2 || len(hunks[1].New) != 0 {
		t.Fatalf("fixture must yield a change then a pure deletion, got %+v", hunks)
	}
	for i := range hunks {
		ref := hunkRef(hunks[i])
		refs, err := planRefs([]planHunk{{Range: refRange(ref), Digest: ref.Hash.String()}})
		if err != nil {
			t.Fatalf("planRefs: %v", err)
		}
		idx, err := matchHunkRef("f.txt", hunks, refs[0])
		if err != nil {
			t.Fatalf("matchHunkRef: %v", err)
		}
		if idx != i {
			t.Errorf("ref for hunk %d matched hunk %d", i, idx)
		}
	}
}
