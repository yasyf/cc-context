package cli

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

// runFormat executes the format subcommand through the real root command (so
// SilenceUsage/SilenceErrors match production) with the given stdin and args,
// returning its stdout and the command error.
func runFormat(t *testing.T, stdin string, args ...string) (string, error) {
	t.Helper()
	cmd := NewRootCmd()
	var out bytes.Buffer
	cmd.SetIn(strings.NewReader(stdin))
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs(append([]string{"format"}, args...))
	err := cmd.Execute()
	return out.String(), err
}

func TestFormatFilterMode(t *testing.T) {
	out, err := runFormat(t, `[{"id":1,"name":"Ada"},{"id":2,"name":"Lin"}]`, "--format", "toon")
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if want := "[2]{id,name}:\n  1,Ada\n  2,Lin\n"; out != want {
		t.Errorf("filter out = %q, want %q", out, want)
	}
}

func TestFormatFilterTrailingNewlineOnConverted(t *testing.T) {
	out, err := runFormat(t, `{"a":1}`, "--format", "toon")
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if !strings.HasSuffix(out, "\n") {
		t.Errorf("converted output should end with newline, got %q", out)
	}
	if want := "a: 1\n"; out != want {
		t.Errorf("filter out = %q, want %q", out, want)
	}
}

func TestFormatFilterAutoFloorsToCompactJSON(t *testing.T) {
	out, err := runFormat(t, `{"a": 1, "b": [2, 3]}`)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if want := "{\"a\":1,\"b\":[2,3]}\n"; out != want {
		t.Errorf("auto out = %q, want %q", out, want)
	}
}

func TestFormatFilterPassthroughUntouched(t *testing.T) {
	out, err := runFormat(t, "hello not json")
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if out != "hello not json" {
		t.Errorf("passthrough out = %q, want byte-exact %q", out, "hello not json")
	}
}

func TestFormatFilterStrictErrors(t *testing.T) {
	_, err := runFormat(t, "not json", "--strict")
	if err == nil {
		t.Fatal("strict on bad JSON: want error, got nil")
	}
}

func TestFormatWrapperMode(t *testing.T) {
	out, err := runFormat(t, "", "--format", "toon", "--", "sh", "-c", `printf '[{"a":1}]'`)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if want := "[1]{a}:\n  1\n"; out != want {
		t.Errorf("wrapper out = %q, want %q", out, want)
	}
}

func TestFormatWrapperPropagatesExitCode(t *testing.T) {
	out, err := runFormat(t, "", "--", "sh", "-c", "echo not-json; exit 3")
	if err == nil {
		t.Fatal("non-zero child: want error, got nil")
	}
	var ee *ExitError
	if !errors.As(err, &ee) {
		t.Fatalf("error type = %T, want *ExitError", err)
	}
	if ee.Code != 3 {
		t.Errorf("exit code = %d, want 3", ee.Code)
	}
	if out != "not-json\n" {
		t.Errorf("passthrough stdout = %q, want %q", out, "not-json\n")
	}
}

func TestFormatWrapperZeroExitNoError(t *testing.T) {
	_, err := runFormat(t, "", "--", "sh", "-c", "exit 0")
	if err != nil {
		t.Fatalf("zero exit should not error, got %v", err)
	}
}

func TestFormatDelimiterFlag(t *testing.T) {
	tests := []struct {
		name string
		flag string
		want string
	}{
		{"tab", "tab", "[2\t]{a\tb}:\n  1\t2\n  3\t4\n"},
		{"pipe", "pipe", "[2|]{a|b}:\n  1|2\n  3|4\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, err := runFormat(t, `[{"a":1,"b":2},{"a":3,"b":4}]`, "--format", "toon", "--delimiter", tt.flag)
			if err != nil {
				t.Fatalf("Execute() error = %v", err)
			}
			if out != tt.want {
				t.Errorf("out = %q, want %q", out, tt.want)
			}
		})
	}
}

func TestFormatInvalidDelimiter(t *testing.T) {
	_, err := runFormat(t, `{"a":1}`, "--delimiter", "semicolon")
	if err == nil {
		t.Fatal("invalid delimiter: want error, got nil")
	}
	if !strings.Contains(err.Error(), "semicolon") {
		t.Errorf("error should name the bad delimiter: %v", err)
	}
}

func TestFormatInvalidFormat(t *testing.T) {
	_, err := runFormat(t, `{"a":1}`, "--format", "yaml")
	if err == nil {
		t.Fatal("invalid format: want error, got nil")
	}
	if !strings.Contains(err.Error(), "yaml") {
		t.Errorf("error should name the bad format: %v", err)
	}
}

func TestFormatIndentFlag(t *testing.T) {
	out, err := runFormat(t, `{"a":{"b":1}}`, "--format", "toon", "--indent", "4")
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if want := "a:\n    b: 1\n"; out != want {
		t.Errorf("out = %q, want %q", out, want)
	}
}

func TestFormatBudgetFlag(t *testing.T) {
	out, err := runFormat(t, `[{"id":1,"name":"Ada"},{"id":2,"name":"Lin"},{"id":3,"name":"Bob"}]`,
		"--format", "toon", "--budget", "5")
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if !strings.Contains(out, "tokens omitted") {
		t.Errorf("budget overflow should append a footer, got %q", out)
	}
}

func TestFormatExplicitSkipsByteNet(t *testing.T) {
	// A deeply-nested value where compact JSON is smaller; auto emits JSON.
	src := `{"aaa":{"bbb":{"ccc":{"ddd":{"eee":1}}}}}`

	jsonOut, err := runFormat(t, src, "--indent", "4")
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if !strings.HasPrefix(jsonOut, "{") {
		t.Errorf("auto should emit compact JSON, got %q", jsonOut)
	}

	toonOut, err := runFormat(t, src, "--indent", "4", "--format", "toon")
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if strings.HasPrefix(toonOut, "{") {
		t.Errorf("--format toon should emit TOON even when larger, got %q", toonOut)
	}
}

func TestFormatExplicitIncompatibleShapeErrors(t *testing.T) {
	_, err := runFormat(t, `{"a":1}`, "--format", "csv")
	if err == nil {
		t.Fatal("csv on an object: want error, got nil")
	}
	if !strings.Contains(err.Error(), "encode csv") {
		t.Errorf("error should carry the encoder prefix: %v", err)
	}
}

func TestFormatToonCommandGone(t *testing.T) {
	cmd := NewRootCmd()
	cmd.SetIn(strings.NewReader(`{"a":1}`))
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"toon"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("ccx toon should be an unknown command, got nil error")
	}
	if !strings.Contains(err.Error(), "toon") {
		t.Errorf("error should name the unknown command: %v", err)
	}
}
