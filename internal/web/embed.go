package web

import (
	"bytes"
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/yasyf/cc-context/internal/cache"
	"github.com/yasyf/cc-context/internal/vendor"
)

//go:embed embed_driver.py
var embedDriverSource []byte

// EmbedModelID pins the embedding model to an exact HuggingFace revision as
// "<repo>@<commit-sha>": the driver resolves it via snapshot_download, and the
// full string is stamped into Page.EmbedModel and compared by store.Load, so
// bumping the pin invalidates every cached vector set on next load.
const EmbedModelID = "minishlab/potion-base-8M@bf8b056651a2c21b8d2565580b8569da283cab23"

// model2vecRequirement pins the embedding runtime uv provisions for the
// driver. Bump only alongside EmbedModelID verification — a runtime change can
// alter vector values as silently as a model change.
const model2vecRequirement = "model2vec==0.8.2"

// embedPython pins the CPython ABI so the per-ABI wheels stay warm in uv's
// cache.
const embedPython = "3.13"

// Per-call deadlines for the one-shot driver subprocess. A warm invocation
// (uv env + HF snapshot cached) measures ~0.6 s wall (2026-07, M-series), so
// 3 min is pure headroom; the first run also downloads the ~30 MB model, so it
// gets 5. Warmth is detected by the marker file a first success leaves beside
// the driver — simpler than probing the HF hub cache layout across its env
// overrides.
const (
	embedTimeout         = 3 * time.Minute
	embedFirstRunTimeout = 5 * time.Minute
)

// stderrTail bounds how much driver stderr is retained for crash reports.
const stderrTail = 8 << 10

// Supported reports whether hybrid (dense+BM25) search is available: the
// embedding driver needs uv on PATH to provision its Python runtime.
func Supported() bool { return vendor.LookPath("uv") != "" }

// UnsupportedReason explains the missing prerequisite when Supported is false.
const UnsupportedReason = "ccx web search runs BM25-only without uv on PATH (brew install uv) — hybrid ranking needs it"

// Embedder embeds texts into L2-normalized vectors, one per text, all of equal
// dimensionality. texts must be non-empty; a text with no in-vocabulary tokens
// embeds to the zero vector.
type Embedder interface {
	Embed(ctx context.Context, texts []string) ([][]float32, error)
}

// UVEmbedder runs the embedded model2vec driver as a one-shot uv subprocess
// per Embed call, pinned to EmbedModelID. The zero value is ready to use.
type UVEmbedder struct{}

type embedRequest struct {
	Model string   `json:"model"`
	Texts []string `json:"texts"`
}

type embedResponse struct {
	Dims    int         `json:"dims"`
	Vectors [][]float32 `json:"vectors"`
}

