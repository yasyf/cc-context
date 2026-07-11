package web

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/yasyf/cc-context/internal/vendor"
)

// agentBrowserBin is the local browser-automation CLI the render escalation's
// last lane drives; it is resolved on PATH, so an absent binary skips the lane.
const agentBrowserBin = "agent-browser"

// agentBrowserSeq makes each render's session name unique within a process, so
// concurrent renders (an MCP server, a ccx exec fan-out) never share one browser
// tab and interleave navigation or race one another's deferred close.
var agentBrowserSeq uint64

// agentBrowser lane timeouts. agentBrowserTimeout bounds the whole batch;
// agentBrowserNavTimeoutMS is the per-navigation networkidle wait handed to the
// batch "wait" step (well under the batch timeout); agentBrowserCloseTimeout
// bounds the deferred best-effort session close.
const (
	agentBrowserTimeout      = 60 * time.Second
	agentBrowserNavTimeoutMS = 15000
	agentBrowserCloseTimeout = 5 * time.Second
)

// batchResult is one command's entry in agent-browser's `batch --json` output
// array: the echoed command, a per-command error (null on success), the
// command-specific result payload, and the success flag the parser branches on.
type batchResult struct {
	Command []string        `json:"command"`
	Error   string          `json:"error"`
	Result  json.RawMessage `json:"result"`
	Success bool            `json:"success"`
}

// agentBrowser renders targetURL by driving the local agent-browser CLI through
// one `batch --json` subprocess: open, wait for network idle, read the rendered
// markdown, and read the title. `read <url>` is a plain HTTP fetch, so the URL is
// loaded with `open` first and the bare `read` returns the live DOM. An explicit
// per-process session isolates this from the user's interactive session; the
// session is closed best-effort afterward. It is the render escalation's terminal
// lane, so a rendered bot challenge returns a plain error, never errStealthRequired.
func (t *tiers) agentBrowser(ctx context.Context, targetURL string, targetLocal bool) (FetchResult, error) {
	bin := vendor.LookPath(agentBrowserBin)
	if bin == "" {
		return FetchResult{}, fmt.Errorf("web: render escalation needs %s on PATH", agentBrowserBin)
	}

	session := fmt.Sprintf("ccx-web-%d-%d", os.Getpid(), atomic.AddUint64(&agentBrowserSeq, 1))
	// Close the session after the batch regardless of how it ends, detached from
	// the batch deadline so a timeout still releases the browser tab.
	defer closeAgentBrowserSession(context.WithoutCancel(ctx), bin, session)

	commands := [][]string{
		{"open", targetURL},
		{"wait", "--load", "networkidle", "--timeout", strconv.Itoa(agentBrowserNavTimeoutMS)},
		{"read"},
		{"get", "title"},
	}
	stdinJSON, err := json.Marshal(commands)
	if err != nil {
		return FetchResult{}, fmt.Errorf("agent-browser: marshal batch: %w", err)
	}

	batchCtx, cancel := context.WithTimeout(ctx, agentBrowserTimeout)
	defer cancel()
	out, err := runAgentBrowserBatch(batchCtx, bin, session, stdinJSON)
	if err != nil {
		return FetchResult{}, err
	}

	var results []batchResult
	if err := json.Unmarshal(out, &results); err != nil {
		return FetchResult{}, fmt.Errorf("agent-browser: decode batch output: %w", err)
	}
	if len(results) < len(commands) {
		return FetchResult{}, fmt.Errorf("agent-browser: batch returned %d of %d results", len(results), len(commands))
	}

	// open must succeed: on a navigation failure `read` still succeeds, returning
	// the browser's own error page, so the open gate is what keeps that out.
	if open := results[0]; !open.Success {
		return FetchResult{}, fmt.Errorf("agent-browser: open failed: %s", open.Error)
	}
	read := results[2]
	if !read.Success {
		return FetchResult{}, fmt.Errorf("agent-browser: read failed: %s", read.Error)
	}
	var rd struct {
		Content  string `json:"content"`
		FinalURL string `json:"finalUrl"`
		URL      string `json:"url"`
	}
	if err := json.Unmarshal(read.Result, &rd); err != nil {
		return FetchResult{}, fmt.Errorf("agent-browser: decode read result: %w", err)
	}
	if strings.TrimSpace(rd.Content) == "" {
		return FetchResult{}, errors.New("agent-browser: read returned empty content")
	}

	// The wait and get-title steps are best-effort: a networkidle timeout on a
	// beacon-chatty page, or an absent title, must not sink a good read.
	title := ""
	if gt := results[3]; gt.Success {
		var td struct {
			Title string `json:"title"`
		}
		if err := json.Unmarshal(gt.Result, &td); err == nil {
			title = td.Title
		}
	}
	if challengeSignature(challengeInput{Title: title, Body: rd.Content, Kind: cleanMarkdown}) {
		return FetchResult{}, errors.New("agent-browser: rendered page is a bot challenge")
	}

	final := rd.FinalURL
	if final == "" {
		final = rd.URL
	}
	// SSRF guard: the browser subprocess bypasses the HTTP client's
	// refuseLocalRedirect, so for a public target a page that JS-navigates or
	// redirects to a loopback/private address would cache local content under the
	// public URL. targetLocal is threaded in from renderFetch's own lane-gating
	// resolution rather than re-resolved here, so attacker DNS cannot flip the
	// target from public (at fetch) to local (at check) and slip past. A missing
	// or hostless final URL fails CLOSED — the real agent-browser CLI always emits
	// an absolute URL, so an absent one is anomalous (fail-fast). A local original
	// target (localhost dev SPA) is a designed use and keeps the lenient fallback.
	//
	// Residual, shared with the HTTP lanes' pre-follow checks: the FINAL host's own
	// resolution can still flip between the browser's fetch and this post-hoc check
	// — a rebinding window in the opposite direction. Parity with those lanes is
	// the bar; both carry the same class of window.
	if !targetLocal {
		host := hostOf(final)
		if host == "" {
			return FetchResult{}, errors.New("agent-browser: read returned no verifiable final URL")
		}
		if t.hostIsLocal(ctx, host) {
			return FetchResult{}, errors.New("agent-browser: redirected to local address")
		}
	} else if final == "" {
		final = targetURL
	}
	return FetchResult{Tier: TierAgentBrowser, FinalURL: final, Title: title, Markdown: rd.Content}, nil
}

