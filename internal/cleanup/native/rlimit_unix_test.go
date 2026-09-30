//go:build unix

package native

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

const (
	helperPrefix    = "helper: "
	nofileHelperEnv = "CCX_NATIVE_HELPER_NOFILE"
)

func runHelper(t *testing.T, test, env string) string {
	t.Helper()
	_, report := runHelperIn(t, "", test, env)
	return report
}

func runHelperIn(t *testing.T, dir, test, env string) (pid int, report string) {
	t.Helper()
	helper := exec.Command(os.Args[0], "-test.run=^"+test+"$") //nolint:gosec // the test binary re-invoking itself
	helper.Dir = dir
	helper.Env = append(os.Environ(), env)
	helper.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	out, err := helper.CombinedOutput()
	if err != nil {
		t.Fatalf("helper %s: %v\n%s", env, err, out)
	}
	for line := range strings.SplitSeq(string(out), "\n") {
		if report, ok := strings.CutPrefix(line, helperPrefix); ok {
			return helper.Process.Pid, report
		}
	}
	t.Fatalf("helper %s reported nothing:\n%s", env, out)
	return 0, ""
}

func nofile(t *testing.T) unix.Rlimit {
	t.Helper()
	var limit unix.Rlimit
	if err := unix.Getrlimit(unix.RLIMIT_NOFILE, &limit); err != nil {
		t.Fatalf("Getrlimit: %v", err)
	}
	return limit
}

func reportRaisedLimit(t *testing.T, start string) {
	soft, err := strconv.ParseUint(start, 10, 64)
	if err != nil {
		t.Fatalf("parse %q: %v", start, err)
	}
	limit := nofile(t)
	limit.Cur = soft
	if err := unix.Setrlimit(unix.RLIMIT_NOFILE, &limit); err != nil {
		t.Fatalf("Setrlimit(%d): %v", soft, err)
	}
	if err := RaiseFileLimit(); err != nil {
		t.Fatalf("RaiseFileLimit: %v", err)
	}
	fmt.Printf("%s%d\n", helperPrefix, nofile(t).Cur)
}

func TestRaiseFileLimit(t *testing.T) {
	if start := os.Getenv(nofileHelperEnv); start != "" {
		reportRaisedLimit(t, start)
		return
	}
	hard := nofile(t).Max
	tests := []struct {
		name  string
		start uint64
		want  uint64
	}{
		{"raised to the cap", 256, min(hard, fileLimit)},
		{"left alone above the cap", 9000, 9000},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.start > hard {
				t.Skipf("hard limit %d is below the %d this case starts from", hard, tt.start)
			}
			got := runHelper(t, "TestRaiseFileLimit", fmt.Sprintf("%s=%d", nofileHelperEnv, tt.start))
			if want := strconv.FormatUint(tt.want, 10); got != want {
				t.Errorf("soft limit after RaiseFileLimit from %d = %s, want %s", tt.start, got, want)
			}
		})
	}
}
