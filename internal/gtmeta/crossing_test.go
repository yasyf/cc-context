package gtmeta

import (
	"fmt"
	"reflect"
	"testing"
)

func TestCrossingEnumeratesEdgesOnce(t *testing.T) {
	for _, n := range []int{32, 512} {
		t.Run(fmt.Sprintf("branches-%d", n), func(t *testing.T) {
			visits, enumerations := 0, 0
			got := crossing([]string{"native"}, func(yield func(string, string) bool) {
				enumerations++
				for i := n; i > 0; i-- {
					parent := "native"
					if i > 1 {
						parent = fmt.Sprintf("branch-%d", i-1)
					}
					visits++
					if !yield(fmt.Sprintf("branch-%d", i), parent) {
						return
					}
				}
			})
			t.Logf("branches=%d enumerations=%d edge_visits=%d crossed=%d", n, enumerations, visits, len(got))
			if len(got) != n {
				t.Fatalf("crossed %d branches, want %d", len(got), n)
			}
			if enumerations != 1 || visits != n {
				t.Fatalf("enumerations=%d edge_visits=%d; want one enumeration and %d visits", enumerations, visits, n)
			}
		})
	}
}

func TestCrossingPreservesReachability(t *testing.T) {
	for _, tc := range []struct {
		name   string
		native []string
		edges  [][2]string
		want   map[string]bool
	}{
		{"empty", nil, nil, map[string]bool{}},
		{"unconnected", []string{"native"}, [][2]string{{"legacy", "main"}}, map[string]bool{}},
		{"duplicates and siblings", []string{"native", "other", "native"}, [][2]string{{"c", "b"}, {"b", "native"}, {"b", "native"}, {"d", "other"}, {"legacy", "main"}}, map[string]bool{"b": true, "c": true, "d": true}},
		{"connected cycle", []string{"native"}, [][2]string{{"a", "b"}, {"b", "a"}, {"a", "native"}, {"c", "b"}}, map[string]bool{"a": true, "b": true, "c": true}},
		{"unconnected cycle", []string{"native"}, [][2]string{{"a", "b"}, {"b", "a"}}, map[string]bool{}},
		{"native branch is not crossed by membership", []string{"native", "other"}, [][2]string{{"native", "main"}}, map[string]bool{}},
		{"native branch with native parent", []string{"native", "other"}, [][2]string{{"native", "other"}, {"child", "native"}}, map[string]bool{"native": true, "child": true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := crossing(tc.native, func(yield func(string, string) bool) {
				for _, edge := range tc.edges {
					if !yield(edge[0], edge[1]) {
						return
					}
				}
			})
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("crossed=%v, want %v", got, tc.want)
			}
		})
	}
}

func TestCrossingWithoutNativeRootsDoesNotEnumerateEdges(t *testing.T) {
	enumerations, visits := 0, 0
	got := crossing(nil, func(yield func(string, string) bool) {
		enumerations++
		for i := 0; i < 512; i++ {
			visits++
			if !yield(fmt.Sprintf("legacy-%d", i), "main") {
				return
			}
		}
	})
	if len(got) != 0 || enumerations != 0 || visits != 0 {
		t.Fatalf("crossed=%v enumerations=%d edge_visits=%d; want no traversal", got, enumerations, visits)
	}
}

var crossingAllocationSink map[string]bool

func TestCrossingWithoutEdgesDoesNotAllocatePerNativeRoot(t *testing.T) {
	roots := make([]string, 1024)
	for i := range roots {
		roots[i] = fmt.Sprintf("native-%d", i)
	}
	enumerations := 0
	got := crossing(roots, func(func(string, string) bool) { enumerations++ })
	if len(got) != 0 || enumerations != 1 {
		t.Fatalf("crossed=%v enumerations=%d; want one empty edge enumeration", got, enumerations)
	}
	emptyEdges := func(func(string, string) bool) {}
	oneRoot := testing.AllocsPerRun(20, func() { crossingAllocationSink = crossing(roots[:1], emptyEdges) })
	withRoots := testing.AllocsPerRun(20, func() { crossingAllocationSink = crossing(roots, emptyEdges) })
	crossingAllocationSink = nil
	t.Logf("empty_edges native1_allocations=%g native1024_allocations=%g", oneRoot, withRoots)
	if withRoots != oneRoot {
		t.Fatalf("empty graph allocated %g times with 1024 native roots, want the single-root %g", withRoots, oneRoot)
	}
}
