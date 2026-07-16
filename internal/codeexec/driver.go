package codeexec

import (
	"bytes"
	"context"
	"crypto/sha256"
	_ "embed"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/yasyf/cc-context/internal/cache"
	"github.com/yasyf/cc-context/internal/lookpath"
)

//go:embed driver.py
var driverSource []byte

// montyRequirement pins the sandbox runtime uv provisions for the driver
// (pydantic-monty embeds the monty interpreter). Bump only in dedicated PRs
// that re-run the codeexec suite and the episode replays — never via a bulk
// dependency sweep.
const montyRequirement = "pydantic-monty==0.0.18"

// driverPython pins the CPython ABI so the per-ABI pydantic-monty wheel stays
// warm in uv's cache.
const driverPython = "3.13"

// stderrTail bounds how much driver stderr is retained for crash reports.
const stderrTail = 8 << 10

// driverProc is one live sandbox driver child: uv resolving the pinned
// pydantic-monty, then the embedded driver speaking JSON Lines.
type driverProc struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout io.ReadCloser
	stderr *tailBuffer
}

func launchDriver(ctx context.Context) (*driverProc, error) {
	uv := lookpath.Find("uv")
	if uv == "" {
		return nil, fmt.Errorf("codeexec: uv not on PATH — needed to run the %s sandbox driver (brew install uv)", montyRequirement)
	}
	path, err := driverPath()
	if err != nil {
		return nil, err
	}
	// --no-config and a cache-dir CWD keep a malicious repo's uv.toml /
	// .python-version out of the launch; --no-build (wheels only, all deps
	// ship them) forecloses sdist build steps spawning children uv's kill
	// would orphan. The parent env is inherited deliberately: an attacker
	// who controls it controls ccx itself.
	cmd := exec.CommandContext(ctx, uv, "run", "--no-project", "--no-config", "--no-build", "--quiet", "--python", driverPython, "--with", montyRequirement, "python", path) //nolint:gosec // argv is fixed: uv from PATH runs the cached driver against a pinned requirement
	cmd.Dir = filepath.Dir(path)
	cmd.WaitDelay = 5 * time.Second
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("pipe sandbox driver stdin: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("pipe sandbox driver stdout: %w", err)
	}
	stderr := &tailBuffer{}
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("launch sandbox driver (uv run --with %s): %w", montyRequirement, err)
	}
	return &driverProc{cmd: cmd, stdin: stdin, stdout: stdout, stderr: stderr}, nil
}

// kill tears the child down: closing stdin first trips the driver's EOF
// backstop so a python grandchild dies even when the kill only reaches uv,
// then the process is killed and reaped, bounded by WaitDelay.
func (d *driverProc) kill() {
	_ = d.stdin.Close()
	_ = d.cmd.Process.Kill()
	_ = d.cmd.Wait()
}

// driverPath installs the embedded driver into the cache dir,
// content-addressed so a ccx upgrade never runs a stale driver. A cache hit
// is trusted only after its bytes match the embedded source — the filename
// alone would let anything that can write the cache dir swap in its own
// driver for every later run.
func driverPath() (string, error) {
	dir, err := cache.Dir("codeexec")
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(driverSource)
	path := filepath.Join(dir, fmt.Sprintf("driver-%x.py", sum[:8]))
	if got, err := os.ReadFile(path); err == nil && bytes.Equal(got, driverSource) { //nolint:gosec // path is rooted at the cache dir and content-addressed from the embedded source
		return path, nil
	}
	if err := cache.Store(path, driverSource, 0o644); err != nil {
		return "", fmt.Errorf("install sandbox driver: %w", err)
	}
	return path, nil
}

// tailBuffer keeps the last stderrTail bytes the driver wrote, for crash
// reports.
type tailBuffer struct {
	mu  sync.Mutex
	buf []byte
}

func (b *tailBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf = append(b.buf, p...)
	if over := len(b.buf) - stderrTail; over > 0 {
		b.buf = b.buf[over:]
	}
	return len(p), nil
}

func (b *tailBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return strings.TrimSpace(string(b.buf))
}
