package web

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yasyf/cc-context/internal/vendor"
)

// abEntry is one command's entry in a faked `batch --json` output array; a nil
// Error marshals to JSON null (the success shape) and a nil Result to null (the
// failure shape), matching what agent-browser really prints.
type abEntry struct {
	Command []string `json:"command"`
	Error   any      `json:"error"`
	Result  any      `json:"result"`
	Success bool     `json:"success"`
}

// okBatch is the four-entry all-success array for the fixed open/wait/read/get
// title batch, with content and title threaded into the read and title results.
func okBatch(content, title string) []abEntry {
	return []abEntry{
		{Command: []string{"open", "https://x"}, Result: map[string]any{"title": title, "url": "https://x/"}, Success: true},
		{Command: []string{"wait"}, Result: map[string]any{"state": "networkidle"}, Success: true},
		{Command: []string{"read"}, Result: map[string]any{"content": content, "finalUrl": "https://x/final", "url": "https://x/"}, Success: true},
		{Command: []string{"get", "title"}, Result: map[string]any{"title": title}, Success: true},
	}
}

// batchWithFinal is okBatch with the read step's finalUrl/url overridden, for the
// SSRF guard tests that need a specific final address.
func batchWithFinal(content, finalURL string) []abEntry {
	e := okBatch(content, "T")
	e[2] = abEntry{Command: []string{"read"}, Result: map[string]any{"content": content, "finalUrl": finalURL, "url": finalURL}, Success: true}
	return e
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}

// abTiers is a minimal tiers for the agent-browser lane: it uses only the DNS
// resolver (the SSRF guard's resolvesLocal) and reports every host public, so the
// lane's tests never touch the real network. agentBrowser uses no HTTP client.
func abTiers() *tiers { return &tiers{lookupIP: publicLookupIP} }

// sessionField returns the token following --session in a stub argv-log line.
func sessionField(line string) string {
	fields := strings.Fields(line)
	for i, f := range fields {
		if f == "--session" && i+1 < len(fields) {
			return fields[i+1]
		}
	}
	return ""
}

// stubAgentBrowser installs a fake agent-browser on PATH (via a vendor.LookPath
// override) whose `batch` drains stdin, prints batchOut, and exits with exitCode,
// and whose `close` exits 0. Every invocation's argv is appended to the returned
// log file so a test can assert the session handed to `close`. exitCode lets a
// test reproduce the production behavior that batch exits non-zero whenever any
// command fails, even a tolerated one.
func stubAgentBrowser(t *testing.T, batchOut string, exitCode int) string {
	t.Helper()
	dir := t.TempDir()
	batchFile := filepath.Join(dir, "batch.json")
	if err := os.WriteFile(batchFile, []byte(batchOut), 0o600); err != nil {
		t.Fatalf("write batch fixture: %v", err)
	}
	argvLog := filepath.Join(dir, "argv.log")
	script := fmt.Sprintf(`#!/bin/sh
printf '%%s\n' "$*" >> %q
for arg in "$@"; do
  if [ "$arg" = batch ]; then
    cat > /dev/null
    cat %q
    exit %d
  fi
  if [ "$arg" = close ]; then
    exit 0
  fi
done
exit 0
`, argvLog, batchFile, exitCode)
	installStub(t, dir, script)
	return argvLog
}

// installStub writes script as an executable agent-browser in dir and points
// vendor.LookPath at it (and only it).
func installStub(t *testing.T, dir, script string) {
	t.Helper()
	stub := filepath.Join(dir, agentBrowserBin)
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil { //nolint:gosec // test-only stub must be executable
		t.Fatalf("write stub: %v", err)
	}
	prev := vendor.LookPath
	vendor.LookPath = func(name string) string {
		if name == agentBrowserBin {
			return stub
		}
		return ""
	}
	t.Cleanup(func() { vendor.LookPath = prev })
}

