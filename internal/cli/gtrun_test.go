package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

// gtRunFake installs body as a fake gt on a PATH holding nothing else, so a
// lookup can only find it. Absolute interpreter and helper paths keep working
// under that PATH.
func gtRunFake(t *testing.T, body string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake shell scripts are POSIX-only")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "gt"), []byte("#!/bin/sh\n"+body), 0o700); err != nil { //nolint:gosec // fake executable must be owner-executable
		t.Fatalf("write fake gt: %v", err)
	}
	t.Setenv("PATH", dir)
}

// gtRunScript is the fake gt the streams/policy tests drive: it records its argv
// and replays a stdout, a stderr, and an exit code named in the environment.
const gtRunScript = `if [ -n "$GTRUN_LOG" ]; then
  { for a in "$@"; do printf '%s\0' "$a"; done; } >> "$GTRUN_LOG"
fi
if [ -n "$GTRUN_STDOUT" ]; then printf '%s\n' "$GTRUN_STDOUT"; fi
if [ -n "$GTRUN_STDERR" ]; then printf '%s\n' "$GTRUN_STDERR" >&2; fi
exit ${GTRUN_EXIT:-0}
`

// gtRunInterleaveScript alternates the two streams. Every write is an external
// /bin/echo, which flushes as it exits, so the order the bytes reach the shared
// fd is the order the script names — never an artifact of a shell's own stdout
// buffering.
const gtRunInterleaveScript = `/bin/echo 'one-stdout'
/bin/echo 'two-stderr' >&2
/bin/echo 'three-stdout'
/bin/echo 'ERROR: four-stderr' >&2
/bin/echo 'five-stdout'
`

func gtRunArgvLog(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "argv")
	t.Setenv("GTRUN_LOG", path)
	return path
}

func gtRunReadArgv(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read argv log: %v", err)
	}
	return strings.Split(strings.TrimSuffix(string(data), "\x00"), "\x00")
}

// TestGTRunBufferedRunCarriesBothStreams pins what a buffered Output owes a
// classifier: every line of both streams, so a matcher cannot miss evidence the
// other stream carried. It does not owe emission order — separating stderr costs
// the single fd that ordering came from, and the streamed arm, which keeps the
// fd, is where order still holds. No Output reader depends on it: they match
// substrings and scan lines.
func TestGTRunBufferedRunCarriesBothStreams(t *testing.T) {
	gtRunFake(t, gtRunInterleaveScript)

	r, err := gtRun(context.Background(), []string{"sync", "--no-interactive"}, gtZeroSurfaces, io.Discard)
	if err != nil {
		t.Fatalf("gtRun: %v", err)
	}
	want := "one-stdout\nthree-stdout\nfive-stdout\ntwo-stderr\nERROR: four-stderr\n"
	if r.Output != want {
		t.Errorf("Output = %q, want %q", r.Output, want)
	}
	if wantErr := "two-stderr\nERROR: four-stderr\n"; r.Stderr != wantErr {
		t.Errorf("Stderr = %q, want %q", r.Stderr, wantErr)
	}
}

func TestGTRunPassesArgvThroughUntouched(t *testing.T) {
	gtRunFake(t, gtRunScript)
	log := gtRunArgvLog(t)

	want := []string{"submit", "--no-interactive", "--no-edit", "--no-ai", "--no-stack", "--publish"}
	if _, err := gtRun(context.Background(), want, gtZeroFatal, io.Discard); err != nil {
		t.Fatalf("gtRun: %v", err)
	}
	got := gtRunReadArgv(t, log)
	if !slices.Equal(got, want) {
		t.Errorf("argv = %q, want %q — the runner must inject no flag of its own", got, want)
	}
}

