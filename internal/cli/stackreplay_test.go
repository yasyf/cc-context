package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/yasyf/cc-context/internal/render"
	"github.com/yasyf/cc-context/internal/vcstest"
)

var (
	stackReplayPinArg   = regexp.MustCompile(`^--ref=refs/ccx/publication-runs/[0-9a-f]{64}$`)
	stackReplayOntoArg  = regexp.MustCompile(`^--onto=[0-9a-f]{40}$`)
	stackReplayRangeArg = regexp.MustCompile(`^[0-9a-f]{40}\.\.[0-9a-f]{40}$`)
)

var stackReplayHooks = []string{"post-checkout", "post-index-change", "pre-rebase", "post-rewrite", "pre-commit", "commit-msg", "post-commit", "post-merge", "pre-auto-gc"}

var stackReplayForbidden = []string{"worktree", "rebase", "checkout", "switch", "read-tree", "checkout-index", "sparse-checkout", "reset", "stash", "cherry-pick"}

func stackGitSubcommand(argv []string) string {
	for i := 1; i < len(argv); i++ {
		switch a := argv[i]; {
		case a == "-c" || a == "-C":
			i++
		case strings.HasPrefix(a, "-"):
		default:
			return a
		}
	}
	return ""
}

func stackReplayCommit(t *testing.T, f *vcstest.Fixture, file, content, subject string) string {
	t.Helper()
	writeShipFile(t, f.Dir, file, content)
	mustRun(t, f.Env(), f.Dir, "git", "add", file)
	mustRun(t, f.Env(), f.Dir, "git", "commit", "-qm", subject)
	return gitAt(t, f.Env(), f.Dir, "rev-parse", "HEAD")
}

func stackReplayRefs(t *testing.T, f *vcstest.Fixture, patterns ...string) string {
	t.Helper()
	return gitAt(t, f.Env(), f.Dir, append([]string{"for-each-ref", "--format=%(refname) %(objectname)"}, patterns...)...)
}

func stackReplayClaim(t *testing.T, f *vcstest.Fixture, root string) *stackRebaseRun {
	t.Helper()
	run := &stackRebaseRun{Trunk: "main", Origin: f.Dir, Roots: []string{root}}
	if err := stackClaim(filepath.Join(f.Dir, ".git"), run); err != nil {
		t.Fatal(err)
	}
	return run
}

func stackReplayGitWrapper(t *testing.T, intercept string) string {
	t.Helper()
	bin := t.TempDir()
	writeExecutable(t, filepath.Join(bin, "git"), "#!/bin/sh\n"+intercept+"PATH=${PATH#"+bin+":} exec git \"$@\"\n")
	return bin
}

func stackReplaySettleIndex(t *testing.T, f *vcstest.Fixture) {
	t.Helper()
	old := time.Now().Add(-time.Hour)
	for file := range strings.Lines(gitAt(t, f.Env(), f.Dir, "ls-files")) {
		if err := os.Chtimes(filepath.Join(f.Dir, strings.TrimSpace(file)), old, old); err != nil {
			t.Fatal(err)
		}
	}
	mustRun(t, f.Env(), f.Dir, "git", "update-index", "-q", "--refresh")
}

func stackReplayInstallHooks(t *testing.T, f *vcstest.Fixture) string {
	t.Helper()
	hooks := t.TempDir()
	marker := filepath.Join(t.TempDir(), "fired")
	for _, name := range stackReplayHooks {
		writeExecutable(t, filepath.Join(hooks, name), "#!/bin/sh\necho "+name+" >> "+shQuote(marker)+"\n")
	}
	mustRun(t, f.Env(), f.Dir, "git", "config", "core.hooksPath", hooks)
	return marker
}

func stackConflictDirs(t *testing.T, f *vcstest.Fixture) []string {
	t.Helper()
	home, err := render.Home(f.Context())
	if err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(filepath.Join(home, ".claude", "worktrees", filepath.Base(f.Dir)))
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	var dirs []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "conflict-") {
			dirs = append(dirs, e.Name())
		}
	}
	return dirs
}

