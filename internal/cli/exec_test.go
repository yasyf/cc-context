package cli_test

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yasyf/cc-context/internal/cli"
	"github.com/yasyf/cc-context/internal/codeexec"
)

// executeExec runs `ccx exec` with args and the given in-stream (nil leaves the
// default stdin), capturing the out and err streams separately.
func executeExec(t *testing.T, args []string, in string) (string, string, error) {
	t.Helper()
	t.Setenv("CCX_EXEC_MCP", "off")
	t.Setenv("CLAUDE_PLUGIN_DATA", t.TempDir())
	var out, errOut bytes.Buffer
	root := cli.NewRootCmd()
	root.SetOut(&out)
	root.SetErr(&errOut)
	if in != "" {
		root.SetIn(strings.NewReader(in))
	}
	root.SetArgs(append([]string{"exec"}, args...))
	err := root.Execute()
	return out.String(), errOut.String(), err
}

func TestExecScriptSources(t *testing.T) {
	if !codeexec.Supported {
		t.Skip(codeexec.UnsupportedReason)
	}
	tests := []struct {
		name  string
		arg   string // positional script
		file  string // script written to a temp file, passed via --file
		dash  bool   // pass --file - so the script comes from stdin
		stdin string // script offered on the in stream
	}{
		{name: "arg", arg: "40+2"},
		{name: "file", file: "40+2"},
		{name: "file dash reads stdin", dash: true, stdin: "40+2"},
		{name: "piped stdin", stdin: "40+2"},
		{name: "arg wins over stdin", arg: "40+2", stdin: "1+1"},
		{name: "arg wins over file", arg: "40+2", file: "1+1"},
		{name: "file wins over stdin", file: "40+2", stdin: "1+1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var args []string
			if tt.arg != "" {
				args = append(args, tt.arg)
			}
			if tt.file != "" {
				path := filepath.Join(t.TempDir(), "script.py")
				if err := os.WriteFile(path, []byte(tt.file), 0o600); err != nil {
					t.Fatalf("write script: %v", err)
				}
				args = append(args, "--file", path)
			}
			if tt.dash {
				args = append(args, "--file", "-")
			}
			out, errOut, err := executeExec(t, args, tt.stdin)
			if err != nil {
				t.Fatalf("Execute(exec %v) error = %v", args, err)
			}
			if out != "42" {
				t.Errorf("stdout = %q, want %q", out, "42")
			}
			if errOut != "" {
				t.Errorf("stderr = %q, want empty", errOut)
			}
		})
	}
}

func TestExecListTools(t *testing.T) {
	if !codeexec.Supported {
		t.Skip(codeexec.UnsupportedReason)
	}
	out, errOut, err := executeExec(t, []string{"--list-tools"}, "")
	if err != nil {
		t.Fatalf("Execute(exec --list-tools) error = %v", err)
	}
	for _, sig := range []string{"search(", "sh(cmd"} {
		if !strings.Contains(out, sig) {
			t.Errorf("stdout missing signature %q\n%s", sig, out)
		}
	}
	if errOut != "" {
		t.Errorf("stderr = %q, want empty", errOut)
	}
}

func TestExecMissingScript(t *testing.T) {
	if !codeexec.Supported {
		t.Skip(codeexec.UnsupportedReason)
	}
	tests := []struct {
		name string
		in   string
	}{
		{"no input", ""},
		{"blank piped stdin", " \n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, _, err := executeExec(t, nil, tt.in)
			if err == nil {
				t.Fatal("Execute(exec) error = nil, want missing-script error")
			}
			if !strings.Contains(err.Error(), "no script") {
				t.Errorf("error = %v, want it to name the missing script", err)
			}
			if out != "" {
				t.Errorf("stdout = %q, want empty", out)
			}
		})
	}
}
