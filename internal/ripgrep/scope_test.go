package ripgrep

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/yasyf/cc-context/internal/render"

	"github.com/yasyf/cc-context/internal/backend"
)

func TestDirectoryScopeGlobs(t *testing.T) {
	bin, err := exec.LookPath("rg")
	if err != nil {
		t.Skip("rg not on PATH")
	}
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, ".git"), 0o750); err != nil {
		t.Fatal(err)
	}
	for name, text := range map[string]string{
		".gitignore":                          "node_modules/\n.worktrees/\ndist/\n# needle\n",
		".agents/guide.md":                    "needle\n",
		"docs/.gitignore":                     "# needle\n",
		"infra/.gitignore":                    ".terraform*/\n# needle\n",
		"infra/source.ts":                     "needle\n",
		"infra/node_modules/dep/index.js":     "needle\n",
		"infra/node_modules/dep/.gitignore":   "# needle\n",
		"infra/.worktrees/copy/source.ts":     "needle\n",
		"infra/.terraform/providers/provider": "needle\n",
		"infra/dist/out.js":                   "needle\n",
		"infra/generated/skip.ts":             "needle\n",
		"infra/docs/keep.ts":                  "needle\n",
		"infra/docs/drop.ts":                  "needle\n",
		"infra/binary.dat":                    "needle\x00binary\n",
	} {
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Chdir(root)
	cases := []struct {
		name               string
		globs, paths, want []string
	}{
		{"disjoint scopes retain basename matching", []string{"infra/**", ".agents/**", ".gitignore", "!**/generated/**"}, nil, []string{".agents/guide.md", ".gitignore", "docs/.gitignore", "infra/.gitignore", "infra/docs/drop.ts", "infra/docs/keep.ts", "infra/source.ts"}},
		{"negative excludes scope root", []string{"infra/**", ".agents/**", "!infra"}, nil, []string{".agents/guide.md"}},
		{"all scope roots excluded", []string{"infra/**", "!infra"}, nil, nil},
		{"ordered reinclusion", []string{"infra/docs/**", "!infra/docs/*.ts", "infra/docs/keep.ts"}, nil, []string{"infra/docs/keep.ts"}},
		{"later exclusion", []string{"infra/docs/**", "infra/docs/keep.ts", "!infra/docs/*.ts"}, nil, nil},
		{"explicit ignored file", nil, []string{"infra/node_modules/dep/index.js"}, []string{"infra/node_modules/dep/index.js"}},
		{"explicit ignored directory", []string{"infra/node_modules/**"}, nil, []string{"infra/node_modules/dep/index.js"}},
		{"overlapping scopes deduplicate", []string{"infra/**", "infra/docs/**", "!**/generated/**"}, nil, []string{"infra/docs/drop.ts", "infra/docs/keep.ts", "infra/source.ts"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			args := backend.Args{Query: "needle", Globs: tc.globs, Paths: tc.paths}
			var calls [][]string
			run := func(ctx context.Context, dir render.Dir, bin string, argv []string) (string, error) {
				calls = append(calls, append([]string(nil), argv...))
				return execEngine(ctx, dir, bin, argv)
			}
			groups, _, err := searchGroups(context.Background(), engineRipgrep, bin, testDir(t), args, run)
			if err != nil {
				t.Fatal(err)
			}
			var got []string
			for _, g := range groups {
				got = append(got, g.path)
				if len(g.lines) != 1 {
					t.Fatalf("%s has duplicate lines: %+v", g.path, g.lines)
				}
			}
			sort.Strings(got)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("paths=%v, want %v", got, tc.want)
			}
			if tc.name == "disjoint scopes retain basename matching" {
				want := [][]string{
					{"--json", "--fixed-strings", "--glob", "!**/generated/**", "-e", "needle", "--", "infra", ".agents"},
					{"--json", "--fixed-strings", "--glob", ".gitignore", "--glob", "!**/generated/**", "-e", "needle"},
				}
				if !reflect.DeepEqual(calls, want) {
					t.Fatalf("search argv=%q, want ignore-preserving %q", calls, want)
				}
			}
			args.FilesWithMatches = true
			files, _, err := searchFilesWithMatches(context.Background(), engineRipgrep, bin, testDir(t), args, execEngine)
			if err != nil {
				t.Fatal(err)
			}
			sort.Strings(files)
			if !reflect.DeepEqual(files, tc.want) {
				t.Fatalf("files-only=%v, want %v", files, tc.want)
			}
			if strings.Contains(strings.Join(got, "\n"), "binary.dat") {
				t.Fatal("binary content searched")
			}
		})
	}
}