// TestStackRebaseReplaysCleanSpansInPlace pins that a stack whose spans all
// replay cleanly is rewritten without a working copy: no hook fires, no
// worktree is cut, the invoking checkout's index is untouched, and git replay
// runs over saved SHAs alone, printing into the run's publication pin.
func TestStackRebaseReplaysCleanSpansInPlace(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		branches []string
		build    func(t *testing.T, f *vcstest.Fixture)
		files    map[string]map[string]string
		commits  map[string]int
	}{
		{
			name:     "linear",
			branches: []string{"base", "top"},
			build:    func(*testing.T, *vcstest.Fixture) {},
			files: map[string]map[string]string{
				"base": {"base.txt": "base\n", "upstream.txt": "upstream\n"},
				"top":  {"base.txt": "base\n", "top.txt": "top\n", "upstream.txt": "upstream\n"},
			},
			commits: map[string]int{"base": 1, "top": 2},
		},
		{
			name:     "merge",
			branches: []string{"feature"},
			build: func(t *testing.T, f *vcstest.Fixture) {
				t.Helper()
				mustRun(t, f.Env(), f.Dir, "git", "switch", "-qc", "side", "main")
				stackReplayCommit(t, f, "side.txt", "side\n", "side")
				mustRun(t, f.Env(), f.Dir, "git", "switch", "-q", "feature")
				mustRun(t, f.Env(), f.Dir, "git", "merge", "-q", "--no-ff", "-m", "merge side", "side")
			},
			files: map[string]map[string]string{
				"feature": {"feature.txt": "feature\n", "side.txt": "side\n", "upstream.txt": "upstream\n"},
			},
			commits: map[string]int{"feature": 2},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := stackRebaseRepo(t, tt.branches...)
			tt.build(t, f)
			stackAdvanceTrunk(t, f, "upstream.txt", "upstream\n")
			mustRun(t, f.Env(), f.Dir, "git", "switch", "-q", "main")
			oldMain := gitAt(t, f.Env(), f.Dir, "rev-parse", "main")
			newMain := gitAt(t, f.Env(), f.Dir, "rev-parse", "origin/main")
			heads := map[string]string{}
			for _, b := range tt.branches {
				heads[b] = gitAt(t, f.Env(), f.Dir, "rev-parse", b)
			}
			stackReplaySettleIndex(t, f)
			marker := stackReplayInstallHooks(t, f)
			worktrees := gitAt(t, f.Env(), f.Dir, "worktree", "list", "--porcelain")
			indexPath := filepath.Join(f.Dir, ".git", "index")
			index, err := os.ReadFile(indexPath)
			if err != nil {
				t.Fatal(err)
			}
			indexInfo, err := os.Stat(indexPath)
			if err != nil {
				t.Fatal(err)
			}
			shipResetLog(t, f)

			out, _, err := runStackCmd(t, f, "rebase", "--no-push", "--parent", tt.branches[0]+"=main")
			if err != nil {
				t.Fatalf("stack rebase: %v\n%s", err, out)
			}

			var replays [][]string
			for _, argv := range shipGTInvocations(t, f) {
				if argv[0] != "git" {
					continue
				}
				sub := stackGitSubcommand(argv)
				if slices.Contains(stackReplayForbidden, sub) {
					t.Errorf("ccx ran git %s: %q", sub, argv)
				}
				if sub == "replay" {
					replays = append(replays, argv)
				}
			}
			if len(replays) != len(tt.branches) {
				t.Fatalf("ran %d git replay(s), want %d, one per moving branch: %q", len(replays), len(tt.branches), replays)
			}
			pins := map[string]bool{}
			for i, argv := range replays {
				if len(argv) != 8 || argv[2] != "replay" || argv[3] != "--ref-action=print" || argv[4] != "--linearize" ||
					!stackReplayPinArg.MatchString(argv[5]) || !stackReplayOntoArg.MatchString(argv[6]) || !stackReplayRangeArg.MatchString(argv[7]) {
					t.Errorf("replay argv = %q, want git --attr-source=<sha> replay --ref-action=print --linearize --ref=refs/ccx/publication-runs/<sha256> --onto=<sha> <sha>..<sha>", argv)
					continue
				}
				pins[argv[5]] = true
				b := tt.branches[i]
				oldBase, onto := oldMain, newMain
				if i > 0 {
					oldBase = heads[tt.branches[i-1]]
					onto = gitAt(t, f.Env(), f.Dir, "rev-parse", tt.branches[i-1])
				}
				if want := oldBase + ".." + heads[b]; argv[7] != want {
					t.Errorf("replay of %s ranged %s, want %s", b, argv[7], want)
				}
				if want := "--onto=" + onto; argv[6] != want {
					t.Errorf("replay of %s ran %s, want %s", b, argv[6], want)
				}
				if want := "--attr-source=" + onto; argv[1] != want {
					t.Errorf("replay of %s read attributes with %s, want %s", b, argv[1], want)
				}
			}
			if len(pins) != len(tt.branches) {
				t.Errorf("replays printed into %d distinct pins, want %d: %q", len(pins), len(tt.branches), replays)
			}
			if fired, err := os.ReadFile(marker); !os.IsNotExist(err) {
				t.Errorf("hooks fired: %q (%v)", fired, err)
			}
			if got := gitAt(t, f.Env(), f.Dir, "worktree", "list", "--porcelain"); got != worktrees {
				t.Errorf("worktree list changed:\n%s\nwant:\n%s", got, worktrees)
			}
			after, err := os.ReadFile(indexPath)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(after, index) {
				t.Error("the invoking checkout's index changed")
			}
			afterInfo, err := os.Stat(indexPath)
			if err != nil {
				t.Fatal(err)
			}
			if !afterInfo.ModTime().Equal(indexInfo.ModTime()) {
				t.Errorf("index mtime moved from %v to %v", indexInfo.ModTime(), afterInfo.ModTime())
			}
			for _, b := range tt.branches {
				if !stackOnto(t, f, newMain, b) {
					t.Errorf("%s does not sit on the new trunk %s", b, newMain)
				}
				if got := gitAt(t, f.Env(), f.Dir, "rev-list", "--count", newMain+".."+b); got != strconv.Itoa(tt.commits[b]) {
					t.Errorf("%s carries %s commit(s) past trunk, want %d", b, got, tt.commits[b])
				}
				if got := gitAt(t, f.Env(), f.Dir, "rev-list", "--merges", "--count", newMain+".."+b); got != "0" {
					t.Errorf("%s carries %s merge commit(s), want none", b, got)
				}
				for file, want := range tt.files[b] {
					if got := mustRun(t, f.Env(), f.Dir, "git", "show", b+":"+file); got != want {
						t.Errorf("%s:%s = %q, want %q", b, file, got, want)
					}
				}
			}
			mustRun(t, f.Env(), f.Dir, "git", "commit", "-q", "--allow-empty", "-m", "control")
			if fired, _ := os.ReadFile(marker); !strings.Contains(string(fired), "post-commit") {
				t.Errorf("a commit fired %q, want post-commit — the marker hooks are not live", fired)
			}
		})
	}
}

