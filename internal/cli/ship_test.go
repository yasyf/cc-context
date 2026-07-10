package cli

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"
)

const (
	fakeHeadSHA        = "abcdef0123456789abcdef0123456789abcdef01"
	fakeRunListJSON    = `[{"databaseId":42,"workflowName":"ci","status":"in_progress","url":"https://github.com/x/actions/runs/42"}]`
	fakeRunViewSuccess = `{"workflowName":"ci","conclusion":"success","startedAt":"2026-07-08T18:00:00Z","updatedAt":"2026-07-08T18:00:58Z","url":"https://github.com/x/actions/runs/42","jobs":[]}`
	fakeRunViewFailure = `{"workflowName":"ci","conclusion":"failure","startedAt":"2026-07-08T18:00:00Z","updatedAt":"2026-07-08T18:01:00Z","url":"https://github.com/x/actions/runs/42","jobs":[{"name":"test","conclusion":"failure","steps":[{"name":"go test ./...","conclusion":"failure"}]}]}`
)

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
  "run list")
    if [ -n "$GH_LIST_FAIL" ]; then printf 'gh: network timeout\n' >&2; exit 1; fi
    if [ -n "$GH_LIST_FAIL_MARKER" ] && [ -s "$GH_LIST_FAIL_MARKER" ]; then
      : > "$GH_LIST_FAIL_MARKER"; printf 'gh: transient tls timeout\n' >&2; exit 1
    fi
    if [ -n "$GH_LIST_SETTLE_MARKER" ]; then
      if [ -s "$GH_LIST_SETTLE_MARKER" ]; then printf '%s' "$GH_RUN_LIST_JSON_2"
      else printf 'x' > "$GH_LIST_SETTLE_MARKER"; printf '%s' "$GH_RUN_LIST_JSON"; fi
    else
      printf '%s' "$GH_RUN_LIST_JSON"
    fi ;;
  "run watch")
    id="$3"
    eval "code=\${GH_WATCH_EXIT_$id:-\${GH_WATCH_EXIT:-0}}"
    printf 'watch stream %s\n' "$id"
    if [ "$code" != 0 ]; then printf 'run %s concluded failure\n' "$id" >&2; fi
    exit "$code" ;;
  "run view")
    id="$3"
    case "$*" in
      *--log-failed*) eval "printf '%s' \"\${GH_LOG_FAILED_$id:-\$GH_LOG_FAILED}\"" ;;
      *) eval "printf '%s' \"\${GH_RUN_VIEW_JSON_$id:-\$GH_RUN_VIEW_JSON}\"" ;;
    esac ;;
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

