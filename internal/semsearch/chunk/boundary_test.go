package chunk

import (
	"reflect"
	"testing"
)

// leaf builds a childless node spanning [start, end).
func leaf(start, end uint32) node { return node{start: start, end: end} }

// parent builds a node spanning [start, end) with the given children.
func parent(start, end uint32, kids ...node) node {
	return node{start: start, end: end, children: kids}
}

func TestMergeAdjacent(t *testing.T) {
	tests := []struct {
		name    string
		chunks  []boundary
		desired int
		want    []boundary
	}{
		{
			name:    "packs up to desired then breaks",
			chunks:  []boundary{{0, 10}, {10, 20}, {20, 35}},
			desired: 25,
			want:    []boundary{{0, 20}, {20, 35}},
		},
		{
			name:    "all fit",
			chunks:  []boundary{{0, 10}, {10, 20}, {20, 35}},
			desired: 40,
			want:    []boundary{{0, 35}},
		},
		{
			// Length is summed per span's extent, so a gap between spans is not
			// counted toward the budget even though the merged extent spans it.
			name:    "sums span extents across a gap",
			chunks:  []boundary{{0, 10}, {20, 30}},
			desired: 25,
			want:    []boundary{{0, 30}},
		},
		{
			name:    "single span",
			chunks:  []boundary{{5, 40}},
			desired: 25,
			want:    []boundary{{5, 40}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := mergeAdjacent(tt.chunks, tt.desired, byteLen)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("mergeAdjacent() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestMergeNodeInner(t *testing.T) {
	tests := []struct {
		name    string
		root    node
		desired int
		depth   int
		want    []boundary
	}{
		{
			name:    "leaf returns itself",
			root:    leaf(5, 15),
			desired: 25,
			want:    []boundary{{5, 15}},
		},
		{
			// A node shorter than minChunkSize (50) is not descended into.
			name:    "below min chunk size returns whole node",
			root:    parent(0, 40, leaf(0, 20), leaf(20, 40)),
			desired: 25,
			want:    []boundary{{0, 40}},
		},
		{
			// depth over recursionDepth (500) returns the node as one span.
			name:    "recursion depth guard",
			root:    parent(0, 100, leaf(0, 60), leaf(60, 100)),
			desired: 25,
			depth:   recursionDepth + 1,
			want:    []boundary{{0, 100}},
		},
		{
			name:    "packs siblings up to desired",
			root:    parent(0, 60, leaf(0, 20), leaf(20, 40), leaf(40, 60)),
			desired: 25,
			want:    []boundary{{0, 20}, {20, 40}, {40, 60}},
		},
		{
			// A child longer than desired is recursed into and split by its kids.
			name:    "oversized child is split",
			root:    parent(0, 100, parent(0, 100, leaf(0, 50), leaf(50, 100))),
			desired: 25,
			want:    []boundary{{0, 50}, {50, 100}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := mergeNodeInner(tt.root, tt.desired, tt.depth)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("mergeNodeInner() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestChunkLines(t *testing.T) {
	tests := []struct {
		name    string
		src     string
		desired int
		want    []boundary
	}{
		{
			name:    "merges all short lines",
			src:     "aa\nbb\ncc\n",
			desired: 750,
			want:    []boundary{{0, 9}},
		},
		{
			name:    "breaks per line when tight",
			src:     "aa\nbb\ncc\n",
			desired: 4,
			want:    []boundary{{0, 3}, {3, 6}, {6, 9}},
		},
		{
			// Length is measured in characters: "é\n" is 3 bytes but 2 chars, so
			// it fits with "ab\n" (3 chars) under a 5-char budget.
			name:    "character length metric",
			src:     "é\nab\n",
			desired: 5,
			want:    []boundary{{0, 6}},
		},
		{
			name:    "character length metric splits",
			src:     "é\nab\n",
			desired: 4,
			want:    []boundary{{0, 3}, {3, 6}},
		},
		{
			name:    "whitespace only yields nothing",
			src:     "  \n\t\n",
			desired: 750,
			want:    nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := chunkLines([]byte(tt.src), tt.desired)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("chunkLines() = %v, want %v", got, tt.want)
			}
		})
	}
}
