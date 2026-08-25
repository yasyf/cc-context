package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os/exec"
	"strconv"
	"strings"

	"github.com/yasyf/cc-context/internal/render"
)

const (
	// gtErrorPrefix and gtWarningPrefix lead every diagnostic line gt 1.8.6
	// writes, and are its whole severity vocabulary — measured by running the
	// binary across two dozen subcommands, where no other ^[A-Z]+: prefix ever
	// appeared. Both ride stderr, at exit 0 as readily as at exit 1. The prefix
	// is matched rather than any one message so a reworded diagnostic still
	// surfaces; a diagnostic's continuation lines carry no prefix of their own,
	// so a match reports the diagnostic and drops its elaboration.
	gtErrorPrefix   = "ERROR: "
	gtWarningPrefix = "WARNING: "
)

// gtZeroPolicy states what an exit-0 run that printed an ERROR: means for one gt
// verb. Every runner takes it as a required parameter, declared beside the argv
// it governs, because gt exits 0 while reporting work it did not do — measured:
// `gt restack` prints "Did not restack branch feat1 because it is checked out in
// worktree …" and exits 0 — so whether gt's own words are the last word is a
// per-verb judgment. A default would let a call site silently inherit "trust
// exit 0", which is the mistake this type exists to make unrepresentable.
type gtZeroPolicy int

const (
	// gtZeroSurfaces keeps exit 0 a success and leaves the ERROR: lines to
	// Diagnostics. It is for a verb ccx re-measures itself: restack's verdict
	// re-reads the stack's ancestry against the remote-tracking trunk, so gt's
	// diagnostic explains a report ccx already made rather than deciding it.
	gtZeroSurfaces gtZeroPolicy = iota
	// gtZeroFatal turns an exit-0 ERROR: into a failure. It is for a verb with
	// no second oracle — a submit, create, modify, or track whose only evidence
	// that it did nothing is the sentence gt printed.
	gtZeroFatal
)

// gtResult is one finished gt invocation.
type gtResult struct {
	// Output is everything gt printed, both streams together. Emission order
	// survives on one arm only: a streamed run hands os/exec one writer for
	// stdout and stderr alike, so the child shares a single fd and no second
	// goroutine races. The buffered arms keep the two apart — gtCapture because
	// a diagnostic interleaved into gt state's JSON would break the parse, gtRun
	// because Stderr has to be separable — and join them stdout-then-stderr,
	// trading an ordering no reader of Output needs: they match substrings and
	// scan whole lines.
	//
	// Both views exist because the two readers need opposite things. A
	// classifier needs Output: gt splits one report across the streams — a
	// restack conflict banner on stdout, the ERROR: explaining a trunk it could
	// not pull on stderr — so one able to see half the evidence silently matches
	// nothing. The echo needs Stderr: stdout is already on screen, and gt's
	// diagnostics carry unprefixed continuation lines that no rule can pick out
	// of a merged buffer.
	Output string
	// Stderr is gt's stderr alone, for a caller re-emitting what the user has
	// not already seen. A streamed run leaves it empty: that arm keeps both
	// streams on one fd to preserve emission order, which is the trade that
	// makes them inseparable, and it has nothing to re-emit anyway.
	Stderr string
	// Code is gt's exit status, and is not a verdict on its own: see
	// gtZeroPolicy.
	Code int
	// streamed records that the runner already wired gt's output to the caller's
	// terminal as it was produced, which is what lets Diagnostics decline to
	// report the same lines twice.
	streamed bool
}

// Diagnostics returns gt's stderr whole when it led at least one line with a
// severity prefix, and "" otherwise. Whole, because a diagnostic's remediation
// is an unprefixed line of its own — "WARNING: <b> could not be restacked
// cleanly." is followed, past a blank line, by "Please resolve conflicts in the
// current stack with gt restack." — and gt delimits that line from the tip block
// after it with the same "\n\n", so no rule recovers one without the other.
// Gated, because gt's NUX tips are unprefixed stderr too: a run that only tipped
// has nothing to report, and reporting it would make every fresh install noisy.
// A streamed result reports nothing — the user watched the stream go by.
func (r gtResult) Diagnostics() string {
	if r.streamed || !gtSeverityLed(r.Stderr) {
		return ""
	}
	if strings.HasSuffix(r.Stderr, "\n") {
		return r.Stderr
	}
	return r.Stderr + "\n"
}

// gtSeverityLed reports whether s has a line gt led with a severity prefix.
func gtSeverityLed(s string) bool {
	for _, line := range strings.Split(s, "\n") {
		if strings.HasPrefix(line, gtErrorPrefix) || strings.HasPrefix(line, gtWarningPrefix) {
			return true
		}
	}
	return false
}

// reportedError reports whether gt printed an ERROR: line. A WARNING: never
// counts — gt warns about a deprecated alias while still doing the work.
func (r gtResult) reportedError() bool {
	for _, line := range strings.Split(r.Output, "\n") {
		if strings.HasPrefix(line, gtErrorPrefix) {
			return true
		}
	}
	return false
}

// verdict applies policy to a finished run: a nonzero exit always fails, and an
// exit-0 ERROR: fails only where the caller said gt's word is the only evidence.
func (r gtResult) verdict(verb string, policy gtZeroPolicy) error {
	if r.Code == 0 && (policy == gtZeroSurfaces || !r.reportedError()) {
		return nil
	}
	return &gtError{Verb: verb, Code: r.Code, Output: r.Output}
}