// TestGTRunDiagnosticsGatesOnSeverity pins both halves of the echo's contract:
// a severity-led stderr goes out whole — continuation lines included, since gt's
// remediation is one — and a stderr with no severity line goes out not at all.
// The tip rows are gt 1.8.6's own bytes: NUX tips are unprefixed stderr like the
// remediation is, so the gate, not the shape of the line, is what keeps a fresh
// install quiet.
func TestGTRunDiagnosticsGatesOnSeverity(t *testing.T) {
	tips := "\ntip: Feeling like an expert? Disable tips in gt config [tip.expert-message ●]\n\n"
	tests := []struct {
		name     string
		result   gtResult
		want     string
		wantErrs bool
	}{
		{
			name: "a severity-led stderr goes out whole, remediation included",
			result: gtResult{
				Output: "🥞 Restacking branches...\nWARNING: feat could not be restacked cleanly.\n\nPlease resolve conflicts in the current stack with gt restack.\n",
				Stderr: "WARNING: feat could not be restacked cleanly.\n\nPlease resolve conflicts in the current stack with gt restack.\n",
			},
			want: "WARNING: feat could not be restacked cleanly.\n\nPlease resolve conflicts in the current stack with gt restack.\n",
		},
		{
			name:   "tips alone stay silent",
			result: gtResult{Output: "🥞 Restacking branches...\n" + tips, Stderr: tips},
			want:   "",
		},
		{
			name:     "a warning among tips still carries the whole block",
			result:   gtResult{Output: tips + "ERROR: Could not determine the name of this repo. \n", Stderr: tips + "ERROR: Could not determine the name of this repo. \n"},
			want:     tips + "ERROR: Could not determine the name of this repo. \n",
			wantErrs: true,
		},
		{
			name:   "an indented severity word does not lead its line",
			result: gtResult{Output: "  ERROR: indented\nplain\n", Stderr: "  ERROR: indented\nplain\n"},
			want:   "",
		},
		{
			name:   "a missing trailing newline is supplied",
			result: gtResult{Output: "WARNING: no newline", Stderr: "WARNING: no newline"},
			want:   "WARNING: no newline\n",
		},
		{
			name:     "a streamed result reports nothing twice",
			result:   gtResult{Output: "ERROR: already shown\n", Stderr: "ERROR: already shown\n", streamed: true},
			want:     "",
			wantErrs: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.result.Diagnostics(); got != tt.want {
				t.Errorf("Diagnostics() = %q, want %q", got, tt.want)
			}
			if got := tt.result.reportedError(); got != tt.wantErrs {
				t.Errorf("reportedError() = %v, want %v", got, tt.wantErrs)
			}
		})
	}
}

func TestGTRunStreamsOnceToTerminal(t *testing.T) {
	gtRunFake(t, gtRunInterleaveScript)
	old := shipStreamCI
	t.Cleanup(func() { shipStreamCI = old })
	shipStreamCI = func(io.Writer) bool { return true }

	var errW bytes.Buffer
	r, err := gtRun(context.Background(), []string{"sync"}, gtZeroSurfaces, &errW)
	if err != nil {
		t.Fatalf("gtRun: %v", err)
	}
	want := "one-stdout\ntwo-stderr\nthree-stdout\nERROR: four-stderr\nfive-stdout\n"
	if errW.String() != want {
		t.Errorf("streamed output = %q, want %q", errW.String(), want)
	}
	if r.Output != want {
		t.Errorf("Output = %q, want %q — a streamed run still buffers for the classifier", r.Output, want)
	}
	if got := r.Diagnostics(); got != "" {
		t.Errorf("Diagnostics() = %q, want none — the user has already seen these lines", got)
	}
	if r.Stderr != "" {
		t.Errorf("Stderr = %q, want empty — the streamed arm trades the split for emission order", r.Stderr)
	}
}

func TestGTRunBufferedRunReportsItsDiagnostics(t *testing.T) {
	gtRunFake(t, gtRunScript)
	t.Setenv("GTRUN_STDERR", "ERROR: Could not pull trunk main")

	var errW bytes.Buffer
	r, err := gtRun(context.Background(), []string{"sync"}, gtZeroSurfaces, &errW)
	if err != nil {
		t.Fatalf("gtRun: %v", err)
	}
	if errW.Len() != 0 {
		t.Errorf("errW = %q, want empty — a buffered run streams nothing", errW.String())
	}
	want := "ERROR: Could not pull trunk main\n"
	if got := r.Diagnostics(); got != want {
		t.Errorf("Diagnostics() = %q, want %q", got, want)
	}
	if r.Stderr != want {
		t.Errorf("Stderr = %q, want %q — the buffered arm keeps the streams apart", r.Stderr, want)
	}
}

