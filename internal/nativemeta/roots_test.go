package nativemeta

import (
	"fmt"
	"maps"
	"path/filepath"
	"reflect"
	"slices"
	"testing"
)

func TestNativeRootIndexMatchesBoundedWalk(t *testing.T) {
	for size := 0; size <= 4; size++ {
		branches := make([]string, size)
		for i := range branches {
			branches[i] = fmt.Sprintf("b%d", i)
		}
		parents := append([]string{"", "missing"}, branches...)
		graphs := 1
		for range size {
			graphs *= len(parents)
		}
		for graph := range graphs {
			byName := make(map[string]branchRow, size)
			value := graph
			for _, branch := range branches {
				byName[branch] = branchRow{branch: branch, parent: parents[value%len(parents)]}
				value /= len(parents)
			}
			for _, order := range [][]string{branches, reverseRoots(branches)} {
				visits := 0
				roots := indexRoots(order, func(branch string) string {
					visits++
					return byName[branch].parent
				})
				if len(roots) != size || visits > 2*size {
					t.Fatalf("size=%d graph=%d roots=%v visits=%d", size, graph, roots, visits)
				}
				for _, branch := range branches {
					if got, want := roots[branch], rootOf(byName, branch); got != want {
						t.Fatalf("size=%d graph=%d branch=%s roots=%v got=%q want=%q", size, graph, branch, byName, got, want)
					}
				}
			}
		}
	}
}

func TestNativeRootIndexKeepsEmptyNamedRoot(t *testing.T) {
	byName := map[string]branchRow{
		"":      {branch: ""},
		"child": {branch: "child"},
		"next":  {branch: "next", parent: "child"},
	}
	branches := slices.Sorted(maps.Keys(byName))
	roots := indexRoots(branches, func(branch string) string { return byName[branch].parent })
	for _, branch := range branches {
		if roots[branch] != rootOf(byName, branch) {
			t.Fatalf("branch=%q roots=%v", branch, roots)
		}
	}
}

func TestNativeRootIndexTraversalCost(t *testing.T) {
	for _, size := range []int{32, 256, 4096, 65536} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			branches := make([]string, size)
			byName := make(map[string]branchRow, size)
			for i := range branches {
				branches[i] = fmt.Sprintf("b%d", i)
				parent := ""
				if i > 0 {
					parent = branches[i-1]
				}
				byName[branches[i]] = branchRow{branch: branches[i], parent: parent}
			}
			for _, order := range [][]string{branches, reverseRoots(branches)} {
				visits := 0
				roots := indexRoots(order, func(branch string) string {
					visits++
					return byName[branch].parent
				})
				if visits != size || len(roots) != size {
					t.Fatalf("nodes=%d visits=%d roots=%d", size, visits, len(roots))
				}
				for _, branch := range branches {
					if roots[branch] != branches[0] {
						t.Fatalf("branch=%s root=%s", branch, roots[branch])
					}
				}
			}
			if size <= 256 {
				visits := 0
				for _, branch := range branches {
					root := boundedRootWalk(size, branch, func(branch string) string {
						visits++
						return byName[branch].parent
					})
					if root != rootOf(byName, branch) {
						t.Fatalf("legacy root=%q branch=%s", root, branch)
					}
				}
				if want := size * (size + 1) / 2; visits != want {
					t.Fatalf("legacy visits=%d want=%d", visits, want)
				}
				t.Logf("source-equivalent bounded walk: nodes=%d parent_reads=%d; indexed parent_reads=%d", size, visits, size)
			}
		})
	}
}

func TestNativePlannedReadKeepsCommonTrunkState(t *testing.T) {
	dir := repo(t)
	common := filepath.Join(dir, ".git")
	head := git(t, dir, "rev-parse", "main")
	for _, branch := range []string{"parent", "child", "other", "other-child"} {
		git(t, dir, "branch", branch, head)
	}
	git(t, dir, "update-ref", "refs/remotes/origin/main", head)
	git(t, dir, "symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/main")
	writeCatalog(t, common, catalog{SchemaVersion: 1, Stacks: []stack{
		{Trunk: branchRef{Branch: "main"}, Branches: []branchRef{{Branch: "parent", Base: head}, {Branch: "child", Base: head}}},
		{Trunk: branchRef{Branch: "other"}, Branches: []branchRef{{Branch: "other-child", Base: head}}},
	}})
	state, orphans, err := ReadOriginPlannedOrphans(t.Context(), common)
	if err != nil {
		t.Fatal(err)
	}
	want := State{
		"main":   {Trunk: true, Head: head},
		"parent": {Head: head, Parents: []Ref{{Ref: "main", SHA: head}}},
		"child":  {Head: head, Parents: []Ref{{Ref: "parent", SHA: head}}},
	}
	if !reflect.DeepEqual(state, want) || len(orphans) != 0 {
		t.Fatalf("state=%+v orphans=%+v want=%+v", state, orphans, want)
	}
}

func reverseRoots(branches []string) []string {
	out := slices.Clone(branches)
	slices.Reverse(out)
	return out
}

func boundedRootWalk(size int, branch string, parent func(string) string) string {
	for range size {
		next := parent(branch)
		if next == "" {
			return branch
		}
		branch = next
	}
	return ""
}
