package format

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"
)

func defaultOpts() Options {
	return Options{Format: FormatAuto, Indent: 2, Delimiter: DelimiterComma}
}

// TestConvertAutoSkipsLossyTOON pins the default path on a null-bearing table
// whose classifier candidates lead with TOON: the 26-digit decimal would not
// survive TOON's float64 canonicalization, so the engine skips TOON and auto
// falls through to a verbatim encoder that keeps every digit.
func TestConvertAutoSkipsLossyTOON(t *testing.T) {
	const pi = "3.14159265358979323846264338"
	var b strings.Builder
	for i := range 400 {
		fmt.Fprintf(&b, "{\"v\":%s,\"n\":null,\"id\":%d}\n", pi, i)
	}
	got, converted, err := Convert([]byte(b.String()), defaultOpts())
	if err != nil {
		t.Fatalf("Convert() error = %v", err)
	}
	if !converted {
		t.Fatal("Convert() converted = false, want true")
	}
	if !strings.Contains(got, pi) {
		t.Errorf("Convert() lost decimal precision; %q missing from output starting %q", pi, got[:200])
	}
}

func TestConvertStrict(t *testing.T) {
	opts := Options{Format: FormatAuto, Indent: 2, Delimiter: DelimiterComma, Strict: true}
	_, converted, err := Convert([]byte("not json"), opts)
	if err == nil {
		t.Fatal("Convert(strict) on bad JSON: want error, got nil")
	}
	if converted {
		t.Error("Convert(strict) converted = true, want false")
	}
}

// TestConvertPassthrough pins format.go's passthrough policy — the corpus test
// drives runEngine directly and bypasses it. Empty, whitespace-only, and
// non-JSON auto-mode input each return src verbatim with converted=false and no
// error, so the wrapper never corrupts non-JSON stdout.
func TestConvertPassthrough(t *testing.T) {
	tests := []struct {
		name string
		src  string
	}{
		{"empty input", ""},
		{"whitespace-only input", "   \n  "},
		{"non-json auto mode", "hello not json\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, converted, err := Convert([]byte(tt.src), defaultOpts())
			if err != nil {
				t.Fatalf("Convert() error = %v, want nil", err)
			}
			if converted {
				t.Errorf("Convert() converted = true, want false")
			}
			if got != tt.src {
				t.Errorf("Convert() = %q, want passthrough %q", got, tt.src)
			}
		})
	}
}

// TestConvertForcedShapeError pins the loud failure when a forced format cannot
// represent the payload: an explicit format never falls back to passthrough.
func TestConvertForcedShapeError(t *testing.T) {
	_, converted, err := Convert([]byte(`{"a":1}`), Options{Format: FormatCSV, Indent: 2, Delimiter: DelimiterComma})
	if err == nil {
		t.Fatal("Convert(csv on object): want error, got nil")
	}
	if converted {
		t.Error("Convert(csv on object) converted = true, want false")
	}
}

// TestConvertUnknownFormat pins the loud failure on a format name the engine
// cannot parse.
func TestConvertUnknownFormat(t *testing.T) {
	_, converted, err := Convert([]byte(`{"a":1}`), Options{Format: Format("bogus"), Indent: 2, Delimiter: DelimiterComma})
	if err == nil {
		t.Fatal("Convert(bogus format): want error, got nil")
	}
	if converted {
		t.Error("Convert(bogus format) converted = true, want false")
	}
}

func TestRunConvertsStdout(t *testing.T) {
	out, converted, code, err := Run(
		context.Background(),
		[]string{"sh", "-c", `printf '[{"a":1},{"a":2}]'`},
		Options{Format: FormatTOON, Indent: 2, Delimiter: DelimiterComma},
		nil, &bytes.Buffer{},
	)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if code != 0 {
		t.Errorf("Run() code = %d, want 0", code)
	}
	if !converted {
		t.Errorf("Run() converted = false, want true")
	}
	if want := "[2]{a}:\n  1\n  2"; out != want {
		t.Errorf("Run() out = %q, want %q", out, want)
	}
}

func TestRunNonZeroExitCapturesStderr(t *testing.T) {
	var stderr bytes.Buffer
	out, converted, code, err := Run(
		context.Background(),
		[]string{"sh", "-c", `echo boom 1>&2; echo not-json; exit 3`},
		defaultOpts(),
		nil, &stderr,
	)
	if err != nil {
		t.Fatalf("Run() error = %v, want nil (command ran, just failed)", err)
	}
	if code != 3 {
		t.Errorf("Run() code = %d, want 3", code)
	}
	if converted {
		t.Errorf("Run() converted = true, want false (stdout was not JSON)")
	}
	if out != "not-json\n" {
		t.Errorf("Run() out = %q, want passthrough %q", out, "not-json\n")
	}
	if got := strings.TrimSpace(stderr.String()); got != "boom" {
		t.Errorf("stderr = %q, want %q", got, "boom")
	}
}

func TestRunSpawnFailure(t *testing.T) {
	_, _, _, err := Run(
		context.Background(),
		[]string{"this-binary-does-not-exist-xyz"},
		defaultOpts(),
		nil, &bytes.Buffer{},
	)
	if err == nil {
		t.Fatal("Run() on missing binary: want error, got nil")
	}
}

func TestRunForwardsStdin(t *testing.T) {
	in := strings.NewReader(`{"a":1}`)
	out, converted, code, err := Run(
		context.Background(),
		[]string{"sh", "-c", "cat"},
		Options{Format: FormatTOON, Indent: 2, Delimiter: DelimiterComma},
		in, &bytes.Buffer{},
	)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if code != 0 {
		t.Errorf("Run() code = %d, want 0", code)
	}
	if !converted {
		t.Errorf("Run() converted = false, want true")
	}
	if want := "a: 1"; out != want {
		t.Errorf("Run() out = %q, want %q (stdin forwarded and converted)", out, want)
	}
}

func TestRunEmptyArgv(t *testing.T) {
	_, _, _, err := Run(context.Background(), nil, defaultOpts(), nil, &bytes.Buffer{})
	if err == nil {
		t.Fatal("Run() with empty argv: want error, got nil")
	}
}