func TestGTRunZeroPolicyDecidesExitZero(t *testing.T) {
	tests := []struct {
		name     string
		stdout   string
		stderr   string
		exit     string
		policy   gtZeroPolicy
		wantErr  bool
		wantCode int
		wantMsg  string
	}{
		{
			name:   "exit 0 with no report succeeds under either policy",
			stdout: "feat2 does not need to be restacked on feat1.",
			policy: gtZeroFatal,
		},
		{
			name:   "exit 0 with an ERROR is gt's own oracle under gtZeroSurfaces",
			stdout: "Did not restack branch feat1 because it is checked out in worktree /tmp/wt.",
			stderr: "ERROR: Could not pull trunk main",
			policy: gtZeroSurfaces,
		},
		{
			name:     "exit 0 with an ERROR is fatal under gtZeroFatal",
			stderr:   "ERROR: Could not determine the name of this repo.",
			policy:   gtZeroFatal,
			wantErr:  true,
			wantCode: 0,
			wantMsg:  "gt submit: exit 0 but reported an error: ERROR: Could not determine the name of this repo.",
		},
		{
			name:   "a WARNING never turns exit 0 fatal",
			stderr: "WARNING: This command has been renamed and will be fully removed soon.",
			policy: gtZeroFatal,
		},
		{
			name:     "a nonzero exit fails even where exit 0 would be surfaced",
			stdout:   "Hit conflict restacking feat on main.",
			exit:     "1",
			policy:   gtZeroSurfaces,
			wantErr:  true,
			wantCode: 1,
			wantMsg:  "gt submit: exit 1: Hit conflict restacking feat on main.",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gtRunFake(t, gtRunScript)
			t.Setenv("GTRUN_STDOUT", tt.stdout)
			t.Setenv("GTRUN_STDERR", tt.stderr)
			t.Setenv("GTRUN_EXIT", tt.exit)

			r, err := gtRun(context.Background(), []string{"submit", "--no-interactive"}, tt.policy, io.Discard)
			if !tt.wantErr {
				if err != nil {
					t.Fatalf("gtRun: unexpected error %v", err)
				}
				return
			}
			var ge *gtError
			if !errors.As(err, &ge) {
				t.Fatalf("gtRun error = %v, want a *gtError", err)
			}
			if ge.Code != tt.wantCode {
				t.Errorf("Code = %d, want %d", ge.Code, tt.wantCode)
			}
			if ge.Verb != "submit" {
				t.Errorf("Verb = %q, want %q", ge.Verb, "submit")
			}
			if err.Error() != tt.wantMsg {
				t.Errorf("Error() = %q, want %q", err.Error(), tt.wantMsg)
			}
			if !strings.Contains(ge.Output, strings.TrimSpace(tt.stdout+"\n"+tt.stderr)) && ge.Output != r.Output {
				t.Errorf("Output = %q, want the run's whole output %q", ge.Output, r.Output)
			}
		})
	}
}

func TestGTRunReportsAGtThatCannotRun(t *testing.T) {
	t.Setenv("PATH", t.TempDir())

	r, err := gtRun(context.Background(), []string{"state"}, gtZeroFatal, io.Discard)
	if err == nil {
		t.Fatal("gtRun: want an error for a gt that is not on PATH")
	}
	var ge *gtError
	if errors.As(err, &ge) {
		t.Errorf("error = %v, want render's own explanation, not a *gtError verdict", err)
	}
	if r.Code != 0 || r.Output != "" {
		t.Errorf("result = %+v, want a zero result", r)
	}
}

func TestGTRunCaptureKeepsThePayloadParseable(t *testing.T) {
	gtRunFake(t, gtRunScript)
	t.Setenv("GTRUN_STDOUT", `{"main":{"trunk":true}}`)
	t.Setenv("GTRUN_STDERR", "WARNING: This command has been renamed and will be fully removed soon.")

	payload, r, err := gtCapture(context.Background(), []string{"state"}, gtZeroFatal)
	if err != nil {
		t.Fatalf("gtCapture: %v", err)
	}
	var state gtState
	if err := json.Unmarshal([]byte(payload), &state); err != nil {
		t.Fatalf("unmarshal payload %q: %v", payload, err)
	}
	if !state["main"].Trunk {
		t.Errorf("state = %+v, want main marked trunk", state)
	}
	want := "WARNING: This command has been renamed and will be fully removed soon.\n"
	if got := r.Diagnostics(); got != want {
		t.Errorf("Diagnostics() = %q, want %q — Output must carry the stream the payload does not", got, want)
	}
}

