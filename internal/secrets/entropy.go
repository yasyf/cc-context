package secrets

import "math"

// shannonEntropy reimplements gitleaks' Shannon entropy over data: character
// frequency, -Σ p·log2(p). Like gitleaks it counts runes but normalizes by
// byte length, so the two agree on the ASCII secrets the rules match.
func shannonEntropy(data string) float64 {
	if data == "" {
		return 0
	}
	counts := make(map[rune]int)
	for _, r := range data {
		counts[r]++
	}
	entropy := 0.0
	invLength := 1.0 / float64(len(data))
	for _, count := range counts {
		freq := float64(count) * invLength
		entropy -= freq * math.Log2(freq)
	}
	return entropy
}
