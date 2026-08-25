// Package render runs backend invocations and shapes their output to a budget.
package render

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"slices"
	"strings"
	"time"
	"unicode/utf8"
)

// charsPerToken is the crude chars-per-token ratio used to estimate budgets.
const charsPerToken = 4

// waitDelay bounds how long Wait blocks after the child is gone, for a
// descendant still holding the inherited output pipes open. Matches the
// in-repo precedent (internal/web, internal/codeexec).
const waitDelay = 5 * time.Second

// runTimeout is the runaway guard on a child whose caller named no deadline of
// its own, never a policy knob: a real git push, gh round trip, or gt submit over
// a slow link is legitimately slow, so the bound only has to beat "forever".
// Tests shorten it.
var runTimeout = 10 * time.Minute

// errRunTimeout is the cancellation cause the guard cancels with, so timedOut can
// tell the guard's own expiry from a caller's deadline or a Ctrl-C.
var errRunTimeout = errors.New("render: run timeout")

// Dir is the working copy a child runs in, required by every runner.
type Dir string

// Ambient is ccx's own working directory, for a child whose answer cannot
// depend on which repository it runs in.
const Ambient Dir = ""

// newCmd is every helper's child: bounded by withRunGuard, whose expiry SIGKILLs
// the child alone and leaves waitDelay to bound a descendant that outlives it on
// the inherited pipes. The process group belongs to RunCLIProbe, not here. The
// returned context is where timedOut reads the guard back off; cancel must be
// deferred.
func newCmd(ctx context.Context, dir Dir, bin string, argv, extraEnv []string) (*exec.Cmd, context.Context, context.CancelFunc) {
	runCtx, cancel := withRunGuard(ctx)
	cmd := exec.CommandContext(runCtx, bin, argv...) //nolint:gosec // bin/argv come from trusted backend translation, not user free-text
	cmd.WaitDelay = waitDelay
	cmd.Dir = string(dir)
	cmd.Env = childEnv(dir, extraEnv)
	return cmd, runCtx, cancel
}

func childEnv(dir Dir, extraEnv []string) []string {
	if dir == Ambient && len(extraEnv) == 0 {
		return nil
	}
	env := os.Environ()
	if dir != Ambient {
		env = slices.DeleteFunc(env, func(kv string) bool {
			return strings.HasPrefix(kv, "GIT_DIR=") || strings.HasPrefix(kv, "GIT_WORK_TREE=")
		})
	}
	return append(env, extraEnv...)
}

// withRunGuard bounds ctx by runTimeout only when ctx carries no deadline at all.
// An explicit caller deadline governs in either direction — a probe's tighter one
// and ship's CI watch, which legitimately outlives any default — so runTimeout is
// the bound for a caller that named none, not a ceiling over one that did.
func withRunGuard(ctx context.Context) (context.Context, context.CancelFunc) {
	if _, ok := ctx.Deadline(); ok {
		return context.WithCancel(ctx)
	}
	return context.WithTimeoutCause(ctx, runTimeout, errRunTimeout)
}

// timedOut names runTimeout as the cause when the guard — and not a deadline the
// caller brought or a cancellation — killed the child, whose death otherwise
// surfaces as an opaque signal exit. The guard alone cancels with errRunTimeout,
// so a caller's expired deadline (context.DeadlineExceeded) can never be read as
// the guard firing. Any other failure returns nil, leaving the caller's own
// wrapping in place.
func timedOut(runCtx context.Context, bin string, err error) error {
	if !errors.Is(context.Cause(runCtx), errRunTimeout) {
		return nil
	}
	return fmt.Errorf("%s did not finish within %s and was killed; run it by hand to see what it waits on: %w", bin, runTimeout, err)
}

// failure wraps a child's failure with its stderr, or names the deadline when
// runTimeout is what killed it.
func failure(runCtx context.Context, bin string, err error, stderr string) error {
	if timeout := timedOut(runCtx, bin, err); timeout != nil {
		return timeout
	}
	return fmt.Errorf("%s: %w: %s", bin, err, strings.TrimSpace(stderr))
}

// RunCLI executes bin with argv in dir, returning its stdout. A nonzero exit
// wraps the child's stderr in the returned error.
func RunCLI(ctx context.Context, dir Dir, bin string, argv []string) (string, error) {
	return RunCLIEnv(ctx, dir, bin, argv, nil)
}

// RunCLIEnv is RunCLI with extraEnv appended to the child's environment, for a
// caller that must set an env-only variable the flag surface cannot express
// (e.g. GIT_INDEX_FILE). A "KEY=value" element overrides any inherited KEY per
// exec's last-wins rule.
func RunCLIEnv(ctx context.Context, dir Dir, bin string, argv, extraEnv []string) (string, error) {
	cmd, runCtx, cancel := newCmd(ctx, dir, bin, argv, extraEnv)
	defer cancel()
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", failure(runCtx, bin, err, stderr.String())
	}
	return stdout.String(), nil
}