func TestGTRunCaptureAppliesItsPolicy(t *testing.T) {
	gtRunFake(t, gtRunScript)
	t.Setenv("GTRUN_STDERR", "ERROR: Could not determine the name of this repo.")

	payload, _, err := gtCapture(context.Background(), []string{"state"}, gtZeroFatal)
	var ge *gtError
	if !errors.As(err, &ge) {
		t.Fatalf("gtCapture error = %v, want a *gtError", err)
	}
	if payload != "" {
		t.Errorf("payload = %q, want empty", payload)
	}
	if ge.Verb != "state" || ge.Code != 0 {
		t.Errorf("gtError = %+v, want state at exit 0", ge)
	}
}

func TestGTRunJoinStreamsKeepsLinesWhole(t *testing.T) {
	tests := []struct {
		name           string
		stdout, stderr string
		want           string
	}{
		{"newline-terminated stdout joins as is", "{}\n", "ERROR: nope\n", "{}\nERROR: nope\n"},
		{"bare stdout gains the separator", "{}", "ERROR: nope\n", "{}\nERROR: nope\n"},
		{"empty stderr adds nothing", "{}", "", "{}"},
		{"empty stdout adds nothing", "", "ERROR: nope\n", "ERROR: nope\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := gtJoinStreams(tt.stdout, tt.stderr); got != tt.want {
				t.Errorf("gtJoinStreams(%q, %q) = %q, want %q", tt.stdout, tt.stderr, got, tt.want)
			}
		})
	}
}

func TestGTRunAdviceKeepsGtsCauseReachable(t *testing.T) {
	cause := &gtError{Verb: "submit", Code: 0, Output: "ERROR: Your Graphite auth token is invalid/expired\n"}
	err := error(&gtAdvice{advice: "ship: graphite auth required — run gt auth", cause: cause})

	if got := err.Error(); got != "ship: graphite auth required — run gt auth" {
		t.Errorf("Error() = %q, want the advice alone", got)
	}
	var ge *gtError
	if !errors.As(err, &ge) {
		t.Fatalf("errors.As reached no *gtError through %v", err)
	}
	if ge.Output != cause.Output {
		t.Errorf("cause Output = %q, want %q", ge.Output, cause.Output)
	}
	if !errors.Is(err, cause) {
		t.Error("errors.Is did not reach the cause")
	}
}

// TestGTRunCarriesExtraEnvWithoutLosingAStream is the hunk-scoped commit's shape:
// gt shells out to git, so a verb run under a throwaway GIT_INDEX_FILE has to
// reach the same runner every other verb does. A helper that took the env but
// returned stdout alone would drop the sentence a lying exit 0 is judged on.
func TestGTRunCarriesExtraEnvWithoutLosingAStream(t *testing.T) {
	gtRunFake(t, `printf 'index=%s\n' "$GIT_INDEX_FILE"
printf 'ERROR: Could not create feature.\n' >&2
exit 0
`)
	var errW bytes.Buffer
	r, err := gtRun(context.Background(), []string{"create", "feature"}, gtZeroFatal, &errW, "GIT_INDEX_FILE=/tmp/ccx-oracle-index")

	if !strings.Contains(r.Output, "index=/tmp/ccx-oracle-index") {
		t.Fatalf("Output = %q, want gt to have seen GIT_INDEX_FILE", r.Output)
	}
	if !strings.Contains(r.Output, gtErrorPrefix+"Could not create feature.") {
		t.Fatalf("Output = %q, want the stderr diagnostic too", r.Output)
	}
	var ge *gtError
	if !errors.As(err, &ge) {
		t.Fatalf("gtRun error = %v, want a *gtError for an exit-0 ERROR:", err)
	}
	if ge.Code != 0 {
		t.Errorf("Code = %d, want 0", ge.Code)
	}
}
