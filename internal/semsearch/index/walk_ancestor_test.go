package index

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"testing"
)

func TestWalkFilesScopedAncestorPolicy(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		gitRoot string
		gitFile bool
		scope   string
		files   map[string]string
		want    []string
	}{
		{
			name:    "root rules",
			gitRoot: ".",
			scope:   "scope",
			files: map[string]string{
				".gitignore":                   "/scope/generated/\n/scope/no.go\n",
				"scope/keep.go":                "package keep\n",
				"scope/no.go":                  "package no\n",
				"scope/generated/generated.go": "package generated\n",
			},
			want: []string{"keep.go"},
		},
		{
			name:    "linked git entry",
			gitRoot: ".",
			gitFile: true,
			scope:   "scope",
			files: map[string]string{
				".gitignore":                   "/scope/generated/\n",
				"scope/keep.go":                "package keep\n",
				"scope/generated/generated.go": "package generated\n",
			},
			want: []string{"keep.go"},
		},
		{
			name:    "nested ordered negation",
			gitRoot: ".",
			scope:   "scope/nested",
			files: map[string]string{
				".gitignore":               "/scope/nested/*.go\n",
				"scope/.gitignore":         "!nested/keep.go\n",
				"scope/nested/.gitignore":  "keep.go\n!override.go\n",
				"scope/nested/keep.go":     "package keep\n",
				"scope/nested/override.go": "package override\n",
				"scope/nested/drop.go":     "package drop\n",
			},
			want: []string{"override.go"},
		},
		{
			name:    "semble priority and extension negation",
			gitRoot: ".",
			scope:   "scope",
			files: map[string]string{
				".gitignore":          "/scope/*.go\n",
				".sembleignore":       "!/scope/first.go\n",
				"scope/.gitignore":    "!keep.go\n",
				"scope/.sembleignore": "first.go\nkeep.go\n!again.go\n!special.kjs\n",
				"scope/first.go":      "package first\n",
				"scope/keep.go":       "package keep\n",
				"scope/again.go":      "package again\n",
				"scope/drop.go":       "package drop\n",
				"scope/special.kjs":   "special\n",
			},
			want: []string{"again.go", "special.kjs"},
		},
		{
			name:    "explicit ignored root",
			gitRoot: ".",
			scope:   "scope/generated",
			files: map[string]string{
				".gitignore":                 "/scope/generated/\n",
				"scope/generated/.gitignore": "local.go\n",
				"scope/generated/keep.go":    "package keep\n",
				"scope/generated/local.go":   "package local\n",
			},
			want: []string{"keep.go"},
		},
		{
			name:    "nested repository boundary",
			gitRoot: "inner",
			scope:   "inner/scope",
			files: map[string]string{
				".git/HEAD":            "ref: refs/heads/main\n",
				".gitignore":           "/inner/scope/outer.go\n",
				"inner/.gitignore":     "/scope/inner.go\n",
				"inner/scope/outer.go": "package outer\n",
				"inner/scope/inner.go": "package inner\n",
			},
			want: []string{"outer.go"},
		},
		{
			name:  "non git root",
			scope: "scope",
			files: map[string]string{
				".gitignore":    "/scope/keep.go\n",
				"scope/keep.go": "package keep\n",
			},
			want: []string{"keep.go"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			if tt.gitRoot != "" {
				entry := filepath.Join(root, tt.gitRoot, ".git")
				if tt.gitFile {
					writeAncestorFixture(t, entry, "gitdir: .git-admin\n")
				} else if err := os.MkdirAll(entry, 0o750); err != nil {
					t.Fatal(err)
				}
			}
			for name, body := range tt.files {
				writeAncestorFixture(t, filepath.Join(root, name), body)
			}
			scope := filepath.Join(root, tt.scope)
			got, err := WalkFiles(context.Background(), scope, []string{".go"})
			if err != nil {
				t.Fatal(err)
			}
			rels := make([]string, len(got))
			for i, path := range got {
				rel, err := filepath.Rel(scope, path)
				if err != nil {
					t.Fatal(err)
				}
				rels[i] = filepath.ToSlash(rel)
			}
			sort.Strings(rels)
			if !slices.Equal(rels, tt.want) {
				t.Fatalf("SEMANTIC_ANCESTOR_RULE_MISSING got=%v want=%v", rels, tt.want)
			}
		})
	}
}

func TestWalkFilesScopedAncestorCost(t *testing.T) {
	var work []int
	for _, n := range []int{32, 10000} {
		t.Run(strconv.Itoa(n), func(t *testing.T) {
			root := t.TempDir()
			if err := os.Mkdir(filepath.Join(root, ".git"), 0o750); err != nil {
				t.Fatal(err)
			}
			writeAncestorFixture(t, filepath.Join(root, ".gitignore"), "/scope/generated/\n")
			writeAncestorFixture(t, filepath.Join(root, "scope", "keep.go"), "package keep\n")
			for i := range n {
				writeAncestorFixture(t, filepath.Join(root, "scope", "generated", fmt.Sprintf("generated-%05d.go", i)), "package generated\n")
			}
			ctx := &ancestorWalkWorkContext{Context: context.Background()}
			got, err := WalkFiles(ctx, filepath.Join(root, "scope"), []string{".go"})
			if err != nil {
				t.Fatal(err)
			}
			work = append(work, ctx.checks)
			t.Logf("SEMANTIC_ANCESTOR_WORK n=%d cancellation_checks=%d returned_files=%d", n, ctx.checks, len(got))
			if ctx.checks > 16 {
				t.Errorf("SEMANTIC_ANCESTOR_WORK_GROWTH n=%d checks=%d limit=16", n, ctx.checks)
			}
			if !slices.Equal(got, []string{filepath.Join(root, "scope", "keep.go")}) {
				t.Errorf("SEMANTIC_ANCESTOR_RULE_MISSING n=%d returned=%d want=1", n, len(got))
			}
		})
	}
	if len(work) == 2 && work[0] != work[1] {
		t.Errorf("SEMANTIC_ANCESTOR_WORK_GROWTH checks32=%d checks10000=%d", work[0], work[1])
	}
}

type ancestorWalkWorkContext struct {
	context.Context
	checks int
}

func (ctx *ancestorWalkWorkContext) Err() error {
	ctx.checks++
	return ctx.Context.Err()
}

func writeAncestorFixture(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}
