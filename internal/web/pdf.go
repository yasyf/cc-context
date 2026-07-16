package web

import (
	"bytes"
	"context"
	"crypto/sha256"
	_ "embed"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"github.com/yasyf/cc-context/internal/cache"
	"github.com/yasyf/cc-context/internal/lookpath"
)

//go:embed pdf_driver.py
var pdfDriverSource []byte

// liteparseRequirement pins the PDF-to-markdown runtime uv provisions for the
// driver.
const liteparseRequirement = "liteparse==2.5.0"

// pdfPython pins the CPython ABI so the per-ABI wheels stay warm in uv's cache.
const pdfPython = "3.13"

// pdfTimeout bounds the one-shot driver subprocess; the first run also downloads
// the liteparse wheels, so it is generous.
const pdfTimeout = 5 * time.Minute

// parsePDFFn is the PDF-to-markdown entry point, a package var so tests can drive
// the plainHTTP PDF branch without spawning uv. It defaults to parsePDF.
var parsePDFFn = parsePDF

// parsePDF converts raw PDF bytes to markdown by running the embedded liteparse
// driver as a one-shot uv subprocess. The bytes are streamed to the driver's
// stdin and the markdown read back from stdout; uv must be on PATH.
func parsePDF(ctx context.Context, data []byte) (string, error) {
	uv := lookpath.Find("uv")
	if uv == "" {
		return "", errors.New("web: pdf extraction requires uv on PATH (brew install uv)")
	}
	driver, err := pdfDriverPath()
	if err != nil {
		return "", err
	}

	ctx, cancel := context.WithTimeout(ctx, pdfTimeout)
	defer cancel()

	// Flags mirror the embedding driver's launch (embed.go).
	cmd := exec.CommandContext(ctx, uv, "run", "--no-project", "--no-config", "--no-build", "--quiet", "--python", pdfPython, "--with", liteparseRequirement, "python", driver) //nolint:gosec // argv is fixed: uv from PATH runs the cached driver against a pinned requirement
	cmd.Dir = filepath.Dir(driver)
	// Own process group so a ctx kill reaches the liteparse child, not just uv.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.WaitDelay = 5 * time.Second
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return "", fmt.Errorf("pipe pdf driver stdin: %w", err)
	}
	defer func() { _ = stdin.Close() }()
	cmd.Cancel = func() error {
		_ = stdin.Close()
		// A cancel that races Wait reaping the child sees ESRCH: the group is
		// already gone, so report it as finished rather than fail a done parse.
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
		return "", fmt.Errorf("launch pdf driver (uv run --with %s): %w", liteparseRequirement, err)
	}
	if _, err := stdin.Write(data); err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return "", driverErr(ctx, "write pdf bytes", err, stderr)
	}
	// EOF is the driver's end-of-input signal, so close before Wait.
	if err := stdin.Close(); err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return "", driverErr(ctx, "close pdf driver stdin", err, stderr)
	}
	if err := cmd.Wait(); err != nil {
		return "", driverErr(ctx, "pdf driver", err, stderr)
	}
	return out.String(), nil
}

// pdfDriverPath installs the embedded driver into the web cache dir,
// content-addressed so a ccx upgrade never runs a stale driver. A cache hit is
// trusted only after its bytes match the embedded source.
func pdfDriverPath() (string, error) {
	dir, err := cache.Dir("web")
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(pdfDriverSource)
	path := filepath.Join(dir, fmt.Sprintf("pdf-driver-%x.py", sum[:8]))
	if got, err := os.ReadFile(path); err == nil && bytes.Equal(got, pdfDriverSource) { //nolint:gosec // path is rooted at the cache dir and content-addressed from the embedded source
		return path, nil
	}
	if err := cache.Store(path, pdfDriverSource, 0o644); err != nil {
		return "", fmt.Errorf("install pdf driver: %w", err)
	}
	return path, nil
}