// RunCLIStdin is RunCLI with stdin fed from the given bytes, for a command that
// reads its payload from stdin (e.g. git hash-object --stdin).
func RunCLIStdin(ctx context.Context, dir Dir, bin string, argv []string, stdin []byte) (string, error) {
	cmd, runCtx, cancel := newCmd(ctx, dir, bin, argv, nil)
	defer cancel()
	cmd.Stdin = bytes.NewReader(stdin)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", failure(runCtx, bin, err, stderr.String())
	}
	return stdout.String(), nil
}

// RunCLIStream executes bin with argv, wiring the child's stdout and stderr to w
// as they are produced. It does not buffer output; the returned error carries the
// exit status only (any stderr already flowed to w).
func RunCLIStream(ctx context.Context, dir Dir, bin string, argv []string, w io.Writer) error {
	return RunCLIStreamEnv(ctx, dir, bin, argv, w, nil)
}

// RunCLIStreamEnv is RunCLIStream with extraEnv appended to the child's
// environment, for a streamed command that also needs an env-only variable
// (e.g. a gt verb run under GIT_INDEX_FILE).
func RunCLIStreamEnv(ctx context.Context, dir Dir, bin string, argv []string, w io.Writer, extraEnv []string) error {
	return RunCLIStreamSplitEnv(ctx, dir, bin, argv, w, w, extraEnv)
}

// RunCLIStreamSplitEnv is RunCLIStreamEnv with the child's two streams wired to
// separate writers, for a caller that must report one stream on its own. Which
// form to pass is a real choice: one writer for both makes os/exec hand the
// child a single fd, so its lines arrive in the order it wrote them but nothing
// downstream can tell which stream carried one; two writers mean two pipes and
// two copying goroutines, so the streams stay separable and their relative order
// does not survive.
func RunCLIStreamSplitEnv(ctx context.Context, dir Dir, bin string, argv []string, outW, errW io.Writer, extraEnv []string) error {
	cmd, runCtx, cancel := newCmd(ctx, dir, bin, argv, extraEnv)
	defer cancel()
	cmd.Stdout = outW
	cmd.Stderr = errW
	if err := cmd.Run(); err != nil {
		if timeout := timedOut(runCtx, bin, err); timeout != nil {
			return timeout
		}
		return fmt.Errorf("%s: %w", bin, err)
	}
	return nil
}

// RunCLIAllowExit is RunCLI but tolerates the listed nonzero exit codes: when the
// child exits with one of okCodes and writes nothing to stderr, its stdout is
// returned without error (the caller interprets an empty stdout — e.g. ast-grep
// `run` exits 1 with empty output on a clean no-match). A tolerated exit that
// still wrote to stderr is treated as a real failure and wrapped, as is any
// non-listed nonzero exit. The exit code is read from the process error via
// errors.As, never by string-matching.
func RunCLIAllowExit(ctx context.Context, dir Dir, bin string, argv []string, okCodes ...int) (string, error) {
	cmd, runCtx, cancel := newCmd(ctx, dir, bin, argv, nil)
	defer cancel()
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	if err == nil {
		return stdout.String(), nil
	}

	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && tolerated(exitErr.ExitCode(), okCodes) && stderr.Len() == 0 {
		return stdout.String(), nil
	}
	return "", failure(runCtx, bin, err, stderr.String())
}

// RunCLIExitCode executes bin with argv, returning its stdout, the child's exit
// code (via errors.As, never string-matching), and its stderr, so a caller can
// branch on a command that signals through its status, like git merge-base
// --is-ancestor, and still surface the stderr on an unexpected code. A nonzero
// exit is not an error; err is non-nil only when the child could not run, or
// when runTimeout killed it.
func RunCLIExitCode(ctx context.Context, dir Dir, bin string, argv []string) (string, int, string, error) {
	cmd, runCtx, cancel := newCmd(ctx, dir, bin, argv, nil)
	defer cancel()
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	if err == nil {
		return stdout.String(), 0, stderr.String(), nil
	}
	if timeout := timedOut(runCtx, bin, err); timeout != nil {
		return "", 0, "", timeout
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return stdout.String(), exitErr.ExitCode(), stderr.String(), nil
	}
	return "", 0, "", fmt.Errorf("%s: %w: %s", bin, err, strings.TrimSpace(stderr.String()))
}

// RunCLIProbe is RunCLIExitCode for a probe under a tight deadline: the child
// leads its own process group and cancellation SIGKILLs the group, so a
// grandchild cannot outlive the deadline holding the output pipes open. Only a
// probe earns that, because a group is not a session: a member that opens
// /dev/tty for git's or ssh's prompt is stopped by SIGTTIN while the terminal's
// keystrokes still go to ccx's group, so it hangs until the guard kills it. A
// probe is non-interactive by construction and never prompts; a push may.
// Whatever the child managed to write is returned even when it was killed, so the
// caller can still read a reason off a partial answer.
func RunCLIProbe(ctx context.Context, dir Dir, bin string, argv []string) (string, int, string, error) {
	cmd, runCtx, cancel := newCmd(ctx, dir, bin, argv, nil)
	defer cancel()
	configureProbeCommand(cmd)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	if err == nil {
		return stdout.String(), 0, stderr.String(), nil
	}
	if timeout := timedOut(runCtx, bin, err); timeout != nil {
		return stdout.String(), 0, stderr.String(), timeout
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return stdout.String(), exitErr.ExitCode(), stderr.String(), nil
	}
	return stdout.String(), 0, stderr.String(), fmt.Errorf("%s: %w: %s", bin, err, strings.TrimSpace(stderr.String()))
}