func TestAgentBrowserParsesBatchOutput(t *testing.T) {
	argvLog := stubAgentBrowser(t, mustJSON(t, okBatch("# Rendered\n\nreal rendered content here.", "Rendered Title")), 0)

	res, err := abTiers().agentBrowser(context.Background(), "https://example.com/app", false)
	if err != nil {
		t.Fatalf("agentBrowser: %v", err)
	}
	if res.Tier != TierAgentBrowser {
		t.Errorf("Tier = %q, want %q", res.Tier, TierAgentBrowser)
	}
	if res.Markdown != "# Rendered\n\nreal rendered content here." {
		t.Errorf("Markdown = %q", res.Markdown)
	}
	if res.Title != "Rendered Title" {
		t.Errorf("Title = %q, want %q", res.Title, "Rendered Title")
	}
	if res.FinalURL != "https://x/final" {
		t.Errorf("FinalURL = %q, want the read result's finalUrl", res.FinalURL)
	}

	// Every invocation — batch and the deferred close — must carry the same
	// per-process session, isolating this from the user's interactive session.
	session := fmt.Sprintf("ccx-web-%d", os.Getpid())
	log := readStubLog(t, argvLog)
	if !strings.Contains(log, "batch") || !strings.Contains(log, "close") {
		t.Fatalf("stub argv log missing batch and/or close:\n%s", log)
	}
	for _, line := range strings.Split(strings.TrimSpace(log), "\n") {
		if !strings.Contains(line, session) {
			t.Errorf("invocation not scoped to session %q: %q", session, line)
		}
	}
}

func TestAgentBrowserToleratesWaitFailure(t *testing.T) {
	entries := okBatch("# Rendered\n\nreal content.", "T")
	entries[1] = abEntry{Command: []string{"wait"}, Error: "Operation timed out.", Result: nil, Success: false}
	// batch exits 1 because the wait step failed, yet the read still succeeded.
	stubAgentBrowser(t, mustJSON(t, entries), 1)

	res, err := abTiers().agentBrowser(context.Background(), "https://example.com/app", false)
	if err != nil {
		t.Fatalf("agentBrowser must tolerate a wait failure: %v", err)
	}
	if res.Markdown != "# Rendered\n\nreal content." {
		t.Errorf("Markdown = %q", res.Markdown)
	}
}

func TestAgentBrowserOpenFailureErrors(t *testing.T) {
	entries := okBatch("# This site can't be reached\n\nERR_UNSAFE_PORT", "")
	entries[0] = abEntry{Command: []string{"open"}, Error: "Navigation failed: net::ERR_UNSAFE_PORT", Result: nil, Success: false}
	// A failed open still lets read return the browser error page; the open gate
	// must reject it rather than serve that page.
	stubAgentBrowser(t, mustJSON(t, entries), 1)

	_, err := abTiers().agentBrowser(context.Background(), "https://example.com/app", false)
	if err == nil {
		t.Fatal("agentBrowser: want an error when open fails, got nil")
	}
	if !strings.Contains(err.Error(), "open failed") {
		t.Errorf("err = %v, want an open-failed error", err)
	}
}

func TestAgentBrowserEmptyReadErrors(t *testing.T) {
	stubAgentBrowser(t, mustJSON(t, okBatch("   \n\t ", "T")), 0)

	_, err := abTiers().agentBrowser(context.Background(), "https://example.com/app", false)
	if err == nil || !strings.Contains(err.Error(), "empty content") {
		t.Fatalf("err = %v, want an empty-content error", err)
	}
}

func TestAgentBrowserChallengeNotServed(t *testing.T) {
	// A rendered interstitial: the title marker trips challengeSignature, so the
	// terminal lane returns a plain error rather than serve the challenge.
	stubAgentBrowser(t, mustJSON(t, okBatch("Checking your browser before accessing the site.", "Just a moment...")), 0)

	_, err := abTiers().agentBrowser(context.Background(), "https://example.com/app", false)
	if err == nil || !strings.Contains(err.Error(), "challenge") {
		t.Fatalf("err = %v, want a challenge error", err)
	}
}

func TestAgentBrowserTimeoutKillsGroup(t *testing.T) {
	dir := t.TempDir()
	installStub(t, dir, `#!/bin/sh
for arg in "$@"; do
  if [ "$arg" = batch ]; then
    sleep 30
    exit 0
  fi
done
exit 0
`)

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := abTiers().agentBrowser(ctx, "https://example.com/app", false)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("agentBrowser: want a timeout error, got nil")
	}
	// The 300ms deadline plus reaping must return well before the stub's 30s
	// sleep would finish — proof the process group was killed.
	if elapsed > 10*time.Second {
		t.Errorf("agentBrowser took %v; the deadline kill did not reach the batch process", elapsed)
	}
}