// runShipCmdFull runs ship with usage and cobra error echo silenced so the whole
// captured stdout (summary plus every report line) can be asserted verbatim.
func runShipCmdFull(t *testing.T, args ...string) (string, string, error) {
	t.Helper()
	cmd := newShipCmd()
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	var out, errBuf bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errBuf)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return out.String(), errBuf.String(), err
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
				{"gh", "run", "list", "--commit", fakeHeadSHA, "--limit", "10", "--json", "databaseId,workflowName,status,url"},
				{"gh", "run", "watch", "42", "--exit-status"},
				{"gh", "run", "view", "42", "--json", "workflowName,conclusion,startedAt,updatedAt,url,jobs"},
				{"gh", "run", "list", "--commit", fakeHeadSHA, "--limit", "10", "--json", "databaseId,workflowName,status,url"},
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
				{"gh", "run", "list", "--commit", fakeHeadSHA, "--limit", "10", "--json", "databaseId,workflowName,status,url"},
				{"gh", "run", "watch", "42", "--exit-status"},
				{"gh", "run", "view", "42", "--json", "workflowName,conclusion,startedAt,updatedAt,url,jobs"},
				{"gh", "run", "list", "--commit", fakeHeadSHA, "--limit", "10", "--json", "databaseId,workflowName,status,url"},
			},
			summary: `committed a1b2c3d "fix: frobnicate" · pushed main → origin · CI success`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			log := setupShip(t, tt.marker, true)
			t.Setenv("GH_RUN_LIST_JSON", fakeRunListJSON)
			t.Setenv("GH_RUN_VIEW_JSON", fakeRunViewSuccess)
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
	t.Setenv("GH_RUN_LIST_JSON", fakeRunListJSON)
	t.Setenv("GH_RUN_VIEW_JSON", fakeRunViewSuccess)
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
		name      string
		withGh    bool
		runList   string
		viewJSON  string
		watchExit string
		summary   string
		wantErr   bool
		wantWatch bool
	}{
		{
			name:      "gh missing",
			withGh:    false,
			summary:   `committed a1b2c3d "fix: frobnicate" · pushed main → origin · CI gh-missing`,
			wantWatch: false,
		},
		{
			name:      "no run",
			withGh:    true,
			runList:   "[]",
			summary:   `committed a1b2c3d "fix: frobnicate" · pushed main → origin · CI no-run`,
			wantWatch: false,
		},
		{
			name:      "failure",
			withGh:    true,
			runList:   fakeRunListJSON,
			viewJSON:  fakeRunViewFailure,
			watchExit: "1",
			summary:   `committed a1b2c3d "fix: frobnicate" · pushed main → origin · CI failure`,
			wantErr:   true,
			wantWatch: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			log := setupShip(t, ".jj", tt.withGh)
			if tt.runList != "" {
				t.Setenv("GH_RUN_LIST_JSON", tt.runList)
			}
			if tt.viewJSON != "" {
				t.Setenv("GH_RUN_VIEW_JSON", tt.viewJSON)
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

func TestShipCISuccessReportLine(t *testing.T) {
	setupShip(t, ".jj", true)
	t.Setenv("GH_RUN_LIST_JSON", fakeRunListJSON)
	t.Setenv("GH_RUN_VIEW_JSON", fakeRunViewSuccess)
	shipCIPollInterval = 0

	out, _, err := runShipCmdFull(t, "-m", "fix: frobnicate")
	if err != nil {
		t.Fatalf("ship error = %v", err)
	}
	want := "ci · success · 58s · https://github.com/x/actions/runs/42"
	if !strings.Contains(out, want) {
		t.Errorf("output missing run line %q\ngot:\n%s", want, out)
	}
}

func TestShipCIFailureDetail(t *testing.T) {
	setupShip(t, ".jj", true)
	t.Setenv("GH_RUN_LIST_JSON", fakeRunListJSON)
	t.Setenv("GH_RUN_VIEW_JSON", fakeRunViewFailure)
	t.Setenv("GH_WATCH_EXIT", "1")
	t.Setenv("GH_LOG_FAILED", "test\tgo test ./...\t##[error]FAIL: TestFrobnicate (0.01s)\n")
	shipCIPollInterval = 0

	out, _, err := runShipCmdFull(t, "-m", "fix: frobnicate")
	if err == nil {
		t.Fatal("expected error on CI failure, got nil")
	}
	for _, want := range []string{
		"· CI failure",
		"failed: test · go test ./...",
		"##[error]FAIL: TestFrobnicate",
		"full log: gh run view 42 --log-failed",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q\ngot:\n%s", want, out)
		}
	}
}

func TestShipCIBudgetCapsLog(t *testing.T) {
	bigLog := strings.Repeat("a padded log line stretched to about fifty chars\n", 900) // ~44 KB

	tests := []struct {
		name       string
		args       []string
		wantCapped bool
	}{
		{"default budget caps the excerpt", nil, true},
		{"budget 0 leaves it uncapped", []string{"--budget", "0"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setupShip(t, ".jj", true)
			t.Setenv("GH_RUN_LIST_JSON", fakeRunListJSON)
			t.Setenv("GH_RUN_VIEW_JSON", fakeRunViewFailure)
			t.Setenv("GH_LOG_FAILED", bigLog)
			shipCIPollInterval = 0

			args := append([]string{"-m", "fix: frobnicate"}, tt.args...)
			out, _, err := runShipCmdFull(t, args...)
			if err == nil {
				t.Fatal("expected error on CI failure, got nil")
			}
			capped := strings.Contains(out, "tokens omitted")
			if capped != tt.wantCapped {
				t.Errorf("capped = %v, want %v", capped, tt.wantCapped)
			}
			if !tt.wantCapped && !strings.Contains(out, bigLog[:len(bigLog)-1]) {
				t.Errorf("uncapped output should contain the whole log")
			}
			// The pointer line survives regardless of capping.
			if !strings.Contains(out, "full log: gh run view 42 --log-failed") {
				t.Errorf("missing full-log pointer\ngot tail:\n%s", out[max(0, len(out)-200):])
			}
		})
	}
}

func TestShipCIStripsANSI(t *testing.T) {
	setupShip(t, ".jj", true)
	t.Setenv("GH_RUN_LIST_JSON", fakeRunListJSON)
	t.Setenv("GH_RUN_VIEW_JSON", fakeRunViewFailure)
	t.Setenv("GH_LOG_FAILED", "\x1b[31mERROR\x1b[0m the build \x1b[1mboom\x1b[0m\n")
	shipCIPollInterval = 0

	out, _, err := runShipCmdFull(t, "-m", "fix: frobnicate")
	if err == nil {
		t.Fatal("expected error on CI failure, got nil")
	}
	if strings.Contains(out, "\x1b") {
		t.Errorf("ANSI escapes leaked into output: %q", out)
	}
	if !strings.Contains(out, "ERROR the build boom") {
		t.Errorf("stripped log text missing\ngot:\n%s", out)
	}
}

func TestShipCITransientPollTolerated(t *testing.T) {
	log := setupShip(t, ".jj", true)
	marker := filepath.Join(t.TempDir(), "fail-once")
	if err := os.WriteFile(marker, []byte("x"), 0o600); err != nil {
		t.Fatalf("write marker: %v", err)
	}
	t.Setenv("GH_LIST_FAIL_MARKER", marker)
	t.Setenv("GH_RUN_LIST_JSON", fakeRunListJSON)
	t.Setenv("GH_RUN_VIEW_JSON", fakeRunViewSuccess)
	shipCIPollInterval = 0

	got, err := runShipCmd(t, "-m", "fix: frobnicate")
	if err != nil {
		t.Fatalf("transient list error should be tolerated, got %v", err)
	}
	if want := "· CI success"; !strings.Contains(got, want) {
		t.Errorf("summary = %q, want it to contain %q", got, want)
	}
	listCalls := 0
	for _, inv := range readInvocations(t, log) {
		if len(inv) >= 3 && inv[0] == "gh" && inv[1] == "run" && inv[2] == "list" {
			listCalls++
		}
	}
	if listCalls < 2 {
		t.Errorf("expected the poll to retry (>=2 list calls), got %d", listCalls)
	}
}

func TestShipCIAllPollsFailStillReports(t *testing.T) {
	setupShip(t, ".jj", true)
	t.Setenv("GH_LIST_FAIL", "1")
	shipCIPollTries = 3
	shipCIPollInterval = 0
	t.Cleanup(func() { shipCIPollTries = 12 })

	out, _, err := runShipCmdFull(t, "-m", "fix: frobnicate")
	if err == nil {
		t.Fatal("expected error when every poll fails, got nil")
	}
	summary := out
	if i := strings.IndexByte(summary, '\n'); i >= 0 {
		summary = summary[:i]
	}
	if want := "· CI error"; !strings.Contains(summary, want) {
		t.Errorf("summary = %q, want it to contain %q (abort-before-summary regression)", summary, want)
	}
	if want := "check: gh run list --commit " + fakeHeadSHA; !strings.Contains(out, want) {
		t.Errorf("output missing %q\ngot:\n%s", want, out)
	}
}

func TestShipCIViewFailureIsError(t *testing.T) {
	setupShip(t, ".jj", true)
	t.Setenv("GH_RUN_LIST_JSON", fakeRunListJSON)
	// GH_RUN_VIEW_JSON unset: gh run view emits empty stdout, so the parse fails.
	shipCIPollInterval = 0

	out, _, err := runShipCmdFull(t, "-m", "fix: frobnicate")
	if err == nil {
		t.Fatal("expected error when gh run view cannot be parsed, got nil")
	}
	if want := "· CI error"; !strings.Contains(out, want) {
		t.Errorf("output missing %q\ngot:\n%s", want, out)
	}
}

func TestShipCIWatchErrViewGreenIsSuccess(t *testing.T) {
	setupShip(t, ".jj", true)
	t.Setenv("GH_RUN_LIST_JSON", fakeRunListJSON)
	t.Setenv("GH_RUN_VIEW_JSON", fakeRunViewSuccess)
	t.Setenv("GH_WATCH_EXIT", "1") // watch drops, view says success — view wins
	shipCIPollInterval = 0

	got, err := runShipCmd(t, "-m", "fix: frobnicate")
	if err != nil {
		t.Fatalf("view-green run should heal a dropped watch, got %v", err)
	}
	if want := "· CI success"; !strings.Contains(got, want) {
		t.Errorf("summary = %q, want it to contain %q", got, want)
	}
}

func TestShipCIMultiRunWatchesAll(t *testing.T) {
	log := setupShip(t, ".jj", true)
	t.Setenv("GH_RUN_LIST_JSON", `[`+
		`{"databaseId":42,"workflowName":"ci","status":"completed","url":"https://x/42"},`+
		`{"databaseId":43,"workflowName":"cc-notes","status":"completed","url":"https://x/43"}]`)
	t.Setenv("GH_RUN_VIEW_JSON_42", fakeRunViewSuccess)
	t.Setenv("GH_RUN_VIEW_JSON_43", `{"workflowName":"cc-notes","conclusion":"failure","startedAt":"2026-07-08T18:00:00Z","updatedAt":"2026-07-08T18:00:05Z","url":"https://x/43","jobs":[{"name":"notes","conclusion":"failure","steps":[{"name":"sync","conclusion":"failure"}]}]}`)
	t.Setenv("GH_WATCH_EXIT_43", "1")
	t.Setenv("GH_LOG_FAILED_43", "notes sync exploded\n")
	shipCIPollInterval = 0

	out, _, err := runShipCmdFull(t, "-m", "fix: frobnicate")
	if err == nil {
		t.Fatal("expected error when one of several runs is red, got nil")
	}
	watched := map[string]bool{}
	for _, inv := range readInvocations(t, log) {
		if len(inv) >= 4 && inv[0] == "gh" && inv[1] == "run" && inv[2] == "watch" {
			watched[inv[3]] = true
		}
	}
	if !watched["42"] || !watched["43"] {
		t.Errorf("expected both runs watched, got %v", watched)
	}
	for _, want := range []string{
		"· CI failure",
		"ci · success",
		"cc-notes · failure",
		"failed: notes · sync",
		"full log: gh run view 43 --log-failed",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q\ngot:\n%s", want, out)
		}
	}
}

func TestShipCISettleWatchesLateRuns(t *testing.T) {
	log := setupShip(t, ".jj", true)
	marker := filepath.Join(t.TempDir(), "settle")
	t.Setenv("GH_LIST_SETTLE_MARKER", marker)
	t.Setenv("GH_RUN_LIST_JSON", fakeRunListJSON) // first list: run 42 only
	t.Setenv("GH_RUN_LIST_JSON_2", `[`+
		`{"databaseId":42,"workflowName":"ci","status":"completed","url":"https://x/42"},`+
		`{"databaseId":44,"workflowName":"settle-late","status":"completed","url":"https://x/44"}]`)
	t.Setenv("GH_RUN_VIEW_JSON_42", fakeRunViewSuccess)
	t.Setenv("GH_RUN_VIEW_JSON_44", `{"workflowName":"settle-late","conclusion":"success","startedAt":"2026-07-08T18:00:00Z","updatedAt":"2026-07-08T18:00:10Z","url":"https://x/44","jobs":[]}`)
	shipCIPollInterval = 0

	out, _, err := runShipCmdFull(t, "-m", "fix: frobnicate")
	if err != nil {
		t.Fatalf("ship error = %v", err)
	}
	watched := map[string]bool{}
	for _, inv := range readInvocations(t, log) {
		if len(inv) >= 4 && inv[0] == "gh" && inv[1] == "run" && inv[2] == "watch" {
			watched[inv[3]] = true
		}
	}
	if !watched["42"] || !watched["44"] {
		t.Errorf("expected the settle pass to watch both runs, got %v", watched)
	}
	for _, want := range []string{"ci · success", "settle-late · success"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing settle report line %q\ngot:\n%s", want, out)
		}
	}
}