func TestStackReplayMatchesRebasePolicy(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		build func(t *testing.T, f *vcstest.Fixture) (oldBase, head, newBase string)
		want  []string
	}{
		{
			name: "intentionally empty commit kept",
			build: func(t *testing.T, f *vcstest.Fixture) (string, string, string) {
				t.Helper()
				base := gitAt(t, f.Env(), f.Dir, "rev-parse", "main")
				mustRun(t, f.Env(), f.Dir, "git", "switch", "-qc", "branch")
				stackReplayCommit(t, f, "a.txt", "a\n", "a")
				mustRun(t, f.Env(), f.Dir, "git", "commit", "-q", "--allow-empty", "-m", "empty")
				head := gitAt(t, f.Env(), f.Dir, "rev-parse", "HEAD")
				mustRun(t, f.Env(), f.Dir, "git", "switch", "-q", "main")
				return base, head, stackReplayCommit(t, f, "t.txt", "trunk\n", "trunk")
			},
			want: []string{"empty", "a"},
		},
		{
			name: "commit trunk also made dropped",
			build: func(t *testing.T, f *vcstest.Fixture) (string, string, string) {
				t.Helper()
				base := gitAt(t, f.Env(), f.Dir, "rev-parse", "main")
				mustRun(t, f.Env(), f.Dir, "git", "switch", "-qc", "branch")
				stackReplayCommit(t, f, "same.txt", "same\n", "same")
				head := stackReplayCommit(t, f, "own.txt", "own\n", "own")
				mustRun(t, f.Env(), f.Dir, "git", "switch", "-q", "main")
				return base, head, stackReplayCommit(t, f, "same.txt", "same\n", "trunk same")
			},
			want: []string{"own"},
		},
		{
			name: "commit cherry-picked upstream dropped",
			build: func(t *testing.T, f *vcstest.Fixture) (string, string, string) {
				t.Helper()
				base := gitAt(t, f.Env(), f.Dir, "rev-parse", "main")
				mustRun(t, f.Env(), f.Dir, "git", "switch", "-qc", "branch")
				pick := stackReplayCommit(t, f, "pick.txt", "pick\n", "pick")
				head := stackReplayCommit(t, f, "own.txt", "own\n", "own")
				mustRun(t, f.Env(), f.Dir, "git", "switch", "-q", "main")
				stackReplayCommit(t, f, "t.txt", "trunk\n", "trunk")
				mustRun(t, f.Env(), f.Dir, "git", "cherry-pick", pick)
				return base, head, gitAt(t, f.Env(), f.Dir, "rev-parse", "HEAD")
			},
			want: []string{"own"},
		},
		{
			name: "apply then revert both kept",
			build: func(t *testing.T, f *vcstest.Fixture) (string, string, string) {
				t.Helper()
				base := gitAt(t, f.Env(), f.Dir, "rev-parse", "main")
				mustRun(t, f.Env(), f.Dir, "git", "switch", "-qc", "branch")
				stackReplayCommit(t, f, "r.txt", "r\n", "apply")
				mustRun(t, f.Env(), f.Dir, "git", "revert", "--no-edit", "HEAD")
				head := gitAt(t, f.Env(), f.Dir, "rev-parse", "HEAD")
				mustRun(t, f.Env(), f.Dir, "git", "switch", "-q", "main")
				return base, head, stackReplayCommit(t, f, "t.txt", "trunk\n", "trunk")
			},
			want: []string{`Revert "apply"`, "apply"},
		},
		{
			name: "merge linearized",
			build: func(t *testing.T, f *vcstest.Fixture) (string, string, string) {
				t.Helper()
				base := gitAt(t, f.Env(), f.Dir, "rev-parse", "main")
				mustRun(t, f.Env(), f.Dir, "git", "switch", "-qc", "branch")
				stackReplayCommit(t, f, "feature.txt", "feature\n", "feature")
				mustRun(t, f.Env(), f.Dir, "git", "switch", "-qc", "side", "main")
				stackReplayCommit(t, f, "side.txt", "side\n", "side")
				mustRun(t, f.Env(), f.Dir, "git", "switch", "-q", "branch")
				mustRun(t, f.Env(), f.Dir, "git", "merge", "-q", "--no-ff", "-m", "merge side", "side")
				head := gitAt(t, f.Env(), f.Dir, "rev-parse", "HEAD")
				mustRun(t, f.Env(), f.Dir, "git", "switch", "-q", "main")
				return base, head, stackReplayCommit(t, f, "t.txt", "trunk\n", "trunk")
			},
			want: []string{"side", "feature"},
		},
		{
			name: "no own commits",
			build: func(t *testing.T, f *vcstest.Fixture) (string, string, string) {
				t.Helper()
				base := gitAt(t, f.Env(), f.Dir, "rev-parse", "main")
				return base, base, stackReplayCommit(t, f, "t.txt", "trunk\n", "trunk")
			},
			want: nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := shipRepo(t)
			oldBase, head, newBase := tt.build(t, f)
			run := stackReplayClaim(t, f, "branch")
			b := stackRebaseBranch{Name: "branch", Parent: "main", WasParent: "main", Local: head, Head: head, OldBase: oldBase, SourceBase: oldBase, NewBase: newBase}
			refs := stackReplayRefs(t, f)

			got, err := stackReplay(f.Context(), render.Dir(f.Dir), run, &b)
			if err != nil {
				t.Fatalf("stackReplay: %v", err)
			}

			pin := stackPublicationPin(run, "branch")
			if pinned := gitAt(t, f.Env(), f.Dir, "rev-parse", pin); pinned != got {
				t.Errorf("pin = %s, want the replayed head %s", pinned, got)
			}
			mustRun(t, f.Env(), f.Dir, "git", "update-ref", "-d", pin, got)
			if after := stackReplayRefs(t, f); after != refs {
				t.Errorf("stackReplay moved refs besides its pin:\n%s\nwant:\n%s", after, refs)
			}
			if head == oldBase && got != newBase {
				t.Errorf("stackReplay of an empty span = %s, want the new base %s", got, newBase)
			}
			ref := filepath.Join(t.TempDir(), "reference")
			mustRun(t, f.Env(), f.Dir, "git", "worktree", "add", "-q", "--detach", ref, head)
			mustRun(t, f.Env(), ref, "git", slices.Concat(stackGitRebaseArgs, []string{"rebase"}, stackReplayRebaseArgs, []string{"--onto", newBase, oldBase})...)
			want := gitAt(t, f.Env(), ref, "rev-parse", "HEAD")
			if g, w := gitAt(t, f.Env(), f.Dir, "rev-parse", got+"^{tree}"), gitAt(t, f.Env(), f.Dir, "rev-parse", want+"^{tree}"); g != w {
				t.Errorf("replayed tree %s, rebased tree %s", g, w)
			}
			replayed := stackReplaySubjects(t, f, newBase, got)
			rebased := stackReplaySubjects(t, f, newBase, want)
			if !slices.Equal(replayed, rebased) {
				t.Errorf("replayed subjects %q, rebased subjects %q", replayed, rebased)
			}
			if !slices.Equal(replayed, tt.want) {
				t.Errorf("replayed subjects %q, want %q", replayed, tt.want)
			}
		})
	}
}

