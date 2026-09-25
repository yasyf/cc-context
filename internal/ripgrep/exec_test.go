package ripgrep

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/yasyf/cc-context/internal/render"
)

func TestSearchOutputBounds(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w := searchOutputWriter{limit: 8, stream: "stdout", cancel: cancel}
	for _, part := range []string{"123", "45678"} {
		n, err := w.Write([]byte(part))
		if n != len(part) || err != nil {
			t.Fatalf("Write(%q) = %d, %v", part, n, err)
		}
	}
	if w.buffer.String() != "12345678" || ctx.Err() != nil {
		t.Fatalf("at limit: buffer = %q, context = %v", w.buffer.String(), ctx.Err())
	}
	n, err := w.Write([]byte("9"))
	if n != 0 || err == nil || err != w.err {
		t.Fatalf("overflow Write = %d, %v; stored error = %v", n, err, w.err)
	}
	if w.buffer.String() != "12345678" || !errors.Is(ctx.Err(), context.Canceled) {
		t.Fatalf("overflow: buffer = %q, context = %v", w.buffer.String(), ctx.Err())
	}
}

func TestExecSearchExitStatus(t *testing.T) {
	tests := []struct {
		name   string
		script string
		out    string
		code   int
		detail string
	}{
		{name: "success", script: "printf match", out: "match"},
		{name: "success with stderr", script: "printf match; printf warning >&2", out: "match"},
		{name: "no match", script: "exit 1"},
		{name: "exit one stdout", script: "printf match; exit 1", out: "match"},
		{name: "exit one stderr", script: "printf warning >&2; exit 1", code: 1, detail: "warning"},
		{name: "regex diagnostic", script: "printf partial; printf 'regex parse error: unclosed character class' >&2; exit 2", code: 2, detail: "regex parse error: unclosed character class"},
		{name: "non match failure", script: "exit 3", code: 3},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, err := execSearch(context.Background(), render.Dir(t.TempDir()), "sh", []string{"-c", tt.script})
			if out != tt.out {
				t.Fatalf("stdout = %q, want %q", out, tt.out)
			}
			if tt.code == 0 {
				if err != nil {
					t.Fatalf("execSearch: %v", err)
				}
				return
			}
			var exitErr *exec.ExitError
			if !errors.As(err, &exitErr) || exitErr.ExitCode() != tt.code {
				t.Fatalf("error = %v, want exit %d", err, tt.code)
			}
			if !strings.Contains(err.Error(), tt.detail) {
				t.Fatalf("error = %v, want diagnostic %q", err, tt.detail)
			}
		})
	}
}

func TestExecSearchOutputBounds(t *testing.T) {
	tests := []struct {
		name   string
		script string
		out    string
		stream string
	}{
		{name: "exact stdout", script: "printf 12345678", out: "12345678"},
		{name: "exact stderr", script: "printf 12345678 >&2"},
		{name: "stdout overflow", script: "printf 123456789", stream: "stdout"},
		{name: "stderr overflow", script: "printf 123456789 >&2", stream: "stderr"},
		{name: "exit one overflow", script: "printf 123456789; exit 1", stream: "stdout"},
		{name: "stdout termination", script: "printf 123456789; exec sleep 30", stream: "stdout"},
		{name: "stderr termination", script: "printf 123456789 >&2; exec sleep 30", stream: "stderr"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			out, err := execSearchBounded(ctx, render.Dir(t.TempDir()), "sh", []string{"-c", tt.script}, 8, 8)
			if ctx.Err() != nil {
				t.Fatalf("child outlived output bound: %v", ctx.Err())
			}
			if out != tt.out {
				t.Fatalf("stdout = %q, want %q", out, tt.out)
			}
			if tt.stream == "" {
				if err != nil {
					t.Fatalf("execSearchBounded: %v", err)
				}
				return
			}
			want := "grep " + tt.stream + " exceeded 8 bytes; no partial results returned"
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("error = %v, want %q", err, want)
			}
		})
	}
}

func TestExecSearchCancellation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	out, err := execSearch(ctx, render.Dir(t.TempDir()), "sh", []string{"-c", "printf partial; exec sleep 30"})
	if out != "" || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("execSearch = %q, %v; want no output and deadline exceeded", out, err)
	}
	canceled, stop := context.WithCancel(context.Background())
	stop()
	out, err = execSearch(canceled, render.Dir(t.TempDir()), "sh", []string{"-c", "printf partial"})
	if out != "" || !errors.Is(err, context.Canceled) {
		t.Fatalf("execSearch = %q, %v; want no output and canceled", out, err)
	}
}
