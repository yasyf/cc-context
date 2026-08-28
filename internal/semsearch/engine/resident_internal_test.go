package engine

import (
	"testing"
	"time"

	"github.com/yasyf/cc-context/internal/semsearch/index"
)

func key(repo string) indexKey { return indexKey{repo: repo} }

func seed(entries map[string]time.Duration) {
	residentIndex = map[indexKey]*residentEntry{}
	now := time.Now()
	for repo, age := range entries {
		residentIndex[key(repo)] = &residentEntry{idx: &index.Index{}, lastUsed: now.Add(-age)}
	}
}

func repos() map[string]bool {
	out := map[string]bool{}
	for k := range residentIndex {
		out[k.repo] = true
	}
	return out
}

func TestEvictOldestKeepsMostRecent(t *testing.T) {
	tests := []struct {
		name    string
		entries map[string]time.Duration
		keep    int
		want    map[string]bool
	}{
		{
			name:    "evicts the single least-recently-used",
			entries: map[string]time.Duration{"a": 3 * time.Minute, "b": 2 * time.Minute, "c": time.Minute},
			keep:    2,
			want:    map[string]bool{"b": true, "c": true},
		},
		{
			name:    "evicts down to one",
			entries: map[string]time.Duration{"a": 3 * time.Minute, "b": 2 * time.Minute, "c": time.Minute},
			keep:    1,
			want:    map[string]bool{"c": true},
		},
		{
			name:    "under the cap is untouched",
			entries: map[string]time.Duration{"a": time.Minute},
			keep:    2,
			want:    map[string]bool{"a": true},
		},
		{
			name:    "keep zero drops everything",
			entries: map[string]time.Duration{"a": time.Minute, "b": 2 * time.Minute},
			keep:    0,
			want:    map[string]bool{},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			seed(tt.entries)
			evictOldest(tt.keep)
			got := repos()
			if len(got) != len(tt.want) {
				t.Fatalf("retained %v, want %v", got, tt.want)
			}
			for r := range tt.want {
				if !got[r] {
					t.Errorf("evicted %q, want retained (got %v)", r, got)
				}
			}
		})
	}
}

func TestSweepIdleDropsOnlyStaleEntries(t *testing.T) {
	tests := []struct {
		name    string
		entries map[string]time.Duration
		want    map[string]bool
	}{
		{
			name:    "drops past the ttl, keeps within it",
			entries: map[string]time.Duration{"stale": residentIdleTTL + time.Minute, "fresh": time.Minute},
			want:    map[string]bool{"fresh": true},
		},
		{
			name:    "keeps an entry exactly at the ttl",
			entries: map[string]time.Duration{"edge": residentIdleTTL - time.Second},
			want:    map[string]bool{"edge": true},
		},
		{
			name:    "drops every stale entry",
			entries: map[string]time.Duration{"a": residentIdleTTL * 2, "b": residentIdleTTL * 3},
			want:    map[string]bool{},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			seed(tt.entries)
			SweepIdle()
			got := repos()
			if len(got) != len(tt.want) {
				t.Fatalf("retained %v, want %v", got, tt.want)
			}
			for r := range tt.want {
				if !got[r] {
					t.Errorf("swept %q, want retained (got %v)", r, got)
				}
			}
		})
	}
}
