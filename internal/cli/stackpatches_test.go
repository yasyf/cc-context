package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/yasyf/cc-context/internal/execstub"
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
	execstub.Write(t, git, script)
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
			ctx := f.Context()
			b, err := stackSnapshot(ctx, dir, tr, s, "feature", remote, submitted, nil, false, pin, true, false)
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
			firstErr := err
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
			remoteCalls, localCalls := 1, 1
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
			seen := map[string]bool{}
			for _, entry := range entries {
				seen[entry.Name()] = true
			}
			mustRun(t, f.Env(), f.Dir, "git", "config", "diff.noprefix", "true")
			again, againErr := stackSnapshot(ctx, dir, tr, s, "feature", remote, submitted, nil, false, pin, true, false)
			if !reflect.DeepEqual(again, b) || reflect.TypeOf(againErr) != reflect.TypeOf(firstErr) || fmt.Sprint(againErr) != fmt.Sprint(firstErr) {
				t.Fatalf("second snapshot = %+v, %v; first = %+v, %v", again, againErr, b, firstErr)
			}
			entries, err = os.ReadDir(output)
			if err != nil {
				t.Fatal(err)
			}
			freshCounts := map[string]int{}
			freshBytes := 0
			for _, entry := range entries {
				if seen[entry.Name()] || !strings.HasSuffix(entry.Name(), ".argv") {
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
				if bytes.Equal(patches[span], patch) || bytes.Contains(patch, []byte("diff --git a/")) {
					t.Fatalf("new snapshot reused patch bytes for %s after diff.noprefix changed", span)
				}
				freshCounts[span]++
				freshBytes += len(patch)
			}
			if !maps.Equal(freshCounts, want) {
				t.Fatalf("second snapshot full-patch calls = %v, want %v", freshCounts, want)
			}
			t.Logf("PATCH_READ_LIFECYCLE calls=3 total_bytes=%d same_parent_context=true config_changed=true decision_preserved=true", freshBytes)
		})
	}
}

