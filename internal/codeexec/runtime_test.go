package codeexec

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yasyf/cc-context/internal/backend"
	"github.com/yasyf/cc-context/internal/lookpath"
)

// requireUV skips a test that spawns the real sandbox driver when uv is
// absent.
func requireUV(t *testing.T) {
	t.Helper()
	if !Supported() {
		t.Skip(UnsupportedReason)
	}
}

func testRuntime() *Runtime {
	return NewRuntime(map[string]HostFunc{
		// slow blocks ~100ms then returns 1; used to prove concurrent awaits.
		"slow": func(ctx context.Context, _ Call) (any, error) {
			select {
			case <-time.After(100 * time.Millisecond):
				return int64(1), nil
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		},
		// echo returns its single positional arg as a string.
		"echo": func(_ context.Context, call Call) (any, error) {
			return call.Args[0].(string), nil
		},
	})
}

func TestRuntimeRun(t *testing.T) {
	requireUV(t)
	tests := []struct {
		name   string
		script string
		want   string
	}{
		{"arith", "40 + 2", "42"},
		{"string_return", `"hello"`, "hello"},
		{"stdout", `print("hi")`, "hi\n"},
		// Both payloads sit under FormatAuto's size floor, so the structured
		// renderer emits compact JSON.
		{"dict_compact_json", `{"a": 1, "b": [2, 3]}`, `{"a":1,"b":[2,3]}`},
		{"tabular_compact_json", `[{"n": 1, "s": "x"}, {"n": 2, "s": "y"}]`, `[{"n":1,"s":"x"},{"n":2,"s":"y"}]`},
		{"await_host", "import asyncio\nasyncio.run(slow())", "1"},
		{"await_echo", `import asyncio` + "\n" + `asyncio.run(echo("ok"))`, "ok"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := testRuntime().Run(context.Background(), tt.script, 0)
			if err != nil {
				t.Fatalf("Run(%q) error: %v", tt.script, err)
			}
			if got != tt.want {
				t.Errorf("Run(%q) = %q, want %q", tt.script, got, tt.want)
			}
		})
	}
}

// TestDoneError proves a failed done frame wraps ErrNotFound only for the
// "not_found" wire code; empty and unknown codes stay plain errors, and the
// phase-prefixed message is preserved either way. No sandbox, so no uv gate.
func TestDoneError(t *testing.T) {
	tests := []struct {
		name         string
		frame        *driverFrame
		wantMsg      string
		wantNotFound bool
	}{
		{
			"not_found wraps sentinel",
			&driverFrame{Phase: "run", Error: `read "/x": path not found`, ErrCode: "not_found"},
			`run: read "/x": path not found: not found`,
			true,
		},
		{
			"empty code plain error",
			&driverFrame{Phase: "run", Error: "ValueError: boom"},
			"run: ValueError: boom",
			false,
		},
		{
			"unknown code plain error",
			&driverFrame{Phase: "run", Error: "weird", ErrCode: "teapot"},
			"run: weird",
			false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := doneError(tt.frame)
			if err.Error() != tt.wantMsg {
				t.Errorf("doneError() = %q, want %q", err, tt.wantMsg)
			}
			if got := errors.Is(err, ErrNotFound); got != tt.wantNotFound {
				t.Errorf("errors.Is(err, ErrNotFound) = %t, want %t", got, tt.wantNotFound)
			}
		})
	}
}

// TestRunHostErrorCodes proves the not_found code survives the full sandbox
// round-trip: an uncaught host not-found wraps ErrNotFound (exit 3), a caught
// one that continues succeeds (exit 0), and a script that raises its own error
// stays plain (exit 1) with no tag leak.
func TestRunHostErrorCodes(t *testing.T) {
	requireUV(t)
	rt := NewRuntime(map[string]HostFunc{
		"missing": func(context.Context, Call) (any, error) {
			return nil, fmt.Errorf(`read "/nope": %w`, backend.ErrPathNotFound)
		},
	})
	tests := []struct {
		name         string
		script       string
		wantErr      bool
		wantNotFound bool
		wantContains []string
	}{
		{
			name:         "uncaught not_found wraps sentinel",
			script:       "import asyncio\nasyncio.run(missing())",
			wantErr:      true,
			wantNotFound: true,
			wantContains: []string{"run:", "path not found"},
		},
		{
			name:    "caught and continued succeeds",
			script:  "import asyncio\nasync def main():\n    try:\n        await missing()\n    except Exception:\n        return \"recovered\"\nasyncio.run(main())",
			wantErr: false,
		},
		{
			name:         "raises own error stays plain",
			script:       "import asyncio\nasync def main():\n    try:\n        await missing()\n    except Exception:\n        raise ValueError(\"boom\")\nasyncio.run(main())",
			wantErr:      true,
			wantNotFound: false,
			wantContains: []string{"ValueError", "boom"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := rt.Run(context.Background(), tt.script, 0)
			if !tt.wantErr {
				if err != nil {
					t.Fatalf("Run(%q) error: %v", tt.script, err)
				}
				if got != "recovered" {
					t.Errorf("Run(%q) = %q, want %q", tt.script, got, "recovered")
				}
				return
			}
			if err == nil {
				t.Fatalf("Run(%q) = nil error, want failure", tt.script)
			}
			if got := errors.Is(err, ErrNotFound); got != tt.wantNotFound {
				t.Errorf("errors.Is(err, ErrNotFound) = %t, want %t (err %q)", got, tt.wantNotFound, err)
			}
			for _, w := range tt.wantContains {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("error %q missing %q", err, w)
				}
			}
		})
	}
}