func stackReplaySubjects(t *testing.T, f *vcstest.Fixture, base, tip string) []string {
	t.Helper()
	out := gitAt(t, f.Env(), f.Dir, "log", "--format=%s", base+".."+tip)
	if out == "" {
		return nil
	}
	return strings.Split(out, "\n")
}

func TestStackReplayReadsTheOutputPin(t *testing.T) {
	t.Parallel()
	f := shipRepo(t)
	base := gitAt(t, f.Env(), f.Dir, "rev-parse", "main")
	mustRun(t, f.Env(), f.Dir, "git", "switch", "-qc", "branch")
	head := stackReplayCommit(t, f, "b.txt", "b\n", "branch")
	mustRun(t, f.Env(), f.Dir, "git", "switch", "-q", "main")
	newBase := stackReplayCommit(t, f, "t.txt", "trunk\n", "trunk")
	run := stackReplayClaim(t, f, "branch")
	pin := stackPublicationPin(run, "branch")
	mustRun(t, f.Env(), f.Dir, "git", "update-ref", pin, base)
	b := stackRebaseBranch{Name: "branch", Parent: "main", WasParent: "main", Local: head, Head: head, OldBase: base, SourceBase: base, NewBase: newBase}
	refs := stackReplayRefs(t, f)

	got, err := stackReplay(f.Context(), render.Dir(f.Dir), run, &b)
	if err != nil {
		t.Fatalf("stackReplay over a set pin: %v", err)
	}
	if !stackOnto(t, f, newBase, got) || got == newBase {
		t.Errorf("stackReplay = %s, want a commit on %s", got, newBase)
	}
	if pinned := gitAt(t, f.Env(), f.Dir, "rev-parse", pin); pinned != got {
		t.Errorf("pin = %s, want it moved from %s to the replayed head %s", pinned, base, got)
	}
	mustRun(t, f.Env(), f.Dir, "git", "update-ref", pin, base, got)
	if after := stackReplayRefs(t, f); after != refs {
		t.Errorf("stackReplay moved refs besides its pin:\n%s\nwant:\n%s", after, refs)
	}

	tests := []struct {
		name string
		awk  string
	}{
		{"another ref", `{ $2 = "refs/heads/elsewhere"; print }`},
		{"a wrong old value", `{ $4 = "` + newBase + `"; print }`},
		{"two updates", `{ print; print }`},
		{"no update", `{ }`},
		{"an update split across lines", `{ print $1, $2; print $3, $4 }`},
		{"a blank line before the update", `{ print ""; print }`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bin := t.TempDir()
			writeExecutable(t, filepath.Join(bin, "git"), "#!/bin/sh\n"+
				"if [ \"$2\" = replay ]; then\n"+
				"  out=$(PATH=${PATH#"+bin+":} git \"$@\") || exit $?\n"+
				"  printf '%s\\n' \"$out\" | awk "+shQuote(tt.awk)+"\n"+
				"  exit 0\n"+
				"fi\n"+
				"PATH=${PATH#"+bin+":} exec git \"$@\"\n")
			ctx := render.WithEnv(f.Context(), "PATH="+bin+string(os.PathListSeparator)+f.PATH())
			wrapped := b
			_, err := stackReplay(ctx, render.Dir(f.Dir), run, &wrapped)
			if err == nil || !strings.Contains(err.Error(), "want exactly one update") {
				t.Fatalf("stackReplay = %v, want a refusal naming exactly one update", err)
			}
			if after := stackReplayRefs(t, f); after != refs {
				t.Errorf("stackReplay moved refs:\n%s\nwant:\n%s", after, refs)
			}
		})
	}
}

