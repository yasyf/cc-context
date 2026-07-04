package codeexec

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	monty "github.com/ewhauser/gomonty"
)

func testRuntime() *Runtime {
	return NewRuntime(map[string]HostFunc{
		// slow blocks ~100ms then returns 1; used to prove concurrent awaits.
		"slow": func(ctx context.Context, _ monty.Call) (monty.Value, error) {
			select {
			case <-time.After(100 * time.Millisecond):
				return monty.Int(1), nil
			case <-ctx.Done():
				return monty.None(), ctx.Err()
			}
		},
		// echo returns its single positional arg as a string.
		"echo": func(_ context.Context, call monty.Call) (monty.Value, error) {
			return monty.String(call.Args[0].Raw().(string)), nil
		},
	})
}

func TestRuntimeRun(t *testing.T) {
	tests := []struct {
		name   string
		script string
		want   string
	}{
		{"arith", "40 + 2", "42"},
		{"string_return", `"hello"`, "hello"},
		{"stdout", `print("hi")`, "hi\n"},
		{"dict_toon", `{"a": 1, "b": [2, 3]}`, "a: 1\nb[2]: 2,3"},
		{"tabular_toon", `[{"n": 1, "s": "x"}, {"n": 2, "s": "y"}]`, "[2]{n,s}:\n  1,x\n  2,y"},
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

// TestConcurrentAwaits is the P1 gate: host calls awaited together via
// asyncio.gather must run concurrently. A counter records the max number of
// host calls in flight simultaneously — deterministic proof of the
// Pending/Waiter parallel path, where timing alone is too noisy.
func TestConcurrentAwaits(t *testing.T) {
	const n = 4
	var active, maxActive int32
	rt := NewRuntime(map[string]HostFunc{
		"slow": func(_ context.Context, _ monty.Call) (monty.Value, error) {
			cur := atomic.AddInt32(&active, 1)
			for {
				prev := atomic.LoadInt32(&maxActive)
				if cur <= prev || atomic.CompareAndSwapInt32(&maxActive, prev, cur) {
					break
				}
			}
			time.Sleep(50 * time.Millisecond)
			atomic.AddInt32(&active, -1)
			return monty.Int(1), nil
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
}

// TestRunTypeCheckErrors proves failures surface before execution with the
// checker's own diagnostic text, so the calling model can self-correct.
func TestRunTypeCheckErrors(t *testing.T) {
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
	rt := NewRuntime(map[string]HostFunc{
		"flood": func(_ context.Context, _ monty.Call) (monty.Value, error) {
			return monty.String(strings.Repeat("x", hostCallValve+1)), nil
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