// TestConcurrentAwaits is the P1 gate: host calls awaited together via
// asyncio.gather must run concurrently, and each must execute exactly once.
// Counters record the max number of host calls in flight and the total
// invocations, deterministic proof of the parallel dispatch path where timing
// alone is too noisy.
func TestConcurrentAwaits(t *testing.T) {
	requireUV(t)
	const n = 4
	var active, maxActive, calls int32
	rt := NewRuntime(map[string]HostFunc{
		"slow": func(_ context.Context, _ Call) (any, error) {
			atomic.AddInt32(&calls, 1)
			cur := atomic.AddInt32(&active, 1)
			for {
				prev := atomic.LoadInt32(&maxActive)
				if cur <= prev || atomic.CompareAndSwapInt32(&maxActive, prev, cur) {
					break
				}
			}
			// Wide enough that all four dispatches overlap even when a loaded
			// CI runner spreads their subprocess round-trips out; the assertion
			// (peak == n) is never relaxed, only the window widened.
			time.Sleep(250 * time.Millisecond)
			atomic.AddInt32(&active, -1)
			return int64(1), nil
		},
	})
	script := "import asyncio\n" +
		"async def main():\n" +
		"    rs = await asyncio.gather(slow(), slow(), slow(), slow())\n" +
		"    return sum(rs)\n" +
		"asyncio.run(main())"

	got, err := rt.Run(context.Background(), script, 0)
	if err != nil {
		t.Fatalf("Run error: %v", err)
	}
	if got != "4" {
		t.Errorf("got %q, want %q", got, "4")
	}
	if peak := atomic.LoadInt32(&maxActive); peak != n {
		t.Errorf("max concurrent host calls = %d, want %d (gather did not parallelize)", peak, n)
	}
	if total := atomic.LoadInt32(&calls); total != n {
		t.Errorf("total host call invocations = %d, want %d (a host call ran more than once)", total, n)
	}
}

// TestRunTypeCheckErrors proves failures surface before execution with the
// checker's own diagnostic text, so the calling model can self-correct.
func TestRunTypeCheckErrors(t *testing.T) {
	requireUV(t)
	tests := []struct {
		name   string
		script string
		want   []string
	}{
		{"type mismatch", `1 + "a"`, []string{"typecheck:", "unsupported-operator", `1 + "a"`}},
		{"undefined host call", "import asyncio\nasyncio.run(nope())", []string{"typecheck:", "nope"}},
		{"top-level return", "return 1", []string{"typecheck:", "return"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := testRuntime().Run(context.Background(), tt.script, 0)
			if err == nil {
				t.Fatalf("Run(%q) = nil error, want typecheck failure", tt.script)
			}
			for _, want := range tt.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q missing %q", err, want)
				}
			}
		})
	}
}

// TestRunCompileError proves a syntax error surfaces from compilation with the
// parser's own text, before typecheck or execution.
func TestRunCompileError(t *testing.T) {
	requireUV(t)
	_, err := testRuntime().Run(context.Background(), "def f(:", 0)
	if err == nil {
		t.Fatal("Run = nil error, want compile failure")
	}
	for _, want := range []string{"compile:", "SyntaxError"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q missing %q", err, want)
		}
	}
}

// TestHostCallValve proves one oversized host return raises a labeled sandbox
// error instead of flooding the run.
func TestHostCallValve(t *testing.T) {
	requireUV(t)
	rt := NewRuntime(map[string]HostFunc{
		"flood": func(_ context.Context, _ Call) (any, error) {
			return strings.Repeat("x", hostCallValve+1), nil
		},
	})
	_, err := rt.Run(context.Background(), "import asyncio\nasyncio.run(flood())", 0)
	if err == nil {
		t.Fatal("Run = nil error, want valve error")
	}
	for _, want := range []string{"codeexec valve", "narrow the call"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q missing %q", err, want)
		}
	}
}

// TestRunUVMissing proves the launch failure names uv and the pinned
// requirement when uv is off PATH.
func TestRunUVMissing(t *testing.T) {
	orig := lookpath.Find
	lookpath.Find = func(string) string { return "" }
	t.Cleanup(func() { lookpath.Find = orig })

	if Supported() {
		t.Fatal("Supported() = true with uv stubbed off PATH")
	}
	_, err := testRuntime().Run(context.Background(), "40 + 2", 0)
	if err == nil {
		t.Fatal("Run = nil error, want launch failure")
	}
	for _, want := range []string{"uv", montyRequirement} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q missing %q", err, want)
		}
	}
}

