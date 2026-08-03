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
	// Output is everything gt printed, both streams together. gtRun hands
	// os/exec one writer for stdout and stderr alike, which makes the child
	// share a single fd across them, so its lines land in emission order with no
	// second goroutine to race; gtCapture keeps the two apart (a diagnostic
	// interleaved into gt state's JSON would break the parse) and joins them
	// stdout-then-stderr, trading an ordering it has no parser for.
	//
	// What neither offers is a stderr-only view, deliberately. gt splits one
	// report across the two streams — a restack conflict banner on stdout, the
	// ERROR: explaining a trunk it could not pull on stderr — so a classifier
	// able to see half the evidence is one that silently matches nothing.
	Output string
	// Code is gt's exit status, and is not a verdict on its own: see
	// gtZeroPolicy.
	Code int
	// streamed records that the runner already wired gt's output to the caller's
	// terminal as it was produced, which is what lets Diagnostics decline to
	// report the same lines twice.
	streamed bool
}

// Diagnostics returns the severity-led lines gt printed, in order, for a caller
// to re-emit. A streamed result reports none: the user has already seen every
// line, so re-emitting would print each one twice.
func (r gtResult) Diagnostics() []string {
	if r.streamed {
		return nil
	}
	var lines []string
	for _, line := range strings.Split(r.Output, "\n") {
		if strings.HasPrefix(line, gtErrorPrefix) || strings.HasPrefix(line, gtWarningPrefix) {
			lines = append(lines, line)
		}
	}
	return lines
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
// caller may add -q or --debug. Measured against gt 1.8.6: -q silences the
// report entirely, so a `gt restack` that declined a branch prints nothing at
// all, and --debug prepends thousands of bytes of JSON log records to stdout,
// ahead of both the payload a parser reads and the lines a classifier matches.
// extraEnv extends the child's environment for a verb that needs an env-only
// variable (gt shells out to git, which honors GIT_INDEX_FILE); it is variadic
// so policy stays a required positional and the ordinary call spells no env.
func gtRun(ctx context.Context, argv []string, policy gtZeroPolicy, errW io.Writer, extraEnv ...string) (gtResult, error) {
	var buf bytes.Buffer
	w := io.Writer(&buf)
	streamed := shipStreamCI(errW)
	if streamed {
		w = io.MultiWriter(errW, &buf)
	}
	code, err := gtStream(ctx, argv, w, extraEnv)
	r := gtResult{Output: buf.String(), Code: code, streamed: streamed}
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
func gtCapture(ctx context.Context, argv []string, policy gtZeroPolicy) (string, gtResult, error) {
	stdout, code, stderr, err := render.RunCLIExitCode(ctx, "gt", argv)
	if err != nil {
		return "", gtResult{}, err
	}
	r := gtResult{Output: gtJoinStreams(stdout, stderr), Code: code}
	return stdout, r, r.verdict(argv[0], policy)
}

// gtStream runs gt with both of the child's streams wired to w, and reports its
// exit status. os/exec gives the child one fd for both when the two fields are
// interface-equal, so w receives gt's lines in the order gt wrote them. A
// non-nil error means gt never ran or was killed — render has already explained
// which — and is returned as it came, since the caller's own prefix is the
// context worth adding.
func gtStream(ctx context.Context, argv []string, w io.Writer, extraEnv []string) (int, error) {
	err := render.RunCLIStreamEnv(ctx, "gt", argv, w, extraEnv)
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
