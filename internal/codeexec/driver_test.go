package codeexec

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"testing"
	"time"
)

// TestDriverPathVerifiesContent proves a tampered cached driver is rewritten
// on the next resolve instead of being trusted by filename.
func TestDriverPathVerifiesContent(t *testing.T) {
	t.Setenv("CLAUDE_PLUGIN_DATA", t.TempDir())
	path, err := driverPath()
	if err != nil {
		t.Fatalf("driverPath error: %v", err)
	}
	if err := os.WriteFile(path, []byte("print('tampered')\n"), 0o600); err != nil {
		t.Fatalf("tamper: %v", err)
	}
	again, err := driverPath()
	if err != nil {
		t.Fatalf("driverPath after tamper error: %v", err)
	}
	if again != path {
		t.Fatalf("driverPath = %q, want %q", again, path)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read restored driver: %v", err)
	}
	if !bytes.Equal(got, driverSource) {
		t.Error("cached driver not restored to the embedded source after tampering")
	}
}

// TestDriverStdinEOFAnyPhase proves the process-lifetime reader thread reaps
// the driver on stdin EOF regardless of phase: a busy-looping script gives
// the interpreter no yield point, so only that thread can notice the closed
// pipe (a per-call read would leave the grandchild alive for the full
// duration limit).
func TestDriverStdinEOFAnyPhase(t *testing.T) {
	requireUV(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	d, err := launchDriver(ctx)
	if err != nil {
		t.Fatalf("launchDriver error: %v", err)
	}
	go func() { _, _ = io.Copy(io.Discard, d.stdout) }()

	init := initFrame{
		Script: "while True:\n    pass",
		Limits: map[string]any{"max_duration_secs": 300, "max_recursion_depth": 200},
	}
	if err := json.NewEncoder(d.stdin).Encode(init); err != nil {
		t.Fatalf("write init: %v", err)
	}
	_ = d.stdin.Close()

	waited := make(chan error, 1)
	go func() { waited <- d.cmd.Wait() }()
	select {
	case err := <-waited:
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() != 2 {
			t.Errorf("driver exit = %v, want exit code 2 (stdin EOF backstop)", err)
		}
	case <-ctx.Done():
		_ = d.cmd.Process.Kill()
		<-waited
		t.Fatal("driver ignored stdin EOF; reader thread not running before the run phase")
	}
}
