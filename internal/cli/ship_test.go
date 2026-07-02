package cli

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

const fakeHeadSHA = "abcdef0123456789abcdef0123456789abcdef01"

func fakeRunListJSON(sha string) string {
	return fmt.Sprintf(`[{"databaseId":42,"headSha":%q,"status":"in_progress"}]`, sha)
}

// writeShipFakes installs fake jj, git, and (when withGh) gh executables into
// dir. Each records its argv into $SHIP_LOG as a blank-line-delimited record
// (command name then one arg per line) and emits canned stdout so the ship
// command's parsing paths run without a real VCS or network.
func writeShipFakes(t *testing.T, dir string, withGh bool) {
	t.Helper()
	log := func(name string) string {
		return "{ printf '" + name + "\\n'; for a in \"$@\"; do printf '%s\\n' \"$a\"; done; printf '\\n'; } >> \"$SHIP_LOG\"\n"
	}

	jj := "#!/bin/sh\n" + log("jj") + `case "$*" in
  *first_line*) printf '%s\n%s' 'a1b2c3d' 'fix: frobnicate' ;;
  *name*) if [ -z "$JJ_NO_BOOKMARK" ]; then printf 'main'; fi ;;
  *commit_id*) printf '%s' '` + fakeHeadSHA + `' ;;
esac
exit 0
`
	git := "#!/bin/sh\n" + log("git") + `case "$1 $2" in
  "log -1") printf '%s\0%s' 'a1b2c3d' 'fix: frobnicate' ;;
  "branch --show-current") printf 'main\n' ;;
  "rev-parse HEAD") printf '%s' '` + fakeHeadSHA + `' ;;
esac
exit 0
`
	gh := "#!/bin/sh\n" + log("gh") + `case "$1 $2" in
  "run list") printf '%s' "$GH_RUN_LIST_JSON" ;;
  "run watch")
    if [ "${GH_WATCH_EXIT:-0}" != 0 ]; then printf 'run %s concluded failure\n' "$3" >&2; fi
    exit "${GH_WATCH_EXIT:-0}" ;;
esac
exit 0
`
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o700); err != nil { //nolint:gosec // fake executable must be owner-executable
			t.Fatalf("write fake %s: %v", name, err)
		}
	}
	write("jj", jj)
	write("git", git)
	if withGh {
		write("gh", gh)
	}
}

// setupShip stands up an isolated repo of the given marker (".git" or ".jj"),
// chdirs into it, puts the fakes on PATH, and points $SHIP_LOG at a fresh log.
// It returns the log path so a test can assert the exact argv sequence.
func setupShip(t *testing.T, marker string, withGh bool) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake shell scripts are POSIX-only")
	}
	dir := t.TempDir()
	if marker != "" {
		if err := os.Mkdir(filepath.Join(dir, marker), 0o750); err != nil {
			t.Fatalf("mkdir %s: %v", marker, err)
		}
	}
	binDir := filepath.Join(dir, "bin")
	if err := os.Mkdir(binDir, 0o750); err != nil {
		t.Fatalf("mkdir bin: %v", err)
	}
	writeShipFakes(t, binDir, withGh)

	old, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(old) })
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("chdir: %v", err)
	}

	t.Setenv("PATH", binDir)
	log := filepath.Join(dir, "ship.log")
	t.Setenv("SHIP_LOG", log)
	return log
}

func runShipCmd(t *testing.T, args ...string) (string, error) {
	t.Helper()
	cmd := newShipCmd()
	var out, errBuf bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errBuf)
	cmd.SetArgs(args)
	err := cmd.Execute()
	// A standalone ship command has no root to set SilenceUsage, so cobra appends
	// its usage to stdout after an error; the summary is always the first line.
	summary := out.String()
	if i := strings.IndexByte(summary, '\n'); i >= 0 {
		summary = summary[:i]
	}
	return summary, err
}

