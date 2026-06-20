package backend

import (
	"slices"
	"testing"
)

func TestTilthDiffArgv(t *testing.T) {
	tests := []struct {
		name       string
		translated string
		scope      string
		budget     int
		want       []string
	}{
		{"working tree (empty source omits positional)", "", "", 0, []string{"diff"}},
		{"ref passthrough", "HEAD~1", "", 0, []string{"diff", "HEAD~1"}},
		{"scope and budget", "main", "internal", 500, []string{"diff", "main", "--scope", "internal", "--budget", "500"}},
		{"empty source with scope", "", "cmd", 0, []string{"diff", "--scope", "cmd"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tilthDiffArgv(tt.translated, tt.scope, tt.budget); !slices.Equal(got, tt.want) {
				t.Errorf("tilthDiffArgv(%q, %q, %d) = %v, want %v", tt.translated, tt.scope, tt.budget, got, tt.want)
			}
		})
	}
}
