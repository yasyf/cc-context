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

	"github.com/yasyf/cc-context/internal/render"
	"github.com/yasyf/cc-context/internal/vcstest"
)

// gtGoldenAtExitZero pairs a recorded diagnostic with exit 0 — the run
// gtZeroFatal exists to judge, and the one pairing the corpus cannot supply:
// every gt 1.8.6 run that led stderr with ERROR: also exited nonzero, so no
// recording holds both halves at once. The bytes stay gt's; only the status is
// the test's own axis.
func gtGoldenAtExitZero(t *testing.T, name string) gtGolden {
	t.Helper()
	g := loadGTGolden(t, name)
	g.exit = 0
	return g
}

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

	r, err := gtRun(context.Background(), render.Ambient, []string{"sync", "--no-interactive"}, gtZeroSurfaces, io.Discard)
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
	if _, err := gtRun(context.Background(), render.Ambient, want, gtZeroFatal, io.Discard); err != nil {
		t.Fatalf("gtRun: %v", err)
	}
	got := gtRunReadArgv(t, log)
	if !slices.Equal(got, want) {
		t.Errorf("argv = %q, want %q — the runner must inject no flag of its own", got, want)
	}
}

// gtGoldenIndentSeverity indents every severity-led line of payload, which is
// how a test reaches a line gt never writes: gt's splog puts the prefix at
// column zero, so no recording holds an indented one, and the gate's column
// rule needs the negative case.
func gtGoldenIndentSeverity(payload string) string {
	lines := strings.Split(payload, "\n")
	for i, line := range lines {
		if strings.HasPrefix(line, gtErrorPrefix) || strings.HasPrefix(line, gtWarningPrefix) {
			lines[i] = "  " + line
		}
	}
	return strings.Join(lines, "\n")
}

