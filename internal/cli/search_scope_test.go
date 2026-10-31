package cli

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/yasyf/cc-context/internal/render"
	"github.com/yasyf/cc-context/internal/workspace"
)

func TestSearchLiteralPathScope(t *testing.T) {
	rg, err := exec.LookPath("rg")
	if err != nil {
		t.Fatal(err)
	}
	phase := os.Getenv("CCX_LITERAL_SCOPE_PHASE")
	traceOutput := os.Getenv("CCX_LITERAL_SCOPE_OUTPUT")
	strace := ""
	if phase != "" {
		if phase != "before" && phase != "fixed" {
			t.Fatalf("unknown scope phase %q", phase)
		}
		strace, err = exec.LookPath("strace")
		if err != nil || traceOutput == "" {
			t.Fatalf("scope observation needs strace and output directory: %v", err)
		}
	}
	for _, count := range []int{32, 10000} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			root := t.TempDir()
			root, err = filepath.EvalSymlinks(root)
			if err != nil {
				t.Fatal(err)
			}
			for _, dir := range []string{"target", "unrelated", "bin"} {
				if err := os.Mkdir(filepath.Join(root, dir), 0o750); err != nil {
					t.Fatal(err)
				}
			}
			body := []byte("tail prepare_graph\n")
			for index := -1; index < count; index++ {
				name := filepath.Join(root, "target", "evidence.py")
				if index >= 0 {
					name = filepath.Join(root, "unrelated", fmt.Sprintf("entry-%05d.py", index))
				}
				if err := os.WriteFile(name, body, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			wrapper := "#!/bin/sh\nprintf '%s\\0' \"$@\" > \"$CCX_SCOPE_ARGV\"\n/bin/pwd > \"$CCX_SCOPE_CWD\"\nif [ -n \"$CCX_SCOPE_STRACE\" ]; then\nexec \"$CCX_SCOPE_STRACE\" -f -e trace=openat -o \"$CCX_SCOPE_TRACE\" \"$CCX_SCOPE_RG\" \"$@\"\nfi\nexec \"$CCX_SCOPE_RG\" \"$@\"\n"
			writeExecutable(t, filepath.Join(root, "bin", "rg"), wrapper)
			t.Chdir(root)
			for _, query := range []string{"tail", "prepare_graph"} {
				for _, operand := range []string{filepath.Join(root, "target"), filepath.Join(root, "target", "evidence.py"), "target"} {
					label := fmt.Sprintf("%s-%d-%s-%d", phase, count, query, len(operand))
					output := t.TempDir()
					if traceOutput != "" {
						output = filepath.Join(traceOutput, label)
						if err := os.MkdirAll(output, 0o750); err != nil {
							t.Fatal(err)
						}
					}
					ctx := render.WithEnv(workspace.WithRoot(t.Context(), root),
						"PATH="+filepath.Join(root, "bin")+string(os.PathListSeparator)+os.Getenv("PATH"),
						"CCX_SCOPE_RG="+rg, "CCX_SCOPE_STRACE="+strace,
						"CCX_SCOPE_ARGV="+filepath.Join(output, "argv"), "CCX_SCOPE_CWD="+filepath.Join(output, "cwd"),
						"CCX_SCOPE_TRACE="+filepath.Join(output, "openat.trace"))
					cmd := newSearchCmd()
					var out bytes.Buffer
					cmd.SetOut(&out)
					cmd.SetErr(&out)
					cmd.SetArgs([]string{query, operand, "--literal", "--budget", "3500", "-k", "20"})
					if err := cmd.ExecuteContext(ctx); err != nil {
						t.Fatal(err)
					}
					argv, err := os.ReadFile(filepath.Join(output, "argv"))
					if err != nil {
						t.Fatal(err)
					}
					cwd, err := os.ReadFile(filepath.Join(output, "cwd"))
					if err != nil || strings.TrimSpace(string(cwd)) != root {
						t.Fatalf("engine cwd=%q, want %q: %v", cwd, root, err)
					}
					before := phase == "before"
					hasOperand := strings.Contains(string(argv), "\x00--\x00"+operand+"\x00")
					if hasOperand == before || strings.Contains(out.String(), "unrelated/") != before {
						t.Fatalf("before=%t operand=%q argv=%q output=%s", before, operand, argv, out.String())
					}
					if !before && !strings.Contains(out.String(), "evidence.py") {
						t.Fatalf("selected evidence missing: %s", out.String())
					}
					if strace != "" {
						trace, err := os.ReadFile(filepath.Join(output, "openat.trace"))
						if err != nil {
							t.Fatal(err)
						}
						opened := map[string]bool{}
						for _, name := range regexp.MustCompile(`entry-[0-9]+\.py`).FindAllString(string(trace), -1) {
							opened[name] = true
						}
						want := 0
						if before {
							want = count
						}
						if len(opened) != want || !strings.Contains(string(trace), "evidence.py") {
							t.Fatalf("phase=%s unrelated opens=%d want=%d; selected open=%t", phase, len(opened), want, strings.Contains(string(trace), "evidence.py"))
						}
						t.Logf("phase=%s unrelated=%d query=%s operand=%q unrelated_opened=%d selected_opened=true", phase, count, query, operand, len(opened))
					}
				}
			}
		})
	}
}
