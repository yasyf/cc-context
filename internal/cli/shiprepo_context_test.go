package cli

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/yasyf/cc-context/internal/vcstest"
)

func TestShipUsesExplicitRepositoryInsteadOfWarmForeignMetadata(t *testing.T) {
	f := shipPRFixture(t, vcstest.Branch("feat"))
	seedLaneRecords(f.Context(), t, f.Dir, laneSeed{
		nameWithOwner: "cli/cli", owner: "cli", public: true, permission: "READ", unaffiliated: true,
	})
	f.Setenv("GH_REPO_JSON", ghStdout(t, "repo-api-own"))
	shipPRCreated(t)
	t.Setenv("GH_REPO", fakePRRepo)
	f.Setenv("GIT_CONFIG_COUNT", "2")
	f.Setenv("GIT_CONFIG_KEY_0", "remote.origin.url")
	f.Setenv("GIT_CONFIG_VALUE_0", f.RemoteDir)
	f.Setenv("GIT_CONFIG_KEY_1", "remote.origin.pushurl")
	f.Setenv("GIT_CONFIG_VALUE_1", f.RemoteDir)
	dump := filepath.Join(t.TempDir(), "repository-context")
	f.Setenv("GH_CONTEXT_DUMP", dump)
	writeShipExecutable(t, f.ShimBin, "gh", "#!/bin/sh\n"+vcstest.RecordArgv("gh")+`
if [ "$1 $2" = 'api repos/{owner}/{repo}' ]; then
  printf '%s' "${GH_REPO-<unset>}" > "$GH_CONTEXT_DUMP"
fi
`+shipGHBody)
	if _, err := runShipCmd(f.Context(), t, "-m", "fix: explicit repository", "--no-watch", "--no-stack"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(dump)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != fakePRRepo {
		t.Fatalf("repository lookup GH_REPO = %q, want %q", data, fakePRRepo)
	}
	created := false
	for _, argv := range vcstest.Invocations(t, f.ArgvLog) {
		if len(argv) == 0 || argv[0] != "gh" {
			continue
		}
		for _, arg := range argv {
			if strings.HasPrefix(arg, "repos/cli/cli/") {
				t.Fatalf("foreign repository request: %q", argv)
			}
		}
		if slices.Contains(argv, "POST") && slices.Contains(argv, "repos/"+fakePRRepo+"/pulls") {
			created = true
		}
	}
	if !created {
		t.Fatal("no pull request created in explicit repository")
	}
}
