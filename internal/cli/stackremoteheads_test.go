package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yasyf/cc-context/internal/render"
)

func TestStackRemoteHeadsDropsABranchDeletedBeforeItsFetch(t *testing.T) {
	base := t.TempDir()
	remote := filepath.Join(base, "remote")
	local := filepath.Join(base, "local")
	mustRun(t, nil, base, "git", "init", "--bare", "-q", remote)
	mustRun(t, nil, base, "git", "clone", "-q", remote, local)
	mustRun(t, nil, local, "git", "config", "user.email", "test@example.com")
	mustRun(t, nil, local, "git", "config", "user.name", "Test")
	mustRun(t, nil, local, "git", "config", "commit.gpgsign", "false")
	if err := os.WriteFile(filepath.Join(local, "first.txt"), []byte("first\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	mustRun(t, nil, local, "git", "add", "first.txt")
	mustRun(t, nil, local, "git", "commit", "-qm", "first")
	mustRun(t, nil, local, "git", "push", "-q", "origin", "HEAD:refs/heads/feature", "HEAD:refs/heads/graphite-base/7")
	want := strings.TrimSpace(mustRun(t, nil, local, "git", "rev-parse", "HEAD"))
	mustRun(t, nil, local, "git", "update-ref", "-d", "refs/remotes/origin/feature")
	mustRun(t, nil, local, "git", "update-ref", "-d", "refs/remotes/origin/graphite-base/7")

	counter := filepath.Join(base, "upload-packs")
	uploadPack := filepath.Join(base, "upload-pack")
	script := "#!/bin/sh\n" +
		"n=$(cat '" + counter + "' 2>/dev/null || echo 0)\n" +
		"n=$((n + 1))\n" +
		"echo $n > '" + counter + "'\n" +
		"if [ $n = 2 ]; then git --git-dir='" + remote + "' update-ref -d refs/heads/graphite-base/7 || exit $?; fi\n" +
		"exec git upload-pack \"$@\"\n"
	if err := os.WriteFile(uploadPack, []byte(script), 0o755); err != nil { //nolint:gosec // upload-pack must be executable
		t.Fatal(err)
	}
	mustRun(t, nil, local, "git", "config", "remote.origin.uploadpack", uploadPack)

	heads, err := stackRemoteHeads(context.Background(), render.Dir(local), stackRebasePrefix, "origin", []string{"feature", "graphite-base/7"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if heads["feature"] != want {
		t.Errorf("feature = %q, want %s", heads["feature"], want)
	}
	if got, ok := heads["graphite-base/7"]; ok {
		t.Errorf("graphite-base/7 = %q, want it dropped once the remote deleted it", got)
	}
	if got := strings.TrimSpace(mustRun(t, nil, local, "git", "rev-parse", "refs/remotes/origin/feature")); got != want {
		t.Errorf("origin/feature = %s, want %s fetched", got, want)
	}
}
