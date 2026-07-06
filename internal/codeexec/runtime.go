// Package codeexec runs model-authored Python in a monty sandbox, exposing
// host functions that call back into cc-context's tools. Only the distilled
// return value of a composition crosses back into the model's context, instead
// of every intermediate tool result.
package codeexec

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	monty "github.com/ewhauser/gomonty"

	"github.com/yasyf/cc-context/internal/cache"
	"github.com/yasyf/cc-context/internal/format"
	"github.com/yasyf/cc-context/internal/render"
)

// hostCallValve caps a single host call's string return, so one careless call
// (a full-repo grep, an un-scoped read) cannot flood the sandbox. It bounds
// each call, not the run: the final budget cap in Run is separate.
const hostCallValve = 4 << 20

// HostFunc does the blocking work of one host call: it reads the Python call's
// args and returns a single monty value (or an error surfaced to the script as
// a RuntimeError). Runtime wraps it so the script awaits it concurrently.
type HostFunc func(ctx context.Context, call monty.Call) (monty.Value, error)

// Runtime executes scripts against a fixed registry of host functions.
type Runtime struct {
	funcs  map[string]monty.ExternalFunction
	stubs  string
	limits *monty.ResourceLimits
}

// NewRuntime builds a Runtime whose scripts can call the given host functions
// (awaited from Python). Each is registered async so independent awaits in one
// script run concurrently.
func NewRuntime(funcs map[string]HostFunc) *Runtime {
	wrapped := make(map[string]monty.ExternalFunction, len(funcs))
	for name, fn := range funcs {
		wrapped[name] = async(fn)
	}
	return &Runtime{
		funcs: wrapped,
		stubs: stubs(funcs),
		limits: &monty.ResourceLimits{
			MaxDuration:       2 * time.Minute,
			MaxRecursionDepth: 200,
		},
	}
}

// Run compiles, typechecks, and executes script, returning its rendered final
// value plus any captured stdout, trimmed to budget tokens. Compile, typecheck,
// and runtime failures are returned as the error so the caller can surface them
// for self-correction.
func (rt *Runtime) Run(ctx context.Context, script string, budget int) (string, error) {
	if err := ensureFFICache(); err != nil {
		return "", err
	}
	runner, err := monty.New(script, monty.CompileOptions{ScriptName: "exec.py"})
	if err != nil {
		return "", fmt.Errorf("compile: %w", err)
	}
	if err := rt.typecheck(runner); err != nil {
		return "", err
	}
	var stdout strings.Builder
	val, err := runner.Run(ctx, monty.RunOptions{
		Functions: rt.funcs,
		Print:     monty.WriterPrintCallback(&stdout),
		Limits:    rt.limits,
	})
	if err != nil {
		return "", fmt.Errorf("run: %w", err)
	}
	return render.Cap(rendered(val, stdout.String()), budget), nil
}

// typecheck statically checks the compiled script with the host functions
// declared as untyped async stubs, so an error names real mistakes (a typo'd
// host call, a type mismatch) before anything executes.
func (rt *Runtime) typecheck(runner *monty.Runner) error {
	err := runner.TypeCheck(rt.stubs)
	if err == nil {
		return nil
	}
	var typing *monty.TypingError
	if errors.As(err, &typing) {
		return fmt.Errorf("typecheck: %s", typing.Display("full", false))
	}
	return fmt.Errorf("typecheck: %w", err)
}

// stubs renders one untyped async stub per host function; untyped so the
// checker treats results as Any instead of guessing a return type.
func stubs(funcs map[string]HostFunc) string {
	var b strings.Builder
	for _, name := range slices.Sorted(maps.Keys(funcs)) {
		fmt.Fprintf(&b, "async def %s(*args, **kwargs): ...\n", name)
	}
	return b.String()
}

var (
	ffiOnce sync.Once
	ffiErr  error
)

// ensureFFICache roots gomonty's extracted shared library under ccx's cache
// dir unless the caller already pinned GOMONTY_FFI_CACHE_DIR.
func ensureFFICache() error {
	ffiOnce.Do(func() {
		if os.Getenv("GOMONTY_FFI_CACHE_DIR") != "" {
			return
		}
		dir, err := cache.Dir("ffi")
		if err != nil {
			ffiErr = fmt.Errorf("resolve ffi cache dir: %w", err)
			return
		}
		ffiErr = os.Setenv("GOMONTY_FFI_CACHE_DIR", dir)
	})
	return ffiErr
}