// TestRunCancelMidHostCall proves cancelling the context while a host call is
// in flight returns promptly and reaps the child (kill waits, bounded by
// WaitDelay).
func TestRunCancelMidHostCall(t *testing.T) {
	requireUV(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started := make(chan struct{})
	rt := NewRuntime(map[string]HostFunc{
		"hang": func(ctx context.Context, _ Call) (any, error) {
			close(started)
			<-ctx.Done()
			return nil, ctx.Err()
		},
	})
	go func() {
		<-started
		cancel()
	}()

	begin := time.Now()
	_, err := rt.Run(ctx, "import asyncio\nasyncio.run(hang())", 0)
	if err == nil {
		t.Fatal("Run = nil error, want cancellation failure")
	}
	if elapsed := time.Since(begin); elapsed > 15*time.Second {
		t.Errorf("Run took %v after cancel, want prompt return", elapsed)
	}
}

// TestRunStdoutBeforeValue proves captured stdout is prepended exactly once,
// never interleaved with the final value, even across an awaited host call.
func TestRunStdoutBeforeValue(t *testing.T) {
	requireUV(t)
	script := "import asyncio\n" +
		"async def main():\n" +
		"    print(\"before\")\n" +
		"    out = await echo(\"mid\")\n" +
		"    print(\"after\")\n" +
		"    return out\n" +
		"asyncio.run(main())"
	got, err := testRuntime().Run(context.Background(), script, 0)
	if err != nil {
		t.Fatalf("Run error: %v", err)
	}
	if want := "before\nafter\nmid"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// TestRunLargeStdout proves a ~1MiB print survives the frame stream intact.
func TestRunLargeStdout(t *testing.T) {
	requireUV(t)
	got, err := testRuntime().Run(context.Background(), `print("x" * 1048576)`, 0)
	if err != nil {
		t.Fatalf("Run error: %v", err)
	}
	if want := strings.Repeat("x", 1048576) + "\n"; got != want {
		t.Errorf("got %d bytes, want %d bytes of exact repetition", len(got), len(want))
	}
}

// TestRunTagShapedValues proves tag-shaped in-band data survives both wire
// directions: a literal {"$bytes": …} dict stays a dict, never bytes.
func TestRunTagShapedValues(t *testing.T) {
	requireUV(t)
	rt := NewRuntime(map[string]HostFunc{
		"tagged": func(context.Context, Call) (any, error) {
			return map[string]any{"$bytes": []any{int64(104), int64(105)}}, nil
		},
	})
	tests := []struct {
		name   string
		script string
	}{
		{"sandbox to host", `{"$bytes": [104, 105]}`},
		{"host to sandbox and back", "import asyncio\nasyncio.run(tagged())"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := rt.Run(context.Background(), tt.script, 0)
			if err != nil {
				t.Fatalf("Run(%q) error: %v", tt.script, err)
			}
			if want := `{"$bytes":[104,105]}`; got != want {
				t.Errorf("Run(%q) = %q, want %q", tt.script, got, want)
			}
		})
	}
}

// TestRunDeepValueErrors proves a too-deep final value surfaces as a clean
// run-phase error (the driver's encode failure path), not a driver crash.
func TestRunDeepValueErrors(t *testing.T) {
	requireUV(t)
	script := "x = 1\nfor _ in range(70):\n    x = [x]\nx"
	_, err := testRuntime().Run(context.Background(), script, 0)
	if err == nil {
		t.Fatal("Run = nil error, want depth-cap failure")
	}
	for _, want := range []string{"run:", "value nesting exceeds depth 64"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q missing %q", err, want)
		}
	}
}

// TestRunStdoutTruncated proves runaway print noise is cut at the driver with
// the marker instead of shipping an unbounded done frame.
func TestRunStdoutTruncated(t *testing.T) {
	requireUV(t)
	got, err := testRuntime().Run(context.Background(), `print("x" * 9437184)`, 0)
	if err != nil {
		t.Fatalf("Run error: %v", err)
	}
	const marker = "\n[stdout truncated at 8 MiB]\n"
	if !strings.HasSuffix(got, marker) {
		t.Errorf("output tail %q missing truncation marker", got[max(0, len(got)-64):])
	}
	if want := 8<<20 + len(marker); len(got) != want {
		t.Errorf("output = %d bytes, want %d (8 MiB + marker)", len(got), want)
	}
}

// TestRunOversizedValue proves an over-cap final value errors as a run-phase
// failure instead of a truncated or giant frame.
func TestRunOversizedValue(t *testing.T) {
	requireUV(t)
	_, err := testRuntime().Run(context.Background(), `"x" * 34603008`, 0)
	if err == nil {
		t.Fatal("Run = nil error, want value-cap failure")
	}
	for _, want := range []string{"run:", "final value exceeds 32 MiB"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q missing %q", err, want)
		}
	}
}
