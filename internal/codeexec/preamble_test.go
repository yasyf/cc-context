package codeexec

import (
	"context"
	"strings"
	"testing"

	monty "github.com/ewhauser/gomonty"
)

func TestPreamble(t *testing.T) {
	got := Preamble([]ToolSig{{"railway_deploy", "railway_deploy(service)", "deploy a service"}})

	subsetRules := []string{
		"Import each module on its own line",
		"classes",
		"match",
		"re, json, datetime, asyncio",
		"async def main()",
		"asyncio.run(main())",
		"Top-level return is illegal",
	}
	signatures := []string{
		"search(query", "sh(cmd)", "await",
		"railway_deploy(service)", "Reflected MCP tools",
	}
	for _, want := range append(subsetRules, signatures...) {
		if !strings.Contains(got, want) {
			t.Errorf("preamble missing %q", want)
		}
	}
	for _, banned := range []string{"--mcp", "servers=", "Connected MCP servers"} {
		if strings.Contains(got, banned) {
			t.Errorf("preamble still mentions %q", banned)
		}
	}
}

// TestPreambleExampleRuns proves the worked example shown to the model is valid
// monty: extracted verbatim from the preamble, it must compile, typecheck, and
// run against a stub grep.
func TestPreambleExampleRuns(t *testing.T) {
	rt := NewRuntime(map[string]HostFunc{
		"grep": func(_ context.Context, _ monty.Call) (monty.Value, error) {
			return monty.String("func RunDiffCLI() {\n\tx := 1"), nil
		},
	})
	got, err := rt.Run(context.Background(), preambleExample(t), 0)
	if err != nil {
		t.Fatalf("preamble example failed: %v", err)
	}
	if want := `["func RunDiffCLI() {"]`; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func preambleExample(t *testing.T) string {
	t.Helper()
	_, rest, ok := strings.Cut(preambleText, "Example:\n")
	if !ok {
		t.Fatal("preamble has no Example section")
	}
	var lines []string
	for _, ln := range strings.Split(rest, "\n") {
		if !strings.HasPrefix(ln, "  ") {
			break
		}
		lines = append(lines, strings.TrimPrefix(ln, "  "))
	}
	return strings.Join(lines, "\n")
}
