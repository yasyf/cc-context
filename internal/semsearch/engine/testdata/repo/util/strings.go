package util

import "strings"

// Repeat joins a string to itself n times with a separator.
func Repeat(s, sep string, n int) string {
	parts := make([]string, n)
	for i := range parts {
		parts[i] = s
	}
	return strings.Join(parts, sep)
}
