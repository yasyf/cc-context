package cli

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

// runToon executes the toon subcommand through the real root command (so
// SilenceUsage/SilenceErrors match production) with the given stdin and args,
// returning its stdout and the command error.
func runToon(t *testing.T, stdin string, args ...string) (string, error) {
	t.Helper()
	cmd := NewRootCmd()
	var out bytes.Buffer
	cmd.SetIn(strings.NewReader(stdin))
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs(append([]string{"toon"}, args...))
	err := cmd.Execute()
	return out.String(), err
}

func TestToonFilterMode(t *testing.T) {
	out, err := runToon(t, `[{"id":1,"name":"Ada"},{"id":2,"name":"Lin"}]`, "--force-toon")
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if want := "[2]{id,name}:\n  1,Ada\n  2,Lin\n"; out != want {
		t.Errorf("filter out = %q, want %q", out, want)
	}
}

func TestToonFilterTrailingNewlineOnConverted(t *testing.T) {
	out, err := runToon(t, `{"a":1}`, "--force-toon")
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

func TestToonFilterPassthroughUntouched(t *testing.T) {
	out, err := runToon(t, "hello not json")
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if out != "hello not json" {
		t.Errorf("passthrough out = %q, want byte-exact %q", out, "hello not json")
	}
}

func TestToonFilterStrictErrors(t *testing.T) {
	_, err := runToon(t, "not json", "--strict")
	if err == nil {
		t.Fatal("strict on bad JSON: want error, got nil")
	}
}

func TestToonWrapperMode(t *testing.T) {
	out, err := runToon(t, "", "--force-toon", "--", "sh", "-c", `printf '[{"a":1}]'`)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if want := "[1]{a}:\n  1\n"; out != want {
		t.Errorf("wrapper out = %q, want %q", out, want)
	}
}

func TestToonWrapperPropagatesExitCode(t *testing.T) {
	out, err := runToon(t, "", "--", "sh", "-c", "echo not-json; exit 3")
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

func TestToonWrapperZeroExitNoError(t *testing.T) {
	_, err := runToon(t, "", "--", "sh", "-c", "exit 0")
	if err != nil {
		t.Fatalf("zero exit should not error, got %v", err)
	}
}

func TestToonDelimiterFlag(t *testing.T) {
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
			out, err := runToon(t, `[{"a":1,"b":2},{"a":3,"b":4}]`, "--force-toon", "--delimiter", tt.flag)
			if err != nil {
				t.Fatalf("Execute() error = %v", err)
			}
			if out != tt.want {
				t.Errorf("out = %q, want %q", out, tt.want)
			}
		})
	}
}

func TestToonInvalidDelimiter(t *testing.T) {
	_, err := runToon(t, `{"a":1}`, "--delimiter", "semicolon")
	if err == nil {
		t.Fatal("invalid delimiter: want error, got nil")
	}
	if !strings.Contains(err.Error(), "semicolon") {
		t.Errorf("error should name the bad delimiter: %v", err)
	}
}

func TestToonIndentFlag(t *testing.T) {
	out, err := runToon(t, `{"a":{"b":1}}`, "--force-toon", "--indent", "4")
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if want := "a:\n    b: 1\n"; out != want {
		t.Errorf("out = %q, want %q", out, want)
	}
}

func TestToonBudgetFlag(t *testing.T) {
	out, err := runToon(t, `[{"id":1,"name":"Ada"},{"id":2,"name":"Lin"},{"id":3,"name":"Bob"}]`,
		"--force-toon", "--budget", "5")
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if !strings.Contains(out, "tokens omitted") {
		t.Errorf("budget overflow should append a footer, got %q", out)
	}
}

func TestToonForceToonOverridesFallback(t *testing.T) {
	// A deeply-nested value where compact JSON is smaller; default would emit JSON.
	src := `{"aaa":{"bbb":{"ccc":{"ddd":{"eee":1}}}}}`

	jsonOut, err := runToon(t, src, "--indent", "4")
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if !strings.HasPrefix(jsonOut, "{") {
		t.Errorf("default should fall back to compact JSON, got %q", jsonOut)
	}

	toonOut, err := runToon(t, src, "--indent", "4", "--force-toon")
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if strings.HasPrefix(toonOut, "{") {
		t.Errorf("--force-toon should emit TOON, got %q", toonOut)
	}
}
