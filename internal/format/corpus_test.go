package format

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"testing"
)

// corpusDir holds the shared golden vectors the Rust harness (format-core/core/
// tests/corpus.rs) also gates on; this test is the Go-side cross-boundary gate.
var corpusDir = filepath.Join("..", "..", "format-core", "corpus")

type corpusVector struct {
	Name              string     `json:"name"`
	Input             string     `json:"input"`
	Opts              corpusOpts `json:"opts"`
	ExpectedFormat    *string    `json:"expected_format"`
	ExpectedOutput    *string    `json:"expected_output"`
	ExpectError       bool       `json:"expect_error"`
	ExpectPassthrough bool       `json:"expect_passthrough"`
}

type corpusOpts struct {
	Format    *string `json:"format"`
	Indent    *int    `json:"indent"`
	Delimiter *string `json:"delimiter"`
	Strict    *bool   `json:"strict"`
}

// TestCorpusThroughWASM runs every corpus vector through the real wazero engine
// (auto or forced per opts) and asserts expected_format/expected_output/
// expect_error/expect_passthrough where non-null. It mirrors corpus.rs, including
// the two locked >2^53 policy skips (toon-go quotes; this port rejects).
func TestCorpusThroughWASM(t *testing.T) {
	files, err := filepath.Glob(filepath.Join(corpusDir, "*.json"))
	if err != nil {
		t.Fatalf("glob corpus: %v", err)
	}
	if len(files) == 0 {
		t.Fatalf("no corpus vectors found under %s", corpusDir)
	}
	sort.Strings(files)

	var passed, skipped, failed int
	for _, path := range files {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		var vectors []corpusVector
		if err := json.Unmarshal(raw, &vectors); err != nil {
			t.Fatalf("decode %s: %v", path, err)
		}
		file := filepath.Base(path)
		for _, v := range vectors {
			switch runCorpusVector(t, file, v) {
			case outcomePass:
				passed++
			case outcomeSkip:
				skipped++
			case outcomeFail:
				failed++
			}
		}
	}

	t.Logf("corpus-through-wasm: %d passed, %d failed, %d skipped", passed, failed, skipped)
	if failed != 0 {
		t.Errorf("failed = %d, want 0", failed)
	}
	if skipped != 2 {
		t.Errorf("skipped = %d, want 2 (the locked >2^53 policy vectors)", skipped)
	}
}

type outcome int

const (
	outcomeFail outcome = iota
	outcomePass
	outcomeSkip
)

// runCorpusVector evaluates one vector through runEngine, reporting a failure on
// t and returning the outcome for the summary tally.
func runCorpusVector(t *testing.T, file string, v corpusVector) outcome {
	res, err := runEngine([]byte(v.Input), corpusVectorOpts(v))
	if err != nil {
		t.Errorf("FAIL %s :: %s — host error: %v", file, v.Name, err)
		return outcomeFail
	}

	switch {
	case v.ExpectPassthrough:
		if res.errKind == "not_json" {
			return outcomePass
		}
		t.Errorf("FAIL %s :: %s — want not_json passthrough, got %+v", file, v.Name, res)
		return outcomeFail

	case v.ExpectError:
		if res.errKind != "" {
			return outcomePass
		}
		t.Errorf("FAIL %s :: %s — want error, got %q", file, v.Name, res.text)
		return outcomeFail

	case res.errKind == "unsafe_number" && v.ExpectedOutput != nil:
		t.Logf("SKIP %s :: %s — locked >2^53 policy (toon-go quotes, this port rejects): %s", file, v.Name, res.errMsg)
		return outcomeSkip

	case res.errKind != "":
		t.Errorf("FAIL %s :: %s — unexpected %s error: %s", file, v.Name, res.errKind, res.errMsg)
		return outcomeFail
	}

	// expected_format is meaningful only on the auto path. A classify-only vector
	// (no pinned output) may floor to compact JSON; corpus.rs asserts its list.
	if isAutoVector(v) && v.ExpectedFormat != nil {
		got, want := string(res.format), *v.ExpectedFormat
		classifyOnly := v.ExpectedOutput == nil
		if got != want && !(classifyOnly && got == string(FormatJSON)) {
			t.Errorf("FAIL %s :: %s — chose %q, want %q", file, v.Name, got, want)
			return outcomeFail
		}
	}
	if v.ExpectedOutput != nil && res.text != *v.ExpectedOutput {
		t.Errorf("FAIL %s :: %s — output mismatch:\n want %q\n got  %q", file, v.Name, *v.ExpectedOutput, res.text)
		return outcomeFail
	}
	return outcomePass
}

func isAutoVector(v corpusVector) bool {
	return v.Opts.Format == nil || *v.Opts.Format == "auto"
}

// corpusVectorOpts maps a vector's opts to Options, sending an unparseable
// format name (e.g. "" or "bogus") to the engine so it yields unknown_format.
func corpusVectorOpts(v corpusVector) Options {
	opts := Options{Format: FormatAuto, Indent: 2, Delimiter: DelimiterComma}
	if !isAutoVector(v) {
		opts.Format = Format(*v.Opts.Format)
	}
	if v.Opts.Indent != nil {
		opts.Indent = *v.Opts.Indent
	}
	if v.Opts.Delimiter != nil {
		opts.Delimiter = corpusDelimiter(*v.Opts.Delimiter)
	}
	if v.Opts.Strict != nil {
		opts.Strict = *v.Opts.Strict
	}
	return opts
}

func corpusDelimiter(s string) Delimiter {
	switch s {
	case "\t":
		return DelimiterTab
	case "|":
		return DelimiterPipe
	default:
		return DelimiterComma
	}
}