func readInvocations(t *testing.T, log string) [][]string {
	t.Helper()
	data, err := os.ReadFile(log)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatalf("read log: %v", err)
	}
	var got [][]string
	for _, rec := range strings.Split(string(data), "\n\n") {
		rec = strings.Trim(rec, "\n")
		if rec == "" {
			continue
		}
		got = append(got, strings.Split(rec, "\n"))
	}
	return got
}

func assertInvocations(t *testing.T, got, want [][]string) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Errorf("invocation sequence mismatch\n got: %v\nwant: %v", got, want)
	}
}

func TestShipCommitPushWatch(t *testing.T) {
	tests := []struct {
		name    string
		marker  string
		args    []string
		want    [][]string
		summary string
	}{
		{
			name:   "jj happy path",
			marker: ".jj",
			args:   []string{"-m", "fix: frobnicate"},
			want: [][]string{
				{"jj", "commit", "-m", "fix: frobnicate"},
				{"jj", "log", "-r", "@-", "--no-graph", "-T", jjDescribeTemplate},
				{"jj", "log", "-r", jjNearestBookmarkRevset, "--no-graph", "-T", jjBookmarkTemplate},
				{"jj", "bookmark", "move", "--from", jjNearestBookmarkRevset, "--to", "@-"},
				{"jj", "git", "push"},
				{"jj", "log", "-r", "@-", "--no-graph", "-T", "commit_id"},
				{"gh", "run", "list", "--limit", "1", "--json", "databaseId,headSha,status"},
				{"gh", "run", "watch", "42", "--exit-status"},
			},
			summary: `committed a1b2c3d "fix: frobnicate" · pushed main → origin · CI success`,
		},
		{
			name:   "git happy path",
			marker: ".git",
			args:   []string{"-m", "fix: frobnicate"},
			want: [][]string{
				{"git", "add", "-A"},
				{"git", "commit", "-m", "fix: frobnicate"},
				{"git", "log", "-1", "--format=%h%x00%s"},
				{"git", "branch", "--show-current"},
				{"git", "push"},
				{"git", "rev-parse", "HEAD"},
				{"gh", "run", "list", "--limit", "1", "--json", "databaseId,headSha,status"},
				{"gh", "run", "watch", "42", "--exit-status"},
			},
			summary: `committed a1b2c3d "fix: frobnicate" · pushed main → origin · CI success`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			log := setupShip(t, tt.marker, true)
			t.Setenv("GH_RUN_LIST_JSON", fakeRunListJSON(fakeHeadSHA))
			shipCIPollInterval = 0

			got, err := runShipCmd(t, tt.args...)
			if err != nil {
				t.Fatalf("ship error = %v", err)
			}
			if got != tt.summary {
				t.Errorf("summary = %q, want %q", got, tt.summary)
			}
			assertInvocations(t, readInvocations(t, log), tt.want)
		})
	}
}

func TestShipJJNeverInvokesGitCommit(t *testing.T) {
	log := setupShip(t, ".jj", true)
	t.Setenv("GH_RUN_LIST_JSON", fakeRunListJSON(fakeHeadSHA))
	shipCIPollInterval = 0

	if _, err := runShipCmd(t, "-m", "fix: frobnicate"); err != nil {
		t.Fatalf("ship error = %v", err)
	}
	for _, inv := range readInvocations(t, log) {
		if inv[0] == "git" {
			t.Errorf("jj path invoked git: %v", inv)
		}
	}
}

