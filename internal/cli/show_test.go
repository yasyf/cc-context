package cli

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/yasyf/cc-context/internal/vcs"
	"github.com/yasyf/cc-context/internal/workspace"
)

func TestShowHeader(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		c    vcs.Commit
		want string
	}{
		{
			name: "with body",
			c: vcs.Commit{
				ShortID: "abc1234",
				Author:  "Ada Lovelace",
				Email:   "ada@example.com",
				Date:    "2026-07-02",
				Subject: "Add the widget",
				Body:    "Explain the widget.",
			},
			want: "commit abc1234\n" +
				"Author: Ada Lovelace <ada@example.com>\n" +
				"Date:   2026-07-02\n" +
				"\n" +
				"Add the widget\n" +
				"\n" +
				"Explain the widget.\n" +
				"\n",
		},
		{
			name: "no body",
			c: vcs.Commit{
				ShortID: "abc1234",
				Author:  "Ada Lovelace",
				Email:   "ada@example.com",
				Date:    "2026-07-02",
				Subject: "Add the widget",
			},
			want: "commit abc1234\n" +
				"Author: Ada Lovelace <ada@example.com>\n" +
				"Date:   2026-07-02\n" +
				"\n" +
				"Add the widget\n" +
				"\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := showHeader(tt.c); got != tt.want {
				t.Errorf("showHeader() =\n%q\nwant\n%q", got, tt.want)
			}
		})
	}
}

func TestShowCmdMetadata(t *testing.T) {
	t.Parallel()
	cmd := newShowCmd()
	if cmd.Use != "show [ref]" {
		t.Errorf("Use = %q, want %q", cmd.Use, "show [ref]")
	}
	for _, flag := range []string{"glob", "budget"} {
		if cmd.Flags().Lookup(flag) == nil {
			t.Errorf("missing --%s flag", flag)
		}
	}
	if err := cmd.Args(cmd, []string{"a", "b"}); err == nil {
		t.Error("Args accepted two positionals, want at most one")
	}
}

// TestShowLiveSmoke runs the command against this real repo through the full
// native pipeline, on whichever of jj and git the checkout is — hence no ref,
// each backend's default being its own. CCX_LIVE_SMOKE gates it so the default
// suite stays hermetic: CCX_LIVE_SMOKE=1 go test -run TestShowLiveSmoke.
func TestShowLiveSmoke(t *testing.T) {
	t.Parallel()
	if os.Getenv("CCX_LIVE_SMOKE") == "" {
		t.Skip("live smoke against the real repo; set CCX_LIVE_SMOKE=1 to run")
	}
	var out bytes.Buffer
	cmd := newShowCmd()
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(nil)
	if err := cmd.ExecuteContext(workspace.WithRoot(t.Context(), ccxRepoRoot(t))); err != nil {
		t.Fatalf("show @- error = %v", err)
	}
	got := out.String()
	t.Logf("show @- output:\n%s", got)
	if !strings.HasPrefix(got, "commit ") {
		t.Errorf("output missing commit header:\n%s", got)
	}
	if !strings.Contains(got, "## ") {
		t.Errorf("output missing structural per-file section:\n%s", got)
	}
}

// rawAWSKey is a well-known example AWS access key id the aws-access-token
// masking rule fires on.
const rawAWSKey = "AKIAIOSFODNN7EXAMPLE" //nolint:gosec // AWS's documented example key id, not a credential

