package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yasyf/cc-context/internal/render"
	"github.com/yasyf/cc-context/internal/vcstest"
)

func TestGitFetchPackIndexPublication(t *testing.T) {
	for _, tt := range []struct {
		name      string
		mode      string
		wantErr   bool
		wantIndex bool
	}{
		{"successful producer", "new", false, true},
		{"unchanged fetch", "unchanged", false, false},
		{"failed fetch", "failed", true, false},
		{"capture unavailable", "capture-failed", false, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := vcstest.Repo(t)
			objectDir := strings.TrimSpace(f.Out(t, "git", "rev-parse", "--path-format=absolute", "--git-path", "objects"))
			packDir := filepath.Join(objectDir, "pack")
			if err := os.MkdirAll(packDir, 0o700); err != nil {
				t.Fatal(err)
			}
			stage := t.TempDir()
			for _, name := range []string{"pack-new.idx", "pack-new.pack"} {
				if err := os.WriteFile(filepath.Join(stage, name), nil, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.Truncate(filepath.Join(stage, "pack-new.idx"), 8<<20); err != nil {
				t.Fatal(err)
			}
			marker := filepath.Join(t.TempDir(), "published")
			bin := t.TempDir()
			script := `#!/bin/sh
case "$1" in
rev-parse)
  if [ "$CCX_TEST_FETCH_MODE" = capture-failed ]; then exit 9; fi
  exec "$CCX_TEST_REAL_GIT" "$@"
  ;;
fetch)
  case "$CCX_TEST_FETCH_MODE" in
  failed) printf 'fetch failure\n' >&2; exit 5 ;;
  new) /bin/mv "$CCX_TEST_STAGE/pack-new.idx" "$CCX_TEST_PACK_DIR/"; /bin/mv "$CCX_TEST_STAGE/pack-new.pack" "$CCX_TEST_PACK_DIR/" ;;
  esac
  exit 0
  ;;
multi-pack-index)
  printf '%s\n' "$@" > "$CCX_TEST_PACK_MARKER"
  printf 'index publication failure\n' >&2
  exit 9
  ;;
esac
exit 99
`
			if err := os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0o700); err != nil {
				t.Fatal(err)
			}
			for key, value := range map[string]string{
				"CCX_GIT":              filepath.Join(bin, "git"),
				"CCX_TEST_REAL_GIT":    filepath.Join(f.ShimBin, "git"),
				"CCX_TEST_FETCH_MODE":  tt.mode,
				"CCX_TEST_STAGE":       stage,
				"CCX_TEST_PACK_DIR":    packDir,
				"CCX_TEST_PACK_MARKER": marker,
			} {
				f.Setenv(key, value)
			}
			f.PrependPATH(bin)
			err := gitFetch(f.Context(), render.Dir(f.Dir), "origin", "main")
			if (err != nil) != tt.wantErr {
				t.Fatalf("fetch = %v, want error %v", err, tt.wantErr)
			}
			if tt.wantErr && !strings.Contains(err.Error(), "fetch failure") {
				t.Fatalf("fetch error lost: %v", err)
			}
			args, err := os.ReadFile(marker)
			if !tt.wantIndex {
				if !os.IsNotExist(err) {
					t.Fatalf("unexpected publication %q, %v", args, err)
				}
				return
			}
			want := "multi-pack-index\n--object-dir=" + objectDir + "\nwrite\n--no-bitmap\n"
			if err != nil || string(args) != want {
				t.Fatalf("publication = %q, %v; want %q", args, err, want)
			}
		})
	}
}