func TestShipCommitOnlyVariants(t *testing.T) {
	tests := []struct {
		name    string
		marker  string
		args    []string
		want    [][]string
		summary string
	}{
		{
			name:   "jj no-push",
			marker: ".jj",
			args:   []string{"-m", "fix: frobnicate", "--no-push"},
			want: [][]string{
				{"jj", "commit", "-m", "fix: frobnicate"},
				{"jj", "log", "-r", "@-", "--no-graph", "-T", jjDescribeTemplate},
			},
			summary: `committed a1b2c3d "fix: frobnicate" · not pushed`,
		},
		{
			name:   "git no-push",
			marker: ".git",
			args:   []string{"-m", "fix: frobnicate", "--no-push"},
			want: [][]string{
				{"git", "add", "-A"},
				{"git", "commit", "-m", "fix: frobnicate"},
				{"git", "log", "-1", "--format=%h%x00%s"},
			},
			summary: `committed a1b2c3d "fix: frobnicate" · not pushed`,
		},
		{
			name:   "jj amend no message",
			marker: ".jj",
			args:   []string{"--amend", "--no-push"},
			want: [][]string{
				{"jj", "squash", "--use-destination-message"},
				{"jj", "log", "-r", "@-", "--no-graph", "-T", jjDescribeTemplate},
			},
			summary: `committed a1b2c3d "fix: frobnicate" · not pushed`,
		},
		{
			name:   "jj amend with message",
			marker: ".jj",
			args:   []string{"--amend", "-m", "fix: frobnicate", "--no-push"},
			want: [][]string{
				{"jj", "squash", "-m", "fix: frobnicate"},
				{"jj", "log", "-r", "@-", "--no-graph", "-T", jjDescribeTemplate},
			},
			summary: `committed a1b2c3d "fix: frobnicate" · not pushed`,
		},
		{
			name:   "git amend no message",
			marker: ".git",
			args:   []string{"--amend", "--no-push"},
			want: [][]string{
				{"git", "add", "-A"},
				{"git", "commit", "--amend", "--no-edit"},
				{"git", "log", "-1", "--format=%h%x00%s"},
			},
			summary: `committed a1b2c3d "fix: frobnicate" · not pushed`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			log := setupShip(t, tt.marker, true)
			got, err := runShipCmd(t, tt.args...)
			if err != nil {
				t.Fatalf("ship error = %v", err)
			}
			if got != tt.summary {
				t.Errorf("summary = %q, want %q", got, tt.summary)
			}
			assertInvocations(t, readInvocations(t, log), tt.want)
		})
	}
}

func TestShipGitAmendForcePushes(t *testing.T) {
	log := setupShip(t, ".git", true)
	got, err := runShipCmd(t, "--amend", "-m", "fix: frobnicate", "--no-watch")
	if err != nil {
		t.Fatalf("ship error = %v", err)
	}
	want := `committed a1b2c3d "fix: frobnicate" · pushed main → origin`
	if got != want {
		t.Errorf("summary = %q, want %q", got, want)
	}
	assertInvocations(t, readInvocations(t, log), [][]string{
		{"git", "add", "-A"},
		{"git", "commit", "--amend", "-m", "fix: frobnicate"},
		{"git", "log", "-1", "--format=%h%x00%s"},
		{"git", "branch", "--show-current"},
		{"git", "push", "--force-with-lease"},
	})
}

func TestShipNoWatchSkipsCI(t *testing.T) {
	log := setupShip(t, ".jj", true)
	got, err := runShipCmd(t, "-m", "fix: frobnicate", "--no-watch")
	if err != nil {
		t.Fatalf("ship error = %v", err)
	}
	want := `committed a1b2c3d "fix: frobnicate" · pushed main → origin`
	if got != want {
		t.Errorf("summary = %q, want %q", got, want)
	}
	assertInvocations(t, readInvocations(t, log), [][]string{
		{"jj", "commit", "-m", "fix: frobnicate"},
		{"jj", "log", "-r", "@-", "--no-graph", "-T", jjDescribeTemplate},
		{"jj", "log", "-r", jjNearestBookmarkRevset, "--no-graph", "-T", jjBookmarkTemplate},
		{"jj", "bookmark", "move", "--from", jjNearestBookmarkRevset, "--to", "@-"},
		{"jj", "git", "push"},
	})
}

