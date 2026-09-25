package format

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yasyf/cc-context/internal/workspace"
)

func defaultOpts() Options {
	return Options{Format: FormatAuto, Indent: 2, Delimiter: DelimiterComma}
}

// TestConvertAutoSkipsLossyTOON pins the default path on a null-bearing table
// whose classifier candidates lead with TOON: the 26-digit decimal would not
// survive TOON's float64 canonicalization, so the engine skips TOON and auto
// falls through to a verbatim encoder that keeps every digit.
func TestConvertAutoSkipsLossyTOON(t *testing.T) {
	t.Parallel()
	const pi = "3.14159265358979323846264338"
	var b strings.Builder
	for i := range 400 {
		fmt.Fprintf(&b, "{\"v\":%s,\"n\":null,\"id\":%d}\n", pi, i)
	}
	got, converted, err := Convert(t.Context(), []byte(b.String()), defaultOpts())
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
	t.Parallel()
	opts := Options{Format: FormatAuto, Indent: 2, Delimiter: DelimiterComma, Strict: true}
	_, converted, err := Convert(t.Context(), []byte("not json"), opts)
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
	t.Parallel()
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
			t.Parallel()
			got, converted, err := Convert(t.Context(), []byte(tt.src), defaultOpts())
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
	t.Parallel()
	_, converted, err := Convert(t.Context(), []byte(`{"a":1}`), Options{Format: FormatCSV, Indent: 2, Delimiter: DelimiterComma})
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
	t.Parallel()
	_, converted, err := Convert(t.Context(), []byte(`{"a":1}`), Options{Format: Format("bogus"), Indent: 2, Delimiter: DelimiterComma})
	if err == nil {
		t.Fatal("Convert(bogus format): want error, got nil")
	}
	if converted {
		t.Error("Convert(bogus format) converted = true, want false")
	}
}

func TestRunConvertsStdout(t *testing.T) {
	t.Parallel()
	out, converted, code, err := Run(
		t.Context(),
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
	t.Parallel()
	var stderr bytes.Buffer
	out, converted, code, err := Run(
		t.Context(),
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
	t.Parallel()
	_, _, _, err := Run(
		t.Context(),
		[]string{"this-binary-does-not-exist-xyz"},
		defaultOpts(),
		nil, &bytes.Buffer{},
	)
	if err == nil {
		t.Fatal("Run() on missing binary: want error, got nil")
	}
}

func TestRunForwardsStdin(t *testing.T) {
	t.Parallel()
	in := strings.NewReader(`{"a":1}`)
	out, converted, code, err := Run(
		t.Context(),
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
	t.Parallel()
	_, _, _, err := Run(t.Context(), nil, defaultOpts(), nil, &bytes.Buffer{})
	if err == nil {
		t.Fatal("Run() with empty argv: want error, got nil")
	}
}

// oversizedJSON builds a valid JSON array whose encoding exceeds
// maxConvertBytes, for the ceiling cases below.
func oversizedJSON(t *testing.T) []byte {
	t.Helper()
	var b bytes.Buffer
	b.WriteString(`[`)
	for i := 0; b.Len() <= maxConvertBytes; i++ {
		if i > 0 {
			b.WriteString(",")
		}
		fmt.Fprintf(&b, `{"id":%d,"name":"row-%d-padding-padding-padding"}`, i, i)
	}
	b.WriteString(`]`)
	return b.Bytes()
}

// TestConvertPayloadCeiling covers the maxConvertBytes guard: the default auto,
// non-strict mode passes an oversized payload through verbatim (the same result
// the engine's call timeout produced, without the memory it cost), while strict
// mode and a forced encoder surface ErrPayloadTooLarge.
func TestConvertPayloadCeiling(t *testing.T) {
	t.Parallel()
	big := oversizedJSON(t)
	if len(big) <= maxConvertBytes {
		t.Fatalf("fixture is %d bytes, want > %d", len(big), maxConvertBytes)
	}

	tests := []struct {
		name      string
		opts      Options
		wantErr   bool
		converted bool
	}{
		{"auto non-strict passes through", Options{Format: FormatAuto, Indent: 2}, false, false},
		{"strict surfaces the ceiling", Options{Format: FormatAuto, Indent: 2, Strict: true}, true, false},
		{"forced encoder surfaces the ceiling", Options{Format: FormatTOON, Indent: 2}, true, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			out, converted, err := Convert(t.Context(), big, tt.opts)
			if tt.wantErr {
				if !errors.Is(err, ErrPayloadTooLarge) {
					t.Fatalf("err = %v, want ErrPayloadTooLarge", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Convert() error = %v, want nil", err)
			}
			if converted != tt.converted {
				t.Errorf("converted = %v, want %v", converted, tt.converted)
			}
			if out != string(big) {
				t.Errorf("output was not the verbatim passthrough (%d bytes vs %d)", len(out), len(big))
			}
		})
	}
}

// TestConvertUnderCeilingStillConverts pins that the guard does not disturb a
// payload below the ceiling.
func TestConvertUnderCeilingStillConverts(t *testing.T) {
	t.Parallel()
	src := []byte(`[{"id":1,"name":"a"},{"id":2,"name":"b"}]`)
	out, converted, err := Convert(t.Context(), src, defaultOpts())
	if err != nil {
		t.Fatalf("Convert() error = %v", err)
	}
	if !converted {
		t.Fatalf("converted = false, want true (out=%q)", out)
	}
}

// runTempRoot materializes marker.txt carrying tag in a symlink-resolved temp
// dir, and returns the dir.
func runTempRoot(t *testing.T, tag string) string {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("resolve temp root: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "marker.txt"), []byte(tag+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return root
}

// runIn runs argv through Run in the root ctx resolves, returning its trimmed
// stdout.
func runIn(ctx context.Context, t *testing.T, argv ...string) string {
	t.Helper()
	out, _, code, err := Run(ctx, argv, defaultOpts(), nil, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if code != 0 {
		t.Fatalf("Run() code = %d, want 0", code)
	}
	return strings.TrimSpace(out)
}

// TestRunExecutesInTheRootTheContextResolves covers both arms of the resolution
// Run drives: a context declaring a project root runs the child there, and one
// declaring none falls back to the directory ccx itself stands in.
func TestRunExecutesInTheRootTheContextResolves(t *testing.T) {
	t.Parallel()
	declared := runTempRoot(t, "declaredtree")
	ctx := workspace.WithRoot(t.Context(), declared)
	if got := runIn(ctx, t, "cat", "marker.txt"); got != "declaredtree" {
		t.Errorf("declared Run() read %q, want the declared tree's %q", got, "declaredtree")
	}

	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	cwd, err = filepath.EvalSymlinks(cwd)
	if err != nil {
		t.Fatalf("resolve cwd: %v", err)
	}
	ctx = workspace.WithRoot(ctx, "")
	if got := runIn(ctx, t, "pwd", "-P"); got != cwd {
		t.Errorf("undeclared Run() ran in %q, want ccx's own %q", got, cwd)
	}
}