// TestShowMasksSecretOutput scripts a real git repo whose head commit adds a
// credentials file, then drives the show command end to end: the hunk line comes
// out masked in the file's path context with the shared footer after the cap,
// and --reveal-secrets prints it raw with no footer.
func TestShowMasksSecretOutput(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	dir := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...) //nolint:gosec // fixed git verb; dir is a test TempDir and args are literals
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	write := func(name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	git("init", "-q")
	git("config", "user.email", "t@example.com")
	git("config", "user.name", "Test")
	write("README.md", "# fixture\n")
	git("add", "-A")
	git("commit", "-qm", "init")
	write("creds.txt", "KEY = \""+rawAWSKey+"\"\n")
	git("add", "-A")
	git("commit", "-qm", "add creds")
	t.Chdir(dir)

	show := func(args ...string) string {
		t.Helper()
		var out bytes.Buffer
		cmd := newShowCmd()
		cmd.SetOut(&out)
		cmd.SetErr(&out)
		cmd.SetArgs(args)
		if err := cmd.Execute(); err != nil {
			t.Fatalf("show %v: %v\n%s", args, err, out.String())
		}
		return out.String()
	}

	got := show()
	if strings.Contains(got, rawAWSKey) {
		t.Errorf("show output leaked the raw secret:\n%s", got)
	}
	if !strings.Contains(got, "KEY = \"AKIA…[masked:aws-access-token]\"") {
		t.Errorf("show output missing the masked hunk line:\n%s", got)
	}
	footer := "# 1 secret(s) masked (aws-access-token) — --reveal-secrets prints raw\n" //nolint:gosec // footer text, not a credential
	if !strings.HasSuffix(got, footer) {
		t.Errorf("show output missing the secrets footer:\n%s", got)
	}

	reveal := show("--reveal-secrets")
	if !strings.Contains(reveal, rawAWSKey) {
		t.Errorf("show --reveal-secrets output missing the raw secret:\n%s", reveal)
	}
	if strings.Contains(reveal, "[masked:") || strings.Contains(reveal, "secret(s) masked") {
		t.Errorf("show --reveal-secrets output still masked:\n%s", reveal)
	}
}

// TestShowMasksHeaderSecret scripts a repo whose head commit carries a secret
// in its subject and body, then proves the show header masks both pathlessly
// with the shared footer, and that --reveal-secrets prints them raw.
func TestShowMasksHeaderSecret(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	dir := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...) //nolint:gosec // fixed git verb; dir is a test TempDir and args are literals
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git("init", "-q")
	git("config", "user.email", "t@example.com")
	git("config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("# fixture\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	git("add", "-A")
	git("commit", "-qm", "init")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("# fixture\nmore\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	git("add", "-A")
	git("commit", "-qm", "leak "+rawAWSKey+" in subject\n\nrotate "+rawAWSKey+" later")
	t.Chdir(dir)

	show := func(args ...string) string {
		t.Helper()
		var out bytes.Buffer
		cmd := newShowCmd()
		cmd.SetOut(&out)
		cmd.SetErr(&out)
		cmd.SetArgs(args)
		if err := cmd.Execute(); err != nil {
			t.Fatalf("show %v: %v\n%s", args, err, out.String())
		}
		return out.String()
	}

	got := show()
	if strings.Contains(got, rawAWSKey) {
		t.Errorf("show header leaked the raw secret:\n%s", got)
	}
	for _, want := range []string{
		"leak AKIA…[masked:aws-access-token] in subject",
		"rotate AKIA…[masked:aws-access-token] later",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("show header missing %q:\n%s", want, got)
		}
	}
	footer := "# 2 secret(s) masked (aws-access-token) — --reveal-secrets prints raw\n" //nolint:gosec // footer text, not a credential
	if !strings.HasSuffix(got, footer) {
		t.Errorf("show output missing the secrets footer:\n%s", got)
	}

	reveal := show("--reveal-secrets")
	if !strings.Contains(reveal, "leak "+rawAWSKey+" in subject") {
		t.Errorf("show --reveal-secrets header missing the raw secret:\n%s", reveal)
	}
	if strings.Contains(reveal, "[masked:") || strings.Contains(reveal, "secret(s) masked") {
		t.Errorf("show --reveal-secrets output still masked:\n%s", reveal)
	}
}

// ccxRepoRoot is cc-context's own checkout, the nearest ancestor of this source
// file holding a .jj or .git entry. It walks up from the file because TestMain
// stands the binary in an empty scratch directory, and returns the path rather
// than chdir'ing into it because the working directory is process-wide.
func ccxRepoRoot(t *testing.T) string {
	t.Helper()
	_, self, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller could not name this source file")
	}
	dir := filepath.Dir(self)
	for {
		for _, marker := range []string{".jj", ".git"} {
			if _, err := os.Stat(filepath.Join(dir, marker)); err == nil {
				return dir
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("no .jj or .git ancestor above %q", self)
		}
		dir = parent
	}
}
