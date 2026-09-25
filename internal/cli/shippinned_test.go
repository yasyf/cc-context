package cli

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/yasyf/cc-context/internal/vcs"
	"github.com/yasyf/cc-context/internal/vcstest"
)

func TestShipExpectedRemoteValidation(t *testing.T) {
	for _, test := range []struct {
		name     string
		opts     shipOpts
		kind     vcs.Kind
		graphite bool
		valid    bool
	}{
		{name: "sha1", opts: shipOpts{noCommit: true, expectRemote: strings.Repeat("a", 40)}, kind: vcs.Git, valid: true},
		{name: "sha256", opts: shipOpts{noCommit: true, expectRemote: strings.Repeat("b", 64)}, kind: vcs.Git, valid: true},
		{name: "abbreviated", opts: shipOpts{noCommit: true, expectRemote: "abcdef0"}, kind: vcs.Git},
		{name: "symbolic", opts: shipOpts{noCommit: true, expectRemote: "origin/feature"}, kind: vcs.Git},
		{name: "nonhex", opts: shipOpts{noCommit: true, expectRemote: strings.Repeat("z", 40)}, kind: vcs.Git},
		{name: "empty", opts: shipOpts{noCommit: true}, kind: vcs.Git},
		{name: "commit", opts: shipOpts{expectRemote: strings.Repeat("a", 40)}, kind: vcs.Git},
		{name: "no push", opts: shipOpts{noCommit: true, noPush: true, expectRemote: strings.Repeat("a", 40)}, kind: vcs.Git},
		{name: "jj", opts: shipOpts{noCommit: true, expectRemote: strings.Repeat("a", 40)}, kind: vcs.JJ},
		{name: "graphite", opts: shipOpts{noCommit: true, expectRemote: strings.Repeat("a", 40)}, kind: vcs.Git, graphite: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := validateShipExpectedRemote(test.opts, test.kind, test.graphite)
			if (err == nil) != test.valid {
				t.Fatalf("validation error = %v, valid = %v", err, test.valid)
			}
		})
	}
}

