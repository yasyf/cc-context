package lfsclean

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const serveEnv = "LFSCLEAN_TEST_SERVE"

func TestMain(m *testing.M) {
	if os.Getenv(serveEnv) != "" {
		if err := Serve(os.Stdin, os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func gitIn(t *testing.T, dir, stdin string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...) //nolint:gosec // the test names every git argument
	cmd.Dir = dir
	cmd.Stdin = strings.NewReader(stdin)
	cmd.Env = append(os.Environ(), serveEnv+"=1", "HOME="+t.TempDir(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

func pointerTo(body []byte) string {
	return fmt.Sprintf("version https://git-lfs.github.com/spec/v1\noid sha256:%x\nsize %d\n", sha256.Sum256(body), len(body))
}

func trackedPointer(t *testing.T, body []byte) string {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	gitIn(t, dir, "", "init", "-q")
	gitIn(t, dir, "", "config", "filter.lfs.process", Command(executable))
	gitIn(t, dir, "", "config", "filter.lfs.required", "true")
	if err := os.WriteFile(filepath.Join(dir, ".gitattributes"), []byte("*.bin filter=lfs\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	blob := strings.TrimSpace(gitIn(t, dir, pointerTo(body), "hash-object", "-w", "--stdin", "--no-filters"))
	gitIn(t, dir, "", "update-index", "--add", "--cacheinfo", "100644,"+blob+",big.bin")
	gitIn(t, dir, "", "add", ".gitattributes")
	gitIn(t, dir, "", "-c", "user.name=test", "-c", "user.email=test@example.com", "commit", "-qm", "track a pointer")
	return dir
}

func TestGitReadsAnLFSFileThroughItsPointer(t *testing.T) {
	body := bytes.Repeat([]byte("0123456789abcdef"), 16<<10)
	edited := bytes.Clone(body)
	edited[len(edited)/2] ^= 1
	tests := []struct {
		name string
		file []byte
		want string
	}{
		{"the real file", body, ""},
		{"the pointer itself", []byte(pointerTo(body)), ""},
		{"another pointer", []byte(pointerTo(edited)), " M big.bin\n"},
		{"a same-size edit", edited, " M big.bin\n"},
		{"a shorter file", body[:1024], " M big.bin\n"},
		{"an empty file", nil, " M big.bin\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := trackedPointer(t, body)
			if err := os.WriteFile(filepath.Join(dir, "big.bin"), tt.file, 0o600); err != nil {
				t.Fatal(err)
			}
			if got := gitIn(t, dir, "", "status", "--porcelain=v1", "--no-renames"); got != tt.want {
				t.Errorf("status = %q, want %q", got, tt.want)
			}
			if _, err := os.Stat(filepath.Join(dir, ".git", "lfs")); !os.IsNotExist(err) {
				t.Errorf(".git/lfs exists: %v", err)
			}
		})
	}
}

func TestCommandQuotesTheBinaryForTheShellGitRunsItIn(t *testing.T) {
	if got, want := Command("/a b/it's/ccx"), `'/a b/it'\''s/ccx' vcs cleanup lfs-clean`; got != want {
		t.Errorf("Command() = %q, want %q", got, want)
	}
}