func TestStackRebaseOpensNoWorkspaceOnAReplayError(t *testing.T) {
	t.Parallel()
	t.Run("replay error", func(t *testing.T) {
		t.Parallel()
		f := stackRebaseRepo(t, "feature")
		stackAdvanceTrunk(t, f, "upstream.txt", "upstream\n")
		heads := stackReplayRefs(t, f, "refs/heads")
		worktrees := gitAt(t, f.Env(), f.Dir, "worktree", "list", "--porcelain")
		f.PrependPATH(stackReplayGitWrapper(t, "if [ \"$2\" = replay ]; then echo 'fatal: boom' >&2; exit 128; fi\n"))

		_, _, err := runStackCmd(t, f, "rebase", "--no-push")
		if err == nil {
			t.Fatal("stack rebase succeeded, want the replay error")
		}
		if msg := err.Error(); !strings.Contains(msg, "exited 128") || !strings.Contains(msg, "boom") {
			t.Errorf("error = %q, want it to carry exited 128 and boom", msg)
		}
		if strings.Contains(err.Error(), "workspace: ") {
			t.Errorf("error names a workspace: %v", err)
		}
		if got := gitAt(t, f.Env(), f.Dir, "worktree", "list", "--porcelain"); got != worktrees {
			t.Errorf("worktree list changed:\n%s\nwant:\n%s", got, worktrees)
		}
		if dirs := stackConflictDirs(t, f); len(dirs) != 0 {
			t.Errorf("conflict workspaces on disk: %q", dirs)
		}
		if got := stackReplayRefs(t, f, "refs/heads"); got != heads {
			t.Errorf("branches moved:\n%s\nwant:\n%s", got, heads)
		}
	})
	t.Run("genuine conflict", func(t *testing.T) {
		t.Parallel()
		f := shipGTRepo(t, vcstest.GTStack("base"))
		stackConflicting(t, f)

		_, _, err := runStackCmd(t, f, "rebase", "--no-push")
		if err == nil {
			t.Fatal("stack rebase succeeded, want the conflict on feature")
		}
		ws := stackWorkspaceOf(t, err)
		if _, err := os.Stat(ws); err != nil {
			t.Errorf("conflict workspace %s: %v", ws, err)
		}
		if listed := gitAt(t, f.Env(), f.Dir, "worktree", "list", "--porcelain"); !strings.Contains(listed, ws) {
			t.Errorf("conflict workspace %s not registered:\n%s", ws, listed)
		}
		if dirs := stackConflictDirs(t, f); !slices.Equal(dirs, []string{"conflict-feature"}) {
			t.Errorf("conflict workspaces on disk = %q, want [conflict-feature]", dirs)
		}
	})
}

