package vcs

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

const (
	fakeRepoViewJSON = `{"nameWithOwner":"yasyf/cc-context","owner":{"login":"YASYF"},"isPrivate":true,"viewerPermission":"ADMIN"}`
	fakeViewerJSON   = `{"data":{"viewer":{"login":"yasyf","organizations":{"nodes":[{"login":"Poetic"}]}}}}`
)

func TestRepoOwnership(t *testing.T) {
	tests := []struct {
		name                     string
		repo                     Repo
		writable, mine, personal bool
	}{
		{
			name:     "admin on own public repo",
			repo:     Repo{Owner: "yasyf", ViewerLogin: "yasyf", ViewerPermission: "ADMIN", Affiliated: true},
			writable: true, mine: true, personal: true,
		},
		{
			name:     "maintainer on an org repo",
			repo:     Repo{Owner: "poetic", ViewerLogin: "yasyf", ViewerPermission: "MAINTAIN", Affiliated: true},
			writable: true, mine: true,
		},
		{
			name: "write collaborator on a foreign public repo",
			repo: Repo{Owner: "cli", ViewerLogin: "yasyf", ViewerPermission: "WRITE"},
		},
		{
			name: "read on a foreign public repo",
			repo: Repo{Owner: "cli", ViewerLogin: "yasyf", ViewerPermission: "READ"},
		},
		{
			name: "read on a private repo",
			repo: Repo{Owner: "acme", ViewerLogin: "yasyf", ViewerPermission: "READ", IsPrivate: true},
			mine: true,
		},
		{
			name:     "owner login differing only in case",
			repo:     Repo{Owner: "YASYF", ViewerLogin: "yasyf", ViewerPermission: "READ", Affiliated: true},
			mine:     true,
			personal: true,
		},
		{
			name: "zero value is nobody's",
			repo: Repo{},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.repo.Writable(); got != tt.writable {
				t.Errorf("Writable() = %v, want %v", got, tt.writable)
			}
			if got := tt.repo.Mine(); got != tt.mine {
				t.Errorf("Mine() = %v, want %v", got, tt.mine)
			}
			if got := tt.repo.Personal(); got != tt.personal {
				t.Errorf("Personal() = %v, want %v", got, tt.personal)
			}
		})
	}
}

func TestLookupRepoCaches(t *testing.T) {
	root, calls := setupGitHubFake(t)
	ctx := context.Background()

	repo, err := LookupRepo(ctx, root, false)
	if err != nil {
		t.Fatalf("LookupRepo: %v", err)
	}
	if repo.NameWithOwner != "yasyf/cc-context" || !repo.IsPrivate || repo.ViewerPermission != "ADMIN" {
		t.Fatalf("repo = %+v, want the fake's fields", repo)
	}
	if !repo.Affiliated {
		t.Error("Affiliated = false, want true: owner YASYF matches viewer yasyf case-insensitively")
	}
	if !repo.Mine() || !repo.Personal() {
		t.Errorf("Mine() = %v, Personal() = %v, want both true", repo.Mine(), repo.Personal())
	}
	assertGHCalls(t, calls, 1, 1)

	if _, err := LookupRepo(ctx, root, false); err != nil {
		t.Fatalf("cached LookupRepo: %v", err)
	}
	assertGHCalls(t, calls, 1, 1)

	if _, err := LookupRepo(ctx, root, true); err != nil {
		t.Fatalf("refreshed LookupRepo: %v", err)
	}
	assertGHCalls(t, calls, 2, 2)

	bumpRepoSchema(t, root)
	if _, err := LookupRepo(ctx, root, false); err != nil {
		t.Fatalf("post-schema-bump LookupRepo: %v", err)
	}
	assertGHCalls(t, calls, 3, 2)
}

func TestLookupRepoWithoutGH(t *testing.T) {
	t.Setenv("CLAUDE_PLUGIN_DATA", t.TempDir())
	t.Setenv("PATH", t.TempDir())

	if _, err := LookupRepo(context.Background(), t.TempDir(), false); !strings.Contains(err.Error(), ErrNoGitHub.Error()) {
		t.Fatalf("error = %v, want it to wrap ErrNoGitHub", err)
	}
}

// setupGitHubFake installs a gh that answers both lookups and appends each
// "$1 $2" to a call log, and roots the cache at a fresh temp dir. It returns
// the repo root to look up and the call-log path.
func setupGitHubFake(t *testing.T) (root, calls string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake shell scripts are POSIX-only")
	}
	dir := t.TempDir()
	calls = filepath.Join(dir, "gh.calls")
	body := "#!/bin/sh\nprintf '%s\\n' \"$1 $2\" >> \"" + calls + `"
case "$1 $2" in
  "repo view") printf '%s' '` + fakeRepoViewJSON + `' ;;
  "api graphql") printf '%s' '` + fakeViewerJSON + `' ;;
  *) printf 'fake gh: unmatched argv: %s\n' "$*" >&2; exit 2 ;;
esac
exit 0
`
	binDir := filepath.Join(dir, "bin")
	if err := os.Mkdir(binDir, 0o750); err != nil {
		t.Fatalf("mkdir bin: %v", err)
	}
	if err := os.WriteFile(filepath.Join(binDir, "gh"), []byte(body), 0o700); err != nil { //nolint:gosec // fake executable must be owner-executable
		t.Fatalf("write fake gh: %v", err)
	}
	t.Setenv("PATH", binDir)
	t.Setenv("CLAUDE_PLUGIN_DATA", filepath.Join(dir, "cache"))
	return dir, calls
}

func assertGHCalls(t *testing.T, calls string, wantView, wantViewer int) {
	t.Helper()
	data, err := os.ReadFile(calls)
	if err != nil {
		t.Fatalf("read gh calls: %v", err)
	}
	log := string(data)
	if got := strings.Count(log, "repo view\n"); got != wantView {
		t.Errorf("gh repo view calls = %d, want %d\nlog: %s", got, wantView, log)
	}
	if got := strings.Count(log, "api graphql\n"); got != wantViewer {
		t.Errorf("gh api graphql calls = %d, want %d\nlog: %s", got, wantViewer, log)
	}
}

// bumpRepoSchema rewrites root's cached record with an unknown schema, the
// shape a future format change leaves behind.
func bumpRepoSchema(t *testing.T, root string) {
	t.Helper()
	path, err := RepoCachePath(root)
	if err != nil {
		t.Fatalf("repo cache path: %v", err)
	}
	var rec repoRecord
	data, err := os.ReadFile(path) //nolint:gosec // path is the test's own cache dir
	if err != nil {
		t.Fatalf("read cached record: %v", err)
	}
	if err := json.Unmarshal(data, &rec); err != nil {
		t.Fatalf("decode cached record: %v", err)
	}
	rec.Schema = githubSchema + 1
	bumped, err := json.Marshal(rec)
	if err != nil {
		t.Fatalf("encode bumped record: %v", err)
	}
	if err := os.WriteFile(path, bumped, 0o600); err != nil {
		t.Fatalf("write bumped record: %v", err)
	}
}