// TestGTRunDiagnosticsGatesOnSeverity pins both halves of the echo's contract:
// a severity-led stderr goes out whole — continuation lines included, since gt's
// remediation is one — and a stderr with no severity line goes out not at all.
// Every row reads its bytes out of the corpus, so a re-recording carries them:
// NUX tips are unprefixed stderr exactly as gt's remediation is, which is why
// the gate — not the shape of any line — is what keeps an ordinary run quiet.
// The two rows the corpus cannot state directly — an indented prefix, a stderr
// missing its final newline — reshape recorded bytes rather than invent them.
func TestGTRunDiagnosticsGatesOnSeverity(t *testing.T) {
	t.Parallel()
	tips := loadGTGolden(t, "sync-tips-exit0")
	declined := loadGTGolden(t, "sync-decline-exit0")
	warned := loadGTGolden(t, "sync-tips-and-warning-exit0")
	errored := loadGTGolden(t, "sync-repo-404")
	tests := []struct {
		name     string
		result   gtResult
		want     string
		wantErrs bool
	}{
		{
			name:   "a severity-led stderr goes out whole, remediation included",
			result: declined.result(),
			want:   declined.stderr,
		},
		{
			name:   "tips alone stay silent",
			result: tips.result(),
			want:   "",
		},
		{
			name:   "a warning among tips still carries the whole block",
			result: warned.result(),
			want:   warned.stderr,
		},
		{
			name:     "an error among tips still carries the whole block",
			result:   errored.result(),
			want:     errored.stderr,
			wantErrs: true,
		},
		{
			name:   "an indented severity word does not lead its line",
			result: gtResult{Output: gtGoldenIndentSeverity(declined.stderr), Stderr: gtGoldenIndentSeverity(declined.stderr)},
			want:   "",
		},
		{
			name:   "a missing trailing newline is supplied",
			result: gtResult{Output: strings.TrimSuffix(declined.stderr, "\n"), Stderr: strings.TrimSuffix(declined.stderr, "\n")},
			want:   declined.stderr,
		},
		{
			name:     "a streamed result reports nothing twice",
			result:   gtResult{Output: errored.result().Output, Stderr: errored.stderr, streamed: true},
			want:     "",
			wantErrs: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
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
	r, err := gtRun(context.Background(), render.Ambient, []string{"sync"}, gtZeroSurfaces, &errW)
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
	want := loadGTGolden(t, "submit-unauth").stderr
	gtRunFake(t, gtRunScript)
	t.Setenv("GTRUN_STDERR", strings.TrimSuffix(want, "\n"))

	var errW bytes.Buffer
	r, err := gtRun(context.Background(), render.Ambient, []string{"sync"}, gtZeroSurfaces, &errW)
	if err != nil {
		t.Fatalf("gtRun: %v", err)
	}
	if errW.Len() != 0 {
		t.Errorf("errW = %q, want empty — a buffered run streams nothing", errW.String())
	}
	if got := r.Diagnostics(); got != want {
		t.Errorf("Diagnostics() = %q, want %q", got, want)
	}
	if r.Stderr != want {
		t.Errorf("Stderr = %q, want %q — the buffered arm keeps the streams apart", r.Stderr, want)
	}
}

// TestGTRunZeroPolicyDecidesExitZero pins the exit-status matrix over recorded
// runs: which combination of exit code, severity line, and policy is a failure,
// and how gtError words the one it is. The two exit-0-with-an-ERROR rows are the
// pairing no recording holds — see gtGoldenAtExitZero.
func TestGTRunZeroPolicyDecidesExitZero(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		golden     gtGolden
		policy     gtZeroPolicy
		wantErr    bool
		wantStatus string
	}{
		{
			name:   "exit 0 with no report succeeds under gtZeroFatal",
			golden: loadGTGolden(t, "sync-quiet-exit0"),
			policy: gtZeroFatal,
		},
		{
			name:   "exit 0 with no report succeeds under gtZeroSurfaces",
			golden: loadGTGolden(t, "sync-quiet-exit0"),
			policy: gtZeroSurfaces,
		},
		{
			name:   "exit 0 with an ERROR is gt's own oracle under gtZeroSurfaces",
			golden: gtGoldenAtExitZero(t, "sync-auth-invalid"),
			policy: gtZeroSurfaces,
		},
		{
			name:       "exit 0 with an ERROR is fatal under gtZeroFatal",
			golden:     gtGoldenAtExitZero(t, "submit-unauth"),
			policy:     gtZeroFatal,
			wantErr:    true,
			wantStatus: "exit 0 but reported an error",
		},
		{
			name:   "a WARNING never turns exit 0 fatal",
			golden: loadGTGolden(t, "sync-decline-exit0"),
			policy: gtZeroFatal,
		},
		{
			name:       "a nonzero exit fails even where exit 0 would be surfaced",
			golden:     loadGTGolden(t, "restack-conflict"),
			policy:     gtZeroSurfaces,
			wantErr:    true,
			wantStatus: "exit 1",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			verb := tt.golden.argv[0]
			r := tt.golden.result()

			err := r.verdict(verb, tt.policy)
			if !tt.wantErr {
				if err != nil {
					t.Fatalf("verdict() = %v, want nil", err)
				}
				return
			}
			var ge *gtError
			if !errors.As(err, &ge) {
				t.Fatalf("verdict() = %v, want a *gtError", err)
			}
			if ge.Code != tt.golden.exit {
				t.Errorf("Code = %d, want %d", ge.Code, tt.golden.exit)
			}
			if ge.Verb != verb {
				t.Errorf("Verb = %q, want %q", ge.Verb, verb)
			}
			if want := "gt " + verb + ": " + tt.wantStatus + ": " + strings.TrimSpace(r.Output); err.Error() != want {
				t.Errorf("Error() = %q, want %q", err.Error(), want)
			}
			if ge.Output != r.Output {
				t.Errorf("Output = %q, want the run's whole output %q", ge.Output, r.Output)
			}
		})
	}
}