func TestStackRebaseRefusesAnOldGitBeforeAnyState(t *testing.T) {
	t.Parallel()
	f := stackRebaseRepo(t, "feature")
	stackAdvanceTrunk(t, f, "upstream.txt", "upstream\n")
	state := filepath.Join(f.Dir, ".git", stackRebaseStateDir)
	if _, err := os.Stat(state); !os.IsNotExist(err) {
		t.Fatalf("%s exists before the run: %v", state, err)
	}
	refs := stackReplayRefs(t, f, "refs/heads", "refs/ccx")
	worktrees := gitAt(t, f.Env(), f.Dir, "worktree", "list", "--porcelain")
	f.PrependPATH(stackReplayGitWrapper(t, "if [ \"$1\" = version ]; then echo 'git version 2.55.0'; exit 0; fi\n"))

	_, _, err := runStackCmd(t, f, "rebase", "--no-push")
	if err == nil {
		t.Fatal("stack rebase succeeded under git 2.55.0")
	}
	if msg := err.Error(); !strings.Contains(msg, "needs git 2.56 or newer") || !strings.Contains(msg, "2.55.0") {
		t.Errorf("error = %q, want it to name git 2.56 and the 2.55.0 found", msg)
	}
	if _, err := os.Stat(state); !os.IsNotExist(err) {
		t.Errorf("%s exists after the refusal: %v", state, err)
	}
	if got := stackReplayRefs(t, f, "refs/heads", "refs/ccx"); got != refs {
		t.Errorf("refs changed:\n%s\nwant:\n%s", got, refs)
	}
	if got := gitAt(t, f.Env(), f.Dir, "worktree", "list", "--porcelain"); got != worktrees {
		t.Errorf("worktree list changed:\n%s\nwant:\n%s", got, worktrees)
	}
}

