package render

import (
	"context"
	"os"
	"runtime"
	"slices"
	"testing"
)

func TestRunCLIExitCodeWithoutEnvIsScopedToOneChild(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses /bin/sh to read the child environment")
	}
	for _, tc := range []struct {
		name  string
		env   []string
		value string
	}{
		{name: "process", value: "process-static"},
		{name: "context", env: []string{"GIT_CONFIG=context-static"}, value: "context-static"},
		{name: "empty", env: []string{"GIT_CONFIG="}, value: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("GIT_CONFIG", "process-static")
			ctx := WithEnv(context.Background(), append([]string{"CCX_KEEP=retained"}, tc.env...)...)
			before := slices.Clone(EnvFrom(ctx))
			argv := []string{"-c", `printf '%s|%s|%s' "${GIT_CONFIG+x}" "$GIT_CONFIG" "$CCX_KEEP"`}
			out, code, stderr, err := RunCLIExitCodeWithoutEnv(ctx, Dir(t.TempDir()), "/bin/sh", argv, []string{"GIT_CONFIG"})
			if err != nil || code != 0 || stderr != "" || out != "||retained" {
				t.Fatalf("filtered child = %q, %d, %q, %v", out, code, stderr, err)
			}
			if os.Getenv("GIT_CONFIG") != "process-static" || !slices.Equal(EnvFrom(ctx), before) {
				t.Fatal("per-call removal changed the process or context environment")
			}
			out, code, stderr, err = RunCLIExitCode(ctx, Dir(t.TempDir()), "/bin/sh", argv)
			if want := "x|" + tc.value + "|retained"; err != nil || code != 0 || stderr != "" || out != want {
				t.Fatalf("ordinary child = %q, %d, %q, %v; want %q", out, code, stderr, err, want)
			}
		})
	}
}