// gtError is a gt invocation that failed: a nonzero exit, or — under
// gtZeroFatal — an exit 0 whose output carried an ERROR:. It keeps the whole
// interleaved output, so the classifier that reads it and the message a caller
// prints are looking at the same evidence.
type gtError struct {
	Verb   string
	Code   int
	Output string
}

func (e *gtError) Error() string {
	status := "exit " + strconv.Itoa(e.Code)
	if e.Code == 0 {
		status = "exit 0 but reported an error"
	}
	return "gt " + e.Verb + ": " + status + ": " + strings.TrimSpace(e.Output)
}

// gtAdvice is a recovery step that replaces gt's sentence without discarding it:
// Error reports the advice alone, so a caller's exact wording survives, while
// Unwrap keeps gt's own failure reachable through errors.As and errors.Is.
type gtAdvice struct {
	advice string
	cause  error
}

func (e *gtAdvice) Error() string { return e.advice }

func (e *gtAdvice) Unwrap() error { return e.cause }

// gtRun runs one gt verb, returning everything it printed and whether gt's
// report counts as a failure under policy. When errW is a terminal the output
// also streams there as gt produces it; a caller with no reporting channel
// passes io.Discard. A non-nil error is either that verdict or render's own
// explanation of a gt that could not run or was killed.
//
// argv reaches gt exactly as given — the runner adds no flag of its own, and no
// caller may add -q or --debug. -q is the tempting one, because it does silence
// the NUX tips Diagnostics has to gate against; measured against gt 1.8.6 on an
// otherwise identical run, it also empties stdout, taking with it the
// "Did not restack branch <b> because it is checked out in worktree <w>." that
// gtSyncSkipped reads — a silent loss traded for a visible one. --debug prepends
// thousands of bytes of JSON log records to stdout, ahead of both the payload a
// parser reads and the lines a classifier matches. extraEnv extends the child's
// environment for a verb that needs an env-only variable (gt shells out to git,
// which honors GIT_INDEX_FILE); it is variadic so policy stays a required
// positional and the ordinary call spells no env.
func gtRun(ctx context.Context, dir render.Dir, argv []string, policy gtZeroPolicy, errW io.Writer, extraEnv ...string) (gtResult, error) {
	var out, errBuf bytes.Buffer
	outW, stderrW := io.Writer(&out), io.Writer(&errBuf)
	streamed := shipStreamCI(errW)
	if streamed {
		// One writer for both, so os/exec gives gt a single fd and the terminal
		// shows its lines in the order it wrote them. Nothing reads Stderr on
		// this arm, which is what makes the ordering worth more than the split.
		both := io.MultiWriter(errW, &out)
		outW, stderrW = both, both
	}
	code, err := gtStream(ctx, dir, argv, outW, stderrW, extraEnv)
	r := gtResult{Output: out.String(), Code: code, streamed: streamed}
	if !streamed {
		r.Output, r.Stderr = gtJoinStreams(out.String(), errBuf.String()), errBuf.String()
	}
	if err != nil {
		return r, err
	}
	return r, r.verdict(argv[0], policy)
}

// gtCapture is gtRun for a verb whose stdout ccx parses, and is the one runner
// that keeps gt's two streams apart: a diagnostic line interleaved into gt
// state's JSON would break the unmarshal. The payload is returned on its own, so
// a parser cannot be handed the diagnostics by accident, while Output still
// carries both streams for a classifier to read whole.
func gtCapture(ctx context.Context, dir render.Dir, argv []string, policy gtZeroPolicy) (string, gtResult, error) {
	stdout, code, stderr, err := render.RunCLIExitCode(ctx, dir, "gt", argv)
	if err != nil {
		return "", gtResult{}, err
	}
	r := gtResult{Output: gtJoinStreams(stdout, stderr), Stderr: stderr, Code: code}
	return stdout, r, r.verdict(argv[0], policy)
}

// gtStream runs gt with its stdout wired to outW and its stderr to errW, and
// reports its exit status. Passing one writer as both keeps the child on a
// single fd, so that writer receives gt's lines in the order gt wrote them. A
// non-nil error means gt never ran or was killed — render has already explained
// which — and is returned as it came, since the caller's own prefix is the
// context worth adding.
func gtStream(ctx context.Context, dir render.Dir, argv []string, outW, errW io.Writer, extraEnv []string) (int, error) {
	err := render.RunCLIStreamSplitEnv(ctx, dir, "gt", argv, outW, errW, extraEnv)
	if err == nil {
		return 0, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() >= 0 {
		return exitErr.ExitCode(), nil
	}
	return 0, err
}

// gtJoinStreams concatenates the two streams a captured run kept apart, keeping
// each one's lines whole: a stdout that does not end in a newline would
// otherwise glue its last line onto stderr's first, hiding a diagnostic behind a
// prefix that no longer starts a line.
func gtJoinStreams(stdout, stderr string) string {
	if stdout == "" || stderr == "" || strings.HasSuffix(stdout, "\n") {
		return stdout + stderr
	}
	return stdout + "\n" + stderr
}
