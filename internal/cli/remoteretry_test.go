package cli

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/yasyf/cc-context/internal/render"
	"github.com/yasyf/cc-context/internal/vcstest"
)

func TestTransientRemoteRetriesOnlyARefusalTheRemoteMayNotRepeat(t *testing.T) {
	for _, tt := range []struct {
		name, failure string
		want          bool
	}{
		{"a 502 over smart HTTP", "error: RPC failed; HTTP 502 curl 22 The requested URL returned error: 502\nfatal: the remote end hung up unexpectedly", true},
		{"a 429", "fatal: unable to access 'https://github.com/o/r/': The requested URL returned error: 429", true},
		{"a reset connection", "fatal: unable to access 'https://github.com/o/r/': Recv failure: Connection reset by peer", true},
		{"an early EOF", "fetch-pack: unexpected disconnect while reading sideband packet\nfatal: early EOF", true},
		{"a network timeout", "ssh: connect to host github.com port 22: Operation timed out", true},
		{"gh answering 503", "HTTP 503: Service Unavailable (https://api.github.com/graphql)", true},
		{"an overloaded server", "remote: the server is overloaded, try again later", true},
		{"a non-fast-forward rejection", " ! [rejected]        main -> main (non-fast-forward)\nerror: failed to push some refs", false},
		{"a stale lease", " ! [rejected]        main -> main (stale info)", false},
		{"a declined hook", " ! [remote rejected] main -> main (pre-receive hook declined)\nremote: upstream overloaded", false},
		{"a 400 beside a hung-up remote", "error: RPC failed; HTTP 400 curl 22 The requested URL returned error: 400\nsend-pack: unexpected disconnect while reading sideband packet\nfatal: the remote end hung up unexpectedly", false},
		{"an authentication failure", "remote: Invalid username or password.\nfatal: Authentication failed for 'https://github.com/o/r/'", false},
		{"gh answering 404", "HTTP 404: Not Found (https://api.github.com/repos/o/r/pulls/9)", false},
		{"ccx's own guard expiry", "git did not finish within 10m0s and was killed; run it by hand to see what it waits on: signal: killed", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := transientRemote(tt.failure); got != tt.want {
				t.Errorf("transientRemote(%q) = %v, want %v", tt.failure, got, tt.want)
			}
		})
	}
}

func TestRunRemoteGitRetriesATransientFailureAndNamesItsTriesWhenItKeepsFailing(t *testing.T) {
	remoteRetryWaits = []time.Duration{time.Millisecond, time.Millisecond, time.Millisecond}
	t.Cleanup(func() { remoteRetryWaits = []time.Duration{2 * time.Second, 5 * time.Second, 15 * time.Second} })
	for _, tt := range []struct {
		name, stderr     string
		fails, wantCalls int
		wantErr          string
	}{
		{"a transient failure then success retries and succeeds", "error: RPC failed; HTTP 502 curl 22 The requested URL returned error: 502", 1, 2, ""},
		{"a rejected push is never retried", " ! [rejected]        main -> main (non-fast-forward)", 1, 1, "main -> main (non-fast-forward)"},
		{"a 403 is never retried", "fatal: unable to access 'https://github.com/o/r/': The requested URL returned error: 403", 1, 1, "returned error: 403"},
		{"an exhausted retry names its tries and the last error", "fatal: unable to access 'https://github.com/o/r/': Connection timed out", 9, 4, "Connection timed out (after 4 tries)"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := vcstest.Repo(t)
			bin := t.TempDir()
			calls := filepath.Join(t.TempDir(), "calls")
			writeExecutable(t, filepath.Join(bin, "git"), `#!/bin/sh
read n < "$CCX_TEST_CALLS" 2>/dev/null || n=0
n=$((n + 1))
echo "$n" > "$CCX_TEST_CALLS"
if [ "$n" -le "$CCX_TEST_FAILS" ]; then printf '%s\n' "$CCX_TEST_STDERR" >&2; exit 128; fi
printf 'pushed\n'
`)
			for key, value := range map[string]string{
				"CCX_GIT":         filepath.Join(bin, "git"),
				"CCX_TEST_CALLS":  calls,
				"CCX_TEST_FAILS":  strconv.Itoa(tt.fails),
				"CCX_TEST_STDERR": tt.stderr,
			} {
				f.Setenv(key, value)
			}
			f.PrependPATH(bin)
			out, err := runRemoteGit(f.Context(), render.Dir(f.Dir), []string{"push", "origin", "main"})
			ran, readErr := os.ReadFile(calls)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if got := strings.TrimSpace(string(ran)); got != strconv.Itoa(tt.wantCalls) {
				t.Fatalf("git ran %s times, want %d (err = %v)", got, tt.wantCalls, err)
			}
			if tt.wantErr == "" {
				if err != nil || out != "pushed\n" {
					t.Fatalf("runRemoteGit = %q, %v; want the retried push to succeed", out, err)
				}
				return
			}
			if err == nil || !strings.HasSuffix(err.Error(), tt.wantErr) {
				t.Fatalf("err = %v, want it to end with %q", err, tt.wantErr)
			}
		})
	}
}
