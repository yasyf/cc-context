package cli

import (
	"bytes"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/yasyf/cc-context/internal/render"
	"github.com/yasyf/cc-context/internal/vcstest"
)

func recordStackPatchBytes(t *testing.T, f *vcstest.Fixture) string {
	t.Helper()
	output := t.TempDir()
	git := filepath.Join(f.ShimBin, "git")
	recorded := filepath.Join(f.ShimBin, "git-recorded")
	if err := os.Rename(git, recorded); err != nil {
		t.Fatal(err)
	}
	quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'" }
	script := "#!/bin/sh\n" +
		"patch=false\nfor arg do\ncase \"$arg\" in '--format=commit %H') patch=true;; esac\ndone\n" +
		"if [ \"$patch\" = true ]; then\n" +
		"out=" + quote(output) + "/$$\n" +
		"printf '%s\\0' \"$@\" > \"$out.argv\"\n" +
		quote(recorded) + " \"$@\" > \"$out.patch\"\nstatus=$?\n/bin/cat \"$out.patch\"\nexit \"$status\"\nfi\n" +
		"exec " + quote(recorded) + " \"$@\"\n"
	if err := os.WriteFile(git, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return output
}

func TestStackSnapshotPatchReadControl(t *testing.T) {
	t.Parallel()
	for _, replay := range []bool{true, false} {
		name := "diverged"
		if replay {
			name = "replayed"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := stackRebaseRepo(t, "feature")
			pin := gitAt(t, f.Env(), f.Dir, "rev-parse", "main")
			submitted := gitAt(t, f.Env(), f.Dir, "rev-parse", "feature")
			mustRun(t, f.Env(), f.Dir, "git", "switch", "-q", "feature")
			writeShipFile(t, f.Dir, "feature.txt", strings.Repeat("local amendment\n", 8192))
			mustRun(t, f.Env(), f.Dir, "git", "add", "feature.txt")
			mustRun(t, f.Env(), f.Dir, "git", "commit", "-qm", "local amendment", "--amend")
			local := gitAt(t, f.Env(), f.Dir, "rev-parse", "HEAD")
			mustRun(t, f.Env(), f.Dir, "git", "switch", "-qc", "server", pin)
			if replay {
				mustRun(t, append(f.Env(), "GIT_COMMITTER_NAME=server"), f.Dir, "git", "cherry-pick", local)
			} else {
				writeShipFile(t, f.Dir, "remote.txt", strings.Repeat("unowned remote patch\n", 8192))
				mustRun(t, f.Env(), f.Dir, "git", "add", "remote.txt")
				mustRun(t, f.Env(), f.Dir, "git", "commit", "-qm", "unowned remote patch")
			}
			remote := gitAt(t, f.Env(), f.Dir, "rev-parse", "HEAD")
			if local == remote || submitted == local || submitted == remote {
				t.Fatal("fixture heads must differ")
			}
			mustRun(t, f.Env(), f.Dir, "git", "switch", "-q", "feature")
			dir := render.Dir(f.Dir)
			tr, err := gtTrunkRefOffline(f.Context(), dir, stackRebasePrefix, "main")
			if err != nil {
				t.Fatal(err)
			}
			output := recordStackPatchBytes(t, f)
			s := gtBranchState{Head: local, Parents: []gtRef{{Ref: "main", SHA: pin}}}
			b, err := stackSnapshot(f.Context(), dir, tr, s, "feature", remote, submitted, nil, false, pin, true, false)
			wantBranch := stackRebaseBranch{Name: "feature", WasParent: "main", Local: local, Remote: remote, Head: local}
			if !reflect.DeepEqual(b, wantBranch) {
				t.Fatalf("snapshot = %+v, want %+v", b, wantBranch)
			}
			if replay {
				if err != nil {
					t.Fatal(err)
				}
			} else {
				var diverged stackDivergedError
				wantError := fmt.Sprintf("stack rebase: feature has diverged from origin/feature (local %.12s, remote %.12s) — someone pushed to it; reconcile the two by hand, then re-run", local, remote)
				if !errors.As(err, &diverged) || err.Error() != wantError {
					t.Fatalf("snapshot error = %v, want stackDivergedError %q", err, wantError)
				}
			}
			entries, err := os.ReadDir(output)
			if err != nil {
				t.Fatal(err)
			}
			counts := map[string]int{}
			patches := map[string][]byte{}
			total := 0
			for _, entry := range entries {
				if !strings.HasSuffix(entry.Name(), ".argv") {
					continue
				}
				argv, err := os.ReadFile(filepath.Join(output, entry.Name()))
				if err != nil {
					t.Fatal(err)
				}
				args := strings.Split(strings.TrimSuffix(string(argv), "\x00"), "\x00")
				span := args[len(args)-1]
				patch, err := os.ReadFile(filepath.Join(output, strings.TrimSuffix(entry.Name(), ".argv")+".patch"))
				if err != nil {
					t.Fatal(err)
				}
				if previous, ok := patches[span]; ok && !bytes.Equal(previous, patch) {
					t.Fatalf("same range %s produced different patch bytes", span)
				}
				patches[span] = patch
				counts[span]++
				total += len(patch)
			}
			remoteCalls, localCalls := 3, 2
			if replay {
				remoteCalls, localCalls = 2, 1
			}
			want := map[string]int{pin + ".." + remote: remoteCalls, pin + ".." + local: localCalls, pin + ".." + submitted: 1}
			if !maps.Equal(counts, want) {
				t.Fatalf("full-patch calls = %v, want %v", counts, want)
			}
			unique := 0
			for span, patch := range patches {
				unique += len(patch)
				t.Logf("PATCH_RANGE_CONTROL span=%s calls=%d bytes_per_call=%d", span, counts[span], len(patch))
			}
			if len(patches[pin+".."+remote]) < 64*1024 {
				t.Fatal("remote patch control is smaller than 64KiB")
			}
			t.Logf("PATCH_READ_CONTROL calls=%d total_bytes=%d unique_range_bytes=%d repeated_bytes=%d replay=%v", remoteCalls+localCalls+1, total, unique, total-unique, replay)
		})
	}
}
