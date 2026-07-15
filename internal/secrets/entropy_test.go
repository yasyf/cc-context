package secrets

import (
	"math"
	"testing"
)

func TestShannonEntropy(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want float64
	}{
		{"empty", "", 0},
		{"single symbol", "aaaa", 0},
		{"two symbols", "ab", 1},
		{"four symbols", "abcd", 2},
		{"eight symbols", "abcdefgh", 3},
		{"skewed distribution", "1223334444", 1.8464393446710154},
		{"aws example key", "AKIAIOSFODNN7EXAMPLE", 3.684183719779189},
		// Two runes ('a', 'é') but three bytes: normalizing by byte length gives
		// 2·(1/3)·log2(3) ≈ 1.0566, not the 1.0 a rune-length divisor would.
		{"non-ascii normalizes by byte length", "aé", 1.0566416671474375},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := shannonEntropy(tt.in); math.Abs(got-tt.want) > 1e-12 {
				t.Errorf("shannonEntropy(%q) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}
