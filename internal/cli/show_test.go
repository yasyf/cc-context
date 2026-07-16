package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yasyf/cc-context/internal/vcs"
)

func TestShowHeader(t *testing.T) {
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
	cmd := newShowCmd()
	if cmd.Use != "show [ref]" {
		t.Errorf("Use = %q, want %q", cmd.Use, "show [ref]")
	}
	for _, flag := range []string{"scope", "budget"} {
		if cmd.Flags().Lookup(flag) == nil {
			t.Errorf("missing --%s flag", flag)
		}
	}
	if err := cmd.Args(cmd, []string{"a", "b"}); err == nil {
		t.Error("Args accepted two positionals, want at most one")
	}
}

// TestShowLiveSmoke runs the command against this real repo through the full
// native/jj pipeline. It is gated on CCX_LIVE_SMOKE so the default suite stays
// hermetic; run it with CCX_LIVE_SMOKE=1 go test -run TestShowLiveSmoke.
func TestShowLiveSmoke(t *testing.T) {
	if os.Getenv("CCX_LIVE_SMOKE") == "" {
		t.Skip("live smoke against the real repo; set CCX_LIVE_SMOKE=1 to run")
	}
	chdirRepoRoot(t)
	var out bytes.Buffer
	cmd := newShowCmd()
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"@-"})
	if err := cmd.Execute(); err != nil {
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

// chdirRepoRoot changes into the repository root (the nearest ancestor holding a
// .jj entry) so the diff pipeline's repo-root-relative raw-hunk paths resolve,
// restoring the original directory when the test ends.
func chdirRepoRoot(t *testing.T) {
	t.Helper()
	orig, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	dir := orig
	for {
		if _, err := os.Stat(filepath.Join(dir, ".jj")); err == nil {
			break
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("no .jj ancestor above %q", orig)
		}
		dir = parent
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("chdir %q: %v", dir, err)
	}
	t.Cleanup(func() { _ = os.Chdir(orig) })
}
