package render

import (
	"strings"
	"testing"
	"time"

	"github.com/yasyf/cc-context/anchor"
)

func TestSembleResultsGolden(t *testing.T) {
	got, err := SembleResults(readFixture(t, "semble_input.json"), anchor.NewFiles("testdata"), "score")
	if err != nil {
		t.Fatalf("SembleResults: %v", err)
	}
	checkGolden(t, "semble_golden.txt", got)
}

func TestSembleResultsInvariants(t *testing.T) {
	got, err := SembleResults(readFixture(t, "semble_input.json"), anchor.NewFiles("testdata"), "score")
	if err != nil {
		t.Fatalf("SembleResults: %v", err)
	}
	tests := []struct {
		name string
		line string
	}{
		{"header counts results", "# 2 results"},
		{"local result anchored, score to 2 sig figs", "example.go:19-21#"},
		{"local score rounded", " (score=0.48)"},
		{"remote result stays bare", "remote/only.go:3-5 (score=0.12)"},
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
	if _, err := SembleResults("not json", anchor.NewFiles("testdata"), "score"); err == nil {
		t.Fatal("want error on malformed JSON, got nil")
	}
}

func TestSembleResultsEmpty(t *testing.T) {
	got, err := SembleResults(`{"query":"x","results":[]}`, anchor.NewFiles("testdata"), "score")
	if err != nil {
		t.Fatalf("SembleResults: %v", err)
	}
	if got != "# 0 results\n" {
		t.Errorf("empty results = %q, want %q", got, "# 0 results\n")
	}
}

func TestWithSlowSearchNote(t *testing.T) {
	const out = "# 0 results\n"
	if got := WithSlowSearchNote(out, SlowSearchThreshold); got != out {
		t.Errorf("threshold output = %q, want unchanged %q", got, out)
	}
	want := out + "# note: slow search (11s) — first search builds the semantic index; repeats are fast\n"
	if got := WithSlowSearchNote(out, SlowSearchThreshold+500*time.Millisecond); got != want {
		t.Errorf("slow output = %q, want %q", got, want)
	}
}