func TestShipCIStates(t *testing.T) {
	tests := []struct {
		name       string
		withGh     bool
		runListSHA string
		watchExit  string
		summary    string
		wantErr    bool
		wantWatch  bool
	}{
		{
			name:      "gh missing",
			withGh:    false,
			summary:   `committed a1b2c3d "fix: frobnicate" · pushed main → origin · CI gh-missing`,
			wantWatch: false,
		},
		{
			name:       "no run",
			withGh:     true,
			runListSHA: "0000000000000000000000000000000000000000",
			summary:    `committed a1b2c3d "fix: frobnicate" · pushed main → origin · CI no-run`,
			wantWatch:  false,
		},
		{
			name:       "failure",
			withGh:     true,
			runListSHA: fakeHeadSHA,
			watchExit:  "1",
			summary:    `committed a1b2c3d "fix: frobnicate" · pushed main → origin · CI failure`,
			wantErr:    true,
			wantWatch:  true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			log := setupShip(t, ".jj", tt.withGh)
			if tt.runListSHA != "" {
				t.Setenv("GH_RUN_LIST_JSON", fakeRunListJSON(tt.runListSHA))
			}
			if tt.watchExit != "" {
				t.Setenv("GH_WATCH_EXIT", tt.watchExit)
			}
			shipCIPollInterval = 0

			got, err := runShipCmd(t, "-m", "fix: frobnicate")
			if tt.wantErr && err == nil {
				t.Fatalf("expected error, got nil")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("unexpected error = %v", err)
			}
			if got != tt.summary {
				t.Errorf("summary = %q, want %q", got, tt.summary)
			}
			watched := false
			for _, inv := range readInvocations(t, log) {
				if len(inv) >= 3 && inv[0] == "gh" && inv[1] == "run" && inv[2] == "watch" {
					watched = true
				}
			}
			if watched != tt.wantWatch {
				t.Errorf("gh run watch invoked = %v, want %v", watched, tt.wantWatch)
			}
		})
	}
}

func TestShipJJNoBookmarkFails(t *testing.T) {
	log := setupShip(t, ".jj", true)
	t.Setenv("JJ_NO_BOOKMARK", "1")

	_, err := runShipCmd(t, "-m", "fix: frobnicate")
	if err == nil {
		t.Fatal("expected error when no bookmark matches, got nil")
	}
	if !strings.Contains(err.Error(), "no bookmark to advance") {
		t.Errorf("error = %v, want it to mention no bookmark to advance", err)
	}
	assertInvocations(t, readInvocations(t, log), [][]string{
		{"jj", "commit", "-m", "fix: frobnicate"},
		{"jj", "log", "-r", "@-", "--no-graph", "-T", jjDescribeTemplate},
		{"jj", "log", "-r", jjNearestBookmarkRevset, "--no-graph", "-T", jjBookmarkTemplate},
	})
}

func TestShipRequiresMessage(t *testing.T) {
	log := setupShip(t, ".jj", true)
	_, err := runShipCmd(t)
	if err == nil {
		t.Fatal("expected error when message missing, got nil")
	}
	if !strings.Contains(err.Error(), "required") {
		t.Errorf("error = %v, want it to mention message required", err)
	}
	if inv := readInvocations(t, log); inv != nil {
		t.Errorf("no VCS command should run when message is missing, got %v", inv)
	}
}

func TestShipNoRepoFails(t *testing.T) {
	log := setupShip(t, "", true)
	_, err := runShipCmd(t, "-m", "fix: frobnicate")
	if err == nil {
		t.Fatal("expected error outside a repo, got nil")
	}
	if !strings.Contains(err.Error(), "no git or jj repository") {
		t.Errorf("error = %v, want it to mention no repository", err)
	}
	if inv := readInvocations(t, log); inv != nil {
		t.Errorf("no VCS command should run outside a repo, got %v", inv)
	}
}
