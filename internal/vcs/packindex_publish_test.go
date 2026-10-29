package vcs

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/yasyf/cc-context/internal/render"
)

func TestPackIndexPublisherCommandBoundary(t *testing.T) {
	for _, tt := range []struct {
		name      string
		unchanged bool
		size      int64
		exit      string
		wantCall  bool
		wantErr   bool
	}{
		{"unchanged", true, 8 << 20, "0", false, false},
		{"below threshold", false, (8 << 20) - 1, "0", false, false},
		{"due", false, 8 << 20, "0", true, false},
		{"publication failure", false, 8 << 20, "9", true, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			objectDir := t.TempDir()
			writePackPair(t, objectDir, "pack-new")
			index := filepath.Join(objectDir, "pack", "pack-new.idx")
			if err := os.Truncate(index, tt.size); err != nil {
				t.Fatal(err)
			}
			snapshot := PackIndexSnapshot{objectDir: objectDir}
			if tt.unchanged {
				snapshot.indexes = []string{"pack-new.idx"}
				if err := os.WriteFile(filepath.Join(objectDir, "pack", "multi-pack-index"), []byte("must not read"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			binDir := t.TempDir()
			argsPath := filepath.Join(t.TempDir(), "args")
			script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$CCX_TEST_PACK_ARGS\"\nexit \"$CCX_TEST_PACK_EXIT\"\n"
			if err := os.WriteFile(filepath.Join(binDir, "git"), []byte(script), 0o700); err != nil {
				t.Fatal(err)
			}
			ctx := render.WithEnv(t.Context(), "PATH="+binDir, "CCX_GIT="+filepath.Join(binDir, "git"), "CCX_TEST_PACK_ARGS="+argsPath, "CCX_TEST_PACK_EXIT="+tt.exit)
			err := snapshot.Publish(ctx, render.Dir(objectDir))
			if (err != nil) != tt.wantErr {
				t.Fatalf("Publish = %v; want error %v", err, tt.wantErr)
			}
			args, err := os.ReadFile(argsPath)
			if !tt.wantCall {
				if !os.IsNotExist(err) {
					t.Fatalf("unexpected command: %q, %v", args, err)
				}
				return
			}
			want := []string{"multi-pack-index", "--object-dir=" + objectDir, "write", "--no-bitmap"}
			got := strings.Split(strings.TrimSuffix(string(args), "\n"), "\n")
			if err != nil || !slices.Equal(got, want) {
				t.Fatalf("command = %q, %v; want %q", got, err, want)
			}
		})
	}
}