// hostIsLocal reports whether host addresses a machine only this host can reach —
// the same predicate the fetch cascade gates on: a literal local target, or a
// name that resolves entirely to local addresses (best-effort, public on failure).
func (t *tiers) hostIsLocal(ctx context.Context, host string) bool {
	return localTarget(host) || (net.ParseIP(host) == nil && t.resolvesLocal(ctx, host))
}

// hostOf returns the hostname of rawURL, or "" when it does not parse.
func hostOf(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	return u.Hostname()
}

// runAgentBrowserBatch runs one `agent-browser --session <session> batch --json`
// subprocess, feeding it the commands as JSON on stdin and returning stdout. Its
// process hygiene mirrors the pdf/embed drivers (own group, SIGKILL the group on
// cancel, WaitDelay, buffered stdout, tail-buffered stderr). batch without --bail
// exits non-zero when ANY command fails — including a tolerated wait or get-title
// failure — yet still prints the full result array, so a non-zero exit alone is
// not fatal: the array is returned and the caller decides from per-command
// success. Only a ctx-killed run or an empty output surfaces as an error.
func runAgentBrowserBatch(ctx context.Context, bin, session string, stdinJSON []byte) ([]byte, error) {
	cmd := exec.CommandContext(ctx, bin, "--session", session, "batch", "--json") //nolint:gosec // argv is fixed: agent-browser from PATH with a per-process session
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.WaitDelay = 5 * time.Second
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("pipe agent-browser stdin: %w", err)
	}
	defer func() { _ = stdin.Close() }()
	cmd.Cancel = func() error {
		_ = stdin.Close()
		// A cancel that races Wait reaping the child sees ESRCH: the group is
		// already gone, so report it finished rather than fail a done batch.
		if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
			if errors.Is(err, syscall.ESRCH) {
				return os.ErrProcessDone
			}
			return err
		}
		return nil
	}
	var out bytes.Buffer
	stderr := &tailBuffer{}
	cmd.Stdout = &out
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("launch agent-browser: %w", err)
	}
	if _, err := stdin.Write(stdinJSON); err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return nil, driverErr(ctx, "write agent-browser batch", err, stderr)
	}
	// EOF is the batch's end-of-input signal, so close before Wait.
	if err := stdin.Close(); err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return nil, driverErr(ctx, "close agent-browser stdin", err, stderr)
	}
	if err := cmd.Wait(); err != nil {
		if ctx.Err() != nil || len(bytes.TrimSpace(out.Bytes())) == 0 {
			return nil, driverErr(ctx, "agent-browser batch", err, stderr)
		}
	}
	return out.Bytes(), nil
}

// closeAgentBrowserSession closes the agent-browser session best-effort under its
// own short deadline; a failure is logged at Debug, never surfaced, since the
// batch result is already in hand.
func closeAgentBrowserSession(ctx context.Context, bin, session string) {
	ctx, cancel := context.WithTimeout(ctx, agentBrowserCloseTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "--session", session, "close") //nolint:gosec // argv is fixed: agent-browser from PATH closing our own session
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.WaitDelay = time.Second
	if err := cmd.Run(); err != nil {
		slog.Debug("agent-browser session close failed", "session", session, "err", err)
	}
}