func TestStackPatchSeriesPreservesGitSemantics(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"empty range", "empty commit", "merge", "binary", "textconv"} {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()
			f := vcstest.Repo(t)
			pin := gitAt(t, f.Env(), f.Dir, "rev-parse", "HEAD")
			switch kind {
			case "empty commit":
				mustRun(t, f.Env(), f.Dir, "git", "commit", "-qm", "empty", "--allow-empty")
			case "merge":
				mustRun(t, f.Env(), f.Dir, "git", "switch", "-qc", "side")
				writeShipFile(t, f.Dir, "side.txt", "side\n")
				mustRun(t, f.Env(), f.Dir, "git", "add", "side.txt")
				mustRun(t, f.Env(), f.Dir, "git", "commit", "-qm", "side")
				mustRun(t, f.Env(), f.Dir, "git", "switch", "-q", "main")
				writeShipFile(t, f.Dir, "main.txt", "main\n")
				mustRun(t, f.Env(), f.Dir, "git", "add", "main.txt")
				mustRun(t, f.Env(), f.Dir, "git", "commit", "-qm", "main")
				mustRun(t, f.Env(), f.Dir, "git", "merge", "-qm", "merge", "side")
			case "binary", "textconv":
				writeShipFile(t, f.Dir, "payload.bin", "\x00binary payload\n")
				if kind == "textconv" {
					converter := filepath.Join(t.TempDir(), "textconv")
					execstub.Write(t, converter, "#!/bin/sh\nexec /usr/bin/od -An -tx1 \"$1\"\n")
					writeShipFile(t, f.Dir, ".gitattributes", "payload.bin diff=fixture\n")
					mustRun(t, f.Env(), f.Dir, "git", "config", "diff.fixture.textconv", converter)
					mustRun(t, f.Env(), f.Dir, "git", "add", ".gitattributes")
				}
				mustRun(t, f.Env(), f.Dir, "git", "add", "payload.bin")
				mustRun(t, f.Env(), f.Dir, "git", "commit", "-qm", kind)
			}
			head := gitAt(t, f.Env(), f.Dir, "rev-parse", "HEAD")
			dir := render.Dir(f.Dir)
			output := recordStackPatchBytes(t, f)
			want, err := stackReadPatchSeries(f.Context(), dir, pin, head)
			if err != nil {
				t.Fatal(err)
			}
			if (kind == "empty commit" || kind == "merge") && want != nil {
				t.Fatalf("%s series = %v, want nil", kind, want)
			}
			if kind == "empty range" && (want == nil || len(want) != 0) {
				t.Fatalf("empty range series = %#v, want nonnil empty series", want)
			}
			ctx := context.WithValue(f.Context(), stackPatchSeriesKey{}, map[stackPatchRange][]string{})
			for range 2 {
				got, err := stackPatchSeries(ctx, dir, pin, head)
				if err != nil || !reflect.DeepEqual(got, want) {
					t.Fatalf("cached %s series = %#v, %v; native uncached = %#v", kind, got, err, want)
				}
			}
			replays, err := stackRemoteReplays(ctx, dir, head, head, pin)
			if err != nil || replays != (want != nil) {
				t.Fatalf("identical %s heads replay = %v, %v; series = %#v", kind, replays, err, want)
			}
			entries, err := os.ReadDir(output)
			if err != nil {
				t.Fatal(err)
			}
			calls := 0
			for _, entry := range entries {
				if strings.HasSuffix(entry.Name(), ".patch") {
					calls++
				}
			}
			wantCalls := 2
			if kind == "merge" {
				wantCalls = 0
			}
			if calls != wantCalls {
				t.Fatalf("%s patch calls = %d, want %d for one uncached control and one cached read", kind, calls, wantCalls)
			}
			t.Logf("PATCH_SEMANTICS kind=%q calls=%d nil=%v replay=%v", kind, calls, want == nil, replays)
		})
	}
}

func TestStackPatchSeriesDoesNotRetainReadErrors(t *testing.T) {
	t.Parallel()
	f := vcstest.Repo(t)
	dir := render.Dir(f.Dir)
	pin := gitAt(t, f.Env(), f.Dir, "rev-parse", "HEAD")
	tree := gitAt(t, f.Env(), f.Dir, "rev-parse", "HEAD^{tree}")
	body := []byte(fmt.Sprintf("tree %s\nparent %s\nauthor Fixture <fixture@example.com> 946684800 +0000\ncommitter Fixture <fixture@example.com> 946684800 +0000\n\nrecovered empty commit\n", tree, pin))
	head, err := render.RunCLIStdin(f.Context(), dir, "git", []string{"hash-object", "-t", "commit", "--stdin"}, body)
	if err != nil {
		t.Fatal(err)
	}
	head = strings.TrimSpace(head)
	ctx := context.WithValue(f.Context(), stackPatchSeriesKey{}, map[stackPatchRange][]string{})
	_, nativeErr := stackReadPatchSeries(f.Context(), dir, pin, head)
	_, cachedErr := stackPatchSeries(ctx, dir, pin, head)
	if nativeErr == nil || cachedErr == nil || nativeErr.Error() != cachedErr.Error() {
		t.Fatalf("missing object errors: native=%v, cached=%v", nativeErr, cachedErr)
	}
	written, err := render.RunCLIStdin(f.Context(), dir, "git", []string{"hash-object", "-w", "-t", "commit", "--stdin"}, body)
	if err != nil || strings.TrimSpace(written) != head {
		t.Fatalf("write object = %q, %v; want %s", written, err, head)
	}
	for range 2 {
		got, err := stackPatchSeries(ctx, dir, pin, head)
		if err != nil || got != nil {
			t.Fatalf("recovered empty-commit series = %#v, %v; want nil series without error", got, err)
		}
	}
}