func rendered(val monty.Value, stdout string) string {
	var b strings.Builder
	if stdout != "" {
		b.WriteString(stdout)
		if !strings.HasSuffix(stdout, "\n") {
			b.WriteByte('\n')
		}
	}
	switch raw := val.Raw().(type) {
	case nil:
	case string:
		b.WriteString(raw)
	case []byte:
		b.Write(raw)
	case monty.Dict, []monty.Value, monty.Tuple, monty.Set, monty.FrozenSet:
		b.WriteString(structured(val))
	default:
		if enc, err := json.Marshal(native(val)); err == nil {
			b.Write(enc)
		} else {
			b.WriteString(val.String())
		}
	}
	return b.String()
}

// structured renders a list/dict final value the way BashFormat renders JSON
// stdout: format.Convert picks the payload's leanest encoding via FormatAuto,
// with BashFormat's default indent and delimiter.
func structured(val monty.Value) string {
	enc, err := json.Marshal(native(val))
	if err != nil {
		return val.String()
	}
	out, _, err := format.Convert(enc, format.Options{Format: format.FormatAuto, Indent: 2, Delimiter: format.DelimiterComma})
	if err != nil {
		return string(enc)
	}
	return out
}

// native recursively converts a monty value into plain Go (map/slice/scalar) so
// json.Marshal emits ordinary JSON rather than monty's tagged-union encoding.
func native(v monty.Value) any {
	switch d := v.Raw().(type) {
	case []byte:
		return string(d)
	case monty.Dict:
		m := make(map[string]any, len(d))
		for _, p := range d {
			m[fmt.Sprint(native(p.Key))] = native(p.Value)
		}
		return m
	case []monty.Value:
		return nativeSlice(d)
	case monty.Tuple:
		return nativeSlice(d)
	case monty.Set:
		return nativeSlice(d)
	case monty.FrozenSet:
		return nativeSlice(d)
	default:
		return d
	}
}

func nativeSlice(items []monty.Value) []any {
	out := make([]any, len(items))
	for i, item := range items {
		out[i] = native(item)
	}
	return out
}

// async adapts a HostFunc into an awaitable external function: the work runs in
// the waiter, so monty resolves independent awaits in one script concurrently.
// A returned error and an over-valve return both raise into the sandbox.
func async(fn HostFunc) monty.ExternalFunction {
	return func(_ context.Context, call monty.Call) (monty.Result, error) {
		return monty.Pending(newWaiter(func(ctx context.Context) monty.Result {
			val, err := fn(ctx, call)
			if err != nil {
				return raise(err.Error())
			}
			if size := stringSize(val); size > hostCallValve {
				return raise(fmt.Sprintf(
					"codeexec valve: host call returned %d bytes (per-call limit %d); narrow the call with a tighter scope, section, or glob instead of reading it whole",
					size, hostCallValve))
			}
			return monty.Return(val)
		})), nil
	}
}

func raise(msg string) monty.Result {
	return monty.Raise(monty.Exception{Type: "RuntimeError", Arg: &msg})
}

func stringSize(v monty.Value) int {
	switch s := v.Raw().(type) {
	case string:
		return len(s)
	case []byte:
		return len(s)
	}
	return 0
}

// waiter runs fn on the first Wait and replays the memoized result on every
// later one. gomonty's dispatch loop resumes after a partial FutureSnapshot
// drain by re-awaiting still-pending call IDs on the same Waiter
// (gomonty@v0.0.14 dispatch.go waitForFutureResults), so without the
// memoization a host call — a real tool call with side effects — executes
// again on each re-await.
type waiter struct {
	fn     func(context.Context) monty.Result
	once   sync.Once
	result monty.Result
}

func newWaiter(fn func(context.Context) monty.Result) *waiter {
	return &waiter{fn: fn}
}

func (w *waiter) Wait(ctx context.Context) monty.Result {
	w.once.Do(func() { w.result = w.fn(ctx) })
	return w.result
}