// RunCLIKeepStderr is RunCLI but also returns the child's stderr on success, for
// a command that warns on stderr while exiting 0 (git rebase --autostash warns
// that an autostash pop "resulted in conflicts" and exits 0). A nonzero exit wraps
// the child's stderr in the returned error as RunCLI does.
func RunCLIKeepStderr(ctx context.Context, dir Dir, bin string, argv []string) (stdout, stderr string, err error) {
	cmd, runCtx, cancel := newCmd(ctx, dir, bin, argv, nil)
	defer cancel()
	var outBuf, errBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	if err := cmd.Run(); err != nil {
		return "", "", failure(runCtx, bin, err, errBuf.String())
	}
	return outBuf.String(), errBuf.String(), nil
}

// tolerated reports whether code is one of the allowed exit codes.
func tolerated(code int, okCodes []int) bool {
	for _, ok := range okCodes {
		if code == ok {
			return true
		}
	}
	return false
}

// Cap trims s to budgetTokens (estimated as len/4). When it trims, it cuts at a
// line boundary and appends an explicit footer naming the omitted volume; it
// never silently truncates. A non-positive budget returns s unchanged.
func Cap(s string, budgetTokens int) string {
	kept, omitted, trimmed := capTrim(s, budgetTokens)
	if !trimmed {
		return kept
	}
	omittedLines := strings.Count(strings.Trim(omitted, "\n"), "\n") + 1
	omittedTokens := len(omitted) / charsPerToken

	return fmt.Sprintf(
		"%s\n… +%d lines, ~%d tokens omitted — re-run with a larger --budget\n",
		kept, omittedLines, omittedTokens,
	)
}

// CapContinuation serves a fixed-stride page of span for a paged web read: the
// byte window [offset*charsPerToken, (offset+budget)*charsPerToken), each bound
// snapped backward to a UTF-8 rune start so page N's end equals page N+1's start
// and consecutive pages join exactly. A window that stops short of span's end
// appends a footer naming the next --offset (offset+budget); a non-positive budget
// or a window reaching the end serves to span's end with no footer. An empty span
// serves empty; otherwise offset must be a valid page start
// (offset*charsPerToken < len(span)), which serveSpan enforces.
func CapContinuation(span string, offset, budget int) string {
	if span == "" {
		return span
	}
	startRaw := offset * charsPerToken
	start := snapRuneStart(span, startRaw)
	// Divide, don't multiply: budget can be MaxInt and budget*charsPerToken overflow.
	if budget <= 0 || budget > (len(span)-startRaw-1)/charsPerToken {
		return span[start:]
	}
	end := snapRuneStart(span, startRaw+budget*charsPerToken)
	remainder := span[end:]
	omittedLines := strings.Count(remainder, "\n")
	if !strings.HasSuffix(remainder, "\n") {
		omittedLines++ // count the unterminated final line
	}
	omittedTokens := len(remainder) / charsPerToken
	next := offset + budget

	return fmt.Sprintf(
		"%s\n… +%d lines, ~%d tokens omitted — re-run with --offset %d to continue, or a larger --budget\n",
		span[start:end], omittedLines, omittedTokens, next,
	)
}

// snapRuneStart moves i backward to the first byte of the UTF-8 rune it lands in so
// a stride boundary never splits a multi-byte rune, bounding the walk-back to
// utf8.UTFMax-1 bytes so a run of malformed continuation bytes cannot drag the cut
// arbitrarily far — beyond the bound, i is returned unchanged and invalid UTF-8 is
// split as-is. A pure function of (s, i): page N's snapped end equals page N+1's
// snapped start, so consecutive pages still join exactly. i must index into s.
func snapRuneStart(s string, i int) int {
	for j := i; j >= 0 && i-j < utf8.UTFMax; j-- {
		if utf8.RuneStart(s[j]) {
			return j
		}
	}
	return i
}

// capTrim splits s at the last line boundary within budgetTokens, returning the
// kept prefix, the omitted suffix, and whether a trim was needed. A non-positive
// budget or an s already within budget returns s whole with trimmed false.
func capTrim(s string, budgetTokens int) (kept, omitted string, trimmed bool) {
	if budgetTokens <= 0 {
		return s, "", false
	}
	limit := budgetTokens * charsPerToken
	// Guard the multiply: a math.MaxInt64 budget wraps negative, so an overflow (or
	// any budget wide enough to hold s) keeps everything uncut.
	if limit/charsPerToken != budgetTokens || len(s) <= limit {
		return s, "", false
	}
	cut := strings.LastIndexByte(s[:limit], '\n')
	if cut < 0 {
		cut = snapRuneStart(s, limit)
	}
	return s[:cut], s[cut:], true
}