// Embed spawns the driver, sends one request, and returns its vectors. The
// subprocess is killed when ctx is done; stdin stays open until then because
// the driver treats EOF as the kill signal (so a python grandchild dies even
// when the kill only reaches uv — same backstop as codeexec).
func (UVEmbedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	uv := vendor.LookPath("uv")
	if uv == "" {
		return nil, fmt.Errorf("web: uv not on PATH — needed to run the %s embedding driver (brew install uv)", model2vecRequirement)
	}
	driver, err := embedDriverPath()
	if err != nil {
		return nil, err
	}
	req, err := json.Marshal(embedRequest{Model: EmbedModelID, Texts: texts})
	if err != nil {
		return nil, fmt.Errorf("marshal embedding request: %w", err)
	}

	marker, warm := warmMarker(filepath.Dir(driver))
	timeout := embedFirstRunTimeout
	if warm {
		timeout = embedTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// Flags mirror codeexec's driver launch: --no-config and a cache-dir CWD
	// keep a malicious repo's uv.toml / .python-version out of the launch;
	// --no-build (wheels only) forecloses sdist build steps spawning children
	// uv's kill would orphan.
	cmd := exec.CommandContext(ctx, uv, "run", "--no-project", "--no-config", "--no-build", "--quiet", "--python", embedPython, "--with", model2vecRequirement, "python", driver) //nolint:gosec // argv is fixed: uv from PATH runs the cached driver against a pinned requirement
	cmd.Dir = filepath.Dir(driver)
	// Own process group so a ctx kill reaches the model2vec child, not just uv —
	// the stdin-close backstop misses a child blocked on the HF snapshot download.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.WaitDelay = 5 * time.Second
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("pipe embedding driver stdin: %w", err)
	}
	defer func() { _ = stdin.Close() }()
	cmd.Cancel = func() error {
		_ = stdin.Close()
		// A cancel that races Wait reaping the child sees ESRCH: the group is
		// already gone, so report it as finished rather than fail a done embed.
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
		return nil, fmt.Errorf("launch embedding driver (uv run --with %s): %w", model2vecRequirement, err)
	}
	if _, err := stdin.Write(append(req, '\n')); err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return nil, driverErr(ctx, "write embedding request", err, stderr)
	}
	if err := cmd.Wait(); err != nil {
		return nil, driverErr(ctx, "embedding driver", err, stderr)
	}

	vectors, err := parseEmbedVectors(out.Bytes(), len(texts))
	if err != nil {
		return nil, err
	}
	if !warm {
		// Best-effort: the marker only picks the timeout, so a write failure
		// must not fail a successful embed.
		_ = cache.Store(marker, []byte(EmbedModelID), 0o644)
	}
	return vectors, nil
}

// driverErr wraps a driver failure with the context's verdict when the
// deadline (or caller) killed it, else the stderr tail when there is one.
func driverErr(ctx context.Context, op string, err error, stderr *tailBuffer) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return fmt.Errorf("%s: %w", op, ctxErr)
	}
	if tail := stderr.String(); tail != "" {
		return fmt.Errorf("%s: %w: %s", op, err, tail)
	}
	return fmt.Errorf("%s: %w", op, err)
}

// parseEmbedVectors decodes and validates the driver's response: exactly one
// vector per text, each with the advertised dimensionality.
func parseEmbedVectors(data []byte, texts int) ([][]float32, error) {
	var resp embedResponse
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("decode embedding response: %w", err)
	}
	if len(resp.Vectors) != texts {
		return nil, fmt.Errorf("embedding driver returned %d vectors for %d texts", len(resp.Vectors), texts)
	}
	for i, v := range resp.Vectors {
		if len(v) != resp.Dims {
			return nil, fmt.Errorf("embedding vector %d has %d dims, want %d", i, len(v), resp.Dims)
		}
	}
	return resp.Vectors, nil
}

// warmMarker returns the path of the first-success marker in dir and whether
// it records a successful embed with the current EmbedModelID (meaning the HF
// snapshot is downloaded and the short timeout applies). A stale pin reads as
// cold: the new model still needs its first download.
func warmMarker(dir string) (string, bool) {
	path := filepath.Join(dir, ".embed-warm")
	got, err := os.ReadFile(path) //nolint:gosec // path is rooted at the cache dir
	return path, err == nil && string(got) == EmbedModelID
}

// embedDriverPath installs the embedded driver into the web cache dir,
// content-addressed so a ccx upgrade never runs a stale driver. A cache hit is
// trusted only after its bytes match the embedded source — the filename alone
// would let anything that can write the cache dir swap in its own driver for
// every later run.
func embedDriverPath() (string, error) {
	dir, err := cache.Dir("web")
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(embedDriverSource)
	path := filepath.Join(dir, fmt.Sprintf("embed-driver-%x.py", sum[:8]))
	if got, err := os.ReadFile(path); err == nil && bytes.Equal(got, embedDriverSource) { //nolint:gosec // path is rooted at the cache dir and content-addressed from the embedded source
		return path, nil
	}
	if err := cache.Store(path, embedDriverSource, 0o644); err != nil {
		return "", fmt.Errorf("install embedding driver: %w", err)
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