func TestShipCIBudgetFloorsPerRunShare(t *testing.T) {
	setupShip(t, ".jj", true)
	bigLog := strings.Repeat("a padded log line stretched to about fifty chars\n", 900) // ~44 KB
	t.Setenv("GH_RUN_LIST_JSON", `[`+
		`{"databaseId":42,"workflowName":"ci","status":"completed","url":"https://x/42"},`+
		`{"databaseId":43,"workflowName":"cc-notes","status":"completed","url":"https://x/43"}]`)
	t.Setenv("GH_RUN_VIEW_JSON_42", fakeRunViewFailure)
	t.Setenv("GH_RUN_VIEW_JSON_43", `{"workflowName":"cc-notes","conclusion":"failure","startedAt":"2026-07-08T18:00:00Z","updatedAt":"2026-07-08T18:00:05Z","url":"https://x/43","jobs":[{"name":"notes","conclusion":"failure","steps":[{"name":"sync","conclusion":"failure"}]}]}`)
	t.Setenv("GH_LOG_FAILED_42", bigLog)
	t.Setenv("GH_LOG_FAILED_43", bigLog)
	shipCIPollInterval = 0

	// --budget 1 with two red runs floors the per-run share to 1 (not 0 = uncapped).
	out, _, err := runShipCmdFull(t, "-m", "fix: frobnicate", "--budget", "1")
	if err == nil {
		t.Fatal("expected error on CI failure, got nil")
	}
	if !strings.Contains(out, "tokens omitted") {
		t.Errorf("expected both excerpts capped (tokens omitted footer)\ngot tail:\n%s", out[max(0, len(out)-400):])
	}
	if strings.Contains(out, bigLog[:len(bigLog)-1]) {
		t.Errorf("full log leaked past the floored budget")
	}
}

