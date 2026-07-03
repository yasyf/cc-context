package render

import (
	"strings"
	"testing"

	"github.com/yasyf/cc-context/internal/anchor"
)

func TestSembleResultsGolden(t *testing.T) {
	got, err := SembleResults(readFixture(t, "semble_input.json"), anchor.NewFiles("testdata"))
	if err != nil {
		t.Fatalf("SembleResults: %v", err)
	}
	checkGolden(t, "semble_golden.txt", got)
}

func TestSembleResultsInvariants(t *testing.T) {
	got, err := SembleResults(readFixture(t, "semble_input.json"), anchor.NewFiles("testdata"))
	if err != nil {
		t.Fatalf("SembleResults: %v", err)
	}
	tests := []struct {
		name string
		line string
	}{
		{"header counts results", "# 2 results"},
		{"local result anchored, score to 2 sig figs", "example.go:19-21#"},
		{"local score rounded", " (0.48)"},
		{"remote result stays bare", "remote/only.go:3-5 (0.12)"},
		{"query echo dropped", "greet the user"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			has := strings.Contains(got, tt.line)
			wantAbsent := tt.name == "query echo dropped"
			if has == wantAbsent {
				verb := "contain"
				if wantAbsent {
					verb = "not contain"
				}
				t.Errorf("want output to %s %q\n---\n%s", verb, tt.line, got)
			}
		})
	}
}

func TestSembleResultsParseError(t *testing.T) {
	if _, err := SembleResults("not json", anchor.NewFiles("testdata")); err == nil {
		t.Fatal("want error on malformed JSON, got nil")
	}
}

func TestSembleResultsEmpty(t *testing.T) {
	got, err := SembleResults(`{"query":"x","results":[]}`, anchor.NewFiles("testdata"))
	if err != nil {
		t.Fatalf("SembleResults: %v", err)
	}
	if got != "# 0 results\n" {
		t.Errorf("empty results = %q, want %q", got, "# 0 results\n")
	}
}