func TestStackRequireGit(t *testing.T) {
	t.Parallel()
	tests := []struct {
		version string
		ok      bool
	}{
		{"git version 2.55.0", false},
		{"git version 2.9.5", false},
		{"git version 1.99.0", false},
		{"git version 2.56.0", true},
		{"git version 2.100.1", true},
		{"git version 3.0.0 (Apple Git-200)", true},
	}
	for _, tt := range tests {
		t.Run(tt.version, func(t *testing.T) {
			t.Parallel()
			bin := stackReplayGitWrapper(t, "if [ \"$1\" = version ]; then echo "+shQuote(tt.version)+"; exit 0; fi\n")
			ctx := render.WithEnv(context.Background(), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"))
			err := stackRequireGit(ctx, render.Dir(t.TempDir()), "test")
			if tt.ok {
				if err != nil {
					t.Errorf("stackRequireGit = %v, want it accepted", err)
				}
				return
			}
			found := strings.Fields(strings.TrimPrefix(tt.version, "git version "))[0]
			want := "test: needs git 2.56 or newer for git replay --ref and --linearize, and found git " + found + " — upgrade git, then re-run"
			if err == nil || err.Error() != want {
				t.Errorf("stackRequireGit = %v, want %q", err, want)
			}
		})
	}
}

func TestStackRebaseKeepsAnEarlierReplayAcrossALaterConflict(t *testing.T) {
	t.Parallel()
	f := shipGTRepo(t)
	stubStackPRs(t, f, nil)
	writeShipFile(t, f.Dir, "c.txt", "base\n")
	sparseCommit(t, f, "layout")
	mustRun(t, f.Env(), f.Dir, "git", "push", "-q", "origin", "main")
	for _, lane := range [][2]string{{"a", "a.txt"}, {"b", "c.txt"}} {
		mustRun(t, f.Env(), f.Dir, "git", "switch", "-qc", lane[0], "main")
		writeShipFile(t, f.Dir, lane[1], lane[0]+"\n")
		sparseCommit(t, f, lane[0])
		mustRun(t, f.Env(), f.Dir, "gt", "track", "-f", "--no-interactive")
	}
	mustRun(t, f.Env(), f.Dir, "git", "switch", "-q", "main")
	writeShipFile(t, f.Dir, "c.txt", "trunk\n")
	sparseCommit(t, f, "trunk")
	mustRun(t, f.Env(), f.Dir, "git", "push", "-q", "origin", "main")

	_, _, err := runStackCmd(t, f, "rebase", "--no-push", "--parent", "a=main", "--parent", "b=main")
	if err == nil || !strings.Contains(err.Error(), "b does not rebase onto main cleanly") {
		t.Fatalf("stack rebase = %v, want the conflict on b", err)
	}
	ws := stackWorkspaceOf(t, err)
	run, err := stackOnlyTestRun(filepath.Join(f.Dir, ".git"))
	if err != nil {
		t.Fatal(err)
	}
	replayed := run.branch("a").NewHead
	if replayed == "" {
		t.Fatal("a was not replayed before b stopped")
	}
	if pinned := gitAt(t, f.Env(), f.Dir, "rev-parse", stackPublicationPin(run, "a")); pinned != replayed {
		t.Errorf("a's pin = %s, want its replayed head %s", pinned, replayed)
	}
	mustRun(t, f.Env(), f.Dir, "git", "reflog", "expire", "--expire=now", "--all")
	mustRun(t, f.Env(), f.Dir, "git", "gc", "-q", "--prune=now")
	mustRun(t, f.Env(), f.Dir, "git", "cat-file", "-e", replayed+"^{commit}")

	writeShipFile(t, ws, "c.txt", "trunk\nb\n")
	mustRun(t, f.Env(), ws, "git", "add", "c.txt")
	if _, _, err := runStackCmd(t, f, "continue"); err != nil {
		t.Fatalf("continue: %v", err)
	}
	if got := gitAt(t, f.Env(), f.Dir, "rev-parse", "a"); got != replayed {
		t.Errorf("a = %s, want the head replayed before the conflict, %s", got, replayed)
	}
	if !stackOnto(t, f, "main", "a") || !stackOnto(t, f, "main", "b") {
		t.Error("a and b are not both on the new trunk")
	}
	if got := gitAt(t, f.Env(), f.Dir, "show", "b:c.txt"); got != "trunk\nb" {
		t.Errorf("b's c.txt = %q, want the resolution", got)
	}
	if pins := gitAt(t, f.Env(), f.Dir, "for-each-ref", "refs/ccx/publication-runs/"); pins != "" {
		t.Errorf("run pins left after the run finished: %s", pins)
	}
}