func TestShipCIEmptyConclusionIsIndeterminate(t *testing.T) {
	log := setupShip(t, ".jj", true)
	t.Setenv("GH_RUN_LIST_JSON", fakeRunListJSON)
	t.Setenv("GH_RUN_VIEW_JSON", `{"workflowName":"ci","conclusion":"","startedAt":"2026-07-08T18:00:00Z","updatedAt":"2026-07-08T18:00:05Z","url":"https://x/42","jobs":[]}`)
	shipCIPollInterval = 0

	out, _, err := runShipCmdFull(t, "-m", "fix: frobnicate")
	if err == nil {
		t.Fatal("expected error when a run has not concluded, got nil")
	}
	summary := out
	if i := strings.IndexByte(summary, '\n'); i >= 0 {
		summary = summary[:i]
	}
	if want := "· CI error"; !strings.Contains(summary, want) {
		t.Errorf("summary = %q, want it to contain %q", summary, want)
	}
	if want := "run 42 has not concluded; check: gh run view 42"; !strings.Contains(out, want) {
		t.Errorf("output missing not-concluded pointer %q\ngot:\n%s", want, out)
	}
	for _, inv := range readInvocations(t, log) {
		for _, a := range inv {
			if a == "--log-failed" {
				t.Errorf("indeterminate run must not fetch --log-failed, got %v", inv)
			}
		}
	}
}