func TestGTRunReportsAGtThatCannotRun(t *testing.T) {
	t.Setenv("PATH", t.TempDir())

	r, err := gtRun(context.Background(), render.Ambient, []string{"state"}, gtZeroFatal, io.Discard)
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

func assertGTStateTrunk(t *testing.T, payload string) {
	t.Helper()
	var state gtState
	if err := json.Unmarshal([]byte(payload), &state); err != nil {
		t.Fatalf("unmarshal payload %q: %v", payload, err)
	}
	if !state["main"].Trunk {
		t.Errorf("state = %+v, want main marked trunk", state)
	}
}

// TestGTRunCaptureKeepsThePayloadParseable takes the payload from a real gt in a
// real colocated repository — gt state is the one verb ccx parses, and gt is the
// only oracle for its shape — then replays it beside a recorded diagnostic,
// which is the pairing gtCapture exists for: a stderr line interleaved into that
// JSON would break the unmarshal. gt writes nothing to stderr on a settled
// repository, so the second half is the only place the split is observable.
func TestGTRunCaptureKeepsThePayloadParseable(t *testing.T) {
	want := loadGTGolden(t, "sync-decline-exit0").stderr
	f := vcstest.Repo(t, vcstest.GT())

	payload, r, err := gtCapture(context.Background(), render.Dir(f.Dir), []string{"state"}, gtZeroFatal)
	if err != nil {
		t.Fatalf("gtCapture: %v", err)
	}
	assertGTStateTrunk(t, payload)
	if r.Code != 0 {
		t.Errorf("Code = %d, want 0", r.Code)
	}

	gtRunFake(t, gtRunScript)
	t.Setenv("GTRUN_STDOUT", strings.TrimSuffix(payload, "\n"))
	t.Setenv("GTRUN_STDERR", strings.TrimSuffix(want, "\n"))

	replayed, r, err := gtCapture(context.Background(), render.Dir(f.Dir), []string{"state"}, gtZeroFatal)
	if err != nil {
		t.Fatalf("gtCapture replay: %v", err)
	}
	assertGTStateTrunk(t, replayed)
	if got := r.Diagnostics(); got != want {
		t.Errorf("Diagnostics() = %q, want %q — Output must carry the stream the payload does not", got, want)
	}
}

func TestGTRunCaptureAppliesItsPolicy(t *testing.T) {
	gtRunFake(t, gtRunScript)
	t.Setenv("GTRUN_STDERR", strings.TrimSuffix(gtGoldenAtExitZero(t, "submit-unauth").stderr, "\n"))

	payload, _, err := gtCapture(context.Background(), render.Ambient, []string{"state"}, gtZeroFatal)
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
	t.Parallel()
	stdout := loadGTGolden(t, "sync-quiet-exit0").stdout
	stderr := loadGTGolden(t, "submit-unauth").stderr
	bare := strings.TrimSuffix(stdout, "\n")
	tests := []struct {
		name           string
		stdout, stderr string
		want           string
	}{
		{"newline-terminated stdout joins as is", stdout, stderr, stdout + stderr},
		{"bare stdout gains the separator", bare, stderr, bare + "\n" + stderr},
		{"empty stderr adds nothing", bare, "", bare},
		{"empty stdout adds nothing", "", stderr, stderr},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := gtJoinStreams(tt.stdout, tt.stderr); got != tt.want {
				t.Errorf("gtJoinStreams(%q, %q) = %q, want %q", tt.stdout, tt.stderr, got, tt.want)
			}
		})
	}
}

func TestGTRunAdviceKeepsGtsCauseReachable(t *testing.T) {
	t.Parallel()
	r := loadGTGolden(t, "submit-unauth").result()
	cause := r.verdict("submit", gtZeroFatal)
	if cause == nil {
		t.Fatal("verdict() = nil, want the recorded refusal to fail")
	}
	err := error(&gtAdvice{advice: "ship: graphite auth required — run gt auth", cause: cause})

	if got := err.Error(); got != "ship: graphite auth required — run gt auth" {
		t.Errorf("Error() = %q, want the advice alone", got)
	}
	var ge *gtError
	if !errors.As(err, &ge) {
		t.Fatalf("errors.As reached no *gtError through %v", err)
	}
	if ge.Output != r.Output {
		t.Errorf("cause Output = %q, want %q", ge.Output, r.Output)
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
	diagnostic := strings.TrimSuffix(gtGoldenAtExitZero(t, "submit-unauth").stderr, "\n")
	gtRunFake(t, `printf 'index=%s\n' "$GIT_INDEX_FILE"
printf '%s\n' "$GTRUN_STDERR" >&2
exit 0
`)
	t.Setenv("GTRUN_STDERR", diagnostic)

	var errW bytes.Buffer
	r, err := gtRun(context.Background(), render.Ambient, []string{"create", "feature"}, gtZeroFatal, &errW, "GIT_INDEX_FILE=/tmp/ccx-oracle-index")

	if !strings.Contains(r.Output, "index=/tmp/ccx-oracle-index") {
		t.Fatalf("Output = %q, want gt to have seen GIT_INDEX_FILE", r.Output)
	}
	if !strings.Contains(r.Output, diagnostic) {
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