func TestShipPinnedRemoteAfterLocalRebase(t *testing.T) {
	for _, scenario := range []string{"unchanged", "foreign", "fetched foreign", "same clone", "during push", "non-origin", "dry run", "local branch moves"} {
		t.Run(scenario, func(t *testing.T) {
			f := shipRepo(t, vcstest.Remote())
			mustRun(t, f.Env(), f.Dir, "git", "checkout", "-qb", "feature")
			writeShipFile(t, f.Dir, "feature.txt", "feature\n")
			mustRun(t, f.Env(), f.Dir, "git", "add", "feature.txt")
			mustRun(t, f.Env(), f.Dir, "git", "commit", "-qm", "feature")
			mustRun(t, f.Env(), f.Dir, "git", "push", "-qu", "origin", "feature")
			published := shipHead(t, f)
			mustRun(t, f.Env(), f.Dir, "git", "checkout", "-q", "main")
			writeShipFile(t, f.Dir, "trunk.txt", "new base\n")
			mustRun(t, f.Env(), f.Dir, "git", "add", "trunk.txt")
			mustRun(t, f.Env(), f.Dir, "git", "commit", "-qm", "new base")
			mustRun(t, f.Env(), f.Dir, "git", "checkout", "-q", "feature")
			mustRun(t, f.Env(), f.Dir, "git", "rebase", "main")
			rebased := shipHead(t, f)
			if rebased == published {
				t.Fatal("fixture did not rewrite the published commit")
			}
			mustRun(t, f.Env(), f.Dir, "git", "commit", "--amend", "-qm", "amended feature")
			head := shipHead(t, f)
			remote := "origin"
			wantRemote := head
			wantLocal := head
			wantError := false
			wantErrorText := "pinned lease was not refreshed"
			switch scenario {
			case "foreign", "fetched foreign", "same clone", "during push":
				other := filepath.Join(t.TempDir(), "other")
				mustRun(t, f.Env(), f.Dir, "git", "clone", "-q", "-b", "feature", f.RemoteDir, other)
				mustRun(t, f.Env(), other, "git", "config", "user.email", "other@example.invalid")
				mustRun(t, f.Env(), other, "git", "config", "user.name", "Other")
				writeShipFile(t, other, "foreign.txt", "preserve\n")
				mustRun(t, f.Env(), other, "git", "add", "foreign.txt")
				mustRun(t, f.Env(), other, "git", "commit", "-qm", "foreign")
				foreign := gitAt(t, f.Env(), other, "rev-parse", "HEAD")
				switch scenario {
				case "same clone":
					mustRun(t, f.Env(), f.Dir, "git", "fetch", "-q", other, "HEAD")
					mustRun(t, f.Env(), f.Dir, "git", "push", "-q", "origin", foreign+":refs/heads/feature")
					row := gitAt(t, f.Env(), f.Dir, "reflog", "show", "-n", "1", "--format=%H %gs", "refs/remotes/origin/feature", "--")
					if row != foreign+" update by push" {
						t.Fatalf("same-clone publication evidence = %q", row)
					}
				case "during push":
					realBin := shipDisplaceShim(t, f, "git")
					writeShipExecutable(t, f.ShimBin, "git", "#!/bin/sh\n"+
						"if [ -z \"$CCX_SHIM_DEPTH\" ] && [ \"$1\" = push ]; then\n"+
						"  CCX_SHIM_DEPTH=1 git -C '"+other+"' push -q origin feature || exit $?\n"+
						"fi\nexec '"+realBin+"' \"$@\"\n")
				default:
					mustRun(t, f.Env(), other, "git", "push", "-q", "origin", "feature")
				}
				if scenario == "fetched foreign" {
					mustRun(t, f.Env(), f.Dir, "git", "fetch", "-q", "origin", "feature")
				}
				wantRemote, wantError = foreign, true
			case "non-origin":
				mustRun(t, f.Env(), f.Dir, "git", "remote", "rename", "origin", "backup")
				remote = "backup"
			case "local branch moves":
				wantLocal = gitAt(t, f.Env(), f.Dir, "commit-tree", head+"^{tree}", "-p", head, "-m", "unreviewed replacement")
				realBin := shipDisplaceShim(t, f, "git")
				writeShipExecutable(t, f.ShimBin, "git", "#!/bin/sh\n"+
					"if [ -z \"$CCX_SHIM_DEPTH\" ] && [ \"$1\" = push ]; then\n"+
					"  CCX_SHIM_DEPTH=1 git update-ref refs/heads/feature '"+wantLocal+"' '"+head+"' || exit $?\n"+
					"fi\nexec '"+realBin+"' \"$@\"\n")
				wantError = true
				wantErrorText = "left the local ref unchanged"
			case "dry run":
				wantRemote = published
			}
			shipResetLog(t, f)
			args := []string{"--no-commit", "--expect-remote", published, "--no-watch", "--no-pr"}
			if scenario == "dry run" {
				args = append(args, "--dry-run")
			}
			out, _, err := runShipCmdFull(f.Context(), t, args...)
			if (err != nil) != wantError {
				t.Fatalf("ship error = %v, want error = %v; output = %s", err, wantError, out)
			}
			if wantError && !strings.Contains(err.Error(), wantErrorText) {
				t.Fatalf("refusal lacks pinned recovery context: %v", err)
			}
			invocations := vcstest.Invocations(t, f.ArgvLog)
			pushes := 0
			for _, argv := range invocations {
				if len(argv) < 2 || argv[0] != "git" {
					continue
				}
				if slices.Contains([]string{"fetch", "rebase", "commit", "reflog"}, argv[1]) {
					t.Fatalf("pinned publish ran %v", argv)
				}
				if argv[1] == "push" {
					pushes++
					if !slices.Contains(argv, remote) || !slices.Contains(argv, "--force-with-lease=refs/heads/feature:"+published) || !slices.Contains(argv, head+":refs/heads/feature") {
						t.Fatalf("push does not carry exact target and lease: %v", argv)
					}
				}
			}
			wantPushes := 1
			if scenario == "dry run" {
				wantPushes = 0
				if !strings.Contains(out, "exact remote lease "+published) {
					t.Fatalf("dry run omitted lease: %s", out)
				}
			}
			if pushes != wantPushes {
				t.Fatalf("pushes = %d, want %d", pushes, wantPushes)
			}
			if got := shipHead(t, f); got != wantLocal {
				t.Fatalf("local HEAD = %s, want %s", got, wantLocal)
			}
			actualRemote := gitAt(t, f.Env(), f.Dir, "--git-dir="+f.RemoteDir, "rev-parse", "feature")
			if actualRemote != wantRemote {
				t.Fatalf("remote = %s, want %s", actualRemote, wantRemote)
			}
		})
	}
}

func TestShipExpectedRemoteRefusesBeforeCommit(t *testing.T) {
	f := shipRepo(t, vcstest.Remote(), vcstest.Dirty())
	before := shipHead(t, f)
	_, err := runShipCmd(f.Context(), t, "--expect-remote", before, "-m", "must not commit")
	if err == nil || !strings.Contains(err.Error(), "requires --no-commit") {
		t.Fatalf("expected mode refusal, got %v", err)
	}
	assertNoShipMutation(t, vcstest.Invocations(t, f.ArgvLog))
	if got := shipHead(t, f); got != before {
		t.Fatalf("invalid mode changed HEAD from %s to %s", before, got)
	}
}