func TestShipCIStreamingSeam(t *testing.T) {
	tests := []struct {
		name        string
		stream      bool
		wantCompact bool
		wantErrText bool
	}{
		{"tty streams to stderr with --compact", true, true, true},
		{"non-tty buffers watch output away", false, false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			log := setupShip(t, ".jj", true)
			t.Setenv("GH_RUN_LIST_JSON", fakeRunListJSON)
			t.Setenv("GH_RUN_VIEW_JSON", fakeRunViewSuccess)
			shipCIPollInterval = 0

			old := shipStreamCI
			t.Cleanup(func() { shipStreamCI = old })
			shipStreamCI = func(io.Writer) bool { return tt.stream }

			_, errStr, err := runShipCmdFull(t, "-m", "fix: frobnicate")
			if err != nil {
				t.Fatalf("ship error = %v", err)
			}
			compact := false
			for _, inv := range readInvocations(t, log) {
				if len(inv) >= 3 && inv[0] == "gh" && inv[1] == "run" && inv[2] == "watch" {
					for _, a := range inv {
						if a == "--compact" {
							compact = true
						}
					}
				}
			}
			if compact != tt.wantCompact {
				t.Errorf("watch --compact = %v, want %v", compact, tt.wantCompact)
			}
			if got := strings.Contains(errStr, "watch stream 42"); got != tt.wantErrText {
				t.Errorf("stderr carries watch stream = %v, want %v (stderr=%q)", got, tt.wantErrText, errStr)
			}
		})
	}
}

func TestCIDuration(t *testing.T) {
	start := time.Date(2026, 7, 8, 18, 0, 0, 0, time.UTC)
	tests := []struct {
		name  string
		start time.Time
		end   time.Time
		want  string
	}{
		{"normal span", start, start.Add(58 * time.Second), "58s"},
		{"zero start omits", time.Time{}, start, ""},
		{"negative span omits", start, start.Add(-time.Second), ""},
		{"equal is zero seconds", start, start, "0s"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ciDuration(tt.start, tt.end); got != tt.want {
				t.Errorf("ciDuration = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestCIGreen(t *testing.T) {
	tests := []struct {
		conclusion string
		want       bool
	}{
		{"success", true},
		{"skipped", true},
		{"neutral", true},
		{"failure", false},
		{"cancelled", false},
		{"", false},
	}
	for _, tt := range tests {
		t.Run(tt.conclusion, func(t *testing.T) {
			if got := ciGreen(tt.conclusion); got != tt.want {
				t.Errorf("ciGreen(%q) = %v, want %v", tt.conclusion, got, tt.want)
			}
		})
	}
}