func TestAgentBrowserRefusesLocalRedirect(t *testing.T) {
	// A public target whose rendered final URL is a loopback address: the SSRF
	// guard must refuse it rather than cache local content under the public URL.
	stubAgentBrowser(t, mustJSON(t, batchWithFinal("# Admin\n\nprivate internal content.", "http://127.0.0.1:8080/admin")), 0)

	_, err := abTiers().agentBrowser(context.Background(), "https://example.com/app", false)
	if err == nil || !strings.Contains(err.Error(), "local address") {
		t.Fatalf("err = %v, want a local-redirect refusal", err)
	}
}

func TestAgentBrowserLocalTargetAllowed(t *testing.T) {
	// A local original target (localhost dev SPA) is a designed use, so a local
	// final URL is served rather than refused.
	stubAgentBrowser(t, mustJSON(t, batchWithFinal("# Dev\n\n"+strings.Repeat("local dev content. ", 10), "http://localhost:1234/app")), 0)

	res, err := abTiers().agentBrowser(context.Background(), "http://localhost:1234/app", true)
	if err != nil {
		t.Fatalf("local target must serve: %v", err)
	}
	if res.Tier != TierAgentBrowser {
		t.Errorf("Tier = %q, want %q", res.Tier, TierAgentBrowser)
	}
}

func TestAgentBrowserNoFinalURLFailsClosed(t *testing.T) {
	// For a public target, a missing or hostless final URL fails closed rather
	// than skip the SSRF check — the real CLI always emits an absolute URL. A
	// scheme-less address parses to an empty host, so it takes the same path.
	tests := []struct {
		name  string
		final string
	}{
		{"empty final url", ""},
		{"scheme-less loopback is hostless", "127.0.0.1/x"},
		{"scheme-less private is hostless", "192.168.0.5/admin"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stubAgentBrowser(t, mustJSON(t, batchWithFinal("# X\n\nsome rendered content.", tt.final)), 0)
			_, err := abTiers().agentBrowser(context.Background(), "https://example.com/app", false)
			if err == nil || !strings.Contains(err.Error(), "no verifiable final URL") {
				t.Fatalf("err = %v, want a fail-closed no-verifiable-final-URL error", err)
			}
		})
	}
}

func TestAgentBrowserLocalTargetLenientFinal(t *testing.T) {
	// A local dev SPA is allowed to be sloppy about the final URL: an empty one
	// falls back to the target URL and serves.
	stubAgentBrowser(t, mustJSON(t, batchWithFinal("# Dev\n\n"+strings.Repeat("local content. ", 10), "")), 0)

	res, err := abTiers().agentBrowser(context.Background(), "http://localhost:5173/app", true)
	if err != nil {
		t.Fatalf("local target with an empty final URL must serve: %v", err)
	}
	if res.FinalURL != "http://localhost:5173/app" {
		t.Errorf("FinalURL = %q, want the target-URL fallback", res.FinalURL)
	}
}

func TestAgentBrowserSessionUnique(t *testing.T) {
	argvLog := stubAgentBrowser(t, mustJSON(t, okBatch("# Rendered\n\nreal rendered content.", "T")), 0)
	ts := abTiers()

	if _, err := ts.agentBrowser(context.Background(), "https://example.com/a", false); err != nil {
		t.Fatalf("first render: %v", err)
	}
	if _, err := ts.agentBrowser(context.Background(), "https://example.com/b", false); err != nil {
		t.Fatalf("second render: %v", err)
	}

	var sessions []string
	for _, line := range strings.Split(strings.TrimSpace(readStubLog(t, argvLog)), "\n") {
		if strings.Contains(line, "batch") {
			sessions = append(sessions, sessionField(line))
		}
	}
	if len(sessions) != 2 {
		t.Fatalf("want 2 batch invocations, got %d", len(sessions))
	}
	if sessions[0] == sessions[1] || sessions[0] == "" {
		t.Errorf("renders shared session %q; want two distinct non-empty sessions", sessions[0])
	}
}

func readStubLog(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path) //nolint:gosec // path is a test tempdir file
	if err != nil {
		t.Fatalf("read stub argv log %q: %v", path, err)
	}
	return string(b)
}
